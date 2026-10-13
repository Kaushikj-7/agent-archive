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

// TestBackfillCommandMeasurement is a subprocess entry point, not a timed Go
// benchmark loop. Each sample gets a fresh synthetic home and deterministic
// provider. Fixture construction and artifact verification are outside Run's timer.
func TestBackfillCommandMeasurement(t *testing.T) {
	scenario := os.Getenv("AA_BACKFILL_SCENARIO")
	if scenario == "" {
		t.Skip("explicit benchmark subprocess only")
	}
	count := 1500
	if os.Getenv("AA_BACKFILL_SMALL") == "1" {
		count = 12
	}
	if scenario == "tool-heavy" || scenario == "retained-heavy" {
		count = 32
	}
	if scenario == "large" {
		count = 32
	}
	if scenario == "related" || scenario == "ancestry" {
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
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		at := backfillNow.Add(-48 * time.Hour)
		old := scenario == "old" && i%10 != 0 || scenario == "recent" && i%10 == 0 || (scenario == "related" || scenario == "ancestry") && i == 0
		if old {
			at = backfillNow.Add(-30 * 24 * time.Hour)
		}
		meta := map[string]any{"id": id, "timestamp": at.UTC().Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "dev"}
		if scenario == "related" && i > 0 {
			meta["forked_from_id"] = "00000000-0000-4000-8000-000000000001"
			meta["forked_from_ordinal_exclusive"] = 3
		}
		ordinal := 0
		if scenario == "ancestry" && i > 0 {
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
		if scenario == "related" || scenario == "ancestry" {
			rows = 1
			if i > 0 && scenario == "related" {
				inherited := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"00000000-0000-4000-8000-000000000001","started_at":%q}}`+"\n"+row, backfillNow.Add(-30*24*time.Hour).UTC().Format(time.RFC3339Nano))
				n, e = f.WriteString(inherited)
				must(t, e)
				inputBytes += int64(n)
			}
			row = fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+row, id, at.UTC().Format(time.RFC3339Nano))
		}
		if scenario == "ancestry" {
			row = fmt.Sprintf(`{"type":"event_msg","ordinal":%d,"payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+`{"type":"response_item","ordinal":%d,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic own task"}]}}`+"\n", ordinal+1, id, at.UTC().Format(time.RFC3339Nano), ordinal+2)
		}
		for j := 0; j < rows; j++ {
			n, e = f.WriteString(row)
			must(t, e)
			inputBytes += int64(n)
		}
		if scenario == "recent" || scenario == "old" || scenario == "tool-heavy" || scenario == "retained-heavy" {
			sizes := []int64{256 << 10, 256 << 10, 512 << 10, 1 << 20, 1 << 20, 1 << 20, 1 << 20, 2 << 20, 4 << 20, 8 << 20}
			target := sizes[i%len(sizes)]
			if scenario == "tool-heavy" || scenario == "retained-heavy" {
				target = 4 << 20
			}
			if os.Getenv("AA_BACKFILL_SMALL") == "1" {
				target /= 256
			}
			bulk := `{"type":"response_item","payload":{"type":"function_call_output","call_id":"synthetic","output":"` + strings.Repeat("synthetic tool stdout ", 800) + `"}}` + "\n"
			if i%3 == 0 || scenario == "retained-heavy" {
				bulk = `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("Synthetic retained explanation. ", 500) + `"}]}}` + "\n"
			}
			for wrote := int64(0); wrote < target; {
				n, e = f.WriteString(bulk)
				must(t, e)
				wrote += int64(n)
				inputBytes += int64(n)
			}
		}
		if scenario == "large" && i == 0 {
			tool := `{"type":"response_item","payload":{"type":"function_call_output","call_id":"synthetic","output":"` + strings.Repeat("x", 8192) + `"}}` + "\n"
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
	stateName := os.Getenv("AA_BACKFILL_STATE")
	if stateName == "warm" || stateName == "changed" {
		var primeOut, primeErr bytes.Buffer
		if code := Run([]string{"backfill", "--dry-run", "--since", "7d", "--harness", "codex", "--include-temp"}, nil, &primeOut, &primeErr, env); code != 0 {
			t.Fatalf("prime %d: %s", code, primeErr.String())
		}
		if stateName == "changed" {
			f, e := os.OpenFile(filepath.Join(dir, "rollout-00000000-0000-4000-8000-000000000002.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
			must(t, e)
			n, e := f.WriteString(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Synthetic changed-file evidence"}]}}` + "\n")
			must(t, e)
			inputBytes += int64(n)
			must(t, f.Close())
		}
	}
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
	runtime.ReadMemStats(&after)
	if code != 0 {
		t.Fatalf("command failed %d: %s\n%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), fmt.Sprintf("Total: %d sessions", len(expected))) {
		t.Fatalf("unexpected plan counts: %s", stdout.String())
	}
	imported := 0
	var normalized []string
	if mode == "import" {
		regs, e := state.OpenReadOnly(home).LoadRegistrations()
		must(t, e)
		if len(regs) != len(expected) {
			t.Fatalf("registrations %d want %d: %s %s", len(regs), len(expected), stdout.String(), stderr.String())
		}
		for _, reg := range regs {
			if !expected[reg.NativeSessionID] {
				t.Fatalf("unexpected imported identity %s", reg.NativeSessionID)
			}
			key, e := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
			must(t, e)
			m, e := reader.ReadMetadata(t.Context(), bucket, key)
			must(t, e)
			bundle, e := reader.LoadSource(t.Context(), bucket, m, reader.Limits{})
			must(t, e)
			raw, e := json.Marshal(map[string]any{"records": bundle.NativeRecords, "history": bundle.History, "text": bundle.NativeText, "started_at": reg.SessionStartedAt, "started_at_source": reg.StartedAtSource, "origin": reg.Origin, "format": bundle.Capture.SourceFormat})
			must(t, e)
			normalized = append(normalized, reg.NativeSessionID+":"+strings.ReplaceAll(string(raw), project, "$PROJECT"))
		}
		batches, e := backfill.LoadBatches(home)
		must(t, e)
		if len(batches) != 1 || batches[0].CompletedAt == nil || len(batches[0].Sessions) != len(expected) {
			t.Fatalf("incomplete batch: %+v", batches)
		}
		imported = len(regs)
	}
	sort.Strings(normalized)
	digest := sha256.Sum256([]byte(strings.Join(normalized, "\n")))
	result := map[string]any{"fixture_version": 3, "state": stateName, "trace": os.Getenv("AA_BACKFILL_TRACE") == "1", "scenario": scenario, "mode": mode, "sessions": count, "selected": len(expected), "imported": imported, "skipped": count - len(expected), "input_bytes": inputBytes, "elapsed_seconds": elapsed.Seconds(), "checkpoint_offsets": checkpoints, "allocated_bytes": after.TotalAlloc - before.TotalAlloc, "gc_cycles": after.NumGC - before.NumGC, "artifact_sha256": hex.EncodeToString(digest[:]), "toolchain": runtime.Version(), "logical_cpus": runtime.NumCPU(), "planning_workers": 1, "default_adapter_workers": min(8, max(2, runtime.NumCPU()/2)), "byte_budget": 128 << 20}
	raw, e := json.Marshal(result)
	must(t, e)
	if os.Getenv("AA_BACKFILL_TRACE") == "1" {
		fmt.Print(stderr.String())
	}
	fmt.Printf("AA_BACKFILL_RESULT %s\n", raw)
}
