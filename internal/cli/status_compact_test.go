package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// compactProjects are n included projects under the fixture's home.
func compactProjects(n int) []projectCaptureStatus {
	names := []string{"agent-archive", "styleprofile", "personal_website", "notes", "infra", "blog", "dotfiles", "api", "web-app", "scratch"}
	projects := make([]projectCaptureStatus, n)
	for i := range projects {
		name := names[i%len(names)]
		if i >= len(names) {
			name = fmt.Sprintf("%s-%d", name, i/len(names))
		}
		projects[i] = projectCaptureStatus{ProjectRoot: "/Users/alex/" + name, Configured: true, VerificationState: "not_verified"}
	}
	return projects
}

// mixedStatusView is the status of a Mac capturing three apps across ten
// projects: Claude Code busy with subagents, imports, uploads (one never
// published) and gaps; Cursor with one upload; Codex with hooks nobody has
// approved yet. The last pass failed on storage access.
func mixedStatusView() statusView {
	claudeProjects := compactProjects(10)
	claudeProjects[0].ReadBackVerified, claudeProjects[0].Published, claudeProjects[0].sessions, claudeProjects[0].imported, claudeProjects[0].uploading = true, true, 150, 40, 2
	claudeProjects[1].Published, claudeProjects[1].sessions, claudeProjects[1].imported, claudeProjects[1].uploading = true, 62, 8, 1
	status := state.Status{LastScanAt: renderNow.Add(-time.Minute), LastPublishedAt: renderNow.Add(-2 * time.Minute), PendingCount: 4}
	status.SetLastErrors("list registrations: AccessDenied: Access Denied")
	gaps := make([]archive.CaptureGap, 109)
	for i := range gaps {
		gaps[i] = archive.CaptureGap{Code: "transcript_rewritten"}
	}
	projects := make([]string, len(claudeProjects))
	for i, project := range claudeProjects {
		projects[i] = project.ProjectRoot
	}
	view := statusView{
		configured:               true,
		State:                    "Needs attention",
		problem:                  generalSyncProblem,
		lastErrorProblem:         generalSyncProblem,
		Next:                     "Check storage access and run agent-archive sync. To change credentials, run agent-archive setup and choose storage.",
		Storage:                  "r2 / agent-archive / agent-archive/",
		Authentication:           storageHealth{ConfigurationID: "id", State: "verified", CheckedAt: renderNow.Add(-2 * time.Minute)},
		StorageAccessConfirmedAt: renderNow.Add(-2 * time.Minute),
		StorageAccessConfirmedBy: storageAccessConfirmedByCollector,
		PrivacyEvidence:          storage.PrivacyReport{State: "not_verified", Reason: "r2_management_credentials_not_configured", GuidanceURL: "https://developers.cloudflare.com/r2/buckets/public-buckets/"},
		Background:               "loaded",
		SkillEvidence:            "body",
		Projects:                 projects,
		Apps: []appStatus{
			{
				Name: "claude", InstalledVersion: "2.1.283 (Claude Code)", Hooks: "installed", HookObserved: true, Published: true,
				State: "published; read-back pending", VerifiedAt: renderNow.Add(-time.Hour), PublishedSessions: 200, VerifiedSessions: 1,
				Sessions: 252, SubagentSessions: 40, ImportedSessions: 48, UploadingSessions: 3,
				Uploading: []uploadingSession{
					{ArchiveSessionID: "a", Project: "/Users/alex/agent-archive", StartedAt: time.Date(2026, 9, 25, 11, 12, 0, 0, time.UTC), State: uploadingFirst},
					{ArchiveSessionID: "b", Project: "/Users/alex/agent-archive", StartedAt: time.Date(2026, 9, 25, 11, 4, 0, 0, time.UTC), State: uploadingPending},
					{ArchiveSessionID: "c", Project: "/Users/alex/styleprofile", StartedAt: time.Date(2026, 9, 25, 9, 24, 0, 0, time.UTC), State: uploadingPending},
				},
				CaptureGaps: gaps, SessionsWithCaptureGaps: 17, Projects: claudeProjects,
			},
			{
				Name: "cursor", InstalledVersion: "3.21.13", Hooks: "installed", HookObserved: true, Published: true, State: "published; source verified",
				ReadBackVerified: true, VerifiedAt: renderNow.Add(-time.Hour), PublishedSessions: 14, VerifiedSessions: 14,
				Sessions: 14, ImportedSessions: 27, UploadingSessions: 1,
				Uploading: []uploadingSession{{ArchiveSessionID: "d", Project: "/Users/alex/personal_website", StartedAt: time.Date(2026, 9, 25, 11, 40, 0, 0, time.UTC), State: uploadingPending}},
				Projects:  compactProjects(10),
			},
			{Name: "codex", InstalledVersion: "codex-cli 0.155.0-alpha.9.2", Hooks: "installed", State: "waiting for first session", Uploading: []uploadingSession{}, Projects: compactProjects(10)},
		},
		Collector:        status,
		ImportedSessions: 75, LastImport: "2026-09-27-1",
	}
	view.Code = statusCode(view.State)
	for i := range view.Apps {
		view.Apps[i].Code, view.Apps[i].VersionSupport = statusCode(view.Apps[i].State), "unverified"
	}
	return view
}

