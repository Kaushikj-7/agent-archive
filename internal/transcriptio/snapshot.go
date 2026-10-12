// Package transcriptio owns verified, bounded native transcript reads.
package transcriptio

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

// ErrNotRegularFile rejects sources that could block or read without end.
var ErrNotRegularFile = errors.New("transcript is not a regular file")

// ErrCleanup means an owned descriptor could not be released during failed open.
var ErrCleanup = errors.New("transcript cleanup failed")

// ErrChanged indicates that a source was replaced or rewritten during a read.
var ErrChanged = errors.New("transcript changed while reading; try again")

// File is an open random-access regular file.
type File interface {
	io.ReaderAt
	io.Closer
	Stat() (fs.FileInfo, error)
}

// Opener supplies filesystem operations without implicit production defaults.
type Opener interface {
	Lstat(string) (fs.FileInfo, error)
	OpenRegular(string) (File, error)
	EvalSymlinks(string) (string, error)
}

// OS is the production filesystem implementation.
type OS struct{}

// Lstat examines a path without following its final symlink.
func (OS) Lstat(p string) (fs.FileInfo, error) { return os.Lstat(p) }

// EvalSymlinks resolves a path for store containment.
func (OS) EvalSymlinks(p string) (string, error) { return filepath.EvalSymlinks(p) }

// OpenRegular opens a verified non-blocking regular-file descriptor.
func (o OS) OpenRegular(p string) (File, error) { return o.OpenRegularFile(p) }

// OpenRegularFile follows explicit-file symlinks, and never blocks on a FIFO.
func (OS) OpenRegularFile(p string) (*os.File, error) {
	before, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, ErrNotRegularFile
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err == nil && !after.Mode().IsRegular() {
		err = ErrNotRegularFile
	}
	if err == nil && !unchanged(before, after) {
		err = ErrChanged
	}
	if err != nil {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("%w: %w", ErrCleanup, closeErr))
		}
		return nil, err
	}
	return f, nil
}

// unchanged compares identity and observable metadata across verification.
func unchanged(before, after fs.FileInfo) bool { return SameObservation(before, after) }

// SameObservation binds interpreted facts to the regular file that produced them.
// Identity, size and modification time are practical observations, not a lock
// against external writers or a detector of deliberately restored timestamps.
func SameObservation(before, after fs.FileInfo) bool {
	return before != nil && after != nil && before.Mode().IsRegular() && after.Mode().IsRegular() && os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

// Stamp retains real file identity privately, alongside ordering facts.
type Stamp struct {
	Size       int64
	ModifiedAt time.Time
	identity   fs.FileInfo
}

// SameFile compares file identities, independent of size and modification time.
func (s Stamp) SameFile(other Stamp) bool {
	return s.identity != nil && other.identity != nil && os.SameFile(s.identity, other.identity)
}

// OpenPolicy applies stricter containment to discovered files.
type OpenPolicy struct {
	RejectSymlinks bool
	Root           string
}

// Snapshot owns one descriptor and its fixed initial size boundary.
type Snapshot struct {
	files    Opener
	path     string
	file     File
	stamp    Stamp
	policy   OpenPolicy
	closed   bool
	closeErr error
}

// Open verifies the path and descriptor, closing the descriptor on any failure.
func Open(files Opener, p string, policy OpenPolicy) (*Snapshot, error) {
	if files == nil {
		return nil, errors.New("transcript filesystem is required")
	}
	path := p
	if policy.RejectSymlinks || policy.Root != "" {
		info, err := files.Lstat(p)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, ErrNotRegularFile
		}
		canonical, err := files.EvalSymlinks(p)
		if err != nil {
			return nil, err
		}
		path = canonical
		if policy.Root != "" {
			root, err := files.EvalSymlinks(policy.Root)
			if err != nil {
				return nil, err
			}
			policy.Root = root
			if !local.PathWithin(canonical, root) {
				return nil, fmt.Errorf("transcript is outside its store")
			}
		}
		f, err := files.OpenRegular(path)
		if err != nil {
			return nil, err
		}
		opened, err := f.Stat()
		if err == nil && (!opened.Mode().IsRegular() || !unchanged(info, opened)) {
			err = ErrChanged
		}
		if err != nil {
			if closeErr := f.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("%w: %w", ErrCleanup, closeErr))
			}
			return nil, err
		}
		return &Snapshot{files: files, path: p, file: f, stamp: Stamp{opened.Size(), opened.ModTime(), opened}, policy: policy}, nil
	}
	f, err := files.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Size() < 0) {
		err = ErrNotRegularFile
	}
	if err != nil {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("%w: %w", ErrCleanup, closeErr))
		}
		return nil, err
	}
	return &Snapshot{files: files, path: p, file: f, stamp: Stamp{info.Size(), info.ModTime(), info}}, nil
}

