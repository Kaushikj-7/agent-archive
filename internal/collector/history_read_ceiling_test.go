package collector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// budgetedLookup is reconciliationLookup sharing one read ledger with the
// collector's pass, so a test can read what a pass charged at its peak.
type budgetedLookup struct {
	reconciliationLookup
	budget *agentapi.NativeReadBudget
}

func (l *budgetedLookup) NativeReadBudget() *agentapi.NativeReadBudget { return l.budget }

// chainMessage is the size of one user message in a long chain fixture.
const chainMessage = 16 << 10

// writeChainRollout writes one paginated rollout of revisionThread: a
// session_meta at ordinal start, continuing all of base when base is not
// empty, then one user message per text.
func writeChainRollout(t *testing.T, root, id, base string, start uint64, texts []string) agentapi.SourceRef {
	t.Helper()
	meta := map[string]any{"id": revisionThread, "cwd": root, "timestamp": "2026-10-01T12:00:00Z", "cli_version": "0.160.0", "originator": "codex_cli_rs", "source": "cli", "history_mode": "paginated"}
	if base != "" {
		info, err := os.Stat(base)
		if err != nil {
			t.Fatal(err)
		}
		meta["history_base"] = map[string]any{"thread_id": revisionThread, "end_ordinal_exclusive": start, "end_byte_offset": info.Size()}
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	if err := enc.Encode(map[string]any{"type": "session_meta", "ordinal": start, "payload": meta}); err != nil {
		t.Fatal(err)
	}
	for i, text := range texts {
		if err := enc.Encode(map[string]any{"type": "response_item", "ordinal": start + uint64(i) + 1, "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}); err != nil {
			t.Fatal(err)
		}
	}
	ref := agentapi.SourceRef{Path: filepath.Join(root, fmt.Sprintf("rollout-2026-10-01T12-00-00-%s.jsonl", id)), Key: revisionThread}
	if err := os.WriteFile(ref.Path, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return ref
}

// longChain is a long Codex thread whose history spans two rollouts: A, the
// thread's first, and C, which continues all of A (Codex's paginated
// history) and is the tip Codex's index names. Its messages are
// incompressible synthetic text.
type longChain struct {
	local  *state.Store
	remote *storagetest.MemoryStore
	reg    archive.SessionRegistration
	opts   Options
	lookup *budgetedLookup
	now    *time.Time
	// next is the ordinal of the next message appended to C.
	next uint64
}

// newLongChain writes a long chain with a and c incompressible messages of
// chainMessage bytes in A and C, with a stop request queued for it.
func newLongChain(t *testing.T, a, c int) *longChain {
	t.Helper()
	return newLongChainTexts(t, offChainTexts("a", a, chainMessage), offChainTexts("c", c, chainMessage))
}

// newLongChainTexts writes a long chain with one user message per text in
// A and in C, with a stop request queued for it.
func newLongChainTexts(t *testing.T, aTexts, cTexts []string) *longChain {
	t.Helper()
	root := t.TempDir()
	a, c := len(aTexts), len(cTexts)
	first := writeChainRollout(t, root, revisionThread, "", 0, aTexts)
	tip := writeChainRollout(t, root, revisionC, first.Path, uint64(a)+1, cTexts)
	lookup := &budgetedLookup{reconciliationLookup: reconciliationLookup{set: agentapi.CodexRolloutSet{Current: &tip, Candidates: []agentapi.SourceRef{first, tip}, Revision: "current"}, refs: map[string][]agentapi.SourceRef{revisionThread: {first}, revisionC: {tip}}}}
	local := newTestStore(t)
	reg := registration(t, first.Path)
	reg.NativeSessionID = revisionThread
	reg.ProjectRoot = root
	reg.Origin = archive.SessionOriginHook
	reg.SessionStartedAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	reg.RegisteredAt = reg.SessionStartedAt.Add(time.Second)
	reg.CodexBinding = &archive.CodexSourceBinding{Version: 1, NativeThreadID: revisionThread, NativeCreatedAt: reg.SessionStartedAt, Cwd: root, ProducerSource: "cli", RootID: revisionThread, PhysicalRolloutID: revisionThread, Path: first.Path, Home: root}
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{MachineID: "machine", Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: root, ProjectID: reg.ProjectID, Included: true, ActivatedAt: reg.SessionStartedAt}}}}
	if err := config.Save(local.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", reg.RegisteredAt); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	chain := &longChain{local: local, remote: storagetest.NewMemoryStore(), reg: reg, lookup: lookup, now: &now, next: uint64(a+c) + 2}
	chain.opts = Options{MachineID: "machine", Sources: testSources, Parsers: testParsers, CodexRollouts: lookup, Now: func() time.Time { return *chain.now }, RepoKey: func(string) string { return "" }, MinUploadInterval: time.Hour}
	return chain
}

// scan is a scan of the chain's session as a pass would begin it.
func (c *longChain) scan(t *testing.T) *sessionScan {
	t.Helper()
	published, err := c.local.LoadPublishedState(c.reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	return newSessionScan(t.Context(), c.local, c.remote, c.reg, state.Request{}, published, *c.now, c.opts)
}

// filteredBytes is what the chain's active read retains filtered: the
// whole chain, A then C.
func (c *longChain) filteredBytes(t *testing.T) int64 {
	t.Helper()
	scan := c.scan(t)
	defer scan.releaseRetained()
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal("chain read", err)
	}
	return int64(read.filtered.Boundary.RetainedBytes)
}

// run runs one collector pass on a fresh ledger and returns its result and
// the ledger's peak charge.
func (c *longChain) run(t *testing.T) (Result, int64) {
	t.Helper()
	c.lookup.budget = agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	result, err := Run(t.Context(), c.local, c.remote, c.opts)
	if err != nil {
		t.Fatal(err)
	}
	_, peak := c.lookup.budget.Charged()
	return result, peak
}

// grow appends a user message to C.
func (c *longChain) grow(t *testing.T) {
	t.Helper()
	growRevision(t, c.lookup.set.Current.Path, c.next, "grown")
	c.next++
}

// The tip is the current revision, which the scan's own read already holds:
// reconciling the chain reads only the segments before it. Here A is one
// message and C all the rest; reading C again would charge about twice the
// whole chain's filtered size.
func TestReconciliationDoesNotReadTheCurrentRolloutAgain(t *testing.T) {
	t.Parallel()
	chain := newLongChain(t, 1, 256)
	scan := chain.scan(t)
	defer scan.releaseRetained()
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal("chain read", err)
	}
	active, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	f := int64(read.filtered.Boundary.RetainedBytes)
	before, _ := scan.readBudget().Charged()
	plan, err := scan.reconcileRevisions(read, active)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := scan.readBudget().Charged()
	if plan == nil || plan.Current != revisionC {
		t.Fatalf("reconciled plan %+v", plan)
	}
	if grew := after - before; grew > f/8 {
		t.Fatalf("reconciliation kept %d bytes (%.2f of the chain's filtered size), so it read the current rollout again", grew, float64(grew)/float64(f))
	}
}

