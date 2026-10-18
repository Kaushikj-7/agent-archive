package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
)

// These command regressions stay sequential: Run owns process-wide trace totals
// and each native I/O unit retains its real production deadline.

// Each supported child is independently importable; the missing parent supplies
// no permission. The index and every source are synthetic temporary files.
func foregroundChildren(t *testing.T, count int) (*backfillFixture, *sql.DB) {
	t.Helper()
	f, _ := newImportFixture(t)
	native := filepath.Join(f.userHome, ".codex")
	must(t, os.RemoveAll(filepath.Join(native, "sessions")))
	must(t, os.MkdirAll(filepath.Join(native, "sessions"), 0700))
	must(t, os.MkdirAll(filepath.Join(native, "archived_sessions"), 0700))
	db, err := sql.Open("sqlite", filepath.Join(native, "state_5.sqlite"))
	must(t, err)
	t.Cleanup(func() { must(t, db.Close()) })
	_, err = db.ExecContext(t.Context(), "CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT)")
	must(t, err)
	for i := range count {
		id := fmt.Sprintf("11111111-1111-4111-8111-%012d", i)
		f.nativeIDs = append(f.nativeIDs, id)
		header, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{
			"id": id, "session_id": "22222222-2222-4222-8222-222222222222", "parent_thread_id": "22222222-2222-4222-8222-222222222222",
			"cwd": filepath.Join(f.userHome, "agent-archive"), "timestamp": "2026-09-20T17:00:00Z", "cli_version": "dev", "originator": "codex_cli_rs", "subagent_history_start_ordinal": 1,
			"source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "22222222-2222-4222-8222-222222222222", "depth": 1}}},
		}})
		must(t, err)
		body := fmt.Sprintf("\n"+`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":"2026-09-20T17:00:00Z"}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic own prompt"}]}}`+"\n", id)
		path := f.write(t, filepath.Join(".codex", "sessions", "rollout-"+id+".jsonl"), string(header)+body)
		_, err = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", id, path)
		must(t, err)
	}
	return f, db
}

