package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/trace"
)

func TestForegroundInterruptedAppendAllowsExplicitSelectionRetry(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancel"}[canceled], func(t *testing.T) {
			root := chainRoot(t)
			thread, base, _, tip := twoSegmentThread(t, root, 10, "")
			db := hintDatabase(t, root, true)
			addHint(t, db, thread, tip, chainEpoch)
			store, _, _, _ := fixture(t)
			lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root}, ForegroundLookup)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lookup.CloseReadOnly() }()
			set, err := lookup.Thread(t.Context(), thread)
			if err != nil {
				t.Fatal(err)
			}
			// The first refresh creates the projection eligible for incremental
			// appends. Interrupt its next append, after SQLite has been closed.
			addHint(t, db, "unrelated-first", "unused", chainEpoch)
			if err := lookup.Check(t.Context(), thread, set.Revision); err != nil {
				t.Fatal(err)
			}
			projection := lookup.indexes[root].snapshot
			if !lookup.indexes[root].refreshed {
				t.Fatal("fixture did not reach incremental refresh")
			}
			addHint(t, db, "unrelated-second", "unused", chainEpoch)
			const external = int64(17)
			if !lookup.readBudget.ReserveUnscoped(external) {
				t.Fatal("external reservation refused")
			}
			defer lookup.readBudget.ReleaseUnscoped(external)
			charged, _ := lookup.readBudget.Charged()
			proofCharge := projection.charge
			units := lookup.work.units
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			started := time.Now()
			calls := 0
			lookup.work.now = func() time.Time {
				calls++
				if calls == 2 {
					if canceled {
						cancel()
					} else {
						return started.Add(Budget)
					}
				}
				return started
			}
			err = lookup.Check(ctx, thread, set.Revision)
			var source *agentapi.SourceError
			if canceled && !errors.Is(err, context.Canceled) || !canceled && (!errors.As(err, &source) || source.Cause != agentapi.CauseLookupIOTimeout) {
				t.Fatal("interruption lost its cause", err)
			}
			if calls != 2 || lookup.work.units != units {
				t.Fatal("interrupted native unit was retried", calls, lookup.work.units-units)
			}
			if lookup.indexes[root] != nil {
				t.Fatal("interrupted projection was retained")
			}
			if _, err := os.Stat(projection.dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("interrupted private projection survived", err)
			}
			if used, _ := lookup.readBudget.Charged(); used != charged-proofCharge {
				t.Fatal("interruption lost or leaked owned charges", used, charged-proofCharge)
			}
			// A separate caller operation may rebuild; the failed operation did
			// not restart itself or discard the original selection revision.
			lookup.work.now = time.Now
			if err := lookup.Check(t.Context(), thread, set.Revision); err != nil {
				t.Fatal("explicit retry could not rebuild a coherent projection", err)
			}
			if lookup.indexes[root].snapshot == projection {
				t.Fatal("explicit retry reused the interrupted projection")
			}
			if _, err := db.ExecContext(t.Context(), "UPDATE threads SET rollout_path=? WHERE id=?", base, thread); err != nil {
				t.Fatal(err)
			}
			if err := lookup.Check(t.Context(), thread, set.Revision); agentapi.Failure(err) != agentapi.Changed {
				t.Fatal("retry replaced original selection authority", err)
			}
			if err := lookup.CloseReadOnly(); err != nil {
				t.Fatal(err)
			}
			if used, _ := lookup.readBudget.Charged(); used != external {
				t.Fatal("lookup cleanup affected another owner", used)
			}
		})
	}
}

func TestForegroundRegistrationHintPropagatesInterruptionWithoutRetry(t *testing.T) {
	for _, samePath := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("same_path_%t/cancel_%t", samePath, canceled), func(t *testing.T) {
				root := chainRoot(t)
				thread, base, _, tip := twoSegmentThread(t, root, 11, "")
				db := hintDatabase(t, root, true)
				addHint(t, db, thread, tip, chainEpoch)
				store, _, _, _ := fixture(t)
				hint := base
				if samePath {
					hint = tip
				}
				reg := archive.SessionRegistration{ArchiveSessionID: "registered-hint", NativeSessionID: thread, ProjectRoot: root, ProjectID: archive.ProjectID(root), Harness: archive.Harness{Name: "codex"}, TranscriptPath: hint, SessionStartedAt: chainEpoch, RegisteredAt: chainEpoch, Origin: archive.SessionOriginHook}
				if err := store.SaveRegistration(reg); err != nil {
					t.Fatal(err)
				}
				lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root}, ForegroundLookup)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = lookup.CloseReadOnly() }()
				// A cached index makes any continued SQL selection observable,
				// including after a caller cancellation in the optional hint.
				if _, err := lookup.index(lookup.workContext(t.Context()), root, false); err != nil {
					t.Fatal(err)
				}
				charged, _ := lookup.readBudget.Charged()
				units := lookup.work.units
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				started := time.Now()
				calls := 0
				lookup.work.now = func() time.Time {
					calls++
					if calls == 2 {
						if canceled {
							cancel()
						} else {
							return started.Add(Budget)
						}
					}
					return started
				}
				_, err = lookup.Thread(ctx, thread)
				var source *agentapi.SourceError
				if canceled && !errors.Is(err, context.Canceled) || !canceled && (!errors.As(err, &source) || source.Cause != agentapi.CauseLookupIOTimeout) {
					t.Fatal("registration hint hid its interruption", err)
				}
				if calls != 2 || lookup.work.units != units || lookup.queries != 0 || lookup.probes != 1 || len(lookup.threads) != 0 {
					t.Fatal("lookup continued or retried after interrupted hint", calls, lookup.work.units-units, lookup.queries, lookup.probes)
				}
				if used, _ := lookup.readBudget.Charged(); used != charged {
					t.Fatal("interrupted header charge leaked", used, charged)
				}
			})
		}
	}
}

