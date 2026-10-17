package collector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/state"
)

// revisionD is an off-chain rollout of the thread that does not read as one.
const revisionD = "44444444-4444-4444-8444-444444444444"

// offChainTexts is n synthetic transcript messages of size bytes each that
// compress little, so a revision staged from them retains about as much as
// it reads.
func offChainTexts(seed string, n, size int) []string {
	texts := make([]string, 0, n)
	sum := sha256.Sum256([]byte(seed))
	for range n {
		var b strings.Builder
		for b.Len() < size {
			b.WriteString(hex.EncodeToString(sum[:]))
			sum = sha256.Sum256(sum[:])
		}
		texts = append(texts, b.String()[:size])
	}
	return texts
}

// largeOffChainFixture is reconciliationFixture with a large off-chain
// rollout B, which fits on its own but whose retained bytes leave
// publishing the chain short of the session's read ceiling.
func largeOffChainFixture(t *testing.T) (*sessionScan, *reconciliationLookup) {
	t.Helper()
	scan, lookup := reconciliationFixture(t)
	reconciliationNative(t, scan.reg.ProjectRoot, revisionB, 2, true, offChainTexts("b", 1024, 2<<10)...)
	return scan, lookup
}

// runUnderCeiling runs a scan of a fresh large fixture with the session's
// read ceiling at ceiling, with or without its off-chain revisions. The
// scan's scope stays current until the test ends, so the test can inspect it.
func runUnderCeiling(t *testing.T, ceiling int64, offChain bool) (*sessionScan, error) {
	t.Helper()
	scan, _ := largeOffChainFixture(t)
	scan.withoutOffChain = !offChain
	t.Cleanup(scan.readBudget().BeginScope(ceiling))
	_, err := scan.run(scan.ctx)
	if err != nil && !errors.Is(err, archive.ErrHistoryMutationPending) && !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal(err)
	}
	return scan, err
}

// capturedChain reports whether the scan froze a publication of the thread,
// and the revisions it preserves.
func capturedChain(t *testing.T, scan *sessionScan, err error) (bool, []string) {
	t.Helper()
	if !errors.Is(err, archive.ErrHistoryMutationPending) {
		return false, nil
	}
	pending, found, loadErr := scan.local.LoadPending(scan.id())
	if loadErr != nil || !found || pending.History == nil {
		t.Fatal("no frozen publication", loadErr)
	}
	var ids []string
	for _, input := range pending.History.Inputs {
		ids = append(ids, input.RevisionID)
	}
	return true, ids
}

// Off-chain revisions that fit while reconciling, but whose retained bytes
// leave publishing past the session's read ceiling, do not make the whole
// session too large to read: the scan runs again without them and captures
// the thread's own chain, A then C. The ceiling leaves the chain alone 1 MiB
// of room, less than B retains; with room enough, B is kept too.
func TestOffChainRevisionsThatCrowdOutPublishingAreLeftOut(t *testing.T) {
	t.Parallel()
	scan, err := runUnderCeiling(t, 64<<20, false)
	if !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatal("the chain alone does not capture: ", err)
	}
	_, peak := scan.readBudget().Charged()
	ceiling := peak + 1<<20
	if _, err := runUnderCeiling(t, ceiling, false); !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatal("the chain alone does not capture under the ceiling: ", err)
	}
	// With room enough, B is read and kept.
	scan, err = runUnderCeiling(t, 64<<20, true)
	captured, ids := capturedChain(t, scan, err)
	if !captured || scan.withoutOffChain || !slices.Contains(ids, revisionB) {
		t.Fatalf("B was not kept with room for it: captured %v, inputs %v, retried %v", captured, ids, scan.withoutOffChain)
	}
	// Under the ceiling, B was kept while reconciling, then publishing met
	// the ceiling.
	scan, err = runUnderCeiling(t, ceiling, true)
	captured, ids = capturedChain(t, scan, err)
	if !captured {
		t.Fatal("off-chain revisions kept the chain from being captured: ", err)
	}
	if !scan.withoutOffChain {
		t.Fatal("the scan did not meet the ceiling with B kept; the fixture no longer exercises the retry")
	}
	if !slices.Contains(ids, revisionThread) || !slices.Contains(ids, revisionC) || slices.Contains(ids, revisionB) {
		t.Fatalf("captured revisions %v", ids)
	}
	if scan.readBudget().ScopeExceeded() {
		t.Fatal("the first scan's ceiling refusal outlived it, so a later refusal would be misread as the session's own")
	}
	if scan.sizeLimit != nil || scan.gap != "" {
		t.Fatalf("recorded a gap %q (%+v)", scan.gap, scan.sizeLimit)
	}
	if !hasRevisionLeftOut(t, scan) {
		t.Fatal("no diagnostic for the off-chain revision left out")
	}
}

