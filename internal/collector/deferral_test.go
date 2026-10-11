package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// failingReadSources serves the built-in sources, except that every read
// fails with err.
type failingReadSources struct{ err error }

func (s failingReadSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := testSources.LookupSources(name)
	if !ok {
		return nil, nil, false
	}
	return failingReadProvider{SourceProvider: p, err: s.err}, f, true
}

type failingReadProvider struct {
	agentapi.SourceProvider
	err error
}

func (p failingReadProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	pass, err := p.SourceProvider.OpenPass(ctx, env)
	if err != nil {
		return nil, err
	}
	return failingReadPass{SourcePass: pass, err: p.err}, nil
}

type failingReadPass struct {
	agentapi.SourcePass
	err error
}

func (p failingReadPass) Read(context.Context, agentapi.SourceRef, agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	return nil, p.err
}

// coverageLookup answers every thread with the coverage it was given.
type coverageLookup struct {
	complete bool
	err      error
	threads  int
}

func (l *coverageLookup) Rollout(context.Context, string) ([]agentapi.SourceRef, error) {
	return nil, nil
}

func (l *coverageLookup) Thread(context.Context, string) (agentapi.CodexRolloutSet, error) {
	l.threads++
	return agentapi.CodexRolloutSet{Complete: l.complete}, l.err
}

func (l *coverageLookup) Check(context.Context, string, string) error { return nil }

// lineageRefusal is the refusal the Codex source gives while a thread's
// rollout lineage is not fully known.
func lineageRefusal() error {
	return agentapi.Wrap(agentapi.Unavailable, errors.New("rollout lineage incomplete"))
}

// Regression: 2026-10 the owner's sync listed 38 Codex sessions as failed
// with "source unavailable" while their history coverage was only not
// complete yet that pass. Such a scan is deferred, not failed: out of
// Errors and status's session issues, still pending with its scan journal,
// and a real failure is still a failure.
func TestRunDefersScansTheNextPassCanFinish(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		lookup *coverageLookup
		reason string // "" when the scan fails
	}{
		{"coverage incomplete", lineageRefusal(), &coverageLookup{}, DeferredHistoryCoverage},
		{"coverage incomplete through a join", errors.Join(lineageRefusal(), nil), &coverageLookup{}, DeferredHistoryCoverage},
		{"coverage complete", lineageRefusal(), &coverageLookup{complete: true}, ""},
		{"coverage unknown", lineageRefusal(), &coverageLookup{err: errors.New("lookup failed")}, ""},
		{"no coverage lookup", lineageRefusal(), nil, ""},
		{"input/output failure while coverage is incomplete", agentapi.Wrap(agentapi.Unavailable, &fs.PathError{Op: "read", Path: "/synthetic", Err: errors.New("i/o")}), &coverageLookup{}, ""},
		{"source changed", agentapi.Wrap(agentapi.Changed, errors.New("native index changed during snapshot")), nil, DeferredSourceChanged},
		{"source changed beside a refusal", errors.Join(agentapi.Wrap(agentapi.Changed, errors.New("changed")), agentapi.Wrap(agentapi.Unsafe, errors.New("refused"))), nil, ""},
		{"unsafe source", agentapi.Wrap(agentapi.Unsafe, errors.New("refused")), &coverageLookup{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local := newTestStore(t)
			reg := registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript))
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
			// A nil *coverageLookup must stay a nil interface.
			var lookup agentapi.CodexRolloutLookup
			if tc.lookup != nil {
				lookup = tc.lookup
			}
			opts := Options{Sources: failingReadSources{err: tc.err}, Parsers: testParsers, MachineID: "m", CodexRollouts: lookup, Now: func() time.Time { return now }}
			result, err := Run(t.Context(), local, storagetest.NewMemoryStore(), opts)
			if err != nil {
				t.Fatal(err)
			}
			status, err := local.LoadStatus()
			if err != nil {
				t.Fatal(err)
			}
			journaled, err := local.ScanPending(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			if !journaled || status.PendingCount != 1 {
				t.Fatalf("journaled=%v pending=%d, want the scan left for the next pass", journaled, status.PendingCount)
			}
			if tc.reason == "" {
				if result.Errors[reg.ArchiveSessionID] == nil || len(result.Deferred) != 0 || len(status.DeferredSessions) != 0 || len(status.LastErrors) == 0 {
					t.Fatalf("a failure was not reported as one: errors=%v deferred=%v status=%+v", result.Errors, result.Deferred, status)
				}
				return
			}
			if len(result.Errors) != 0 || result.Deferred[reg.ArchiveSessionID] != tc.reason || status.DeferredSessions[reg.ArchiveSessionID] != tc.reason || len(status.LastErrors) != 0 {
				t.Fatalf("deferral misreported: errors=%v deferred=%v status=%+v, want %s", result.Errors, result.Deferred, status, tc.reason)
			}
			owed, err := local.Outstanding(reg, false)
			if err != nil {
				t.Fatal(err)
			}
			// Retention is unchanged: a journaled scan never defers expiry.
			if !owed.Pending() || owed.DefersExpiry() {
				t.Fatalf("outstanding=%+v, want pending without deferring expiry", owed)
			}
		})
	}
}

