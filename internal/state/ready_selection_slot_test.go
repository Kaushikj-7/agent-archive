package state

import (
	"bytes"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

// These controls use existing54b Save/Load/fixture APIs with identical bytes.
// The A proof is fully valid; B has its own real registration and an empty slot.
func readySelectionTwoSlots(t *testing.T) (*Store, PendingPublication, string) {
	t.Helper()
	s, p := readySelectionFixture(t)
	cfg, _, err := config.Load(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	const other = "owned-b"
	reg := registration(t)
	reg.ArchiveSessionID = other
	reg.NativeSessionID = "native-b"
	reg.ProjectID = p.Bundle.ProjectID
	reg.DestinationID = cfg.DestinationID()
	if err = s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err = s.ValidateReadySelection(t.Context(), p); err != nil {
		t.Fatalf("A proof positive control invalid: %v", err)
	}
	if _, found, err := s.LoadPending(other); err != nil || found {
		t.Fatalf("B slot control is not empty: %v %v", found, err)
	}
	return s, p, other
}

func TestReadySelectionSaveRejectsValidProofInForeignSlot(t *testing.T) {
	s, p, other := readySelectionTwoSlots(t)
	if err := s.SavePending(other, p); err == nil {
		t.Fatal("fully valid A proof saved in B journal slot")
	}
	if _, err := os.Lstat(s.pendingPath(other)); !os.IsNotExist(err) {
		t.Fatalf("foreign slot save created journal: %v", err)
	}
	if err := s.SavePending(p.Bundle.ArchiveSessionID, p); err != nil {
		t.Fatalf("owned slot save failed: %v", err)
	}
	loaded, found, err := s.LoadPending(p.Bundle.ArchiveSessionID)
	if err != nil || !found || *loaded.Selection != *p.Selection {
		t.Fatalf("owned slot load failed: %v", err)
	}
}

func TestReadySelectionLoadRejectsTransplantedValidOwnedJournal(t *testing.T) {
	s, p, other := readySelectionTwoSlots(t)
	id := p.Bundle.ArchiveSessionID
	if err := s.SavePending(id, p); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.pendingPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.pendingPath(other), raw, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := s.LoadPending(other)
	if err == nil || !found || loaded.Selection != nil {
		t.Fatalf("valid A journal accepted from B slot: found=%v err=%v", found, err)
	}
	retained, err := os.ReadFile(s.pendingPath(other))
	if err != nil || !bytes.Equal(retained, raw) {
		t.Fatal("rejected transplant changed evidence")
	}
	loaded, found, err = s.LoadPending(id)
	if err != nil || !found || *loaded.Selection != *p.Selection {
		t.Fatalf("owned A journal no longer readable: %v", err)
	}
}