// A scan that kept an off-chain revision and saved its publication before
// meeting the session's read ceiling is not run again without it: the
// publication is owed as saved, B included, and the refusal defers the
// session (overCeiling) rather than recording it as too large. The ceiling
// sits just under the scan's peak with B kept, which it reaches only after
// saving the publication; the fixture checks that it still does.
func TestOffChainCeilingAfterTheSavedPublicationIsNotRetried(t *testing.T) {
	t.Parallel()
	scan, err := runUnderCeiling(t, 64<<20, true)
	if captured, ids := capturedChain(t, scan, err); !captured || !slices.Contains(ids, revisionB) {
		t.Fatalf("B was not kept with room for it: captured %v, inputs %v", captured, ids)
	}
	_, peak := scan.readBudget().Charged()
	scan, err = runUnderCeiling(t, peak-2<<10, true)
	if scan.withoutOffChain {
		t.Fatal("the scan ran again without B although its publication was owed")
	}
	if err == nil || !scan.overSessionCeiling(err) {
		t.Fatal("the scan did not meet the ceiling; the fixture no longer exercises a refusal after saving: ", err)
	}
	pending, found, loadErr := scan.local.LoadPending(scan.id())
	if loadErr != nil || !found || pending.History == nil {
		t.Fatal("the scan met the ceiling before saving its publication; the fixture no longer exercises the guard", loadErr)
	}
	var ids []string
	for _, input := range pending.History.Inputs {
		ids = append(ids, input.RevisionID)
	}
	if !slices.Contains(ids, revisionB) || !slices.Contains(ids, revisionC) {
		t.Fatalf("owed publication inputs %v", ids)
	}
	if hasRevisionLeftOut(t, scan) {
		t.Fatal("recorded a diagnostic with nothing left out")
	}
	// The owed upload is deferred, not recorded as a gap; a revision left
	// out of it would then be reported, since the session is not a gap.
	outcome, deferred := scan.overCeiling(err)
	if _, ok := deferralReason(deferred); !ok || outcome != outcomeSkipped || scan.gap != "" {
		t.Fatalf("overCeiling: outcome %v, err %v, gap %q", outcome, deferred, scan.gap)
	}
	scan.offChainLeftOut = true
	scan.noteRevisionLeftOut(err)
	if !hasRevisionLeftOut(t, scan) {
		t.Fatal("no diagnostic for a revision left out of a session whose upload is owed")
	}
}

// A publication owed before the scan began, which the scan may replace (one
// never attempted, waiting out its upload interval, that a stop request
// replaces with a richer one), does not keep the scan from running again
// without its off-chain revisions: only a publication the scan itself saved
// is owed as it is. Run again, the scan captures the chain, A then C, under
// a ceiling that leaves the chain alone 1 MiB of room. Kept from the retry,
// the session would be deferred on every pass until it failed.
func TestOffChainCeilingRetriesPastAnEarlierReplaceablePublication(t *testing.T) {
	t.Parallel()
	scan, err := runUnderCeiling(t, 64<<20, false)
	if !errors.Is(err, archive.ErrHistoryMutationPending) {
		t.Fatal("the chain alone does not capture: ", err)
	}
	_, peak := scan.readBudget().Charged()
	scan = withEarlierReplaceablePublication(t)
	t.Cleanup(scan.readBudget().BeginScope(peak + 1<<20))
	_, err = scan.run(scan.ctx)
	if !scan.withoutOffChain {
		t.Fatal("the scan did not run again without B, so the earlier publication defers it on every pass: ", err)
	}
	captured, ids := capturedChain(t, scan, err)
	if !captured || slices.Contains(ids, revisionB) || !slices.Contains(ids, revisionC) {
		t.Fatalf("captured %v, revisions %v, err %v", captured, ids, err)
	}
}

