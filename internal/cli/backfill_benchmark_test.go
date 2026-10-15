package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type commandScenarioKind string

const (
	commandScenarioRecent        commandScenarioKind = "recent"
	commandScenarioOld           commandScenarioKind = "old"
	commandScenarioLarge         commandScenarioKind = "large"
	commandScenarioRelated       commandScenarioKind = "related"
	commandScenarioAncestry      commandScenarioKind = "ancestry"
	commandScenarioToolHeavy     commandScenarioKind = "tool-heavy"
	commandScenarioRetainedHeavy commandScenarioKind = "retained-heavy"
)

type commandStateKind string

const (
	commandStateWarm    commandStateKind = "warm"
	commandStateChanged commandStateKind = "changed"
)

// benchmarkTraceMetric reads one content-free integer aggregate. Missing
// counters are zero, including on an original binary without cache support.
func benchmarkTraceMetric(output, name string) int {
	fields := strings.Fields(output)
	for i, field := range fields {
		if field == name && i+1 < len(fields) {
			value, err := strconv.Atoi(strings.TrimSuffix(fields[i+1], ","))
			if err == nil {
				return value
			}
		}
	}
	return 0
}

func TestBenchmarkTraceMetricReadsOptionalAggregates(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   int
	}{
		{"original binary without cache metrics", 0},
		{"native totals: inspection_cache_hits 1500, adapter_invocations 1", 1500},
		{"native totals: inspection_cache_hits 1500", 1500},
	} {
		if got := benchmarkTraceMetric(tc.output, "inspection_cache_hits"); got != tc.want {
			t.Fatalf("metric %d want %d in %q", got, tc.want, tc.output)
		}
	}
}

// Object addresses include generated archive IDs and disposable project identity.
// The complete loaded source below attests their semantic content instead.
func benchmarkArtifactEvidence(bundle archive.SourceBundle, reg archive.SessionRegistration, metadata archive.Metadata) ([]byte, error) {
	metadata.SourceBundle = archive.SourceReference{}
	if metadata.History != nil && len(metadata.History.Preserved) != 0 {
		return nil, fmt.Errorf("benchmark must verify preserved source contents before normalizing their addresses")
	}
	return json.Marshal(map[string]any{"bundle": bundle, "metadata": metadata, "started_at": reg.SessionStartedAt, "started_at_source": reg.StartedAtSource, "origin": reg.Origin})
}

func benchmarkPlanEvidence(candidates []backfill.Candidate) ([]byte, error) {
	type decision struct {
		Harness         string
		NativeSessionID string
		Skip            backfill.SkipReason
		StartedAt       time.Time
		StartedAtSource archive.StartedAtSource
		NativeChild     bool
		RelatedHistory  bool
		ParentNativeID  string
		RootNativeID    string
		SourceKind      archive.SourceKind
		Bytes           int64
	}
	rows := make([]decision, 0, len(candidates))
	for _, c := range candidates {
		rows = append(rows, decision{c.Harness, c.NativeSessionID, c.Skip, c.StartedAt, c.StartedAtSource, c.NativeChild, c.RelatedHistory, c.ParentNativeID, c.RootNativeID, c.SourceKind, c.Bytes})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Harness != rows[j].Harness {
			return rows[i].Harness < rows[j].Harness
		}
		return rows[i].NativeSessionID < rows[j].NativeSessionID
	})
	return json.Marshal(rows)
}

func TestBenchmarkPlanEvidenceDistinguishesEqualCountSelections(t *testing.T) {
	before := []backfill.Candidate{{Harness: "codex", NativeSessionID: "first"}, {Harness: "codex", NativeSessionID: "second", Skip: backfill.SkipFilteredOut}}
	after := append([]backfill.Candidate(nil), before...)
	after[0].Skip = backfill.SkipFilteredOut
	after[1].Skip = ""
	a, err := benchmarkPlanEvidence(before)
	must(t, err)
	b, err := benchmarkPlanEvidence(after)
	must(t, err)
	if bytes.Equal(a, b) {
		t.Fatal("equal totals concealed different selected identities")
	}
	after = append([]backfill.Candidate(nil), before...)
	after[0].StartedAtSource = archive.StartedAtSourceFileCreated
	b, err = benchmarkPlanEvidence(after)
	must(t, err)
	if bytes.Equal(a, b) {
		t.Fatal("planning provenance was omitted")
	}
}