// healthyStatusView is a Ready install of three apps, every session
// uploaded and read back, in a private bucket.
func healthyStatusView(projects int) statusView {
	view := mixedStatusView()
	view.State, view.problem, view.lastErrorProblem, view.Next = "Ready", "", "", "Keep working. Run agent-archive list to inspect archived sessions."
	view.Collector.LastErrors, view.Collector.LastError = nil, ""
	checked := renderNow.Add(-time.Hour)
	view.PrivacyEvidence = storage.PrivacyReport{State: "verified_private", CheckedAt: &checked}
	for i := range view.Apps {
		app := &view.Apps[i]
		app.Uploading, app.UploadingSessions, app.CaptureGaps, app.SessionsWithCaptureGaps = []uploadingSession{}, 0, nil, 0
		app.HookObserved, app.Sessions, app.SubagentSessions, app.Projects = true, 3, 0, compactProjects(projects)
		app.PublishedSessions, app.VerifiedSessions = 3, 3
	}
	return view
}

// renderClaudeStatus is status claude, as the text status draws it.
func renderClaudeStatus(t *testing.T, view statusView) string {
	t.Helper()
	var out, errOut strings.Builder
	if code := printAppStatus(&out, &errOut, view, "claude", statusScreen{style: textStyle{}, now: renderNow, home: "/Users/alex"}); code != 0 {
		t.Fatalf("status claude exit %d: %s", code, errOut.String())
	}
	return out.String()
}

// The default status of a busy Mac: one line per app with its counts and
// the sessions it is uploading, the storage line with the last upload,
// and the failure said once, in the headline.
func TestStatusCompactScreens(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"mixed.txt":         renderStatus(mixedStatusView(), false),
		"mixed-verbose.txt": renderVerboseStatus(mixedStatusView()),
		"app-claude.txt":    renderClaudeStatus(t, mixedStatusView()),
		"healthy.txt":       renderStatus(healthyStatusView(10), false),
	} {
		golden.Check(t, filepath.Join("testdata", "status-compact", name), []byte(text))
	}
}

// A healthy install fits on one screen, however many projects it captures.
func TestStatusHealthyIsShortWhateverTheProjectCount(t *testing.T) {
	t.Parallel()
	few := renderStatus(healthyStatusView(1), false)
	if lines := strings.Count(few, "\n"); lines > 12 {
		t.Fatalf("a healthy status is %d lines:\n%s", lines, few)
	}
	if many := renderStatus(healthyStatusView(100), false); many != few {
		t.Fatalf("status grew with the project count:\n%s\nwant\n%s", many, few)
	}
	if mixed, busier := renderStatus(mixedStatusView(), false), func() string {
		view := mixedStatusView()
		for i := range view.Apps {
			view.Apps[i].Projects = compactProjects(80)
		}
		view.Projects = append(view.Projects, make([]string, 70)...)
		return renderStatus(view, false)
	}(); strings.Count(mixed, "\n") != strings.Count(busier, "\n") {
		t.Fatalf("mixed status grew with the project count:\n%s", busier)
	}
}

