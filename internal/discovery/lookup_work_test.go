package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// simulatedLookupWork advances synctest's virtual clock at actual native unit
// boundaries. It does not replace index copying, directory reads, SQL or source
// interpretation. Reported duration is simulated work, never a wall benchmark.
func simulatedLookupWork(lookup *CodexRolloutLookup) {
	calls := 0
	lookup.work.now = func() time.Time {
		calls++
		if calls%2 == 0 {
			time.Sleep(500 * time.Millisecond)
		}
		return time.Now()
	}
}

func TestForegroundHistoryRetainsCompositeWorkAcrossSchedulingScopes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := chainRoot(t)
		thread, base, _, tip := twoSegmentThread(t, root, 1, "")
		db := hintDatabase(t, root, true)
		addHint(t, db, thread, tip, chainEpoch)
		// One supported native index requires many real copy/verification/write
		// chunks. Its unrelated payload changes no selected thread evidence.
		if _, err := db.ExecContext(t.Context(), "CREATE TABLE padding (body BLOB); INSERT INTO padding VALUES (zeroblob(524288))"); err != nil {
			t.Fatal(err)
		}
		store, _, _, _ := fixture(t)
		lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root}, ForegroundLookup)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lookup.CloseReadOnly() }()
		simulatedLookupWork(lookup)
		lookup.remaining = 0 // Earlier preparation cannot spend foreground work.
		nativeBefore := nativeIndexBytes(t, root)
		started := time.Now()
		set, err := lookup.Thread(t.Context(), thread)
		if err != nil || set.Current == nil || set.Current.Path != tip {
			t.Fatal(set, err)
		}
		elapsed := time.Since(started)
		projection := lookup.indexes[root].snapshot
		if elapsed <= Budget || lookup.work.scopes < 2 || projection.metrics.NativeReads < 16 || projection.metrics.PrivateWrites < 8 {
			t.Fatalf("composite lookup did not cross scopes: simulated=%s scopes=%d units=%d metrics=%+v", elapsed, lookup.work.scopes, lookup.work.units, projection.metrics)
		}
		// Every charged native read/private write, SQL stage and header probe
		// belongs to an actual completed unit, including both WAL header reads.
		wantUnits := projection.metrics.NativeOpens + projection.metrics.NativeReads + projection.metrics.PrivateWrites + 4
		if lookup.work.units != wantUnits {
			t.Fatalf("unit accounting=%d want=%d metrics=%+v", lookup.work.units, wantUnits, projection.metrics)
		}
		pass := chainPass(t, root, lookup)
		snapshot, err := pass.Read(t.Context(), agentapi.SourceRef{Kind: archive.SourceKindFile, Path: tip, Key: thread}, agentapi.ReadLimits{})
		if err != nil {
			t.Fatal(err)
		}
		paths := revisionPaths(t, snapshot)
		if len(paths) != 2 || paths[0] != base || paths[1] != tip {
			t.Fatal("history lineage changed", paths)
		}
		// Repeated checks retain the same coherent projection and do not repeat
		// native copies or private writes. Only three bounded SQL stages run.
		metrics := projection.metrics
		units := lookup.work.units
		for range 3 {
			if err := lookup.Check(t.Context(), thread, set.Revision); err != nil {
				t.Fatal(err)
			}
		}
		if lookup.indexes[root].snapshot != projection || projection.metrics != metrics || lookup.work.units-units != 9 {
			t.Fatal("unchanged checks repeated composite work")
		}
		if err := snapshot.Close(); err != nil {
			t.Fatal(err)
		}
		if err := pass.Close(); err != nil {
			t.Fatal(err)
		}
		for name, raw := range nativeBefore {
			if !bytes.Equal(raw, nativeIndexBytes(t, root)[name]) {
				t.Fatal("native index changed")
			}
		}
		private := projection.dir
		if err := lookup.CloseReadOnly(); err != nil {
			t.Fatal(err)
		}
		if used, _ := lookup.NativeReadBudget().Charged(); used != 0 {
			t.Fatal("retained native charge", used)
		}
		if _, err := os.Stat(private); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("private projection survived close", err)
		}
		t.Logf("simulated composite lookup duration=%s; actual native reads=%d private writes=%d units=%d scopes=%d", elapsed, metrics.NativeReads, metrics.PrivateWrites, lookup.work.units, lookup.work.scopes)
	})
}

