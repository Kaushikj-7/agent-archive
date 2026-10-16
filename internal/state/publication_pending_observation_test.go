package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
)

func observedPendingFixture(t *testing.T) (*Store, PendingPublication, *agentapi.NativeReadBudget, func()) {
	t.Helper()
	s, owner, _ := pendingPressureFixture(t)
	p, found, err := s.LoadPending(owner.registration.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	budget := agentapi.NewNativeReadBudget(4 << 20)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	t.Cleanup(closeRead)
	if err := read.SavePending(p.Bundle.ArchiveSessionID, p); err != nil {
		t.Fatal(err)
	}
	if w := read.pendingObservation(); w == nil || w.home == nil || w.sum == ([32]byte{}) {
		t.Fatal("validated successful write retained no observation")
	}
	return read, p, budget, closeRead
}

func TestPendingWriteObservationRejectsSameStampFutureBytes(t *testing.T) {
	t.Parallel()
	s, p, _, _ := observedPendingFixture(t)
	path := s.pendingPath(p.Bundle.ArchiveSessionID)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	future := bytes.Replace(raw, []byte(`"journal_version":5`), []byte(`"journal_version":9`), 1)
	if bytes.Equal(raw, future) || len(raw) != len(future) {
		t.Fatal("future mutation did not preserve the file's length")
	}
	if err := os.WriteFile(path, future, before.Mode()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !sameDurableStamp(before, after) {
		t.Fatalf("fixture did not preserve identity, size and time: %v", err)
	}
	p.ReadyAt = p.ReadyAt.Add(time.Minute)
	if err := s.SavePending(p.Bundle.ArchiveSessionID, p); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("same-stamp future journal overwritten: %v", err)
	}
	retained, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(retained, future) {
		t.Fatalf("future bytes changed: %v", err)
	}
	if w := s.pendingObservation(); w != nil && w.home != nil {
		t.Fatal("refused changed bytes retained an observation")
	}
}

func TestPendingWriteObservationEndsWithItsExactReadOwner(t *testing.T) {
	t.Parallel()
	s, _, budget, closeRead := observedPendingFixture(t)
	copyStore := *s
	if copyStore.pendingObservation() != nil {
		t.Fatal("copied Store reused another read owner's observation")
	}
	other, closeOther := s.WithReadBudget(t.Context(), budget)
	defer closeOther()
	if other.pendingObservation() != nil {
		t.Fatal("new read scope inherited the previous observation")
	}
	closeRead()
	if s.pendingObservation() != nil || copyStore.pendingObservation() != nil {
		t.Fatal("closed scope retained a usable observation")
	}
	if n, _ := budget.Charged(); n != 0 {
		t.Fatalf("ended owner retained %d bytes", n)
	}
}

func TestPendingWriteObservationIsNotInstalledAfterFailedWrite(t *testing.T) {
	t.Parallel()
	s, p, _, _ := observedPendingFixture(t)
	id := p.Bundle.ArchiveSessionID
	before, err := os.ReadFile(s.pendingPath(id))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = config.WithPublicationComposition(s.home, func(composition config.PublicationCompositionGuard) error {
		guard, err := composition.Storage(s.home)
		if err != nil {
			return err
		}
		return s.writeObservedPending(ctx, guard, id, p)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled guarded writer: %v", err)
	}
	if w := s.pendingObservation(); w != nil && w.home != nil {
		t.Fatal("failed writer installed an observation")
	}
	after, err := os.ReadFile(s.pendingPath(id))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed writer changed prior bytes: %v", err)
	}
}

func TestPendingWriteObservationRefusesCloseFailureAndContextChange(t *testing.T) {
	t.Parallel()
	for _, closeFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "context", true: "close"}[closeFailure], func(t *testing.T) {
			t.Parallel()
			s, p, _, _ := observedPendingFixture(t)
			id := p.Bundle.ArchiveSessionID
			before, err := os.ReadFile(s.pendingPath(id))
			if err != nil {
				t.Fatal(err)
			}
			if !closeFailure {
				ctx, cancel := context.WithCancel(s.resourceContext)
				s.resourceContext = ctx
				if s.pendingObservation() != nil {
					t.Fatal("changed context reused observation")
				}
				cancel()
				if err := s.SavePending(id, p); !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled owner: %v", err)
				}
			} else {
				failure := errors.New("synthetic pending hash close failure")
				s.publicationFileClose = func(file *os.File) error { return errors.Join(file.Close(), failure) }
				p.ReadyAt = p.ReadyAt.Add(time.Minute)
				if err := s.SavePending(id, p); !errors.Is(err, failure) {
					t.Fatalf("failed hash close allowed write: %v", err)
				}
				if w := s.pendingObservation(); w != nil && w.home != nil {
					t.Fatal("failed hash close retained a usable observation")
				}
			}
			after, err := os.ReadFile(s.pendingPath(id))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("refused observation changed evidence: %v", err)
			}
		})
	}
}

