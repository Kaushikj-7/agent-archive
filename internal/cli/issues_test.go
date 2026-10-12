package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
	"github.com/aws/smithy-go"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// A session's failure is classified by what its error wraps, however deeply,
// never by its text, and the code never carries the text.
func TestSessionIssueCodeClassifiesWrappedErrors(t *testing.T) {
	t.Parallel()
	denied := &smithy.OperationError{ServiceID: "S3", OperationName: "PutObject", Err: &smithy.GenericAPIError{Code: "AccessDenied", Message: "no"}}
	expired := &smithy.GenericAPIError{Code: "ExpiredToken"}
	process := fmt.Errorf("operation error S3: PutObject, failed to retrieve credentials: %w", &processcreds.ProviderError{Err: errors.New("printed-secret")})
	outage := &smithy.OperationError{ServiceID: "S3", OperationName: "PutObject", Err: errors.New("dial tcp: timeout")}
	retentionOf := func(err error) error { return fmt.Errorf("%w: %w", errRetentionFailed, err) }
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"unrecognized", errors.New("secret output"), issueCaptureFailed},
		{"text alone names no size limit", errors.New("transcript exceeds collection limit"), issueCaptureFailed},
		{"text alone names no rewrite", errors.New("transcript was truncated, compacted, or rewritten"), issueCaptureFailed},
		{"text alone names no retention", errors.New("retention: failed"), issueCaptureFailed},
		{"quarantined", fmt.Errorf("load registration: %w", state.ErrQuarantined), issueLocalStateUnreadable},
		{"quarantined beside another error", errors.Join(errors.New("other"), fmt.Errorf("x: %w", state.ErrQuarantined)), issueLocalStateUnreadable},
		{"access denied", fmt.Errorf("publish source: %w", denied), issueStorageAuth},
		{"expired token", fmt.Errorf("publish: %w", expired), issueStorageAuth},
		{"credential_process failed", process, issueStorageAuth},
		{"outage", fmt.Errorf("publish source: %w", outage), issueStorageUnavailable},
		{"object missing", fmt.Errorf("read back: %w", storage.ErrNotFound), issueStorageUnavailable},
		{"checksum mismatch", fmt.Errorf("read back: %w", storage.ErrChecksumMismatch), issueStorageUnavailable},
		{"quarantine beside refused credentials", errors.Join(fmt.Errorf("publish: %w", denied), fmt.Errorf("x: %w", state.ErrQuarantined)), issueLocalStateUnreadable},
		{"subagent not captured", fmt.Errorf("candidate: %w", collector.ErrSubagentNotCaptured), issueSubagentNotCaptured},
		{"retention", retentionOf(errors.New("simulated delete failure")), issueRetentionFailed},
		{"retention during an outage", retentionOf(outage), issueRetentionFailed},
		{"retention refused credentials", retentionOf(denied), issueStorageAuth},
		{"retention beside a capture failure", errors.Join(errors.New("filter"), retentionOf(errors.New("delete"))), issueRetentionFailed},
	} {
		if got := sessionIssueCode(tc.err); got != tc.want {
			t.Errorf("%s: code = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Every declared code has a label that ends in a next step or says there
// is nothing to do, and every label belongs to a declared code.
func TestEveryIssueCodeHasALabel(t *testing.T) {
	t.Parallel()
	for _, code := range issueCodes {
		label, ok := issueLabels[code]
		if !ok {
			t.Errorf("issue code %q has no label", code)
			continue
		}
		if label.what == "" || label.next == "" {
			t.Errorf("issue code %q: label %+v is incomplete", code, label)
		}
		if !strings.HasPrefix(label.next, "nothing to do") && !strings.Contains(label.next, "agent-archive ") {
			t.Errorf("issue code %q: next step %q names no command and does not say there is nothing to do", code, label.next)
		}
		// A kind that is not about storage and needs something done leads
		// status with a headline of its own; the others never do.
		storageKind := storageIssues[code]
		wantHeadline := !storageKind && !strings.HasPrefix(label.next, "nothing to do")
		if (label.headline != "") != wantHeadline {
			t.Errorf("issue code %q: headline %q, want one: %v", code, label.headline, wantHeadline)
		}
	}
	for code := range issueLabels {
		if !slices.Contains(issueCodes, code) {
			t.Errorf("label for %q, which issueCodes does not declare", code)
		}
	}
	if len(issueCodes) != len(issueLabels) {
		t.Errorf("%d codes, %d labels", len(issueCodes), len(issueLabels))
	}
}

// The summary names each kind of failure with its count and next step,
// sessions and subagents apart, in a fixed order.
func TestIssueSummaryWordsEachKind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		tallies map[string]issueTally
		want    string
	}{
		{"nothing", nil, ""},
		{"one session", map[string]issueTally{issueCaptureFailed: {sessions: 1}}, "1 session failed to capture or upload — run agent-archive sync for details"},
		{"subagent not captured", map[string]issueTally{issueSubagentNotCaptured: {subagents: 1}}, "1 subagent could not be captured (transcript unreadable, too large, or not matching its parent) — nothing to do, its parent session records the link as unavailable"},
		{"subagents only", map[string]issueTally{issueTranscriptSizeLimit: {subagents: 2}}, "2 subagents stopped being captured at the transcript size limit — nothing to do, the last snapshot is kept"},
		{"sessions and a subagent", map[string]issueTally{issueStorageUnavailable: {sessions: 2, subagents: 1}}, "2 sessions and 1 subagent failed to reach storage (network or service unavailable) — check the network and the storage service, then run agent-archive sync (the next pass also retries)"},
		{"mixed", map[string]issueTally{
			issueRetentionFailed: {sessions: 1},
			issueStorageAuth:     {sessions: 3},
			issueCaptureFailed:   {},
		}, "3 sessions failed to reach storage (credentials were refused or unavailable) — check the credentials with agent-archive setup (choose storage), then run agent-archive sync" +
			" · 1 session couldn't be removed after the retention period — the next pass retries, or run agent-archive sync for details"},
	} {
		if got := issueSummary(tc.tallies); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// No summary reads, to status, as a Keychain failure, a storage refusal, or
// a credential_process failure: those hints come only from the errors that
// report them.
func TestIssueSummaryTriggersNoCredentialHint(t *testing.T) {
	t.Parallel()
	for _, code := range issueCodes {
		summary := issueSummary(map[string]issueTally{code: {sessions: 2, subagents: 1}})
		if action := credentials.RecoveryActionForMessage(summary); action != "" {
			t.Errorf("%s: summary reads as a Keychain failure: %q", code, action)
		}
		if cause := storageErrorCause(summary); cause != "" {
			t.Errorf("%s: summary reads as storage refusing: %q", code, cause)
		}
		if strings.Contains(summary, backgroundCredentialProcessFailure) || strings.Contains(summary, "; ") {
			t.Errorf("%s: summary %q", code, summary)
		}
	}
}

// A pass whose collection and retention both fail records one last error
// covering both, and counts each code, rather than retention replacing the
// collection failures with wording of its own.
func TestRetentionAndCaptureFailuresShareOneLastError(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	// A subagent's registration, as the collector saves it.
	if err := os.MkdirAll(filepath.Join(home, "registrations"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "registrations", "child.json"), []byte(`{"archive_session_id":"child","parent_session_id":"parent"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result := collector.Result{Errors: map[string]error{
		"a":     errors.New("filter transcript: unsafe"),
		"child": errors.New("filter transcript: unsafe"),
		"b":     fmt.Errorf("x: %w", state.ErrQuarantined),
	}}
	summary, err := recordSessionIssues(store, result.Errors, subagentLookup(store), "")
	if err != nil {
		t.Fatal(err)
	}
	recordRetentionErrors(store, &result, retention.Result{Errors: map[string]error{
		"old": errors.New("simulated delete failure"),
		"a":   errors.New("simulated delete failure"),
	}}, summary)
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	want := "1 session had local state that could not be read and was moved aside — see quarantined_files in agent-archive status --json, then delete those files" +
		" · 1 subagent failed to capture or upload — run agent-archive sync for details" +
		" · 2 sessions couldn't be removed after the retention period — the next pass retries, or run agent-archive sync for details"
	if len(status.LastErrors) != 1 || status.LastError != want {
		t.Fatalf("last errors %q\nwant %q", status.LastErrors, want)
	}
	wantIssues := map[string]string{"a": issueRetentionFailed, "b": issueLocalStateUnreadable, "child": issueCaptureFailed, "old": issueRetentionFailed}
	wantCounts := map[string]int{issueRetentionFailed: 2, issueLocalStateUnreadable: 1, issueCaptureFailed: 1}
	if !mapsEqual(status.SessionIssues, wantIssues) || !mapsEqual(status.IssueCounts, wantCounts) {
		t.Fatalf("issues %v counts %v", status.SessionIssues, status.IssueCounts)
	}
	// Both of a's failures stay in what sync reports.
	if text := result.Errors["a"].Error(); !strings.Contains(text, "unsafe") || !strings.Contains(text, "retention: simulated") {
		t.Fatalf("a's errors: %q", text)
	}
}

// Each pass's session issues and counts replace the last pass's, not merge
// with them.
func TestRecordSessionIssuesReplacesThePreviousPass(t *testing.T) {
	t.Parallel()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	notSubagent := func(string) bool { return false }
	if _, err := recordSessionIssues(store, map[string]error{"earlier": fmt.Errorf("x: %w", state.ErrQuarantined)}, notSubagent, ""); err != nil {
		t.Fatal(err)
	}
	// The next pass: collector.Run saves its own count, as it would.
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	status.SetLastErrors(collector.FailedSessionsProblem(1))
	if err := store.SaveStatus(status); err != nil {
		t.Fatal(err)
	}
	if _, err := recordSessionIssues(store, map[string]error{"now": errors.New("x")}, notSubagent, collector.FailedSessionsProblem(1)); err != nil {
		t.Fatal(err)
	}
	if status, err = store.LoadStatus(); err != nil {
		t.Fatal(err)
	}
	if !mapsEqual(status.SessionIssues, map[string]string{"now": issueCaptureFailed}) || !mapsEqual(status.IssueCounts, map[string]int{issueCaptureFailed: 1}) {
		t.Fatalf("issues %v counts %v", status.SessionIssues, status.IssueCounts)
	}
	if want := "1 session failed to capture or upload — run agent-archive sync for details"; len(status.LastErrors) != 1 || status.LastError != want {
		t.Fatalf("last errors %q", status.LastErrors)
	}
}

// The summary retention replaces is the one collection recorded, even when
// recomputing it now would read differently: here the subagent's
// registration went during the sweep, so it would count as a session.
func TestRetentionReplacesTheRecordedSummary(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	registration := filepath.Join(home, "registrations", "child.json")
	if err := os.MkdirAll(filepath.Dir(registration), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registration, []byte(`{"archive_session_id":"child","parent_session_id":"parent"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result := collector.Result{Errors: map[string]error{"child": errors.New("filter transcript: unsafe")}}
	summary, err := recordSessionIssues(store, result.Errors, subagentLookup(store), "")
	if err != nil || !strings.HasPrefix(summary, "1 subagent ") {
		t.Fatalf("summary %q err %v", summary, err)
	}
	if err := os.Remove(registration); err != nil {
		t.Fatal(err)
	}
	recordRetentionErrors(store, &result, retention.Result{Errors: map[string]error{"old": errors.New("delete failed")}}, summary)
	if got := lastErrors(t, store); len(got) != 1 {
		t.Fatalf("LastErrors = %q, want one summary", got)
	}
}

// Problems with nothing to do are shown as information, not as failures:
// no ✗ and no "Last error".
func TestNothingToDoProblemsAreNotShownAsFailures(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 21, 24, 0, 0, time.UTC)
	env, home, _, _ := publishedThroughSync(t, now)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	subagent := issueSummary(map[string]issueTally{issueSubagentNotCaptured: {subagents: 1}})
	for _, tc := range []struct {
		name     string
		counts   map[string]int
		problems []string
		wantRow  string
	}{
		{"subagent not captured", map[string]int{issueSubagentNotCaptured: 1}, []string{subagent}, "· Last pass: " + subagent + "\n"},
		{"size limit", nil, []string{collector.SizeLimitProblem(1)}, "· Last pass: " + collector.SizeLimitProblem(1) + "\n"},
		{"both", map[string]int{issueSubagentNotCaptured: 1}, []string{subagent, collector.SizeLimitProblem(1)}, "· Last pass: " + collector.SizeLimitProblem(1) + "\n"},
	} {
		status, err := store.LoadStatus()
		if err != nil {
			t.Fatal(err)
		}
		status.IssueCounts = tc.counts
		status.SetLastErrors(tc.problems...)
		if err := store.SaveStatus(status); err != nil {
			t.Fatal(err)
		}
		plain := statusOutput(t, env)
		if !strings.Contains(plain, tc.wantRow) || strings.Contains(plain, "✗") || strings.Contains(plain, "Last error") {
			t.Errorf("%s: status:\n%s", tc.name, plain)
		}
	}
	// A failure beside them is still one.
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	status.IssueCounts = map[string]int{issueCaptureFailed: 1}
	status.SetLastErrors(issueSummary(map[string]issueTally{issueCaptureFailed: {sessions: 1}}), collector.SizeLimitProblem(1))
	if err := store.SaveStatus(status); err != nil {
		t.Fatal(err)
	}
	// The headline states the failure, so default status doesn't repeat
	// it; the size-limit notice beside it is still shown, and --verbose
	// shows both.
	plain := statusOutput(t, env)
	if !strings.Contains(plain, "! Some sessions could not be captured\n") || strings.Contains(plain, "Last error: 1 session failed to capture") || !strings.Contains(plain, "stopped being captured at a size limit") {
		t.Errorf("status with a failure:\n%s", plain)
	}
	if verbose := statusOutput(t, env, "--verbose"); !strings.Contains(verbose, "✗ Last error: 1 session failed to capture") {
		t.Errorf("status --verbose with a failure:\n%s", verbose)
	}
}

func mapsEqual[V comparable](got, want map[string]V) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// status shows the last pass's mixed failures as recorded, each session's
// gap with the next step for its kind, and the counts in status --json. A
// status file from before capture_failed, which wrote
// capture_or_publication_failed, still reads as that failure.
func TestStatusShowsMixedSessionIssues(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 21, 24, 0, 0, time.UTC)
	env, home, _, _ := publishedThroughSync(t, now)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations %v %v", regs, err)
	}
	id := regs[0].ArchiveSessionID
	for _, tc := range []struct {
		name       string
		code       string
		wantDetail string
	}{
		{"legacy fallback", "capture_or_publication_failed", "Last scan could not update this session; retained evidence was kept. Run agent-archive sync for details."},
		{"storage auth", issueStorageAuth, "Last scan could not update this session; retained evidence was kept. Check the credentials with agent-archive setup (choose storage), then run agent-archive sync."},
	} {
		sessionErrs := map[string]error{id: errors.New("other"), "gone": fmt.Errorf("%w: x", errRetentionFailed)}
		if _, err := recordSessionIssues(store, sessionErrs, func(string) bool { return false }, ""); err != nil {
			t.Fatal(err)
		}
		status, err := store.LoadStatus()
		if err != nil {
			t.Fatal(err)
		}
		status.SessionIssues[id] = tc.code
		if err := store.SaveStatus(status); err != nil {
			t.Fatal(err)
		}
		view, err := readStatus(env)
		if err != nil {
			t.Fatal(err)
		}
		gaps := view.Apps[0].CaptureGaps
		if len(gaps) != 1 || gaps[0].Code != tc.code || gaps[0].Detail != tc.wantDetail {
			t.Errorf("%s: gaps %+v", tc.name, gaps)
		}
		if view.State != "Needs attention" {
			t.Errorf("%s: state %q", tc.name, view.State)
		}
	}
	summary := "1 session failed to capture or upload — run agent-archive sync for details · 1 session couldn't be removed after the retention period — the next pass retries, or run agent-archive sync for details"
	if plain := statusOutput(t, env); !strings.Contains(plain, "Last error: "+summary+"\n") {
		t.Errorf("status:\n%s", plain)
	}
	if verbose := statusOutput(t, env, "--verbose"); !strings.Contains(verbose, "  Last error:    "+summary+"\n") {
		t.Errorf("status --verbose:\n%s", verbose)
	}
	asJSON := statusOutput(t, env, "--json")
	for _, want := range []string{`"issue_counts": {`, `"capture_failed": 1`, `"retention_failed": 1`} {
		if !strings.Contains(asJSON, want) {
			t.Errorf("status --json lacks %s:\n%s", want, asJSON)
		}
	}
}

// Status leads with the most pressing kind of failed session when none is
// about storage, and with no failure at all when every kind has nothing to
// do. Storage kinds, counts beside another problem, codes this version does
// not know, and a status file with no counts keep the general headline.
func TestStatusHeadlineFollowsIssueKind(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 21, 24, 0, 0, time.UTC)
	env, home, _, _ := publishedThroughSync(t, now)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if healthy.Collector.LastError != "" {
		t.Fatalf("test precondition: a clean pass, got %q", healthy.Collector.LastError)
	}
	const general = "The last sync failed"
	for _, tc := range []struct {
		name        string
		counts      map[string]int
		lastErrors  []string
		legacy      bool
		wantProblem string
		wantNext    string
	}{
		{name: "capture", counts: map[string]int{issueCaptureFailed: 2},
			wantProblem: "Some sessions could not be captured", wantNext: "Run agent-archive sync for details."},
		{name: "retention", counts: map[string]int{issueRetentionFailed: 1},
			wantProblem: "Some sessions couldn't be removed after the retention period", wantNext: "The next pass retries, or run agent-archive sync for details."},
		{name: "local state before capture", counts: map[string]int{issueCaptureFailed: 3, issueLocalStateUnreadable: 1},
			wantProblem: "Some local state could not be read", wantNext: "See quarantined_files in agent-archive status --json, then delete those files."},
		{name: "a subagent not captured beside retention", counts: map[string]int{issueSubagentNotCaptured: 1, issueRetentionFailed: 1},
			wantProblem: "Some sessions couldn't be removed after the retention period", wantNext: "The next pass retries, or run agent-archive sync for details."},
		{name: "only subagents not captured", counts: map[string]int{issueSubagentNotCaptured: 2},
			wantProblem: healthy.problem, wantNext: healthy.Next},
		{name: "only nothing to do", counts: map[string]int{issueSubagentNotCaptured: 1, issueTranscriptSizeLimit: 1},
			wantProblem: healthy.problem, wantNext: healthy.Next},
		{name: "storage and capture", counts: map[string]int{issueStorageUnavailable: 1, issueCaptureFailed: 1},
			wantProblem: general},
		{name: "storage auth", counts: map[string]int{issueStorageAuth: 1},
			wantProblem: general},
		{name: "unknown code", counts: map[string]int{"from_a_newer_version": 1},
			wantProblem: general},
		{name: "another problem beside the summary", counts: map[string]int{issueCaptureFailed: 1},
			lastErrors: []string{"summary", "retention: clock disagrees"}, wantProblem: general},
		{name: "legacy status without counts", lastErrors: []string{"1 session(s) need capture or publication"},
			legacy: true, wantProblem: general},
		{name: "size limit only", lastErrors: []string{collector.SizeLimitProblem(2)},
			wantProblem: healthy.problem, wantNext: healthy.Next},
		{name: "size limit and capture", counts: map[string]int{issueCaptureFailed: 1},
			lastErrors:  []string{issueSummary(map[string]issueTally{issueCaptureFailed: {sessions: 1}}), collector.SizeLimitProblem(1)},
			wantProblem: "Some sessions could not be captured", wantNext: "Run agent-archive sync for details."},
		{name: "size limit and storage", counts: map[string]int{issueStorageUnavailable: 1},
			lastErrors:  []string{issueSummary(map[string]issueTally{issueStorageUnavailable: {sessions: 1}}), collector.SizeLimitProblem(1)},
			wantProblem: general},
		{name: "size limit and another problem", lastErrors: []string{collector.SizeLimitProblem(1), "retention: clock disagrees"},
			wantProblem: general},
	} {
		status, err := store.LoadStatus()
		if err != nil {
			t.Fatal(err)
		}
		status.IssueCounts = tc.counts
		if tc.lastErrors != nil {
			status.SetLastErrors(tc.lastErrors...)
		} else {
			tallies := map[string]issueTally{}
			for code, n := range tc.counts {
				tallies[code] = issueTally{sessions: n}
			}
			status.SetLastErrors(issueSummary(tallies))
			if status.LastError == "" {
				status.SetLastErrors("from a newer version")
			}
		}
		if tc.legacy {
			// Written before LastErrors existed.
			status.LastErrors = nil
		}
		if err := store.SaveStatus(status); err != nil {
			t.Fatal(err)
		}
		view, err := readStatus(env)
		if err != nil {
			t.Fatal(err)
		}
		if view.problem != tc.wantProblem || (tc.wantProblem != general && view.Next != tc.wantNext) {
			t.Errorf("%s: problem %q next %q, want %q %q", tc.name, view.problem, view.Next, tc.wantProblem, tc.wantNext)
		}
		if tc.wantProblem == general && !strings.HasPrefix(view.Next, "Check storage access") {
			t.Errorf("%s: next %q", tc.name, view.Next)
		}
		if tc.wantProblem == healthy.problem && view.State != healthy.State {
			t.Errorf("%s: state %q, want %q as with no failure", tc.name, view.State, healthy.State)
		}
	}
}

// A failure recorded after the pass's summary (a Keychain that is locked,
// say) replaces the counts with it, so status gives that failure's own fix
// rather than a headline for sessions that failed before it.
func TestPreflightFailureReplacesIssueHeadline(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 21, 24, 0, 0, time.UTC)
	env, home, _, _ := publishedThroughSync(t, now)
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recordSessionIssues(store, map[string]error{"a": errors.New("filter")}, func(string) bool { return false }, ""); err != nil {
		t.Fatal(err)
	}
	recordPreflightError(store, fmt.Errorf("open storage: load R2 credentials: %w", credentials.ErrKeychainLocked))
	status, err := store.LoadStatus()
	if err != nil || len(status.IssueCounts) != 0 || status.SessionIssues["a"] != issueCaptureFailed {
		t.Fatalf("status %+v err %v", status, err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.problem != "The last sync failed" || !strings.Contains(view.Next, "Unlock the login Keychain") {
		t.Fatalf("problem %q next %q", view.problem, view.Next)
	}
}
