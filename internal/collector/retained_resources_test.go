package collector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRetainedRefilterMakesSmallProgressUnderSharedPressure(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	bundle, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	scan.releaseRetained()
	budget := agentapi.NewNativeReadBudget(2 << 20)
	reservation := budget.Reservation()
	pressure := int64(1 << 20)
	if !reservation.Reserve(pressure) {
		t.Fatal("pressure")
	}
	scan.retainedBudget = budget
	out, err := scan.refilterRetained(codex.Filter{}, bundle)
	if err != nil {
		t.Fatalf("small valid work starved by ceiling: %v", err)
	}
	if err := out.ValidateHistory(); err != nil {
		t.Fatal(err)
	}
	if len(out.NativeRecords) != len(bundle.NativeRecords) {
		t.Fatal("partial refilter output")
	}
	used, peak := budget.Charged()
	if used <= pressure || peak >= 2<<20 {
		t.Fatalf("owned charge %d peak%d", used, peak)
	}
	scan.releaseRetained()
	used, _ = budget.Charged()
	if used != pressure {
		t.Fatalf("release imbalance %d", used-pressure)
	}
	// Complete exhaustion refuses without retaining output, and does not prevent
	// independent small progress when the unrelated reservation is returned.
	fill := budget.Available()
	if !reservation.Reserve(fill) {
		t.Fatal("fill")
	}
	_, err = scan.refilterRetained(codex.Filter{}, bundle)
	if !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("refusal %v", err)
	}
	reservation.Release(fill)
	reservation.Release(pressure)
	if _, err := scan.refilterRetained(codex.Filter{}, bundle); err != nil {
		t.Fatal(err)
	}
	scan.releaseRetained()
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("independent progress leaked%d", used)
	}
}

func TestRetainedComparisonReservationReleasesOnRefusal(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	budget := agentapi.NewNativeReadBudget(32 << 10)
	scan.retainedBudget = budget
	_, err := scan.jsonEncodingsEqual(archive.SupplementalEvidence{Payload: map[string]any{"text": "synthetic"}}, nil)
	if !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal(err)
	}
	scan.releaseRetained()
	used, _ := budget.Charged()
	if used != 0 {
		t.Fatalf("comparison refusal leaked%d", used)
	}
}

// HEAD checksum absence is supported by production S3 stores, including older
// objects and multipart checksums. This lane must still use charged bounded GET.
type noChecksumHistoryStore struct {
	*storagetest.MemoryStore
	sourceGets        int
	sourceLimitedGets int
}

func (s *noChecksumHistoryStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	info, err := s.MemoryStore.Stat(ctx, key)
	info.SHA256 = ""
	return info, err
}

func (s *noChecksumHistoryStore) Get(ctx context.Context, key string) ([]byte, error) {
	if strings.Contains(key, "/source.") {
		s.sourceGets++
		return nil, errors.New("unbounded source verification read")
	}
	return s.MemoryStore.Get(ctx, key)
}

func (s *noChecksumHistoryStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if strings.Contains(key, "/source.") {
		s.sourceLimitedGets++
	}
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

func TestHistoryPublicationWithoutHeadChecksumUsesBoundedVerification(t *testing.T) {
	scan, pending := privacyJournal(t)
	remote := &noChecksumHistoryStore{MemoryStore: scan.remote.(*storagetest.MemoryStore)}
	result := runHistoryRetry(t, scan, remote)
	if len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("bounded checksum fallback did not publish: %#v", result)
	}
	if remote.sourceGets != 0 || remote.sourceLimitedGets == 0 {
		t.Fatalf("source verification reads: unbounded=%d bounded=%d", remote.sourceGets, remote.sourceLimitedGets)
	}
	assertCompleteHistory(t, scan, pending, remote.MemoryStore)
}

func TestHistoryVerificationRefusesSharedPressureThenMakesProgress(t *testing.T) {
	scan, pending, cloud, _ := frozenHistoryFixture(t)
	remote := &noChecksumHistoryStore{MemoryStore: cloud}
	scan.remote = remote
	budget := scan.readBudget()
	reservation := budget.Reservation()
	pressure := budget.Available()
	if !reservation.Reserve(pressure) {
		t.Fatal("pressure")
	}
	if err := scan.ensureHistorySource(pending.SourceKey, pending.SourceSHA256, pending.SourceBytes); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("verification bypassed exhausted shared ledger: %v", err)
	}
	if remote.sourceLimitedGets != 0 || remote.sourceGets != 0 {
		t.Fatal("source was read before reserving")
	}
	reservation.Release(pressure)
	if err := scan.ensureHistorySource(pending.SourceKey, pending.SourceSHA256, pending.SourceBytes); err != nil {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatalf("verification lease leaked: %d", used)
	}
}

