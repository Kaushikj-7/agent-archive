package discovery

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// CodexLookupPolicy selects scheduling, independently of native selection,
// source safety limits and the lookup's shared memory ledger.
type CodexLookupPolicy uint8

const (
	// BackgroundLookup keeps the existing cumulative collector-pass allowance.
	BackgroundLookup CodexLookupPolicy = iota
	// ForegroundLookup renews scheduling at completed bounded native work units.
	// It does not renew a validation slice, replace an opened snapshot or grant
	// capture permission. Callers still need the original final selection checks.
	ForegroundLookup
)

// lookupWork owns scheduling only. Its units execute synchronously, so failed
// units are never retried and cancellation leaves no worker to join. A coherent
// composite operation retains its descriptors, cursor and charged buffers while
// successful units cross scheduling allowances. Selection lifetimes are separate.
type lookupWork struct {
	now       func() time.Time
	remaining time.Duration
	units     int64
	scopes    int64
}

type lookupWorkKey struct{}

func (l *CodexRolloutLookup) workContext(ctx context.Context) context.Context {
	if l.work == nil {
		return ctx
	}
	return context.WithValue(ctx, lookupWorkKey{}, l.work)
}

func foregroundWork(ctx context.Context) *lookupWork {
	work, _ := ctx.Value(lookupWorkKey{}).(*lookupWork)
	return work
}

func nativeInterrupted(err error) bool {
	var source *agentapi.SourceError
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.As(err, &source) && source.Cause == agentapi.CauseLookupIOTimeout
}

func nativeOpenRegular(ctx context.Context, root, path string) (file *os.File, err error) {
	err = nativeUnit(ctx, func(context.Context) error {
		var openErr error
		file, openErr = sourcefacts.OpenRegular(root, path)
		return openErr
	})
	if err != nil && file != nil {
		err = errors.Join(err, file.Close())
		file = nil
	}
	return file, err
}

// nativeUnit bounds an actual I/O/SQL unit, rather than the composite operation
// containing it. Regular-file syscalls remain synchronous, as in background
// reads; check cancellation/deadline both before and after each bounded syscall.
func nativeUnit(ctx context.Context, run func(context.Context) error) error {
	w := foregroundWork(ctx)
	if w == nil {
		return run(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.remaining <= 0 {
		w.remaining = Budget
		w.scopes++
		trace.Add(trace.CodexLookupScopes, 1)
	}
	started := w.now()
	unit, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	err := run(unit)
	elapsed := w.now().Sub(started)
	w.remaining -= elapsed
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if unit.Err() != nil || elapsed >= Budget {
		return agentapi.WithCause(agentapi.Wrap(agentapi.Unavailable, errors.New("native lookup I/O unit timed out")), agentapi.CauseLookupIOTimeout)
	}
	if err != nil {
		return err
	}
	w.units++
	trace.Add(trace.CodexLookupWorkUnits, 1)
	return nil
}

func nativeReadAt(ctx context.Context, reader io.ReaderAt, buf []byte, offset int64) (n int, err error) {
	var readErr error
	err = nativeUnit(ctx, func(context.Context) error {
		n, readErr = reader.ReadAt(buf, offset)
		// Observing EOF is a completed read, including an exact-boundary probe.
		// Preserve it for the caller to validate against the expected byte count.
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		return readErr
	})
	if err == nil {
		err = readErr
	}
	return n, err
}

func nativeWriteAt(ctx context.Context, writer io.WriterAt, buf []byte, offset int64) (n int, err error) {
	err = nativeUnit(ctx, func(context.Context) error {
		var writeErr error
		n, writeErr = writer.WriteAt(buf, offset)
		if writeErr == nil && n != len(buf) {
			return io.ErrShortWrite
		}
		return writeErr
	})
	return n, err
}
