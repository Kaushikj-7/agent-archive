package collector

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/state"
)

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