func TestBackfillForegroundCompletesNativeProbeWindows(t *testing.T) {
	for _, count := range []int{3, 512} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			f, _ := foregroundChildren(t, count)
			resources := decorateForegroundSource(t, f, nil)
			f.env = withTrace(f.env, "1")
			beforeNative := snapshotTree(t, f.userHome)
			bucket, err := f.env.OpenStore(mustLoadConfig(t, f.data))
			must(t, err)
			beforeBucket, err := bucket.List(t.Context(), "")
			must(t, err)
			if len(beforeBucket) != 0 {
				t.Fatal("expected empty synthetic bucket")
			}
			var coldUnits int64
			for _, warm := range []bool{false, true} {
				if warm {
					warmForegroundCatalog(t, f)
				}
				beforeHome := snapshotTree(t, f.data)
				var owner *discovery.CodexRolloutLookup
				const external = int64(17)
				f.env.observeBackfillLookup = func(l *discovery.CodexRolloutLookup) {
					owner = l
					if !owner.NativeReadBudget().ReserveUnscoped(external) {
						t.Fatal("external owner reservation failed")
					}
				}
				var plan backfill.Plan
				f.env.observeBackfillPlan = func(p backfill.Plan) { plan = p }
				var out, errOut bytes.Buffer
				started := time.Now()
				code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex"}, nil, &out, &errOut, f.env)
				if code != 0 {
					t.Fatal(code, errOut.String())
				}
				assertForegroundCandidates(t, plan, count)
				resources.check(t)
				objects, err := bucket.List(t.Context(), "")
				must(t, err)
				if len(objects) != 0 {
					t.Fatal("dry run wrote synthetic bucket")
				}
				if owner == nil {
					t.Fatal("foreground lookup owner unavailable")
				}
				if used, peak := owner.NativeReadBudget().Charged(); used != external || peak <= external || peak > agentapi.PassReadBudget {
					t.Fatalf("unbalanced native ledger used=%d peak=%d", used, peak)
				}
				owner.NativeReadBudget().ReleaseUnscoped(external)
				if used, _ := owner.NativeReadBudget().Charged(); used != 0 {
					t.Fatalf("native owner leak after external release=%d", used)
				}
				if _, err := owner.Thread(t.Context(), "11111111-1111-4111-8111-000000000000"); !errors.Is(err, agentapi.ErrClosed) {
					t.Fatal("lookup owner remains open", err)
				}
				if n := foregroundMetric(t, errOut.String(), "backfill_native_preparation_attempts"); n != int64(count) {
					t.Fatalf("preparation attempts=%d want=%d", n, count)
				}
				if n := foregroundMetric(t, errOut.String(), "backfill_native_inspection_attempts"); n != int64(count) {
					t.Fatalf("inspection attempts=%d want=%d", n, count)
				}
				if !warm && count > discovery.HeaderProbes && foregroundMetric(t, errOut.String(), "codex_lookup_scopes") < 3 {
					t.Fatal("native preparation/inventory did not renew bounded probe windows")
				}
				units := foregroundMetric(t, errOut.String(), "codex_lookup_work_units")
				if !warm {
					coldUnits = units
				} else if units >= coldUnits {
					t.Fatalf("warm facts were not reused: units=%d cold=%d", units, coldUnits)
				}
				if units < int64(count) || units > 40*int64(count)+200 {
					t.Fatalf("unbounded/repeated native work units=%d count=%d", units, count)
				}
				if strings.Contains(errOut.String(), "backfill_lookup_unfinished_candidates") {
					t.Fatal("completed work reported unfinished", errOut.String())
				}
				accounting := plan.InventoryAccounting()
				if accounting.UniqueCandidateFiles != count+16 || accounting.Dispositions["planned_import"] != count || accounting.Dispositions["excluded"] != 15 || accounting.Dispositions["already_archived"] != 1 || accounting.Dispositions["unresolved"] != 0 {
					t.Fatalf("physical inventory not reconciled: %+v", accounting)
				}
				if !strings.Contains(errOut.String(), "codex_lookup_work_units ") {
					t.Fatal("foreground native work was not selected", errOut.String())
				}
				if snapshotTree(t, f.data) != beforeHome || snapshotTree(t, f.userHome) != beforeNative {
					t.Fatal("dry run changed archive/config/native files")
				}
				t.Logf("warm=%v candidates=%d actual_wall=%s", warm, len(plan.Candidates), time.Since(started))
			}
		})
	}
}

func assertForegroundCandidates(t *testing.T, plan backfill.Plan, count int) {
	t.Helper()
	native, filtered, archived := 0, 0, 0
	for _, c := range plan.Candidates {
		if c.Harness == "codex" {
			native++
			if c.Skip != "" {
				t.Fatalf("native unfinished=%s diagnostic=%+v", c.Skip, c.Diagnostic)
			}
		} else {
			filtered++
			if c.Skip == backfill.SkipAlreadyArchived {
				archived++
				continue
			}
			if c.Skip != backfill.SkipFilteredOut {
				t.Fatalf("unselected harness has unexpected disposition %s", c.Skip)
			}
		}
	}
	if native != count || filtered != 16 || archived != 1 || len(plan.Candidates) != count+16 {
		t.Fatalf("candidates native=%d filtered=%d total=%d want=%d+16", native, filtered, len(plan.Candidates), count)
	}
}

func foregroundMetric(t *testing.T, text, name string) int64 {
	t.Helper()
	for line := range strings.SplitSeq(text, "\n") {
		fields := strings.Fields(line)
		for i, field := range fields {
			if field == name && i+1 < len(fields) {
				n, err := strconv.ParseInt(strings.TrimSuffix(fields[i+1], ","), 10, 64)
				must(t, err)
				return n
			}
		}
	}
	t.Fatalf("missing metric %s in %s", name, text)
	return 0
}

