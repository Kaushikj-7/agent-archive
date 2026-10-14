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
// fails with err, after calling cancel when it is set.
type failingReadSources struct {
	err    error
	cancel func()
}

func (s failingReadSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := testSources.LookupSources(name)
	if !ok {
		return nil, nil, false
	}
	return failingReadProvider{SourceProvider: p, err: s.err, cancel: s.cancel}, f, true
}

type failingReadProvider struct {
	agentapi.SourceProvider
	err    error
	cancel func()
}

func (p failingReadProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	pass, err := p.SourceProvider.OpenPass(ctx, env)
	if err != nil {
		return nil, err
	}
	return failingReadPass{SourcePass: pass, err: p.err, cancel: p.cancel}, nil
}

type failingReadPass struct {
	agentapi.SourcePass
	err    error
	cancel func()
}

func (p failingReadPass) Read(context.Context, agentapi.SourceRef, agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	if p.cancel != nil {
		p.cancel()
	}
	return nil, p.err
}

// threadCountingLookup answers every thread with an empty set, and counts the
// questions.
type threadCountingLookup struct {
	threads int
}

func (l *threadCountingLookup) Rollout(context.Context, string) ([]agentapi.SourceRef, error) {
	return nil, nil
}

func (l *threadCountingLookup) Thread(context.Context, string) (agentapi.CodexRolloutSet, error) {
	l.threads++
	return agentapi.CodexRolloutSet{}, nil
}

func (l *threadCountingLookup) Check(context.Context, string, string) error { return nil }

// baseRefusal is the refusal the Codex source gives when a rollout a
// thread's lineage continues from cannot be found: it names its cause.
func baseRefusal() error {
	return agentapi.WithCause(lineageRefusal(), agentapi.CauseHistoryBaseUnavailable)
}

// lineageRefusal is the same refusal without its cause.
func lineageRefusal() error {
	return agentapi.Wrap(agentapi.Unavailable, errors.New("history dependency missing or ambiguous"))
}

