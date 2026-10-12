package collector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// keptTranscript is a Codex transcript whose user messages, all of which the
// filter keeps, come to about n bytes.
func keptTranscript(n int) string {
	var b strings.Builder
	b.WriteString(`{"type":"turn_context","model":"gpt-test"}` + "\n")
	text := strings.Repeat("kept words ", 6000)
	for b.Len() < n {
		b.WriteString(`{"type":"response_item","timestamp":"2026-01-01T00:01:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}}` + "\n")
	}
	return b.String()
}

// TestSessionOverItsReadShareIsANamedGapOnce: a session that needs more than
// one session's share of a pass is recorded as a size-limit gap naming that
// limit, once: the next pass skips it without reading it, and the pass's
// status names the limit. It is not an error every pass.
//
// Not parallel: it lowers sessionReadCeiling and counts transcriptFilters.
func TestSessionOverItsReadShareIsANamedGapOnce(t *testing.T) {
	previous := sessionReadCeiling
	sessionReadCeiling = 36 << 20
	t.Cleanup(func() { sessionReadCeiling = previous })
	path := writeTranscript(t, t.TempDir(), "large.jsonl", keptTranscript(40<<20))
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m"}
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 0 || len(result.Published) != 0 || len(result.Deferred) != 0 {
		t.Fatalf("over its share must be a recorded gap: %#v %v", result, result.Errors)
	}
	if reason, blocked, err := local.LoadBlocked("session-1"); err != nil || !blocked || reason != state.BlockedReasonTranscriptTooLarge {
		t.Fatalf("blocked=%v reason=%q err=%v", blocked, reason, err)
	}
	signature, _, err := local.LoadScanSignature("session-1")
	if err != nil || signature.SizeLimit == nil || signature.SizeLimit.Kind != state.SizeLimitReadBudget || signature.SizeLimit.LimitBytes != 36<<20 || signature.SizeLimit.SizeBytes != 0 {
		t.Fatalf("gap does not name the read limit: %+v err=%v", signature.SizeLimit, err)
	}
	status, err := local.LoadStatus()
	if err != nil || len(status.LastErrors) != 1 || !strings.Contains(status.LastErrors[0], "(per-session read memory 36 MiB)") || !IsSizeLimitProblem(status.LastErrors[0]) {
		t.Fatalf("status problem: %q err=%v", status.LastErrors, err)
	}
	filters := transcriptFilters.Load()
	again, err := Run(context.Background(), local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Errors) != 0 || len(again.Skipped) != 1 || transcriptFilters.Load() != filters {
		t.Fatalf("an unchanged gap was read again: %#v filters %d -> %d", again, filters, transcriptFilters.Load())
	}
	if status, err := local.LoadStatus(); err != nil || len(status.LastErrors) != 1 || !strings.Contains(status.LastErrors[0], "per-session read memory 36 MiB") {
		t.Fatalf("second pass status: %q err=%v", status.LastErrors, err)
	}
	// Within its share, the same session is captured.
	sessionReadCeiling = previous
	if err := local.SaveRegistration(registration(t, writeTranscript(t, t.TempDir(), "large.jsonl", keptTranscript(40<<20)))); err != nil {
		t.Fatal(err)
	}
	captured, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(captured.Errors) != 0 || len(captured.Published) != 1 {
		t.Fatalf("within its share: %#v err=%v", captured, err)
	}
}

// TestPendingUploadOverItsShareDefersThenEscalates: a publication left
// pending that needs more than the session's share to finish is deferred,
// not recorded as a gap (which would leave the upload owed and retried on
// every pass), and fails once it has been deferred for MaxDeferral.
//
// At 96 MiB the session loads what it published and the pending upload,
// and finishing the upload is refused; at 36 MiB even loading them is.
//
// Not parallel: it lowers sessionReadCeiling.
func TestPendingUploadOverItsShareDefersThenEscalates(t *testing.T) {
	previous := sessionReadCeiling
	t.Cleanup(func() { sessionReadCeiling = previous })
	for _, ceiling := range []int64{96 << 20, 36 << 20} {
		sessionReadCeiling = previous
		pendingUploadOverCeiling(t, ceiling)
	}
}

