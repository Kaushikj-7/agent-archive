package cli

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
)

// deferredFixture is a configured home whose imported Codex sessions are set
// up one state each.
type deferredFixture struct {
	home     string
	userHome string
	now      time.Time
	cfg      config.Config
	store    *state.Store
	project  string
}

func newDeferredFixture(t *testing.T) deferredFixture {
	t.Helper()
	f := deferredFixture{home: t.TempDir(), userHome: t.TempDir(), now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), project: t.TempDir()}
	if err := os.Chmod(f.home, 0o700); err != nil {
		t.Fatal(err)
	}
	f.cfg = pairTestConfig(f.now, []string{"codex"}, f.project)
	if err := config.Save(f.home, f.cfg); err != nil {
		t.Fatal(err)
	}
	var err error
	if f.store, err = state.Open(f.home); err != nil {
		t.Fatal(err)
	}
	return f
}

// session registers an imported session, published or not, with a re-scan
// journaled and a request queued as asked.
func (f deferredFixture) session(t *testing.T, id string, published, journaled, requested bool) {
	t.Helper()
	reg := saveImportedSession(t, f.store, f.now, id, f.project)
	if published {
		bundle := archive.SourceBundle{ArchiveSessionID: id, Capture: archive.SourceCapture{Harness: reg.Harness, CapturedAt: f.now.Add(-time.Hour)}}
		if err := statetest.SavePublished(f.store, id, bundle, f.now.Add(-time.Hour), state.CacheStatusPublished); err != nil {
			t.Fatal(err)
		}
	}
	if journaled {
		if err := f.store.SetScanPending(id, true); err != nil {
			t.Fatal(err)
		}
	}
	if requested {
		if err := f.store.SaveRequest(id, "stop", f.now); err != nil {
			t.Fatal(err)
		}
	}
}

