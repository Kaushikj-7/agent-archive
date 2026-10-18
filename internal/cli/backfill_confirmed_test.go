package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestBackfillSelectedParentStillRequiresCurrentPublicationPolicy(t *testing.T) {
	for _, excluded := range []bool{false, true} {
		t.Run(fmt.Sprint(excluded), func(t *testing.T) {
			f, bucket := newImportFixture(t)
			changed := false
			f.env.backfillCheckpoint = func(step string) error {
				if step == "uploading" && excluded {
					cfg := mustLoadConfig(t, f.data)
					for i := range cfg.Archive.Projects {
						cfg.Archive.Projects[i].Included = false
					}
					must(t, config.Save(f.data, cfg))
					changed = true
				}
				return nil
			}
			// Restrict below the8-owner group so the exclusion occurs before
			// any selected parent or its queued child reaches publication.
			args := []string{"--yes", "--harness", "claude", "--project", filepath.Join(f.userHome, "agent-archive")}
			out, stderr, code := f.importRun(t, nil, false, args...)
			if excluded {
				if !changed || code != 1 || !strings.Contains(out, "current policy prevents publication") || strings.Contains(out, "Uploaded ") {
					t.Fatal("excluded selected owner reported completion", code, stderr, out)
				}
				objects, err := bucket.List(t.Context(), "")
				must(t, err)
				if len(objects) != 0 {
					t.Fatal("current policy did not veto selected publication")
				}
				return
			}
			if code != 0 || !strings.Contains(out, "Uploaded ") {
				t.Fatal("eligible selected parent/child failed", code, stderr, out)
			}
			regs, err := state.OpenReadOnly(f.data).LoadRegistrations()
			must(t, err)
			children := 0
			for _, reg := range regs {
				if reg.InBatch(firstImport) && reg.ParentSessionID != "" {
					children++
				}
			}
			if children != 2 {
				t.Fatal("selected eligible queued children were not materialized", children)
			}
		})
	}
}

func TestBackfillForegroundGroupsPreserveOtherImportEvidence(t *testing.T) {
	f, _ := foregroundChildren(t, 9)
	_, stderr, code := f.importRun(t, nil, false, "--yes", "--background", "--harness", "claude")
	if code != 0 {
		t.Fatal("background preparation", code, stderr)
	}
	local := state.OpenReadOnly(f.data)
	stages, _, err := local.ImportStages(t.Context(), "", 50)
	must(t, err)
	if len(stages) < 2 {
		t.Fatal("missing unrelated staged parent/child cohort")
	}
	before := map[string]string{}
	for _, stage := range stages {
		before[stage.Registration.ArchiveSessionID] = snapshotTree(t, filepath.Join(f.data, "sessions", stage.Registration.ArchiveSessionID))
	}
	queued := snapshotTree(t, filepath.Join(f.data, "subagent-candidates"))
	if queued == "" {
		t.Fatal("missing unrelated queued child")
	}
	queuedBytes := map[string]string{}
	entries, err := os.ReadDir(filepath.Join(f.data, "subagent-candidates"))
	must(t, err)
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(f.data, "subagent-candidates", entry.Name()))
		must(t, err)
		queuedBytes[entry.Name()] = string(raw)
	}
	bucket, err := f.env.OpenStore(mustLoadConfig(t, f.data))
	must(t, err)
	objects, err := bucket.List(t.Context(), "")
	must(t, err)
	if len(objects) != 0 {
		t.Fatal("background import uploaded archive content")
	}
	out, stderr, code := f.importRun(t, nil, false, "--yes", "--harness", "codex")
	if code != 0 {
		t.Fatal("foreground groups", code, stderr, out)
	}
	entries, err = os.ReadDir(filepath.Join(f.data, "subagent-candidates"))
	must(t, err)
	if len(entries) != len(queuedBytes) {
		t.Fatal("unselected parent lost queued child membership")
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(f.data, "subagent-candidates", entry.Name()))
		must(t, err)
		if string(raw) != queuedBytes[entry.Name()] {
			t.Fatal("unselected parent changed queued child bytes or provenance")
		}
	}
	if queued != snapshotTree(t, filepath.Join(f.data, "subagent-candidates")) {
		t.Log("existing global admission recovery rewrote filesystem stamps; exact queued child bytes and provenance remained unchanged")
	}
	for _, stage := range stages {
		reg := stage.Registration
		if before[reg.ArchiveSessionID] != snapshotTree(t, filepath.Join(f.data, "sessions", reg.ArchiveSessionID)) {
			t.Fatal("unrelated durable evidence changed")
		}
		key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
		must(t, err)
		if _, err := bucket.Get(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatal("unrelated staged owner reached remote storage", err)
		}
	}
	if !strings.Contains(out, "Freed staging capacity") {
		t.Fatal("bounded foreground group did not publish before registration finished", out)
	}
}

