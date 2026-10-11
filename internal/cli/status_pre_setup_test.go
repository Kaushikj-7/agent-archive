package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// writeCodexRollout writes a minimal supported Codex rollout that started
// at created, indexed in the state database.
func writeCodexRollout(t *testing.T, db *sql.DB, source, project, id string, created time.Time) {
	t.Helper()
	path := filepath.Join(source, "sessions", "rollout-"+created.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	meta, err := json.Marshal(map[string]any{"type": "session_meta", "ordinal": 0, "timestamp": created.Format(time.RFC3339Nano), "payload": map[string]any{"id": id, "timestamp": created.Format(time.RFC3339Nano), "cwd": project, "source": "cli", "originator": "codex-tui", "cli_version": "0.159.3", "history_mode": "paginated"}})
	must(t, err)
	task, err := json.Marshal(map[string]any{"type": "event_msg", "ordinal": 1, "timestamp": created.Format(time.RFC3339Nano), "payload": map[string]any{"type": "task_started", "turn_id": id, "root_turn_id": id, "started_at": created.Format(time.RFC3339Nano)}})
	must(t, err)
	prompt := `{"type":"response_item","ordinal":2,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"a prompt before setup"}]}}`
	must(t, os.WriteFile(path, []byte(string(meta)+"\n"+string(task)+"\n"+prompt+"\n"), 0o600))
	must(t, os.Chtimes(path, created.Add(time.Minute), created.Add(time.Minute)))
	_, err = db.ExecContext(t.Context(), "INSERT INTO threads VALUES(?,?)", id, path)
	must(t, err)
}

// status points at backfill for the Codex sessions discovery refused for
// starting in the 7 days before it was enabled, and stops once backfill
// has imported them. A session from before those 7 days is not counted.
func TestStatusNamesPreSetupSessionsUntilImported(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	physicalTemp := func() string {
		path, err := filepath.EvalSymlinks(t.TempDir())
		must(t, err)
		return path
	}
	home, userHome, project := physicalTemp(), physicalTemp(), physicalTemp()
	must(t, os.Chmod(home, 0o700))
	source := filepath.Join(userHome, ".codex")
	must(t, os.MkdirAll(source, 0o700))
	cfg := config.Config{MachineID: "synthetic-machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true, ActivatedAt: at}}}, Discovery: &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{source}}}
	must(t, config.ReconcileDiscovery(&cfg, config.Config{}, at))
	must(t, config.Save(home, cfg))
	db, err := sql.Open("sqlite", filepath.Join(source, "state_5.sqlite"))
	must(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.ExecContext(t.Context(), "PRAGMA journal_mode=WAL; CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT)")
	must(t, err)
	// Two sessions inside the 7 days before discovery was enabled, one
	// before them.
	writeCodexRollout(t, db, source, project, "00000000-0000-0000-0000-000000000001", at.Add(-time.Hour))
	writeCodexRollout(t, db, source, project, "00000000-0000-0000-0000-000000000002", at.Add(-6*24*time.Hour))
	writeCodexRollout(t, db, source, project, "00000000-0000-0000-0000-000000000003", at.Add(-9*24*time.Hour))

	remote := storagetest.NewMemoryStore()
	now := at.Add(2 * time.Minute)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	env.Now = func() time.Time { return now }
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	pass := func(stage string) {
		t.Helper()
		for range 2 {
			if result, err := runOnePass(env, true); err != nil || len(result.Errors) > 0 {
				t.Fatalf("%s: %+v %v", stage, result, err)
			}
		}
	}
	status := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if code := Run(append([]string{"status"}, args...), strings.NewReader(""), &out, &out, env); code != 0 {
			t.Fatalf("status %v exit %d\n%s", args, code, &out)
		}
		return out.String()
	}
	const line = "2 Codex sessions from the 7 days before setup were not captured."
	const command = "Import them with agent-archive backfill --since 2026-09-25."

	if text := status(); strings.Contains(text, "before setup") {
		t.Fatalf("status named pre-setup sessions before any scan:\n%s", text)
	}
	pass("before import")
	if regs, err := state.OpenReadOnly(home).LoadRegistrations(); err != nil || len(regs) != 0 {
		t.Fatalf("discovery admitted a session that started before it was enabled: %+v %v", regs, err)
	}
	for _, args := range [][]string{nil, {"codex"}} {
		text := status(args...)
		if !strings.Contains(text, "! "+line+"\n      "+command+"\n") {
			t.Fatalf("status %v:\n%s", args, text)
		}
		for _, private := range []string{"00000000-0000", source} {
			if strings.Contains(text, private) {
				t.Fatalf("status %v exposed %q:\n%s", args, private, text)
			}
		}
	}
	var view struct {
		Apps []struct {
			Name             string          `json:"name"`
			PreSetupSessions json.RawMessage `json:"pre_setup_sessions"`
		} `json:"applications"`
	}
	text := status("--json")
	must(t, json.Unmarshal([]byte(text), &view))
	if len(view.Apps) != 1 {
		t.Fatalf("applications: %s", text)
	}
	want := `{"sessions":2,"since":"2026-09-25","window_start":"2026-09-25T00:00:00Z","start_floor":"2026-10-02T12:00:00Z","command":"agent-archive backfill --since 2026-09-25"}`
	var compact bytes.Buffer
	must(t, json.Compact(&compact, view.Apps[0].PreSetupSessions))
	if got := compact.String(); got != want {
		t.Fatalf("pre_setup_sessions = %s, want %s", got, want)
	}
	if strings.Contains(text, "00000000-0000") || strings.Contains(text, source) {
		t.Fatalf("status --json exposed a session identity or source:\n%s", text)
	}

	var out bytes.Buffer
	if code := Run([]string{"backfill", "--yes", "--background", "--since", "2026-09-25"}, strings.NewReader(""), &out, &out, env); code != 0 {
		t.Fatalf("backfill exit %d\n%s", code, &out)
	}
	// Imported sessions are archived: status drops them at once, before a
	// scan revisits them.
	if text := status(); strings.Contains(text, "before setup") {
		t.Fatalf("status still names imported sessions:\n%s", text)
	}
	pass("after import")
	if text := status("--json"); strings.Contains(text, "pre_setup_sessions") {
		t.Fatalf("status --json still names imported sessions:\n%s", text)
	}
	if refused, err := discovery.ReadRefusedStarts(home); err != nil || len(refused) != 0 {
		t.Fatalf("discovery kept imported sessions in its record: %+v %v", refused, err)
	}
}

func TestPreSetupSessionsCountsOnlyRefusedTopLevelStartsInTheWindow(t *testing.T) {
	t.Parallel()
	floor := time.Date(2026, 10, 5, 21, 43, 0, 0, time.UTC)
	pacific := time.FixedZone("PDT", -7*60*60)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, pacific)
	cfg := config.Config{Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true}, Discovery: &config.DiscoveryConfig{Enabled: true, Authorizations: []config.DiscoveryAuthorization{
		{Agent: "codex", ProjectRoot: "/later", NativeStartFloor: floor.Add(48 * time.Hour), DestinationID: config.Config{}.DestinationID()},
		{Agent: "codex", ProjectRoot: "/first", NativeStartFloor: floor, DestinationID: config.Config{}.DestinationID()},
	}}}
	store, err := state.Open(t.TempDir())
	must(t, err)
	// The window is the local day 7 days before the floor (2026-09-28 in
	// Pacific time, 07:00 UTC) up to the floor.
	windowStart := time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC)
	refused := []discovery.RefusedStart{
		{Agent: "codex", NativeSessionID: "first-minute", StartedAt: windowStart},
		{Agent: "codex", NativeSessionID: "before-floor", StartedAt: floor.Add(-time.Second)},
		{Agent: "codex", NativeSessionID: "before-window", StartedAt: windowStart.Add(-time.Second)},
		{Agent: "codex", NativeSessionID: "at-floor", StartedAt: floor},
		{Agent: "codex", NativeSessionID: "archived", StartedAt: floor.Add(-time.Hour)},
		{Agent: "codex", NativeSessionID: "removed", StartedAt: floor.Add(-time.Hour)},
		{Agent: "claude", NativeSessionID: "other-app", StartedAt: floor.Add(-time.Hour)},
	}
	must(t, store.RecordRemoval("codex", "removed", state.RemovalReasonUndo, now))
	archived := map[string]bool{"codex\x00archived": true}
	got := preSetupSessionsFor("codex", cfg, refused, archived, store, now)
	if got == nil || got.Sessions != 2 || got.Since != "2026-09-28" || !got.WindowStart.Equal(windowStart) || !got.StartFloor.Equal(floor) || got.Command != "agent-archive backfill --since 2026-09-28" {
		t.Fatalf("pre-setup sessions = %+v", got)
	}
	if got := preSetupSessionsFor("codex", cfg, refused[2:], archived, store, now); got != nil {
		t.Fatalf("counted sessions outside the window or already archived: %+v", got)
	}
	noFloor := cfg
	noFloor.Discovery = &config.DiscoveryConfig{Enabled: true}
	if got := preSetupSessionsFor("codex", noFloor, refused, archived, store, now); got != nil {
		t.Fatalf("counted sessions without a start floor: %+v", got)
	}
}

