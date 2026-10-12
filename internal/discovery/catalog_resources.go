package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"github.com/wangjohn/agent-archive/internal/local"
)

var errCatalogBudget = agentapi.ReadBudgetLimit(errors.New("native catalog exceeds shared data budget"))

// errCatalogTooLarge refuses a restored catalog past maxCatalogBytes. It is
// no memory pressure a later pass clears: the catalog is rebuilt instead,
// as a corrupt one is.
var errCatalogTooLarge = errors.New("native catalog exceeds its size cap")

// errCatalogChanged refuses a catalog that grew while it was read.
var errCatalogChanged = errors.New("native catalog changed while read")

// maxCatalogBytes caps what the lookup keeps for its whole life: the
// restored catalog and the coverage facts it adds. It is part of
// agentapi.PassCacheReserve (see PassCacheBytes).
const maxCatalogBytes = 32 << 20

// PassCacheBytes is the most the lookup keeps between sessions: catalog,
// coverage observations and metadata inventory, all charged unscoped.
const PassCacheBytes = maxCatalogBytes + maxCoverageBytes/2 + metadataBytes

// reserveCatalog keeps restored facts and new observation/coverage allocations
// charged for the lookup lifetime. The catalog and its coverage borrow the same
// owner; assigning their pointers does not charge a second decoded document.
func (l *CodexRolloutLookup) reserveCatalog(n int64) bool {
	if n < 0 || l.catalogCharge+l.restoredCharge+n > maxCatalogBytes || !l.readBudget.ReserveUnscoped(n) {
		return false
	}
	l.catalogCharge += n
	return true
}

func (l *CodexRolloutLookup) readCatalog(ctx context.Context, path string, c *catalog) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	n := info.Size()
	if !info.Mode().IsRegular() || n < 0 {
		return errors.New("native catalog is not a regular file")
	}
	// A read replaces the catalog the lookup restored before, if any: the
	// same file is never charged twice. Check the cap before reserving so
	// even a catalog larger than the shared ledger is rebuilt.
	if l.catalogCharge+n > maxCatalogBytes {
		return errCatalogTooLarge
	}
	if !l.readBudget.Reserve(n) {
		return errCatalogBudget
	}
	defer l.readBudget.Release(n)
	if !l.readBudget.ReserveUnscoped(n) {
		return errCatalogBudget
	}
	keep := false
	defer func() {
		if !keep {
			l.readBudget.ReleaseUnscoped(n)
		}
	}()
	raw := make([]byte, n)
	if _, err := io.ReadFull(f, raw); err != nil {
		return err
	}
	var extra [1]byte
	if count, err := f.Read(extra[:]); count != 0 || !errors.Is(err, io.EOF) {
		return errCatalogChanged
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, c); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.releaseRestored()
	l.restoredCharge = n
	keep = true
	return nil
}

// releaseRestored returns the charge of a restored catalog the lookup no
// longer keeps (a reset discards it; a later read replaces it).
func (l *CodexRolloutLookup) releaseRestored() {
	l.readBudget.ReleaseUnscoped(l.restoredCharge)
	l.restoredCharge = 0
}

// writeCatalog writes c within maxCatalogBytes, the most the next read
// accepts: a catalog written past it would be rejected, rebuilt and its
// coverage restarted on every pass. Over the cap, the observation cache (hints
// that only spare header reads) is dropped and the rest written. Still over,
// nothing is written: the previous file, which the next read accepts, stays,
// and errCatalogTooLarge says so.
func (l *CodexRolloutLookup) writeCatalog(ctx context.Context, path string, c catalog) error {
	err := l.writeCatalogWithin(ctx, path, c)
	if !errors.Is(err, errCatalogTooLarge) || len(c.Cache) == 0 {
		return err
	}
	c.Cache = nil
	return l.writeCatalogWithin(ctx, path, c)
}

func (l *CodexRolloutLookup) writeCatalogWithin(ctx context.Context, path string, c catalog) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	const countScratch = 32 << 10
	if !l.readBudget.Reserve(countScratch) {
		return errCatalogBudget
	}
	available := l.readBudget.Available()
	limit := min(available-1, maxCatalogBytes)
	n, err := jsonwire.Bound(ctx, c, limit)
	overCap := errors.Is(err, jsonwire.ErrLimit) && limit == maxCatalogBytes
	if errors.Is(err, jsonwire.ErrLimit) && !overCap {
		l.readBudget.Refuse(available + 1)
	}
	l.readBudget.Release(countScratch)
	if overCap {
		return errCatalogTooLarge
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return errors.Join(errCatalogBudget, err)
	}
	if !l.readBudget.Reserve(n + 1) {
		return errCatalogBudget
	}
	defer l.readBudget.Release(n + 1)
	if err := ctx.Err(); err != nil {
		return err
	}
	return local.WriteCompact(path, c)
}

// retainObservation keeps an observation only when the pass has room for
// it: a pass-lifetime cache, so neither its scratch nor its refusal is any
// session's.
func (l *CodexRolloutLookup) retainObservation(ctx context.Context, entry cached) bool {
	const countScratch = 32 << 10
	if !l.readBudget.ReserveUnscoped(countScratch) {
		return false
	}
	n, err := jsonwire.Bound(ctx, entry, l.readBudget.Available())
	l.readBudget.ReleaseUnscoped(countScratch)
	return err == nil && l.reserveCatalog(n)
}