func pendingUploadOverCeiling(t *testing.T, ceiling int64) {
	t.Helper()
	local := newTestStore(t)
	dir := t.TempDir()
	reg := registration(t, writeTranscript(t, dir, "large.jsonl", keptTranscript(1<<20)))
	now := reg.RegisteredAt.Add(time.Hour)
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }, MinUploadInterval: time.Hour}
	publishOnce(t, local, remote, reg, &opts)
	// The transcript grows; its next publication waits, pending, for the
	// upload interval.
	writeTranscript(t, dir, "large.jsonl", keptTranscript(40<<20))
	now = now.Add(time.Minute)
	if result, err := Run(t.Context(), local, remote, opts); err != nil || len(result.Waiting) != 1 {
		t.Fatalf("fixture: %#v %v", result, err)
	}
	if pending, err := local.HasPending(reg.ArchiveSessionID); err != nil || !pending {
		t.Fatalf("fixture left no pending upload: %v", err)
	}
	// The upload is due, but finishing it needs more than the session's
	// share: on each later pass the session is deferred, the source is not
	// read past it, and no gap is recorded.
	sessionReadCeiling = ceiling
	for range 2 {
		now = now.Add(2 * time.Hour)
		filters := transcriptFilters.Load()
		result, err := Run(t.Context(), local, remote, opts)
		if err != nil || result.Deferred[reg.ArchiveSessionID] != DeferredReadBudget || len(result.Errors) != 0 || len(result.Published) != 0 {
			t.Fatalf("pending upload over a %d MiB share: %v %v", ceiling>>20, result.Errors, err)
		}
		if _, blocked, _ := local.LoadBlocked(reg.ArchiveSessionID); blocked {
			t.Fatal("pending upload recorded as a gap")
		}
		if transcriptFilters.Load() != filters {
			t.Fatal("the source was read past a pending upload")
		}
	}
	now = now.Add(MaxDeferral)
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil || !strings.Contains(result.Errors[reg.ArchiveSessionID].Error(), DeferralExpiredProblem) {
		t.Fatalf("expired deferral: %#v %v", result, err)
	}
}

// TestRaisedRawLimitReadsAGapAgain: a size-limit gap stands only while the
// limit it was reached under does. A gap from before the raw limit was
// recorded (a Codex thread over the former 128 MiB cap) is read again.
func TestRaisedRawLimitReadsAGapAgain(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m"}
	publishOnce(t, local, remote, reg, &opts)
	signature, _, err := local.LoadScanSignature(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	signature.Failed, signature.Blocked = true, state.BlockedReasonTranscriptTooLarge
	signature.FailedMaxBytes, signature.FailedRecordLimit = opts.maxTranscriptBytes(), recordLimit
	signature.FailedRawLimit = opts.maxRawBytes(reg.Harness.Name)
	if err := local.SaveScanSignature(reg.ArchiveSessionID, signature); err != nil {
		t.Fatal(err)
	}
	stored, _, err := local.LoadRegistration(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged(t, local, stored, opts) {
		t.Fatal("a gap under the current limits is read again")
	}
	// A gap recorded before the raw limit was recorded (FailedRawLimit 0),
	// or under the former 128 MiB cap, is read again.
	for _, former := range []int64{0, 128 << 20} {
		signature.FailedRawLimit = former
		if err := local.SaveScanSignature(reg.ArchiveSessionID, signature); err != nil {
			t.Fatal(err)
		}
		if unchanged(t, local, stored, opts) {
			t.Fatalf("a gap reached under raw limit %d still stands", former)
		}
	}
	// A per-session read gap stands only while the ceiling does.
	signature.FailedRawLimit = opts.maxRawBytes(reg.Harness.Name)
	for ceiling, stands := range map[int64]bool{sessionReadCeiling: true, 96 << 20: false} {
		signature.SizeLimit = &state.SizeLimitGap{Kind: state.SizeLimitReadBudget, LimitBytes: ceiling}
		if err := local.SaveScanSignature(reg.ArchiveSessionID, signature); err != nil {
			t.Fatal(err)
		}
		if unchanged(t, local, stored, opts) != stands {
			t.Fatalf("read gap under ceiling %d stands=%v", ceiling, !stands)
		}
	}
}

// TestSharedPressureDefersThenEscalates: a source read refused because the
// pass's ledger was held, the session itself within its share, is deferred
// (read_budget) rather than failed or gapped, and, like every deferral,
// fails once it has been deferred for MaxDeferral.
func TestSharedPressureDefersThenEscalates(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", keptTranscript(1<<20)))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	now := reg.RegisteredAt.Add(time.Hour)
	// A pass ledger far smaller than one session's share: every refusal is
	// the shared ledger's.
	budget := agentapi.NewNativeReadBudget(256 << 10)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }, CodexRollouts: recoveryBudgetLookup{CodexRolloutLookup: &reconciliationLookup{}, budget: budget}}
	remote := storagetest.NewMemoryStore()
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Deferred[reg.ArchiveSessionID] != DeferredReadBudget || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("shared pressure: %#v %v", result, err)
	}
	if _, blocked, _ := local.LoadBlocked(reg.ArchiveSessionID); blocked {
		t.Fatal("shared pressure was recorded as the session's own gap")
	}
	now = now.Add(MaxDeferral + time.Minute)
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil || !strings.Contains(result.Errors[reg.ArchiveSessionID].Error(), DeferralExpiredProblem) {
		t.Fatalf("expired deferral: %#v %v", result, err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatalf("charge leaked: %d", used)
	}
}

