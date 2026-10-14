package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Regression: 2026-10 a Codex session the collector's passes kept running
// out of time before reaching was listed as "waiting for its first upload",
// with nothing saying it was queued behind hundreds of others. A session the
// last pass did not reach is listed as queued, with how many were ahead of
// it, and is not a problem.
func TestStatusListsUnreachedSessionsAsQueued(t *testing.T) {
	t.Parallel()
	f := newDeferredFixture(t)
	f.session(t, "never-published", false, false, true)
	f.session(t, "published-before", true, false, true)
	f.session(t, "front-of-queue", false, false, true)
	// Not reached either, but its last scan failed: the failure is what
	// status says.
	f.session(t, "failed", false, false, true)
	status := state.Status{
		LastScanAt:        f.now,
		PendingCount:      4,
		SessionIssues:     map[string]string{"failed": issueCaptureFailed},
		IssueCounts:       map[string]int{issueCaptureFailed: 1},
		UnreachedSessions: map[string]int{"never-published": 372, "published-before": 12, "front-of-queue": 0, "failed": 3},
	}
	status.SetLastErrors("1 session failed to capture or upload — run agent-archive sync for details")
	if err := f.store.SaveStatus(status); err != nil {
		t.Fatal(err)
	}
	env := pairStatusEnv(t, f.home, f.userHome, f.now, "codex")
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]uploadingSession{}
	for _, session := range view.Apps[0].Uploading {
		states[session.ArchiveSessionID] = session
	}
	for id, ahead := range map[string]int{"never-published": 372, "published-before": 12, "front-of-queue": 0} {
		if got := states[id]; got.State != uploadingQueued || got.QueuedAhead == nil || *got.QueuedAhead != ahead || got.Issue != "" {
			t.Errorf("%s = %+v, want queued with %d ahead", id, got, ahead)
		}
	}
	if got := states["failed"]; got.State != uploadingFailing || got.QueuedAhead != nil {
		t.Errorf("failed session = %+v", got)
	}

	var out strings.Builder
	if code := runStatusCommand([]string{"codex"}, &out, &out, env); code != 0 {
		t.Fatalf("status exit %d:\n%s", code, out.String())
	}
	text := out.String()
	for _, want := range []string{"queued for the next pass, 372 ahead", "queued for the next pass, 12 ahead"} {
		if strings.Count(text, want) != 1 {
			t.Fatalf("status codex does not say %q once:\n%s", want, text)
		}
	}
	if strings.Contains(text, "waiting for its first upload") {
		t.Fatalf("status codex lists a queued session as waiting for its first upload:\n%s", text)
	}
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "queued for the next pass") && strings.HasPrefix(strings.TrimSpace(line), "!") {
			t.Fatalf("a queued session is marked as a problem: %q", line)
		}
	}

	out.Reset()
	if code := runStatusCommand([]string{"--json"}, &out, &out, env); code != 0 {
		t.Fatalf("status --json exit %d", code)
	}
	var fields struct {
		Collector struct {
			UnreachedSessions map[string]int `json:"unreached_sessions"`
		} `json:"collector"`
		Apps []struct {
			Uploading []struct {
				ArchiveSessionID string         `json:"archive_session_id"`
				State            uploadingState `json:"state"`
				QueuedAhead      *int           `json:"queued_ahead"`
			} `json:"uploading"`
		} `json:"applications"`
	}
	if err := json.Unmarshal([]byte(out.String()), &fields); err != nil {
		t.Fatal(err)
	}
	if fields.Collector.UnreachedSessions["never-published"] != 372 {
		t.Fatalf("status --json unreached_sessions = %v", fields.Collector.UnreachedSessions)
	}
	// Every queued session has queued_ahead, the one at the front of the
	// queue (0 ahead) too; no other session has it.
	wantAhead := map[string]int{"never-published": 372, "published-before": 12, "front-of-queue": 0}
	seen := 0
	for _, session := range fields.Apps[0].Uploading {
		want, queued := wantAhead[session.ArchiveSessionID]
		switch {
		case queued && (session.State != uploadingQueued || session.QueuedAhead == nil || *session.QueuedAhead != want):
			t.Errorf("status --json lists %s as %s with queued_ahead %v, want queued with %d", session.ArchiveSessionID, session.State, session.QueuedAhead, want)
		case !queued && session.QueuedAhead != nil:
			t.Errorf("status --json gives %s (%s) queued_ahead %d", session.ArchiveSessionID, session.State, *session.QueuedAhead)
		}
		if queued {
			seen++
		}
	}
	if seen != len(wantAhead) {
		t.Fatalf("status --json lists %d of the %d queued sessions:\n%s", seen, len(wantAhead), out.String())
	}
}

func TestSyncCountsSessionsLeftForTheNextPass(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		unreached map[string]int
		errors    map[string]error
		want      string
	}{
		{nil, nil, "Scanned 2 session(s): 2 published, 0 unchanged, 0 failed."},
		{map[string]int{"a": 0, "b": 1, "c": 2}, nil, "Scanned 2 session(s): 2 published, 0 unchanged, 0 failed; 3 left for the next pass."},
		// A session that failed is counted as failed only, as a deferred
		// one is.
		{map[string]int{"a": 0, "b": 1}, map[string]error{"a": errors.New("unreadable")}, "Scanned 2 session(s): 2 published, 0 unchanged, 1 failed; 1 left for the next pass."},
		{map[string]int{"a": 0}, map[string]error{"a": errors.New("unreadable")}, "Scanned 2 session(s): 2 published, 0 unchanged, 1 failed."},
	} {
		result := collector.Result{Scanned: 2, Published: []string{"x", "y"}, Unreached: tc.unreached, Errors: tc.errors}
		if got := syncSummary(result); got != tc.want {
			t.Errorf("sync summary = %q, want %q", got, tc.want)
		}
	}
}