// Regression: 2026-10 the owner's backfill history said "14 waiting" for an
// import whose sessions were all in the archive: a published session with a
// journaled re-scan counted as waiting. Waiting means not in the archive yet;
// a published one is counted apart.
func TestBackfillHistoryCountsPublishedSessionsApartFromWaiting(t *testing.T) {
	t.Parallel()
	done := time.Date(2026, 10, 10, 11, 0, 0, 0, time.UTC)
	batch := backfill.Batch{ID: "batch-1", CompletedAt: &done, Sessions: []string{"x"}}
	for _, tc := range []struct {
		name     string
		sessions func(t *testing.T, f deferredFixture)
		want     string
	}{
		{"published with a re-scan journaled", func(t *testing.T, f deferredFixture) {
			t.Helper()
			f.session(t, "settled", true, false, false)
			f.session(t, "rechecking", true, true, false)
		}, "uploaded · 1 rechecking"},
		{"published with newer work", func(t *testing.T, f deferredFixture) {
			t.Helper()
			f.session(t, "updating", true, true, true)
		}, "uploaded · 1 updating"},
		{"every kind", func(t *testing.T, f deferredFixture) {
			t.Helper()
			f.session(t, "never", false, false, false)
			f.session(t, "never-journaled", false, true, false)
			f.session(t, "rechecking", true, true, false)
			f.session(t, "updating", true, false, true)
		}, "2 waiting · 1 rechecking · 1 updating"},
		{"all published", func(t *testing.T, f deferredFixture) {
			t.Helper()
			f.session(t, "settled", true, false, false)
		}, "uploaded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newDeferredFixture(t)
			tc.sessions(t, f)
			regs, err := f.store.LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			got, err := batchUploadState(f.store, f.cfg, regs, batch, true)
			if err != nil || got != tc.want {
				t.Fatalf("upload state = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// Regression: 2026-10 status listed Codex sessions whose history coverage
// was incomplete as "failed to capture or upload". A deferred session is
// listed as rechecking, with its reason, and is not a failure; a session
// whose last scan failed still is.
func TestStatusListsDeferredSessionsApartFromFailures(t *testing.T) {
	t.Parallel()
	f := newDeferredFixture(t)
	f.session(t, "deferred", true, true, false)
	f.session(t, "failed", true, true, false)
	// Deferred by an earlier pass, but its scan has completed since.
	f.session(t, "stale", true, false, true)
	summary := "1 session failed to capture or upload — run agent-archive sync for details"
	status := state.Status{
		LastScanAt:       f.now,
		PendingCount:     3,
		SessionIssues:    map[string]string{"failed": issueCaptureFailed},
		IssueCounts:      map[string]int{issueCaptureFailed: 1},
		DeferredSessions: map[string]string{"deferred": collector.DeferredHistoryCoverage, "stale": collector.DeferredSourceChanged},
	}
	status.SetLastErrors(summary)
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
	if got := states["deferred"]; got.State != uploadingRechecking || got.Deferred != collector.DeferredHistoryCoverage || got.Issue != "" {
		t.Errorf("deferred session = %+v", got)
	}
	if got := states["failed"]; got.State != uploadingFailing || got.Issue != issueCaptureFailed {
		t.Errorf("failed session = %+v", got)
	}
	if got := states["stale"]; got.State != uploadingPending || got.Deferred != "" {
		t.Errorf("session whose scan completed since = %+v", got)
	}

	var out strings.Builder
	if code := runStatusCommand([]string{"codex"}, &out, &out, env); code != 0 {
		t.Fatalf("status exit %d:\n%s", code, out.String())
	}
	text := out.String()
	if strings.Count(text, "failed to capture or upload") != 1 || strings.Count(text, "waiting for Codex history coverage") != 1 {
		t.Fatalf("status codex:\n%s", text)
	}
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, "waiting for Codex history coverage") && strings.HasPrefix(strings.TrimSpace(line), "!") {
			t.Fatalf("a deferred session is marked as a problem: %q", line)
		}
	}
	out.Reset()
	if code := runStatusCommand([]string{"--json"}, &out, &out, env); code != 0 {
		t.Fatalf("status --json exit %d", code)
	}
	var fields struct {
		Collector struct {
			DeferredSessions map[string]string `json:"deferred_sessions"`
		} `json:"collector"`
	}
	if err := json.Unmarshal([]byte(out.String()), &fields); err != nil {
		t.Fatal(err)
	}
	if fields.Collector.DeferredSessions["deferred"] != collector.DeferredHistoryCoverage {
		t.Fatalf("status --json deferred_sessions = %v", fields.Collector.DeferredSessions)
	}
}

func TestSyncCountsDeferredSessionsByReason(t *testing.T) {
	t.Parallel()
	result := collector.Result{
		Scanned: 4,
		Skipped: []string{"unchanged"},
		Errors:  map[string]error{"failed": errors.New("boom")},
		// "failed" was deferred, then failed retention: counted as failed only.
		Deferred: map[string]string{"a": collector.DeferredHistoryCoverage, "b": collector.DeferredHistoryCoverage, "failed": collector.DeferredHistoryCoverage},
	}
	if got, want := syncSummary(result), "Scanned 4 session(s): 0 published, 1 unchanged, 1 failed; 2 waiting for Codex history coverage."; got != want {
		t.Errorf("sync summary = %q, want %q", got, want)
	}
	for _, tc := range []struct {
		deferred map[string]string
		want     string
	}{
		{nil, ""},
		{map[string]string{"a": collector.DeferredHistoryCoverage}, "; 1 waiting for Codex history coverage"},
		{map[string]string{"a": collector.DeferredHistoryCoverage, "b": collector.DeferredHistoryCoverage, "c": collector.DeferredSourceChanged, "d": "from_a_later_version"},
			"; 1 rechecking after the source changed while being read; 1 rechecking on the next pass; 2 waiting for Codex history coverage"},
	} {
		if got := deferredSummary(tc.deferred, nil); got != tc.want {
			t.Errorf("deferredSummary(%v) = %q, want %q", tc.deferred, got, tc.want)
		}
	}
}

// Durable admission and deferral reporting must compose: an already published
// import owes only a recheck or newer work, while its verification count stays
// distinct from the sessions whose first publication is still waiting.
func TestBackfillHistoryStagedVerificationAndDeferralCounts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		waiting bool
		want    string
	}{
		{"first publication waiting", true, "3 registered; 3 durably staged; 2 remotely verified; 1 waiting · 1 rechecking · 1 updating"},
		{"all first publications verified", false, "uploaded; 2 remotely verified · 1 rechecking · 1 updating"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newDeferredFixture(t)
			f.session(t, "rechecking", true, true, false)
			f.session(t, "updating", true, true, true)
			if tc.waiting {
				f.session(t, "waiting", false, true, true)
			}
			regs, err := f.store.LoadRegistrations()
			if err != nil {
				t.Fatal(err)
			}
			for i := range regs {
				regs[i].StagedImportSHA256 = strings.Repeat("a", 64)
			}
			done := f.now
			batch := backfill.Batch{ID: "batch-1", CompletedAt: &done, Sessions: []string{"rechecking", "updating"}}
			got, err := batchUploadState(f.store, f.cfg, regs, batch, true)
			if err != nil || got != tc.want {
				t.Fatalf("upload state = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