// A refusal during or after the source read records a named gap only when
// no upload is pending. A superseded, unattempted upload remains owed and
// defers the scan instead; shared pressure also defers.
func TestOverCeilingReadAndBuildRespectPendingUpload(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		pending bool
		read    bool
	}{{false, false}, {true, false}, {false, true}, {true, true}} {
		pending := c.pending
		local := newTestStore(t)
		reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
		remote := storagetest.NewMemoryStore()
		opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m"}
		publishOnce(t, local, remote, reg, &opts)
		if pending {
			// resume validates protected pending bytes before superseding an
			// unattempted upload, so the fixture needs their real checksum.
			if err := local.SavePending(reg.ArchiveSessionID, state.PendingPublication{ReadyAt: reg.RegisteredAt, SourceKey: "source", MetadataKey: "metadata", SourceSHA256: storage.SHA256Hex([]byte("source")), SourceBytes: []byte("source"), MetadataBytes: []byte(`{}`)}); err != nil {
				t.Fatal(err)
			}
		}
		published, err := local.LoadPublishedState(reg.ArchiveSessionID)
		if err != nil {
			t.Fatal(err)
		}
		scan := newSessionScan(t.Context(), local, remote, reg, state.Request{Token: "new-request"}, published, reg.RegisteredAt.Add(time.Hour), opts)
		if pending && c.read {
			// The new hook request supersedes the unattempted publication,
			// so resume lets the scan proceed to the source read.
			if _, handled, err := scan.resume(); err != nil || handled {
				t.Fatalf("superseding pending upload: handled=%v err=%v", handled, err)
			}
		}
		reader, _ := newSourceReader(reg, opts)
		observed, err := reader.Signature(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := sourceAdapter(opts.Sources, reg.Harness.Name)
		if err != nil {
			t.Fatal(err)
		}
		scan.lastRead = &sourceRead{adapter: adapter, observed: observed}
		budget := scan.readBudget()
		end := budget.BeginScope(1 << 20)
		if budget.Reserve(2 << 20) {
			t.Fatal("over the ceiling was reserved")
		}
		var outcome sessionOutcome
		if c.read {
			scan.lastRead = nil
			outcome, err = scan.readFailed(sourceRead{adapter: adapter, observed: observed}, errRetainedBudget)
		} else {
			outcome, err = scan.overCeiling(errRetainedBudget)
		}
		end()
		if pending {
			// A gap would leave the upload owed and retried every pass.
			if reason, ok := deferralReason(err); !ok || reason != DeferredReadBudget {
				t.Fatalf("pending upload over the ceiling: %v", err)
			}
			if _, blocked, _ := local.LoadBlocked(reg.ArchiveSessionID); blocked {
				t.Fatal("pending upload recorded as a gap")
			}
			if owed, err := local.HasPending(reg.ArchiveSessionID); err != nil || !owed {
				t.Fatalf("pending upload lost: owed=%v err=%v", owed, err)
			}
			continue
		}
		if err != nil || outcome != outcomeSkipped {
			t.Fatalf("pending=%v: %v", pending, err)
		}
		if reason, blocked, _ := local.LoadBlocked(reg.ArchiveSessionID); !blocked || reason != state.BlockedReasonTranscriptTooLarge {
			t.Fatalf("pending=%v: no gap", pending)
		}
		signature, _, _ := local.LoadScanSignature(reg.ArchiveSessionID)
		if signature.SizeLimit == nil || signature.SizeLimit.Kind != state.SizeLimitReadBudget || !observed.matches(signature) {
			t.Fatalf("pending=%v: gap %+v", pending, signature)
		}
	}
	// Shared pressure after the read defers.
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m"}
	scan := newSessionScan(t.Context(), local, storagetest.NewMemoryStore(), reg, state.Request{}, &state.Published{}, reg.RegisteredAt, opts)
	budget := scan.readBudget()
	held := budget.Available() - (1 << 20)
	if !budget.ReserveUnscoped(held) {
		t.Fatal("reserve")
	}
	end := budget.BeginScope(sessionReadCeiling)
	if budget.Reserve(2 << 20) {
		t.Fatal("reserved past the held ledger")
	}
	_, err := scan.overCeiling(errRetainedBudget)
	end()
	budget.ReleaseUnscoped(held)
	if reason, ok := deferralReason(err); !ok || reason != DeferredReadBudget {
		t.Fatalf("shared pressure after the read: %v", err)
	}
}

