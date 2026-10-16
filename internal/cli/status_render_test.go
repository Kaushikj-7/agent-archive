package cli

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// renderNow is the clock the rendering tests draw status with.
var renderNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// busyStatusView is a status with something in every row: several apps in
// different states, a failing read-back, gaps, diagnostics, imports,
// warnings, and the codes and exact times status --json carries for each.
func busyStatusView() statusView {
	checked := renderNow.Add(-3 * time.Hour)
	return statusView{
		configured: true,
		State:      "Needs attention",
		problem:    "The last sync failed",
		Next:       "Check storage access and run agent-archive sync. To change credentials, run agent-archive setup and choose storage.",
		Storage:    "s3 / team-archive / agent-archive/",
		Authentication: storageHealth{
			ConfigurationID: "id", State: "verified", CheckedAt: renderNow.Add(-2 * time.Minute), Context: "manual_sync",
		},
		StorageAccessConfirmedAt: renderNow.Add(-2 * time.Minute),
		StorageAccessConfirmedBy: storageAccessConfirmedByCollector,
		PrivacyEvidence:          storage.PrivacyReport{State: "not_verified", Reason: "inspection_stale", CheckedAt: &checked, GuidanceURL: "https://example.com/guide"},
		Background:               "loaded",
		Projects:                 []string{"/Users/alex/src/web-app", "/Users/alex/src/api"},
		Apps: []appStatus{
			{
				Name: "codex", InstalledVersion: "0.121.0", Hooks: "installed", Trust: "unknown",
				State: "published; source verified", VerificationState: "verified_at_recorded_time",
				HookObserved: true, Published: true, ReadBackVerified: true, PublishedSessions: 2, Sessions: 2,
				VerifiedAt: renderNow.Add(-26 * time.Hour), VersionSupport: "unverified", VersionSupportReason: supportReasonNoMatchingVersion,
				CaptureGaps:             []archive.CaptureGap{{Code: "transcript_rewritten"}, {Code: "record_too_large"}},
				SessionsWithCaptureGaps: 1,
			},
			{
				Name: "claude", InstalledVersion: "2.1.90", Hooks: "installed", Trust: "unknown",
				State: "published; read-back pending", VerificationState: "read_back_failed",
				HookObserved: true, Published: true, PublishedSessions: 1, Sessions: 1, VerifiedAt: renderNow.Add(-time.Hour),
				VerificationDetail: "failed: not found (attempt 2; next retry 2026-09-25T12:05:00Z)",
				readBackFailure:    verificationEvidence{Outcome: verificationOutcomeFailed, Attempts: 2, LastError: "not found", NextRetryAt: renderNow.Add(5 * time.Minute)},
				Projects: []projectCaptureStatus{
					{ProjectRoot: "/Users/alex/src/web-app", Published: true, ReadBackVerified: true, VerificationState: "verified_at_recorded_time"},
					{ProjectRoot: "/Users/alex/src/api", Published: true, VerificationState: "incomplete"},
				},
			},
			{Name: "cursor", Hooks: "missing or incomplete", Trust: "unknown", State: "waiting for first session", VerificationState: "not_verified", VersionSupport: "absent"},
		},
		CaptureDiagnostics: []capture.Diagnostic{{Code: capture.DiagnosticSetupInProgress, ProjectRoot: "/Users/alex/src/api", Harness: "claude", ObservedAt: renderNow.Add(-40 * time.Minute)}},
		Collector: state.Status{
			LastScanAt: renderNow.Add(-time.Minute), LastPublishedAt: renderNow.Add(-50 * time.Minute), PendingCount: 3,
			LastError:        "list registrations: AccessDenied: Access Denied",
			QuarantinedFiles: []string{"registrations/x.json.corrupt", "registrations/y.json.corrupt"}, UnrefreshableSummaries: 1,
		},
		ImportedSessions: 4, ImportedPending: 1, LastImport: "import-7",
		Warnings: []string{"Installed versions could not be read from /Users/alex/.agent-archive/application-versions.json: bad JSON. Run agent-archive setup to refresh them."},
	}
}

func renderStatus(view statusView, color bool) string {
	var out strings.Builder
	printStatus(&out, view, statusScreen{style: textStyle{color: color}, now: renderNow, home: "/Users/alex"})
	return out.String()
}