func warmForegroundCatalog(t *testing.T, f *backfillFixture) {
	t.Helper()
	lookup, err := discovery.NewCodexRolloutLookup(t.Context(), state.OpenReadOnly(f.data), []string{filepath.Join(f.userHome, ".codex")}, discovery.ForegroundLookup)
	must(t, err)
	defer func() { must(t, lookup.Close()) }()
	for range 10 {
		lookup.BeginCollection()
		health, err := discovery.Run(t.Context(), state.OpenReadOnly(f.data), config.Config{}, discovery.Options{Rollouts: lookup})
		must(t, err)
		if !health.Pending {
			return
		}
	}
	t.Fatal("synthetic warm catalog did not complete")
}

// This decorator changes only the native boundary's side effect or fault. The
// real lookup, selection, source interpretation and command scheduler still run.
type foregroundProvider struct {
	agentapi.SourceProvider
	before    func(context.Context) error
	resources *foregroundResources
}

func (p foregroundProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.before != nil {
		if err := p.before(ctx); err != nil {
			return nil, err
		}
	}
	pass, err := p.SourceProvider.OpenPass(ctx, env)
	if err != nil {
		return nil, err
	}
	p.resources.passes++
	return &foregroundPass{SourcePass: pass, resources: p.resources}, nil
}

type foregroundResources struct {
	passes         int
	passCloses     int
	snapshots      int
	snapshotCloses int
}

type foregroundPass struct {
	agentapi.SourcePass
	resources *foregroundResources
}

func (p *foregroundPass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	snapshot, err := p.SourcePass.Read(ctx, ref, limits)
	if err != nil {
		return nil, err
	}
	p.resources.snapshots++
	return &foregroundSnapshot{SourceSnapshot: snapshot, resources: p.resources}, nil
}

func (p *foregroundPass) Close() error { p.resources.passCloses++; return p.SourcePass.Close() }

type foregroundSnapshot struct {
	agentapi.SourceSnapshot
	resources *foregroundResources
}

func (s *foregroundSnapshot) Close() error {
	s.resources.snapshotCloses++
	return s.SourceSnapshot.Close()
}

func (s *foregroundSnapshot) AdmissionEvidence(ctx context.Context, facts agentapi.SourceAdmission) (agentapi.AdmissionEvidence, error) {
	return s.SourceSnapshot.(agentapi.SourceAdmissionEvidence).AdmissionEvidence(ctx, facts)
}

func (r *foregroundResources) check(t *testing.T) {
	t.Helper()
	if r.passes != r.passCloses || r.snapshots != r.snapshotCloses {
		t.Fatalf("native descriptors unbalanced: %+v", r)
	}
}

func decorateForegroundSource(t *testing.T, f *backfillFixture, before func(context.Context) error) *foregroundResources {
	t.Helper()
	var bindings []builtin.Integration
	resources := &foregroundResources{}
	for _, d := range productionAgents.Catalog().All() {
		binding, ok := productionAgents.Lookup(string(d.ID))
		if !ok {
			t.Fatal("missing builtin")
		}
		binding.Descriptor = agentmeta.Descriptor{ID: d.ID}
		if d.ID == agentmeta.Codex {
			binding.Sources = foregroundProvider{binding.Sources, before, resources}
		}
		bindings = append(bindings, binding)
	}
	var err error
	f.env.Agents, err = builtin.New(agentmeta.Builtins(), bindings)
	must(t, err)
	return resources
}

