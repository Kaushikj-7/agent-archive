package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestHistoricalOnlyCollectorDoesNotStartNativeInventory(t *testing.T) {
	t.Parallel()
	fixture, _, mapping := newHistoricalOnlyFixture(t)
	cfg := mustLoadConfig(t, fixture.data)
	cfg.Harnesses = nil
	must(t, config.Save(fixture.data, cfg))
	_, stderr, code := fixture.importRun(t, nil, false, "--yes", "--background", "--mapping-file", mapping)
	if code != 0 {
		t.Fatalf("historical import %d %s", code, stderr)
	}
	native := filepath.Join(fixture.userHome, ".codex", "sessions")
	must(t, os.MkdirAll(native, 0700))
	const id = "11111111-1111-4111-8111-111111111111"
	header, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "cwd": filepath.Join(fixture.userHome, "vanished-service"), "timestamp": fixture.env.now().Add(-time.Hour).Format(time.RFC3339Nano), "cli_version": "0.160.0", "originator": "codex_cli_rs", "source": "cli"}})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(native, "rollout-"+id+".jsonl"), append(header, '\n'), 0600))
	catalog := filepath.Join(fixture.data, "discovery-catalog.json")
	before, beforeErr := os.ReadFile(catalog)
	if beforeErr != nil && !os.IsNotExist(beforeErr) {
		t.Fatal(beforeErr)
	}
	for range 2 {
		result, err := runOnePass(fixture.env, true)
		must(t, err)
		if len(result.Errors) > 0 {
			t.Fatal(result.Errors)
		}
	}
	after, afterErr := os.ReadFile(catalog)
	if os.IsNotExist(beforeErr) != os.IsNotExist(afterErr) || afterErr != nil && !os.IsNotExist(afterErr) || !bytes.Equal(before, after) {
		t.Fatal("historical-only collector started or advanced native discovery inventory")
	}
}

type historicalImportCheckpoint string

const (
	historicalBatchSaved historicalImportCheckpoint = "batch saved"
	historicalCommitted  historicalImportCheckpoint = "committed"
	historicalStaged     historicalImportCheckpoint = "staged"
	historicalRegistered historicalImportCheckpoint = "registered"
	historicalUploading  historicalImportCheckpoint = "uploading"
)

func newHistoricalOnlyFixture(t *testing.T) (*backfillFixture, *storagetest.MemoryStore, string) {
	t.Helper()
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	cfg := config.Config{MachineID: "historical-machine", Storage: credentialsTestConfig(), RetentionDays: 90, Harnesses: []string{"claude"}, Archive: archive.Config{Enabled: true}}
	must(t, config.Save(home, cfg))
	cwd := filepath.Join(userHome, "vanished-service")
	source := filepath.Join(userHome, ".claude", "projects", "synthetic", "historical-session.jsonl")
	must(t, os.MkdirAll(filepath.Dir(source), 0700))
	must(t, os.WriteFile(source, []byte(fmt.Sprintf(`{"type":"user","sessionId":"historical-session","cwd":%q,"timestamp":"2026-10-11T12:00:00Z","message":{"role":"user","content":"Review synthetic retired service"}}`+"\n", cwd)), 0600))
	bucket := storagetest.NewMemoryStore()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return bucket, nil }
	env.repoKey = func(string) string { t.Fatal("historical import invoked Git"); return "" }
	return &backfillFixture{env: env, data: home, userHome: userHome}, bucket, writeHistoricalMapping(t, cwd)
}

