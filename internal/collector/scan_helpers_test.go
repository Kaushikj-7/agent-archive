package collector

import (
	"context"
	"fmt"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// These drive one step of a pass directly, for tests that need a session in
// a state Run would not leave it in by itself. They bypass Run's skip logic,
// which is why they live here and not in the package: production code goes
// through Run.

// processSession scans one session: see sessionScan for its steps.
func processSession(ctx context.Context, local *state.Store, store storage.ObjectStore, reg archive.SessionRegistration, req state.Request, now time.Time, opts Options) (sessionOutcome, error) {
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("load published cache: %w", err)
	}
	return newSessionScan(ctx, local, store, reg, req, published, now, opts).run(ctx)
}

// blockSession is sessionScan.block for one session outside a scan, at a
// source state not known.
func blockSession(local *state.Store, id string, req state.Request, reason state.BlockedReason, candidate *archive.SourceBundle) (sessionOutcome, error) {
	published, err := local.LoadPublishedState(id)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("load published cache: %w", err)
	}
	scan := &sessionScan{local: local, reg: archive.SessionRegistration{ArchiveSessionID: id}, req: req, published: published}
	return scan.block(reason, candidate, nil)
}

// markPublishedSubagent is announceSubagent from the subagent's published
// state on disk.
func markPublishedSubagent(local *state.Store, reg archive.SessionRegistration) error {
	if reg.ParentSessionID == "" {
		return nil
	}
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		return err
	}
	return announceSubagent(local, reg, published)
}

// unchangedSinceLastScan is pass.unchangedSinceLastScan outside a pass, with
// the session's parent (if any) registered.
func unchangedSinceLastScan(ctx context.Context, local *state.Store, reg archive.SessionRegistration, opts Options) (bool, error) {
	p := &pass{ctx: ctx, local: local, opts: opts, registered: map[string]bool{reg.ParentSessionID: true}, parents: map[string]state.PublishedSummary{}}
	unchanged, _, err := p.unchangedSinceLastScan(reg)
	return unchanged, err
}

// recordScanSignature is sessionScan.recordScanSignature for a session
// outside a scan.
func recordScanSignature(local *state.Store, reg archive.SessionRegistration, observed sourceState, bundle archive.SourceBundle, opts Options) error {
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		return err
	}
	scan := &sessionScan{local: local, reg: reg, opts: opts, published: published}
	return scan.recordScanSignature(observed, bundle)
}