// The clock sleeps only inside synctest: real files, SQL, selection and Run still
// execute, while context timers also see the simulated elapsed lookup work.
func TestBackfillForegroundSingleHistoryOutlivesPreparationAllowance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, db := foregroundChildren(t, 1)
		_, err := db.ExecContext(t.Context(), "CREATE TABLE padding(body BLOB); INSERT INTO padding VALUES(zeroblob(524288))")
		must(t, err)
		calls := 0
		f.env.nativeLookupNow = func() time.Time {
			calls++
			if calls%2 == 0 {
				time.Sleep(500 * time.Millisecond)
			}
			return time.Now()
		}
		preparationCalls := 0
		decorateForegroundSource(t, f, func(context.Context) error {
			if preparationCalls == 0 {
				preparationCalls = calls
			}
			return nil
		})
		f.env = withTrace(f.env, "1")
		var owner *discovery.CodexRolloutLookup
		f.env.observeBackfillLookup = func(l *discovery.CodexRolloutLookup) { owner = l }
		var plan backfill.Plan
		f.env.observeBackfillPlan = func(p backfill.Plan) { plan = p }
		before := snapshotTree(t, f.data)
		var out, errOut bytes.Buffer
		started := time.Now()
		code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex"}, nil, &out, &errOut, f.env)
		if code != 0 {
			t.Fatal(code, errOut.String())
		}
		assertForegroundCandidates(t, plan, 1)
		if preparationCalls < 22 || time.Since(started) <= 5*time.Second {
			t.Fatalf("preparation did not spend allowance: calls=%d simulated=%s", preparationCalls, time.Since(started))
		}
		if scopes := foregroundMetric(t, errOut.String(), "codex_lookup_scopes"); scopes < 3 {
			t.Fatalf("supported history did not renew scopes=%d", scopes)
		}
		if owner == nil {
			t.Fatal("missing foreground owner")
		}
		if used, _ := owner.NativeReadBudget().Charged(); used != 0 {
			t.Fatalf("native charges leaked=%d", used)
		}
		if snapshotTree(t, f.data) != before {
			t.Fatal("simulated dry run mutated archive")
		}
		t.Logf("simulated_lookup=%s preparation_units=%d total_units=%d; not a wall timing measurement", time.Since(started), preparationCalls/2, calls/2)
	})
}

func TestBackfillForegroundMovingCurrentKeepsUnrelatedCandidates(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run(strconv.FormatBool(rewrite), func(t *testing.T) {
			f, db := foregroundChildren(t, 3)
			changed := false
			decorateForegroundSource(t, f, func(ctx context.Context) error {
				if changed {
					return nil
				}
				changed = true
				if rewrite {
					path := filepath.Join(f.userHome, ".codex", "sessions", "rollout-11111111-1111-4111-8111-000000000000.jsonl")
					raw, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					return os.WriteFile(path, bytes.ReplaceAll(raw, []byte("Synthetic own prompt"), []byte("Synthetic changed own prompt")), 0600)
				}
				_, err := db.ExecContext(ctx, "UPDATE threads SET rollout_path=? WHERE id=?", filepath.Join(f.userHome, ".codex", "sessions", "rollout-11111111-1111-4111-8111-000000000001.jsonl"), "11111111-1111-4111-8111-000000000000")
				return err
			})
			var plan backfill.Plan
			f.env.observeBackfillPlan = func(p backfill.Plan) { plan = p }
			before := snapshotTree(t, f.data)
			var out, errOut bytes.Buffer
			if code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex"}, nil, &out, &errOut, f.env); code != 0 {
				t.Fatal(code, errOut.String())
			}
			good, bad := 0, 0
			for _, c := range plan.Candidates {
				if c.Harness == "codex" {
					if c.Skip != "" && c.Skip != backfill.SkipSourceChanged && c.Skip != backfill.SkipRelatedHistory {
						t.Fatalf("unexpected moving-source refusal %s %+v", c.Skip, c.Diagnostic)
					}
					if c.Skip == "" {
						good++
					} else {
						bad++
						if c.Diagnostic == nil || c.Diagnostic.Detail != backfill.DetailSourceChanged {
							t.Fatalf("moving source lost cause: %+v", c.Diagnostic)
						}
					}
				}
			}
			if good != 2 || bad != 1 {
				t.Fatalf("moving source starved unrelated candidates: good=%d bad=%d", good, bad)
			}
			if snapshotTree(t, f.data) != before {
				t.Fatal("changed-source preview mutated archive")
			}
		})
	}
}