func TestForegroundRolloutSearchAdvancesDirectoryBatchesAcrossScopes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := chainRoot(t)
		thread, base, _, _ := twoSegmentThread(t, root, 2, "archived_sessions")
		for n := range 600 {
			path := filepath.Join(root, "archived_sessions", fmt.Sprintf("irrelevant-%06d", n))
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		store, _, _, _ := fixture(t)
		lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root}, ForegroundLookup)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lookup.CloseReadOnly() }()
		simulatedLookupWork(lookup)
		started := time.Now()
		refs, err := lookup.Rollout(t.Context(), thread)
		if err != nil || len(refs) != 1 || refs[0].Path != base {
			t.Fatal(refs, err)
		}
		elapsed := time.Since(started)
		if elapsed <= Budget || lookup.work.scopes < 2 || lookup.work.units < 10 || lookup.work.units > 100 {
			t.Fatalf("directory continuation: simulated=%s units=%d scopes=%d", elapsed, lookup.work.units, lookup.work.scopes)
		}
		units := lookup.work.units
		if _, err := lookup.Rollout(t.Context(), thread); err != nil || lookup.work.units != units {
			t.Fatal("warm listing repeated traversal", err)
		}
		t.Logf("simulated directory duration=%s; actual bounded batches=%d", elapsed, units)
	})
}

func TestForegroundRefreshStillRejectsMovingSelectionAcrossScopes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := chainRoot(t)
		thread, base, _, tip := twoSegmentThread(t, root, 3, "")
		db := hintDatabase(t, root, true)
		addHint(t, db, thread, tip, chainEpoch)
		store, _, _, _ := fixture(t)
		lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root}, ForegroundLookup)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lookup.CloseReadOnly() }()
		simulatedLookupWork(lookup)
		set, err := lookup.Thread(t.Context(), thread)
		if err != nil {
			t.Fatal(err)
		}
		pass := chainPass(t, root, lookup)
		snapshot, err := pass.Read(t.Context(), agentapi.SourceRef{Kind: archive.SourceKindFile, Path: tip, Key: thread}, agentapi.ReadLimits{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = snapshot.Close() }()
		// The first refresh establishes the incremental projection. Later
		// unrelated appends keep it, renewing only scheduling allowances.
		for n := range 3 {
			addHint(t, db, fmt.Sprintf("unrelated-%d", n), "unused", chainEpoch)
			if err := lookup.Check(t.Context(), thread, set.Revision); err != nil {
				t.Fatal(err)
			}
		}
		projection := lookup.indexes[root].snapshot
		if projection.metrics.PrivateWrites < 2 || lookup.work.scopes < 2 {
			t.Fatal("append refresh did not cross scopes")
		}
		// Selecting the existing same-thread base changes the revision even
		// though that source was already valid evidence for the old chain.
		if _, err := db.ExecContext(t.Context(), "UPDATE threads SET rollout_path=? WHERE id=?", base, thread); err != nil {
			t.Fatal(err)
		}
		if err := lookup.Check(t.Context(), thread, set.Revision); agentapi.Failure(err) != agentapi.Changed {
			t.Fatal("renewal accepted changed selection", err)
		}
		if _, err := snapshot.(agentapi.SourceAdmissionEvidence).AdmissionEvidence(t.Context(), agentapi.SourceAdmission{NativeID: thread, Cwd: root}); agentapi.Failure(err) != agentapi.Changed {
			t.Fatal("active source snapshot lost its original selection check", err)
		}
		if lookup.indexes[root].snapshot != projection {
			t.Fatal("incremental refresh restarted a whole snapshot")
		}
	})
}

