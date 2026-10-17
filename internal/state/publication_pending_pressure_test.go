package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func pendingPressureFixture(t *testing.T) (*Store, publicationReadOwner, []byte) {
	t.Helper()
	s := newTestStore(t)
	fixture := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	fixture.History = &PendingHistory{Version: 1}
	p, err := PreparePublicationV2(fixture, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SavePending(p.Bundle.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	owner := publicationReadOwner{registration: archive.SessionRegistration{ArchiveSessionID: m.SessionID, NativeSessionID: m.NativeSessionID, ProjectID: m.ProjectID, Harness: m.Harness, Origin: m.Origin, SessionStartedAt: m.StartedAt, PreviousGenerationID: m.PreviousGenerationID, DestinationID: "destination"}, machine: m.MachineID, admission: "admission"}
	raw, err := os.ReadFile(s.pendingPath(m.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	return s, owner, raw
}

func TestPublicationPendingReadPressureRetainsExactOwedWork(t *testing.T) {
	t.Parallel()
	s, owner, before := pendingPressureFixture(t)
	id := owner.registration.ArchiveSessionID
	budget := agentapi.NewNativeReadBudget(1)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	err := read.validateDurableReadPending(id, true, &owner)
	closeRead()
	if !errors.Is(err, agentapi.ErrReadBudget) || errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("capacity refusal reclassified as damaged authority: %v", err)
	}
	if n, _ := budget.Charged(); n != 0 {
		t.Fatalf("refused read retained %d bytes", n)
	}
	after, err := os.ReadFile(s.pendingPath(id))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("capacity refusal changed pending bytes: %v", err)
	}
	if owed, err := s.HasPending(id); err != nil || !owed {
		t.Fatalf("capacity refusal discharged pending work: owed=%v err=%v", owed, err)
	}
	if entries, err := os.ReadDir(filepath.Dir(s.pendingPath(id))); err != nil || len(entries) != 1 {
		t.Fatalf("capacity refusal quarantined evidence: %v %v", entries, err)
	}
	read, closeRead = s.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(1<<20))
	defer closeRead()
	if err := read.validateDurableReadPending(id, true, &owner); err != nil {
		t.Fatalf("independent retry with room refused: %v", err)
	}
}

func TestPublicationPendingForeignAndFutureEvidenceStillRequiresRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		foreign bool
		change  func([]byte) []byte
	}{
		{name: "foreign", foreign: true},
		{name: "future", change: func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"journal_version":5`), []byte(`"journal_version":99`), 1)
		}},
		{name: "malformed", change: func(raw []byte) []byte { return append(raw, '{') }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, owner, raw := pendingPressureFixture(t)
			id := owner.registration.ArchiveSessionID
			if tc.foreign {
				owner.registration.ProjectID = "another-project"
			}
			if tc.change != nil {
				changed := tc.change(raw)
				if bytes.Equal(changed, raw) {
					t.Fatal("fixture mutation did not change evidence")
				}
				raw = changed
				if err := os.WriteFile(s.pendingPath(id), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			read, closeRead := s.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(1<<20))
			defer closeRead()
			err := read.validateDurableReadPending(id, true, &owner)
			if !errors.Is(err, ErrDurableStorageRecovery) || errors.Is(err, agentapi.ErrReadBudget) {
				t.Fatalf("invalid authority became a capacity deferral: %v", err)
			}
			after, err := os.ReadFile(s.pendingPath(id))
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatalf("refused authority was altered: %v", err)
			}
		})
	}
}