func TestBackfillForegroundCancellationClosesOwner(t *testing.T) {
	f, _ := foregroundChildren(t, 3)
	signals := make(chan os.Signal, 1)
	stopped := 0
	f.env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() { stopped++ } }
	calls := 0
	decorateForegroundSource(t, f, func(ctx context.Context) error {
		calls++
		signals <- os.Interrupt
		<-ctx.Done()
		return ctx.Err()
	})
	var owner *discovery.CodexRolloutLookup
	f.env.observeBackfillLookup = func(l *discovery.CodexRolloutLookup) { owner = l }
	before := snapshotTree(t, f.data)
	var out, errOut bytes.Buffer
	if code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex"}, nil, &out, &errOut, f.env); code != 1 || !strings.Contains(errOut.String(), "stopped. Nothing was changed.") {
		t.Fatal(code, errOut.String())
	}
	if calls != 1 || stopped != 1 {
		t.Fatalf("cancel continued work or watcher: calls=%d stopped=%d", calls, stopped)
	}
	if owner == nil {
		t.Fatal("missing owner")
	}
	if used, _ := owner.NativeReadBudget().Charged(); used != 0 {
		t.Fatalf("cancel leaked native charges=%d", used)
	}
	if snapshotTree(t, f.data) != before {
		t.Fatal("cancel mutated archive")
	}
}

func TestBackfillForegroundUnavailableInventoryStopsWithoutRetryingPrefix(t *testing.T) {
	f, _ := foregroundChildren(t, 3)
	native := filepath.Join(f.userHome, ".codex")
	// A directory replaced with an ordinary file is deterministically unreadable
	// as an inventory, including under privileged test runners.
	must(t, os.Remove(filepath.Join(native, "archived_sessions")))
	must(t, os.WriteFile(filepath.Join(native, "archived_sessions"), []byte("synthetic unavailable directory"), 0600))
	var owner *discovery.CodexRolloutLookup
	f.env.observeBackfillLookup = func(l *discovery.CodexRolloutLookup) { owner = l }
	f.env = withTrace(f.env, "1")
	before := snapshotTree(t, f.data)
	var plan backfill.Plan
	f.env.observeBackfillPlan = func(p backfill.Plan) { plan = p }
	var out, errOut bytes.Buffer
	if code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex"}, nil, &out, &errOut, f.env); code != 0 {
		t.Fatal(code, errOut.String())
	}
	assertForegroundCandidates(t, plan, 3)
	if plan.NativeLookupIncomplete == nil || plan.NativeLookupIncomplete.Detail != backfill.DetailLookupNoProgress || !strings.Contains(out.String(), "native_lookup_incomplete") {
		t.Fatal("no-progress inventory was not reported", out.String())
	}
	if units := foregroundMetric(t, errOut.String(), "codex_lookup_work_units"); units > 80 {
		t.Fatalf("unavailable prefix retried without progress: units=%d", units)
	}
	if owner == nil {
		t.Fatal("missing owner")
	}
	if used, _ := owner.NativeReadBudget().Charged(); used != 0 {
		t.Fatalf("no progress leaked=%d", used)
	}
	if snapshotTree(t, f.data) != before {
		t.Fatal("no-progress run mutated archive")
	}
}

func TestBackfillForegroundInventoryRenewsAfterPreparation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, _ := foregroundChildren(t, 16)
		calls := 0
		f.env.nativeLookupNow = func() time.Time {
			calls++
			if calls%2 == 0 {
				time.Sleep(500 * time.Millisecond)
			}
			return time.Now()
		}
		var plan backfill.Plan
		f.env.observeBackfillPlan = func(p backfill.Plan) { plan = p }
		f.env = withTrace(f.env, "1")
		before := snapshotTree(t, f.data)
		var out, errOut bytes.Buffer
		started := time.Now()
		if code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex"}, nil, &out, &errOut, f.env); code != 0 {
			t.Fatal(code, errOut.String())
		}
		assertForegroundCandidates(t, plan, 16)
		if time.Since(started) <= 5*time.Second || calls < 64 || calls > 1600 {
			t.Fatalf("work did not advance bounded scopes: simulated=%s calls=%d", time.Since(started), calls)
		}
		if snapshotTree(t, f.data) != before {
			t.Fatal("renewed inventory changed archive")
		}
		t.Logf("simulated_lookup=%s actual_native_units=%d (context timers advanced; not wall timing)", time.Since(started), calls/2)
	})
}