// The line needs discovery on and capture not paused (backfill refuses
// while paused).
func TestReadPreSetupSessionsNeedsDiscoveryAndActiveCapture(t *testing.T) {
	t.Parallel()
	floor := time.Date(2026, 10, 5, 21, 43, 0, 0, time.UTC)
	now := floor.Add(24 * time.Hour)
	home := t.TempDir()
	must(t, os.WriteFile(filepath.Join(home, "discovery-refused-starts.json"), []byte(`{"version":1,"sessions":[{"agent":"codex","native_session_id":"one","started_at":"2026-10-05T20:00:00Z","locator":"/codex/one.jsonl"}]}`), 0o600))
	store, err := state.Open(home)
	must(t, err)
	cfg := config.Config{Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true}, Discovery: &config.DiscoveryConfig{Enabled: true, Authorizations: []config.DiscoveryAuthorization{{Agent: "codex", ProjectRoot: "/p", NativeStartFloor: floor, DestinationID: config.Config{}.DestinationID()}}}}
	read := func(cfg config.Config, discoveryOn bool) *preSetupSessions {
		view := statusView{Apps: []appStatus{{Name: "codex", Discovery: &discovery.Health{Enabled: discoveryOn}}, {Name: "claude"}}}
		readPreSetupSessions(&view, cfg, home, store, nil, now)
		if view.Apps[1].PreSetupSessions != nil {
			t.Fatalf("a hook app got pre-setup sessions: %+v", view.Apps[1].PreSetupSessions)
		}
		return view.Apps[0].PreSetupSessions
	}
	if got := read(cfg, true); got == nil || got.Sessions != 1 {
		t.Fatalf("pre-setup sessions = %+v", got)
	}
	if got := read(cfg, false); got != nil {
		t.Fatalf("named with discovery off: %+v", got)
	}
	paused := cfg
	paused.Paused = true
	if got := read(paused, true); got != nil {
		t.Fatalf("named while paused: %+v", got)
	}
	disabled := cfg
	disabled.Archive.Enabled = false
	if got := read(disabled, true); got != nil {
		t.Fatalf("named with the archive off: %+v", got)
	}
}

