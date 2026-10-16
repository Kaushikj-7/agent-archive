package transcriptio

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// RootOpener preserves root-descriptor confinement when using verified snapshots.
type RootOpener struct{ Root string }

// Lstat observes a confined locator without following its final symlink.
func (o RootOpener) Lstat(path string) (fs.FileInfo, error) {
	root, err := os.OpenRoot(o.Root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	relative, err := filepath.Rel(o.Root, path)
	if err != nil {
		return nil, err
	}
	return root.Lstat(relative)
}

// EvalSymlinks supplies canonical containment facts to the snapshot policy.
func (o RootOpener) EvalSymlinks(path string) (string, error) { return filepath.EvalSymlinks(path) }

// OpenRegular opens through the approved root descriptor.
func (o RootOpener) OpenRegular(path string) (File, error) {
	root, err := filepath.EvalSymlinks(o.Root)
	if err != nil {
		return nil, err
	}
	return OpenRootRegular(root, path)
}

// OpenRootRegular opens within an approved root without blocking on FIFOs or
// permitting a symlink escape. The caller owns the returned file.
func OpenRootRegular(root, path string) (*os.File, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, errors.New("invalid source locator")
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, errors.New("source root unavailable")
	}
	defer func() { _ = r.Close() }()
	info, err := r.Lstat(rel)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("source is not a regular file")
	}
	f, err := r.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("source unavailable")
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, errors.New("source changed while opening")
	}
	return f, nil
}