// A scan that replaced an earlier publication with its own, B included, and
// then met the session's read ceiling still runs again, since a publication
// was owed when it began; but the second scan resumes the replacement as it
// was saved, B included, rather than building one without B over it. The
// ceiling sits just under the scan's peak with B kept, which it reaches only
// after saving the replacement.
func TestOffChainCeilingRetryResumesTheReplacementItSaved(t *testing.T) {
	t.Parallel()
	scan := withEarlierReplaceablePublication(t)
	t.Cleanup(scan.readBudget().BeginScope(64 << 20))
	_, err := scan.run(scan.ctx)
	if captured, ids := capturedChain(t, scan, err); !captured || scan.withoutOffChain || !slices.Contains(ids, revisionB) {
		t.Fatalf("B was not kept with room for it: captured %v, inputs %v, retried %v", captured, ids, scan.withoutOffChain)
	}
	_, peak := scan.readBudget().Charged()
	scan = withEarlierReplaceablePublication(t)
	t.Cleanup(scan.readBudget().BeginScope(peak - 2<<10))
	_, err = scan.run(scan.ctx)
	if !scan.withoutOffChain {
		t.Fatal("the scan did not meet the ceiling with B kept; the fixture no longer exercises the retry: ", err)
	}
	// Only the first scan kept B, and the earlier publication had no history,
	// so a publication with B is the first scan's replacement.
	captured, ids := capturedChain(t, scan, err)
	if !captured || !slices.Contains(ids, revisionB) || !slices.Contains(ids, revisionC) {
		t.Fatalf("the retry replaced the saved publication: captured %v, inputs %v, err %v", captured, ids, err)
	}
	if pending, _, _ := scan.local.LoadPending(scan.id()); pending.RequestToken != scan.req.Token {
		t.Fatalf("owed publication token %q, want the stop request's", pending.RequestToken)
	}
	if hasRevisionLeftOut(t, scan) {
		t.Fatal("recorded a diagnostic with nothing left out")
	}
}

// A replacement the first scan saved without an off-chain revision it could
// not read is still reported as leaving it out when the second scan resumes
// it: the second scan never reconciles, so the first scan's record stands.
func TestOffChainCeilingRetryKeepsTheLeftOutRecordOfTheReplacementItResumes(t *testing.T) {
	t.Parallel()
	withUnreadable := func() *sessionScan {
		scan := withEarlierReplaceablePublication(t)
		lookup := scan.opts.CodexRollouts.(*reconciliationLookup)
		d := agentapi.SourceRef{Path: filepath.Join(scan.reg.ProjectRoot, "rollout-2026-10-01T12-00-00-"+revisionD+".jsonl"), Key: revisionThread}
		if err := os.WriteFile(d.Path, []byte("D-only is not a rollout\n"), 0600); err != nil {
			t.Fatal(err)
		}
		lookup.set.Candidates = append(lookup.set.Candidates, d)
		lookup.refs[revisionD] = []agentapi.SourceRef{d}
		return scan
	}
	scan := withUnreadable()
	t.Cleanup(scan.readBudget().BeginScope(64 << 20))
	_, err := scan.run(scan.ctx)
	if captured, ids := capturedChain(t, scan, err); !captured || scan.withoutOffChain || !slices.Contains(ids, revisionB) || slices.Contains(ids, revisionD) {
		t.Fatalf("with room, B was not kept or D was: captured %v, inputs %v, retried %v", captured, ids, scan.withoutOffChain)
	}
	_, peak := scan.readBudget().Charged()
	scan = withUnreadable()
	t.Cleanup(scan.readBudget().BeginScope(peak - 2<<10))
	_, err = scan.run(scan.ctx)
	if !scan.withoutOffChain {
		t.Fatal("the scan did not meet the ceiling with B kept; the fixture no longer exercises the retry: ", err)
	}
	if captured, ids := capturedChain(t, scan, err); !captured || !slices.Contains(ids, revisionB) || slices.Contains(ids, revisionD) {
		t.Fatalf("the retry did not resume the saved replacement: captured %v, inputs %v, err %v", captured, ids, err)
	}
	if !hasRevisionLeftOut(t, scan) {
		t.Fatal("no diagnostic for D, left out of the replacement the retry resumed")
	}
}

// withEarlierReplaceablePublication is largeOffChainFixture with a
// publication of the thread alone already owed: never attempted, waiting out
// its upload interval, under an earlier request than the scan's stop request,
// which may replace it.
func withEarlierReplaceablePublication(t *testing.T) *sessionScan {
	t.Helper()
	scan, lookup := largeOffChainFixture(t)
	filtered, adapter, err := FilterSource(scan.ctx, "codex", lookup.refs[revisionThread][0], scan.now, testSources)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.NewSourceBundle(scan.reg, adapter, filtered, scan.now, nil)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := renderPublication(scan.ctx, scan.resolveParser(), scan.parserVersion(), bundle, scan.reg, scan.now, scan.opts, scan.priorRepoKey)
	if err != nil {
		t.Fatal(err)
	}
	earlier := state.PendingPublication{
		SkillEvidence: string(scan.opts.skillEvidence()), Bundle: bundle, SourceKey: rendered.source.Key, MetadataKey: rendered.metadataKey,
		SourceSHA256: rendered.source.SHA256, SourceBytes: rendered.sourceBytes, MetadataBytes: rendered.metadata,
		RequestToken: "earlier-request", ReadyAt: scan.now.Add(time.Minute),
	}
	// Freeze a real current publication before exercising its replacement.
	if err := scan.savePending(&earlier); err != nil {
		t.Fatal(err)
	}
	scan.req.Token = "stop-request"
	return scan
}