// A long Codex thread whose history spans two rollouts, far under the
// filtered ceiling, keeps being captured as it grows within a session read
// ceiling of seven times its filtered size. Capturing its growth once
// re-read and re-filtered the current rollout (the whole chain again), held
// the read's encoded rows beside the decoded candidate, reserved twice the
// pending file to read one field of it, decoded the frozen candidate again
// to prepare it and held the whole pending encoding to save it: it peaked
// near 12 times the chain's filtered size, and this ceiling recorded the
// thread as too large to read. The first capture runs under the default
// ceiling; it also holds a fixed reservation (the remote absence check)
// that a chain this small would not separate from its own size.
//
// Not parallel: it lowers sessionReadCeiling.
func TestLongCodexChainUnderTheFilteredCeilingIsCaptured(t *testing.T) {
	previous := sessionReadCeiling
	t.Cleanup(func() { sessionReadCeiling = previous })
	chain := newLongChain(t, 32, 32)
	f := chain.filteredBytes(t)
	captured := func(what string, want func(Result) bool) {
		t.Helper()
		result, peak := chain.run(t)
		t.Logf("%s: peak %d bytes, %.2f times the chain's filtered size %d", what, peak, float64(peak)/float64(f), f)
		if !want(result) || len(result.Errors) != 0 || len(result.Deferred) != 0 {
			t.Fatalf("%s: %+v", what, result)
		}
		if reason, blocked, err := chain.local.LoadBlocked(chain.reg.ArchiveSessionID); err != nil || blocked {
			t.Fatalf("%s: recorded gap %q (%v)", what, reason, err)
		}
	}
	captured("first capture", func(r Result) bool { return len(r.Published) == 1 })
	sessionReadCeiling = 7 * f
	if f > chain.opts.maxTranscriptBytes()/16 {
		t.Fatalf("fixture: filtered %d bytes no longer far under the filtered ceiling %d", f, chain.opts.maxTranscriptBytes())
	}
	// The thread grows; its next publication waits out the upload interval,
	// then uploads.
	chain.grow(t)
	*chain.now = chain.now.Add(time.Minute)
	captured("growth", func(r Result) bool { return len(r.Waiting) == 1 })
	*chain.now = chain.now.Add(2 * time.Hour)
	captured("upload", func(r Result) bool { return len(r.Published) == 1 })
}

