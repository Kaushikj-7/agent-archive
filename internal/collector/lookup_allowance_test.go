package collector

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// spentAllowanceLookup is a Codex lookup whose collector allowance is used up
// as soon as it begins: every call is refused as the real lookup refuses
// calls past its allowance, and each one is counted.
type spentAllowanceLookup struct {
	begun int
	calls int
}

func (l *spentAllowanceLookup) Rollout(context.Context, string) ([]agentapi.SourceRef, error) {
	l.calls++
	return nil, lookupAllowanceSpent()
}

func (l *spentAllowanceLookup) Thread(context.Context, string) (agentapi.CodexRolloutSet, error) {
	l.calls++
	return agentapi.CodexRolloutSet{}, lookupAllowanceSpent()
}

func (l *spentAllowanceLookup) Check(context.Context, string, string) error {
	l.calls++
	return lookupAllowanceSpent()
}

func (l *spentAllowanceLookup) BeginCollection() { l.begun++ }

func (l *spentAllowanceLookup) AllowanceSpent() bool { return true }

// A pass whose Codex lookup allowance is spent leaves its Codex sessions for
// the next pass without opening them: none fails, and the pass's other
// sessions are captured as usual.
func TestSpentCodexLookupAllowanceLeavesCodexSessionsWithoutFailing(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	dir := t.TempDir()
	codexReg := registration(t, writeTranscript(t, dir, "codex.jsonl", codexTranscript))
	if err := local.SaveRegistration(codexReg); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(codexReg.ArchiveSessionID, "stop", codexReg.RegisteredAt); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(dir, "claude.jsonl")
	if err := os.WriteFile(claudePath, []byte(`{"type":"assistant","sessionId":"claude-native","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":"visible"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Started after the Codex session, so the pass reaches it second.
	claudeReg := archive.SessionRegistration{ArchiveSessionID: "session-2", NativeSessionID: "claude-native", ProjectID: "project-1", ProjectRoot: "/p", Harness: archive.Harness{Name: "claude"}, TranscriptPath: claudePath, SessionStartedAt: codexReg.SessionStartedAt.Add(time.Minute), RegisteredAt: codexReg.RegisteredAt.Add(time.Minute)}
	if err := local.SaveRegistration(claudeReg); err != nil {
		t.Fatal(err)
	}
	lookup := &spentAllowanceLookup{}
	store := storagetest.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	result, err := Run(t.Context(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", CodexRollouts: lookup, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if lookup.begun != 1 {
		t.Fatalf("collector began %d lookup allowances, want 1", lookup.begun)
	}
	if lookup.calls != 0 {
		t.Fatalf("the pass made %d lookup calls past a spent allowance", lookup.calls)
	}
	if len(result.Errors) != 0 || len(result.Deferred) != 0 {
		t.Fatalf("a spent lookup allowance failed or deferred sessions: errors=%v deferred=%v", result.Errors, result.Deferred)
	}
	if len(result.Published) != 1 || result.Published[0] != claudeReg.ArchiveSessionID {
		t.Fatalf("published=%v, want only the Claude Code session", result.Published)
	}
	status, err := local.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.PendingCount != 1 || len(status.LastErrors) != 0 || len(status.SessionIssues) != 0 {
		t.Fatalf("status=%+v, want the Codex session pending with no problem", status)
	}

	// The next pass, with an allowance, captures the Codex session.
	result, err = Run(t.Context(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now.Add(time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 || result.Published[0] != codexReg.ArchiveSessionID || len(result.Errors) != 0 {
		t.Fatalf("next pass result=%+v, want the Codex session published", result)
	}
}

// allowanceLookup is a Codex lookup that finds no rollout, so each
// registered legacy session is read from its registered transcript, and
// whose collector allowance is fresh when a pass begins and spent once spent
// is set.
type allowanceLookup struct {
	threadCountingLookup
	spent atomic.Bool
}

func (l *allowanceLookup) RegisteredThread(context.Context, string) (agentapi.CodexRolloutSet, error) {
	return agentapi.CodexRolloutSet{}, nil
}

func (l *allowanceLookup) BeginCollection() { l.spent.Store(false) }

func (l *allowanceLookup) AllowanceSpent() bool { return l.spent.Load() }

// allowanceSpendingSources serves the built-in sources, except that reading
// the transcript at path spends all of lookup's allowance and is refused as
// the real lookup refuses a call past it: that session's lookups alone need
// more than the whole allowance. Reading the transcript at once, when set,
// is refused the same way the first time only, as for a session that was
// being read when the sessions before it had used up the rest.
type allowanceSpendingSources struct {
	path string
	// also, when set, is a second transcript read like the one at path.
	also     string
	once     string
	onceDone *atomic.Bool
	lookup   *allowanceLookup
}

func (s allowanceSpendingSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := testSources.LookupSources(name)
	if !ok {
		return nil, nil, false
	}
	return allowanceSpendingProvider{SourceProvider: p, sources: s}, f, true
}

type allowanceSpendingProvider struct {
	agentapi.SourceProvider
	sources allowanceSpendingSources
}

func (p allowanceSpendingProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	pass, err := p.SourceProvider.OpenPass(ctx, env)
	if err != nil {
		return nil, err
	}
	return allowanceSpendingPass{SourcePass: pass, sources: p.sources}, nil
}

type allowanceSpendingPass struct {
	agentapi.SourcePass
	sources allowanceSpendingSources
}

func (p allowanceSpendingPass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	if ref.Path == p.sources.path || (p.sources.also != "" && ref.Path == p.sources.also) || (p.sources.once != "" && ref.Path == p.sources.once && !p.sources.onceDone.Swap(true)) {
		p.sources.lookup.spent.Store(true)
		return nil, lookupAllowanceSpent()
	}
	return p.SourcePass.Read(ctx, ref, limits)
}

// A Codex session whose lookups alone use up the pass's whole Codex lookup
// allowance does not keep the Codex sessions after it from being captured:
// once deferred for it, the session waits behind the other Codex sessions on
// the following passes, including once its deferral has outlasted
// MaxDeferral and it fails instead.
func TestCodexSessionThatExhaustsTheLookupAllowanceDoesNotBlockLaterCodexSessions(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	dir := t.TempDir()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	save := func(n int) archive.SessionRegistration {
		t.Helper()
		reg := registration(t, writeTranscript(t, dir, fmt.Sprintf("codex-%d.jsonl", n), codexTranscript))
		reg.ArchiveSessionID, reg.NativeSessionID = fmt.Sprintf("session-%d", n), fmt.Sprintf("native-%d", n)
		reg.Origin = archive.SessionOriginImport
		reg.SessionStartedAt, reg.RegisteredAt = start.Add(time.Duration(n)*time.Minute), start.Add(time.Duration(n)*time.Minute+time.Second)
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		if err := local.SaveRequest(reg.ArchiveSessionID, "stop", reg.RegisteredAt); err != nil {
			t.Fatal(err)
		}
		return reg
	}
	heavy := save(1)
	lookup := &allowanceLookup{}
	store := storagetest.NewMemoryStore()
	run := func(at time.Time) Result {
		t.Helper()
		result, err := Run(t.Context(), local, store, Options{Sources: allowanceSpendingSources{path: heavy.TranscriptPath, lookup: lookup}, Parsers: testParsers, MachineID: "m", CodexRollouts: lookup, Now: func() time.Time { return at }})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	now := start.Add(time.Hour)

	// The sessions are imported, so the pass reads them oldest first with no
	// slot reserved for a live session: the heavy session, the oldest, is
	// read first; it spends the allowance and the session after it is left
	// unopened.
	later := save(2)
	result := run(now)
	if result.Deferred[heavy.ArchiveSessionID] != DeferredLookupAllowance || len(result.Published) != 0 {
		t.Fatalf("first pass result=%+v, want the heavy session deferred for the lookup allowance and nothing published", result)
	}

	// The next pass gives the allowance to the later session first.
	result = run(now.Add(time.Hour))
	if len(result.Published) != 1 || result.Published[0] != later.ArchiveSessionID || len(result.Errors) != 0 {
		t.Fatalf("second pass result=%+v, want the later Codex session published", result)
	}
	if result.Deferred[heavy.ArchiveSessionID] != DeferredLookupAllowance {
		t.Fatalf("second pass deferred=%v, want the heavy session deferred again", result.Deferred)
	}

	// Past MaxDeferral the heavy session fails instead of being deferred,
	// and still waits behind a newer Codex session on this pass. On the next
	// it is held for its next attempt on the backoff schedule, with no new
	// failure.
	newer := save(3)
	result = run(now.Add(MaxDeferral + time.Hour))
	if len(result.Published) != 1 || result.Published[0] != newer.ArchiveSessionID || result.Errors[heavy.ArchiveSessionID] == nil {
		t.Fatalf("pass past MaxDeferral result=%+v, want the newer session published and the heavy session failed", result)
	}
	newest := save(4)
	result = run(now.Add(MaxDeferral + 2*time.Hour))
	if len(result.Published) != 1 || result.Published[0] != newest.ArchiveSessionID || len(result.Errors) != 0 || !result.RetryLater[heavy.ArchiveSessionID].Equal(now.Add(MaxDeferral+3*time.Hour)) {
		t.Fatalf("pass after the heavy session failed result=%+v, want the newest session published and the heavy session held until %v", result, now.Add(MaxDeferral+3*time.Hour))
	}
}

// Only requested Codex sessions whose deferral the lookup allowance may be
// keeping going move, and only into slots requested Codex sessions held:
// every other session keeps its place, a Codex session deferred because its
// source changed keeps its turn, and among those that move, a session left
// unopened takes the waiter lane, ahead of the sessions not waiting, one
// that ran out goes after them, and one whose deferral has lasted
// MaxDeferral goes last.
func TestLookupAllowanceDeferralsMoveBehindOnlyOtherCodexSessions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	recent, old := now.Add(-time.Hour), now.Add(-MaxDeferral-time.Hour)
	reg := func(id, harness string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, Harness: archive.Harness{Name: harness}}
	}
	p := &pass{
		now: now,
		registrations: []archive.SessionRegistration{
			reg("expired", "codex"), reg("claude", "claude"), reg("unopened", "codex"), reg("changed", "codex"), reg("cursor", "cursor"), reg("heavy", "codex"), reg("plain", "codex"), reg("quiet", "codex"),
		},
		requests:           map[string]state.Request{},
		priorDeferred:      map[string]string{"heavy": DeferredLookupAllowance, "changed": DeferredSourceChanged, "claude": DeferredLookupAllowance},
		priorDeferredSince: map[string]time.Time{"heavy": recent, "unopened": recent, "expired": old, "changed": recent, "claude": recent},
	}
	for _, id := range []string{"heavy", "claude", "unopened", "changed", "cursor", "expired", "plain"} {
		p.requests[id] = state.Request{Token: "t-" + id}
	}
	p.orderLookupAllowanceDeferralsLast(p.registrations)
	var got []string
	for _, r := range p.registrations {
		got = append(got, r.ArchiveSessionID)
	}
	want := []string{"unopened", "claude", "changed", "plain", "cursor", "heavy", "expired", "quiet"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order=%v, want %v", got, want)
	}
}

// A session deferred for the lookup allowance only because the sessions read
// before it used up the rest is not held behind a session whose lookups
// alone need the whole allowance: among the sessions waiting on the
// allowance, the one the last pass left unopened is read before the one the
// last pass read and ran out on, so each gets a fresh allowance in turn.
func TestSessionWaitingOnTheLookupAllowanceIsNotHeldBehindAHeavierOne(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	dir := t.TempDir()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	save := func(n int) archive.SessionRegistration {
		t.Helper()
		reg := registration(t, writeTranscript(t, dir, fmt.Sprintf("codex-%d.jsonl", n), codexTranscript))
		reg.ArchiveSessionID, reg.NativeSessionID = fmt.Sprintf("session-%d", n), fmt.Sprintf("native-%d", n)
		reg.Origin = archive.SessionOriginImport
		reg.SessionStartedAt, reg.RegisteredAt = start.Add(time.Duration(n)*time.Minute), start.Add(time.Duration(n)*time.Minute+time.Second)
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		if err := local.SaveRequest(reg.ArchiveSessionID, "stop", reg.RegisteredAt); err != nil {
			t.Fatal(err)
		}
		return reg
	}
	heavy, unlucky := save(1), save(2)
	lookup := &allowanceLookup{}
	sources := allowanceSpendingSources{path: heavy.TranscriptPath, once: unlucky.TranscriptPath, onceDone: &atomic.Bool{}, lookup: lookup}
	store := storagetest.NewMemoryStore()
	now := start.Add(time.Hour)
	run := func() Result {
		t.Helper()
		now = now.Add(time.Hour)
		result, err := Run(t.Context(), local, store, Options{Sources: sources, Parsers: testParsers, MachineID: "m", CodexRollouts: lookup, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	// The sessions are imported, so the pass reads them oldest first. The
	// heavy session is read first and runs out; the next pass reads the other
	// session first, and it runs out too.
	if result := run(); result.Deferred[heavy.ArchiveSessionID] != DeferredLookupAllowance {
		t.Fatalf("first pass result=%+v, want the heavy session deferred for the lookup allowance", result)
	}
	if result := run(); result.Deferred[unlucky.ArchiveSessionID] != DeferredLookupAllowance {
		t.Fatalf("second pass result=%+v, want the other session deferred for the lookup allowance", result)
	}
	// Both now wait on the allowance. The heavy session, left unopened last
	// pass, goes first; the next pass reads the other session first.
	if result := run(); result.Deferred[heavy.ArchiveSessionID] != DeferredLookupAllowance || len(result.Published) != 0 {
		t.Fatalf("third pass result=%+v, want the heavy session read first", result)
	}
	if result := run(); len(result.Published) != 1 || result.Published[0] != unlucky.ArchiveSessionID {
		t.Fatalf("fourth pass result=%+v, want the session the heavy one held back published", result)
	}
}

// Two sessions whose lookups alone each need the whole allowance do not hold
// back for good a third that needs only part of it. Each pass reads first
// the waiting session the previous pass left unopened earliest in its queue,
// so the sessions waiting on the allowance take the first turn in rotation,
// and the third has its turn on the third pass that all three wait.
func TestTwoHeavySessionsDoNotTakeEveryTurnFromAThird(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	dir := t.TempDir()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	save := func(n int) archive.SessionRegistration {
		t.Helper()
		reg := registration(t, writeTranscript(t, dir, fmt.Sprintf("codex-%d.jsonl", n), codexTranscript))
		reg.ArchiveSessionID, reg.NativeSessionID = fmt.Sprintf("session-%d", n), fmt.Sprintf("native-%d", n)
		reg.Origin = archive.SessionOriginImport
		reg.SessionStartedAt, reg.RegisteredAt = start.Add(time.Duration(n)*time.Minute), start.Add(time.Duration(n)*time.Minute+time.Second)
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		if err := local.SaveRequest(reg.ArchiveSessionID, "stop", reg.RegisteredAt); err != nil {
			t.Fatal(err)
		}
		return reg
	}
	heavy, heavier, light := save(1), save(2), save(3)
	lookup := &allowanceLookup{}
	sources := allowanceSpendingSources{path: heavy.TranscriptPath, also: heavier.TranscriptPath, once: light.TranscriptPath, onceDone: &atomic.Bool{}, lookup: lookup}
	store := storagetest.NewMemoryStore()
	now := start.Add(time.Hour)
	// The first three passes each read a different session first (the oldest
	// on the first pass, then the waiter lane's), and each runs out. From
	// then on all three wait on the allowance.
	for pass := 1; pass <= 9; pass++ {
		now = now.Add(time.Hour)
		result, err := Run(t.Context(), local, store, Options{Sources: sources, Parsers: testParsers, MachineID: "m", CodexRollouts: lookup, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Published) == 1 && result.Published[0] == light.ArchiveSessionID {
			if pass != 6 {
				t.Fatalf("light session published on pass %d, want pass 6", pass)
			}
			return
		}
		if len(result.Published) != 0 {
			t.Fatalf("pass %d result=%+v, want nothing published but the light session", pass, result)
		}
	}
	t.Fatal("the light session was never read in nine passes: the two heavy sessions took every turn")
}

// Among the sessions waiting on the allowance, the ones the previous pass
// left unopened go in the order it left them, before the ones it read (a
// session that kept its clock without a rank, as after a status from an
// older pass, still goes before the one that ran out), the first of them in
// the waiter lane ahead of the sessions not waiting, and one whose wait the
// previous pass began after all of them, whatever its rank; and native children
// waiting for their parent stay behind every other requested Codex session,
// waiting on the allowance or not, as orderRequests put them, with no lane
// of their own. The sessions not waiting keep orderRequests' order (its
// live lane), wherever the previous pass left them.
func TestLookupAllowanceWaitersRotateAndChildrenWaitingForAParentStayLast(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	recent, old := now.Add(-time.Hour), now.Add(-MaxDeferral-time.Hour)
	reg := func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, Harness: archive.Harness{Name: "codex"}}
	}
	orphan := reg("orphan")
	orphan.NativeChild = true
	waitingOrphan := reg("waiting-orphan")
	waitingOrphan.NativeChild = true
	leftOrphan := reg("left-orphan")
	leftOrphan.NativeChild = true
	p := &pass{
		now:           now,
		registrations: []archive.SessionRegistration{reg("ran-out"), reg("left-unranked"), reg("left-second"), reg("left-first"), reg("expired-read"), reg("expired-left"), reg("live"), reg("left-joined"), reg("plain"), waitingOrphan, leftOrphan, orphan},
		requests:      map[string]state.Request{},
		priorDeferred: map[string]string{"ran-out": DeferredLookupAllowance, "waiting-orphan": DeferredLookupAllowance},
		priorDeferredSince: map[string]time.Time{
			"ran-out": recent, "left-unranked": recent, "left-second": recent, "left-first": recent, "expired-read": old, "expired-left": old, "waiting-orphan": recent, "left-orphan": recent, "left-joined": now.Add(-time.Minute),
		},
		priorScanAt:    now.Add(-time.Minute),
		priorUnreached: map[string]int{"left-first": 3, "left-second": 5, "expired-left": 4, "orphan": 6, "left-orphan": 7, "left-joined": 2, "plain": 0, "live": 1},
	}
	for _, r := range p.registrations {
		p.requests[r.ArchiveSessionID] = state.Request{Token: "t-" + r.ArchiveSessionID}
	}
	p.orderLookupAllowanceDeferralsLast(p.registrations)
	var got []string
	for _, r := range p.registrations {
		got = append(got, r.ArchiveSessionID)
	}
	want := []string{"left-first", "live", "plain", "left-second", "left-unranked", "left-joined", "ran-out", "expired-left", "expired-read", "orphan", "left-orphan", "waiting-orphan"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order=%v, want %v", got, want)
	}
}

// The waiter lane goes only to a session the previous pass left unopened: a
// session that ran out on it, or whose wait has lasted MaxDeferral, never
// goes ahead of the sessions not waiting, so with neither left unopened the
// pass starts with orderRequests' order.
func TestWaiterLaneTakesOnlyASessionLeftUnopened(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	recent, old := now.Add(-time.Hour), now.Add(-MaxDeferral-time.Hour)
	reg := func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, Harness: archive.Harness{Name: "codex"}}
	}
	p := &pass{
		now:                now,
		registrations:      []archive.SessionRegistration{reg("expired-left"), reg("ran-out"), reg("new"), reg("live")},
		requests:           map[string]state.Request{},
		priorDeferred:      map[string]string{"ran-out": DeferredLookupAllowance},
		priorDeferredSince: map[string]time.Time{"ran-out": recent, "expired-left": old},
		priorUnreached:     map[string]int{"expired-left": 0},
	}
	for _, r := range p.registrations {
		p.requests[r.ArchiveSessionID] = state.Request{Token: "t-" + r.ArchiveSessionID}
	}
	p.orderLookupAllowanceDeferralsLast(p.registrations)
	var got []string
	for _, r := range p.registrations {
		got = append(got, r.ArchiveSessionID)
	}
	want := []string{"new", "live", "ran-out", "expired-left"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("order=%v, want %v", got, want)
	}
}

// newSessionRunner registers imported Codex sessions with requests, each
// started a minute after the last, and runs collector passes over them.
type newSessionRunner struct {
	t      *testing.T
	local  *state.Store
	dir    string
	start  time.Time
	heavy  *sync.Map
	lookup *allowanceLookup
}

func (r newSessionRunner) save(n int, heavy bool) archive.SessionRegistration {
	r.t.Helper()
	reg := registration(r.t, writeTranscript(r.t, r.dir, fmt.Sprintf("codex-%d.jsonl", n), codexTranscript))
	reg.ArchiveSessionID, reg.NativeSessionID = fmt.Sprintf("session-%d", n), fmt.Sprintf("native-%d", n)
	reg.Origin = archive.SessionOriginImport
	reg.SessionStartedAt, reg.RegisteredAt = r.start.Add(time.Duration(n)*time.Minute), r.start.Add(time.Duration(n)*time.Minute+time.Second)
	if heavy {
		r.heavy.Store(reg.TranscriptPath, true)
	}
	if err := r.local.SaveRegistration(reg); err != nil {
		r.t.Fatal(err)
	}
	if err := r.local.SaveRequest(reg.ArchiveSessionID, "stop", reg.RegisteredAt); err != nil {
		r.t.Fatal(err)
	}
	return reg
}

// spendingSources serves the built-in sources, except that a transcript in
// heavy is read completely but uses up the rest of the lookup allowance, as
// a new session whose lookups take what is left does, and one in refused
// is refused as the real lookup refuses a call past the allowance, after
// spending it: those sessions' lookups alone need more than all of it. The
// transcript at once is refused that way the first time only.
type spendingSources struct {
	heavy    *sync.Map
	refused  *sync.Map
	once     string
	onceDone *atomic.Bool
	lookup   *allowanceLookup
	// opened, when set, counts the reads of each transcript in refused.
	opened *sync.Map
	// interrupt, when set, is called first on every read; an error it
	// returns is the read's.
	interrupt func(path string) error
}

func (s spendingSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := testSources.LookupSources(name)
	if !ok {
		return nil, nil, false
	}
	return spendingProvider{SourceProvider: p, sources: s}, f, true
}

type spendingProvider struct {
	agentapi.SourceProvider
	sources spendingSources
}

func (p spendingProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	pass, err := p.SourceProvider.OpenPass(ctx, env)
	if err != nil {
		return nil, err
	}
	return spendingPass{SourcePass: pass, sources: p.sources}, nil
}

type spendingPass struct {
	agentapi.SourcePass
	sources spendingSources
}

func (p spendingPass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	if p.sources.interrupt != nil {
		if err := p.sources.interrupt(ref.Path); err != nil {
			return nil, err
		}
	}
	if _, heavy := p.sources.heavy.Load(ref.Path); heavy {
		snapshot, err := p.SourcePass.Read(ctx, ref, limits)
		p.sources.lookup.spent.Store(true)
		return snapshot, err
	}
	_, refused := p.sources.refused.Load(ref.Path)
	if refused && p.sources.opened != nil {
		count, _ := p.sources.opened.LoadOrStore(ref.Path, new(atomic.Int64))
		count.(*atomic.Int64).Add(1)
	}
	if refused || (p.sources.once != "" && ref.Path == p.sources.once && !p.sources.onceDone.Swap(true)) {
		p.sources.lookup.spent.Store(true)
		return nil, lookupAllowanceSpent()
	}
	return p.SourcePass.Read(ctx, ref, limits)
}

// A Codex session waiting on the lookup allowance is still read when new
// Codex sessions, read before it as sessions not waiting, use up every
// pass's allowance: the waiter lane gives it the first turn with a fresh
// allowance. Without it the session was left unopened on every pass, never
// deferred, so it never failed either, and waited as queued for good.
func TestWaiterIsReadWhileNewCodexSessionsUseUpEveryPassAllowance(t *testing.T) {
	t.Parallel()
	lookup := &allowanceLookup{}
	r := newSessionRunner{t: t, local: newTestStore(t), dir: t.TempDir(), start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), heavy: &sync.Map{}, lookup: lookup}
	waiter := r.save(1, false)
	sources := spendingSources{heavy: r.heavy, refused: &sync.Map{}, once: waiter.TranscriptPath, onceDone: &atomic.Bool{}, lookup: lookup}
	store := storagetest.NewMemoryStore()
	now := r.start.Add(time.Hour)
	for pass := 1; now.Sub(r.start) <= MaxDeferral; pass++ {
		if pass > 1 {
			r.save(100+pass, true)
		}
		now = now.Add(2 * time.Hour)
		result, err := Run(t.Context(), r.local, store, Options{Sources: sources, Parsers: testParsers, MachineID: "m", CodexRollouts: lookup, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		if pass == 1 && result.Deferred[waiter.ArchiveSessionID] != DeferredLookupAllowance {
			t.Fatalf("first pass result=%+v, want the session deferred for the lookup allowance", result)
		}
		if pass > 1 && len(result.Published) == 0 {
			t.Fatalf("pass %d result=%+v, want the new session read too", pass, result)
		}
		if slices.Contains(result.Published, waiter.ArchiveSessionID) {
			return
		}
		if result.Errors[waiter.ArchiveSessionID] != nil {
			return
		}
	}
	t.Fatal("the waiting session was neither published nor reported failed within MaxDeferral")
}

// Two Codex sessions whose lookups alone each need more than the whole
// allowance, waiting while new Codex sessions arrive on every pass and each
// use up what is left of it: the waiters queue, so the heavy sessions are
// opened in turn and the new sessions are all published, none failing; and
// no session is left unopened past MaxDeferral without being reported
// failed, whether the pass opened it (and deferral expiry failed it) or
// left it unopened (and the skip failed it); after that, each pass fails it
// again on an attempt on its backoff schedule or holds it for the next one.
func TestHeavyWaitersAndNewArrivalsLeaveNoSessionUnreportedPastMaxDeferral(t *testing.T) {
	t.Parallel()
	lookup := &allowanceLookup{}
	r := newSessionRunner{t: t, local: newTestStore(t), dir: t.TempDir(), start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), heavy: &sync.Map{}, lookup: lookup}
	refused := &sync.Map{}
	first, second := r.save(1, false), r.save(2, false)
	refused.Store(first.TranscriptPath, true)
	refused.Store(second.TranscriptPath, true)
	sources := spendingSources{heavy: r.heavy, refused: refused, lookup: lookup}
	store := storagetest.NewMemoryStore()
	// waitingSince is when each session was first left pending.
	waitingSince := map[string]time.Time{}
	var arrived []string
	opened := map[string]int{}
	now := r.start.Add(time.Hour)
	for pass := 1; pass <= 48; pass++ {
		if pass > 1 {
			arrived = append(arrived, r.save(100+pass, true).ArchiveSessionID)
		}
		now = now.Add(time.Hour)
		result, err := Run(t.Context(), r.local, store, Options{Sources: sources, Parsers: testParsers, MachineID: "m", CodexRollouts: lookup, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		for id := range result.Deferred {
			opened[id]++
		}
		for _, id := range arrived {
			if result.Errors[id] != nil {
				t.Fatalf("pass %d: new session %s failed: %v", pass, id, result.Errors[id])
			}
		}
		for _, id := range result.Published {
			delete(waitingSince, id)
		}
		for _, id := range []string{first.ArchiveSessionID, second.ArchiveSessionID} {
			if _, ok := waitingSince[id]; !ok {
				waitingSince[id] = now
			}
		}
		for id := range result.Unreached {
			if _, ok := waitingSince[id]; !ok {
				waitingSince[id] = now
			}
		}
		for id, since := range waitingSince {
			if now.Sub(since) <= MaxDeferral+time.Hour {
				continue
			}
			if _, unreached := result.Unreached[id]; unreached && result.Errors[id] == nil {
				t.Fatalf("pass %d: %s left unopened %v after it began waiting, not reported failed; result=%+v", pass, id, now.Sub(since), result)
			}
			if _, held := result.RetryLater[id]; !held && result.Errors[id] == nil && !slices.Contains(result.Published, id) {
				t.Fatalf("pass %d: %s waiting %v, neither failed nor held for a retry; result=%+v", pass, id, now.Sub(since), result)
			}
			if _, deferred := result.Deferred[id]; deferred {
				t.Fatalf("pass %d: %s deferred %v after it began waiting, want it failed", pass, id, now.Sub(since))
			}
		}
	}
	for _, id := range []string{first.ArchiveSessionID, second.ArchiveSessionID} {
		if _, ok := waitingSince[id]; !ok {
			t.Fatalf("%s published, want it failing: its lookups need more than the allowance", id)
		}
		// Each is read on its first pass and then on its turns in the
		// waiter lane until its wait reaches MaxDeferral.
		if opened[id] < 4 {
			t.Fatalf("%s opened on %d passes, want it to take the waiter lane in turn", id, opened[id])
		}
	}
	// The queue drains: every session that arrived in the first 30 passes is
	// published by the 48th. Once the heavy sessions fail, each attempt on
	// their backoff schedule takes a pass's allowance, but the attempts
	// grow rarer.
	for _, id := range arrived[:29] {
		if _, waiting := waitingSince[id]; waiting {
			t.Fatalf("new session %s still not published after 48 passes", id)
		}
	}
}

// With AcceptSession filtering sessions out of the pass, the waiter
// rotation and the backoff schedule act on the sessions the pass accepts,
// at their places in the slice the pass walks, and never touch the rejected
// ones: a stop after a publication leaves the accepted heavy session with
// its clock; past MaxDeferral it fails, is then held until its attempt, and
// a pass that stops after reading an earlier session, before reaching the
// heavy one with its attempt due, holds it still due, so the next pass
// makes the attempt.
func TestAcceptedLookupQueueRotatesWithoutChangingRejectedWork(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	dir := t.TempDir()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	save := func(n int) archive.SessionRegistration {
		t.Helper()
		reg := registration(t, writeTranscript(t, dir, fmt.Sprintf("codex-%d.jsonl", n), codexTranscript))
		reg.ArchiveSessionID, reg.NativeSessionID = fmt.Sprintf("session-%d", n), fmt.Sprintf("native-%d", n)
		reg.Origin = archive.SessionOriginImport
		reg.SessionStartedAt, reg.RegisteredAt = start.Add(time.Duration(n)*time.Minute), start.Add(time.Duration(n)*time.Minute+time.Second)
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		if err := local.SaveRequest(reg.ArchiveSessionID, "stop", reg.RegisteredAt); err != nil {
			t.Fatal(err)
		}
		return reg
	}
	heavy := save(1)
	rejected := save(9)
	rejected.NativeChild, rejected.ParentNativeSessionID = true, "absent-parent"
	if err := local.SaveRegistration(rejected); err != nil {
		t.Fatal(err)
	}
	rejectedTop := save(10)
	originals := map[string][]byte{}
	for _, reg := range []archive.SessionRegistration{rejected, rejectedTop} {
		request, found, err := local.LoadRequest(reg.ArchiveSessionID)
		if err != nil || !found || request.Token == "" {
			t.Fatalf("rejected request control: %+v %t %v", request, found, err)
		}
		for _, kind := range []string{"requests", "registrations"} {
			path := filepath.Join(local.Home(), kind, reg.ArchiveSessionID+".json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			originals[path] = raw
		}
	}
	lookup := &allowanceLookup{}
	stopAfterPublish := false
	published := false
	store := storagetest.NewMemoryStore()
	run := func(at time.Time) Result {
		t.Helper()
		result, err := Run(t.Context(), local, store, Options{Sources: allowanceSpendingSources{path: heavy.TranscriptPath, lookup: lookup}, Parsers: testParsers, MachineID: "m", CodexRollouts: lookup,
			AcceptSession: func(reg archive.SessionRegistration) bool {
				return reg.ArchiveSessionID != rejected.ArchiveSessionID && reg.ArchiveSessionID != rejectedTop.ArchiveSessionID
			},
			Progress: func(progress Progress) { published = published || progress.Published },
			Stop:     func() bool { return stopAfterPublish && published }, Now: func() time.Time { return at }})
		if err != nil {
			t.Fatal(err)
		}
		for path, want := range originals {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(want) {
				t.Fatalf("rejected bytes changed at %s: %v", path, err)
			}
		}
		for _, reg := range []archive.SessionRegistration{rejected, rejectedTop} {
			_, held := result.RetryLater[reg.ArchiveSessionID]
			if _, found := result.Unreached[reg.ArchiveSessionID]; found || held || result.Errors[reg.ArchiveSessionID] != nil || result.Deferred[reg.ArchiveSessionID] != "" {
				t.Fatalf("rejected execution work: %+v", result)
			}
		}
		return result
	}
	now := start.Add(time.Hour)

	// The sessions are imported, so the pass reads them oldest first with no
	// slot reserved for a live session: the heavy session, the oldest, is
	// read first; it spends the allowance and the session after it is left
	// unopened.
	later := save(2)
	result := run(now)
	if result.Deferred[heavy.ArchiveSessionID] != DeferredLookupAllowance || len(result.Published) != 0 || len(result.Unreached) != 1 || result.Unreached[later.ArchiveSessionID] != 0 {
		t.Fatalf("first pass result=%+v, want the heavy session deferred for the lookup allowance, the later one left unopened, and nothing published", result)
	}

	status, err := local.LoadStatus()
	if err != nil || !status.DeferredSince[heavy.ArchiveSessionID].Equal(now) || !status.DeferredSince[later.ArchiveSessionID].Equal(now) || status.PendingCount != 2 {
		t.Fatalf("first pass clocks/pending: %+v %v", status, err)
	}
	stopAfterPublish, published = true, false
	// The next pass gives the allowance to the later session first.
	result = run(now.Add(time.Hour))
	if len(result.Published) != 1 || result.Published[0] != later.ArchiveSessionID || len(result.Errors) != 0 {
		t.Fatalf("second pass result=%+v, want the later Codex session published", result)
	}
	status, err = local.LoadStatus()
	if err != nil || len(result.Unreached) != 1 || result.Unreached[heavy.ArchiveSessionID] != 0 || !status.DeferredSince[heavy.ArchiveSessionID].Equal(now) || status.PendingCount != 1 {
		t.Fatalf("stopped accepted suffix/clocks: %+v %+v %v", result, status, err)
	}
	request, found, err := local.LoadRequest(heavy.ArchiveSessionID)
	if err != nil || !found || request.Token == "" {
		t.Fatalf("stopped heavy request: %+v %t %v", request, found, err)
	}
	stopAfterPublish = false

	// Past MaxDeferral the heavy session fails instead of being deferred,
	// and still waits behind a newer Codex session on this pass. On the next
	// it is held for its next attempt on the backoff schedule, with no new
	// failure.
	newer := save(3)
	result = run(now.Add(MaxDeferral + time.Hour))
	if len(result.Published) != 1 || result.Published[0] != newer.ArchiveSessionID || result.Errors[heavy.ArchiveSessionID] == nil {
		t.Fatalf("pass past MaxDeferral result=%+v, want the newer session published and the heavy session failed", result)
	}
	attempt := now.Add(MaxDeferral + 3*time.Hour)
	newest := save(4)
	result = run(now.Add(MaxDeferral + 2*time.Hour))
	if len(result.Published) != 1 || result.Published[0] != newest.ArchiveSessionID || len(result.Errors) != 0 || !result.RetryLater[heavy.ArchiveSessionID].Equal(attempt) {
		t.Fatalf("pass after the heavy session failed result=%+v, want the newest session published and the heavy session held until %v", result, attempt)
	}

	// At its attempt, an older Claude Code session is read ahead of it (the
	// retry lane is the first of the Codex sessions' slots), and the pass
	// stops after publishing that one: the heavy session, one place on in
	// the accepted slice (but not in the full census, where the rejected
	// session-10 follows it), is held with its attempt still due rather
	// than failed and moved on along its schedule.
	first := registration(t, writeTranscript(t, dir, "claude-90.jsonl", claudePromptLine+"\n"))
	first.ArchiveSessionID, first.NativeSessionID, first.Harness.Name = "session-90", "native-claude", "claude"
	first.Origin, first.SessionStartedAt, first.RegisteredAt = archive.SessionOriginImport, start, start.Add(time.Second)
	if err := local.SaveRegistration(first); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(first.ArchiveSessionID, "stop", first.RegisteredAt); err != nil {
		t.Fatal(err)
	}
	stopAfterPublish, published = true, false
	result = run(attempt)
	if len(result.Published) != 1 || result.Published[0] != first.ArchiveSessionID || len(result.Errors) != 0 || len(result.Unreached) != 0 {
		t.Fatalf("stopped attempt pass result=%+v, want only the older session published and no failure", result)
	}
	if at, held := result.RetryLater[heavy.ArchiveSessionID]; !held || at.After(attempt) {
		t.Fatalf("stopped attempt pass retry later=%v, want the heavy session held and still due", result.RetryLater)
	}
	stopAfterPublish = false
	result = run(attempt.Add(10 * time.Minute))
	if result.Errors[heavy.ArchiveSessionID] == nil {
		t.Fatalf("pass after the stop result=%+v, want the attempt made and failed", result)
	}
	status, err = local.LoadStatus()
	if err != nil || !status.NextRetryAt[heavy.ArchiveSessionID].Equal(now.Add(MaxDeferral+7*time.Hour)) {
		t.Fatalf("after the attempt next=%v %v, want the attempt after %v", status.NextRetryAt, err, attempt)
	}
}

// The backoff schedule: since + MaxDeferral + 1h, then gaps of 2h, 4h and so
// on, doubling up to LookupRetryMaxGap and staying there; each call gives
// the first attempt strictly after its argument.
func TestNextLookupRetryDoublesUpToAWeek(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := since.Add(MaxDeferral)
	var want []time.Time
	at, gap := expiry.Add(time.Hour), time.Hour
	for range 14 {
		want = append(want, at)
		gap = min(2*gap, LookupRetryMaxGap)
		at = at.Add(gap)
	}
	// Gaps 1h, 2h, ... 128h, then 168h from there on.
	if got := want[1].Sub(want[0]); got != 2*time.Hour {
		t.Fatalf("second gap = %v, want 2h", got)
	}
	if got := want[8].Sub(want[7]); got != LookupRetryMaxGap {
		t.Fatalf("ninth gap = %v, want %v", got, LookupRetryMaxGap)
	}
	if got := nextLookupRetry(since, since); !got.Equal(want[0]) {
		t.Fatalf("next after the clock began = %v, want %v", got, want[0])
	}
	previous := expiry
	for i, attempt := range want {
		for _, after := range []time.Time{previous, attempt.Add(-time.Nanosecond)} {
			if got := nextLookupRetry(since, after); !got.Equal(attempt) {
				t.Fatalf("next after %v = %v, want attempt %d at %v", after, got, i, attempt)
			}
		}
		previous = attempt
	}
}

// The schedule as the collector runs it: a Codex session whose lookups need
// more than the whole allowance fails when its wait reaches MaxDeferral, and
// from then on is opened only on the passes at its backoff attempts (1h,
// 3h, 7h, ... after that, the gap capped at a week), each of which fails it
// again, over four weeks of passes. On every other pass it is held
// unopened, with no new failure, and the pass reads the other Codex
// sessions as usual. The passes are a sample of the hourly ones: some
// during its wait, each attempt with the hour before and after it, and
// the middle of each hold. An attempt the schedule put in a skipped hour
// would be due on the next sampled pass, which then reads the session.
func TestHeavySessionIsRetriedOnTheBackoffSchedule(t *testing.T) {
	t.Parallel()
	lookup := &allowanceLookup{}
	r := newSessionRunner{t: t, local: newTestStore(t), dir: t.TempDir(), start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), heavy: &sync.Map{}, lookup: lookup}
	heavy := r.save(1, false)
	id := heavy.ArchiveSessionID
	refused, opened := &sync.Map{}, &sync.Map{}
	refused.Store(heavy.TranscriptPath, true)
	sources := spendingSources{heavy: &sync.Map{}, refused: refused, lookup: lookup, opened: opened}
	store := storagetest.NewMemoryStore()
	reads := func() int64 {
		count, ok := opened.Load(heavy.TranscriptPath)
		if !ok {
			return 0
		}
		return count.(*atomic.Int64).Load()
	}
	clock := r.start.Add(time.Hour) // the first pass defers it, starting its clock
	end := clock.Add(29 * 24 * time.Hour)
	// Every attempt, in hours after the expiry: the gap doubles from 1h to
	// 128h, then stays at a week. The last is after the end, for the next
	// attempt after the last pass.
	attempts, scheduled := map[time.Time]bool{}, 0
	for _, hours := range []int{1, 3, 7, 15, 31, 63, 127, 255, 423, 591, 759} {
		at := clock.Add(MaxDeferral + time.Duration(hours)*time.Hour)
		if at.Before(end) {
			scheduled++
		}
		attempts[at] = true
	}
	// The hourly passes sampled: four during the wait, the expiry, each
	// attempt with the hour before and after it, the middle of each hold,
	// and the last hour.
	sample := map[time.Time]bool{clock: true, clock.Add(time.Hour): true, clock.Add(MaxDeferral / 2): true, clock.Add(MaxDeferral - time.Hour): true, end.Add(-time.Hour): true}
	previous := clock.Add(MaxDeferral)
	for _, at := range slices.SortedFunc(maps.Keys(attempts), time.Time.Compare) {
		sample[previous], sample[previous.Add(time.Hour)] = true, true
		sample[previous.Add(at.Sub(previous)/2).Truncate(time.Hour)] = true
		sample[at.Add(-time.Hour)] = true
		previous = at
	}
	var passes []time.Time
	for _, at := range slices.SortedFunc(maps.Keys(sample), time.Time.Compare) {
		if at.Before(end) {
			passes = append(passes, at)
		}
	}
	var light []string
	failures := 0
	for pass, now := range passes {
		light = append(light, r.save(1000+pass, false).ArchiveSessionID)
		before := reads()
		result, err := Run(t.Context(), r.local, store, Options{Sources: sources, Parsers: testParsers, MachineID: "m", CodexRollouts: lookup, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		read := reads() > before
		switch waited := now.Sub(clock); {
		case waited < MaxDeferral:
			if !read || result.Deferred[id] != DeferredLookupAllowance {
				t.Fatalf("pass %d (%v waited): read=%v result=%+v, want it read and deferred", pass, waited, read, result)
			}
		case waited == MaxDeferral || attempts[now]:
			failures++
			if !read || result.Errors[id] == nil || !strings.Contains(result.Errors[id].Error(), DeferralExpiredProblem) {
				t.Fatalf("pass %d (%v waited): read=%v result=%+v, want an attempt that fails", pass, waited, read, result)
			}
			if _, held := result.RetryLater[id]; held {
				t.Fatalf("pass %d: attempted and held both", pass)
			}
		default:
			if read || len(result.Errors) != 0 {
				t.Fatalf("pass %d (%v waited): read=%v errors=%v, want it held unopened with no new failure", pass, waited, read, result.Errors)
			}
			if next := nextLookupRetry(clock, now); !attempts[next] || !result.RetryLater[id].Equal(next) {
				t.Fatalf("pass %d: retry later=%v, next attempt %v, want the next attempt on the schedule", pass, result.RetryLater, next)
			}
			// The other Codex sessions are read as usual: none is left.
			if len(result.Unreached) != 0 {
				t.Fatalf("pass %d: unreached=%v, want every other Codex session read", pass, result.Unreached)
			}
		}
		status, err := r.local.LoadStatus()
		if err != nil {
			t.Fatal(err)
		}
		if waited := now.Sub(clock); waited >= MaxDeferral && (!status.DeferredSince[id].Equal(clock) || !status.NextRetryAt[id].Equal(nextLookupRetry(clock, now))) {
			t.Fatalf("pass %d: since=%v next=%v, want the clock from %v and the next attempt after now", pass, status.DeferredSince, status.NextRetryAt, clock)
		}
	}
	// The expiry, then 10 attempts in the 28 days after it: 1, 3, 7, 15,
	// 31, 63, 127, 255, 423 and 591 hours after it.
	if failures != 11 || scheduled != 10 {
		t.Fatalf("failures=%d attempts=%d, want 11 and 10", failures, scheduled)
	}
	for _, lightID := range light {
		if published, err := r.local.LoadPublishedState(lightID); err != nil || published == nil {
			t.Fatalf("light session %s not published: %v", lightID, err)
		}
	}
}

// On a pass with an attempt due, the session on the backoff schedule with
// the oldest clock takes the retry lane, ahead of the waiter lane (the
// head of the sessions left unopened) and of every other requested Codex
// session; another due session and one with no attempt due are held, the
// one due still due on the next pass.
func TestDueRetryTakesTheLaneAheadOfTheWaiterLane(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	priorScan := now.Add(-15 * time.Minute)
	// Two clocks whose schedules put an attempt in (priorScan, now], and
	// one whose next attempt is later.
	dueOld, dueNew := now.Add(-MaxDeferral-time.Hour-5*time.Minute), now.Add(-MaxDeferral-time.Hour-time.Minute)
	later := now.Add(-MaxDeferral - 2*time.Hour)
	reg := func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, Harness: archive.Harness{Name: "codex"}}
	}
	p := &pass{
		now:                now,
		registrations:      []archive.SessionRegistration{reg("plain"), reg("later"), reg("due-new"), reg("left"), reg("due-old")},
		requests:           map[string]state.Request{},
		priorDeferredSince: map[string]time.Time{"left": now.Add(-time.Hour), "due-old": dueOld, "due-new": dueNew, "later": later},
		priorUnreached:     map[string]int{"left": 0},
		priorScanAt:        priorScan,
		priorRetryAt:       map[string]time.Time{"due-old": now.Add(-5 * time.Minute), "due-new": now.Add(-time.Minute), "later": now.Add(time.Hour)},
	}
	for _, r := range p.registrations {
		p.requests[r.ArchiveSessionID] = state.Request{Token: "t-" + r.ArchiveSessionID}
	}
	p.orderLookupAllowanceDeferralsLast(p.registrations)
	var got []string
	for _, r := range p.registrations {
		got = append(got, r.ArchiveSessionID)
	}
	if got[0] != "due-old" || got[1] != "left" || got[2] != "plain" {
		t.Fatalf("order=%v, want due-old in the retry lane, then left in the waiter lane, then plain", got)
	}
	if _, held := p.lookupRetryHeld["due-old"]; held {
		t.Fatal("the session in the retry lane is held")
	}
	dueAt, dueHeld := p.lookupRetryHeld["due-new"]
	if !dueHeld || dueAt.After(now) {
		t.Fatalf("due-new held=%v at %v, want held and still due", dueHeld, dueAt)
	}
	if at := p.lookupRetryHeld["later"]; !at.Equal(nextLookupRetry(later, now)) || !at.After(now) {
		t.Fatalf("later held until %v, want %v", at, nextLookupRetry(later, now))
	}
	// The next pass, 15 minutes on, finds due-new due still, though no
	// attempt of its schedule falls in between.
	next := &pass{now: now.Add(15 * time.Minute), priorScanAt: now, priorRetryAt: map[string]time.Time{"due-new": dueAt}}
	if due, _ := next.lookupRetryDue("due-new", dueNew); !due {
		t.Fatal("due-new not due on the next pass after another session took the lane")
	}
	// A previous pass of unknown time makes an attempt due.
	unknown := &pass{now: now, priorRetryAt: map[string]time.Time{"later": now.Add(time.Hour)}}
	if due, _ := unknown.lookupRetryDue("later", later); !due {
		t.Fatal("an attempt is not due after a pass of unknown time")
	}
}

// A pass that holds a session on the backoff schedule records no new
// failure for it, but its issue and its clock stay in status, with its
// next attempt, and it stays pending; and an attempt that reads it
// publishes it and clears its clock, its next attempt and its issue.
func TestHeldRetryKeepsItsIssueAndASuccessfulAttemptClearsIt(t *testing.T) {
	t.Parallel()
	lookup := &allowanceLookup{}
	r := newSessionRunner{t: t, local: newTestStore(t), dir: t.TempDir(), start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), heavy: &sync.Map{}, lookup: lookup}
	heavy := r.save(1, false)
	id := heavy.ArchiveSessionID
	refused := &sync.Map{}
	refused.Store(heavy.TranscriptPath, true)
	sources := spendingSources{heavy: &sync.Map{}, refused: refused, lookup: lookup}
	store := storagetest.NewMemoryStore()
	run := func(now time.Time) (Result, state.Status) {
		t.Helper()
		result, err := Run(t.Context(), r.local, store, Options{Sources: sources, Parsers: testParsers, MachineID: "m", CodexRollouts: lookup, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		status, err := r.local.LoadStatus()
		if err != nil {
			t.Fatal(err)
		}
		return result, status
	}
	clock := r.start.Add(time.Hour)
	if result, _ := run(clock); result.Deferred[id] != DeferredLookupAllowance {
		t.Fatalf("first pass result=%+v, want it deferred", result)
	}
	firstAttempt := clock.Add(MaxDeferral + time.Hour)
	result, status := run(clock.Add(MaxDeferral))
	if result.Errors[id] == nil || !status.NextRetryAt[id].Equal(firstAttempt) {
		t.Fatalf("expiry pass errors=%v next=%v, want it failed and its first attempt an hour on", result.Errors, status.NextRetryAt)
	}
	// What internal/cli records over the collector's status for the failure.
	status.SessionIssues = map[string]string{id: "capture_failed"}
	if err := r.local.SaveStatus(status); err != nil {
		t.Fatal(err)
	}
	result, status = run(clock.Add(MaxDeferral + 30*time.Minute))
	if len(result.Errors) != 0 || !result.RetryLater[id].Equal(firstAttempt) {
		t.Fatalf("held pass result=%+v, want no failure and the session held until its attempt", result)
	}
	if status.SessionIssues[id] != "capture_failed" || !status.NextRetryAt[id].Equal(firstAttempt) || !status.DeferredSince[id].Equal(clock) || status.PendingCount != 1 || len(status.LastErrors) != 0 {
		t.Fatalf("held pass status=%+v, want its issue, next attempt, clock and pending count carried, with no new problem", status)
	}
	// The source can be read now; the attempt publishes it.
	refused.Delete(heavy.TranscriptPath)
	result, status = run(firstAttempt)
	if !slices.Contains(result.Published, id) || len(result.Errors) != 0 || len(result.RetryLater) != 0 {
		t.Fatalf("attempt result=%+v, want it published", result)
	}
	if _, running := status.DeferredSince[id]; running || len(status.NextRetryAt) != 0 || len(status.SessionIssues) != 0 || status.PendingCount != 0 {
		t.Fatalf("attempt status=%+v, want its clock, next attempt and issue cleared", status)
	}
}

// A requested Codex session waiting on the allowance that a pass leaves
// unopened once its wait has lasted MaxDeferral fails and goes on the
// backoff schedule; a session with a clock from another deferral, left
// unopened by an early end, fails without it, as on every pass.
func TestSessionLeftUnopenedPastMaxDeferralGoesOnTheSchedule(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-MaxDeferral - 10*time.Minute)
	reg := func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{ArchiveSessionID: id, Harness: archive.Harness{Name: "codex"}}
	}
	p := &pass{
		now:                now,
		registrations:      []archive.SessionRegistration{reg("waiter"), reg("changed")},
		requests:           map[string]state.Request{"waiter": {Token: "t"}, "changed": {Token: "t"}},
		priorDeferredSince: map[string]time.Time{"waiter": old, "changed": old},
		priorUnreached:     map[string]int{"waiter": 0},
		result:             Result{Errors: map[string]error{}},
	}
	p.orderLookupAllowanceDeferralsLast(p.registrations)
	p.leaveForNextPass(p.registrations)
	for _, id := range []string{"waiter", "changed"} {
		if err := p.result.Errors[id]; err == nil || !errors.Is(err, errLeftUnopened) {
			t.Fatalf("%s error=%v, want it failed for being left unopened", id, err)
		}
	}
	if at, ok := p.retryAt["waiter"]; !ok || !at.Equal(nextLookupRetry(old, now)) {
		t.Fatalf("waiter next attempt=%v, want %v", p.retryAt, nextLookupRetry(old, now))
	}
	if _, ok := p.retryAt["changed"]; ok {
		t.Fatal("a session not waiting on the allowance went on its backoff schedule")
	}
}

// retryRun runs one collector pass at now, with the pass's context and Stop
// as given, and returns its result and the status it saved.
type retryRun func(ctx context.Context, now time.Time, stop func() bool) (Result, state.Status)

// expiredRetrySession runs passes until a Codex session whose lookups need
// more than the whole allowance has failed at MaxDeferral and is on the
// backoff schedule, and returns a runner, its ID and its first attempt.
// The runner reads through *sources, which the caller may change.
func expiredRetrySession(t *testing.T, sources *spendingSources) (retryRun, string, time.Time) {
	t.Helper()
	lookup := &allowanceLookup{}
	r := newSessionRunner{t: t, local: newTestStore(t), dir: t.TempDir(), start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), heavy: &sync.Map{}, lookup: lookup}
	heavy := r.save(1, false)
	refused := &sync.Map{}
	refused.Store(heavy.TranscriptPath, true)
	*sources = spendingSources{heavy: &sync.Map{}, refused: refused, lookup: lookup}
	store := storagetest.NewMemoryStore()
	run := func(ctx context.Context, now time.Time, stop func() bool) (Result, state.Status) {
		t.Helper()
		result, err := Run(ctx, r.local, store, Options{Sources: *sources, Parsers: testParsers, MachineID: "m", CodexRollouts: lookup, Now: func() time.Time { return now }, Stop: stop})
		if err != nil {
			t.Fatal(err)
		}
		status, err := r.local.LoadStatus()
		if err != nil {
			t.Fatal(err)
		}
		return result, status
	}
	clock := r.start.Add(time.Hour)
	id := heavy.ArchiveSessionID
	if result, _ := run(t.Context(), clock, nil); result.Deferred[id] != DeferredLookupAllowance {
		t.Fatalf("first pass result=%+v, want it deferred", result)
	}
	firstAttempt := clock.Add(MaxDeferral + time.Hour)
	result, status := run(t.Context(), clock.Add(MaxDeferral), nil)
	if result.Errors[id] == nil || !status.NextRetryAt[id].Equal(firstAttempt) {
		t.Fatalf("expiry pass errors=%v next=%v, want it failed and its first attempt an hour on", result.Errors, status.NextRetryAt)
	}
	// What internal/cli records over the collector's status for the failure.
	status.SessionIssues = map[string]string{id: "capture_failed"}
	if err := r.local.SaveStatus(status); err != nil {
		t.Fatal(err)
	}
	return run, id, firstAttempt
}

// A pass that ends (Stop or its deadline) with a due attempt in flight, or
// before it reaches the session at all, makes no attempt: it records no
// failure for the session, keeps its issue and lists it as held, and the
// attempt stays due, so the next pass makes it rather than waiting for the
// next time on the schedule.
func TestAPassEndingBeforeItsAttemptCompletesKeepsTheAttemptDue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// pass runs the pass at the attempt that ends early.
		pass func(t *testing.T, run retryRun, sources *spendingSources, at time.Time) (Result, state.Status)
	}{
		{"stopped before reaching it", func(t *testing.T, run retryRun, _ *spendingSources, at time.Time) (Result, state.Status) {
			t.Helper()
			return run(t.Context(), at, func() bool { return true })
		}},
		{"deadline with the attempt in flight", func(t *testing.T, run retryRun, sources *spendingSources, at time.Time) (Result, state.Status) {
			t.Helper()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sources.interrupt = func(string) error {
				cancel()
				return ctx.Err()
			}
			defer func() { sources.interrupt = nil }()
			return run(ctx, at, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sources spendingSources
			run, id, attempt := expiredRetrySession(t, &sources)
			result, status := tc.pass(t, run, &sources, attempt)
			if len(result.Errors) != 0 {
				t.Fatalf("ended pass errors=%v, want no failure for an attempt it did not make", result.Errors)
			}
			if at, held := result.RetryLater[id]; !held || at.After(attempt) {
				t.Fatalf("ended pass retry later=%v, want the session held and still due", result.RetryLater)
			}
			if status.SessionIssues[id] != "capture_failed" || status.NextRetryAt[id].After(status.LastScanAt) {
				t.Fatalf("ended pass status issues=%v next=%v last scan=%v, want its issue kept and its attempt due", status.SessionIssues, status.NextRetryAt, status.LastScanAt)
			}
			// The next pass, ten minutes on, makes the attempt.
			result, _ = run(t.Context(), attempt.Add(10*time.Minute), nil)
			if result.Errors[id] == nil {
				t.Fatalf("next pass result=%+v, want the attempt made and failed", result)
			}
		})
	}
}

// The next attempt is after its argument and on the schedule however far
// apart the clock and the argument are: a clock read as zero, or one
// centuries back, still gives an attempt within a capped gap after it.
func TestNextLookupRetryHandlesClocksFarApart(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, since := range []time.Time{{}, now.AddDate(-300, 0, 0), now.AddDate(-200, 0, 0), now.Add(-30 * 24 * time.Hour)} {
		got := nextLookupRetry(since, now)
		if !got.After(now) || got.Sub(now) > LookupRetryMaxGap {
			t.Fatalf("since %v: next attempt %v, want one within %v after %v", since, got, LookupRetryMaxGap, now)
		}
		// On the capped part of the schedule: a whole number of gaps after
		// the eighth doubling's attempt (since + MaxDeferral + 255h + 168h).
		if steps := got.Sub(since.Add(MaxDeferral+255*time.Hour)) % LookupRetryMaxGap; since.Year() > 1800 && steps != 0 {
			t.Fatalf("since %v: next attempt %v off the schedule by %v", since, got, steps)
		}
	}
	// A clock ahead of after (stepped back) gives its first attempt.
	if ahead := now.AddDate(5, 0, 0); !nextLookupRetry(ahead, now).Equal(ahead.Add(MaxDeferral + time.Hour)) {
		t.Fatalf("clock ahead: next = %v", nextLookupRetry(ahead, now))
	}
}