// A scan the next pass is expected to finish (its source changed while
// being read) is deferred, not failed: out of Errors and status's session
// issues, still pending with its scan journal. A lineage the source could
// not resolve is a named failure, reported as one and retried, never a
// silent wait. The source's own error decides: the pass never asks the
// lookup about a failed scan.
func TestRunDefersScansTheNextPassCanFinish(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		lookup *threadCountingLookup
		reason string // "" when the scan fails
	}{
		{"history base unavailable", baseRefusal(), &threadCountingLookup{}, ""},
		{"history base unavailable through a join", errors.Join(baseRefusal(), nil), &threadCountingLookup{}, ""},
		{"history base unavailable wrapped with context", fmt.Errorf("revision candidates: %w", baseRefusal()), &threadCountingLookup{}, ""},
		{"untagged refusal", lineageRefusal(), &threadCountingLookup{}, ""},
		{"input/output failure", agentapi.Wrap(agentapi.Unavailable, &fs.PathError{Op: "read", Path: "/synthetic", Err: errors.New("i/o")}), &threadCountingLookup{}, ""},
		{"current header unavailable", agentapi.Wrap(agentapi.Unavailable, errors.New("current header unavailable")), &threadCountingLookup{}, ""},
		{"source changed", agentapi.Wrap(agentapi.Changed, errors.New("native index changed during snapshot")), nil, DeferredSourceChanged},
		{"source changed beside a refusal", errors.Join(agentapi.Wrap(agentapi.Changed, errors.New("changed")), agentapi.Wrap(agentapi.Unsafe, errors.New("refused"))), nil, ""},
		{"unsafe source", agentapi.Wrap(agentapi.Unsafe, errors.New("refused")), &threadCountingLookup{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			local := newTestStore(t)
			reg := registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript))
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
			// A nil *threadCountingLookup must stay a nil interface.
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
			if tc.lookup != nil && tc.lookup.threads != 0 {
				t.Fatalf("the pass asked the lookup %d times about a failed scan", tc.lookup.threads)
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
	base := baseRefusal()
	changed := agentapi.Wrap(agentapi.Changed, errors.New("changed"))
	unsafe := agentapi.Wrap(agentapi.Unsafe, errors.New("refused"))
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"nil", nil, ""},
		{"plain error", errors.New("boom"), ""},
		{"history base unavailable", base, ""},
		{"unnamed refusal", lineageRefusal(), ""},
		{"changed", changed, DeferredSourceChanged},
		{"changed wrapped with context", fmt.Errorf("read: %w", changed), DeferredSourceChanged},
		{"changed beside an unavailable base", errors.Join(changed, base), ""},
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

// A refusal no later pass clears (a source changed on every read) must not stay "rechecking" for good: once a
// session's scans have been deferred on every pass for MaxDeferral, every
// further deferral is a failure; a pass that ends before scanning it leaves
// the clock running, and a pass that reads it starts the clock again.
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
	remote := storagetest.NewMemoryStore()
	run := func(opts Options, at time.Time) (Result, state.Status) {
		t.Helper()
		opts.Parsers, opts.MachineID, opts.Now = testParsers, "m", func() time.Time { return at }
		result, err := Run(t.Context(), local, remote, opts)
		if err != nil {
			t.Fatal(err)
		}
		status, err := local.LoadStatus()
		if err != nil {
			t.Fatal(err)
		}
		return result, status
	}
	refusing := Options{Sources: failingReadSources{err: changed}}
	pass := func(at time.Time) (Result, state.Status) {
		t.Helper()
		return run(refusing, at)
	}
	deferredSince := func(at, since time.Time) {
		t.Helper()
		result, status := pass(at)
		if len(result.Errors) != 0 || result.Deferred[id] != DeferredSourceChanged || !status.DeferredSince[id].Equal(since) {
			t.Fatalf("pass at %v: errors=%v deferred=%v since=%v, want deferred since %v", at, result.Errors, result.Deferred, status.DeferredSince, since)
		}
	}
	expired := func(at time.Time) {
		t.Helper()
		result, status := pass(at)
		if err := result.Errors[id]; err == nil || !strings.Contains(err.Error(), DeferralExpiredProblem) || len(result.Deferred) != 0 || len(status.DeferredSessions) != 0 || !status.DeferredSince[id].Equal(start) || len(status.LastErrors) == 0 {
			t.Fatalf("pass at %v: errors=%v deferred=%v status=%+v, want a failure with the clock still running from %v", at, result.Errors, result.Deferred, status, start)
		}
		if status.PendingCount != 1 {
			t.Fatalf("pending=%d, want the failed scan still pending", status.PendingCount)
		}
	}
	deferredSince(start, start)
	// A pass that stops before reaching the session keeps its clock.
	stopped := Options{Sources: refusing.Sources, Stop: func() bool { return true }}
	if result, status := run(stopped, start.Add(time.Hour)); result.Deferred[id] != "" || !status.DeferredSince[id].Equal(start) {
		t.Fatalf("stopped pass: deferred=%v since=%v, want the session unscanned and its clock from %v kept", result.Deferred, status.DeferredSince, start)
	}
	// So does one that runs out of time with the session in flight.
	ctx, cancel := context.WithCancel(t.Context())
	interrupted := Options{Sources: failingReadSources{err: context.Canceled, cancel: cancel}, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return start.Add(2 * time.Hour) }}
	if result, err := Run(ctx, local, remote, interrupted); err != nil || len(result.Errors) != 0 || result.Deferred[id] != "" {
		t.Fatalf("interrupted pass: err=%v errors=%v deferred=%v, want the session left for the next pass", err, result.Errors, result.Deferred)
	}
	if status, err := local.LoadStatus(); err != nil || !status.DeferredSince[id].Equal(start) {
		t.Fatalf("interrupted pass: since=%v (%v), want the clock from %v kept", status.DeferredSince, err, start)
	}
	deferredSince(start.Add(MaxDeferral-time.Minute), start)
	expired(start.Add(MaxDeferral))
	// Once reported, it stays a failure on every pass that defers it.
	expired(start.Add(MaxDeferral + time.Hour))
	// A pass that reads the session ends the run.
	if result, status := run(Options{Sources: testSources}, start.Add(MaxDeferral+2*time.Hour)); len(result.Errors) != 0 || len(status.DeferredSince) != 0 {
		t.Fatalf("read pass: errors=%v since=%v, want no failure and no clock", result.Errors, status.DeferredSince)
	}
	// Scan it again: the next deferral starts a new run.
	if err := local.SetScanPending(id, true); err != nil {
		t.Fatal(err)
	}
	restart := start.Add(MaxDeferral + 3*time.Hour)
	deferredSince(restart, restart)
}

// A recorded start after the pass's own clock (the clock was set back) is
// not trusted: the run starts again now rather than never expiring.
func TestDeferredSinceRestartsAFutureClock(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	p := &pass{now: now, priorDeferredSince: map[string]time.Time{"future": now.Add(time.Hour), "past": now.Add(-time.Hour)}}
	if got := p.deferredSince("future"); !got.Equal(now) {
		t.Errorf("deferredSince(future) = %v, want %v", got, now)
	}
	if got := p.deferredSince("past"); !got.Equal(now.Add(-time.Hour)) {
		t.Errorf("deferredSince(past) = %v, want %v", got, now.Add(-time.Hour))
	}
}

// A session the pass already recorded a failure for (a warning, say) is a
// failed session: its deferral joins that failure rather than counting it
// in Deferred as well.
func TestDeferScanFailsASessionThatAlreadyFailed(t *testing.T) {
	t.Parallel()
	warning := errors.New("synthetic warning")
	changed := agentapi.Wrap(agentapi.Changed, errors.New("changed"))
	p := &pass{now: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC), result: Result{Errors: map[string]error{"s": warning}}}
	p.deferScan("s", DeferredSourceChanged, changed)
	if len(p.result.Deferred) != 0 || !errors.Is(p.result.Errors["s"], warning) || !errors.Is(p.result.Errors["s"], changed) || p.pending != 1 {
		t.Fatalf("deferred=%v errors=%v pending=%d, want one failure joining both", p.result.Deferred, p.result.Errors, p.pending)
	}
}
