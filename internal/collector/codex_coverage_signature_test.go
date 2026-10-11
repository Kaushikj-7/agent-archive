package collector

import (
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Coverage completeness can change between passes while a lineage stays the
// same. The scan-skip check must still find that source unchanged, and must
// not when the lineage token or the candidate set changes.
func TestCodexScanSkipIgnoresCoverageCompleteness(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	lookup.set.Lineage = "lineage"
	observe := func() sourceState {
		t.Helper()
		reader, ok := newSourceReader(scan.reg, scan.opts)
		if !ok {
			t.Fatal("no source reader")
		}
		observed, err := reader.Signature(scan.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return observed
	}
	settled := observe()
	signature := state.ScanSignature{SourceKind: settled.kind, SourceSignature: signaturePointer(settled)}

	lookup.set.Complete, lookup.set.Revision = false, "incomplete"
	if !observe().matches(signature) {
		t.Fatal("a coverage flip alone made an unchanged lineage look changed")
	}
	lookup.set.Complete, lookup.set.Revision = true, "complete again"
	if !observe().matches(signature) {
		t.Fatal("coverage completing again made an unchanged lineage look changed")
	}

	lookup.set.Lineage = "new segment"
	if observe().matches(signature) {
		t.Fatal("a changed lineage token was skipped")
	}
	lookup.set.Lineage = "lineage"
	lookup.set.Candidates = lookup.set.Candidates[1:]
	if observe().matches(signature) {
		t.Fatal("a changed candidate set was skipped")
	}

	// A lookup without a separate lineage token keeps its revision's meaning.
	lookup.set = agentapi.CodexRolloutSet{Current: lookup.set.Current, Candidates: lookup.set.Candidates, Revision: "only", Complete: true}
	before := observe()
	lookup.set.Revision = "changed"
	if observe().observation.Signature == before.observation.Signature {
		t.Fatal("a revision-only lookup lost change detection")
	}
}
