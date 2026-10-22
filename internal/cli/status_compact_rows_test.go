package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A headline names one kind of failed session. When the last pass had
// another kind that needs the user too, the summary naming both stays on
// its ✗ row, beside the size-limit notice.
func TestStatusKeepsOtherIssueKindsInTheLastError(t *testing.T) {
	t.Parallel()
	view := mixedStatusView()
	view.problem, view.lastErrorProblem, view.lastErrorByIssue = "Some sessions could not be captured", "Some sessions could not be captured", true
	summary := issueSummary(map[string]issueTally{issueCaptureFailed: {sessions: 2}, issueRetentionFailed: {sessions: 3}})
	view.Collector.SetLastErrors(summary, collector.SizeLimitProblem(1))
	view.Collector.IssueCounts = map[string]int{issueCaptureFailed: 2, issueRetentionFailed: 3}
	rows := failedStorageRows(view)
	if len(rows) != 2 || !strings.Contains(rows[0], "2 sessions failed to capture or upload") || !strings.Contains(rows[0], "3 sessions couldn't be removed after the retention period") || !strings.Contains(rows[1], "size limit") {
		t.Fatalf("rows %q", rows)
	}
	// A kind with nothing to do beside the headline's kind adds nothing.
	view.Collector.IssueCounts = map[string]int{issueCaptureFailed: 2, issueSubagentNotCaptured: 4}
	if rows := failedStorageRows(view); len(rows) != 1 || !strings.Contains(rows[0], "size limit") {
		t.Fatalf("with a nothing-to-do kind: rows %q", rows)
	}
}

// While the headline says the last pass failed on storage, the destination
// row doesn't call storage reachable on the strength of an older check.
func TestStatusDestinationAgreesWithAStorageHeadline(t *testing.T) {
	t.Parallel()
	sc := statusScreen{now: renderNow, home: "/Users/alex"}
	destination := func(view statusView) statusRow { return sc.storageRows(view)[0] }
	view := mixedStatusView()
	if row := destination(view); row.mark != symbolWarn || row.detail != "refused access on the last pass · uploaded 2 minutes ago" {
		t.Errorf("storage refused: %q %q", row.mark, row.detail)
	}
	view.Collector.SetLastErrors("list registrations: operation error S3: ListObjectsV2, api error NoSuchBucket: gone")
	if row := destination(view); row.detail != "bucket not found on the last pass · uploaded 2 minutes ago" {
		t.Errorf("no bucket: %q", row.detail)
	}
	view.Collector.SetLastErrors("list registrations: dial tcp: connection refused")
	if row := destination(view); row.mark != symbolWarn || !strings.HasPrefix(row.detail, "unreachable on the last pass") {
		t.Errorf("outage: %q %q", row.mark, row.detail)
	}
	for recorded, want := range map[string]string{
		"list registrations: operation error S3: ListObjectsV2, https response error StatusCode: 500": "! unreachable on the last pass",
		"open storage: load credentials: no profile":                                                  "! unreachable on the last pass",
		backgroundCredentialProcessFailure:                                                            "! couldn't get credentials on the last pass",
		// Not about storage: the clock hold proves storage answered, and
		// the others are this machine's own files and checks.
		"retention: this machine's clock is ahead of the storage service's; retention deletes nothing until it is corrected (by 5 minutes)": "✓ reachable",
		"list registrations: open /Users/alex/.agent-archive/registrations: permission denied":                                              "✓ reachable",
		"collection succeeded but retention cleanup failed: remove local copy: permission denied":                                           "✓ reachable",
	} {
		failing := mixedStatusView()
		failing.Collector.SetLastErrors(recorded)
		if row := destination(failing); row.mark+" "+strings.TrimSuffix(row.detail, " · uploaded 2 minutes ago") != want {
			t.Errorf("%q: %q %q want %q", recorded, row.mark, row.detail, want)
		}
	}
	// A destination the collector already found failing keeps its ✗.
	signInFailed := mixedStatusView()
	signInFailed.Authentication = storageHealth{State: "authentication_failed", CheckedAt: renderNow.Add(-time.Minute)}
	if row := destination(signInFailed); row.mark != symbolFail || !strings.HasPrefix(row.detail, "sign-in failed") {
		t.Errorf("sign-in failed: %q %q", row.mark, row.detail)
	}
	// A headline from the failed sessions' kinds is not about storage.
	byIssue := mixedStatusView()
	byIssue.problem, byIssue.lastErrorProblem, byIssue.lastErrorByIssue = "Some sessions could not be captured", "Some sessions could not be captured", true
	if row := destination(byIssue); row.mark != symbolOK || !strings.HasPrefix(row.detail, "reachable") {
		t.Errorf("issue headline: %q %q", row.mark, row.detail)
	}
	// A headline about something else leaves the row as the check found it.
	view.problem = "Collection is paused"
	if row := destination(view); row.mark != symbolOK || !strings.HasPrefix(row.detail, "reachable") {
		t.Errorf("other headline: %q %q", row.mark, row.detail)
	}
}

// End to end: a pass whose failed sessions decide the headline is not a
// storage failure, so the destination stays reachable and the summary the
// headline states is not repeated.
func TestStatusIssueHeadlineEndToEnd(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := pairTestConfig(now, []string{"codex"}, project)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	publishPairSession(t, home, store, storagetest.NewMemoryStore(), cfg, now, "published", project, true)
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	status.IssueCounts = map[string]int{issueCaptureFailed: 1}
	status.SessionIssues = map[string]string{"published": issueCaptureFailed}
	status.SetLastErrors(issueSummary(map[string]issueTally{issueCaptureFailed: {sessions: 1}}))
	if err := store.SaveStatus(status); err != nil {
		t.Fatal(err)
	}
	env := pairStatusEnv(t, home, userHome, now, "codex")
	env.Now = func() time.Time { return now }
	text := statusOutput(t, env, "codex")
	if !strings.Contains(text, "  ! Some sessions could not be captured\n") || strings.Contains(text, "on the last pass") || strings.Contains(text, "✗") {
		t.Fatalf("status:\n%s", text)
	}
}

