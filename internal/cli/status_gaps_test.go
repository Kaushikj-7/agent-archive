package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
)

// A session blocked by one oversize record shows as a capture gap with its
// own explanation, and never as an error. (The collector's record limit
// cannot be lowered from this package; the collector's own tests drive the
// block and show the pass records no error, which is what sync's exit code
// is made of. This checks the status wiring.)
func TestStatusReportsTheRecordSizeLimitAsAGap(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	env, home, _, _ := publishedThroughSync(t, now)
	reg, _ := onlyRegistration(t, home)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, publishedAt, found, err := store.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("setup: %v", err)
	}
	if err := statetest.SaveBlocked(store, reg.ArchiveSessionID, bundle, publishedAt, state.BlockedReasonRecordTooLarge); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Collector.LastError != "" {
		t.Fatalf("a record-size gap surfaced as an error: %q", view.Collector.LastError)
	}
	gaps := view.Apps[0].CaptureGaps
	if len(gaps) != 1 || gaps[0].Code != "record_size_limit" || !strings.Contains(gaps[0].Detail, "64 MiB record size limit") || !strings.Contains(gaps[0].Detail, "resumes when the transcript changes") {
		t.Fatalf("gaps = %+v", gaps)
	}
	var out strings.Builder
	if code := runStatusCommand(nil, &out, os.Stderr, env); code != 0 || strings.Contains(out.String(), "Last error") || !strings.Contains(out.String(), "1 session has capture gaps") {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
}

// A session stopped at a size limit whose limit was recorded is listed by
// status APP with that limit and its size, and its gap says which limit and
// how to go on capturing.
func TestStatusAppListsSessionsStoppedAtASizeLimit(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	env, home, _, _ := publishedThroughSync(t, now)
	reg, _ := onlyRegistration(t, home)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, publishedAt, found, err := store.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("setup: %v", err)
	}
	if err := statetest.SaveBlocked(store, reg.ArchiveSessionID, bundle, publishedAt, state.BlockedReasonTranscriptTooLarge); err != nil {
		t.Fatal(err)
	}
	limit := state.SizeLimitGap{Kind: state.SizeLimitRaw, LimitBytes: 512 << 20, SizeBytes: 600_000_000}
	if err := store.SaveScanSignature(reg.ArchiveSessionID, state.ScanSignature{Failed: true, Blocked: state.BlockedReasonTranscriptTooLarge, SizeLimit: &limit}); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	app := view.Apps[0]
	if len(app.SizeLimitedSessions) != 1 || app.SizeLimitedSessions[0] != (sizeLimitedSession{ArchiveSessionID: reg.ArchiveSessionID, Limit: "Codex raw 512 MiB", SizeBytes: 600_000_000}) {
		t.Fatalf("listed = %+v", app.SizeLimitedSessions)
	}
	gaps := app.CaptureGaps
	if len(gaps) != 1 || gaps[0].Code != "transcript_too_large" || !strings.Contains(gaps[0].Detail, "at 600.0 MB is over the Codex raw 512 MiB limit") || !strings.Contains(gaps[0].Detail, "agent-archive handoff "+reg.ArchiveSessionID) {
		t.Fatalf("gaps = %+v", gaps)
	}
	var out strings.Builder
	if code := runStatusCommand([]string{"codex", "--no-pager"}, &out, os.Stderr, env); code != 0 || !strings.Contains(out.String(), "Stopped at a size limit") || !strings.Contains(out.String(), reg.ArchiveSessionID+"  600.0 MB, over the Codex raw 512 MiB limit") {
		t.Fatalf("status codex exit=%d output=%s", code, out.String())
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestBlockedCaptureIsNotPendingAndStatusReportsGap(t *testing.T) {
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
	// Compaction rewrote the transcript to something that no longer extends
	// the published snapshot, and a stop hook asked for a flush.
	if err := os.WriteFile(path, []byte(`{"type":"turn_context","model":"gpt-test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := handleTestHookEvent(home, "codex", map[string]any{"hook_event_name": "Stop", "session_id": "native", "cwd": project, "transcript_path": path}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now.Add(2 * time.Second) }
	for pass := range 2 {
		result, err := runOnePass(env, false)
		if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
			t.Fatalf("pass %d: result=%+v err=%v", pass, result, err)
		}
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := pendingSessions(home, cfg); err != nil || pending != 0 {
		t.Fatalf("blocked session counted as pending: %d err=%v", pending, err)
	}
	next := cfg
	next.Storage.Bucket = "another-bucket"
	if err := reviewChanges(home, cfg, next, newPrompter(strings.NewReader(""), os.Stdout), env); err != nil {
		t.Fatalf("blocked session prevented a destination change: %v", err)
	}
	after, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if after.Collector.LastError != "" || after.Collector.PendingCount != 0 || len(after.Apps[0].CaptureGaps) != 1 || !after.Apps[0].LastPublishedAt.Equal(before.Apps[0].LastPublishedAt) || after.Apps[0].State != "published; source verified" {
		t.Fatalf("status=%+v", after)
	}
	var out strings.Builder
	if code := runStatusCommand(nil, &out, os.Stderr, env); code != 0 {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "1 session has capture gaps") || strings.Contains(out.String(), "Last error") {
		t.Fatalf("status output=%s", out.String())
	}
}

// Applications delete their own transcripts. That is a capture gap, reported
// as such with its own explanation, and never a collector error.
func TestStatusReportsDeletedTranscriptAsGapNotError(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	env, _, path, _ := publishedThroughSync(t, now)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now.Add(time.Hour) }
	for pass := range 2 {
		if result, err := runOnePass(env, false); err != nil || len(result.Errors) != 0 {
			t.Fatalf("pass %d: %+v %v", pass, result, err)
		}
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Collector.LastError != "" || view.Collector.PendingCount != 0 {
		t.Fatalf("a deleted transcript surfaced as an error: %+v", view.Collector)
	}
	gaps := view.Apps[0].CaptureGaps
	if len(gaps) != 1 || gaps[0].Code != string(state.BlockedReasonTranscriptMissing) || !strings.Contains(gaps[0].Detail, "resumes by itself") {
		t.Fatalf("gaps=%+v", gaps)
	}
	var out strings.Builder
	if code := runStatusCommand(nil, &out, os.Stderr, env); code != 0 || strings.Contains(out.String(), "Last error") || !strings.Contains(out.String(), "1 session has capture gaps") {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
}
