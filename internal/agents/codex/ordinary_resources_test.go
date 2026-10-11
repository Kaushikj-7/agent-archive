package codex

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestOrdinaryOwnTaskScannerRequiresAndReleasesSharedScratch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, strings.Repeat("x", 1<<20))
	ledger := agentapi.NewNativeReadBudget(relatedRawBudget)
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: ledger})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pass.Close() }()
	snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	ordinary, ok := snap.(*ordinarySnapshot)
	if !ok {
		t.Fatalf("%T", snap)
	}
	usedBefore, _ := ledger.Charged()
	remaining := ledger.Available()
	if !ledger.Reserve(remaining) {
		t.Fatal("reserve")
	}
	_, err = ordinary.FirstOwnTask(t.Context())
	if !agentapi.HasFailure(err, agentapi.Unavailable) {
		t.Fatal("scanner allocated with full ledger", err)
	}
	ledger.Release(remaining)
	if _, err := ordinary.FirstOwnTask(t.Context()); err != nil {
		t.Fatal(err)
	}
	used, peak := ledger.Charged()
	if used != usedBefore || peak > relatedRawBudget {
		t.Fatal("scanner charge leaked", used, usedBefore, peak)
	}
}

func TestHistoryUnterminatedBoundaryRequiresSharedScratchAndRetries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, raw := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA}, "complete", "unterminated")
	if err := os.WriteFile(ref.Path, bytes.TrimSuffix(raw, []byte("\n")), 0600); err != nil {
		t.Fatal(err)
	}
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &ref}}
	pass := historyPass(t, dir, lookup).(*relatedSourcePass)
	selection, err := pass.selectSource(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	ledger := pass.env.ReadBudget
	before, _ := ledger.Charged()
	pressure := ledger.Available()
	if !ledger.Reserve(pressure) {
		t.Fatal("reserve pressure")
	}
	if _, err := pass.graph(t.Context(), selection.leaf, selection.set); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("newline scratch allocated under pressure", err)
	}
	used, _ := ledger.Charged()
	if used != before+pressure {
		t.Fatal("refused scratch charge leaked")
	}
	ledger.Release(pressure)
	bundle := historyRead(t, pass, ref)
	if len(bundle.NativeRecords) != 2 {
		t.Fatal("history committed an unterminated record")
	}
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	used, _ = ledger.Charged()
	if used != 0 {
		t.Fatal("history source/scratch leaked", used)
	}
}

func TestIdleNativeExtentReopensOnlyAfterReservation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, strings.Repeat("safe text ", 10000))
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &ref}}
	pass := historyPass(t, dir, lookup).(*relatedSourcePass)
	snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	file := pass.files[ref.Path]
	if file == nil || !file.rawCharged {
		t.Fatal("live raw extent uncharged")
	}
	facts, err := snap.(agentapi.SourceAdmissionFacts).AdmissionFacts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.Close(); err != nil {
		t.Fatal(err)
	}
	ledger := pass.env.ReadBudget
	before, _ := ledger.Charged()
	if file.rawCharged || before < sourceHeaderCharge || pass.files[ref.Path] != file {
		t.Fatal("idle extent or header ownership", before)
	}
	pressure := ledger.Available()
	if !ledger.Reserve(pressure) {
		t.Fatal("pressure")
	}
	if _, err := pass.Signature(t.Context(), ref); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("cached signature bypassed reservation", err)
	}
	if _, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{}); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("cached read bypassed reservation", err)
	}
	if pass.files[ref.Path] != file || file.rawCharged || facts.PhysicalProducerVersion != "0.160.0" {
		t.Fatal("refusal evicted live header facts")
	}
	ledger.Release(pressure)
	snap, err = pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil || !file.rawCharged {
		t.Fatal("cached reopen failed", err)
	}
	if err := snap.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	used, _ := ledger.Charged()
	if used != 0 {
		t.Fatal("cached owners leaked", used)
	}
}
