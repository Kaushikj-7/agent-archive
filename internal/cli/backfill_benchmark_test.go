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
	if scenario == "large" {
		count = 32
	}
	if scenario == "related" {
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
	dir := filepath.Join(userHome, ".codex", "sessions")
	must(t, os.MkdirAll(dir, 0700))
	expected := map[string]bool{}
	var inputBytes int64
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		at := backfillNow.Add(-48 * time.Hour)
		old := scenario == "old" && i%10 != 0 || scenario == "recent" && i%10 == 0 || scenario == "related" && i == 0
		if old {
			at = backfillNow.Add(-30 * 24 * time.Hour)
		}
		meta := map[string]any{"id": id, "timestamp": at.UTC().Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "dev"}
		if scenario == "related" && i > 0 {
			meta["forked_from_id"] = "00000000-0000-4000-8000-000000000001"
			meta["forked_from_ordinal_exclusive"] = 3
		}
		h, e := json.Marshal(map[string]any{"type": "session_meta", "payload": meta})
		must(t, e)
		path := filepath.Join(dir, "rollout-"+id+".jsonl")
		f, e := os.Create(path)
		must(t, e)
		n, e := f.Write(append(h, '\n'))
		must(t, e)
		inputBytes += int64(n)
		row := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic prompt"}]}}` + "\n"
		rows := 1 + (i%8)*8
		if scenario == "related" {
			rows = 1
			if i > 0 {
				inherited := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"00000000-0000-4000-8000-000000000001","started_at":%q}}`+"\n"+row, backfillNow.Add(-30*24*time.Hour).UTC().Format(time.RFC3339Nano))
				n, e = f.WriteString(inherited)
				must(t, e)
				inputBytes += int64(n)
			}
			row = fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+row, id, at.UTC().Format(time.RFC3339Nano))
		}
		for j := 0; j < rows; j++ {
			n, e = f.WriteString(row)
			must(t, e)
			inputBytes += int64(n)
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
	var stdout, stderr bytes.Buffer
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
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
			raw, e := json.Marshal(bundle.NativeRecords)
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
	result := map[string]any{"fixture_version": 1, "scenario": scenario, "mode": mode, "sessions": count, "selected": len(expected), "imported": imported, "skipped": count - len(expected), "input_bytes": inputBytes, "elapsed_seconds": elapsed.Seconds(), "allocated_bytes": after.TotalAlloc - before.TotalAlloc, "gc_cycles": after.NumGC - before.NumGC, "artifact_sha256": hex.EncodeToString(digest[:]), "toolchain": runtime.Version(), "logical_cpus": runtime.NumCPU(), "workers": min(8, max(2, runtime.NumCPU()/2))}
	raw, e := json.Marshal(result)
	must(t, e)
	fmt.Printf("AA_BACKFILL_RESULT %s\n", raw)
}