func TestDeferralReasonNeedsEveryFailureToBeDeferrable(t *testing.T) {
	t.Parallel()
	coverage := agentapi.WithCause(lineageRefusal(), agentapi.CauseHistoryCoverage)
	changed := agentapi.Wrap(agentapi.Changed, errors.New("changed"))
	unsafe := agentapi.Wrap(agentapi.Unsafe, errors.New("refused"))
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"nil", nil, ""},
		{"plain error", errors.New("boom"), ""},
		{"coverage", coverage, DeferredHistoryCoverage},
		{"unnamed refusal", lineageRefusal(), ""},
		{"changed", changed, DeferredSourceChanged},
		{"changed wrapped with context", fmt.Errorf("read: %w", changed), DeferredSourceChanged},
		{"coverage names a mixed deferral", errors.Join(changed, coverage), DeferredHistoryCoverage},
		{"coverage beside a refusal", errors.Join(coverage, unsafe), ""},
		{"wrapped join with a refusal", fmt.Errorf("read: %w", errors.Join(changed, unsafe)), ""},
		{"changed beside a plain error", errors.Join(changed, errors.New("close failed")), ""},
		{"unsafe wrapping changed", agentapi.Wrap(agentapi.Unsafe, changed), ""},
	} {
		reason, ok := deferralReason(tc.err)
		if reason != tc.reason || ok != (tc.reason != "") {
			t.Errorf("%s: deferralReason=%q,%v want %q", tc.name, reason, ok, tc.reason)
		}
	}
}

func TestNameCoverageRefusalsOnlyNamesBareRefusals(t *testing.T) {
	t.Parallel()
	bare := lineageRefusal()
	named := nameCoverageRefusals(errors.Join(bare, agentapi.Wrap(agentapi.Unsafe, errors.New("refused"))))
	var e *agentapi.SourceError
	if !errors.As(named, &e) || e.Cause != agentapi.CauseHistoryCoverage || e.Kind != agentapi.Unavailable {
		t.Fatalf("joined refusal not named: %v", named)
	}
	var original *agentapi.SourceError
	if !errors.As(bare, &original) || original.Cause != "" {
		t.Fatal("naming changed the original error")
	}
	for _, unnamed := range []error{
		agentapi.Wrap(agentapi.Unavailable, fmt.Errorf("open: %w", fs.ErrPermission)),
		agentapi.Wrap(agentapi.Unavailable, context.Canceled),
		agentapi.Wrap(agentapi.Unavailable, agentapi.ErrReadBudget),
		agentapi.Wrap(agentapi.Unavailable, errors.Join(errors.New("a"), errors.New("b"))),
		agentapi.Wrap(agentapi.Missing, errors.New("gone")),
		fmt.Errorf("context: %w", lineageRefusal()),
	} {
		if _, deferred := deferralReason(nameCoverageRefusals(unnamed)); deferred {
			t.Errorf("%v was named a coverage deferral", unnamed)
		}
	}
}

// A refusal no later pass clears (coverage that never completes, a source
// changed on every read) must not stay "rechecking" for good: once a
// session's scans have been deferred on every pass for MaxDeferral, the next
// deferral is a failure, and a pass that does not defer it starts the clock
// again.
func TestRunFailsDeferralsOlderThanMaxDeferral(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	id := reg.ArchiveSessionID
	start := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	changed := agentapi.Wrap(agentapi.Changed, errors.New("native index changed during snapshot"))
	pass := func(at time.Time) (Result, state.Status) {
		t.Helper()
		opts := Options{Sources: failingReadSources{err: changed}, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at }}
		result, err := Run(t.Context(), local, storagetest.NewMemoryStore(), opts)
		if err != nil {
			t.Fatal(err)
		}
		status, err := local.LoadStatus()
		if err != nil {
			t.Fatal(err)
		}
		return result, status
	}
	deferredSince := func(at, since time.Time) {
		t.Helper()
		result, status := pass(at)
		if len(result.Errors) != 0 || result.Deferred[id] != DeferredSourceChanged || !status.DeferredSince[id].Equal(since) {
			t.Fatalf("pass at %v: errors=%v deferred=%v since=%v, want deferred since %v", at, result.Errors, result.Deferred, status.DeferredSince, since)
		}
	}
	deferredSince(start, start)
	deferredSince(start.Add(MaxDeferral-time.Minute), start)
	result, status := pass(start.Add(MaxDeferral))
	if err := result.Errors[id]; err == nil || !strings.Contains(err.Error(), DeferralExpiredProblem) || len(result.Deferred) != 0 || len(status.DeferredSessions) != 0 || len(status.DeferredSince) != 0 || len(status.LastErrors) == 0 {
		t.Fatalf("expired deferral: errors=%v deferred=%v status=%+v, want a failure", result.Errors, result.Deferred, status)
	}
	if status.PendingCount != 1 {
		t.Fatalf("pending=%d, want the failed scan still pending", status.PendingCount)
	}
	// The failure was reported; the next deferral starts a new run.
	restart := start.Add(MaxDeferral + time.Hour)
	deferredSince(restart, restart)
}