func TestBenchmarkArtifactEvidenceDistinguishesMetadataAndCapture(t *testing.T) {
	bundle := archive.SourceBundle{NativeRecords: []map[string]any{{"type": "synthetic"}}}
	metadata := archive.Metadata{Title: "original"}
	a, err := benchmarkArtifactEvidence(bundle, archive.SessionRegistration{}, metadata)
	must(t, err)
	metadata.Title = "changed"
	b, err := benchmarkArtifactEvidence(bundle, archive.SessionRegistration{}, metadata)
	must(t, err)
	if bytes.Equal(a, b) {
		t.Fatal("same records concealed changed uploaded metadata")
	}
	metadata.Title = "original"
	bundle.Capture.FilterVersion = "changed"
	b, err = benchmarkArtifactEvidence(bundle, archive.SessionRegistration{}, metadata)
	must(t, err)
	if bytes.Equal(a, b) {
		t.Fatal("capture provenance was omitted")
	}
}

// History ordinals are serialized separately from records and determine which
// retained records belong to the selected task. Equal records and spans alone
// cannot establish equivalent uploaded history artifacts.
func TestBenchmarkArtifactEvidenceIncludesHistoryOrdinals(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000001"
	ownStart := uint64(3)
	bundle := archive.SourceBundle{
		SchemaVersion: archive.HistorySourceSchemaVersion, NativeSessionID: id,
		Capture: archive.SourceCapture{Harness: archive.Harness{Name: "codex"}},
		History: &archive.SourceHistory{ActiveRolloutID: id, ThreadID: id, OwnStart: &ownStart,
			Spans: []archive.HistorySpan{{RolloutID: id, ThreadID: id, EndRecord: 2, EndOrdinal: 4}}},
		NativeRecords: []map[string]any{{"type": "session_meta"}, {"type": "response_item"}},
		Ordinals:      []uint64{0, 2},
	}
	must(t, bundle.ValidateHistory())
	before, err := benchmarkArtifactEvidence(bundle, archive.SessionRegistration{}, archive.Metadata{})
	must(t, err)
	if bundle.OwnRecord(1) {
		t.Fatal("initial ordinal unexpectedly belongs to the selected task")
	}
	bundle.Ordinals = []uint64{0, 3}
	must(t, bundle.ValidateHistory())
	after, err := benchmarkArtifactEvidence(bundle, archive.SessionRegistration{}, archive.Metadata{})
	must(t, err)
	if !bundle.OwnRecord(1) || bytes.Equal(before, after) {
		t.Fatal("changed record ownership was absent from artifact evidence")
	}
}