// The app line counts top-level sessions, their subagents apart, imports,
// and uploads, leaving out what is zero.
func TestStatusAppCounts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		app  appStatus
		want string
	}{
		{appStatus{}, "no sessions yet"},
		{appStatus{Sessions: 1}, "1 session"},
		{appStatus{Sessions: 4, PublishedSessions: 2, VerifiedSessions: 1, UploadingSessions: 3}, "4 sessions · 2 archived (1 verified) · 3 uploading"},
		{appStatus{Sessions: 1, PublishedSessions: 1}, "1 session · 1 archived"},
		{appStatus{Sessions: 252, SubagentSessions: 40, ImportedSessions: 48, UploadingSessions: 3}, "212 sessions (+40 subagents) · 48 imported · 3 uploading"},
		{appStatus{Sessions: 3, SubagentSessions: 1}, "2 sessions (+1 subagent)"},
		{appStatus{ImportedSessions: 2, UploadingSessions: 1}, "no sessions yet · 2 imported · 1 uploading"},
	} {
		if got := appCounts(tc.app); got != tc.want {
			t.Errorf("%+v: %q want %q", tc.app, got, tc.want)
		}
	}
	for version, want := range map[string]string{"2.1.283 (Claude Code)": "2.1.283", "codex-cli 0.155.0-alpha.9.2": "0.155.0-alpha.9.2", "3.21.13": "3.21.13", "dev": "dev", "": ""} {
		if got := displayVersion(version); got != want {
			t.Errorf("displayVersion(%q) = %q want %q", version, got, want)
		}
	}
}

// Default status lists at most five uploading sessions per app and counts
// the rest; status APP lists them all.
func TestStatusCapsUploadingRows(t *testing.T) {
	t.Parallel()
	view := mixedStatusView()
	claude := &view.Apps[0]
	claude.Uploading = nil
	for i := range 8 {
		claude.Uploading = append(claude.Uploading, uploadingSession{ArchiveSessionID: strconv.Itoa(i), Project: "/Users/alex/agent-archive", StartedAt: renderNow.Add(-time.Duration(i+1) * time.Minute), State: uploadingPending})
	}
	claude.UploadingSessions = 8
	text := renderStatus(view, false)
	if got := strings.Count(text, "      ~/agent-archive   started "); got != 5 {
		t.Errorf("%d rows shown, want 5:\n%s", got, text)
	}
	if !strings.Contains(text, "\n      … and 3 more (agent-archive status claude)\n") {
		t.Errorf("no count of the rest:\n%s", text)
	}
	full := renderClaudeStatus(t, view)
	if got := strings.Count(full, "      ~/agent-archive   started "); got != 8 || strings.Contains(full, "more (agent-archive status") {
		t.Errorf("status claude shows %d rows, want all 8:\n%s", got, full)
	}
	view.Apps[0].Uploading, view.Apps[0].UploadingSessions = view.Apps[0].Uploading[:5], 5
	if text := renderStatus(view, false); strings.Contains(text, "more (agent-archive status") {
		t.Errorf("five rows counted a rest:\n%s", text)
	}
}

// An uploading row says only what is unusual: never published, or failing
// with the kind of issue the last pass recorded (a ! row). Its start is the
// time today, the day otherwise.
func TestStatusUploadingRowWording(t *testing.T) {
	t.Parallel()
	view := mixedStatusView()
	view.Apps[0].Uploading = []uploadingSession{
		{Project: "/Users/alex/agent-archive", StartedAt: renderNow.Add(-2 * time.Hour), State: uploadingFailing, Issue: issueCaptureFailed},
		{Project: "/Users/alex/blog", StartedAt: renderNow.Add(-30 * time.Hour), State: uploadingFirst},
		{Project: "/Users/alex/api", StartedAt: renderNow.AddDate(-1, 0, 0), State: uploadingPending},
		{Project: "/Users/alex/api", State: uploadingFailing, Issue: "some_future_code"},
	}
	text := renderStatus(view, false)
	for _, want := range []string{
		"    ! ~/agent-archive   started 10:00   failed to capture or upload\n",
		"      ~/blog            started Sep 24   waiting for its first upload\n",
		"      ~/api             started Sep 25 2025\n",
		"    ! ~/api             start unknown   failed to capture or upload\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"in under a minute", "uploads in"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("rows carry timing text %q:\n%s", unwanted, text)
		}
	}
}