func TestHistoricalImportInterruptionKeepsFrozenOwnership(t *testing.T) {
	t.Parallel()
	for _, checkpoint := range []historicalImportCheckpoint{historicalBatchSaved, historicalCommitted, historicalStaged, historicalRegistered, historicalUploading} {
		t.Run(string(checkpoint), func(t *testing.T) {
			t.Parallel()
			fixture, _, mapping := newHistoricalOnlyFixture(t)
			observed := false
			fixture.env.backfillCheckpoint = func(step string) error {
				if historicalImportCheckpoint(step) == checkpoint {
					observed = true
					return errors.New("synthetic interruption")
				}
				return nil
			}
			args := []string{"--yes", "--mapping-file", mapping}
			if checkpoint != historicalUploading {
				args = append(args, "--background")
			}
			_, stderr, code := fixture.importRun(t, nil, false, args...)
			if !observed {
				t.Fatalf("checkpoint %s was not reached", checkpoint)
			}
			if code != 1 || !strings.Contains(stderr, "synthetic interruption") {
				t.Fatalf("interruption %d %s", code, stderr)
			}
			fixture.env.backfillCheckpoint = nil
			if checkpoint == historicalStaged || checkpoint == historicalRegistered || checkpoint == historicalUploading {
				must(t, os.RemoveAll(filepath.Join(fixture.userHome, ".claude")))
				result, err := runOnePass(fixture.env, true)
				must(t, err)
				if len(result.Errors) > 0 {
					t.Fatal(result.Errors)
				}
			} else {
				_, stderr, code = fixture.importRun(t, nil, false, "--yes", "--mapping-file", mapping)
				if code != 0 {
					t.Fatalf("resume %d %s", code, stderr)
				}
			}
			local, err := state.Open(fixture.data)
			must(t, err)
			regs, err := local.LoadRegistrations()
			must(t, err)
			if len(regs) != 1 || regs[0].HistoricalProject == nil || regs[0].ProjectRoot != "" {
				t.Fatal("resumed import lost historical proof", regs)
			}
		})
	}
}

func TestHistoricalBulkAssignmentsImportBeyondInlineLimit(t *testing.T) {
	t.Parallel()
	fixture, _, mapping := newHistoricalOnlyFixture(t)
	must(t, os.RemoveAll(filepath.Join(fixture.userHome, ".claude")))
	var rows strings.Builder
	rows.WriteString(`{"schema_version":1,"historical_projects":[{"id":"11111111111111111111111111111111","label":"Retired service"}],"mappings":[`)
	for i := range 160 {
		cwd := filepath.Join(fixture.userHome, fmt.Sprintf("vanished-%03d", i))
		source := filepath.Join(fixture.userHome, ".claude", "projects", "synthetic", fmt.Sprintf("bulk-%03d.jsonl", i))
		must(t, os.MkdirAll(filepath.Dir(source), 0700))
		must(t, os.WriteFile(source, []byte(fmt.Sprintf(`{"type":"user","sessionId":"bulk-%03d","cwd":%q,"timestamp":"2026-10-11T12:00:00Z","message":{"role":"user","content":"Synthetic bulk historical prompt"}}`+"\n", i, cwd)), 0600))
		if i > 0 {
			rows.WriteByte(',')
		}
		_, err := fmt.Fprintf(&rows, `{"cwd":%q,"historical_project_id":"11111111111111111111111111111111"}`, cwd)
		must(t, err)
	}
	rows.WriteString("]}")
	must(t, os.WriteFile(mapping, []byte(rows.String()), 0600))
	_, stderr, code := fixture.importRun(t, nil, false, "--yes", "--background", "--mapping-file", mapping)
	if code != 0 {
		t.Fatalf("bulk import %d %s", code, stderr)
	}
	local, err := state.Open(fixture.data)
	must(t, err)
	regs, err := local.LoadRegistrations()
	must(t, err)
	if len(regs) != 160 {
		t.Fatalf("bulk import stopped at inline limit: %d", len(regs))
	}
	cfg := mustLoadConfig(t, fixture.data)
	if len(cfg.HistoricalProjects) != 1 || len(cfg.Archive.Projects) != 0 {
		t.Fatal("bulk assignments widened capture", cfg.Archive.Projects)
	}
	for _, reg := range regs {
		if reg.ProjectID != "11111111111111111111111111111111" || reg.HistoricalProject == nil || reg.StagedImportSHA256 == "" {
			t.Fatal("bulk registration lost frozen identity")
		}
	}
}