func hasRevisionLeftOut(t *testing.T, scan *sessionScan) bool {
	t.Helper()
	diagnostics, err := capture.ReadDiagnostics(scan.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(diagnostics, func(d capture.Diagnostic) bool { return d.Code == capture.DiagnosticRevisionLeftOut })
}

// An off-chain revision left out is recorded as a capture diagnostic with
// the app, the configured project and the time only: no revision or thread
// identifiers, rollout paths or transcript text. Nothing left out records
// nothing.
func TestLeftOutOffChainRevisionRecordsAContentFreeDiagnostic(t *testing.T) {
	t.Parallel()
	scan, _ := reconciliationFixture(t)
	captured, ids := capturedChain(t, scan, scanErr(t, scan))
	if !captured || !slices.Contains(ids, revisionB) {
		t.Fatalf("B was not kept: captured %v, inputs %v", captured, ids)
	}
	if hasRevisionLeftOut(t, scan) {
		t.Fatal("recorded a diagnostic with nothing left out")
	}
	scan, lookup := reconciliationFixture(t)
	// B no longer reads as a Codex rollout of this thread.
	bPath := lookup.refs[revisionB][0].Path
	if err := os.WriteFile(bPath, []byte("B-only is not a rollout\n"), 0600); err != nil {
		t.Fatal(err)
	}
	captured, ids = capturedChain(t, scan, scanErr(t, scan))
	if !captured {
		t.Fatal("an unreadable off-chain rollout blocked the chain")
	}
	if slices.Contains(ids, revisionB) {
		t.Fatal("B was kept")
	}
	if len(scan.warnings) != 0 {
		t.Fatal(scan.warnings)
	}
	diagnostics, err := capture.ReadDiagnostics(scan.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	want := capture.Diagnostic{Code: capture.DiagnosticRevisionLeftOut, Harness: archive.HarnessCodex, ProjectRoot: scan.reg.ProjectRoot, ObservedAt: scan.now}
	if len(diagnostics) != 1 || diagnostics[0] != want {
		t.Fatalf("diagnostics %+v, want %+v", diagnostics, want)
	}
	raw, err := os.ReadFile(capture.DiagnosticsPath(scan.local.Home()))
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{revisionB, revisionThread, revisionC, bPath, "rollout-", "B-only"} {
		if strings.Contains(string(raw), private) {
			t.Errorf("the diagnostic records %q: %s", private, raw)
		}
	}
}

// scanErr runs one scan of the session and returns how it ended.
func scanErr(t *testing.T, scan *sessionScan) error {
	t.Helper()
	_, err := scan.run(scan.ctx)
	return err
}

// The diagnostic says the rest of the session was captured, so a scan that
// left an off-chain revision out records it only once it ends without the
// session recorded as a capture gap: not when the session met its own read
// ceiling with no upload owed (overCeiling records it as too large to read),
// nor when the scan recorded another gap.
func TestRevisionLeftOutIsNotRecordedForASessionLeftAsAGap(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		ceiling bool
		gap     state.BlockedReason
		want    bool
	}{
		{"captured", false, "", true},
		{"too large to read", true, "", false},
		{"rewritten", false, state.BlockedReasonTranscriptRewritten, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			scan, _ := reconciliationFixture(t)
			budget := scan.readBudget()
			t.Cleanup(budget.BeginScope(1 << 20))
			var err error
			if c.ceiling {
				budget.Refuse(1 << 40)
				err = errRetainedBudget
				if !scan.overSessionCeiling(err) {
					t.Fatal("not a refusal at the session's ceiling")
				}
			}
			scan.offChainLeftOut, scan.gap = true, c.gap
			scan.noteRevisionLeftOut(err)
			if got := hasRevisionLeftOut(t, scan); got != c.want {
				t.Fatalf("recorded %v, want %v", got, c.want)
			}
		})
	}
}