func TestBackfillForegroundDeduplicatesPreparationIdentities(t *testing.T) {
	f, _ := foregroundChildren(t, 3)
	source := filepath.Join(f.userHome, ".codex", "sessions", "rollout-11111111-1111-4111-8111-000000000000.jsonl")
	raw, err := os.ReadFile(source)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(f.userHome, ".codex", "archived_sessions", filepath.Base(source)), raw, 0600))
	f.env = withTrace(f.env, "1")
	var plan backfill.Plan
	f.env.observeBackfillPlan = func(p backfill.Plan) { plan = p }
	before := snapshotTree(t, f.data)
	var out, errOut bytes.Buffer
	if code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex"}, nil, &out, &errOut, f.env); code != 0 {
		t.Fatal(code, errOut.String())
	}
	native, duplicates := 0, 0
	for _, c := range plan.Candidates {
		if c.Harness == "codex" {
			if c.Skip != "" && c.Skip != backfill.SkipDuplicateSession {
				t.Fatalf("unexpected duplicate disposition %s %+v", c.Skip, c.Diagnostic)
			}
			if c.Skip == "" {
				native++
			} else {
				duplicates++
			}
		}
	}
	if native != 3 || duplicates != 0 || foregroundMetric(t, errOut.String(), "backfill_native_preparation_attempts") != 3 {
		t.Fatal("existing physical-copy deduplication repeated preparation identity", native, duplicates, errOut.String())
	}
	if snapshotTree(t, f.data) != before {
		t.Fatal("duplicate preview mutated archive")
	}
}

func TestBackfillForegroundPreparationTimeoutIsNotRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, _ := foregroundChildren(t, 3)
		calls := 0
		f.env.nativeLookupNow = func() time.Time {
			calls++
			if calls%2 == 0 {
				time.Sleep(5 * time.Second)
			}
			return time.Now()
		}
		var owner *discovery.CodexRolloutLookup
		f.env.observeBackfillLookup = func(l *discovery.CodexRolloutLookup) { owner = l }
		before := snapshotTree(t, f.data)
		var out, errOut bytes.Buffer
		if code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex"}, nil, &out, &errOut, f.env); code != 1 || !strings.Contains(errOut.String(), "native lookup I/O operation timed out") {
			t.Fatal(code, errOut.String())
		}
		if calls != 2 {
			t.Fatalf("failed bounded I/O was retried: clock calls=%d", calls)
		}
		if owner == nil {
			t.Fatal("missing lookup owner")
		}
		if used, _ := owner.NativeReadBudget().Charged(); used != 0 {
			t.Fatalf("timeout leaked charges=%d", used)
		}
		if snapshotTree(t, f.data) != before {
			t.Fatal("timeout mutated archive")
		}
	})
}

// Continuation commits a whole bounded native directory batch before yielding.
// A changing prefix therefore cannot make the same cookie repeat indefinitely.
func TestBackfillForegroundChangingPrefixAdvancesOtherCandidates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, _ := foregroundChildren(t, 16)
		original := make([][]byte, 8)
		for i := range 8 {
			path := filepath.Join(f.userHome, ".codex", "sessions", fmt.Sprintf("rollout-11111111-1111-4111-8111-%012d.jsonl", i))
			var err error
			original[i], err = os.ReadFile(path)
			must(t, err)
		}
		calls := 0
		f.env.nativeLookupNow = func() time.Time {
			calls++
			if calls%2 == 0 {
				for i := range 8 {
					path := filepath.Join(f.userHome, ".codex", "sessions", fmt.Sprintf("rollout-11111111-1111-4111-8111-%012d.jsonl", i))
					raw := bytes.ReplaceAll(original[i], []byte("Synthetic own prompt"), []byte(fmt.Sprintf("Synthetic changed prompt %d", calls)))
					must(t, os.WriteFile(path, raw, 0600))
				}
				// A broken rewind cannot make this deterministic test spin forever.
				delay := 500 * time.Millisecond
				if calls >= 1000 {
					delay = 5 * time.Second
				}
				time.Sleep(delay)
			}
			return time.Now()
		}
		var plan backfill.Plan
		f.env.observeBackfillPlan = func(p backfill.Plan) { plan = p }
		before := snapshotTree(t, f.data)
		var out, errOut bytes.Buffer
		if code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex"}, nil, &out, &errOut, f.env); code != 0 {
			t.Fatal(code, errOut.String())
		}
		good, changed := 0, 0
		for _, c := range plan.Candidates {
			if c.Harness == "codex" {
				if c.Skip != "" && c.Skip != backfill.SkipSourceChanged {
					t.Fatalf("unexpected changing-prefix disposition %s %+v", c.Skip, c.Diagnostic)
				}
				if c.Skip == "" {
					good++
				} else {
					changed++
				}
			}
		}
		if good != 8 || changed != 8 || calls > 1000 {
			t.Fatalf("changing prefix starved unrelated sources: good=%d changed=%d units=%d", good, changed, calls/2)
		}
		if snapshotTree(t, f.data) != before {
			t.Fatal("changing native prefix mutated archive")
		}
		t.Logf("bounded changing-prefix units=%d, supported tail=%d; clock time simulated", calls/2, good)
	})
}

