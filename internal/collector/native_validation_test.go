package collector

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// This injected producer supplies independent synthetic selection/root facts.
// The real Muse provider remains fenced. Its concrete local snapshot and Filter
// still perform all physical startup, raw proof and complete grammar work.
type validationSources struct {
	facts  archive.NativeSourceBinding
	counts *nativeReadCounts
}

func (s validationSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	if name != "muse" {
		return nil, nil, false
	}
	return validationProvider(s), muse.Filter{}, true
}

type validationProvider struct {
	facts  archive.NativeSourceBinding
	counts *nativeReadCounts
}

func (validationProvider) Describe(ref agentapi.SourceRef) (agentapi.SourceSemantics, error) {
	return (muse.SourceProvider{}).Describe(ref)
}

func (p validationProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	if p.counts != nil {
		if env.Files == nil {
			env.Files = transcriptio.OS{}
		}
		env.Files = countedNativeOpener{Opener: env.Files, counts: p.counts}
	}
	env.NativeRoot = nil
	env.RequireConfinedHistory = false
	env.LegacyUnboundRegistration = false
	pass, e := (muse.SourceProvider{}).OpenPass(ctx, env)
	if e != nil {
		return nil, e
	}
	return &validationPass{SourcePass: pass, facts: p.facts}, nil
}

type validationPass struct {
	agentapi.SourcePass
	facts archive.NativeSourceBinding
}

func (p *validationPass) ValidateSourceAdmission(ctx context.Context, ref agentapi.SourceRef, a agentapi.SourceAdmission) error {
	return p.SourcePass.(agentapi.SourceAdmissionSignature).ValidateSourceAdmission(ctx, ref, a)
}

func (p *validationPass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	snap, e := p.SourcePass.Read(ctx, ref, limits)
	if e != nil {
		return nil, e
	}
	return &validationSnapshot{SourceSnapshot: snap, facts: p.facts}, nil
}

type validationSnapshot struct {
	agentapi.SourceSnapshot
	facts        archive.NativeSourceBinding
	beforeVerify func() error
}

func (s *validationSnapshot) ContentProofFor(actual agentapi.SourceSnapshot, in agentapi.NativeInput) bool {
	own, ok := actual.(*validationSnapshot)
	return ok && own == s && s.SourceSnapshot.(agentapi.VerifiedContentSnapshot).ContentProofFor(s.SourceSnapshot, in)
}

func (s *validationSnapshot) ContentProof(ctx context.Context) (agentapi.SourceContentProof, error) {
	return s.SourceSnapshot.(agentapi.VerifiedContentSnapshot).ContentProof(ctx)
}

func (s *validationSnapshot) VerifyContent(ctx context.Context) error {
	if s.beforeVerify != nil {
		if e := s.beforeVerify(); e != nil {
			return e
		}
	}
	return s.SourceSnapshot.(agentapi.VerifiedContentSnapshot).VerifyContent(ctx)
}

func (s *validationSnapshot) AdmissionEvidence(ctx context.Context, a agentapi.SourceAdmission) (agentapi.AdmissionEvidence, error) {
	if a.Binding == nil {
		proof, e := s.ContentProof(ctx)
		if e != nil {
			return agentapi.AdmissionEvidence{}, e
		}
		facts := s.facts
		facts.Checkpoint = &archive.NativeCheckpoint{Version: 1, Domain: proof.Provider, Algorithm: "sha256", Boundary: proof.Boundary, SHA256: hex.EncodeToString(proof.SHA256[:])}
		a.Binding = &agentapi.SourceBinding{Native: &facts}
	}
	return s.SourceSnapshot.(agentapi.SourceAdmissionEvidence).AdmissionEvidence(ctx, a)
}

type validationSelection struct {
	checks  int
	changed bool
}

func (*validationSelection) Locate(context.Context, agentapi.LocatorRequest) (agentapi.LocatorResult, error) {
	panic("inventory not part of this foundation")
}

func (l *validationSelection) Check(_ context.Context, id, token string) error {
	l.checks++
	if l.changed || id != "00000000-0000-7000-8000-000000000001" || token != "synthetic-complete-selection" {
		return errors.New("selection changed")
	}
	return nil
}

