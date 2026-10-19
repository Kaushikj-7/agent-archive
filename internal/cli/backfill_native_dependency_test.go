package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestBackfillMissingNativeBaseKeepsDependencyDiagnostic(t *testing.T) {
	t.Parallel()
	canonical := func() string { p, err := filepath.EvalSymlinks(t.TempDir()); must(t, err); return p }
	home, userHome, project := canonical(), canonical(), canonical()
	native := filepath.Join(userHome, ".codex")
	must(t, os.MkdirAll(filepath.Join(native, "sessions"), 0700))
	const child = "11111111-1111-4111-8111-111111111111"
	const parent = "22222222-2222-4222-8222-222222222222"
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	header, err := json.Marshal(map[string]any{"type": "session_meta", "ordinal": 3, "payload": map[string]any{"id": child, "timestamp": at.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex_cli_rs", "cli_version": "dev", "parent_thread_id": parent, "subagent_history_start_ordinal": 3, "history_mode": "paginated", "history_base": map[string]any{"thread_id": parent, "end_ordinal_exclusive": 3, "end_byte_offset": 1}}})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(native, "sessions", "rollout-"+child+".jsonl"), append(header, '\n'), 0600))
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at.Add(time.Hour)}}}}
	must(t, config.Save(home, cfg))
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at.Add(2*time.Hour))
	before := snapshotTree(t, home)
	var out, errout bytes.Buffer
	if code := Run([]string{"backfill", "--dry-run", "--json", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errout, env); code != 0 {
		t.Fatal("missing dependency preview failed", code, errout.String())
	}
	var result struct {
		Diagnostics []backfill.DiagnosticSummary `json:"diagnostics"`
	}
	must(t, json.Unmarshal(out.Bytes(), &result))
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Skip != backfill.SkipRelatedHistory || result.Diagnostics[0].Detail != backfill.DetailHistoryBaseUnavailable || result.Diagnostics[0].Residual != "" || snapshotTree(t, home) != before {
		t.Fatal("missing dependency became a no-record or missing-task diagnostic", out.String())
	}
	out.Reset()
	errout.Reset()
	if code := Run([]string{"backfill", "--yes", "--background", "--harness", "codex", "--include-temp", "--project", project}, nil, &out, &errout, env); code != 0 {
		t.Fatal("missing dependency import failed", code, errout.String())
	}
	store, err := state.Open(home)
	must(t, err)
	registrations, err := store.LoadRegistrations()
	must(t, err)
	stages, next, err := store.ImportStages(t.Context(), "", 50)
	must(t, err)
	if len(registrations) != 0 || len(stages) != 0 || next != "" || mustLoadConfig(t, home).CodexHistoryProtection {
		t.Fatal("missing dependency acquired durable authority")
	}
}