func TestBackfillConfirmedResumesFrozenStagesWithoutNativeInput(t *testing.T) {
	for _, checkpoint := range []string{"batch saved", "committed", "staged", "registered", "uploading"} {
		t.Run(checkpoint, func(t *testing.T) {
			f, bucket, mapping := newHistoricalOnlyFixture(t)
			f.env.backfillCheckpoint = func(step string) error {
				if step == checkpoint {
					return errors.New("synthetic durable-boundary interruption")
				}
				return nil
			}
			args := []string{"--yes", "--mapping-file", mapping}
			_, stderr, code := f.importRun(t, nil, false, args...)
			if code != 1 || !strings.Contains(stderr, "synthetic durable-boundary interruption") {
				t.Fatal("boundary not reached", code, stderr)
			}
			if checkpoint == "staged" || checkpoint == "registered" || checkpoint == "uploading" {
				must(t, os.RemoveAll(filepath.Join(f.userHome, ".claude")))
			}
			f.env.backfillCheckpoint = nil
			out, stderr, code := f.importRun(t, nil, false, args...)
			if code != 0 {
				t.Fatal("foreground continuation", code, stderr, out)
			}
			local := state.OpenReadOnly(f.data)
			regs, err := local.LoadRegistrations()
			must(t, err)
			if len(regs) != 1 || regs[0].HistoricalProject == nil {
				t.Fatal("frozen logical owner changed", regs)
			}
			published, err := local.LoadPublishedState(regs[0].ArchiveSessionID)
			must(t, err)
			metadata, found, err := published.LastPublishedMetadata()
			must(t, err)
			if !found {
				t.Fatal("continuation returned without publication")
			}
			bundle, err := reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
			must(t, err)
			if bundle.Capture.CapturedAt != regs[0].AdmittedAt || bundle.NativeSessionID != regs[0].NativeSessionID {
				t.Fatal("frozen capture provenance changed")
			}
			batches, err := backfill.LoadBatches(f.data)
			must(t, err)
			if len(batches) != 1 || len(batches[0].Sessions) != 1 || batches[0].CompletedAt == nil {
				t.Fatal("continuation duplicated or lost batch ownership", batches)
			}
		})
	}
}

func TestBackfillConfirmedCompletesMixedNativeWindows(t *testing.T) {
	for _, count := range []int{3, 513} {
		t.Run(fmt.Sprint(count), func(t *testing.T) { checkConfirmedMixedNativeWindows(t, count) })
	}
}

func TestBackfillConfirmedStagingRenewsAcrossElapsedScopes(t *testing.T) {
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
		resources := decorateForegroundSource(t, f, nil)
		f.env = withTrace(f.env, "1")
		started := time.Now()
		out, stderr, code := f.importRun(t, nil, false, "--yes", "--harness", "codex")
		if code != 0 {
			var logFailure func(error)
			logFailure = func(cause error) {
				if cause == nil {
					return
				}
				t.Logf("native failure cause: %T %#v", cause, cause)
				if joined, ok := cause.(interface{ Unwrap() []error }); ok {
					for _, child := range joined.Unwrap() {
						logFailure(child)
					}
					return
				}
				logFailure(errors.Unwrap(cause))
			}
			for _, failure := range resources.failures {
				logFailure(failure)
			}
			t.Fatal("renewable confirmed staging", code, stderr, out)
		}
		resources.check(t)
		regs, err := state.OpenReadOnly(f.data).LoadRegistrations()
		must(t, err)
		verified := 0
		for _, reg := range regs {
			if reg.InBatch(firstImport) {
				summary, _, err := state.OpenReadOnly(f.data).LoadPublishedSummary(reg.ArchiveSessionID)
				must(t, err)
				if summary.Published {
					verified++
				}
			}
		}
		if verified != 16 || time.Since(started) <= 5*time.Second || foregroundMetric(t, stderr, "codex_lookup_scopes") < 3 {
			t.Fatal("confirmed work did not cross bounded native scopes", verified, time.Since(started), calls)
		}
		t.Logf("verified=%d simulated=%s native_clock_calls=%d; actual native files/SQL, virtual scheduling correctness only", verified, time.Since(started), calls)
	})
}