// TestSessionStartsWithItsCeilingFree: when what the pass keeps leaves less
// than a session's ceiling free (the baseline sits between the ceiling and
// the pass ledger), the source passes' idle caches are dropped before the
// session begins, so a session within its ceiling runs.
func TestSessionStartsWithItsCeilingFree(t *testing.T) {
	t.Parallel()
	if SessionReadCeiling+agentapi.PassCacheReserve != agentapi.PassReadBudget {
		t.Fatal("ceiling and pass caches do not fit the pass ledger")
	}
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	cache := &cachingPass{budget: budget, held: agentapi.PassCacheReserve + 32<<20}
	if !budget.ReserveUnscoped(cache.held) {
		t.Fatal("reserve")
	}
	set := &sourcePassSet{passes: map[sourcePassKey]agentapi.SourcePass{{name: "codex"}: cache}}
	if budget.Available() >= SessionReadCeiling {
		t.Fatal("fixture baseline does not sit between the ceiling and the pass")
	}
	if err := set.releaseCaches(budget, SessionReadCeiling); err != nil {
		t.Fatal(err)
	}
	end := budget.BeginScope(SessionReadCeiling)
	defer end()
	if !budget.Reserve(SessionReadCeiling) || budget.ScopeExceeded() || budget.ScopePressured() {
		t.Fatal("a session within its ceiling could not run")
	}
	budget.Release(SessionReadCeiling)
}

type cachingPass struct {
	agentapi.SourcePass
	budget   *agentapi.NativeReadBudget
	held     int64
	releases int
}

func (p *cachingPass) ReleaseCaches() error {
	p.budget.ReleaseUnscoped(p.held)
	p.held = 0
	p.releases++
	return nil
}

func (p *cachingPass) Close() error {
	p.budget.ReleaseUnscoped(p.held)
	p.held = 0
	return p.SourcePass.Close()
}

// cachingSources opens source passes that keep more cached than
// agentapi.PassCacheReserve, as a pass whose caches grew past their caps.
type cachingSources struct {
	agentapi.SourcesLookup
	passes *[]*cachingPass
}

func (s cachingSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := s.SourcesLookup.LookupSources(name)
	return cachingProvider{SourceProvider: p, passes: s.passes}, f, ok
}

type cachingProvider struct {
	agentapi.SourceProvider
	passes *[]*cachingPass
}

func (p cachingProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	pass, err := p.SourceProvider.OpenPass(ctx, env)
	if err != nil {
		return nil, err
	}
	held := agentapi.PassCacheReserve + 32<<20
	if !env.ReadBudget.ReserveUnscoped(held) {
		return nil, errors.Join(errors.New("reserve cache"), pass.Close())
	}
	cached := &cachingPass{SourcePass: pass, budget: env.ReadBudget, held: held}
	*p.passes = append(*p.passes, cached)
	return cached, nil
}

