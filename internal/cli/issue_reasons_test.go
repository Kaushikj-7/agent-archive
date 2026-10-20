package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

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
	} {
		if got := sessionIssueReason(tc.err); got != tc.want {
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

	full := "268 sessions failed to capture or upload: " + state.ErrDurableStorageCapacity.Error() + " — run agent-archive sync for details"
	other := "1 session failed to capture or upload: error not recognized — run agent-archive sync for details"
	issues := statusOutput(t, env, "--issues")
	for _, want := range []string{full, "synthetic-000, synthetic-001, synthetic-002 and 265 more", other, "synthetic-other"} {
		if strings.Count(issues, want) != 1 {
			t.Errorf("status --issues shows %q %d times, want once:\n%s", want, strings.Count(issues, want), issues)
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
