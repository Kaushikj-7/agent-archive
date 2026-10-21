package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/state"
)

// A failed session's reason is the fixed text of a sentinel its error wraps,
// never the text around it, which can name paths or hold transcript content.
func TestSessionIssueReasonIsOnlyFixedText(t *testing.T) {
	t.Parallel()
	capacity := state.ErrDurableStorageCapacity.Error()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"wrapped sentinel", fmt.Errorf("persist pending publication for /Users/someone/secret-project: %w", state.ErrDurableStorageCapacity), capacity},
		{"joined", errors.Join(errors.New("transcript says hunter2"), fmt.Errorf("x: %w", state.ErrDurableStorageCapacity)), capacity},
		{"operating system", fmt.Errorf("write snapshot: %w", &fs.PathError{Op: "write", Path: "/Users/someone/secret.jsonl", Err: syscall.ENOSPC}), syscall.ENOSPC.Error()},
		{"unrecognized", errors.New("filter transcript: hunter2 in /Users/someone"), ""},
		// As recordRetentionErrors joins them: the session is reported as
		// retention_failed, so its reason is the removal's.
		{"retention beside collection", errors.Join(fmt.Errorf("x: %w", state.ErrDurableStorageCapacity), fmt.Errorf("%w: %w", errRetentionFailed, &fs.PathError{Op: "remove", Path: "/Users/someone/x", Err: syscall.EACCES})), fs.ErrPermission.Error()},
	} {
		if got := sessionIssueReason(tc.err, sessionIssueCode(tc.err)); got != tc.want {
			t.Errorf("%s: reason %q, want %q", tc.name, got, tc.want)
		}
	}
}