// Failing sessions come first, then the most recently started.
func TestStatusSortsUploadingFailingFirst(t *testing.T) {
	t.Parallel()
	sessions := []uploadingSession{
		{ArchiveSessionID: "old", StartedAt: renderNow.Add(-2 * time.Hour), State: uploadingPending},
		{ArchiveSessionID: "new", StartedAt: renderNow, State: uploadingFirst},
		{ArchiveSessionID: "failing", StartedAt: renderNow.Add(-3 * time.Hour), State: uploadingFailing},
	}
	sortUploading(sessions)
	if got := sessions[0].ArchiveSessionID + "," + sessions[1].ArchiveSessionID + "," + sessions[2].ArchiveSessionID; got != "failing,new,old" {
		t.Fatalf("order %s", got)
	}
}

// The failure is said once: a headline that states the last pass's lone
// error names its cause and the Storage section doesn't repeat it, while
// errors the headline doesn't state are all still shown, and --verbose
// shows every one.
func TestStatusSaysTheFailureOnce(t *testing.T) {
	t.Parallel()
	storageRowsOf := func(view statusView) string {
		text := renderStatus(view, false)
		_, storageSection, _ := strings.Cut(text, "\nStorage\n")
		return storageSection
	}
	view := mixedStatusView()
	if text := renderStatus(view, false); !strings.Contains(text, "  ! The last sync failed: storage refused access\n") || strings.Contains(storageRowsOf(view), "✗") {
		t.Errorf("lone error:\n%s", text)
	}
	if text := renderVerboseStatus(view); !strings.Contains(text, "  ✗ Storage refused access\n") {
		t.Errorf("status --verbose leaves out the error:\n%s", text)
	}

	// An error without a plain cause is named as recorded.
	view.Collector.SetLastErrors("list registrations: api error SlowDown: slow down")
	if text := renderStatus(view, false); !strings.Contains(text, "! The last sync failed: list registrations: api error SlowDown: slow down\n") || strings.Contains(storageRowsOf(view), "Last error") {
		t.Errorf("raw error:\n%s", text)
	}

	// Two errors: the general headline states neither, so both stay.
	view.Collector.SetLastErrors("2 session(s) failed to scan or publish", "list registrations: api error AccessDenied: Access Denied")
	if rows := failedStorageRows(view); len(rows) != 2 || !strings.Contains(renderStatus(view, false), "  ! The last sync failed\n") {
		t.Errorf("two errors: rows %q\n%s", rows, renderStatus(view, false))
	}

	// A later problem outranks the error's headline: the error is shown.
	view = mixedStatusView()
	view.problem = "Collection is paused"
	if rows := failedStorageRows(view); len(rows) != 1 || strings.Contains(renderStatus(view, false), "failed: storage") {
		t.Errorf("headline about something else: rows %q\n%s", rows, renderStatus(view, false))
	}

	// A headline from the failed sessions' kinds states their summary, not
	// the size-limit notice beside it.
	view = mixedStatusView()
	view.problem, view.lastErrorProblem, view.lastErrorByIssue = "Some sessions could not be captured", "Some sessions could not be captured", true
	summary := issueSummary(map[string]issueTally{issueCaptureFailed: {sessions: 1}})
	view.Collector.SetLastErrors(summary, collector.SizeLimitProblem(1))
	view.Collector.IssueCounts = map[string]int{issueCaptureFailed: 1}
	rows := failedStorageRows(view)
	if len(rows) != 1 || strings.Contains(rows[0], "failed to capture") || !strings.Contains(rows[0], "size limit") {
		t.Errorf("issue headline: rows %q", rows)
	}
	if text := renderStatus(view, false); !strings.Contains(text, "  ! Some sessions could not be captured\n") {
		t.Errorf("issue headline changed:\n%s", text)
	}
	if text := renderVerboseStatus(view); !strings.Contains(text, "✗ Last error: "+summary) {
		t.Errorf("status --verbose leaves out the summary:\n%s", text)
	}
}