func TestForegroundRegistrationHintKeepsOrdinaryFailuresBestEffort(t *testing.T) {
	root := chainRoot(t)
	thread, _, _, tip := twoSegmentThread(t, root, 12, "")
	db := hintDatabase(t, root, true)
	addHint(t, db, thread, tip, chainEpoch)
	store, _, _, _ := fixture(t)
	reg := archive.SessionRegistration{ArchiveSessionID: "stale-hint", NativeSessionID: thread, ProjectRoot: root, ProjectID: archive.ProjectID(root), Harness: archive.Harness{Name: "codex"}, TranscriptPath: filepath.Join(root, "sessions", "missing.jsonl"), SessionStartedAt: chainEpoch, RegisteredAt: chainEpoch, Origin: archive.SessionOriginHook}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root}, ForegroundLookup)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.CloseReadOnly() }()
	set, err := lookup.Thread(t.Context(), thread)
	if err != nil || set.Current == nil || set.Current.Path != tip {
		t.Fatal("ordinary stale hint blocked native selection", set, err)
	}
}

func TestForegroundCollectionStartsFreshWorkAndTraceScope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		disable := trace.Enable()
		defer disable()
		root := chainRoot(t)
		thread, _, _, tip := twoSegmentThread(t, root, 13, "")
		db := hintDatabase(t, root, true)
		addHint(t, db, thread, tip, chainEpoch)
		store, _, _, _ := fixture(t)
		lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root}, ForegroundLookup)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lookup.CloseReadOnly() }()
		set, err := lookup.Thread(t.Context(), thread)
		if err != nil {
			t.Fatal(err)
		}
		ctx := lookup.workContext(t.Context())
		spend := func(duration time.Duration) {
			t.Helper()
			if err := nativeUnit(ctx, func(context.Context) error { time.Sleep(duration); return nil }); err != nil {
				t.Fatal(err)
			}
		}
		for range 3 {
			spend(2 * time.Second)
		}
		if lookup.work.remaining > 0 {
			t.Fatal("fixture did not spend preparation allowance")
		}
		projection := lookup.indexes[root].snapshot
		charged, _ := lookup.readBudget.Charged()
		units, scopes := lookup.work.units, lookup.work.scopes
		lookup.BeginCollection()
		if lookup.work.remaining != Budget || lookup.work.scopes != scopes+1 || lookup.work.units != units || lookup.probes != 0 {
			t.Fatal("collection inherited preparation scheduling", lookup.work)
		}
		if lookup.indexes[root].snapshot != projection || lookup.threads[thread].Revision != set.Revision || len(lookup.observations) == 0 {
			t.Fatal("collection reset retained selection or facts")
		}
		if used, _ := lookup.readBudget.Charged(); used != charged {
			t.Fatal("collection reset retained resource charges", used, charged)
		}
		for range 2 {
			spend(2 * time.Second)
		}
		if lookup.work.remaining != time.Second || lookup.work.scopes != scopes+1 {
			t.Fatal("collection renewed before spending its fresh allowance", lookup.work)
		}
		spend(2 * time.Second)
		spend(time.Second)
		if lookup.work.scopes != scopes+2 || lookup.work.units != units+4 || lookup.work.remaining != Budget-time.Second {
			t.Fatal("collection unit renewal accounting diverged", lookup.work)
		}
		var out strings.Builder
		trace.Write(&out)
		if !strings.Contains(out.String(), fmt.Sprintf("codex_lookup_scopes %d", lookup.work.scopes)) || !strings.Contains(out.String(), fmt.Sprintf("codex_lookup_work_units %d", lookup.work.units)) {
			t.Fatal("trace disagrees with retained work owner", out.String())
		}
	})
}