func TestHistoricalStagedSubagentWithoutCheckoutOrHooks(t *testing.T) {
	t.Parallel()
	fixture, _, mapping := newHistoricalOnlyFixture(t)
	cfg := mustLoadConfig(t, fixture.data)
	cfg.Harnesses = nil
	must(t, config.Save(fixture.data, cfg))
	child := filepath.Join(fixture.userHome, ".claude", "projects", "synthetic", "historical-session", "subagents", "agent-helper.jsonl")
	must(t, os.MkdirAll(filepath.Dir(child), 0700))
	must(t, os.WriteFile(child, []byte(`{"type":"assistant","sessionId":"historical-session","agentId":"helper","timestamp":"2026-10-11T12:05:00Z","message":{"role":"assistant","content":"Synthetic historical child"}}`+"\n"), 0600))
	_, stderr, code := fixture.importRun(t, nil, false, "--yes", "--background", "--mapping-file", mapping)
	if code != 0 {
		t.Fatalf("staged child import %d %s", code, stderr)
	}
	must(t, os.RemoveAll(filepath.Join(fixture.userHome, ".claude")))
	for range 3 {
		result, err := runOnePass(fixture.env, true)
		must(t, err)
		if len(result.Errors) > 0 {
			t.Fatal(result.Errors)
		}
	}
	local, err := state.Open(fixture.data)
	must(t, err)
	regs, err := local.LoadRegistrations()
	must(t, err)
	if len(regs) != 2 {
		t.Fatal("staged child lost after native deletion", regs)
	}
	cfg = mustLoadConfig(t, fixture.data)
	if len(cfg.Harnesses) != 0 || len(cfg.ImportedHarnesses) != 1 || len(cfg.Archive.Projects) != 0 {
		t.Fatal("first import installed future capture authority")
	}
	for _, reg := range regs {
		if reg.HistoricalProject == nil || !cfg.AcceptSession(reg) {
			t.Fatal("historical child lost parent import policy")
		}
	}
	_, stderr, code = fixture.command(t, "backfill", "undo", "--yes")
	if code != 0 {
		t.Fatalf("child undo %d %s", code, stderr)
	}
	regs, err = local.LoadRegistrations()
	must(t, err)
	if len(regs) != 0 {
		t.Fatal("historical child undo left ownership")
	}
}

func TestHistoricalCursorTranscriptUsesExplicitGoneWorkspace(t *testing.T) {
	t.Parallel()
	fixture, bucket, mapping := newHistoricalOnlyFixture(t)
	must(t, os.RemoveAll(filepath.Join(fixture.userHome, ".claude")))
	cwd := filepath.Join(fixture.userHome, "vanished-service")
	source := filepath.Join(fixture.userHome, ".cursor", "projects", cursorSlugFor(cwd), "agent-transcripts", "cursor-history", "cursor-history.jsonl")
	must(t, os.MkdirAll(filepath.Dir(source), 0700))
	must(t, os.WriteFile(source, []byte(`{"role":"user","message":{"content":[{"type":"text","text":"Synthetic retired Cursor workspace"}]}}`+"\n"), 0600))
	at := fixture.env.now().Add(-24 * time.Hour)
	must(t, os.Chtimes(source, at, at))
	cfg := mustLoadConfig(t, fixture.data)
	cfg.Harnesses = []string{"cursor"}
	must(t, config.Save(fixture.data, cfg))
	_, stderr, code := fixture.importRun(t, nil, false, "--yes", "--background", "--mapping-file", mapping)
	if code != 0 {
		t.Fatalf("Cursor historical import %d %s", code, stderr)
	}
	local, err := state.Open(fixture.data)
	must(t, err)
	regs, err := local.LoadRegistrations()
	must(t, err)
	if len(regs) != 1 || regs[0].Harness.Name != "cursor" || regs[0].HistoricalProject == nil {
		t.Fatal("explicit missing workspace not imported", regs)
	}
	must(t, os.RemoveAll(filepath.Join(fixture.userHome, ".cursor")))
	result, err := runOnePass(fixture.env, true)
	must(t, err)
	if len(result.Errors) > 0 || len(result.Published) != 1 {
		t.Fatal("Cursor retained staging failed", result)
	}
	_, bundle := publishedCursorSession(t, bucket, regs[0])
	if !strings.Contains(bundleJSON(t, bundle), "Synthetic retired Cursor workspace") {
		t.Fatal("historical Cursor content missing")
	}
}

