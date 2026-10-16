package state

import (
	"errors"
	"testing"
)

// A staged selection that covers a request is read by the fence view as a
// whole decode reads it: re-saving it and the catalog removal fence pass,
// as they did before the view.
func TestPendingFenceViewReadsAStagedSelectionCoveringARequest(t *testing.T) {
	for _, request := range []bool{false, true} {
		s, p, _ := stagedPreparedFixture(t, request)
		id := p.Bundle.ArchiveSessionID
		if _, found, err := s.pendingFenceView(id); err != nil || !found {
			t.Fatalf("request=%v: fence view: %v %v", request, found, err)
		}
		if err := s.SavePending(id, p); err != nil {
			t.Fatalf("request=%v: re-saving the staged publication: %v", request, err)
		}
		if err := s.checkCatalogPendingRemoval(id); err != nil {
			t.Fatalf("request=%v: catalog removal fence: %v", request, err)
		}
		if err := s.checkSelectingPendingRemoval(id); !errors.Is(err, ErrReadySelectionFrozen) {
			t.Fatalf("request=%v: selecting removal fence: %v", request, err)
		}
	}
}
