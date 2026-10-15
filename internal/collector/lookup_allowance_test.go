package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	// and still waits behind a newer Codex session on this pass and the next.
	newer := save(3)
	result = run(now.Add(MaxDeferral + time.Hour))
	if len(result.Published) != 1 || result.Published[0] != newer.ArchiveSessionID || result.Errors[heavy.ArchiveSessionID] == nil {
		t.Fatalf("pass past MaxDeferral result=%+v, want the newer session published and the heavy session failed", result)
	}
	newest := save(4)
	result = run(now.Add(MaxDeferral + 2*time.Hour))
	if len(result.Published) != 1 || result.Published[0] != newest.ArchiveSessionID || result.Errors[heavy.ArchiveSessionID] == nil {
		t.Fatalf("pass after the heavy session failed result=%+v, want the newest session published and the heavy session failed", result)
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
	p.orderLookupAllowanceDeferralsLast()
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
	p.orderLookupAllowanceDeferralsLast()
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
	p.orderLookupAllowanceDeferralsLast()
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
	heavy, refused *sync.Map
	once           string
	onceDone       *atomic.Bool
	lookup         *allowanceLookup
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
	if _, heavy := p.sources.heavy.Load(ref.Path); heavy {
		snapshot, err := p.SourcePass.Read(ctx, ref, limits)
		p.sources.lookup.spent.Store(true)
		return snapshot, err
	}
	_, refused := p.sources.refused.Load(ref.Path)
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
		for _, id := range result.Published {
			if id == waiter.ArchiveSessionID {
				return
			}
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
// left it unopened (and the skip failed it).
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
	for pass := 1; pass <= 40; pass++ {
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
	// published by the 40th.
	for _, id := range arrived[:29] {
		if _, waiting := waitingSince[id]; waiting {
			t.Fatalf("new session %s still not published after 40 passes", id)
		}
	}
}