// A configuration without apps says so under Capture, with what is still
// pending, instead of an empty section; an app it only imported from is
// not listed on its own.
func TestStatusWithoutAppsSaysSo(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := pairTestConfig(now, nil, project)
	cfg.ImportedHarnesses = []string{"codex"}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	saveImportedSession(t, store, now, "imported", project)
	text := statusOutput(t, pairStatusEnv(t, home, userHome, now), "--verbose")
	if !strings.Contains(text, "\nCapture\n  · No apps selected · 1 pending\n") || strings.Contains(text, "imported only") {
		t.Fatalf("status:\n%s", text)
	}
}

// An app whose sessions were only imported counts its imports alone.
func TestStatusImportedOnlyAppRow(t *testing.T) {
	t.Parallel()
	view := mixedStatusView()
	view.importedApps = []appStatus{{Name: "cursor", ImportedSessions: 5, UploadingSessions: 1, Uploading: []uploadingSession{}}}
	if text := renderStatus(view, false); !strings.Contains(text, "  · Cursor                    imported only     5 imported · 1 uploading\n") {
		t.Fatalf("status:\n%s", text)
	}
}

// A session's start is shown in the clock's own time zone, today as a
// time, even when UTC is already on the next day.
func TestStatusStartedUsesTheLocalZone(t *testing.T) {
	t.Parallel()
	zone := time.FixedZone("PDT", -7*60*60)
	sc := statusScreen{now: time.Date(2026, 9, 25, 20, 0, 0, 0, zone)}
	if got := sc.started(time.Date(2026, 9, 26, 1, 30, 0, 0, time.UTC)); got != "started 18:30" {
		t.Errorf("same local day: %q", got)
	}
	if got := sc.started(time.Date(2026, 9, 25, 6, 0, 0, 0, time.UTC)); got != "started Sep 24" {
		t.Errorf("previous local day: %q", got)
	}
}

// status APP shows the configured app when an import-only entry has the
// same name.
func TestStatusAppPrefersTheConfiguredApp(t *testing.T) {
	t.Parallel()
	view := mixedStatusView()
	view.importedApps = []appStatus{{Name: "claude", ImportedSessions: 9, Uploading: []uploadingSession{}}}
	if text := renderClaudeStatus(t, view); !strings.Contains(text, "  ✓ Claude Code 2.1.283   hooks installed   212 sessions") || strings.Contains(text, "imported only") {
		t.Fatalf("status claude:\n%s", text)
	}
}

// The footer suggests the first app with sessions, else the first app.
func TestStatusFooterApp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		apps []appStatus
		want string
	}{
		{[]appStatus{{Name: "codex"}, {Name: "claude", Sessions: 2}}, "claude"},
		{[]appStatus{{Name: "codex"}, {Name: "cursor", ImportedSessions: 1}}, "cursor"},
		{[]appStatus{{Name: "codex"}, {Name: "claude"}}, "codex"},
		{nil, ""},
	} {
		if got := footerApp(statusView{Apps: tc.apps}); got != tc.want {
			t.Errorf("%+v: %q want %q", tc.apps, got, tc.want)
		}
	}
	view := mixedStatusView()
	view.Apps[0], view.Apps[2] = view.Apps[2], view.Apps[0]
	if text := renderStatus(view, false); !strings.HasSuffix(text, "\nMore: agent-archive status --verbose · agent-archive status cursor\n") {
		t.Fatalf("status:\n%s", text)
	}
}

// A resumed subagent still running is neither uploading nor failing: the
// short status says nothing about it, and --verbose counts it with the
// other subagents that are not a problem.
func TestStatusRunningSubagentsAreVerboseOnly(t *testing.T) {
	t.Parallel()
	view := mixedStatusView()
	view.Collector.RunningSubagents = 2
	if text, want := renderStatus(view, false), renderStatus(mixedStatusView(), false); text != want {
		t.Fatalf("running subagents changed the short status:\n%s", text)
	}
	if text := renderVerboseStatus(view); !strings.Contains(text, "  Subagents:     2 still running (archived up to their last stop)\n") {
		t.Fatalf("status --verbose:\n%s", text)
	}
}

// status APP leads with the same headline as the default status, so it
// shows the Storage section too: the errors the headline leaves to it, and
// the destination the last pass failed on.
func TestStatusAppShowsTheLastPassErrors(t *testing.T) {
	t.Parallel()
	view := mixedStatusView()
	view.problem, view.lastErrorProblem = generalSyncProblem, generalSyncProblem
	view.Collector.SetLastErrors(
		"list registrations: operation error S3: ListObjectsV2, https response error StatusCode: 403, api error AccessDenied: Access Denied",
		"collection succeeded but retention cleanup failed: open sessions: permission denied",
	)
	text := renderClaudeStatus(t, view)
	if !strings.Contains(text, "\nStorage\n") || !strings.Contains(text, "retention cleanup failed") || !strings.Contains(text, "Storage refused access") {
		t.Fatalf("status claude hides the last pass's errors:\n%s", text)
	}
	if !strings.Contains(text, "! r2://agent-archive/agent-archive/") {
		t.Fatalf("status claude shows storage as fine after it refused access:\n%s", text)
	}
}