func TestBackfillForegroundPreparationCapacityFailureRemainsFailure(t *testing.T) {
	f, _ := foregroundChildren(t, 3)
	var owner *discovery.CodexRolloutLookup
	external := agentapi.PassReadBudget - 1
	f.env.observeBackfillLookup = func(l *discovery.CodexRolloutLookup) {
		owner = l
		if !l.NativeReadBudget().ReserveUnscoped(external) {
			t.Fatal("external reservation failed")
		}
	}
	before := snapshotTree(t, f.data)
	var out, errOut bytes.Buffer
	if code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex"}, nil, &out, &errOut, f.env); code != 1 || !strings.Contains(errOut.String(), "source limit") || strings.Contains(errOut.String(), "no progress") {
		t.Fatal(code, errOut.String())
	}
	if owner == nil {
		t.Fatal("missing lookup owner")
	}
	if used, _ := owner.NativeReadBudget().Charged(); used != external {
		t.Fatalf("capacity refusal reset or leaked charges: used=%d external=%d", used, external)
	}
	owner.NativeReadBudget().ReleaseUnscoped(external)
	if snapshotTree(t, f.data) != before {
		t.Fatal("capacity refusal mutated archive")
	}
}

func missingForegroundHistory(t *testing.T, f *backfillFixture) {
	t.Helper()
	path := filepath.Join(f.userHome, ".codex", "sessions", "rollout-11111111-1111-4111-8111-000000000000.jsonl")
	raw, err := os.ReadFile(path)
	must(t, err)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for i, line := range lines {
		var record map[string]any
		must(t, json.Unmarshal([]byte(line), &record))
		record["ordinal"] = i + 2
		if i == 0 {
			payload := record["payload"].(map[string]any)
			payload["history_mode"] = "paginated"
			payload["history_base"] = codexmeta.CodexHistoryPosition{RolloutID: "77777777-7777-4777-8777-777777777777", EndOrdinal: 2, EndByteOffset: 256}
		}
		encoded, err := json.Marshal(record)
		must(t, err)
		lines[i] = string(encoded)
	}
	must(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600))
}

