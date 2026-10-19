package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Before setup, status says so and nothing else: no empty authentication,
// no "unknown" collector.
func TestStatusBeforeSetupIsPlain(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	var out bytes.Buffer
	if code := Run([]string{"status"}, nil, &out, nil, env); code != 0 {
		t.Fatal(code)
	}
	if want := "Agent Archive  Not set up\n\nagent-archive setup\n"; out.String() != want {
		t.Fatalf("status:\n%q\nwant\n%q", out.String(), want)
	}
	out.Reset()
	Run([]string{"status", "--json"}, nil, &out, nil, env)
	var view map[string]any
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["background"] != "missing" || view["authentication"].(map[string]any)["state"] != "not_configured" {
		t.Fatalf("json: background=%v authentication=%v", view["background"], view["authentication"])
	}
}

// Collection is called stuck only when the lock's holder took it long ago:
// a pass that started a moment ago (after the machine woke, with the last scan
// hours old) is not, and neither is a record whose holder died, since
// nothing holds the lock then.
func TestStatusReportsAStuckCollectorLock(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	now := time.Now()
	env := pairStatusEnv(t, home, userHome, now)
	setUpTestConfig(t, home, t.TempDir(), now.Add(-48*time.Hour))
	store, _ := state.Open(home)
	must(t, store.SaveStatus(state.Status{LastScanAt: now.Add(-3 * time.Hour)}))
	stuck := func() (statusView, bool) {
		t.Helper()
		view, err := readStatus(env)
		if err != nil {
			t.Fatal(err)
		}
		return view, strings.Contains(view.Next, "Collection is stuck")
	}

	unlock, err := lockCollector(home, "scheduled collection", now.Add(-5*time.Second))
	must(t, err)
	if view, isStuck := stuck(); isStuck {
		t.Fatalf("a pass that just started is stuck: %s", view.Next)
	}
	unlock()
	if _, err := os.Stat(filepath.Join(home, collectorLockRecordName)); !os.IsNotExist(err) {
		t.Fatal("releasing the lock left its record")
	}

	unlock, err = lockCollector(home, "backfill undo", now.Add(-3*time.Hour))
	must(t, err)
	view, isStuck := stuck()
	if !isStuck || view.State != "Needs attention" || !strings.Contains(view.Next, "backfill undo") || !strings.Contains(view.Next, "3h 0m ago") || !strings.Contains(view.Next, strconv.Itoa(os.Getpid())) {
		t.Fatalf("state %q next %q", view.State, view.Next)
	}
	unlock()

	// A holder that died leaves its record, but nothing holds the lock.
	must(t, local.Write(filepath.Join(home, collectorLockRecordName), collectorLockRecord{Holder: "sync", PID: 1, Since: now.Add(-3 * time.Hour)}))
	if view, isStuck := stuck(); isStuck {
		t.Fatalf("a dead holder's record reads as stuck: %s", view.Next)
	}

	// The same old record while a new holder has the lock but has not yet
	// written its own: the PID it names is gone, so status must not send the
	// user to kill it (by then the number may belong to another process).
	dead := exec.CommandContext(t.Context(), "/usr/bin/true")
	must(t, dead.Run())
	must(t, local.Write(filepath.Join(home, collectorLockRecordName), collectorLockRecord{Holder: "sync", PID: dead.Process.Pid, Since: now.Add(-3 * time.Hour)}))
	unlock, err = local.Lock(home)
	must(t, err)
	if view, isStuck := stuck(); isStuck {
		t.Fatalf("a record naming an exited process reads as stuck: %s", view.Next)
	}
	unlock()
}

// The collector's quarantined files and unrefreshable summaries, already in
// status --json, show in the text status too.
func TestStatusShowsQuarantinedFilesAndUnrefreshableSummaries(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	now := time.Now()
	env := pairStatusEnv(t, home, userHome, now)
	setUpTestConfig(t, home, t.TempDir(), now.Add(-48*time.Hour))
	store, _ := state.Open(home)
	must(t, store.SaveStatus(state.Status{LastScanAt: now, QuarantinedFiles: []string{"registrations/x.json.corrupt"}, UnrefreshableSummaries: 2}))
	var out bytes.Buffer
	if code := Run([]string{"status", "--verbose"}, nil, &out, nil, env); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "! 1 local state file couldn't be read and was moved aside") || !strings.Contains(out.String(), "· 2 session summaries can't be refreshed") {
		t.Fatalf("status:\n%s", &out)
	}
}