func checkConfirmedMixedNativeWindows(t *testing.T, count int) {
	t.Helper()
	f, _ := foregroundChildren(t, count)
	for i := range count {
		path := filepath.Join(f.userHome, ".codex", "sessions", fmt.Sprintf("rollout-11111111-1111-4111-8111-%012d.jsonl", i))
		raw, err := os.ReadFile(path)
		must(t, err)
		lines := strings.SplitN(string(raw), "\n", 2)
		var header map[string]any
		must(t, json.Unmarshal([]byte(lines[0]), &header))
		payload := header["payload"].(map[string]any)
		if i%3 != 0 {
			delete(payload, "session_id")
			delete(payload, "parent_thread_id")
			delete(payload, "subagent_history_start_ordinal")
			payload["source"] = "cli"
			if i%3 == 1 {
				payload["forked_from_id"] = "22222222-2222-4222-8222-222222222222"
				payload["forked_from_ordinal_exclusive"] = 1
			}
		}
		headerBytes, err := json.Marshal(header)
		must(t, err)
		must(t, os.WriteFile(path, []byte(string(headerBytes)+"\n"+lines[1]), 0600))
	}
	var plan backfill.Plan
	f.env.observeBackfillPlan = func(p backfill.Plan) { plan = p }
	store := state.OpenReadOnly(f.data)
	beforeOwners, err := store.LoadRegistrations()
	must(t, err)
	beforeNative := snapshotTree(t, f.userHome)
	resources := decorateForegroundSource(t, f, nil)
	highWater, stagedBytes := 0, int64(0)
	f.env.backfillCheckpoint = func(step string) error {
		if step == "staged" {
			paths, err := filepath.Glob(filepath.Join(f.data, "sessions", "*", "import-stage", "stage.json"))
			must(t, err)
			highWater = max(highWater, len(paths))
			var total int64
			for _, path := range paths {
				info, err := os.Stat(path)
				must(t, err)
				total += info.Size()
			}
			stagedBytes = max(stagedBytes, total)
		}
		return nil
	}
	started := time.Now()
	out, stderr, code := f.importRun(t, nil, false, "--yes", "--harness", "codex")
	if code != 0 {
		t.Fatalf("confirmed completion: %d %s\n%s", code, stderr, out)
	}
	assertForegroundCandidates(t, plan, count)
	resources.check(t)
	if beforeNative != snapshotTree(t, f.userHome) {
		t.Fatal("confirmed import changed native input")
	}
	regs, err := store.LoadRegistrations()
	must(t, err)
	if len(regs) != len(beforeOwners)+count {
		t.Fatalf("global registered=%d want=%d (%d prior owners plus %d imported)", len(regs), len(beforeOwners)+count, len(beforeOwners), count)
	}
	for _, original := range beforeOwners {
		current, found, err := store.LoadRegistration(original.ArchiveSessionID)
		must(t, err)
		before, err := json.Marshal(original)
		must(t, err)
		after, err := json.Marshal(current)
		must(t, err)
		if !found || string(before) != string(after) {
			t.Fatal("unrelated logical owner changed")
		}
	}
	owned := regs[:0]
	for _, reg := range regs {
		if reg.InBatch(firstImport) {
			owned = append(owned, reg)
		}
	}
	regs = owned
	if len(regs) != count {
		t.Fatalf("registered=%d want=%d", len(regs), count)
	}
	bucket, err := f.env.OpenStore(mustLoadConfig(t, f.data))
	must(t, err)
	for _, reg := range regs {
		published, err := store.LoadPublishedState(reg.ArchiveSessionID)
		must(t, err)
		_, _, found := published.LastPublished()
		if !found {
			t.Fatal("registered source not published", reg.ArchiveSessionID)
		}
		metadata, _, err := published.LastPublishedMetadata()
		must(t, err)
		bundle, err := reader.LoadSource(t.Context(), bucket, metadata, reader.Limits{})
		must(t, err)
		if bundle.NativeSessionID != reg.NativeSessionID {
			t.Fatalf("published native identity=%q want=%q", bundle.NativeSessionID, reg.NativeSessionID)
		}
		if !bundle.Capture.CapturedAt.Equal(reg.AdmittedAt) {
			t.Fatalf("published capture instant=%v (%s) want=%v (%s)", bundle.Capture.CapturedAt, bundle.Capture.CapturedAt.Location(), reg.AdmittedAt, reg.AdmittedAt.Location())
		}
		if _, found, err := store.LoadImportStage(t.Context(), reg); err != nil || found {
			t.Fatal("settled stage not released", found, err)
		}
		if _, found, err := store.LoadPending(reg.ArchiveSessionID); err != nil || found {
			t.Fatal("verified logical owner still pending", found, err)
		}
	}
	var sourceBytes int64
	for _, candidate := range plan.Imported() {
		sourceBytes += candidate.Bytes
	}
	if highWater > 8 || stagedBytes >= 1<<30 {
		t.Fatal("foreground stage group exceeded its bound", highWater, stagedBytes)
	}
	t.Logf("logical=%d registered=%d remotely verified=%d pending=0 stage_high_water=%d/%dwire_bytes source_bytes=%d provider_passes=%d source_snapshots=%d elapsed=%s; synthetic memory provider, not platform/provider performance qualification", count, len(regs), len(regs), highWater, stagedBytes, sourceBytes, resources.passes, resources.snapshots, time.Since(started))
}