func TestBackfillForegroundUnavailableInventoryKeepsIndependentCandidates(t *testing.T) {
	for _, tc := range []struct {
		name       string
		children   bool
		harness    string
		dependency bool
	}{
		{name: "zero child IDs", harness: "claude"},
		{name: "ordinary sources"},
		{name: "unselected children", children: true, harness: "claude"},
		{name: "mixed selected sources", children: true},
		{name: "dependent unavailable history", children: true, dependency: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *backfillFixture
			if tc.children {
				f, _ = foregroundChildren(t, 3)
			} else {
				f, _ = newImportFixture(t)
			}
			if tc.dependency {
				missingForegroundHistory(t, f)
			}
			unavailable := filepath.Join(f.userHome, ".codex", "archived_sessions")
			must(t, os.RemoveAll(unavailable))
			must(t, os.WriteFile(unavailable, []byte("synthetic unavailable directory"), 0600))
			resources := decorateForegroundSource(t, f, nil)
			f.env = withTrace(f.env, "1")
			var owner *discovery.CodexRolloutLookup
			const external = int64(17)
			f.env.observeBackfillLookup = func(l *discovery.CodexRolloutLookup) {
				owner = l
				if !l.NativeReadBudget().ReserveUnscoped(external) {
					t.Fatal("external reservation failed")
				}
			}
			var plan backfill.Plan
			f.env.observeBackfillPlan = func(p backfill.Plan) { plan = p }
			bucket, err := f.env.OpenStore(mustLoadConfig(t, f.data))
			must(t, err)
			before, beforeNative := snapshotTree(t, f.data), snapshotTree(t, f.userHome)
			args := []string{"backfill", "--dry-run", "--json"}
			if tc.harness != "" {
				args = append(args, "--harness", tc.harness)
			}
			var out, errOut bytes.Buffer
			if code := Run(args, nil, &out, &errOut, f.env); code != 0 {
				t.Fatal("independent candidates lost", code, errOut.String())
			}
			counts := map[string]int{}
			pending := 0
			for _, c := range plan.Candidates {
				if c.Skip == "" {
					counts[c.Harness]++
				}
				if c.Harness == "codex" && c.Skip == backfill.SkipRelatedHistory {
					pending++
					if c.Diagnostic == nil || c.Diagnostic.Detail != backfill.DetailNativeSourceUnavailable || c.Diagnostic.Action != backfill.ActionReviewSource {
						t.Fatalf("dependent history lost source-access refusal: %+v", c.Diagnostic)
					}
				}
			}
			if counts["claude"] != 8 || tc.dependency && (pending != 1 || counts["codex"] != 2) || !tc.dependency && pending != 0 {
				t.Fatal("independent/dependent dispositions changed", counts, pending)
			}
			if tc.children && tc.harness == "" && !tc.dependency && counts["codex"] != 3 {
				t.Fatal("independently proven native candidates were dropped", counts)
			}
			if tc.children {
				if plan.NativeLookupIncomplete == nil || plan.NativeLookupIncomplete.Detail != backfill.DetailLookupNoProgress || plan.NativeLookupIncomplete.Action != backfill.ActionRetry || !strings.Contains(out.String(), `"native_lookup_incomplete"`) {
					t.Fatal("incomplete inventory was hidden", out.String())
				}
				if foregroundMetric(t, errOut.String(), "backfill_native_preparation_attempts") != 3 || foregroundMetric(t, errOut.String(), "codex_lookup_work_units") > 150 {
					t.Fatal("incomplete inventory repeated work", errOut.String())
				}
			} else if plan.NativeLookupIncomplete != nil || strings.Contains(out.String(), `"native_lookup_incomplete"`) || strings.Contains(errOut.String(), "backfill_native_preparation_attempts") {
				t.Fatal("zero requested IDs performed native preparation")
			}
			if plan.UnreadableFolders != 0 || len(plan.UnreadableStores) != 0 {
				t.Fatal("inventory stop fabricated unreadable counts")
			}
			resources.check(t)
			if owner == nil {
				t.Fatal("missing lookup owner")
			}
			if used, peak := owner.NativeReadBudget().Charged(); used != external || peak > agentapi.PassReadBudget {
				t.Fatal("incomplete lookup leaked/reset charges", used, peak)
			}
			if _, err := owner.Thread(t.Context(), "11111111-1111-4111-8111-000000000000"); !errors.Is(err, agentapi.ErrClosed) {
				t.Fatal("incomplete lookup owner remains open", err)
			}
			owner.NativeReadBudget().ReleaseUnscoped(external)
			objects, err := bucket.List(t.Context(), "")
			must(t, err)
			if len(objects) != 0 || before != snapshotTree(t, f.data) || beforeNative != snapshotTree(t, f.userHome) {
				t.Fatal("partial dry run changed config/archive/native/bucket")
			}
		})
	}
}
