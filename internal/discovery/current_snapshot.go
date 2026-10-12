package discovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

const currentSnapshotLimit int64 = 128 << 20

const indexVerificationScratch int64 = 64 << 10

var errIndexChanged = errors.New("native index changed during snapshot")

// indexCopyMetrics accounts actual native reads and private writes, including
// revalidation. Its buffer budget is not a claim about SQLite or process RSS.
type indexCopyMetrics struct {
	NativeBytes   int64
	PrivateBytes  int64
	PeakBuffers   int64
	NativeOpens   int64
	NativeReads   int64
	PrivateOpens  int64
	PrivateWrites int64
}

type privateIndex struct {
	dir         string
	path        string
	metrics     indexCopyMetrics
	lock        *os.File
	proof       indexAppendProof
	reservation *agentapi.NativeReadReservation
	charge      int64
}

func (p *privateIndex) close() error {
	if p.charge != 0 {
		p.reservation.Release(p.charge)
		p.charge = 0
	}
	removeErr := os.RemoveAll(p.dir)
	var closeErr error
	if p.lock != nil {
		closeErr = p.lock.Close()
		p.lock = nil
	}
	return errors.Join(removeErr, closeErr)
}

// snapshotCurrentIndex never opens native files through SQLite. A WAL generation
// is captured BEFORE the main copy; both main content and the committed prefix
// are revalidated before replay. Appends after the captured prefix are harmless.
// Checkpoint/reset/replacement or any ambiguous read refuses the projection.
// The private immutable main file contains the replayed committed state, so
// SQLite does not need a WAL, SHM, lock, or recovery operation beside native data.
func snapshotCurrentIndexBudget(ctx context.Context, root string, step func(string), budget *agentapi.NativeReadBudget) (*privateIndex, error) {
	reservation := budget.Reservation()
	charge := indexVerificationScratch + 64
	if !reservation.Reserve(charge) {
		return nil, agentapi.Wrap(agentapi.Limit, errors.New("shared index scratch budget exhausted"))
	}
	defer func() { reservation.Release(charge) }()
	mainPath := filepath.Join(root, "state_5.sqlite")
	if _, err := os.Lstat(mainPath + "-journal"); !errors.Is(err, os.ErrNotExist) {
		return nil, errIndexChanged
	}
	main, err := sourcefacts.OpenRegular(root, mainPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = main.Close() }()
	before, err := main.Stat()
	if err != nil || before.Size() < 100 || before.Size()+indexVerificationScratch > currentSnapshotLimit {
		return nil, errIndexChanged
	}
	wal, header, walBefore, err := openSnapshotWAL(root, mainPath+"-wal")
	if err != nil {
		return nil, err
	}
	if wal != nil {
		defer func() { _ = wal.Close() }()
	}
	extra, err := reserveIndexExtents(reservation, before, walBefore)
	if err != nil {
		return nil, err
	}
	charge += extra
	if step != nil {
		step("generation")
	}
	metrics := indexCopyMetrics{NativeBytes: int64(len(header)), NativeOpens: 1}
	if wal != nil {
		metrics.NativeOpens++
		metrics.NativeReads++
	}
	mainBytes, err := readIndexExtent(ctx, main, before.Size(), &metrics)
	if err != nil {
		return nil, err
	}
	if step != nil {
		step("main")
	}
	walBytes, err := readCapturedWAL(ctx, wal, before, walBefore, header, &metrics)
	if err != nil {
		return nil, err
	}
	metrics.PeakBuffers = int64(len(mainBytes)+len(walBytes)) + indexVerificationScratch
	committed, pages, pageSize, err := committedWAL(walBytes)
	if err != nil {
		return nil, err
	}
	if step != nil {
		step("wal")
	}
	// Verify content, opened identity, current pathname identity and main stamps.
	if err := verifyIndexExtent(ctx, main, mainPath, before, mainBytes, &metrics); err != nil {
		return nil, err
	}
	if wal != nil {
		// Only the selected committed prefix matters. A partial/uncommitted tail is
		// excluded, but the generation remains part of the validated prefix.
		if err := verifyIndexPrefix(ctx, wal, mainPath+"-wal", walBefore, walBytes[:committed], &metrics); err != nil {
			return nil, err
		}
	} else if _, err := os.Lstat(mainPath + "-wal"); !errors.Is(err, os.ErrNotExist) {
		return nil, errIndexChanged
	}
	if step != nil {
		step("verified")
	}
	if err := verifyIndexGeneration(mainPath, before, wal, walBefore, header, &metrics); err != nil {
		return nil, err
	}
	mainHash := sha256.Sum256(mainBytes)
	projected := int64(pages) * int64(pageSize)
	if projected > int64(len(mainBytes)) {
		peak := projected + int64(len(mainBytes)+len(walBytes))
		if peak > currentSnapshotLimit {
			return nil, errIndexChanged
		}
		if !reservation.Reserve(projected) {
			return nil, agentapi.Wrap(agentapi.Limit, errors.New("shared native replay budget exhausted"))
		}
		charge += projected
		metrics.PeakBuffers = max(metrics.PeakBuffers, peak)
	}
	if err := replayCommittedWAL(&mainBytes, walBytes[:committed], pages, pageSize); err != nil {
		return nil, err
	}
	if int64(len(mainBytes)+len(walBytes)) > currentSnapshotLimit {
		return nil, errIndexChanged
	}
	metrics.PeakBuffers = max(metrics.PeakBuffers, int64(len(mainBytes)+len(walBytes)))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out, err := writePrivateIndex(mainBytes, metrics)
	if err == nil {
		// Transfer the already reserved header/proof allowance to the private
		// view lifetime. Close returns it exactly once.
		charge -= 64
		out.reservation, out.charge = reservation, 64
		out.proof = indexAppendProof{main: before, mainHash: mainHash, wal: walBefore, header: bytes.Clone(header), end: committed, prefixHash: sha256.Sum256(walBytes[:committed]), pageSize: pageSize}
	}
	return out, err
}

