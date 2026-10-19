package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
)

// A start that arrives while setup's transaction is open cannot be registered.
// It must say so instead of disappearing.
func TestSetupInProgressRecordsDiagnosticAndSurfacesInStatus(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err := os.WriteFile(setupjournal.JournalPath(home), []byte(`{"changes":[],"plist":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	start := map[string]any{
		"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
		"cwd": project, "transcript_path": writeTestTranscript(t, "t.jsonl", ""),
	}
	if err := handleTestHookEvent(home, "claude", start, now); err != nil {
		t.Fatal(err)
	}
	store, _ := state.Open(home)
	if regs, _ := store.LoadRegistrations(); len(regs) != 0 {
		t.Fatalf("a hook registered during a setup transaction: %#v", regs)
	}
	ds, err := capture.ReadDiagnostics(home)
	if err != nil || len(ds) != 1 || ds[0].Code != capture.DiagnosticSetupInProgress || ds[0].ProjectRoot != project || ds[0].Harness != "claude" {
		t.Fatalf("diagnostics=%#v err=%v", ds, err)
	}
	raw, err := os.ReadFile(capture.DiagnosticsPath(home))
	if err != nil || bytes.Contains(raw, []byte("native-1")) || bytes.Contains(raw, []byte("transcript")) {
		t.Fatalf("diagnostic leaked session identity: %s err=%v", raw, err)
	}
	var out bytes.Buffer
	if code := runStatusCommand([]string{"--json"}, &out, os.Stderr, testEnv(t, home, now)); code != 0 {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
	if !strings.Contains(out.String(), string(capture.DiagnosticSetupInProgress)) {
		t.Fatalf("status --json omitted the diagnostic: %s", out.String())
	}
}

// status reports a chat that is still waiting for its transcript as a hook
// observed and capture pending, with no error.
func TestCursorWaitingChatShowsInStatus(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	setUpTestConfig(t, home, project, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Harnesses = []string{"cursor"}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	if err := handleTestHookEvent(home, "cursor", cursorDesktopPayload("beforeSubmitPrompt", "5f3c2a10-0000-4000-8000-00000000aaaa", project, nil), at); err != nil {
		t.Fatal(err)
	}
	env := testEnv(t, home, at.Add(time.Second))
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("sync: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := runStatusCommand([]string{"cursor"}, &stdout, &stderr, env); code != 0 {
		t.Fatalf("status: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if out := stdout.String(); strings.Contains(out, "Last error") || !strings.Contains(out, "hooks not checked   1 session\n") || !strings.Contains(out, "· 1 chat has no transcript yet (a chat with transcripts turned off in Cursor never gets one)\n") || strings.Contains(out, "uploading") {
		t.Fatalf("status = %s", out)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestExcludedProjectDiagnosticLeavesStatus(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Now().UTC()
	env := testEnv(t, home, at)
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	resumed := writeTestTranscript(t, "old.jsonl", "{\"role\":\"user\"}\n")
	if err := handleTestHookEvent(home, "cursor", map[string]any{"hook_event_name": "sessionStart", "conversation_id": "old", "workspace_roots": []any{"/work/widget"}, "transcript_path": resumed}, at); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if code := runStatusCommand([]string{"--issues"}, &out, os.Stderr, env); code != 0 || !strings.Contains(out.String(), "Cursor skipped a session in /work/widget") {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
	// The project is excluded afterwards: its path must stop appearing.
	setUpTestConfig(t, home, "/work/kept", at.Add(-time.Hour))
	out.Reset()
	if code := runStatusCommand([]string{"--issues"}, &out, os.Stderr, env); code != 0 || strings.Contains(out.String(), "/work/widget") {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
	out.Reset()
	if code := runStatusCommand([]string{"--json"}, &out, os.Stderr, env); code != 0 || strings.Contains(out.String(), "/work/widget") || strings.Contains(out.String(), "capture_diagnostics") {
		t.Fatalf("status --json exit=%d output=%s", code, out.String())
	}
}