// Default status leaves each project's progress, the included projects,
// skill evidence, the Imported line, the read-back progress and the
// pending count to --verbose.
func TestStatusCompactLeavesDetailToVerbose(t *testing.T) {
	t.Parallel()
	moved := []string{
		"~/styleprofile: waiting for first session",
		"· Projects: ~/agent-archive, ~/styleprofile",
		"· Skill evidence: body",
		"· Imported (all destinations): 75 sessions, 0 waiting to upload; last import 2026-09-27-1",
		"uploaded, read-back pending (1 of 10 projects verified)",
		"· Last upload: 2 minutes ago · 4 pending",
		"R2 object credentials can't inspect public access",
		"Run /hooks in Codex and approve the archive hooks; agent-archive can't see whether you have.",
		"· 17 sessions with a capture gap (109 gaps recorded; details in status --json)",
		"✗ Storage refused access",
	}
	text, verbose := renderStatus(mixedStatusView(), false), renderVerboseStatus(mixedStatusView())
	for _, row := range moved {
		if strings.Contains(text, row) {
			t.Errorf("default status shows %q:\n%s", row, text)
		}
		if !strings.Contains(verbose, row) {
			t.Errorf("status --verbose is missing %q:\n%s", row, verbose)
		}
	}
	for _, row := range []string{"· 17 sessions have capture gaps\n", "Approve the archive hooks with /hooks in Codex.\n", "  ! Codex 0.155.0-alpha.9.2   hooks installed   no sessions yet\n"} {
		if !strings.Contains(text, row) {
			t.Errorf("default status is missing %q:\n%s", row, text)
		}
	}
}

// A read-back failure still gets its own ! row; routine pending read-back
// doesn't.
func TestStatusCompactShowsReadBackFailures(t *testing.T) {
	t.Parallel()
	view := mixedStatusView()
	if strings.Contains(renderStatus(view, false), "Read-back") {
		t.Fatalf("routine read-back shown:\n%s", renderStatus(view, false))
	}
	view.Apps[0].readBackFailure = verificationEvidence{Outcome: verificationOutcomeMismatch, Attempts: 2, NextRetryAt: renderNow.Add(-time.Minute)}
	if text := renderStatus(view, false); !strings.Contains(text, "  ! Claude Code 2.1.283") || !strings.Contains(text, "    ! Read-back doesn't match what this machine uploaded (retrying on the next pass, 2 attempts so far)\n") {
		t.Fatalf("read-back failure:\n%s", text)
	}
}

// status APP takes an app's name in any case, refuses one it doesn't know
// or --json beside it, and says so for an app this machine doesn't capture.
func TestStatusAppArgument(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	cfg := pairTestConfig(now, []string{"codex"}, project)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	env := pairStatusEnv(t, home, userHome, now, "codex")
	for _, tc := range []struct {
		args  []string
		code  int
		out   string
		error string
	}{
		{[]string{"CODEX"}, 0, "\nCapture\n  ! Codex   discovery off; included projects   no sessions yet\n", ""},
		{[]string{"codex", "--verbose"}, 0, "\nDetails\n  Codex: waiting for first session", ""},
		{[]string{"gemini"}, 2, "", "agent-archive: status: unknown app \"gemini\"; choose one of codex, claude, cursor; run agent-archive status --help\n"},
		{[]string{"codex", "--json"}, 2, "", "agent-archive: status: an app and --json can't be combined; status --json lists every app under applications; run agent-archive status --help\n"},
		{[]string{"cursor"}, 1, "", "agent-archive: status: Cursor isn't one of the apps this installation captures (Codex). Run agent-archive setup to add it.\n"},
	} {
		var out, errOut strings.Builder
		code := runStatusCommand(tc.args, &out, &errOut, env)
		if code != tc.code || !strings.Contains(out.String(), tc.out) || errOut.String() != tc.error {
			t.Errorf("status %v: exit %d\nstdout:\n%s\nstderr: %q", tc.args, code, out.String(), errOut.String())
		}
	}
}