func writePrivateIndex(mainBytes []byte, metrics indexCopyMetrics) (_ *privateIndex, resultErr error) {
	root, err := prepareIndexSnapshotRoot()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, privateIndexPrefix)
	if err != nil {
		return nil, err
	}
	out := &privateIndex{dir: dir, path: filepath.Join(dir, "current.sqlite"), metrics: metrics}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, out.close())
		}
	}()
	out.lock, err = lockPrivateIndex(dir)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(out.path, mainBytes, 0600); err != nil {
		return nil, err
	}
	out.metrics.PrivateBytes = int64(len(mainBytes))
	out.metrics.PrivateOpens++
	out.metrics.PrivateWrites++
	return out, nil
}

func openSnapshotWAL(root, path string) (*os.File, []byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 && info.Size() < 32 || info.Size() > currentSnapshotLimit {
		return nil, nil, nil, errIndexChanged
	}
	f, err := sourcefacts.OpenRegular(root, path)
	if err != nil {
		return nil, nil, nil, errIndexChanged
	}
	header := make([]byte, min(int64(32), info.Size()))
	if _, err := f.ReadAt(header, 0); err != nil {
		_ = f.Close()
		return nil, nil, nil, errIndexChanged
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, nil, nil, errIndexChanged
	}
	return f, header, opened, nil
}

func readIndexExtent(ctx context.Context, f *os.File, size int64, metrics *indexCopyMetrics) ([]byte, error) {
	if size < 0 || size > currentSnapshotLimit {
		return nil, errIndexChanged
	}
	out := make([]byte, int(size))
	for offset := 0; offset < len(out); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(offset+64<<10, len(out))
		n, err := f.ReadAt(out[offset:end], int64(offset))
		metrics.NativeBytes += int64(n)
		metrics.NativeReads++
		if err != nil || n != end-offset {
			return nil, errIndexChanged
		}
		offset = end
	}
	return out, nil
}