func TestHistoricalNativeChildPublishesOwnEvidenceWithoutLiveRepository(t *testing.T) {
	t.Parallel()
	for _, aliased := range []bool{false, true} {
		t.Run(fmt.Sprintf("aliased-home-%t", aliased), func(t *testing.T) {
			t.Parallel()
			historicalNativeChildPublishesOwnEvidence(t, aliased)
		})
	}
}

func historicalNativeChildPublishesOwnEvidence(t *testing.T, aliased bool) {
	t.Helper()
	fixture, bucket, mapping := newHistoricalOnlyFixture(t)
	if aliased {
		alias := filepath.Join(t.TempDir(), "home")
		must(t, os.Symlink(fixture.userHome, alias))
		fixture.userHome = alias
		fixture.env.UserHomeDir = func() (string, error) { return alias, nil }
		mapping = writeHistoricalMapping(t, filepath.Join(alias, "vanished-service"))
	}
	must(t, os.RemoveAll(filepath.Join(fixture.userHome, ".claude")))
	at := fixture.env.now().Add(-time.Hour)
	const child = "11111111-1111-4111-8111-111111111111"
	const parent = "22222222-2222-4222-8222-222222222222"
	cwd := filepath.Join(fixture.userHome, "vanished-service")
	header, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": child, "session_id": parent, "parent_thread_id": parent, "cwd": cwd, "timestamp": at.Format(time.RFC3339Nano), "cli_version": "dev", "originator": "codex_cli_rs", "source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}, "subagent_history_start_ordinal": 1}})
	must(t, err)
	body := fmt.Sprintf(`{"type":"event_msg","payload":{"type":"task_started","turn_id":%q,"started_at":%q}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Synthetic historical native child own prompt"}]}}`+"\n", child, at.Format(time.RFC3339Nano))
	native := filepath.Join(fixture.userHome, ".codex")
	must(t, os.MkdirAll(filepath.Join(native, "sessions"), 0700))
	must(t, os.WriteFile(filepath.Join(native, "sessions", "rollout-"+child+".jsonl"), append(append(header, '\n'), []byte(body)...), 0600))
	stdout, stderr, code := fixture.importRun(t, nil, false, "--yes", "--background", "--harness", "codex", "--mapping-file", mapping)
	if code != 0 {
		t.Fatalf("native child import %d %s", code, stderr)
	}
	local, err := state.Open(fixture.data)
	must(t, err)
	regs, err := local.LoadRegistrations()
	must(t, err)
	if len(regs) != 1 || !regs[0].NativeChild || regs[0].HistoricalProject == nil || regs[0].ProjectRoot != "" {
		t.Fatal("native child historical admission lost", regs, stdout, stderr)
	}
	canonicalHome, err := filepath.EvalSymlinks(native)
	must(t, err)
	if regs[0].NativeSourceHome != canonicalHome || regs[0].HistoricalProject.OriginalCwd != cwd {
		t.Fatal("native home normalization changed original historical ownership", regs[0])
	}
	must(t, os.RemoveAll(native))
	for range 4 {
		result, err := runOnePass(fixture.env, true)
		must(t, err)
		if len(result.Errors) > 0 {
			t.Fatal(result.Errors)
		}
	}
	_, bundle := publishedCursorSession(t, bucket, regs[0])
	if !strings.Contains(bundleJSON(t, bundle), "Synthetic historical native child own prompt") {
		t.Fatal("native child own evidence lost")
	}
	_, stderr, code = fixture.command(t, "backfill", "undo", "--yes")
	if code != 0 {
		t.Fatalf("native child undo %d %s", code, stderr)
	}
}