// Close releases the snapshot descriptor.
func (s *Snapshot) Close() error {
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.closeErr = s.file.Close()
	return s.closeErr
}

// SourceInfo returns the initial observation for transient admission checks.
// Callers must not persist the operating-system identity.
func (s *Snapshot) SourceInfo() fs.FileInfo { return s.stamp.identity }

// Stamp returns the initial identity and boundary.
func (s *Snapshot) Stamp() Stamp { return s.stamp }

// ReadAt reads only within the captured boundary.
func (s *Snapshot) ReadAt(p []byte, off int64) (int, error) {
	if s.closed {
		return 0, errors.New("transcript snapshot is closed")
	}
	if off < 0 {
		return 0, errors.New("negative transcript offset")
	}
	if off >= s.stamp.Size {
		return 0, io.EOF
	}
	if int64(len(p)) > s.stamp.Size-off {
		n, e := s.file.ReadAt(p[:s.stamp.Size-off], off)
		if e == nil {
			e = io.EOF
		}
		return n, e
	}
	return s.file.ReadAt(p, off)
}

// Check rejects any observable write during the read; reopen to include later appends.
func (s *Snapshot) Check() error {
	if s.closed {
		return errors.New("transcript snapshot is closed")
	}
	info, err := s.file.Stat()
	if err != nil {
		return err
	}
	if !s.stamp.SameFile(Stamp{identity: info}) || info.Size() != s.stamp.Size || !info.ModTime().Equal(s.stamp.ModifiedAt) {
		return ErrChanged
	}
	return nil
}

// Reader returns a context-aware reader over the initial boundary.
func (s *Snapshot) Reader(ctx context.Context) io.Reader {
	return &contextReader{ctx, io.NewSectionReader(s, 0, s.stamp.Size)}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// Input is a narrow verified borrowed file view shared by bounded native consumers.
type Input interface {
	io.ReaderAt
	Length() int64
	Stamp() Stamp
	Check() error
	Records(context.Context, bool, int64, int64, func([]byte) bool) (RecordWindow, error)
}

// Length returns the captured boundary without another stat.
func (s *Snapshot) Length() int64 { return s.stamp.Size }

// CheckPrefix proves a captured prefix remained unchanged while allowing later appends.
// Both the descriptor and locator must still identify the original regular file.
func (s *Snapshot) CheckPrefix(ctx context.Context, length int64, digest [32]byte) error {
	if s.closed || length < 0 || length > s.stamp.Size {
		return ErrChanged
	}
	check := func() error {
		info, err := s.file.Stat()
		if err != nil {
			return err
		}
		named, err := s.files.Lstat(s.path)
		if err == nil && (s.policy.RejectSymlinks || s.policy.Root != "") && !named.Mode().IsRegular() {
			return ErrChanged
		}
		if err == nil && s.policy.Root != "" {
			resolved, resolveErr := s.files.EvalSymlinks(s.path)
			if resolveErr != nil || !local.PathWithin(resolved, s.policy.Root) {
				return ErrChanged
			}
		}
		if err == nil && named.Mode()&fs.ModeSymlink != 0 {
			var resolved string
			resolved, err = s.files.EvalSymlinks(s.path)
			if err == nil {
				named, err = s.files.Lstat(resolved)
			}
		}
		if err != nil {
			return err
		}
		if !s.stamp.SameFile(Stamp{identity: info}) || !s.stamp.SameFile(Stamp{identity: named}) || info.Size() < length || named.Size() < length {
			return ErrChanged
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, &contextReader{ctx, io.NewSectionReader(s, 0, length)}); err != nil {
		return err
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	if got != digest {
		return ErrChanged
	}
	return check()
}
