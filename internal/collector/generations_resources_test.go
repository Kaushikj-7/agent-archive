package collector

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recoveryBudgetLookup struct {
	agentapi.CodexRolloutLookup
	budget *agentapi.NativeReadBudget
}

func (l recoveryBudgetLookup) NativeReadBudget() *agentapi.NativeReadBudget { return l.budget }

func TestGenerationPreviewHoldsSharedOwnerUntilConsumerClose(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	budget := agentapi.NewNativeReadBudget(128 << 20)
	reservation := budget.Reservation()
	opts := scan.opts
	opts.CodexRollouts = recoveryBudgetLookup{CodexRolloutLookup: opts.CodexRollouts, budget: budget}
	build, closePreview, err := PrepareGenerationRecovery(t.Context(), scan.reg, scan.now, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer closePreview()
	owned, _ := budget.Charged()
	if owned == 0 {
		t.Fatal("preview output has no owned charge")
	}
	pressure := budget.Available()
	if !reservation.Reserve(pressure) {
		t.Fatal("pressure")
	}
	if _, _, err := build(scan.reg, "next"); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("builder bypassed shared pressure", err)
	}
	used, _ := budget.Charged()
	if used != owned+pressure {
		t.Fatal("failed builder retained temporary work", used, owned+pressure)
	}
	reservation.Release(pressure)
	_, pending, err := build(scan.reg, "next")
	if err != nil {
		t.Fatal(err)
	}
	if pending.Bundle.History == nil || pending.Bundle.ArchiveSessionID != "next" {
		t.Fatal("fixed source lost")
	}
	used, _ = budget.Charged()
	if used <= owned {
		t.Fatal("returned publication has no owned charge")
	}
	closePreview()
	closePreview()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("preview close leaked", used)
	}
	if _, _, err := build(scan.reg, "next"); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal("closed preview used released values", err)
	}
}

func TestGenerationPreviewCancellationAndInitialPressureRelease(t *testing.T) {
	scan, _ := reconciliationFixture(t)
	budget := agentapi.NewNativeReadBudget(128 << 20)
	reservation := budget.Reservation()
	opts := scan.opts
	opts.CodexRollouts = recoveryBudgetLookup{CodexRolloutLookup: opts.CodexRollouts, budget: budget}
	pressure := budget.Available()
	if !reservation.Reserve(pressure) {
		t.Fatal("pressure")
	}
	build, closePreview, err := PrepareGenerationRecovery(t.Context(), scan.reg, scan.now, opts)
	closePreview()
	if build != nil || !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("native preview bypassed pressure", err)
	}
	reservation.Release(pressure)
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("initial refusal leaked", used)
	}
	ctx, cancel := context.WithCancel(t.Context())
	build, closePreview, err = PrepareGenerationRecovery(ctx, scan.reg, scan.now, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer closePreview()
	cancel()
	if _, _, err := build(scan.reg, "next"); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled preview built publication", err)
	}
	closePreview()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("canceled preview leaked", used)
	}
	// A changed registration also refuses before accumulating successor output.
	build, closePreview, err = PrepareGenerationRecovery(t.Context(), scan.reg, scan.now, opts)
	if err != nil {
		t.Fatal(err)
	}
	changed := scan.reg
	changed.NativeSessionID = "different"
	if _, _, err := build(changed, "next"); err == nil {
		t.Fatal("changed registration accepted")
	}
	closePreview()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("error cleanup leaked", used)
	}
}

