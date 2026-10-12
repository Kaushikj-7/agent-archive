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

// indexAppendProof retains content-free validation of the initial native main
// and the last applied committed WAL prefix. It is a stamp-based observation,
// not SQLite reader-lock authority or proof against restored-mtime rewrites.
type indexAppendProof struct {
	main       os.FileInfo
	mainHash   [32]byte
	wal        os.FileInfo
	header     []byte
	end        int
	prefixHash [32]byte
	pageSize   int
}

// appendCurrentIndex validates the same generation and unchanged initial main,
// then applies only newly committed frames to the existing private projection.
// The caller closes SQLite first and poisons this view on any failure. Native
// SQLite/WAL/SHM files are only read through confined regular-file handles.
func appendCurrentIndex(ctx context.Context, root string, p *privateIndex, budget *agentapi.NativeReadBudget) error {
	reservation := budget.Reservation()
	if err := ctx.Err(); err != nil {
		return err
	}
	charge := indexVerificationScratch + 64
	if !reservation.Reserve(charge) {
		return agentapi.Wrap(agentapi.Limit, errors.New("shared native append scratch budget exhausted"))
	}
	defer func() { reservation.Release(charge) }()
	proof := p.proof
	if proof.main == nil || proof.wal == nil || len(proof.header) != 32 {
		return errIndexChanged
	}
	mainPath := filepath.Join(root, "state_5.sqlite")
	if !sameIndexFile(mainPath, proof.main, true) {
		return errIndexChanged
	}
	wal, header, info, err := openSnapshotWAL(root, mainPath+"-wal")
	if err != nil {
		return err
	}
	if wal == nil {
		return errIndexChanged
	}
	defer func() { _ = wal.Close() }()
	if !os.SameFile(proof.wal, info) || !bytes.Equal(header, proof.header) || info.Size() < int64(proof.end) {
		return errIndexChanged
	}
	if info.Size()+charge > currentSnapshotLimit || !reservation.Reserve(info.Size()) {
		return agentapi.Wrap(agentapi.Limit, errors.New("shared native append budget exhausted"))
	}
	charge += info.Size()
	metrics := indexCopyMetrics{NativeBytes: int64(len(header)), PeakBuffers: charge, NativeOpens: 2, NativeReads: 1}
	raw, err := readIndexExtent(ctx, wal, info.Size(), &metrics)
	if err != nil {
		return err
	}
	end, pages, pageSize, err := committedWAL(raw)
	if err != nil || end < proof.end || pageSize != proof.pageSize || sha256.Sum256(raw[:proof.end]) != proof.prefixHash {
		return errIndexChanged
	}
	main, err := sourcefacts.OpenRegular(root, mainPath)
	if err != nil {
		return err
	}
	defer func() { _ = main.Close() }()
	opened, err := main.Stat()
	if err != nil || !os.SameFile(proof.main, opened) {
		return errIndexChanged
	}
	if err := verifyIndexHash(ctx, main, proof.main.Size(), proof.mainHash, &metrics); err != nil {
		return err
	}
	if err := verifyIndexPrefix(ctx, wal, mainPath+"-wal", info, raw[:end], &metrics); err != nil {
		return err
	}
	if err := verifyIndexGeneration(mainPath, proof.main, wal, info, header, &metrics); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if end > proof.end {
		if err := writeAppendFrames(ctx, p.path, raw, proof.end, end, pages, pageSize, &metrics); err != nil {
			return err
		}
	}
	p.proof.end = end
	p.proof.prefixHash = sha256.Sum256(raw[:end])
	p.metrics.NativeBytes += metrics.NativeBytes
	p.metrics.PrivateBytes += metrics.PrivateBytes
	p.metrics.NativeOpens += metrics.NativeOpens
	p.metrics.NativeReads += metrics.NativeReads
	p.metrics.PrivateOpens += metrics.PrivateOpens
	p.metrics.PrivateWrites += metrics.PrivateWrites
	p.metrics.PeakBuffers = max(p.metrics.PeakBuffers, metrics.PeakBuffers)
	return nil
}

func verifyIndexHash(ctx context.Context, file *os.File, size int64, expected [32]byte, metrics *indexCopyMetrics) error {
	hash := sha256.New()
	scratch := make([]byte, indexVerificationScratch)
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := file.ReadAt(scratch[:min(int64(len(scratch)), size-offset)], offset)
		metrics.NativeBytes += int64(n)
		metrics.NativeReads++
		if err != nil && !errors.Is(err, io.EOF) || n == 0 {
			return errIndexChanged
		}
		_, _ = hash.Write(scratch[:n])
		offset += int64(n)
	}
	if !bytes.Equal(hash.Sum(nil), expected[:]) {
		return errIndexChanged
	}
	return nil
}

func writeAppendFrames(ctx context.Context, path string, raw []byte, start, end int, pages uint32, pageSize int, metrics *indexCopyMetrics) error {
	size := int64(pages) * int64(pageSize)
	if pages == 0 || size > currentSnapshotLimit {
		return errIndexChanged
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	metrics.PrivateOpens++
	for offset := start; offset < end; offset += 24 + pageSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		page := binary.BigEndian.Uint32(raw[offset:])
		if page > pages {
			continue
		}
		n, err := file.WriteAt(raw[offset+24:offset+24+pageSize], int64(page-1)*int64(pageSize))
		metrics.PrivateBytes += int64(n)
		metrics.PrivateWrites++
		if err != nil {
			return err
		}
		if n != pageSize {
			return io.ErrShortWrite
		}
	}
	return file.Truncate(size)
}