func renderVerboseStatus(view statusView) string {
	var out strings.Builder
	printStatus(&out, view, statusScreen{style: textStyle{}, now: renderNow, home: "/Users/alex", verbose: true})
	return out.String()
}

// The text status words everything: exact times and the codes status --json
// carries stay there.
func TestStatusTextShowsNoCodesOrExactTimes(t *testing.T) {
	t.Parallel()
	text := renderStatus(busyStatusView(), false)
	if exact := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`).FindString(text); exact != "" {
		t.Errorf("status shows the exact time %s:\n%s", exact, text)
	}
	// Codes are snake_case words; the file names in a warning are not.
	if code := regexp.MustCompile(`\b[a-z]+_[a-z_]+\b`).FindString(regexp.MustCompile(`\S+\.json\S*`).ReplaceAllString(text, "")); code != "" {
		t.Errorf("status shows the code %s:\n%s", code, text)
	}
	// A storage error is shown by its cause, not the provider's raw text.
	if strings.Contains(text, "AccessDenied") {
		t.Errorf("status shows the raw storage error:\n%s", text)
	}
	for _, want := range []string{
		"Agent Archive  ● Needs attention\n",
		"  ! The last sync failed\n",
		"    Check storage access and run agent-archive sync.\n    To change credentials, run agent-archive setup and choose storage.\n",
		"  ✓ Codex 0.121.0        hooks installed   2 sessions · 2 archived\n",
		"    · 1 session has capture gaps\n",
		"  ! Claude Code 2.1.90   hooks installed   1 session · 1 archived\n",
		"  ! Cursor               hooks missing     no sessions yet\n",
		"Read-back failed: not found (retrying in 5 minutes, 2 attempts so far)",
		"Claude Code skipped a session in ~/src/api 40 minutes ago: setup was still in progress",
		"  ✓ s3://team-archive/agent-archive/   reachable · uploaded 50 minutes ago\n",
		"  ! Bucket privacy not verified        not checked in over a day\n",
		"last scan 1 minute ago",
		"✗ Storage refused access\n",
		"! 2 local state files couldn't be read and were moved aside",
		"· 1 session summary can't be refreshed",
		"! Installed versions could not be read from ~/.agent-archive/application-versions.json",
		"\nMore: agent-archive status --verbose · agent-archive status codex\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status is missing %q:\n%s", want, text)
		}
	}
}

// Color is a role, never the only signal, and a line colors at most one
// element: the symbol, not the sentence after it.
func TestStatusColorsAtMostOneElementPerLine(t *testing.T) {
	t.Parallel()
	for name, view := range map[string]statusView{"busy": busyStatusView(), "ready": func() statusView {
		v := busyStatusView()
		v.State, v.problem, v.Next = "Ready", "", "Keep working. Run agent-archive list to inspect archived sessions."
		return v
	}()} {
		colored := renderStatus(view, true)
		color := regexp.MustCompile("\x1b\\[3[0-9]m")
		for line := range strings.SplitSeq(colored, "\n") {
			if n := len(color.FindAllString(line, -1)); n > 1 {
				t.Errorf("%s: %d colored elements on %q", name, n, line)
			}
		}
		// Without color the screen says the same thing.
		plain := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(colored, "")
		if plain != renderStatus(view, false) {
			t.Errorf("%s: the colored screen differs from the plain one beyond its colors:\n%s", name, plain)
		}
	}
}

// The state line is green only when all is well, and yellow when status
// needs the user.
func TestStatusStateLineColor(t *testing.T) {
	t.Parallel()
	sc := statusScreen{style: textStyle{color: true}}
	for state, code := range map[string]string{"Ready": "32", "Needs attention": "33", "Waiting for capture": "33", "Paused": "33", "Not set up": "33", "Not installed": "2"} {
		if got, want := sc.stateLabel(state), "\x1b["+code+"m● "+state+"\x1b[0m"; got != want {
			t.Errorf("%s: %q want %q", state, got, want)
		}
	}
}

// A next step shows home paths as ~, points at the warnings below it, and
// highlights only its first command.
func TestStatusNextStepProse(t *testing.T) {
	t.Parallel()
	sc := statusScreen{style: textStyle{color: true}, home: "/Users/alex"}
	got := sc.prose("Another installation's hooks are in Codex's hook file (see the warning above) at /Users/alex/.codex and not /Users/alexandra. Run agent-archive setup --yes, then agent-archive sync.")
	want := "Another installation's hooks are in Codex's hook file (see the warning below) at ~/.codex and not /Users/alexandra. Run \x1b[36magent-archive setup --yes\x1b[0m, then agent-archive sync."
	if got != want {
		t.Fatalf("prose:\n%q\nwant\n%q", got, want)
	}
	if got := sc.prose("agent-archive is no longer usable at /opt/bin/agent-archive."); strings.Contains(got, "\x1b") {
		t.Fatalf("highlighted something that is not a command: %q", got)
	}
}

func TestStatusStorageURL(t *testing.T) {
	t.Parallel()
	for label, want := range map[string]string{
		"s3 / team-archive / agent-archive/": "s3://team-archive/agent-archive/",
		"s3 / bucket":                        "s3://bucket",
		"r2 / bucket / a/b":                  "r2://bucket/a/b",
		"odd":                                "odd",
	} {
		if got := storageURL(label); got != want {
			t.Errorf("storageURL(%q) = %q want %q", label, got, want)
		}
	}
}

// A read-back failure says when the collector tries again, relative to now.
func TestStatusReadBackFailureRetry(t *testing.T) {
	t.Parallel()
	sc := statusScreen{now: renderNow}
	for _, tc := range []struct {
		failure verificationEvidence
		want    string
	}{
		{verificationEvidence{Outcome: verificationOutcomeFailed, Attempts: 1, LastError: "timeout", NextRetryAt: renderNow.Add(30 * time.Second)}, "Read-back failed: timeout (retrying in under a minute, 1 attempt so far)"},
		{verificationEvidence{Outcome: verificationOutcomeMismatch, Attempts: 3, NextRetryAt: renderNow.Add(-time.Minute)}, "Read-back doesn't match what this machine uploaded (retrying on the next pass, 3 attempts so far)"},
		{verificationEvidence{Outcome: verificationOutcomeFailed, Attempts: 4, NextRetryAt: renderNow.Add(6 * time.Hour)}, "Read-back failed (retrying in 6 hours, 4 attempts so far)"},
	} {
		if got := sc.readBackFailure(tc.failure); got != tc.want {
			t.Errorf("got %q want %q", got, tc.want)
		}
	}
}

// Long rows wrap under their own text on a narrow terminal.
func TestStatusWrapsToTheTerminal(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	printStatus(&out, busyStatusView(), statusScreen{style: textStyle{width: 60}, now: renderNow, home: "/Users/alex"})
	for line := range strings.SplitSeq(out.String(), "\n") {
		if visibleWidth(line) > 60 && strings.Contains(strings.TrimSpace(line), " ") {
			t.Errorf("line wider than the terminal: %q", line)
		}
	}
	for _, want := range []string{
		"\n    Check storage access and run agent-archive sync.\n    To change credentials, run agent-archive setup and\n    choose storage.\n",
		"\n  ✓ s3://team-archive/agent-archive/\n    reachable · uploaded 50 minutes ago\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status does not wrap as %q:\n%s", want, out.String())
		}
	}
}

// A storage check newer than the recorded storage health, as setup's after
// new credentials, outranks the failure or staleness that health recorded.
func TestStatusDestinationNewerStorageCheckOutranksOlderHealth(t *testing.T) {
	t.Parallel()
	sc := statusScreen{now: renderNow}
	for _, health := range []string{"authentication_failed", "stale", "stale_configuration"} {
		view := statusView{
			Storage:                  "s3 / team-archive",
			Authentication:           storageHealth{State: health, CheckedAt: renderNow.Add(-30 * time.Minute)},
			StorageAccessConfirmedAt: renderNow.Add(-time.Minute),
			StorageAccessConfirmedBy: storageAccessConfirmedBySetup,
		}
		if row := sc.destinationRow(view); row.mark != "✓" || row.detail != "reachable, checked by setup 1 minute ago" {
			t.Errorf("%s then a newer setup check: %q %q", health, row.mark, row.detail)
		}
		// The same health after the last storage check still shows.
		view.StorageAccessConfirmedAt = renderNow.Add(-time.Hour)
		if row := sc.destinationRow(view); row.mark == "✓" {
			t.Errorf("%s after the last storage check shows as reachable: %q", health, row.detail)
		}
	}
}

// A background collector launchctl could not report on is not called
// stopped, though the next step is the same.
func TestStatusBackgroundUnknownIsNotCalledStopped(t *testing.T) {
	t.Parallel()
	view := statusView{Background: "unknown", Apps: []appStatus{{Name: "codex", Hooks: "installed"}}}
	chooseInstallationStep(&view, statusBackground{ref: "x"})
	if view.problem != "The background collector couldn't be checked" || view.Next != "Run agent-archive setup to restore the background collector." {
		t.Fatalf("problem %q next %q", view.problem, view.Next)
	}
}

// Only paths that start at the home folder are shown with ~.
func TestStatusTildeOnlyPathsStartingAtHome(t *testing.T) {
	t.Parallel()
	sc := statusScreen{home: "/Users/jo"}
	for text, want := range map[string]string{
		"at /Users/jo/src and (/Users/jo/.codex)": "at ~/src and (~/.codex)",
		"/Users/jo/a":                   "~/a",
		"/Volumes/Backup/Users/jo/proj": "/Volumes/Backup/Users/jo/proj",
		"/Users/joe/a and x/Users/jo/b": "/Users/joe/a and x/Users/jo/b",
	} {
		if got := sc.tilde(text); got != want {
			t.Errorf("tilde(%q) = %q want %q", text, got, want)
		}
	}
	root := statusScreen{home: "/"}
	if got := root.tilde("runs /opt/bin/agent-archive"); got != "runs /opt/bin/agent-archive" {
		t.Errorf("with HOME=/: %q", got)
	}
}

// Privacy evidence that was checked is not called unchecked, and evidence
// dated after now is not called over a day old.
func TestStatusPrivacyRowWording(t *testing.T) {
	t.Parallel()
	sc := statusScreen{now: renderNow, verbose: true}
	past, future := renderNow.Add(-5*time.Minute), renderNow.Add(time.Hour)
	row := sc.privacyRow(storage.PrivacyReport{State: "not_verified", Reason: "public_access_controls_not_fully_verified", CheckedAt: &past})
	if row.cells[0] != "Bucket privacy not verified" || row.detail != "some public access settings couldn't be read, checked 5 minutes ago" {
		t.Errorf("partly read: %q %q", row.cells[0], row.detail)
	}
	row = sc.privacyRow(storage.PrivacyReport{State: "not_verified", Reason: "inspection_stale", CheckedAt: &future})
	if strings.Contains(row.detail, "over a day old") || strings.Contains(row.detail, "just now") {
		t.Errorf("future-dated check: %q", row.detail)
	}
}

// A new project of an app already captured elsewhere is not called the
// app's first session.
func TestStatusWaitingProjectOfCapturedApp(t *testing.T) {
	t.Parallel()
	view := statusView{Apps: []appStatus{{Name: "codex", VerifiedSessions: 3, Projects: []projectCaptureStatus{
		{ProjectRoot: "/Users/alex/a", ReadBackVerified: true},
		{ProjectRoot: "/Users/alex/b"},
	}}}}
	chooseCaptureStep(&view)
	if view.problem != "Waiting for a Codex session in /Users/alex/b" {
		t.Fatalf("problem %q", view.problem)
	}
	var out strings.Builder
	statusScreen{home: "/Users/alex"}.printNextStep(&out, view)
	if !strings.Contains(out.String(), "! Waiting for a Codex session in ~/b\n") {
		t.Fatalf("screen:\n%s", out.String())
	}
}

// status --verbose keeps everything the text status printed before it was
// made compact: each app's progress and projects, the included projects,
// skill evidence, imports, when access was checked, why privacy couldn't be
// verified, and the pending count; then codes, exact times, full paths, raw
// errors and evidence, in a Details section.
func TestStatusVerboseKeepsTodaysDetail(t *testing.T) {
	t.Parallel()
	view := busyStatusView()
	view.Code = statusCode(view.State)
	for i := range view.Apps {
		view.Apps[i].Code = statusCode(view.Apps[i].State)
	}
	text := renderVerboseStatus(view)
	for _, want := range []string{
		"    · 2 sessions archived, verified 26 hours ago\n",
		"    · uploaded, read-back pending (1 of 2 projects verified)\n",
		"    ✓ ~/src/web-app: verified\n    · ~/src/api: uploaded, read-back pending\n",
		"    · 1 session with a capture gap (2 gaps recorded; details in status --json)\n",
		"  · Projects: ~/src/web-app, ~/src/api\n",
		"  · Skill evidence:\n",
		"  · Imported (all destinations): 4 sessions, 1 waiting to upload; last import import-7\n",
		"reachable, checked 2 minutes ago · uploaded 50 minutes ago\n",
		"the last check is over a day old, checked 3 hours ago\n",
		"    Check public access: https://example.com/guide\n",
		"  · Last upload: 50 minutes ago · 3 pending\n",
		"\nDetails\n",
		"  State:         Needs attention (needs_attention)\n",
		"  Storage:       s3 / team-archive / agent-archive/\n",
		"  Access:        confirmed 2026-09-25T11:58:00Z by the collector's last successful storage access\n",
		"  Bucket privacy not verified.\n    Checked: 2026-09-25T09:00:00Z; inspection_stale.\n    Review: https://example.com/guide\n",
		"  Authentication: verified (checked 2026-09-25T11:58:00Z; manual_sync)\n",
		"  Background:    loaded\n",
		"  Projects:      2 included\n  Pending:       3 session(s)\n  Last scan:     2026-09-25T11:59:00Z\n  Last publish:  2026-09-25T11:10:00Z\n",
		"  Imported:      4 session(s), 1 waiting to upload; last import import-7\n",
		"  Codex: published; source verified (published_source_verified; 2 session(s); 1 with a capture gap); hooks installed\n",
		"    Hook trust: unknown here;",
		"    Installed version: 0.121.0; support unverified (verified sessions came from a different version).\n",
		"    Read-back verified: 2026-09-24T10:00:00Z; evidence is for that publication.\n",
		"    Capture gaps: 2 recorded across 1 session(s); see status --json for details.\n",
		"    Project /Users/alex/src/api: incomplete.\n",
		"    Last read-back: 2026-09-25T11:00:00Z (1 of 2 projects verified).\n",
		"    Read-back: failed: not found (attempt 2; next retry 2026-09-25T12:05:00Z)\n",
		"  Cursor: waiting for first session (awaiting_session; 0 session(s)); hooks missing or incomplete\n",
		"  Capture skipped in /Users/alex/src/api (Claude Code): setup was still in progress, so the session was not registered; start a new session at 2026-09-25T11:20:00Z.\n",
		"  Last error:    list registrations: AccessDenied: Access Denied\n",
		"  Quarantined:   2 local state file(s)",
		"  Summaries:     1 session summary(ies)",
		"  Warning:       Installed versions could not be read from /Users/alex/.agent-archive/application-versions.json",
		"\nAs JSON: agent-archive status --json\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status --verbose is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(renderStatus(view, false), "\nDetails\n") {
		t.Error("status without --verbose has the Details section")
	}
}

// The Storage section names a storage refusal the collector recorded by its
// cause, from the provider's error code, and shows anything else as recorded.
func TestStatusLastErrorPlainCause(t *testing.T) {
	t.Parallel()
	for recorded, want := range map[string]string{
		"list registrations: AccessDenied: Access Denied": "Storage refused access",
		"list registrations: operation error S3: ListObjectsV2, https response error StatusCode: 403, RequestID: R, HostID: H, api error AccessDenied: Access Denied": "Storage refused access",
		"publish: operation error S3: PutObject, api error NoSuchBucket: The specified bucket does not exist":                                                         "The bucket doesn't exist",
		"list registrations: operation error S3: ListObjectsV2, api error InvalidAccessKeyId: The key does not exist":                                                 "Storage didn't accept the credentials",
		"list registrations: operation error S3: ListObjectsV2, api error PermanentRedirect: use the right endpoint":                                                  "The bucket is in a different region",
		"2 session(s) failed to scan or publish": "Last error: 2 session(s) failed to scan or publish",
		"list registrations: operation error S3: ListObjectsV2, api error SlowDown: Please reduce your request rate": "Last error: list registrations: operation error S3: ListObjectsV2, api error SlowDown: Please reduce your request rate",
		"the note says AccessDenied: is expected here":                                                               "Last error: the note says AccessDenied: is expected here",
		// A refusal by the role or sign-in service is a credential problem,
		// not the bucket's, as storage.Diagnose has it: shown as recorded.
		"list registrations: operation error S3: ListObjectsV2, get identity: get credentials: failed to refresh cached credentials, operation error STS: AssumeRole, https response error StatusCode: 403, RequestID: R, api error AccessDenied: User is not authorized to perform: sts:AssumeRole": "Last error: list registrations: operation error S3: ListObjectsV2, get identity: get credentials: failed to refresh cached credentials, operation error STS: AssumeRole, https response error StatusCode: 403, RequestID: R, api error AccessDenied: User is not authorized to perform: sts:AssumeRole",
	} {
		var status state.Status
		status.SetLastErrors(recorded)
		if got := lastErrorRows(status); !slices.Equal(got, []string{want}) {
			t.Errorf("%q: %q want %q", recorded, got, want)
		}
	}
}

// A recorded problem whose own text contains "; ", as a storage provider's
// message can, is one row, and none of its text is read as a problem of its
// own.
func TestStatusLastErrorWithSeparatorIsOneRow(t *testing.T) {
	t.Parallel()
	recorded := "list registrations: operation error S3: ListObjectsV2, api error SlowDown: Please reduce your request rate; AccessDenied: may follow if you do not"
	var status state.Status
	status.SetLastErrors(recorded)
	want := []string{"Last error: " + recorded}
	if got := lastErrorRows(status); !slices.Equal(got, want) {
		t.Fatalf("rows %q want %q", got, want)
	}
	view := busyStatusView()
	view.Collector.SetLastErrors(recorded)
	if got := failedStorageRows(view); !slices.Equal(got, want) {
		t.Fatalf("Storage section's ✗ rows %q want %q", got, want)
	}
}

// Each problem the last pass recorded is its own row, with its own cause.
func TestStatusLastErrorsEachGetARow(t *testing.T) {
	t.Parallel()
	var status state.Status
	status.SetLastErrors(
		"2 session(s) failed to scan or publish",
		"list registrations: api error AccessDenied: Access Denied",
		"publish: operation error S3: PutObject, api error NoSuchBucket: gone; see the console",
	)
	want := []string{
		"Last error: 2 session(s) failed to scan or publish",
		"Storage refused access",
		"The bucket doesn't exist",
	}
	if got := lastErrorRows(status); !slices.Equal(got, want) {
		t.Fatalf("rows %q want %q", got, want)
	}
	view := busyStatusView()
	view.Collector.SetLastErrors(status.LastErrors...)
	if got := failedStorageRows(view); !slices.Equal(got, want) {
		t.Fatalf("Storage section's ✗ rows %q want %q", got, want)
	}
}

// failedStorageRows is the text of each ✗ row in view's Storage section.
func failedStorageRows(view statusView) []string {
	sc := statusScreen{style: textStyle{}, now: renderNow, home: "/Users/alex"}
	var failed []string
	for _, row := range sc.storageRows(view) {
		if row.mark == sc.style.failMark() {
			failed = append(failed, strings.Join(row.cells, " "))
		}
	}
	return failed
}

// A status file written before the problems were recorded as a list has only
// the joined text, which is split where the collector joined it and shown on
// one line, as it always was.
func TestStatusLastErrorFromOlderStatusFile(t *testing.T) {
	t.Parallel()
	for recorded, want := range map[string][]string{
		"list registrations: AccessDenied: Access Denied":                                                   {"Storage refused access"},
		"2 session(s) failed to scan or publish":                                                            {"Last error: 2 session(s) failed to scan or publish"},
		"2 session(s) failed to scan or publish; list registrations: api error AccessDenied: Access Denied": {"Last error: 2 session(s) failed to scan or publish; Storage refused access"},
	} {
		var status state.Status
		if err := json.Unmarshal([]byte(`{"pending_count":0,"last_error":`+strconv.Quote(recorded)+`}`), &status); err != nil {
			t.Fatal(err)
		}
		if got := lastErrorRows(status); !slices.Equal(got, want) {
			t.Errorf("%q: %q want %q", recorded, got, want)
		}
	}
}

// A next step puts each sentence that names a command on its own line, with
// that command highlighted; a sentence without one stays with the line
// before it.
func TestStatusNextStepOneCommandPerLine(t *testing.T) {
	t.Parallel()
	sc := statusScreen{style: textStyle{color: true}}
	for next, want := range map[string][]string{
		"Check storage access and run agent-archive sync. To change credentials, run agent-archive setup and choose storage.": {
			"Check storage access and run \x1b[36magent-archive sync\x1b[0m.",
			"To change credentials, run \x1b[36magent-archive setup\x1b[0m and choose storage.",
		},
		"Keep working. Run agent-archive list to inspect archived sessions.": {
			"Keep working. Run \x1b[36magent-archive list\x1b[0m to inspect archived sessions.",
		},
		"It runs elsewhere. Check with agent-archive sync, then run agent-archive setup again from a shell where it works.": {
			"It runs elsewhere. Check with \x1b[36magent-archive sync\x1b[0m,",
			"then run \x1b[36magent-archive setup\x1b[0m again from a shell where it works.",
		},
		"Run agent-archive setup to recover the interrupted installation. If setup reports a file changed outside setup, agent-archive setup --abandon-recovery keeps your files as they are now.": {
			"Run \x1b[36magent-archive setup\x1b[0m to recover the interrupted installation.",
			"If setup reports a file changed outside setup, \x1b[36magent-archive setup --abandon-recovery\x1b[0m keeps your files as they are now.",
		},
		"Run agent-archive resume when ready. Registered sessions can catch up after resume.": {
			"Run \x1b[36magent-archive resume\x1b[0m when ready. Registered sessions can catch up after resume.",
		},
	} {
		if got := sc.proseLines(next); !slices.Equal(got, want) {
			t.Errorf("%q:\n%q\nwant\n%q", next, got, want)
		}
	}
}

// status --verbose prints each problem the last pass recorded on its own
// line, as recorded, so one containing "; " reads as one.
func TestStatusVerbosePrintsEachLastErrorOnItsOwnLine(t *testing.T) {
	t.Parallel()
	view := busyStatusView()
	view.Collector.SetLastErrors("2 session(s) failed to scan or publish", "list registrations: api error SlowDown: slow down; then retry")
	text := renderVerboseStatus(view)
	for _, want := range []string{
		"\n  Last error:    2 session(s) failed to scan or publish\n",
		"\n  Last error:    list registrations: api error SlowDown: slow down; then retry\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status --verbose is missing %q:\n%s", want, text)
		}
	}
}

// status --verbose prints an older status file's joined last error once, as
// recorded, since where its problems end is not known.
func TestStatusVerbosePrintsAnOlderLastErrorAsRecorded(t *testing.T) {
	t.Parallel()
	view := busyStatusView()
	view.Collector.LastErrors = nil
	view.Collector.LastError = "a; b"
	if text := renderVerboseStatus(view); strings.Count(text, "  Last error:    ") != 1 || !strings.Contains(text, "\n  Last error:    a; b\n") {
		t.Fatalf("status --verbose:\n%s", text)
	}
}

// A diagnostic for part of a session left out says so, rather than that the
// session was skipped.
func TestStatusSaysAnOffChainRevisionWasLeftOutNotTheSession(t *testing.T) {
	t.Parallel()
	view := busyStatusView()
	view.CaptureDiagnostics = []capture.Diagnostic{{Code: capture.DiagnosticRevisionLeftOut, ProjectRoot: "/Users/alex/src/api", Harness: "codex", ObservedAt: renderNow.Add(-40 * time.Minute)}}
	text, verbose := renderStatus(view, false), renderVerboseStatus(view)
	if want := "Codex left out part of a session in ~/src/api 40 minutes ago: an abandoned branch of the thread"; !strings.Contains(text, want) {
		t.Errorf("status is missing %q:\n%s", want, text)
	}
	if want := "  Capture left out part of a session in /Users/alex/src/api (Codex): an abandoned branch"; !strings.Contains(verbose, want) {
		t.Errorf("status --verbose is missing %q:\n%s", want, verbose)
	}
	if strings.Contains(text, "skipped a session") || strings.Contains(verbose, "Capture skipped") {
		t.Errorf("status says the session was skipped:\n%s\n%s", text, verbose)
	}
}