// A size-limit gap recorded before the raw limit was recorded with it (no
// failed_raw_limit) reads the session once more, since the limits it was
// reached under are unknown. Reached again, the gap is recorded with the
// current limits, so later passes skip the session without reading it.
//
// Not parallel: it lowers sessionReadCeiling and counts transcriptFilters.
func TestReadCeilingGapWithoutItsRawLimitIsReadOnceMore(t *testing.T) {
	previous := sessionReadCeiling
	t.Cleanup(func() { sessionReadCeiling = previous })
	sessionReadCeiling = 8 << 20
	chain := newLongChain(t, 256, 256)
	blocked := func(what string) state.ScanSignature {
		t.Helper()
		result, _ := chain.run(t)
		if len(result.Errors) != 0 || len(result.Published) != 0 || len(result.Deferred) != 0 {
			t.Fatalf("%s: %+v", what, result)
		}
		if reason, blocked, err := chain.local.LoadBlocked(chain.reg.ArchiveSessionID); err != nil || !blocked || reason != state.BlockedReasonTranscriptTooLarge {
			t.Fatalf("%s: blocked=%v reason=%q err=%v", what, blocked, reason, err)
		}
		signature, found, err := chain.local.LoadScanSignature(chain.reg.ArchiveSessionID)
		if err != nil || !found || signature.SizeLimit == nil || signature.SizeLimit.Kind != state.SizeLimitReadBudget {
			t.Fatalf("%s: signature %+v found=%v err=%v", what, signature, found, err)
		}
		return signature
	}
	signature := blocked("first pass")
	if signature.FailedRawLimit != chain.opts.maxRawBytes(chain.reg.Harness.Name) {
		t.Fatalf("gap recorded raw limit %d", signature.FailedRawLimit)
	}
	signature.FailedRawLimit = 0
	if err := chain.local.SaveScanSignature(chain.reg.ArchiveSessionID, signature); err != nil {
		t.Fatal(err)
	}
	filters := transcriptFilters.Load()
	signature = blocked("pass over the older gap")
	if transcriptFilters.Load() == filters {
		t.Fatal("a gap without its raw limit was not read again")
	}
	if signature.FailedRawLimit != chain.opts.maxRawBytes(chain.reg.Harness.Name) {
		t.Fatalf("gap reached again recorded raw limit %d", signature.FailedRawLimit)
	}
	filters = transcriptFilters.Load()
	for range 2 {
		if result, _ := chain.run(t); len(result.Errors) != 0 || len(result.Skipped) != 1 {
			t.Fatalf("later pass: %+v", result)
		}
	}
	if transcriptFilters.Load() != filters {
		t.Fatal("the gap reached again under the current limits was read on later passes")
	}
}

// A scan that freezes a history publication from its own candidate prepares
// the publication's current source from that candidate, which the
// publication carries, instead of decoding its staged bytes again: for a
// long thread that would hold a second copy of the whole chain.
//
// Not parallel: it counts currentInputDecodes.
func TestPreparingAFrozenCandidateDoesNotDecodeItAgain(t *testing.T) {
	chain := newLongChain(t, 4, 4)
	decodes := currentInputDecodes.Load()
	result, _ := chain.run(t)
	if len(result.Published) != 1 || len(result.Errors) != 0 {
		t.Fatalf("first capture: %+v", result)
	}
	if n := currentInputDecodes.Load() - decodes; n != 0 {
		t.Fatalf("preparation decoded the scan's own candidate %d times", n)
	}
	metadata := fetchMetadata(t, chain.remote, "codex", chain.reg.ArchiveSessionID)
	if metadata.History == nil || metadata.History.CurrentRevision != revisionC {
		t.Fatalf("published history %+v", metadata.History)
	}
}
