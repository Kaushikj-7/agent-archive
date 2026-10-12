package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type reconciliationLookup struct {
	set     agentapi.CodexRolloutSet
	refs    map[string][]agentapi.SourceRef
	changed bool
	reads   int
}

func (l *reconciliationLookup) Thread(context.Context, string) (agentapi.CodexRolloutSet, error) {
	l.reads++
	return l.set, nil
}

func (l *reconciliationLookup) Rollout(_ context.Context, id string) ([]agentapi.SourceRef, error) {
	return l.refs[id], nil
}

func (l *reconciliationLookup) Check(context.Context, string, string) error {
	if l.changed {
		return agentapi.Wrap(agentapi.Changed, errors.New("synthetic source changed"))
	}
	return nil
}

func reconciliationNative(t *testing.T, root, id string, start uint64, base bool, texts ...string) agentapi.SourceRef {
	t.Helper()
	meta := map[string]any{"id": revisionThread, "cwd": root, "timestamp": "2026-10-01T12:00:00Z", "cli_version": "0.160.0", "originator": "codex_cli_rs", "source": "cli", "history_mode": "paginated"}
	if base {
		raw, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("rollout-2026-10-01T12-00-00-%s.jsonl", revisionThread)))
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.SplitAfter(raw, []byte("\n"))
		meta["history_base"] = map[string]any{"thread_id": revisionThread, "end_ordinal_exclusive": 2, "end_byte_offset": len(lines[0]) + len(lines[1])}
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

func reconciliationFixture(t *testing.T) (*sessionScan, *reconciliationLookup) {
	t.Helper()
	root := t.TempDir()
	a := reconciliationNative(t, root, revisionThread, 0, false, "common", "A-only")
	b := reconciliationNative(t, root, revisionB, 2, true, "B-only")
	c := reconciliationNative(t, root, revisionC, 2, true, "C-only")
	lookup := &reconciliationLookup{set: agentapi.CodexRolloutSet{Current: &c, Candidates: []agentapi.SourceRef{a, b, c}, Revision: "complete", Complete: true}, refs: map[string][]agentapi.SourceRef{revisionThread: {a}, revisionB: {b}, revisionC: {c}}}
	local := newTestStore(t)
	reg := registration(t, a.Path)
	reg.NativeSessionID = revisionThread
	reg.ProjectRoot = root
	reg.Origin = archive.SessionOriginHook
	reg.SessionStartedAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	reg.RegisteredAt = reg.SessionStartedAt.Add(time.Second)
	reg.CodexBinding = &archive.CodexSourceBinding{Version: 1, NativeThreadID: revisionThread, NativeCreatedAt: reg.SessionStartedAt, Cwd: root, ProducerSource: "cli", RootID: revisionThread, PhysicalRolloutID: revisionThread, Path: a.Path, Home: root}
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{MachineID: "machine", Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: root, ProjectID: reg.ProjectID, Included: true, ActivatedAt: reg.SessionStartedAt}}}}
	if err := config.Save(local.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	opts := Options{MachineID: "machine", Sources: testSources, Parsers: testParsers, CodexRollouts: lookup, Now: func() time.Time { return now }}
	return newSessionScan(t.Context(), local, storagetest.NewMemoryStore(), reg, state.Request{}, published, now, opts), lookup
}

func TestNativeRevisionReconciliationFirstABCUsesActualPorts(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal("native read", err)
	}
	active, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := scan.reconcileRevisions(read, active)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Current != revisionC || len(plan.Preserved) != 2 {
		t.Fatalf("lost never-published A or B suffix: %#v", plan)
	}
	for _, ref := range plan.Preserved {
		if !ref.CapturedAt.Equal(scan.now) {
			t.Fatal("invented historical capture time")
		}
	}
	// A restart with unchanged source produces exactly the same plan bytes/times.
	again, err := scan.reconcileRevisions(read, active)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(plan.Preserved)
	second, _ := json.Marshal(again.Preserved)
	if !bytes.Equal(first, second) {
		t.Fatal("reconciliation is not deterministic within frozen observation")
	}
}

func TestRunRevisionReconciliationStaysPendingWithoutAcknowledgement(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(strconv.FormatBool(incomplete), func(t *testing.T) {
			scan, lookup := reconciliationFixture(t)
			lookup.set.Complete = !incomplete
			if err := scan.local.SaveRequest(scan.id(), "stop", scan.now); err != nil {
				t.Fatal(err)
			}
			req, found, err := scan.local.LoadRequest(scan.id())
			if err != nil || !found {
				t.Fatal(err)
			}
			scan.req = req
			outcome, err := scan.run()
			if outcome != outcomeSkipped || err == nil {
				t.Fatal("reconciliation settled", outcome, err)
			}
			if incomplete && !agentapi.HasFailure(err, agentapi.Unavailable) {
				t.Fatal("incomplete coverage not pending", err)
			}
			if !incomplete && !errors.Is(err, archive.ErrHistoryMutationPending) {
				t.Fatal("publication fence lost", err)
			}
			current, found, err := scan.local.LoadRequest(scan.id())
			if err != nil || !found || current.Token != req.Token {
				t.Fatal("request acknowledged before lifecycle", err)
			}
			if _, found, err := scan.local.LoadScanSignature(scan.id()); err != nil || found {
				t.Fatal("pending reconciliation settled signature", err)
			}
			objects, err := scan.remote.List(t.Context(), "")
			if err != nil || len(objects) != 0 {
				t.Fatal("fenced reconciliation wrote remote", err)
			}
		})
	}
}