func nativeCollectorFixture(t *testing.T) (*sessionScan, providerReader, []byte) {
	t.Helper()
	project, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-empty.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	raw = bytes.ReplaceAll(raw, []byte("/work/synthetic-muse"), []byte(project))
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	id := "00000000-0000-7000-8000-000000000001"
	path := filepath.Join(root, "2026", "10", "15", id, "session.jsonl")
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	store := newTestStore(t)
	reg := registration(t, "")
	reg.Harness.Name = "muse"
	reg.NativeSessionID = id
	reg.ProjectRoot = project
	reg.ProjectID = archive.ProjectID(project)
	authority := archive.NativeRoot{Agent: "muse", Configured: root, Canonical: root, Revision: "one"}
	cfg := config.Config{Storage: destination.Config{Provider: destination.ProviderS3, Bucket: "synthetic-validation", Prefix: "synthetic", AWSProfile: "synthetic"}, MachineID: "synthetic", Harnesses: []string{"muse"}, NativeRoots: []archive.NativeRoot{authority}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, ProjectID: reg.ProjectID, Included: true, ActivatedAt: reg.SessionStartedAt}}}}
	reg.DestinationID = cfg.DestinationID()
	if e = store.SaveRegistration(reg); e != nil {
		t.Fatal(e)
	}
	if e = config.Save(store.Home(), cfg); e != nil {
		t.Fatal(e)
	}
	facts := archive.NativeSourceBinding{Agent: "muse", Version: 2, NativeID: id, NativeCreatedAt: time.UnixMicro(1792083855882704).UTC(), Cwd: project, Path: path, StreamID: id, RootRevision: "one"}
	opts := Options{Sources: validationSources{facts: facts, counts: &nativeReadCounts{}}, NativeRoots: []archive.NativeRoot{authority}}
	scan := newSessionScan(t.Context(), store, storagetest.NewMemoryStore(), reg, state.Request{}, nil, time.Now(), opts)
	t.Cleanup(func() {
		if e := scan.closeNativeLeases(); e != nil {
			t.Error(e)
		}
		scan.releaseRetained()
		if used, _ := scan.readBudget().Charged(); used != 0 {
			t.Errorf("leaked %d", used)
		}
	})
	r := providerReader{nativeRoot: &authority, nativeLocators: &validationSelection{}, nativeSelection: "synthetic-complete-selection", ref: agentapi.SourceRef{Path: path, Key: id}, harness: "muse", admission: sourceAdmission(reg), resourceOwner: scan, sources: opts.Sources}
	return scan, r, raw
}

