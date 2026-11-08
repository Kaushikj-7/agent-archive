package local

import (
	"context"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
)

// Only the preverified writer uses this ownership record. It is captured from
// the created descriptor before caller writes, then sealed from that same
// descriptor. Cleanup never obtains ownership from the current pathname.
type atomicTemporaryCleanup struct {
	root    *os.Root
	name    string
	created os.FileInfo
	parent  os.FileInfo
	written os.FileInfo
	bytes   int64
	sum     hash.Hash
}

type atomicCleanupWriter struct {
	output io.Writer
	owner  *atomicTemporaryCleanup
}

func (w atomicCleanupWriter) Write(p []byte) (int, error) {
	n, err := w.output.Write(p)
	if n > 0 {
		_, _ = w.owner.sum.Write(p[:n])
		w.owner.bytes += int64(n)
	}
	return n, err
}

func captureAtomicCleanup(root *os.Root, name string, f *os.File, h hash.Hash) (*atomicTemporaryCleanup, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	parent, err := root.Lstat(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	return &atomicTemporaryCleanup{root: root, name: name, created: info, parent: parent, sum: h}, nil
}

// removable is refusal-only: a substituted parent/name/inode or unrecognized
// mutation is preserved. Cancellation does not prevent bounded cleanup of the
// writer's own verified partial output after quota ownership has been restored.
func (o *atomicTemporaryCleanup) removable(ctx context.Context, scratch []byte) (bool, error) {
	named, err := o.root.Lstat(o.name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	parent, err := o.root.Lstat(filepath.Dir(o.name))
	if err != nil {
		return false, err
	}
	if o.written == nil || !os.SameFile(o.created, named) || !sameAtomicFile(o.written, named) || !os.SameFile(o.parent, parent) || o.parent.Mode() != parent.Mode() || o.written.Size() != o.bytes {
		return false, nil
	}
	a, b, known := atomicChangeIdentity(o.written)
	x, y, currentKnown := atomicChangeIdentity(named)
	if !known || !currentKnown || a != x || b != y {
		return false, nil
	}
	witness, err := newAtomicWriteWitness(o.root, o.name, o.written, o.sum.Sum(nil))
	if err != nil {
		return false, err
	}
	defer witness.state.active.Store(false)
	if err := witness.verify(context.WithoutCancel(ctx), scratch); err != nil {
		return false, nil
	}
	after, err := o.root.Lstat(o.name)
	if err != nil {
		return false, err
	}
	x, y, currentKnown = atomicChangeIdentity(after)
	return currentKnown && a == x && b == y && sameAtomicFile(o.written, after), nil
}