func TestNativeRevisionReconciliationChangedCancellationAndLimits(t *testing.T) {
	type scenarioVariant0 string
	const (
		scenarioChanged0  scenarioVariant0 = "changed"
		scenarioCancel0   scenarioVariant0 = "cancel"
		scenarioRaw0      scenarioVariant0 = "raw"
		scenarioFiltered0 scenarioVariant0 = "filtered"
		scenarioRecords0  scenarioVariant0 = "records"
	)
	for _, scenario := range []scenarioVariant0{scenarioChanged0, scenarioCancel0, scenarioRaw0, scenarioFiltered0, scenarioRecords0} {
		t.Run(string(scenario), func(t *testing.T) {
			scan, lookup := reconciliationFixture(t)
			read, ok, err := scan.read()
			if err != nil || !ok {
				t.Fatal(err)
			}
			active, _, err := scan.build(read)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case scenarioChanged0:
				lookup.changed = true
			case scenarioCancel0:
				ctx, cancel := context.WithCancel(scan.ctx)
				cancel()
				scan.ctx = ctx
			case scenarioRaw0:
				scan.opts.MaxTranscriptBytes = 1
			case scenarioFiltered0, scenarioRecords0:
				// The source filter receives a retained record budget before accumulation.
				provider, _ := newSourceReader(scan.reg, scan.opts)
				p := provider.(providerReader)
				source, _, err := p.binding()
				if err != nil {
					t.Fatal(err)
				}
				pass, err := source.OpenPass(scan.ctx, agentapi.SourceEnvironment{CodexRollouts: lookup})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = pass.Close() }()
				snapshot, err := pass.Read(scan.ctx, p.ref, agentapi.ReadLimits{})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = snapshot.Close() }()
				limits := agentapi.ReadLimits{Records: 1, FilteredBytes: 128 << 20}
				if scenario == scenarioFiltered0 {
					limits = agentapi.ReadLimits{Records: archive.MaxHistoryRecords, FilteredBytes: 1}
				}
				_, err = read.adapter.Filter(scan.ctx, snapshot.Input(), agentapi.FilterContext{Limits: limits})
				if err == nil {
					t.Fatal("retained evidence limit ignored")
				}
				return
			}
			if _, err := scan.reconcileRevisions(read, active); err == nil {
				t.Fatal("uncertain reconciliation accepted")
			}
		})
	}
}

func TestNativeRevisionReconciliationRateLimitedOutgoingAppendAndMissingFile(t *testing.T) {
	for _, appendSuffix := range []bool{false, true} {
		t.Run(strconv.FormatBool(appendSuffix), func(t *testing.T) {
			scan, lookup := reconciliationFixture(t)
			b := lookup.refs[revisionB][0]
			lookup.set.Current = &b
			read, ok, err := scan.read()
			if err != nil || !ok {
				t.Fatal(err)
			}
			outgoing, _, err := scan.build(read)
			if err != nil {
				t.Fatal(err)
			}
			captured := scan.now.Add(-time.Hour)
			outgoing.Capture.CapturedAt = captured
			if err := scan.published.Save(outgoing, captured, state.CacheStatusRateLimited); err != nil {
				t.Fatal(err)
			}
			if appendSuffix {
				reconciliationNative(t, scan.reg.ProjectRoot, revisionB, 2, true, "B-only", "never-observed")
			} else {
				if err := os.Remove(b.Path); err != nil {
					t.Fatal(err)
				}
				lookup.set.Candidates = []agentapi.SourceRef{lookup.refs[revisionThread][0], lookup.refs[revisionC][0]}
			}
			c := lookup.refs[revisionC][0]
			lookup.set.Current = &c
			read, ok, err = scan.read()
			if err != nil || !ok {
				t.Fatal(err)
			}
			active, _, err := scan.build(read)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := scan.reconcileRevisions(read, active)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, ref := range plan.Preserved {
				if ref.RevisionID == revisionB {
					found = true
					expected := captured
					if appendSuffix {
						expected = scan.now
					}
					if !ref.CapturedAt.Equal(expected) {
						t.Fatal("wrong outgoing observation time")
					}
				}
			}
			if !found {
				t.Fatal("latest verified outgoing candidate was lost")
			}
		})
	}
}