// status.json records each recognized reason beside the issue code, and
// nothing of an error it does not recognize.
func TestRecordSessionIssuesRecordsOnlyRecognizedReasons(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	errs := map[string]error{
		"full":    fmt.Errorf("persist pending publication: %w", state.ErrDurableStorageCapacity),
		"unknown": errors.New("filter transcript: hunter2"),
	}
	if _, err := recordSessionIssues(store, errs, func(string) bool { return false }, ""); err != nil {
		t.Fatal(err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !mapsEqual(status.SessionIssueReasons, map[string]string{"full": state.ErrDurableStorageCapacity.Error()}) {
		t.Fatalf("reasons %v", status.SessionIssueReasons)
	}
	raw, err := os.ReadFile(filepath.Join(home, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "persist pending publication") {
		t.Fatalf("status.json holds error text:\n%s", raw)
	}
	// A pass where nothing failed drops the reasons with the codes.
	if _, err := recordSessionIssues(store, nil, func(string) bool { return false }, ""); err != nil {
		t.Fatal(err)
	}
	if status, err = store.LoadStatus(); err != nil || status.SessionIssueReasons != nil {
		t.Fatalf("reasons %v err %v", status.SessionIssueReasons, err)
	}
}

// status --issues shows sessions that failed alike as one line naming a
// few of them, however many there are; --verbose shows the same lines and
// --json every session's reason.
func TestStatusIssuesGroupsFailedSessionsByReason(t *testing.T) {
	t.Parallel()
	home, env, _, _ := overviewFixture(t)
	store, err := state.Open(home)
	must(t, err)
	status := state.Status{LastScanAt: env.now(), SessionIssues: map[string]string{}, SessionIssueReasons: map[string]string{}}
	for i := range 268 {
		id := fmt.Sprintf("synthetic-%03d", i)
		status.SessionIssues[id] = issueCaptureFailed
		status.SessionIssueReasons[id] = state.ErrDurableStorageCapacity.Error()
	}
	status.SessionIssues["synthetic-other"] = issueCaptureFailed
	must(t, store.SaveStatus(status))

	full := "268 sessions failed to capture or upload: " + state.ErrDurableStorageCapacity.Error() + " — for details run"
	other := "1 session failed to capture or upload: error not recognized — for details run"
	issues := statusOutput(t, env, "--issues")
	for _, want := range []string{full, "synthetic-000, synthetic-001, synthetic-002 and 265 more", other, "synthetic-other"} {
		if strings.Count(issues, want) != 1 {
			t.Errorf("status --issues shows %q %d times, want once:\n%s", want, strings.Count(issues, want), issues)
		}
	}
	// Each group's command is on its own line, right under the group.
	lines := strings.Split(issues, "\n")
	for _, group := range []string{full, other} {
		i := slices.IndexFunc(lines, func(l string) bool { return strings.HasSuffix(l, group) })
		if i < 0 || i+1 == len(lines) || strings.TrimSpace(lines[i+1]) != "agent-archive sync" {
			t.Errorf("status --issues lacks agent-archive sync on its own line after %q:\n%s", group, issues)
		}
	}
	if strings.Contains(issues, "synthetic-003") || strings.Contains(issues, issueGapLead) {
		t.Errorf("status --issues lists sessions one by one:\n%s", issues)
	}
	if strings.Index(issues, full) > strings.Index(issues, other) {
		t.Errorf("status --issues puts the larger group second:\n%s", issues)
	}
	if verbose := statusOutput(t, env, "--verbose"); !strings.Contains(verbose, full) {
		t.Errorf("status --verbose lacks the group:\n%s", verbose)
	}
	var fields struct {
		Collector struct {
			Reasons map[string]string `json:"session_issue_reasons"`
		} `json:"collector"`
	}
	must(t, json.Unmarshal([]byte(statusOutput(t, env, "--json")), &fields))
	if len(fields.Collector.Reasons) != 268 || fields.Collector.Reasons["synthetic-000"] != state.ErrDurableStorageCapacity.Error() {
		t.Errorf("status --json reasons: %d entries", len(fields.Collector.Reasons))
	}
}

// A group counts subagents apart from sessions, as the last error's summary
// does, even candidates the collector rejected before ever registering
// them; a kind with nothing to do is information, not a warning, and comes
// after the groups that need the user however many it holds.
func TestStatusIssueGroupsCountSubagentsAndKeepNoActionKindsQuiet(t *testing.T) {
	t.Parallel()
	home, env, _, _ := overviewFixture(t)
	store, err := state.Open(home)
	must(t, err)
	// As the collector reports a rejected candidate: no registration exists.
	rejected := errors.Join(fmt.Errorf("x: %w", collector.ErrSubagentNotCaptured), collector.ErrSubagentCandidate)
	summary, err := recordSessionIssues(store, map[string]error{
		"synthetic-child-1": rejected,
		"synthetic-child-2": rejected,
		"synthetic-top":     errors.New("synthetic failure"),
	}, subagentLookup(store), "")
	must(t, err)
	if !strings.Contains(summary, "2 subagents "+issueLabels[issueSubagentNotCaptured].what) {
		t.Fatalf("summary %q does not count the subagents", summary)
	}

	// A later pass that selected other sessions keeps the mark it carries.
	previous, err := store.LoadStatus()
	must(t, err)
	must(t, recordSelectedPassSessionIssues(store, collector.Result{}, previous))
	carried, err := store.LoadStatus()
	must(t, err)
	if !mapsEqual(carried.SubagentIssues, map[string]bool{"synthetic-child-1": true, "synthetic-child-2": true}) {
		t.Fatalf("subagent_issues %v after a selected pass", carried.SubagentIssues)
	}

	group := "2 subagents " + issueLabels[issueSubagentNotCaptured].what
	failed := "1 session " + issueLabels[issueCaptureFailed].what
	for _, args := range [][]string{{"--issues"}, {"--verbose"}} {
		out := statusOutput(t, env, args...)
		line := ""
		for l := range strings.SplitSeq(out, "\n") {
			if strings.Contains(l, group) {
				line = l
			}
		}
		if line == "" || strings.Contains(line, "!") {
			t.Errorf("status %v group line %q, want an informational %q:\n%s", args, line, group, out)
		}
		if i := strings.Index(out, failed); i < 0 || i > strings.Index(out, group) {
			t.Errorf("status %v does not put %q before %q:\n%s", args, failed, group, out)
		}
	}
}

// A status file from before subagent_issues still counts a subagent that
// could not be captured as a subagent: only subagents carry that code.
func TestStatusIssueGroupsCountSubagentsFromAnOlderStatusFile(t *testing.T) {
	t.Parallel()
	home, env, _, _ := overviewFixture(t)
	store, err := state.Open(home)
	must(t, err)
	must(t, store.SaveStatus(state.Status{LastScanAt: env.now(), SessionIssues: map[string]string{"synthetic-child": issueSubagentNotCaptured}}))
	out := statusOutput(t, env, "--issues")
	if want := "1 subagent " + issueLabels[issueSubagentNotCaptured].what; !strings.Contains(out, want) {
		t.Errorf("status --issues lacks %q:\n%s", want, out)
	}
}

// A storage outage's group keeps saying the next pass retries, beside the
// command it moves to its own line.
func TestStorageUnavailableGroupSaysTheNextPassRetries(t *testing.T) {
	t.Parallel()
	group := issueGroup{code: issueStorageUnavailable, ids: []string{"synthetic"}, tally: issueTally{sessions: 1}}
	if text := group.text(); !strings.Contains(text, "the next pass also retries") || strings.Contains(text, group.command()) {
		t.Errorf("group text %q, want the retry clause without %q", text, group.command())
	}
}