func TestForegroundIndexCancellationAndPressureKeepCharges(t *testing.T) {
	for _, mode := range []string{"cancel", "pressure"} {
		t.Run(mode, func(t *testing.T) {
			root := chainRoot(t)
			db := hintDatabase(t, root, false)
			addHint(t, db, "selected", "unused", chainEpoch)
			budget := agentapi.NewNativeReadBudget(currentSnapshotLimit)
			work := &lookupWork{now: time.Now, remaining: Budget, scopes: 1}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = context.WithValue(ctx, lookupWorkKey{}, work)
			held := int64(0)
			if mode == "pressure" {
				held = currentSnapshotLimit - 1024
				if !budget.ReserveUnscoped(held) {
					t.Fatal("fixture charge refused")
				}
			}
			_, err := snapshotCurrentIndexBudget(ctx, root, func(stage string) {
				if mode == "cancel" && stage == "main" {
					cancel()
				}
			}, budget)
			if mode == "cancel" && !errors.Is(err, context.Canceled) || mode == "pressure" && agentapi.Failure(err) != agentapi.Limit {
				t.Fatal("wrong interruption", err)
			}
			if used, _ := budget.Charged(); used != held {
				t.Fatal("renewal lost or leaked charges", used, held)
			}
			budget.ReleaseUnscoped(held)
		})
	}
}

func TestForegroundUnitsDoNotRetryTimeoutOrHideIOFailure(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel", "io"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				work := &lookupWork{now: time.Now, remaining: Budget, scopes: 1}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ctx = context.WithValue(ctx, lookupWorkKey{}, work)
				calls := 0
				err := nativeUnit(ctx, func(unit context.Context) error {
					calls++
					switch mode {
					case "timeout":
						<-unit.Done()
						return unit.Err()
					case "cancel":
						cancel()
						return ctx.Err()
					default:
						return io.ErrUnexpectedEOF
					}
				})
				if calls != 1 || work.units != 0 || work.scopes != 1 {
					t.Fatal("failed unit was retried or counted as progress", calls, work)
				}
				var source *agentapi.SourceError
				if mode == "timeout" && (!errors.As(err, &source) || source.Cause != agentapi.CauseLookupIOTimeout || source.Kind != agentapi.Unavailable) ||
					mode == "cancel" && !errors.Is(err, context.Canceled) || mode == "io" && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal("lost interruption classification", err)
				}
			})
		})
	}
}

func TestForegroundSQLStagesHaveIndependentBoundsAndDoNotRetry(t *testing.T) {
	for _, mode := range []string{"progress", "timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := chainRoot(t)
				db := hintDatabase(t, root, false)
				addHint(t, db, "selected", "synthetic", chainEpoch)
				work := &lookupWork{now: time.Now, remaining: Budget, scopes: 1}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ctx = context.WithValue(ctx, lookupWorkKey{}, work)
				if mode == "progress" {
					calls := 0
					work.now = func() time.Time {
						calls++
						if calls%2 == 0 {
							time.Sleep(2 * time.Second)
						}
						return time.Now()
					}
					started := time.Now()
					path, found, err := currentLocator(ctx, db, "selected")
					if err != nil || !found || path != "synthetic" || work.units != 3 || time.Since(started) != 6*time.Second {
						t.Fatal("composite SQL retained a cumulative ceiling", path, found, err, work)
					}
					return
				}
				db.SetMaxOpenConns(1)
				held, err := db.Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = held.Close() }()
				if mode == "cancel" {
					go func() { time.Sleep(time.Second); cancel() }()
				}
				_, _, err = currentLocator(ctx, db, "selected")
				var source *agentapi.SourceError
				if mode == "timeout" && (!errors.As(err, &source) || source.Cause != agentapi.CauseLookupIOTimeout) || mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatal("SQL wait lost its own failure", err)
				}
				if work.units != 0 || work.scopes != 1 {
					t.Fatal("blocked acquisition retried")
				}
				if err := held.Close(); err != nil {
					t.Fatal(err)
				}
				if db.Stats().InUse != 0 || db.Stats().WaitCount != 1 {
					t.Fatal("connection wait/lease leaked", db.Stats())
				}
				if mode == "timeout" {
					// A failed owner cannot starve later work or poison this shared
					// lookup. The caller moves on; the failed operation was not retried.
					if _, found, err := currentLocator(ctx, db, "selected"); err != nil || !found || work.scopes != 2 {
						t.Fatal("later work could not progress", err)
					}
				}
			})
		})
	}
}

