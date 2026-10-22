package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestStatusReportsDiscoveredTaskBeforeLocalCapture(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cfg := pairTestConfig(now, []string{"codex"}, project)
	config.SetDiscoveryChoice(&cfg, true)
	must(t, config.Save(home, cfg))
	store, err := state.Open(home)
	must(t, err)
	reg := archive.SessionRegistration{
		ArchiveSessionID: "discovered-pending", NativeSessionID: "synthetic-pending",
		ProjectID: archive.ProjectID(project), ProjectRoot: project,
		Harness: archive.Harness{Name: "codex"}, Origin: archive.SessionOriginDiscovery,
		SessionStartedAt: now, RegisteredAt: now, AdmittedAt: now,
		TranscriptPath: filepath.Join(project, "pending.jsonl"),
	}
	must(t, store.SaveRegistration(reg))
	must(t, local.Write(filepath.Join(home, "discovery-health.json"), struct {
		Version int              `json:"version"`
		Health  discovery.Health `json:"health"`
	}{Version: 1, Health: discovery.Health{Enabled: true, Supported: true, LastAttempt: now, Registered: 1, Outcomes: map[string]int{"native_format": 1}}}))
	env := pairStatusEnv(t, home, userHome, now, "codex")
	view, err := readStatus(env)
	must(t, err)
	if len(view.Apps) != 1 {
		t.Fatalf("expected one configured app: %+v", view.Apps)
	}
	app := view.Apps[0]
	if app.State != "task found; waiting for capture" || app.Code != "awaiting_capture" {
		t.Errorf("discovered task lost pending capture state/code: %+v", app)
	}
	if app.Sessions != 1 || app.HookObserved || app.CapturedLocally || app.Published || app.ReadBackVerified || app.VerificationState != "not_verified" {
		t.Errorf("registration invented capture, hook or publication evidence: %+v", app)
	}
	var jsonOut bytes.Buffer
	if code := runStatusCommand([]string{"--json"}, &jsonOut, &jsonOut, env); code != 0 {
		t.Fatalf("JSON status exit %d: %s", code, &jsonOut)
	}
	var document statusView
	must(t, json.Unmarshal(jsonOut.Bytes(), &document))
	if len(document.Apps) != 1 || document.Apps[0].Code != "awaiting_capture" || document.Apps[0].HookObserved || document.Apps[0].VerificationState != "not_verified" {
		t.Errorf("JSON lost registration-only evidence: %s", &jsonOut)
	}
	for _, verbose := range []bool{false, true} {
		t.Run(map[bool]string{false: "app", true: "verbose"}[verbose], func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			args := []string{"codex"}
			if verbose {
				args = []string{"--verbose"}
			}
			if code := runStatusCommand(args, &out, &out, env); code != 0 {
				t.Fatalf("status exit %d: %s", code, &out)
			}
			text := out.String()
			if !strings.Contains(text, "1 session") || strings.Contains(strings.SplitN(text, "\nProjects\n", 2)[0], "waiting for first session") {
				t.Errorf("rendered status contradicts discovered registration: %s", text)
			}
			if verbose && (!strings.Contains(text, "session seen, not captured yet") || !strings.Contains(text, "1 registered")) {
				t.Errorf("verbose status lost pending capture progress: %s", text)
			}
		})
	}
}