func TestPreparedNativeValidationFirstBindAndBoundScanAdvance(t *testing.T) {
	// Sequential: measures the real Filter counter shared by the collector.
	scan, r, raw := nativeCollectorFixture(t)
	if _, available := newSourceReader(scan.reg, scan.opts); available {
		t.Fatal("production empty-locator fence opened")
	}
	before := transcriptFilters.Load()
	counts := scan.opts.Sources.(validationSources).counts
	_, observed, e := r.Filter(t.Context(), muse.Filter{}, 64<<20)
	if e != nil {
		t.Fatal(e)
	}
	if observed.prepared == nil || observed.prepared.closed || transcriptFilters.Load() != before+1 {
		t.Fatal("no live prepared exact Filter result")
	}
	if scan.reg.NativeBinding != nil {
		t.Fatal("provisional facts already committed")
	}
	if e = scan.persistNativeValidation(observed.prepared); e != nil {
		t.Fatal(e)
	}
	if !observed.prepared.closed || scan.reg.NativeBinding.Checkpoint.Boundary != int64(len(raw)) {
		t.Fatal("commit did not release exact source")
	}
	t.Logf("first bind: raw=%d read_calls=%d read_bytes=%d starts_at_zero=%d", len(raw), counts.calls, counts.bytes, counts.starts)
	counts.reset()
	// Append a supported causal-free name record with its next physical ordinal.
	suffix := []byte(`{"schema_version":1,"id":"00000000-0000-4000-8000-000000000077","stream":{"kind":"session","id":"00000000-0000-7000-8000-000000000001"},"sequence":5,"recorded_at":1792083855882800,"record_type":"event","durability":"durable","causation_id":null,"payload_type":"session.name.changed","payload_schema_version":1,"payload":{"session_id":"00000000-0000-7000-8000-000000000001","new_name":"synthetic","previous_name":null,"source":"user","operation_id":"00000000-0000-4000-8000-000000000078"}}` + "\n")
	if e = os.WriteFile(r.ref.Path, append(append([]byte{}, raw...), suffix...), 0600); e != nil {
		t.Fatal(e)
	}
	scan.published = &state.Published{}
	_, ok, e := scan.read()
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	if scan.reg.NativeBinding.Checkpoint.Boundary != int64(len(raw)+len(suffix)) {
		t.Fatal("bound scan did not advance")
	}
	t.Logf("advance: raw=%d previous=%d read_calls=%d read_bytes=%d starts_at_zero=%d", len(raw)+len(suffix), len(raw), counts.calls, counts.bytes, counts.starts)
	counts.reset()
	// Warm exact observation uses no Filter or lookup traversal.
	reader, available := newSourceReader(scan.reg, scan.opts)
	if !available {
		t.Fatal("bound source unavailable")
	}
	before = transcriptFilters.Load()
	a, e := reader.Signature(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	b, e := reader.Signature(t.Context())
	if e != nil || a.observation.Signature != b.observation.Signature || transcriptFilters.Load() != before {
		t.Fatal(e)
	}
	success := state.ScanSignature{SourceSetVersion: sourceSetVersion(scan.reg), SourceSignature: &a.observation.Signature, SourceKind: a.kind, ParserVersion: scan.opts.parserVersionFor("muse"), FilterVersion: archive.FilterVersion, AdapterVersion: (muse.Filter{}).Version()}
	if e = scan.local.SaveScanSignature(scan.id(), success); e != nil {
		t.Fatal(e)
	}
	counts.reset()
	tick := &pass{ctx: t.Context(), local: scan.local, opts: scan.opts}
	unchanged, _, e := tick.unchangedSinceLastScan(scan.reg)
	if e != nil || !unchanged || transcriptFilters.Load() != before {
		t.Fatal("warm collector equality did full Filter", e)
	}
	used, peak := scan.readBudget().Charged()
	t.Logf("warm collector: raw=%d calls=%d bytes=%d starts_at_zero=%d filters=%d live_memory=%d peak_memory=%d", len(raw)+len(suffix), counts.calls, counts.bytes, counts.starts, transcriptFilters.Load()-before, used, peak)

	// Unknown/older disposable tokens force reinspection; none can supply the
	// protected raw checkpoint or become equality to a fresh strong observation.
	for _, token := range []string{"AAAAAAAAAAAAAAAAAAAAAA", "AgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "unknown"} {
		old := success
		old.SourceSignature = &agentapi.SourceSignature{Version: 1, Provider: "file", Token: token}
		if e = scan.local.SaveScanSignature(scan.id(), old); e != nil {
			t.Fatal(e)
		}
		if unchanged, _, e = tick.unchangedSinceLastScan(scan.reg); e != nil || unchanged {
			t.Fatal("unknown token skipped full inspection", e)
		}
	}

}

type nativeFailureMode string

const (
	nativeFailureInvalidSuffix  nativeFailureMode = "invalid-suffix"
	nativeFailurePartialLine    nativeFailureMode = "partial-line"
	nativeFailureFinalSameStamp nativeFailureMode = "final-same-stamp"
	nativeFailureSelection      nativeFailureMode = "selection"
)

func TestPreparedNativeValidationInvalidSuffixAndFinalRewriteCannotBind(t *testing.T) {
	t.Parallel()
	for _, mode := range []nativeFailureMode{nativeFailureInvalidSuffix, nativeFailurePartialLine, nativeFailureFinalSameStamp, nativeFailureSelection} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			scan, r, raw := nativeCollectorFixture(t)
			if mode == nativeFailureInvalidSuffix || mode == nativeFailurePartialLine {
				suffix := []byte("{}\n")
				if mode == nativeFailurePartialLine {
					suffix = []byte("{")
				}
				if e := os.WriteFile(r.ref.Path, append(raw, suffix...), 0600); e != nil {
					t.Fatal(e)
				}
			}
			_, observed, e := r.Filter(t.Context(), muse.Filter{}, 64<<20)
			if mode == nativeFailureInvalidSuffix || mode == nativeFailurePartialLine {
				if e == nil {
					t.Fatal("invalid complete source accepted")
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				if mode == nativeFailureSelection {
					r.nativeLocators.(*validationSelection).changed = true
				} else {
					stamp, e := os.Stat(r.ref.Path)
					if e != nil {
						t.Fatal(e)
					}
					changed := bytes.Replace(raw, []byte("metadata"), []byte("metadatz"), 1)
					if e = os.WriteFile(r.ref.Path, changed, 0600); e != nil {
						t.Fatal(e)
					}
					if e = os.Chtimes(r.ref.Path, stamp.ModTime(), stamp.ModTime()); e != nil {
						t.Fatal(e)
					}
				}
				if e = scan.persistNativeValidation(observed.prepared); e == nil {
					t.Fatal("stale final proof committed")
				}
			}
			current, found, e := scan.local.LoadRegistration(scan.id())
			if e != nil || !found || current.NativeBinding != nil || current.TranscriptPath != "" {
				t.Fatal("failed proof mutated owner", e)
			}
		})
	}
}