func TestForegroundCatalogChunksKeepRestoredChargeAcrossScopes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, _, _, _ := fixture(t)
		path := filepath.Join(store.Home(), "discovery-catalog.json")
		if err := local.WriteCompact(path, catalog{Health: Health{Errors: []string{strings.Repeat("synthetic", 1<<16)}}}); err != nil {
			t.Fatal(err)
		}
		budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
		owner := &CodexRolloutLookup{readBudget: budget, work: &lookupWork{now: time.Now, remaining: Budget, scopes: 1}}
		simulatedLookupWork(owner)
		var restored catalog
		if err := owner.readCatalog(owner.workContext(t.Context()), path, &restored); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		wantUnits := (info.Size()+65535)/65536 + 1
		if owner.work.units != wantUnits || owner.work.scopes < 2 || owner.restoredCharge != info.Size() {
			t.Fatal("catalog chunks or retained charge", owner.work, owner.restoredCharge, info.Size())
		}
		if used, _ := budget.Charged(); used != info.Size() {
			t.Fatal("catalog charge was forgiven", used)
		}
		owner.releaseRestored()
		if used, _ := budget.Charged(); used != 0 {
			t.Fatal("catalog charge survived release", used)
		}
	})
}

func TestForegroundPolicyIsExplicitAndKeepsProbeAndResourceOwnershipSeparate(t *testing.T) {
	for _, mode := range []string{"default", "background", "foreground"} {
		t.Run(mode, func(t *testing.T) {
			root := chainRoot(t)
			thread, _, _, tip := twoSegmentThread(t, root, 5, "")
			db := hintDatabase(t, root, false)
			addHint(t, db, thread, tip, chainEpoch)
			store, _, _, _ := fixture(t)
			var policy []CodexLookupPolicy
			if mode == "background" {
				policy = []CodexLookupPolicy{BackgroundLookup}
			} else if mode == "foreground" {
				policy = []CodexLookupPolicy{ForegroundLookup}
			}
			lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root}, policy...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lookup.CloseReadOnly() }()
			lookup.probes = HeaderProbes
			if !lookup.NativeReadBudget().ReserveUnscoped(17) {
				t.Fatal("fixture charge refused")
			}
			_, err = lookup.Thread(t.Context(), thread)
			if mode == "foreground" {
				if err != nil || lookup.work == nil || lookup.work.scopes != 2 || lookup.probes != 1 {
					t.Fatal("completed foreground probe window did not renew", err)
				}
			} else {
				var source *agentapi.SourceError
				if !errors.As(err, &source) || source.Cause != agentapi.CauseLookupAllowanceSpent || lookup.work != nil {
					t.Fatal("background probe allowance changed", err)
				}
			}
			if err := lookup.CloseReadOnly(); err != nil {
				t.Fatal(err)
			}
			if used, _ := lookup.NativeReadBudget().Charged(); used != 17 {
				t.Fatal("scheduling renewal forgave another owner's charge", used)
			}
			lookup.NativeReadBudget().ReleaseUnscoped(17)
		})
	}
}