// status --json counts, per app, the top-level sessions its hooks
// registered and their subagents apart, its imports, and the sessions it is
// uploading, with each listed; only sessions the configuration publishes
// count, as for the rest of status.
func TestStatusCountsEachAppsSessions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	home, userHome, project, removed := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
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
	live := func(id, root, parent string) {
		t.Helper()
		subagentID := ""
		if parent != "" {
			subagentID = "agent-" + id
		}
		reg := archive.SessionRegistration{
			ArchiveSessionID: id, NativeSessionID: "native-" + id, ProjectID: archive.ProjectID(root), ProjectRoot: root,
			Harness: archive.Harness{Name: "codex", Version: "1.2.3"}, TranscriptPath: filepath.Join(root, id+".jsonl"),
			SessionStartedAt: now.Add(-time.Hour), RegisteredAt: now, AdmittedAt: now, ParentSessionID: parent,
			SubagentID: subagentID,
		}
		if err := store.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	live("fresh", project, "")
	live("failing", project, "")
	live("fresh-subagent", project, "fresh")
	live("elsewhere", removed, "")
	// A recorded gap with a request queued is pending, but it is a gap,
	// not an upload.
	live("gap", project, "")
	gap := archive.SourceBundle{ArchiveSessionID: "gap", Capture: archive.SourceCapture{Harness: archive.Harness{Name: "codex"}, CapturedAt: now.Add(-time.Hour)}}
	if err := statetest.SaveBlocked(store, "gap", gap, now.Add(-time.Hour), state.BlockedReasonTranscriptMissing); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRequest("gap", "stop", now); err != nil {
		t.Fatal(err)
	}
	saveImportedSession(t, store, now, "imported", project)
	// An imported subagent goes with its parent: neither an import nor an
	// upload of its own.
	importedChild := saveImportedSession(t, store, now, "imported-subagent", project)
	importedChild.ParentSessionID, importedChild.SubagentID = "imported", "agent-imported"
	if err := store.SaveRegistration(importedChild); err != nil {
		t.Fatal(err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	status.SessionIssues = map[string]string{"failing": issueCaptureFailed}
	if err := store.SaveStatus(status); err != nil {
		t.Fatal(err)
	}

	env := pairStatusEnv(t, home, userHome, now, "codex")
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	app := view.Apps[0]
	if app.Sessions != 5 || app.SubagentSessions != 1 || app.ImportedSessions != 1 || app.UploadingSessions != 3 || len(app.Uploading) != 3 {
		t.Fatalf("sessions=%d subagents=%d imported=%d uploading=%d %+v", app.Sessions, app.SubagentSessions, app.ImportedSessions, app.UploadingSessions, app.Uploading)
	}
	want := []uploadingSession{
		{ArchiveSessionID: "failing", Project: project, StartedAt: now.Add(-time.Hour), State: uploadingFailing, Issue: issueCaptureFailed},
		{ArchiveSessionID: "fresh", Project: project, StartedAt: now.Add(-time.Hour), State: uploadingFirst},
		{ArchiveSessionID: "imported", Project: project, StartedAt: now.AddDate(-1, 0, 0), State: uploadingFirst, Imported: true},
	}
	for i := range want {
		if app.Uploading[i] != want[i] {
			t.Errorf("uploading[%d] = %+v want %+v", i, app.Uploading[i], want[i])
		}
	}
	if pair := app.Projects[0]; pair.sessions != 4 || pair.imported != 1 || pair.uploading != 3 {
		t.Errorf("project counts sessions=%d imported=%d uploading=%d", pair.sessions, pair.imported, pair.uploading)
	}
	if text := statusOutput(t, env, "--verbose"); !strings.Contains(text, "discovery off; included projects   4 sessions (+1 subagent) · 1 archived (1 verified) · 1 imported · 3 uploading\n") {
		t.Errorf("status:\n%s", text)
	}
	if full := statusOutput(t, env, "codex"); !strings.Contains(full, "\nProjects\n  Project") || !strings.Contains(full, "   4          1          3           verified\n") {
		t.Errorf("status codex:\n%s", full)
	}
	asJSON := statusOutput(t, env, "--json")
	for _, field := range []string{`"subagent_sessions": 1`, `"imported_sessions": 1,`, `"uploading_sessions": 3`, `"archive_session_id": "failing"`, `"state": "failing"`, `"issue": "capture_failed"`, `"state": "first_upload"`, `"imported": true`} {
		if !strings.Contains(asJSON, field) {
			t.Errorf("status --json is missing %s:\n%s", field, asJSON)
		}
	}
}