func TestNativeRevisionReconciliationOrdinaryAppendAndUnchangedCost(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	a := lookup.refs[revisionThread][0]
	lookup.set.Current = &a
	lookup.set.Candidates = []agentapi.SourceRef{a}
	outcome, err := scan.run()
	if err != nil || outcome != outcomePublished {
		t.Fatal("ordinary initial publication", outcome, err)
	}
	// No ordinary append creates a preserved revision.
	scan.opts.Now = func() time.Time { return scan.now.Add(time.Hour) }
	reconciliationNative(t, scan.reg.ProjectRoot, revisionThread, 0, false, "common", "A-only", "append")
	result, err := Run(t.Context(), scan.local, scan.remote, scan.opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal("ordinary append", result, err)
	}
	metadata := fetchMetadata(t, scan.remote, "codex", scan.id())
	if metadata.History != nil {
		t.Fatal("ordinary append grew history")
	}
	for range 2 {
		if result, cost := measurePass(t, scan.local, scan.remote, scan.opts); len(result.Errors) != 0 || cost.loads != 0 || cost.writes != 0 {
			t.Fatalf("unchanged modern ordinary source did work: %#v %+v", result, cost)
		}
	}
}

func TestNativeRevisionReconciliationPendingCandidateSurvivesMissingOutgoingFile(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	b := lookup.refs[revisionB][0]
	lookup.set.Current = &b
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	outgoing, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	captured := scan.now.Add(-time.Hour)
	outgoing.Capture.CapturedAt = captured
	rendered, err := renderPublication(scan.ctx, scan.resolveParser(), scan.parserVersion(), outgoing, scan.reg, scan.now, scan.opts, scan.priorRepoKey)
	if err != nil {
		t.Fatal(err)
	}
	pending := state.PendingPublication{Bundle: outgoing, SourceKey: rendered.source.Key, SourceSHA256: rendered.source.SHA256, SourceBytes: rendered.sourceBytes, MetadataKey: rendered.metadataKey, MetadataBytes: rendered.metadata}
	scan.revisions = &revisionPlan{Current: revisionB}
	if err := scan.freezeRevisionPublication(&pending); err != nil {
		t.Fatal(err)
	}
	if err := scan.local.SavePending(scan.id(), pending); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(b.Path); err != nil {
		t.Fatal(err)
	}
	c := lookup.refs[revisionC][0]
	lookup.set.Current = &c
	lookup.set.Candidates = []agentapi.SourceRef{lookup.refs[revisionThread][0], c}
	read, ok, err = scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	active, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := scan.reconcileRevisions(read, active)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ref := range plan.Preserved {
		if ref.RevisionID == revisionB {
			found = true
			if !ref.CapturedAt.Equal(captured) {
				t.Fatal("pending capture time changed")
			}
		}
	}
	if !found {
		t.Fatal("pending candidate absent from complete plan")
	}
	current, found, err := scan.local.LoadPending(scan.id())
	if err != nil || !found || !bytes.Equal(current.SourceBytes, pending.SourceBytes) {
		t.Fatal("planning modified frozen candidate", err)
	}
}