func TestStatusJSONAndTextUseObservedEvidence(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project), 0)
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "Waiting for capture" || view.Apps[0].State != "waiting for first session" {
		t.Fatalf("invented capture: %+v", view)
	}
	var out bytes.Buffer
	if code := Run([]string{"status", "--json"}, nil, &out, nil, env); code != 0 {
		t.Fatal(code)
	}
	var decoded statusView
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.State != view.State || decoded.Next != view.Next {
		t.Fatal("JSON does not match model")
	}
	env.OpenStore = nil // status must not resolve storage at all.
	cfg, _, _ := config.Load(home)
	cfg.Paused = true
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	view, err = readStatus(env)
	if err != nil || view.State != "Paused" {
		t.Fatal(view, err)
	}
	cfg.Paused = false
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := local.Write(setupjournal.JournalPath(home), setupjournal.Journal{}); err != nil {
		t.Fatal(err)
	}
	view, err = readStatus(env)
	if err != nil || view.State != "Setup needs recovery" {
		t.Fatal(view, err)
	}
}

func TestStatusReadDoesNotCreateCollectorLayout(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/project", time.Now())
	env := testEnv(t, home, time.Now())
	env.Scheduler = newFakeScheduler(t, "unknown")
	if _, err := readStatus(env); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"registrations", "requests", "published", "sessions"} {
		if _, err := os.Stat(filepath.Join(home, dir)); !os.IsNotExist(err) {
			t.Fatal("status created", dir)
		}
	}
}

// Unset times are left out of status --json rather than printed as the
// zero time (0001-01-01T00:00:00Z).
func TestStatusJSONOmitsUnsetTimes(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	env, home, _, _ := publishedThroughSync(t, now)
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.StorageVerifiedAt = time.Time{}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := runStatusCommand([]string{"--json"}, &out, &out, env); code != 0 {
		t.Fatalf("exit=%d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "0001-01-01") {
		t.Fatalf("status --json prints a zero time:\n%s", out.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, present := doc["storage_verified_at"]; present {
		t.Fatalf("unset storage_verified_at is present: %v", doc["storage_verified_at"])
	}
	if _, present := doc["storage_access_confirmed_at"]; !present {
		t.Fatalf("a set time was dropped")
	}
	// The collector's own status file: a pass that never published has no
	// last_published_at.
	for _, value := range []any{state.Status{}, storageHealth{}} {
		encoded, err := json.Marshal(value)
		if err != nil || strings.Contains(string(encoded), "_at") {
			t.Fatalf("%T encodes an unset time: %s %v", value, encoded, err)
		}
	}
	// Files written before this change spell the zero time out; they still
	// read as "never", as a missing key does.
	var old state.Status
	if err := json.Unmarshal([]byte(`{"last_scan_at":"0001-01-01T00:00:00Z","last_published_at":"0001-01-01T00:00:00Z","pending_count":0}`), &old); err != nil || !old.LastScanAt.IsZero() || !old.LastPublishedAt.IsZero() {
		t.Fatalf("old status file: %+v %v", old, err)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestStatusReportsJournalWithoutConfiguration(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := local.Write(setupjournal.JournalPath(home), setupjournal.Journal{}); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(testEnv(t, home, time.Now()))
	if err != nil || view.State != "Setup needs recovery" || view.Code != "recovery_required" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestStatusPreservesPublicationDuringRateLimitedUpdate(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, project), 0)
	path := writeCodexTranscript(t, project)
	if err := handleTestHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native", "cwd": project, "transcript_path": path}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	before, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Append a record: an in-place edit would be a rewrite (a recorded
	// capture gap), not a rate-limited update.
	update := `{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"updated"}}`
	if err := os.WriteFile(path, append(append(b, '\n'), update...), 0600); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now.Add(time.Second) }
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	after, err := readStatus(env)
	if err != nil || after.Apps[0].LastPublishedAt.IsZero() || !after.Apps[0].LastPublishedAt.Equal(before.Apps[0].LastPublishedAt) || after.State == "Waiting for capture" || after.Collector.PendingCount != 1 {
		t.Fatalf("status=%+v err=%v", after, err)
	}
}

func TestStatusShowsNotSetUp(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	var out, errOut bytes.Buffer
	if code := runStatusCommand(nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), "Not set up") {
		t.Fatalf("out=%s", out.String())
	}
}
