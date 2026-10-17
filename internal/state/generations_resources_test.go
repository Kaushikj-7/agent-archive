package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerationValidationAndJournalPressurePreserveRetry(t *testing.T) {
	s, reg, at := generationFixture(t)
	next, pending, err := generationBuilder(at)(reg, "next-generation")
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(1)
	if err := validateGenerationPublication(next, pending, budget, t.Context()); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("validation bypassed pressure", err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("validation leaked", used)
	}
	budget = agentapi.NewNativeReadBudget(4 << 20)
	if err := validateGenerationPublication(next, pending, budget, t.Context()); err != nil {
		t.Fatal(err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("validation scratch leaked", used)
	}
	path := s.generationRecoveryPath(reg.ArchiveSessionID)
	journal := generationRecovery{Version: 1, Previous: reg.ArchiveSessionID, Next: next.ArchiveSessionID, Pending: &pending, Registration: &next}
	if err := local.Write(path, journal); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	budget = agentapi.NewNativeReadBudget(2*int64(len(before)) - 1)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	if _, _, err := scoped.loadGenerationRecovery(t.Context(), reg.ArchiveSessionID); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("journal bypassed pressure", err)
	}
	closeScope()
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("pressure changed journal", err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("journal refusal leaked", used)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := validateGenerationPublication(next, pending, nil, ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("validation ignored cancellation", err)
	}
}

func TestGenerationJournalRefusesHistoryAroundOrdinaryBundle(t *testing.T) {
	_, reg, at := generationFixture(t)
	next, pending, err := generationBuilder(at)(reg, "next-generation")
	if err != nil {
		t.Fatal(err)
	}
	pending.History = &PendingHistory{Version: 1}
	var metadata archive.Metadata
	if err := json.Unmarshal(pending.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.SchemaVersion = archive.HistoryMetadataSchemaVersion
	metadata.History = &archive.RevisionHistory{CurrentRevision: "11111111-1111-4111-8111-111111111111"}
	pending.MetadataBytes, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateGenerationPublication(next, pending, nil, t.Context()); err == nil {
		t.Fatal("ordinary bundle admitted as recovery history")
	}
}

func TestMalformedGenerationHistoryJournalRemainsUntouched(t *testing.T) {
	s, reg, at := generationFixture(t)
	interrupted := errors.New("stop after durable generation journal")
	s.onIndexStep = func(step string) error {
		if step == "generation-journal" {
			return interrupted
		}
		return nil
	}
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at)); !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	journal, found, err := s.loadGenerationRecovery(t.Context(), reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(err)
	}
	journal.Pending.History = &PendingHistory{Version: 1}
	var metadata archive.Metadata
	if err := json.Unmarshal(journal.Pending.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.SchemaVersion = archive.HistoryMetadataSchemaVersion
	metadata.History = &archive.RevisionHistory{CurrentRevision: "11111111-1111-4111-8111-111111111111"}
	journal.Pending.MetadataBytes, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	path := s.generationRecoveryPath(reg.ArchiveSessionID)
	if err := local.Write(path, journal); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s.onIndexStep = nil
	if err := s.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatal("malformed history allowed routing mutation", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("malformed journal was changed", err)
	}
	original, found, err := s.LoadRegistration(reg.ArchiveSessionID)
	if err != nil || !found || original.CaptureFrozen {
		t.Fatal("original changed", err)
	}
	if _, found, err := s.LoadRegistration(journal.Next); err != nil || found {
		t.Fatal("malformed journal registered successor", err)
	}
}

func TestScheduledGenerationReplayEndsEachReceiptOwner(t *testing.T) {
	s, reg, _ := generationFixture(t)
	dir := filepath.Dir(s.generationRecoveryPath(reg.ArchiveSessionID))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		id := fmt.Sprintf("receipt-%d", i)
		raw := fmt.Sprintf(`{"version":1,"key":{"Agent":"codex","NativeID":"synthetic"},"previous":%q,"next":"next","complete":true}`, id) + strings.Repeat(" ", 60<<10)
		if err := os.WriteFile(s.generationRecoveryPath(id), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// One decoded receipt plus the fixed strict proof fits; three retained owners do not.
	budget := agentapi.NewNativeReadBudget(400 << 10)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	defer closeScope()
	if err := scoped.ResumeGenerationRecoveries(t.Context()); err != nil {
		t.Fatal("prior receipt starved later receipt", err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("completed receipts retained owners", used)
	}
}

func TestUnscopedScheduledGenerationReplayHasLocalDataCeiling(t *testing.T) {
	s, reg, _ := generationFixture(t)
	path := s.generationRecoveryPath(reg.ArchiveSessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(128<<20 + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("startup used allocation-unbounded journal read", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 128<<20+1 {
		t.Fatal("refusal changed journal", err)
	}
}

func TestGenerationPressureLeavesLargeJournalAndReplaysLaterSmallJournal(t *testing.T) {
	s, reg, at := generationFixture(t)
	interrupted := errors.New("journal saved")
	s.onIndexStep = func(step string) error {
		if step == "generation-journal" {
			return interrupted
		}
		return nil
	}
	_, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at))
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	// Begin returns no routing result on interruption; the durable journal owns it.
	journal, found, err := s.loadGenerationRecovery(t.Context(), reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(err)
	}
	next := journal.Next
	s.onIndexStep = nil
	path := s.generationRecoveryPath("000-pressure")
	raw := []byte(`{"version":1,"key":{"Agent":"codex","NativeID":"synthetic"},"previous":"000-pressure","next":"next","complete":true,"padding":"` + strings.Repeat("x", 3<<20) + `"}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(4 << 20)
	scoped, closeScope := s.WithReadBudget(t.Context(), budget)
	defer closeScope()
	if err := scoped.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("large refusal missing", err)
	}
	resumed, found, err := s.loadGenerationRecovery(t.Context(), reg.ArchiveSessionID)
	if err != nil || !found || !resumed.Complete || resumed.Next != next {
		t.Fatal("large journal starved smaller replay", resumed, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, raw) {
		t.Fatal("refusal changed large journal", err)
	}
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("replay leaked", used)
	}
}

func TestGenerationPressureCannotHideLaterUnsupportedJournal(t *testing.T) {
	s, reg, _ := generationFixture(t)
	dir := filepath.Dir(s.generationRecoveryPath(reg.ArchiveSessionID))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	pressure := []byte(`{"version":1,"key":{"Agent":"codex","NativeID":"synthetic"},"previous":"000-pressure","next":"next","complete":true,"padding":"` + strings.Repeat("x", 3<<20) + `"}`)
	invalid := []byte(`{"version":2,"key":{"Agent":"codex","NativeID":"synthetic"},"previous":"zzz-invalid","next":"next","complete":true}`)
	for id, raw := range map[string][]byte{"000-pressure": pressure, "zzz-invalid": invalid} {
		if err := os.WriteFile(s.generationRecoveryPath(id), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	scoped, closeScope := s.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(4<<20))
	defer closeScope()
	if err := scoped.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, ErrSessionIndexRecoveryRequired) || errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("hard refusal hidden by earlier pressure", err)
	}
	for id, raw := range map[string][]byte{"000-pressure": pressure, "zzz-invalid": invalid} {
		after, err := os.ReadFile(s.generationRecoveryPath(id))
		if err != nil || !bytes.Equal(after, raw) {
			t.Fatal("refusal changed journal", id, err)
		}
	}
}

func TestGenerationJoinedCancellationRemainsDecisive(t *testing.T) {
	s, reg, at := generationFixture(t)
	interrupted := errors.New("journal saved")
	s.onIndexStep = func(step string) error {
		if step == "generation-journal" {
			return interrupted
		}
		return nil
	}
	if _, err := s.BeginGenerationRecovery(reg.ArchiveSessionID, at, generationBuilder(at)); !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	joined := errors.Join(agentapi.ReadBudgetLimit(errors.New("synthetic pressure")), context.Canceled)
	s.onIndexStep = func(step string) error {
		if step == "generation-fenced" {
			return joined
		}
		return nil
	}
	if err := s.ResumeGenerationRecoveries(t.Context()); !errors.Is(err, context.Canceled) || !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("joined cancellation hidden", err)
	}
	journal, found, err := s.loadGenerationRecovery(t.Context(), reg.ArchiveSessionID)
	if err != nil || !found || journal.Complete {
		t.Fatal("cancelled journal acknowledged", journal, err)
	}
	if _, found, err := s.LoadRegistration(journal.Next); err != nil || found {
		t.Fatal("cancelled recovery installed successor", err)
	}
}
