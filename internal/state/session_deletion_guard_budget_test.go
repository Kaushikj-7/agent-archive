package state

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestDeletionOwnerRefusalAvoidsDataReadWithExhaustedAccountingLedger(t *testing.T) {
	s := newTestStore(t)
	p := publicationFixture(t, publicationThread, time.Now())
	reg := registration(t)
	reg.ArchiveSessionID = p.Bundle.ArchiveSessionID
	reg.NativeSessionID = p.Bundle.NativeSessionID
	reg.ProjectID = p.Bundle.ProjectID
	reg.Harness = p.Bundle.Capture.Harness
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareSessionDeletion(reg, RemovalReasonRetention, p.MetadataBytes, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(64 << 10)
	accounted, closeAccounting := s.WithPublicationAccounting(t.Context(), budget)
	defer closeAccounting()
	r := budget.Reservation()
	n := r.Available()
	if !r.Reserve(n) {
		t.Fatal("fixture reserve")
	}
	defer r.Release(n)
	metadataRead := false
	accounted.deletionFileOpen = func(root *os.Root, name string) (*os.File, error) {
		if name == "metadata.json" {
			metadataRead = true
		}
		return root.Open(name)
	}
	_, err := accounted.UpdateRegistration(reg.ArchiveSessionID, func(r *archive.SessionRegistration) error { r.TranscriptPath += "-new"; return nil })
	if !errors.Is(err, ErrRemovalPending) && !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("unexpected guard error", err)
	}
	if metadataRead {
		t.Fatal("refusal guard read retained metadata outside exhausted invocation ledger")
	}
}
