package local

import (
	"context"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
)

var errAtomicWitness = errors.New("atomic temporary witness no longer matches")

// AtomicWriteWitness recognizes only one writer-created temporary during its
// commit callback. Its private state cannot be constructed by callers.
type AtomicWriteWitness struct {
	state *atomicWriteWitnessState
}

type atomicWriteWitnessState struct {
	root   *os.Root
	home   os.FileInfo
	parent os.FileInfo
	file   os.FileInfo
	name   string
	sum    [32]byte
	active atomic.Bool
}

// Match checks one exact entry. A different name is never excused; an exact
// name with replaced identity fails instead of falling through classification.
func (w *AtomicWriteWitness) Match(root *os.Root, name string, info os.FileInfo) (bool, error) {
	if w == nil || w.state == nil || name != w.state.name {
		return false, nil
	}
	if err := w.Check(root); err != nil || !sameAtomicFile(w.state.file, info) {
		return false, errors.Join(errAtomicWitness, err)
	}
	return true, nil
}

// Check preserves rooted home/parent/file identity without reading payload.
func (w *AtomicWriteWitness) Check(root *os.Root) error {
	if w == nil || w.state == nil || !w.state.active.Load() || root == nil {
		return errAtomicWitness
	}
	s := w.state
	home, err := root.Stat(".")
	if err != nil || !home.IsDir() || home.Mode() != s.home.Mode() || home.Mode().Perm()&0077 != 0 || !os.SameFile(home, s.home) {
		return errors.Join(errAtomicWitness, err)
	}
	parent, err := root.Lstat(filepath.Dir(s.name))
	if err != nil || !parent.IsDir() || parent.Mode() != s.parent.Mode() || parent.Mode().Perm()&0077 != 0 || !os.SameFile(parent, s.parent) {
		return errors.Join(errAtomicWitness, err)
	}
	file, err := root.Lstat(s.name)
	if err != nil || !sameAtomicFile(s.file, file) {
		return errors.Join(errAtomicWitness, err)
	}
	return nil
}

func sameAtomicFile(a, b os.FileInfo) bool {
	return a != nil && b != nil && a.Mode().IsRegular() && b.Mode().IsRegular() && a.Mode() == b.Mode() && a.Mode().Perm() == 0600 && os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (w *AtomicWriteWitness) verify(ctx context.Context, scratch []byte) (err error) {
	if w == nil || w.state == nil || len(scratch) == 0 || w.Check(w.state.root) != nil {
		return errAtomicWitness
	}
	s := w.state
	f, err := s.root.Open(s.name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	opened, err := f.Stat()
	if err != nil || !sameAtomicFile(s.file, opened) {
		return errors.Join(errAtomicWitness, err)
	}
	h := sha256.New()
	var total int64
	for total <= s.file.Size() {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := min(int64(len(scratch)), s.file.Size()-total+1)
		n, e := f.Read(scratch[:int(want)])
		_, _ = h.Write(scratch[:n])
		total += int64(n)
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	if total != s.file.Size() || sum != s.sum {
		return errAtomicWitness
	}
	return w.Check(s.root)
}

type witnessedAtomicCommit struct {
	ctx           context.Context
	scratch       []byte
	check         func(*AtomicWriteWitness, func() error) error
	preverified   func(*AtomicWriteWitness, func() (func() error, error)) error
	cleanup       func() error
	beforeDirSync func()
	hash          hash.Hash
}

// RootedAtomicWriteWitnessed adds exact transient recognition to the checked
// writer. Scratch is borrowed only until this call returns, including rename.
func RootedAtomicWriteWitnessed(ctx context.Context, root *os.Root, name string, write func(io.Writer) error, scratch []byte, commit func(*AtomicWriteWitness, func() error) error) error {
	if ctx == nil || len(scratch) == 0 || commit == nil {
		return errAtomicWitness
	}
	return rootedAtomicWrite(root, name, write, nil, &witnessedAtomicCommit{ctx: ctx, scratch: scratch, check: commit, hash: sha256.New()})
}

func newAtomicWriteWitness(root *os.Root, name string, info os.FileInfo, sum []byte) (*AtomicWriteWitness, error) {
	home, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	parent, err := root.Lstat(filepath.Dir(name))
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || !sameAtomicFile(info, info) {
		return nil, errors.Join(errAtomicWitness, err)
	}
	s := &atomicWriteWitnessState{root: root, home: home, parent: parent, file: info, name: name}
	s.active.Store(true)
	copy(s.sum[:], sum)
	return &AtomicWriteWitness{state: s}, nil
}

// RootedAtomicWritePreverifiedWitnessed prepares one checked temporary. The
// callback first requests full verification outside policy locks, then receives
// a one-shot identity-only rename bound to native change time. Cleanup runs only
// after restoreCleanup succeeds; failure preserves the charged temporary.
func RootedAtomicWritePreverifiedWitnessed(ctx context.Context, root *os.Root, name string, write func(io.Writer) error, scratch []byte, commit func(*AtomicWriteWitness, func() (func() error, error)) error, restoreCleanup func() error) error {
	if ctx == nil || len(scratch) == 0 || commit == nil || restoreCleanup == nil {
		return errAtomicWitness
	}
	if root == nil {
		return errAtomicWitness
	}
	info, err := root.Stat(".")
	if err != nil {
		return err
	}
	if _, _, known := atomicChangeIdentity(info); !known {
		return errAtomicWitness
	}
	return rootedAtomicWrite(root, name, write, nil, &witnessedAtomicCommit{ctx: ctx, scratch: scratch, preverified: commit, cleanup: restoreCleanup, hash: sha256.New()})
}

func (w *AtomicWriteWitness) preverifiedRename(ctx context.Context, scratch []byte, rename func() error) (func() error, error) {
	before, err := w.state.root.Lstat(w.state.name)
	if err != nil {
		return nil, err
	}
	sec, nsec, known := atomicChangeIdentity(before)
	if !known {
		return nil, errAtomicWitness
	}
	if err := w.verify(ctx, scratch); err != nil {
		return nil, err
	}
	after, err := w.state.root.Lstat(w.state.name)
	if err != nil {
		return nil, err
	}
	a, b, known := atomicChangeIdentity(after)
	if !known || sec != a || nsec != b || !sameAtomicFile(before, after) {
		return nil, errAtomicWitness
	}
	used := false
	return func() error {
		if used {
			return errAtomicWitness
		}
		used = true
		if err := errors.Join(ctx.Err(), w.Check(w.state.root)); err != nil {
			return err
		}
		current, err := w.state.root.Lstat(w.state.name)
		if err != nil {
			return err
		}
		a, b, known := atomicChangeIdentity(current)
		if !known || a != sec || b != nsec || !sameAtomicFile(after, current) {
			return errAtomicWitness
		}
		w.state.active.Store(false)
		return rename()
	}, nil
}