func TestPreSetupNotesSingularAndPlural(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		sessions int
		want     string
	}{
		{1, "    ! 1 Codex session from the 7 days before setup was not captured.\n      Import it with agent-archive backfill --since 2026-09-28.\n"},
		{3, "    ! 3 Codex sessions from the 7 days before setup were not captured.\n      Import them with agent-archive backfill --since 2026-09-28.\n"},
	} {
		app := appStatus{Name: "codex", Hooks: "installed", PreSetupSessions: &preSetupSessions{Sessions: tc.sessions, Since: "2026-09-28", Command: "agent-archive backfill --since 2026-09-28"}}
		var out bytes.Buffer
		sc := statusScreen{style: styleFor(&out), now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
		sc.printRows(&out, []statusRow{sc.appRow(app, 0)})
		if !strings.Contains(out.String(), tc.want) {
			t.Fatalf("%d sessions:\n%s\nwant:\n%s", tc.sessions, out.String(), tc.want)
		}
	}
	var out bytes.Buffer
	sc := statusScreen{style: styleFor(&out)}
	sc.printRows(&out, []statusRow{sc.appRow(appStatus{Name: "codex", Hooks: "installed"}, 0)})
	if strings.Contains(out.String(), "before setup") {
		t.Fatalf("named pre-setup sessions with none:\n%s", out.String())
	}
}