// A scan run again without its off-chain revisions reports as left out
// only what the publication lacks of what the scan that read them kept.
// B, preserved by reference from an earlier publication of the thread and
// unchanged since, is still published whole, so a run again after C grows
// does not report it. A B that grew since that publication is published
// only as it was then, so its growth is left out and reported. So is a B
// that an earlier publication carried as the thread's current revision and
// that grew before C became current: the run again stages that
// publication's B under the same ID, without its growth. So is a B never
// published before, and a B the scan that read off-chain revisions
// could not read. The first scan's reconciliation is the real one, and run
// forgets the rest of it as it does when that scan meets the ceiling.
func TestRevisionLeftOutReportsOnlyRevisionsAbsentFromThePublication(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		preserved  bool
		grown      bool
		unreadable bool
		wasCurrent bool
		want       bool
	}{
		{"preserved by an earlier publication", true, false, false, false, false},
		{"grown since an earlier publication", true, true, false, false, true},
		{"grown since published as the current revision", true, true, false, true, true},
		{"absent from the publication", false, false, false, false, true},
		{"unread by the scan that read off-chain revisions", false, false, true, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			scan, lookup := reconciliationFixture(t)
			bPath := lookup.refs[revisionB][0].Path
			if c.preserved {
				current := lookup.set.Current
				if c.wasCurrent {
					lookup.set.Current = &lookup.refs[revisionB][0]
				}
				publishFixture(t, scan)
				lookup.set.Current = current
				if c.grown {
					growRevision(t, bPath, 4, "B-later")
				}
				growRevision(t, lookup.refs[revisionC][0].Path, 4, "C-later")
				published, err := scan.local.LoadPublishedState(scan.id())
				if err != nil {
					t.Fatal(err)
				}
				scan = newSessionScan(t.Context(), scan.local, scan.remote, scan.reg, state.Request{}, published, scan.now, scan.opts)
			}
			if c.unreadable {
				if err := os.WriteFile(bPath, []byte("B-only is not a rollout\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			read, ok, err := scan.read()
			if err != nil || !ok {
				t.Fatal("native read", err)
			}
			active, _, err := scan.build(read)
			if err != nil {
				t.Fatal(err)
			}
			if scan.revisions, err = scan.reconcileRevisions(read, active); err != nil {
				t.Fatal(err)
			}
			if kept := scan.offChainKept > 0; kept == c.unreadable {
				t.Fatalf("the first scan kept B %v; the fixture no longer exercises the case", kept)
			}
			stagedB := slices.IndexFunc(scan.offChainPlanned, func(ref archive.RevisionReference) bool { return ref.RevisionID == revisionB })
			if staged := stagedB >= 0; staged != (!c.preserved && !c.unreadable || c.grown) {
				t.Fatalf("the first scan staged B %v: %v", staged, scan.offChainPlanned)
			}
			scan.forgetForRunWithoutOffChain()
			captured, ids := capturedChain(t, scan, scanErr(t, scan))
			if !captured {
				t.Fatal("the chain was not captured")
			}
			pending, _, err := scan.local.LoadPending(scan.id())
			if err != nil {
				t.Fatal(err)
			}
			var metadata archive.Metadata
			if err := json.Unmarshal(pending.MetadataBytes, &metadata); err != nil {
				t.Fatal(err)
			}
			publishedB := slices.IndexFunc(metadata.History.Preserved, func(ref archive.RevisionReference) bool { return ref.RevisionID == revisionB })
			if published := slices.Contains(ids, revisionB) || publishedB >= 0; published != c.preserved {
				t.Fatalf("B published %v, want %v: inputs %v", published, c.preserved, ids)
			}
			if c.grown && metadata.History.Preserved[publishedB].Source == scan.offChainPlanned[stagedB].Source {
				t.Fatal("B was published as the first scan staged it; the fixture no longer exercises the case")
			}
			if got := hasRevisionLeftOut(t, scan); got != c.want {
				t.Fatalf("recorded %v, want %v", got, c.want)
			}
		})
	}
}

// publishFixture runs collector passes until the fixture's session is
// published, B preserved beside A and C.
func publishFixture(t *testing.T, scan *sessionScan) {
	t.Helper()
	if err := scan.local.SaveRequest(scan.id(), "stop", scan.now); err != nil {
		t.Fatal(err)
	}
	opts := scan.opts
	opts.RepoKey = func(string) string { return "" }
	for range 3 {
		result, err := Run(t.Context(), scan.local, scan.remote, opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Published) == 1 {
			return
		}
	}
	t.Fatal("the fixture was not published")
}

// growRevision appends a user message at ordinal to a fixture rollout.
func growRevision(t *testing.T, path string, ordinal uint64, text string) {
	t.Helper()
	line, err := json.Marshal(map[string]any{"type": "response_item", "ordinal": ordinal, "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(errors.Join(err, f.Close()))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