func TestPendingWriteObservationNeverBypassesOriginalEvidence(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	legacy := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	legacy.History = &PendingHistory{Version: 1}
	id := legacy.Bundle.ArchiveSessionID
	if err := s.SavePending(id, legacy); err != nil {
		t.Fatal(err)
	}
	ready, err := PreparePublicationV2(legacy, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	read, closeRead := s.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(4<<20))
	defer closeRead()
	if err := read.SavePending(id, ready); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.home, evidencePath(id))
	before, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(before), `"migration_origin"`) {
		t.Fatalf("migration original was not retained: %v", err)
	}
	err = config.WithPublicationComposition(s.home, func(composition config.PublicationCompositionGuard) error {
		guard, err := composition.Storage(s.home)
		if err != nil {
			return err
		}
		if handled, err := read.rewriteObservedPending(guard, id, ready); handled || err != nil {
			t.Fatalf("original evidence bypassed: handled=%v err=%v", handled, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := read.SavePending(id, ready); err != nil {
		t.Fatal("normal owned migration replay refused", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("original evidence changed: %v", err)
	}
}

func TestPendingWriteObservationDoesNotHandlePhaseRollback(t *testing.T) {
	t.Parallel()
	s, p, _, _ := observedPendingFixture(t)
	p.Phase = PublicationPreparing
	p.History.Preparing = true
	err := config.WithPublicationComposition(s.home, func(composition config.PublicationCompositionGuard) error {
		guard, err := composition.Storage(s.home)
		if err != nil {
			return err
		}
		if handled, err := s.rewriteObservedPending(guard, p.Bundle.ArchiveSessionID, p); handled || err != nil {
			t.Fatalf("phase rollback bypassed full validation: handled=%v err=%v", handled, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPendingWriteObservationDoesNotCrossHomeReplacement(t *testing.T) {
	t.Parallel()
	s, p, _, _ := observedPendingFixture(t)
	cfg, found, err := config.Load(s.home)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	oldHome := s.home + "-original"
	if err := os.Rename(s.home, oldHome); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(s.home, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(s.home); _ = os.Rename(oldHome, s.home) })
	err = config.WithPublicationComposition(s.home, func(composition config.PublicationCompositionGuard) error {
		guard, err := composition.Storage(s.home)
		if err != nil {
			return err
		}
		if handled, err := s.rewriteObservedPending(guard, p.Bundle.ArchiveSessionID, p); handled || err != nil {
			t.Fatalf("replaced root reused observation: handled=%v err=%v", handled, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := s.pendingObservation(); w != nil && w.home != nil {
		t.Fatal("root replacement left usable observation")
	}
}

func TestPendingWriteObservationRefusesNewOriginalEvidence(t *testing.T) {
	t.Parallel()
	s, p, _, _ := observedPendingFixture(t)
	id := p.Bundle.ArchiveSessionID
	path := filepath.Join(s.home, evidencePath(id))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"version":999,"kind":"future-original"}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePending(id, p); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatalf("new original evidence bypassed: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("new evidence overwritten", err)
	}
}

func TestPendingWriteObservationDefersToGlobalSelectionProtection(t *testing.T) {
	t.Parallel()
	s, p, _, _ := observedPendingFixture(t)
	cfg, found, err := config.Load(s.home)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	cfg.PublicationCompositionProtection = true
	if err := config.Save(s.home, cfg); err != nil {
		t.Fatal(err)
	}
	err = config.WithPublicationComposition(s.home, func(composition config.PublicationCompositionGuard) error {
		guard, err := composition.Storage(s.home)
		if err != nil {
			return err
		}
		if handled, err := s.rewriteObservedPending(guard, p.Bundle.ArchiveSessionID, p); handled || err != nil {
			t.Fatalf("global selection protection bypassed: handled=%v err=%v", handled, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