func TestRunGenerationReplayUsesSharedBudgetBeforeStartupRead(t *testing.T) {
	s := newTestStore(t)
	dir := filepath.Join(s.Home(), "generation-recovery")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "previous.json")
	raw := []byte(`{"version":1,"key":{"Agent":"codex","NativeID":"synthetic"},"previous":"previous","next":"next","complete":true}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	reservation := budget.Reservation()
	pressure := budget.Available()
	if !reservation.Reserve(pressure) {
		t.Fatal("pressure")
	}
	opts := Options{MachineID: "machine", Sources: testSources, Parsers: testParsers, SkipSessionIndexRecovery: true, CodexRollouts: recoveryBudgetLookup{budget: budget}}
	result, err := Run(t.Context(), s, storagetest.NewMemoryStore(), opts)
	if err != nil || !errors.Is(result.Errors["generation-recovery"], agentapi.ErrReadBudget) {
		t.Fatal("scheduled replay bypassed shared pressure", result.Errors, err)
	}
	reservation.Release(pressure)
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("replay refusal leaked", used)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(raw) {
		t.Fatal("refusal changed receipt", err)
	}
	if _, err := Run(t.Context(), s, storagetest.NewMemoryStore(), opts); err != nil {
		t.Fatal("replay could not retry", err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("successful replay leaked", used)
	}
}

func TestRunGenerationPressurePreservesJournalAndCapturesUnrelatedSource(t *testing.T) {
	scan, initial := privacyJournal(t)
	lookup := scan.opts.CodexRollouts.(*reconciliationLookup)
	if _, err := scan.publishPending(scan.ctx, initial); err != nil {
		t.Fatal("initial real history publication", err)
	}
	published, err := scan.local.LoadPublishedState(scan.id())
	if err != nil {
		t.Fatal(err)
	}
	bundle, at, found := published.LastPublished()
	if !found {
		t.Fatal("prior history missing")
	}
	if err := published.SaveBlocked(bundle, at, state.BlockedReasonTranscriptRewritten); err != nil {
		t.Fatal(err)
	}
	s := scan.local
	texts := make([]string, 400)
	for i := range texts {
		texts[i] = strings.Repeat("synthetic retained generation observation ", (70<<10)/40)
	}
	ref := reconciliationNative(t, scan.reg.ProjectRoot, revisionC, 2, true, texts...)
	lookup.set = agentapi.CodexRolloutSet{Current: &ref, Candidates: []agentapi.SourceRef{ref}, Revision: "pressure"}
	lookup.refs[revisionC] = []agentapi.SourceRef{ref}
	build, release, err := PrepareGenerationRecovery(t.Context(), scan.reg, scan.now, scan.opts)
	if err != nil {
		var classified *agentapi.SourceError
		if errors.As(err, &classified) {
			t.Fatal(err, "cause:", classified.Unwrap())
		}
		t.Fatal(err)
	}
	registrationDir := filepath.Join(s.Home(), "registrations")
	_, err = s.BeginGenerationRecovery(scan.id(), scan.now, func(old archive.SessionRegistration, next string) (archive.SessionRegistration, state.PendingPublication, error) {
		reg, pending, err := build(old, next)
		if err != nil {
			return reg, pending, err
		}
		// Existing registration write fails only after the real generation journal
		// has been durably produced. This is a disposable filesystem fault.
		if err := os.Chmod(registrationDir, 0500); err != nil {
			t.Fatal(err)
		}
		return reg, pending, nil
	})
	release()
	beginErr := err
	if restoreErr := os.Chmod(registrationDir, 0700); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err == nil {
		t.Fatal("generation fixture did not stop before completion")
	}
	path := filepath.Join(s.Home(), "generation-recovery", scan.id()+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("real unfinished generation journal missing", err, "begin:", beginErr)
	}
	var journal struct {
		Complete bool                      `json:"complete"`
		Pending  *state.PendingPublication `json:"pending"`
	}
	if err := json.Unmarshal(raw, &journal); err != nil || journal.Complete || journal.Pending == nil {
		t.Fatal("invalid unfinished producer", err)
	}
	if 2*len(raw) <= 40<<20 {
		t.Fatalf("journal does not exercise read pressure: %d", len(raw))
	}
	transcript := writeTranscript(t, t.TempDir(), "claude.jsonl", `{"type":"user","sessionId":"native-1","message":{"role":"user","content":"synthetic task"}}
{"type":"assistant","sessionId":"native-1","message":{"role":"assistant","content":[{"type":"text","text":"synthetic answer"}]}}
`)
	reg := registration(t, transcript)
	reg.Harness = archive.Harness{Name: "claude"}
	reg.ArchiveSessionID = "independent-claude"
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(40 << 20)
	opts := Options{MachineID: "machine", Sources: testSources, Parsers: testParsers, SkipSessionIndexRecovery: true, CodexRollouts: recoveryBudgetLookup{budget: budget}}
	remote := storagetest.NewMemoryStore()
	result, err := Run(t.Context(), s, remote, opts)
	if err != nil || !errors.Is(result.Errors["generation-recovery"], agentapi.ErrReadBudget) {
		t.Fatal("pressure was hidden", result, err)
	}
	if len(result.Published) != 1 || result.Published[0] != reg.ArchiveSessionID {
		t.Fatal("pending generation starved independent capture", result)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(raw) {
		t.Fatal("pressure changed journal", err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("pass leaked", used)
	}
}
