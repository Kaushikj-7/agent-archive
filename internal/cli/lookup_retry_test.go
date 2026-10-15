package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/state"
)

// A Codex session the collector held on the lookup allowance's backoff
// schedule keeps the issue its last attempt recorded: recording a pass's
// failures carries it beside them, counted in the summary, and recording a
// pass with no failure of its own still does.
func TestRecordSessionIssuesCarriesSessionsHeldForARetry(t *testing.T) {
	t.Parallel()
	f := newDeferredFixture(t)
	held := map[string]time.Time{"held": f.now.Add(3 * time.Hour)}
	for _, tc := range []struct {
		name   string
		errs   map[string]error
		counts int
	}{
		{"with another failure", map[string]error{"other": errors.New("boom")}, 2},
		{"alone", map[string]error{}, 1},
	} {
		// What collector.Run saves for a pass that held it.
		status := state.Status{LastScanAt: f.now, NextRetryAt: held, SessionIssues: map[string]string{"held": issueCaptureFailed}}
		if len(tc.errs) > 0 {
			status.SetLastErrors(collector.FailedSessionsProblem(len(tc.errs)))
		}
		if err := f.store.SaveStatus(status); err != nil {
			t.Fatal(err)
		}
		summary, err := recordSessionIssues(f.store, tc.errs, func(string) bool { return false }, collector.FailedSessionsProblem(len(tc.errs)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := f.store.LoadStatus()
		if err != nil {
			t.Fatal(err)
		}
		if got.SessionIssues["held"] != issueCaptureFailed || got.IssueCounts[issueCaptureFailed] != tc.counts || len(got.LastErrors) != 1 || got.LastErrors[0] != summary || !strings.HasPrefix(summary, plural(tc.counts, "session")) {
			t.Fatalf("%s: status=%+v summary=%q, want the held session's issue carried and counted", tc.name, got, summary)
		}
	}
}

// status lists a session held for a retry as failing, with its issue and
// when its next attempt is; status --json gives that time on its uploading
// entry and in collector.next_retry_at. sync's summary counts it apart from
// the pass's failures.
func TestStatusShowsASessionHeldForARetryAsFailingWithItsNextRetry(t *testing.T) {
	t.Parallel()
	f := newDeferredFixture(t)
	f.session(t, "held", false, true, true)
	next := f.now.Add(3 * time.Hour)
	status := state.Status{
		LastScanAt:    f.now,
		PendingCount:  1,
		SessionIssues: map[string]string{"held": issueCaptureFailed},
		IssueCounts:   map[string]int{issueCaptureFailed: 1},
		DeferredSince: map[string]time.Time{"held": f.now.Add(-collector.MaxDeferral - 2*time.Hour)},
		NextRetryAt:   map[string]time.Time{"held": next},
	}
	status.SetLastErrors("1 session failed to capture or upload — run agent-archive sync for details")
	if err := f.store.SaveStatus(status); err != nil {
		t.Fatal(err)
	}
	env := pairStatusEnv(t, f.home, f.userHome, f.now, "codex")
	var out strings.Builder
	if code := runStatusCommand([]string{"codex"}, &out, &out, env); code != 0 {
		t.Fatalf("status exit %d:\n%s", code, out.String())
	}
	if text := out.String(); !strings.Contains(text, "failed to capture or upload, next retry "+next.In(time.Local).Format("15:04")) && !strings.Contains(text, "failed to capture or upload, next retry "+next.Format("15:04")) {
		t.Fatalf("status codex:\n%s", text)
	}
	out.Reset()
	if code := runStatusCommand([]string{"--json"}, &out, &out, env); code != 0 {
		t.Fatalf("status --json exit %d", code)
	}
	var fields struct {
		Applications []struct {
			Uploading []struct {
				ArchiveSessionID string    `json:"archive_session_id"`
				State            string    `json:"state"`
				Issue            string    `json:"issue"`
				NextRetryAt      time.Time `json:"next_retry_at"`
			} `json:"uploading"`
		} `json:"applications"`
		Collector struct {
			NextRetryAt map[string]time.Time `json:"next_retry_at"`
		} `json:"collector"`
	}
	if err := json.Unmarshal([]byte(out.String()), &fields); err != nil {
		t.Fatal(err)
	}
	if !fields.Collector.NextRetryAt["held"].Equal(next) {
		t.Fatalf("status --json collector.next_retry_at = %v", fields.Collector.NextRetryAt)
	}
	found := false
	for _, app := range fields.Applications {
		for _, session := range app.Uploading {
			if session.ArchiveSessionID == "held" {
				found = true
				if session.State != string(uploadingFailing) || session.Issue != issueCaptureFailed || !session.NextRetryAt.Equal(next) {
					t.Fatalf("status --json uploading entry = %+v", session)
				}
			}
		}
	}
	if !found {
		t.Fatalf("status --json lists no uploading entry for the held session:\n%s", out.String())
	}

	summary := syncSummary(collector.Result{Scanned: 1, RetryLater: map[string]time.Time{"held": next}})
	if !strings.Contains(summary, "0 failed; 1 failing Codex session(s) held for a later retry (next at "+next.Local().Format("Jan 2 15:04")+")") {
		t.Fatalf("sync summary = %q", summary)
	}
}