func TestRevisionSizingRefusesSharedPressureAndReleasesScratch(t *testing.T) {
	budget := agentapi.NewNativeReadBudget(64 << 10)
	reservation := budget.Reservation()
	pressure := int64(40 << 10)
	if !reservation.Reserve(pressure) {
		t.Fatal("pressure")
	}
	p := revisionPlanner{budget: budget, plan: &revisionPlan{}}
	b := revisionBundle(t, revisionB, "synthetic retained evidence")
	if _, err := p.checkStageEvidence(t.Context(), b); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("sizing bypassed shared pressure: %v", err)
	}
	if used, _ := budget.Charged(); used != pressure {
		t.Fatalf("refusal changed prior charge: %d", used)
	}
	reservation.Release(pressure)
	if n, err := p.checkStageEvidence(t.Context(), b); err != nil || n == 0 {
		t.Fatalf("independent sizing failed: %d %v", n, err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatalf("sizing scratch leaked: %d", used)
	}
}

func TestMetadataRefreshEncodingRefusesSharedPressure(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	bundle, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderPublication(scan.ctx, scan.resolveParser(), scan.parserVersion(), bundle, scan.reg, scan.now, scan.opts, scan.priorRepoKey)
	if err != nil {
		t.Fatal(err)
	}
	var prior archive.Metadata
	if err := json.Unmarshal(rendered.metadata, &prior); err != nil {
		t.Fatal(err)
	}
	scan.releaseRetained()
	scan.retainedBudget = agentapi.NewNativeReadBudget(1)
	scan.opts.ParserVersion = "synthetic-parser-upgrade"
	scan.parserResolved = false
	if raw, changed, err := scan.refreshedMetadata(lastPublication{bundle: bundle, metadata: prior}, rendered.source); !errors.Is(err, agentapi.ErrReadBudget) || changed || len(raw) != 0 {
		t.Fatalf("refresh encoded without reservation: changed=%t bytes=%d err=%v", changed, len(raw), err)
	}
	scan.releaseRetained()
	if used, _ := scan.retainedBudget.Charged(); used != 0 {
		t.Fatalf("refresh refusal leaked: %d", used)
	}
}

func TestNativeFilteredRowsEndAfterDecodedOrdinaryConsumption(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", variedTranscript(200))
	reg := registration(t, path)
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	scan := newSessionScan(t.Context(), local, storagetest.NewMemoryStore(), reg, state.Request{}, published, reg.RegisteredAt, Options{Sources: testSources, Parsers: testParsers})
	defer scan.releaseRetained()
	read, ok, err := scan.read()
	if err != nil || !ok || read.filterLease == nil {
		t.Fatal("leased ordinary read", ok, err)
	}
	candidate, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := scan.readBudget().Charged()
	out, err := scan.finishNativeFilter(&read, candidate)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := scan.readBudget().Charged()
	if after >= before || read.filterLease != nil || len(read.filtered.Records) != 0 {
		t.Fatal("encoded rows retained after consumption", before, after)
	}
	// Independent decoded native rows and the detached envelope remain usable.
	if len(out.NativeRecords) != len(candidate.NativeRecords) || out.Capture.Harness != candidate.Capture.Harness {
		t.Fatal("decoded output lost")
	}
	if same, err := scan.bundleEvidenceEqual(out, candidate); err != nil || !same {
		t.Fatal("ordinary evidence changed", err)
	}
}

func TestNativeHistoryAliasesRemainChargedAfterProviderClose(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	closePass := openCursorPass(nil, &scan.opts)
	defer func() { _ = closePass(); scan.releaseRetained() }()
	read, ok, err := scan.read()
	if err != nil || !ok || read.filterLease == nil {
		t.Fatal(ok, err)
	}
	candidate, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := scan.readBudget().Charged()
	out, err := scan.finishNativeFilter(&read, candidate)
	after, _ := scan.readBudget().Charged()
	if err != nil || after != before || read.filterLease == nil {
		t.Fatal("history alias lease ended", before, after, err)
	}
	if err := closePass(); err != nil {
		t.Fatal(err)
	}
	if err := out.ValidateHistory(); err != nil {
		t.Fatal("borrowed history after provider close", err)
	}
	if len(out.Ordinals) != len(out.NativeRecords) {
		t.Fatal("ordinal aliases lost")
	}
	scan.releaseRetained()
	used, _ := scan.readBudget().Charged()
	if used != 0 {
		t.Fatal("history alias scope leaked", used)
	}
}