// TestPassDropsSourceCachesBeforeASession: a pass whose source caches left
// less than a session's ceiling free drops them before the next session
// begins.
func TestPassDropsSourceCachesBeforeASession(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	dir := t.TempDir()
	for _, id := range []string{"session-1", "session-2"} {
		reg := registration(t, writeTranscript(t, dir, id+".jsonl", codexTranscript))
		reg.ArchiveSessionID, reg.NativeSessionID = id, "native-"+id
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	var passes []*cachingPass
	opts := Options{Sources: cachingSources{SourcesLookup: testSources, passes: &passes}, Parsers: testParsers, MachineID: "m"}
	result, err := Run(t.Context(), local, storagetest.NewMemoryStore(), opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 2 {
		t.Fatalf("%#v %v", result, err)
	}
	releases := 0
	for _, pass := range passes {
		releases += pass.releases
	}
	if len(passes) == 0 || releases == 0 {
		t.Fatalf("no cache dropped before the second session: passes=%d releases=%d", len(passes), releases)
	}
}

func TestCodexRawLimitIsTheSharedConstant(t *testing.T) {
	t.Parallel()
	if got := maxRawBytes(archive.HarnessCodex, DefaultMaxTranscriptBytes); got != archive.MaxCodexRawTranscriptBytes || got < DefaultMaxRawTranscriptBytes {
		t.Fatalf("Codex raw limit %d", got)
	}
	if got := maxRawBytes(archive.HarnessClaude, DefaultMaxTranscriptBytes); got != DefaultMaxRawTranscriptBytes {
		t.Fatalf("general raw limit %d", got)
	}
	// Discovery and backfill read archive's helper and this export: they
	// must refuse nothing capture reads.
	for _, harness := range []string{archive.HarnessCodex, archive.HarnessClaude, archive.HarnessCursor} {
		if MaxRawTranscriptBytes(harness) != archive.MaxRawTranscriptBytesFor(harness) {
			t.Errorf("%s: collector %d, archive %d", harness, MaxRawTranscriptBytes(harness), archive.MaxRawTranscriptBytesFor(harness))
		}
	}
}

// TestReadBudgetDeferralIsOnlyTheSourceRead: a source read refused while
// others held the pass's ledger defers the session; the same refusal
// anywhere else stays a failure, as does one joined with a failure.
func TestReadBudgetDeferralIsOnlyTheSourceRead(t *testing.T) {
	t.Parallel()
	refusal := agentapi.ReadBudgetLimit(errors.New("native record buffer budget exhausted"))
	if reason, ok := deferralReason(&readBudgetDeferralError{err: refusal}); !ok || reason != DeferredReadBudget {
		t.Fatalf("source read refusal: %q %v", reason, ok)
	}
	if reason, ok := deferralReason(errors.Join(&readBudgetDeferralError{err: refusal}, errors.New("upload failed"))); ok {
		t.Fatalf("joined with a failure deferred as %q", reason)
	}
	if reason, ok := deferralReason(refusal); ok {
		t.Fatalf("a refusal outside the source read deferred as %q", reason)
	}
}

func TestSizeLimitOfNamesEachLimit(t *testing.T) {
	t.Parallel()
	observed := sourceState{file: transcriptFileInfo{Size: 900}}
	for _, c := range []struct {
		err  error
		want state.SizeLimitGap
	}{
		{agentapi.RawLimit(700<<20, 512<<20, nil), state.SizeLimitGap{Kind: state.SizeLimitRaw, LimitBytes: 512 << 20, SizeBytes: 700 << 20}},
		{checkFilteredSize(archive.FilteredTranscript{Boundary: archive.CaptureBoundary{RetainedBytes: 70}}, 64), state.SizeLimitGap{Kind: state.SizeLimitFiltered, LimitBytes: 64, SizeBytes: 70}},
		{errSessionReadCeiling, state.SizeLimitGap{Kind: state.SizeLimitReadBudget, LimitBytes: sessionReadCeiling}},
		{translateSourceError(agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)), state.SizeLimitGap{Kind: state.SizeLimitRecord, LimitBytes: recordLimit}},
	} {
		got := sizeLimitOf(c.err, observed, archive.HarnessCodex, DefaultMaxTranscriptBytes)
		if got == nil || *got != c.want {
			t.Errorf("%v: %+v, want %+v", c.err, got, c.want)
		}
	}
	if got := SizeLimitLabel(archive.HarnessCodex, state.SizeLimitGap{Kind: state.SizeLimitRaw, LimitBytes: 512 << 20}); got != "Codex raw 512 MiB" {
		t.Fatal(got)
	}
	if got := SizeLimitLabel(archive.HarnessClaude, state.SizeLimitGap{Kind: state.SizeLimitFiltered, LimitBytes: 64}); got != "filtered 64 bytes" || SizeLimitLabel("", state.SizeLimitGap{Kind: state.SizeLimitFiltered, LimitBytes: 1536 << 10}) != "filtered 1.5 MiB" || SizeLimitLabel("", state.SizeLimitGap{Kind: state.SizeLimitRecord, LimitBytes: 64 << 10}) != "record 64 KiB" {
		t.Fatal(got)
	}
	// A status written before the limits were named is still recognised.
	if !IsSizeLimitProblem("2 session(s) stopped being captured: over the transcript size limit, kept at their last snapshot") {
		t.Fatal("former size-limit problem not recognised")
	}
}