func TestNativeRevisionReconciliationReactivationAndExistingCaptureTimes(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	active, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := scan.reconcileRevisions(read, active)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderPublication(scan.ctx, scan.resolveParser(), scan.parserVersion(), active, scan.reg, scan.now, scan.opts, scan.priorRepoKey)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(rendered.metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.History.Preserved = plan.Preserved
	for _, stage := range plan.Sources {
		if err := scan.remote.Put(t.Context(), stage.Reference.Key, stage.Bytes); err != nil {
			t.Fatal(err)
		}
	}
	if err := scan.remote.Put(t.Context(), rendered.source.Key, rendered.sourceBytes); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := scan.remote.Put(t.Context(), rendered.metadataKey, raw); err != nil {
		t.Fatal(err)
	}
	if err := scan.published.SavePublication(active, scan.now, rendered.source, raw); err != nil {
		t.Fatal(err)
	}
	b := lookup.refs[revisionB][0]
	lookup.set.Current = &b
	scan.now = scan.now.Add(time.Hour)
	read, ok, err = scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	reactivated, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	next, err := scan.reconcileRevisions(read, reactivated)
	if err != nil {
		t.Fatal(err)
	}
	if next.Current != revisionB || len(next.Preserved) != 2 {
		t.Fatalf("reactivation lost retained set: %#v", next)
	}
	for _, ref := range next.Preserved {
		if ref.RevisionID == revisionB {
			t.Fatal("active revision remained preserved")
		}
		if !ref.CapturedAt.Equal(active.Capture.CapturedAt) {
			t.Fatal("unchanged historical evidence reset capture age")
		}
	}
}

func TestNativeRevisionReconciliationMissingBaseRemainsPending(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	active, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(lookup.refs[revisionThread][0].Path); err != nil {
		t.Fatal(err)
	}
	if _, err := scan.reconcileRevisions(read, active); err == nil {
		t.Fatal("missing base accepted")
	}
	if _, found, err := scan.local.LoadScanSignature(scan.id()); err != nil || found {
		t.Fatal("missing base settled", err)
	}
}

func TestNativeRevisionReconciliationUnknownSourceHomeRemainsPending(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	scan.reg.CodexBinding.Home = ""
	provider, _ := newSourceReader(scan.reg, scan.opts)
	adapter, err := sourceAdapter(scan.opts.Sources, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.Filter(scan.ctx, adapter, scan.opts.maxTranscriptBytes()); !agentapi.HasFailure(err, agentapi.Unavailable) {
		t.Fatal("unknown source home authorized history reads before reconciliation", err)
	}
}

func TestNativeRevisionReconciliationIncludesRefreshedRemoteActiveAuthority(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	a := lookup.refs[revisionThread][0]
	lookup.set.Current = &a
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	older, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	oldRendered, err := renderPublication(scan.ctx, scan.resolveParser(), scan.parserVersion(), older, scan.reg, scan.now, scan.opts, scan.priorRepoKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := scan.published.SavePublication(older, scan.now, oldRendered.source, oldRendered.metadata); err != nil {
		t.Fatal(err)
	}
	b := lookup.refs[revisionB][0]
	lookup.set.Current = &b
	read, ok, err = scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	remoteActive, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderPublication(scan.ctx, scan.resolveParser(), scan.parserVersion(), remoteActive, scan.reg, scan.now, scan.opts, scan.priorRepoKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := scan.remote.Put(t.Context(), rendered.source.Key, rendered.sourceBytes); err != nil {
		t.Fatal(err)
	}
	if err := scan.remote.Put(t.Context(), rendered.metadataKey, rendered.metadata); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(b.Path); err != nil {
		t.Fatal(err)
	}
	c := lookup.refs[revisionC][0]
	lookup.set.Current = &c
	lookup.set.Candidates = []agentapi.SourceRef{a, c}
	read, ok, err = scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	active, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := scan.reconcileRevisions(read, active)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ref := range plan.Preserved {
		if ref.RevisionID == revisionB {
			found = true
		}
	}
	if !found {
		t.Fatal("newly verified remote active evidence lost after recovery")
	}
}

func TestNativeRevisionReconciliationUsesRawOrdinalsAfterFilteredOmissions(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	a := lookup.refs[revisionThread][0]
	raw, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	var rewritten bytes.Buffer
	encoder := json.NewEncoder(&rewritten)
	for i, line := range lines {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		ordinal := i
		if i > 0 {
			ordinal += 2
		}
		record["ordinal"] = ordinal
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			for n := range 2 {
				if err := encoder.Encode(map[string]any{"type": "synthetic_omitted", "ordinal": n + 1}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := os.WriteFile(a.Path, rewritten.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{revisionB, revisionC} {
		ref := lookup.refs[id][0]
		raw, err := os.ReadFile(ref.Path)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		for i, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
			var record map[string]any
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatal(err)
			}
			record["ordinal"] = 5 + i
			if i == 0 {
				record["payload"].(map[string]any)["history_base"] = map[string]any{"thread_id": revisionThread, "end_ordinal_exclusive": 5, "end_byte_offset": rewritten.Len()}
			}
			if err := enc.Encode(record); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(ref.Path, out.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal(err)
	}
	active, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := scan.reconcileRevisions(read, active)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Preserved) != 1 || plan.Preserved[0].RevisionID != revisionB {
		t.Fatalf("filtered positions mistaken for raw ordinal evidence: %#v", plan.Preserved)
	}
	if len(plan.Sources) != 1 || plan.Sources[0].Bundle.History == nil {
		t.Fatal("historical originals lost validated ordinal framing")
	}
}

func TestNativeReconciliationDoesNotTreatMissingSourceBookkeepingAsRevision(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	defer scan.releaseRetained()
	if err := scan.published.SaveBlocked(archive.SourceBundle{}, time.Time{}, state.BlockedReasonTranscriptMissing); err != nil {
		t.Fatal(err)
	}
	read, ok, err := scan.read()
	if err != nil || !ok {
		t.Fatal("returned native source", err)
	}
	active, _, err := scan.build(read)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := scan.reconcileRevisions(read, active)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Current != revisionC || len(plan.Preserved) != 2 {
		t.Fatalf("lost real outgoing revisions %#v", plan)
	}
	for _, revision := range plan.Preserved {
		if revision.RevisionID == "" {
			t.Fatal("invented empty revision")
		}
	}
}