func sameIndexFile(path string, before os.FileInfo, exact bool) bool {
	after, err := os.Lstat(path)
	return err == nil && after.Mode().IsRegular() && os.SameFile(before, after) && (!exact || before.Size() == after.Size() && before.ModTime().Equal(after.ModTime()))
}

func verifyIndexExtent(ctx context.Context, f *os.File, path string, before os.FileInfo, raw []byte, metrics *indexCopyMetrics) error {
	if !sameIndexFile(path, before, true) {
		return errIndexChanged
	}
	if err := verifyIndexPrefix(ctx, f, path, before, raw, metrics); err != nil {
		return err
	}
	if !sameIndexFile(path, before, true) {
		return errIndexChanged
	}
	return nil
}

func verifyIndexPrefix(ctx context.Context, f *os.File, path string, before os.FileInfo, raw []byte, metrics *indexCopyMetrics) error {
	if !sameIndexFile(path, before, false) {
		return errIndexChanged
	}
	hash := sha256.New()
	scratch := make([]byte, 64<<10)
	for offset := 0; offset < len(raw); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(offset+len(scratch), len(raw))
		n, err := f.ReadAt(scratch[:end-offset], int64(offset))
		metrics.NativeBytes += int64(n)
		metrics.NativeReads++
		if err != nil && !errors.Is(err, io.EOF) || n != end-offset {
			return errIndexChanged
		}
		_, _ = hash.Write(scratch[:n])
		offset = end
	}
	expected := sha256.Sum256(raw)
	if !bytes.Equal(hash.Sum(nil), expected[:]) || !sameIndexFile(path, before, false) {
		return errIndexChanged
	}
	return nil
}

func walChecksum(raw []byte, order binary.ByteOrder, s0, s1 uint32) (uint32, uint32) {
	for offset := 0; offset < len(raw); offset += 8 {
		s0 += order.Uint32(raw[offset:]) + s1
		s1 += order.Uint32(raw[offset+4:]) + s0
	}
	return s0, s1
}

// committedWAL returns the last checksum-valid committed boundary. Trailing old
// generation frames or an incomplete append never become visible database pages.
func committedWAL(raw []byte) (end int, pages uint32, pageSize int, err error) {
	if len(raw) == 0 {
		return 0, 0, 0, nil
	}
	if len(raw) < 32 {
		return 0, 0, 0, errIndexChanged
	}
	magic := binary.BigEndian.Uint32(raw)
	var order binary.ByteOrder = binary.LittleEndian
	if magic == 0x377f0683 {
		order = binary.BigEndian
	} else if magic != 0x377f0682 {
		return 0, 0, 0, errIndexChanged
	}
	if binary.BigEndian.Uint32(raw[4:]) != 3007000 {
		return 0, 0, 0, errIndexChanged
	}
	size := binary.BigEndian.Uint32(raw[8:])
	if size < 512 || size > 65536 || size&(size-1) != 0 {
		return 0, 0, 0, errIndexChanged
	}
	pageSize = int(size)
	s0, s1 := walChecksum(raw[:24], order, 0, 0)
	if s0 != binary.BigEndian.Uint32(raw[24:]) || s1 != binary.BigEndian.Uint32(raw[28:]) {
		return 0, 0, 0, errIndexChanged
	}
	end = 32
	for offset := 32; offset+24+pageSize <= len(raw); offset += 24 + pageSize {
		frame := raw[offset : offset+24+pageSize]
		if !bytes.Equal(frame[8:16], raw[16:24]) {
			break
		}
		s0, s1 = walChecksum(frame[:8], order, s0, s1)
		s0, s1 = walChecksum(frame[24:], order, s0, s1)
		if s0 != binary.BigEndian.Uint32(frame[16:]) || s1 != binary.BigEndian.Uint32(frame[20:]) {
			return 0, 0, 0, errIndexChanged
		}
		if binary.BigEndian.Uint32(frame) == 0 {
			return 0, 0, 0, errIndexChanged
		}
		if count := binary.BigEndian.Uint32(frame[4:]); count != 0 {
			pages = count
			end = offset + len(frame)
		}
	}
	return end, pages, pageSize, nil
}