func TestNativeFailedObservationCannotBecomeDurableContinuity(t *testing.T) {
	t.Parallel()
	scan, r, raw := nativeCollectorFixture(t)
	_, observed, e := r.Filter(t.Context(), muse.Filter{}, 64<<20)
	if e != nil {
		t.Fatal(e)
	}
	if e = scan.persistNativeValidation(observed.prepared); e != nil {
		t.Fatal(e)
	}
	accepted := scan.reg.NativeBinding
	// A complete invalid B is a negative equality observation, never continuity.
	if e = os.WriteFile(r.ref.Path, append(append([]byte{}, raw...), []byte("{}\n")...), 0600); e != nil {
		t.Fatal(e)
	}
	reader, ok := newSourceReader(scan.reg, scan.opts)
	if !ok {
		t.Fatal("bound unavailable")
	}
	_, failed, e := reader.Filter(t.Context(), muse.Filter{}, 64<<20)
	if e == nil {
		t.Fatal("B accepted")
	}
	if err := rememberFailedRead(scan.local, scan.reg, muse.Filter{}, failed, scan.opts, agentapi.Wrap(agentapi.Unsafe, e), "", nil, ""); err != nil {
		t.Fatal(err)
	}
	tick := &pass{ctx: t.Context(), local: scan.local, opts: scan.opts}
	unchanged, negative, err := tick.unchangedSinceLastScan(scan.reg)
	if err != nil || !unchanged || !negative.Failed {
		t.Fatal("negative cache did not suppress only failed B", err)
	}
	scan.local, e = openTestStore(scan.local.Home())
	if e != nil {
		t.Fatal(e)
	}
	loaded, found, e := scan.local.LoadRegistration(scan.id())
	if e != nil || !found || !loaded.NativeBinding.SameCheckpoint(accepted) {
		t.Fatal("failed cache replaced proof", e)
	}
	// Repair to A plus a supported metadata observation; A remains the old prefix.
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	suffix := bytes.Replace(lines[1], []byte(`"sequence":3`), []byte(`"sequence":5`), 1)
	suffix = bytes.Replace(suffix, []byte("0000000003eb"), []byte("000000000477"), 1)
	fixed := append(append(append([]byte{}, raw...), suffix...), '\n')
	if e = os.WriteFile(r.ref.Path, fixed, 0600); e != nil {
		t.Fatal(e)
	}
	scan.reg = loaded
	scan.published = &state.Published{}
	if _, ok, e = scan.read(); e != nil || !ok {
		t.Fatal("repaired C did not prove A", e)
	}
	if scan.reg.NativeBinding.Checkpoint.Boundary != int64(len(fixed)) {
		t.Fatal("C not committed")
	}
	// Accepted validation is durable even before candidate staging. After a crash,
	// losing/rewriting those native bytes conservatively refuses continuation.
	scan.local, e = openTestStore(scan.local.Home())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(r.ref.Path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e = scan.read(); e == nil {
		t.Fatal("crash milestone reconstructed missing proof")
	}
}

// These counters observe actual descriptor reads, including implicit proof
// verification and startup prefetch; the memory ledger does not count disk I/O.
type nativeReadCounts struct {
	calls  int
	starts int
	opens  int
	closes int
	bytes  int64
}

func (c *nativeReadCounts) reset() { *c = nativeReadCounts{} }

type countedNativeOpener struct {
	transcriptio.Opener
	counts *nativeReadCounts
}

func (o countedNativeOpener) OpenRegular(path string) (transcriptio.File, error) {
	f, e := o.Opener.OpenRegular(path)
	if e != nil {
		return nil, e
	}
	o.counts.opens++
	return countedNativeFile{File: f, counts: o.counts}, nil
}

type countedNativeFile struct {
	transcriptio.File
	counts *nativeReadCounts
}

func (f countedNativeFile) ReadAt(b []byte, off int64) (int, error) {
	n, e := f.File.ReadAt(b, off)
	f.counts.calls++
	f.counts.bytes += int64(n)
	if off == 0 {
		f.counts.starts++
	}
	return n, e
}

func TestNativeValidationReadCostsScaleWithRawBoundary(t *testing.T) {
	// Sequential: compares the process-wide actual Filter counter.
	for _, names := range []int{128, 2048} {
		t.Run(strconv.Itoa(names), func(t *testing.T) {
			scan, r, raw := nativeCollectorFixture(t)
			var body bytes.Buffer
			body.Write(raw)
			for i := range names {
				fmt.Fprintf(&body, `{"schema_version":1,"id":"00000000-0000-4000-8000-%012x","stream":{"kind":"session","id":"00000000-0000-7000-8000-000000000001"},"sequence":%d,"recorded_at":1792083859982704,"record_type":"event","durability":"durable","causation_id":null,"payload_type":"session.name.changed","payload_schema_version":1,"payload":{"session_id":"00000000-0000-7000-8000-000000000001","new_name":"synthetic bounded name","previous_name":null,"source":"user","operation_id":"synthetic"}}`+"\n", i+1, i+5)
			}
			if e := os.WriteFile(r.ref.Path, body.Bytes(), 0600); e != nil {
				t.Fatal(e)
			}
			counts := scan.opts.Sources.(validationSources).counts
			before := transcriptFilters.Load()
			_, observed, e := r.Filter(t.Context(), muse.Filter{}, 64<<20)
			if e != nil {
				t.Fatal(e)
			}
			if e = scan.persistNativeValidation(observed.prepared); e != nil {
				t.Fatal(e)
			}
			if transcriptFilters.Load() != before+1 {
				t.Fatal("full Filter count")
			}
			t.Logf("initial raw=%d calls=%d bytes=%d starts=%d", body.Len(), counts.calls, counts.bytes, counts.starts)
			if counts.bytes != 6*int64(body.Len())+4096 {
				t.Fatal("unaccounted raw passes", counts.bytes)
			}
			scan.releaseRetained()
			success := state.ScanSignature{SourceSetVersion: sourceSetVersion(scan.reg), SourceSignature: &observed.observation.Signature, SourceKind: observed.kind, ParserVersion: scan.opts.parserVersionFor("muse"), FilterVersion: archive.FilterVersion, AdapterVersion: (muse.Filter{}).Version()}
			if e = scan.local.SaveScanSignature(scan.id(), success); e != nil {
				t.Fatal(e)
			}
			counts.reset()
			before = transcriptFilters.Load()
			tick := &pass{ctx: t.Context(), local: scan.local, opts: scan.opts}
			unchanged, _, e := tick.unchangedSinceLastScan(scan.reg)
			if e != nil || !unchanged || transcriptFilters.Load() != before {
				t.Fatal("warm work", e)
			}
			used, peak := scan.readBudget().Charged()
			t.Logf("warm raw=%d calls=%d bytes=%d starts=%d filters=%d live_memory=%d peak_memory=%d", body.Len(), counts.calls, counts.bytes, counts.starts, transcriptFilters.Load()-before, used, peak)
			if counts.bytes != 2*int64(body.Len())+4096 || used != 0 {
				t.Fatal("warm read cost or memory imbalance", counts.bytes, used)
			}
		})
	}
}

func TestNativeFinalVerificationAfterStageRefusesMutationAndRunsOutsideHooks(t *testing.T) {
	t.Parallel()
	scan, r, raw := nativeCollectorFixture(t)
	_, observed, e := r.Filter(t.Context(), muse.Filter{}, 64<<20)
	if e != nil {
		t.Fatal(e)
	}
	snapshot := observed.prepared.snapshot.(*validationSnapshot)
	called := false
	snapshot.beforeVerify = func() error {
		called = true
		unlock, e := local.NamedLockWait(scan.local.Home(), "hooks.lock", 0)
		if e != nil {
			return e
		}
		unlock()
		stamp, e := os.Stat(r.ref.Path)
		if e != nil {
			return e
		}
		changed := bytes.Replace(raw, []byte("metadata"), []byte("metadatz"), 1)
		if e = os.WriteFile(r.ref.Path, changed, 0600); e != nil {
			return e
		}
		return os.Chtimes(r.ref.Path, stamp.ModTime(), stamp.ModTime())
	}
	if e = scan.persistNativeValidation(observed.prepared); e == nil || !called {
		t.Fatal("after-stage source mutation committed", e)
	}
	current, found, e := scan.local.LoadRegistration(scan.id())
	if e != nil || !found || current.NativeBinding != nil {
		t.Fatal("failed final source proof bound", e)
	}
}

type decoratedMuseFilter struct{ muse.Filter }

func TestNativePreparedProofDeclinesPromotedFilterCapability(t *testing.T) {
	t.Parallel()
	scan, r, _ := nativeCollectorFixture(t)
	if _, _, e := r.Filter(t.Context(), decoratedMuseFilter{}, 64<<20); e == nil {
		t.Fatal("promoted filter capability committed")
	}
	current, found, e := scan.local.LoadRegistration(scan.id())
	if e != nil || !found || current.NativeBinding != nil {
		t.Fatal("decorated Filter acquired authority", e)
	}
}

func (s validationSources) LookupParser(name string) (agentapi.TranscriptParser, bool) {
	if name != "muse" {
		return nil, false
	}
	return muse.Parser{}, true
}

func TestNativeCheckpointFingerprintStaysOutOfPortableArtifacts(t *testing.T) {
	t.Parallel()
	scan, r, _ := nativeCollectorFixture(t)
	filtered, observed, e := r.Filter(t.Context(), muse.Filter{}, 64<<20)
	if e != nil {
		t.Fatal(e)
	}
	if e = scan.persistNativeValidation(observed.prepared); e != nil {
		t.Fatal(e)
	}
	bundle, e := archive.NewSourceBundle(scan.reg, muse.Filter{}, filtered, time.Now(), nil)
	if e != nil {
		t.Fatal(e)
	}
	analysis, e := (muse.Parser{}).Parse(t.Context(), bundle)
	if e != nil {
		t.Fatal(e)
	}
	handoff, e := archive.BuildHandoffWithAnalysis(bundle, analysis, nil, archive.HandoffOptions{})
	if e != nil {
		t.Fatal(e)
	}
	metadata, e := archive.BuildMetadataWithAnalysis(bundle, analysis, nil, "synthetic", scan.reg.SessionStartedAt, time.Now(), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, archive.ParserInfo{Version: (muse.Parser{}).Version()})
	if e != nil {
		t.Fatal(e)
	}
	for _, value := range []any{bundle, metadata, handoff} {
		raw, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		if bytes.Contains(raw, []byte(scan.reg.NativeBinding.Checkpoint.SHA256)) || bytes.Contains(raw, []byte("muse-ordinary")) {
			t.Fatal("raw proof escaped local registration")
		}
	}
}

func TestNativeFrozenRetryDoesNotReadNewerBytesOrAdvanceCheckpoint(t *testing.T) {
	t.Parallel()
	scan, r, _ := nativeCollectorFixture(t)
	raw, e := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-conversation.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	raw = bytes.ReplaceAll(raw, []byte("/work/synthetic-muse"), []byte(scan.reg.ProjectRoot))
	if e = os.WriteFile(r.ref.Path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	_, observed, e := r.Filter(t.Context(), muse.Filter{}, 64<<20)
	if e != nil {
		t.Fatal(e)
	}
	if e = scan.persistNativeValidation(observed.prepared); e != nil {
		t.Fatal(e)
	}
	accepted := *scan.reg.NativeBinding.Checkpoint
	remote := &historyCrashStore{MemoryStore: storagetest.NewMemoryStore(), fail: 1}
	scan.remote = remote
	scan.opts.Parsers = scan.opts.Sources.(validationSources)
	scan.opts.MachineID = "synthetic"
	scan.published, e = scan.local.LoadPublishedState(scan.id())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = scan.run(t.Context()); e == nil {
		t.Fatal("publication did not stop at injected first remote operation")
	}
	pending, found, e := scan.local.LoadPending(scan.id())
	if e != nil || !found || !pending.Attempted {
		t.Fatal("missing frozen work", e)
	}
	if e = os.WriteFile(r.ref.Path, append(raw, []byte("{}\n")...), 0600); e != nil {
		t.Fatal(e)
	}
	counts := scan.opts.Sources.(validationSources).counts
	counts.reset()
	remote.fail = 0
	if _, handled, e := scan.resume(t.Context()); e != nil || !handled {
		t.Fatal("frozen retry", handled, e)
	}
	if counts.calls != 0 {
		t.Fatal("frozen retry read newer native bytes", counts.calls)
	}
	current, found, e := scan.local.LoadRegistration(scan.id())
	if e != nil || !found || *current.NativeBinding.Checkpoint != accepted {
		t.Fatal("acknowledgement minted newer raw proof", e)
	}
}

func TestNativeValidationSharedMemoryPressureReleasesAndRetries(t *testing.T) {
	t.Parallel()
	scan, r, _ := nativeCollectorFixture(t)
	budget := agentapi.NewNativeReadBudget(2 << 20)
	scan.retainedBudget = budget
	pressure := budget.Reservation()
	held := int64(2<<20) - 64<<10
	if !pressure.Reserve(held) {
		t.Fatal("synthetic pressure")
	}
	_, _, e := r.Filter(t.Context(), muse.Filter{}, 64<<20)
	if !errors.Is(e, agentapi.ErrReadBudget) {
		t.Fatal("pressure did not defer", e)
	}
	if used, _ := budget.Charged(); used != held {
		t.Fatal("refusal leaked scratch", used)
	}
	current, _, e := scan.local.LoadRegistration(scan.id())
	if e != nil || current.NativeBinding != nil {
		t.Fatal("memory refusal minted proof", e)
	}
	pressure.Release(held)
	_, observed, e := r.Filter(t.Context(), muse.Filter{}, 64<<20)
	if e != nil {
		t.Fatal("independent retry starved", e)
	}
	if e = scan.persistNativeValidation(observed.prepared); e != nil {
		t.Fatal(e)
	}
	scan.releaseRetained()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("retry leaked memory", used)
	}
}

func TestNativeValidationCancellationBeforeCommitLeavesPendingOwner(t *testing.T) {
	t.Parallel()
	scan, r, _ := nativeCollectorFixture(t)
	_, observed, e := r.Filter(t.Context(), muse.Filter{}, 64<<20)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	scan.ctx = ctx
	if e = scan.persistNativeValidation(observed.prepared); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if !observed.prepared.closed {
		t.Fatal("cancelled source lease retained")
	}
	current, _, e := scan.local.LoadRegistration(scan.id())
	if e != nil || current.NativeBinding != nil {
		t.Fatal("cancelled validation minted proof", e)
	}
}

func (f countedNativeFile) Close() error { f.counts.closes++; return f.File.Close() }

func TestNativeRecoveryWithoutCommitOwnerRefusesAndClosesSnapshot(t *testing.T) {
	t.Parallel()
	scan, r, _ := nativeCollectorFixture(t)
	_, observed, e := r.Filter(t.Context(), muse.Filter{}, 64<<20)
	if e != nil {
		t.Fatal(e)
	}
	if e = scan.persistNativeValidation(observed.prepared); e != nil {
		t.Fatal(e)
	}
	scan.releaseRetained()
	counts := scan.opts.Sources.(validationSources).counts
	counts.reset()
	builder, release, e := PrepareGenerationRecovery(t.Context(), scan.reg, time.Now(), scan.opts)
	release()
	if e == nil || builder != nil || counts.opens == 0 || counts.opens != counts.closes {
		t.Fatal("unsupported native recovery retained a source lease", e, counts.opens, counts.closes)
	}
}