// TestBackfillCommandMeasurement is a subprocess entry point, not a timed Go
// benchmark loop. Each sample gets a fresh synthetic home and deterministic
// provider. Fixture construction and artifact verification are outside Run's timer.
func TestBackfillCommandMeasurement(t *testing.T) {
	scenario := commandScenarioKind(os.Getenv("AA_BACKFILL_SCENARIO"))
	if scenario == "" {
		t.Skip("explicit benchmark subprocess only")
	}
	count := 1500
	if os.Getenv("AA_BACKFILL_SMALL") == "1" {
		count = 12
	}
	if scenario == commandScenarioToolHeavy || scenario == commandScenarioRetainedHeavy {
		count = 32
	}
	if scenario == commandScenarioLarge {
		count = 32
	}
	if scenario == commandScenarioRelated || scenario == commandScenarioAncestry {
		count = 5
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	must(t, os.Chmod(home, 0700))
	env := testEnv(t, home, backfillNow)
	userHome, err := env.UserHomeDir()
	must(t, err)
	userHome, err = filepath.EvalSymlinks(userHome)
	must(t, err)
	// Retained-byte capture evidence includes the native cwd's encoded length.
	// Keep disposable home width fixed as well as the raw header framing, so
	// complete capture provenance compares without dropping or rewriting counts.
	const homeWidth = 192
	if len(userHome)+1 >= homeWidth {
		t.Fatalf("benchmark home prefix exceeds frozen width: %d", len(userHome))
	}
	userHome = filepath.Join(userHome, strings.Repeat("h", homeWidth-len(userHome)-1))
	must(t, os.MkdirAll(userHome, 0700))
	env.UserHomeDir = func() (string, error) { return userHome, nil }
	project := filepath.Join(userHome, "project")
	must(t, os.MkdirAll(filepath.Join(project, ".git"), 0700))
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, RetentionDays: 90, Archive: archive.Config{SchemaVersion: 1, MachineID: "synthetic-machine", Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: backfillNow.UTC()}}}}
	must(t, config.Save(home, cfg))
	bucket := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	env.repoKey = func(string) string { return "" }
	if os.Getenv("AA_BACKFILL_TRACE") == "1" {
		env.LookupEnv = func(name string) (string, bool) { return "1", name == envTrace }
	}
	dir := filepath.Join(userHome, ".codex", "sessions")
	must(t, os.MkdirAll(dir, 0700))
	expected := map[string]bool{}
	var inputBytes int64
	var parentBytes int64
	for i := range count {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		at := backfillNow.Add(-48 * time.Hour)
		old := scenario == commandScenarioOld && i%10 != 0 || scenario == commandScenarioRecent && i%10 == 0 || (scenario == commandScenarioRelated || scenario == commandScenarioAncestry) && i == 0
		if old {
			at = backfillNow.Add(-30 * 24 * time.Hour)
		}
		meta := map[string]any{"id": id, "timestamp": at.UTC().Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "dev"}
		if scenario == commandScenarioRelated && i > 0 {
			meta["forked_from_id"] = "00000000-0000-4000-8000-000000000001"
			meta["forked_from_ordinal_exclusive"] = 3
		}
		ordinal := 0
		if scenario == commandScenarioAncestry && i > 0 {
			parent := "00000000-0000-4000-8000-000000000001"
			meta["parent_thread_id"] = parent
			meta["session_id"] = parent
			meta["root_thread_id"] = parent
			meta["subagent_history_start_ordinal"] = 3
			meta["source"] = map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}
			meta["history_base"] = map[string]any{"thread_id": parent, "end_ordinal_exclusive": 3, "end_byte_offset": parentBytes}
			ordinal = 3
		}
		h, e := json.Marshal(map[string]any{"type": "session_meta", "ordinal": ordinal, "payload": meta})
		must(t, e)
		if len(h) > 1024 {
			t.Fatal("fixture header exceeds fixed framing")
		}
		h = append(h, bytes.Repeat([]byte(" "), 1024-len(h))...)
		path := filepath.Join(dir, "rollout-"+id+".jsonl")
		f, e := os.Create(path)
		must(t, e)
		n, e := f.Write(append(h, '\n'))
		must(t, e)
		inputBytes += int64(n)
		row := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic prompt"}]}}` + "\n"
		rows := 1 + (i%8)*8
		if scenario == commandScenarioRelated || scenario == commandScenarioAncestry {
			rows = 1
			if i > 0 && scenario == commandScenarioRelated {
				inherited := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"00000000-0000-4000-8000-000000000001","started_at":%q}}`+"\n"+row, backfillNow.Add(-30*24*time.Hour).UTC().Format(time.RFC3339Nano))
				n, e = f.WriteString(inherited)
				must(t, e)
				inputBytes += int64(n)
			}
			row = fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+row, id, at.UTC().Format(time.RFC3339Nano))
		}
		if scenario == commandScenarioAncestry {
			row = fmt.Sprintf(`{"type":"event_msg","ordinal":%d,"payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+`{"type":"response_item","ordinal":%d,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic own task"}]}}`+"\n", ordinal+1, id, at.UTC().Format(time.RFC3339Nano), ordinal+2)
		}
		for range rows {
			n, e = f.WriteString(row)
			must(t, e)
			inputBytes += int64(n)
		}
		if scenario == commandScenarioRecent || scenario == commandScenarioOld || scenario == commandScenarioToolHeavy || scenario == commandScenarioRetainedHeavy {
			sizes := []int64{256 << 10, 256 << 10, 512 << 10, 1 << 20, 1 << 20, 1 << 20, 1 << 20, 2 << 20, 4 << 20, 8 << 20}
			target := sizes[i%len(sizes)]
			if scenario == commandScenarioToolHeavy || scenario == commandScenarioRetainedHeavy {
				target = 4 << 20
			}
			if os.Getenv("AA_BACKFILL_SMALL") == "1" {
				target /= 256
			}
			bulk := `{"type":"response_item","payload":{"type":"function_call_output","call_id":"synthetic","output":"` + strings.Repeat("synthetic tool stdout ", 800) + `"}}` + "\n"
			if scenario == commandScenarioRetainedHeavy || i%3 == 0 && scenario != commandScenarioToolHeavy {
				bulk = `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("Synthetic retained explanation. ", 500) + `"}]}}` + "\n"
			}
			for wrote := int64(0); wrote < target; {
				n, e = f.WriteString(bulk)
				must(t, e)
				wrote += int64(n)
				inputBytes += int64(n)
			}
		}
		if scenario == commandScenarioLarge && i == 0 {
			// Large native tool results exceed the existing 64-KiB string cap.
			// This keeps retained output admissible while all 210 MiB of raw
			// source must still be read, filtered and verified. Small records
			// would instead accumulate beyond the filtered transcript limit.
			tool := `{"type":"response_item","payload":{"type":"function_call_output","call_id":"synthetic","output":"` + strings.Repeat("x", 1<<20) + `"}}` + "\n"
			target := int64(210 << 20)
			if os.Getenv("AA_BACKFILL_SMALL") == "1" {
				target = 1 << 20
			}
			for wrote := int64(0); wrote < target; {
				n, e = f.WriteString(tool)
				must(t, e)
				wrote += int64(n)
				inputBytes += int64(n)
			}
		}
		if i == 0 {
			info, e := f.Stat()
			must(t, e)
			parentBytes = info.Size()
		}
		must(t, f.Close())
		if !old {
			expected[id] = true
		}
	}
	mode := os.Getenv("AA_BACKFILL_MODE")
	args := []string{"backfill", "--since", "7d", "--harness", "codex", "--include-temp"}
	if mode == "import" {
		args = append(args, "--yes")
	} else {
		args = append(args, "--dry-run")
	}
	stateName := commandStateKind(os.Getenv("AA_BACKFILL_STATE"))
	primeAttempts, primeHits := 0, 0
	primeCacheSupported := false
	if stateName == commandStateWarm || stateName == commandStateChanged {
		// Async bounded cache queues may drop observations in a large first
		// pass. Verify full coverage outside the timer before naming it warm.
		// An original binary has no cache counters and needs only page priming.
		ordinary := scenario != commandScenarioRelated && scenario != commandScenarioAncestry
		for attempt := range 8 {
			var primeOut, primeErr bytes.Buffer
			if code := Run([]string{"backfill", "--dry-run", "--since", "7d", "--harness", "codex", "--include-temp"}, nil, &primeOut, &primeErr, withTrace(env, "1")); code != 0 {
				t.Fatalf("prime %d: %s", code, primeErr.String())
			}
			primeAttempts = attempt + 1
			primeCacheSupported = benchmarkTraceMetric(primeErr.String(), "inspection_cache_lookups") > 0
			primeHits = benchmarkTraceMetric(primeErr.String(), "inspection_cache_hits")
			if !primeCacheSupported || !ordinary {
				break
			}
			if primeHits == count && benchmarkTraceMetric(primeErr.String(), "adapter_invocations") == 0 && benchmarkTraceMetric(primeErr.String(), "filter_input_bytes") == 0 {
				break
			}
			if primeAttempts == 8 {
				t.Fatalf("cache priming incomplete: hits %d want %d: %s", primeHits, count, primeErr.String())
			}
		}
		if stateName == commandStateChanged {
			f, e := os.OpenFile(filepath.Join(dir, "rollout-00000000-0000-4000-8000-000000000002.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
			must(t, e)
			n, e := f.WriteString(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Synthetic changed-file evidence"}]}}` + "\n")
			must(t, e)
			inputBytes += int64(n)
			must(t, f.Close())
		}
	}
	var actualPlan backfill.Plan
	observedPlan := false
	env.observeBackfillPlan = func(plan backfill.Plan) { actualPlan = plan; observedPlan = true }
	var stdout, stderr bytes.Buffer
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	type checkpoint struct {
		First float64 `json:"first_seconds"`
		Last  float64 `json:"last_seconds"`
		Calls int     `json:"calls"`
	}
	checkpoints := map[string]checkpoint{}
	cpuBefore, _, err := backfillProcessResources()
	must(t, err)
	start := time.Now()
	env.backfillCheckpoint = func(step string) error {
		value := checkpoints[step]
		offset := time.Since(start).Seconds()
		if value.Calls == 0 {
			value.First = offset
		}
		value.Last = offset
		value.Calls++
		checkpoints[step] = value
		return nil
	}
	code := Run(args, nil, &stdout, &stderr, env)
	elapsed := time.Since(start)
	cpuAfter, commandRSS, err := backfillProcessResources()
	must(t, err)
	runtime.ReadMemStats(&after)
	if code != 0 {
		t.Fatalf("command failed %d: %s\n%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), fmt.Sprintf("Total: %d sessions", len(expected))) {
		t.Fatalf("unexpected plan counts: %s", stdout.String())
	}
	// Derive skip evidence from the actual rendered command rather than only
	// reporting the fixture's expected count. All unselected fixtures are old.
	wantSkipped := count - len(expected)
	skipped := 0
	for line := range strings.SplitSeq(stdout.String(), "\n") {
		if strings.Contains(line, "not matching the filters") {
			var label string
			if _, err := fmt.Sscan(line, &skipped, &label); err != nil {
				t.Fatal(err)
			}
		}
	}
	if skipped != wantSkipped {
		t.Fatalf("rendered date skips %d want %d: %s", skipped, wantSkipped, stdout.String())
	}
	if !observedPlan || len(actualPlan.Candidates) != count {
		t.Fatalf("missing complete actual plan: observed=%v candidates=%d want=%d", observedPlan, len(actualPlan.Candidates), count)
	}
	selected := 0
	seenPlan := map[string]bool{}
	for _, candidate := range actualPlan.Candidates {
		if seenPlan[candidate.NativeSessionID] {
			t.Fatalf("duplicate planned identity %s", candidate.NativeSessionID)
		}
		seenPlan[candidate.NativeSessionID] = true
		if candidate.Skip == "" {
			selected++
			if !expected[candidate.NativeSessionID] {
				t.Fatalf("unexpected planned identity %s", candidate.NativeSessionID)
			}
		} else if expected[candidate.NativeSessionID] || candidate.Skip != backfill.SkipFilteredOut {
			t.Fatalf("unexpected candidate decision %s: %s", candidate.NativeSessionID, candidate.Skip)
		}
	}
	if selected != len(expected) {
		t.Fatalf("actual selected %d want %d", selected, len(expected))
	}
	planRaw, e := benchmarkPlanEvidence(actualPlan.Candidates)
	must(t, e)
	planDigest := sha256.Sum256(planRaw)
	imported := 0
	var normalized []string
	if mode == "import" {
		regs, e := state.OpenReadOnly(home).LoadRegistrations()
		must(t, e)
		if len(regs) != len(expected) {
			t.Fatalf("registrations %d want %d: %s %s", len(regs), len(expected), stdout.String(), stderr.String())
		}
		replacements := []string{project, "$PROJECT", archive.ProjectID(project), "$PROJECT_ID"}
		for _, owner := range regs {
			replacements = append(replacements, owner.ArchiveSessionID, "$ARCHIVE:"+owner.NativeSessionID)
		}
		normalize := strings.NewReplacer(replacements...)
		seenImports := map[string]bool{}
		for _, reg := range regs {
			if seenImports[reg.NativeSessionID] {
				t.Fatalf("duplicate imported identity %s", reg.NativeSessionID)
			}
			seenImports[reg.NativeSessionID] = true
			if !expected[reg.NativeSessionID] {
				t.Fatalf("unexpected imported identity %s", reg.NativeSessionID)
			}
			key, e := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
			must(t, e)
			m, e := reader.ReadMetadata(t.Context(), bucket, key)
			must(t, e)
			bundle, e := reader.LoadSource(t.Context(), bucket, m, reader.Limits{})
			must(t, e)
			raw, e := benchmarkArtifactEvidence(bundle, reg, m)
			must(t, e)
			retained := bytes.Contains(raw, []byte("Synthetic retained explanation."))
			if scenario == commandScenarioToolHeavy && retained || scenario == commandScenarioRetainedHeavy && !retained {
				t.Fatalf("%s artifact has unexpected retained-output shape for %s", scenario, reg.NativeSessionID)
			}
			safe := normalize.Replace(string(raw))
			sessionDigest := sha256.Sum256([]byte(safe))
			normalized = append(normalized, reg.NativeSessionID+":"+hex.EncodeToString(sessionDigest[:]))
		}
		batches, e := backfill.LoadBatches(home)
		must(t, e)
		if len(batches) != 1 || batches[0].CompletedAt == nil || len(batches[0].Sessions)+len(batches[0].Subagents) != len(expected) {
			t.Fatalf("incomplete batch: %+v", batches)
		}
		imported = len(regs)
	}
	sort.Strings(normalized)
	digest := sha256.Sum256([]byte(strings.Join(normalized, "\n")))
	result := map[string]any{"fixture_version": 7, "plan_sha256": hex.EncodeToString(planDigest[:]), "prime_attempts": primeAttempts, "prime_cache_supported": primeCacheSupported, "prime_cache_hits": primeHits, "state": stateName, "trace": os.Getenv("AA_BACKFILL_TRACE") == "1", "scenario": scenario, "mode": mode, "sessions": count, "selected": selected, "imported": imported, "skipped": skipped, "input_bytes": inputBytes, "elapsed_seconds": elapsed.Seconds(), "checkpoint_offsets": checkpoints, "command_cpu_seconds": cpuAfter - cpuBefore, "command_peak_rss_at_return_bytes": commandRSS, "allocated_bytes": after.TotalAlloc - before.TotalAlloc, "gc_cycles": after.NumGC - before.NumGC, "artifact_sha256": hex.EncodeToString(digest[:]), "toolchain": runtime.Version(), "logical_cpus": runtime.NumCPU(), "gomaxprocs": runtime.GOMAXPROCS(0), "planning_workers": 1, "default_adapter_workers": min(8, max(2, runtime.NumCPU()/2)), "byte_budget": 128 << 20}
	raw, e := json.Marshal(result)
	must(t, e)
	if os.Getenv("AA_BACKFILL_TRACE") == "1" {
		fmt.Print(stderr.String())
	}
	fmt.Printf("AA_BACKFILL_RESULT %s\n", raw)
}