func replayCommittedWAL(main *[]byte, wal []byte, pages uint32, pageSize int) error {
	if len(*main) < 100 || !bytes.Equal((*main)[:16], []byte("SQLite format 3\x00")) {
		return errIndexChanged
	}
	if len(wal) == 0 || pages == 0 {
		return nil
	}
	nativeSize := int(binary.BigEndian.Uint16((*main)[16:]))
	if nativeSize == 1 {
		nativeSize = 65536
	}
	if nativeSize != pageSize {
		return errIndexChanged
	}
	if pageSize < 512 || pageSize > 65536 {
		return errIndexChanged
	}
	size := int64(pages) * int64(pageSize)
	if size < 0 || size > currentSnapshotLimit-int64(len(wal)) || size > 1<<31-1 {
		return errIndexChanged
	}
	if size > int64(len(*main)) {
		grown := make([]byte, int(size))
		copy(grown, *main)
		*main = grown
	} else {
		*main = (*main)[:int(size)]
	}
	for offset := 32; offset+24+pageSize <= len(wal); offset += 24 + pageSize {
		page := binary.BigEndian.Uint32(wal[offset:])
		if page > pages {
			continue
		}
		start64 := int64(page-1) * int64(pageSize)
		if start64 < 0 || start64 > int64(len(*main)-pageSize) || start64 > 1<<31-1 {
			return errIndexChanged
		}
		start := int(start64)
		copy((*main)[start:start+pageSize], wal[offset+24:offset+24+pageSize])
	}
	return nil
}

func verifyIndexGeneration(mainPath string, before os.FileInfo, wal *os.File, walBefore os.FileInfo, header []byte, metrics *indexCopyMetrics) error {
	// Checkpoint writes may occur while validating WAL; check main stamps once
	// more, then generation. Changed stamps conservatively reject even if content
	// happens to return to its original value.
	if !sameIndexFile(mainPath, before, true) {
		return errIndexChanged
	}
	if wal != nil {
		final := make([]byte, len(header))
		n, readErr := wal.ReadAt(final, 0)
		metrics.NativeBytes += int64(n)
		metrics.NativeReads++
		if readErr != nil || !bytes.Equal(final, header) || !sameIndexFile(mainPath+"-wal", walBefore, len(header) == 0) {
			return errIndexChanged
		}
	}
	if _, err := os.Lstat(mainPath + "-journal"); !errors.Is(err, os.ErrNotExist) {
		return errIndexChanged
	}
	return nil
}

func reserveIndexExtents(reservation *agentapi.NativeReadReservation, main, wal os.FileInfo) (int64, error) {
	bytes := main.Size()
	if wal != nil {
		bytes += wal.Size()
	}
	if !reservation.Reserve(bytes) {
		return 0, agentapi.Wrap(agentapi.Limit, errors.New("shared native copy budget exhausted"))
	}
	return bytes, nil
}

func readCapturedWAL(ctx context.Context, wal *os.File, before, walBefore os.FileInfo, header []byte, metrics *indexCopyMetrics) ([]byte, error) {
	var walBytes []byte
	var err error
	if wal != nil {
		if before.Size()+walBefore.Size()+indexVerificationScratch > currentSnapshotLimit {
			return nil, errIndexChanged
		}
		walBytes, err = readIndexExtent(ctx, wal, walBefore.Size(), metrics)
		if err != nil || len(walBytes) < len(header) || !bytes.Equal(header, walBytes[:len(header)]) {
			return nil, errIndexChanged
		}
	}
	return walBytes, nil
}
