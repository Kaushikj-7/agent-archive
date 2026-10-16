package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestPublicationQuotaCloseRefusesClassificationAndReleasesLoan(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful-read-close-failure", true: "corrupt-read-and-close-failure"}[corrupt], func(t *testing.T) {
			s := newTestStore(t)
			fixture := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
			published, err := s.LoadPublishedState(fixture.Bundle.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			if err = published.SavePublication(fixture.Bundle, fixture.Bundle.Capture.CapturedAt, fixture.SourceReference(), fixture.MetadataBytes); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("published", fixture.Bundle.ArchiveSessionID+".json")
			if corrupt {
				if err = os.WriteFile(filepath.Join(s.home, path), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(filepath.Join(s.home, path))
			if err != nil {
				t.Fatal(err)
			}
			budget := agentapi.NewNativeReadBudget(8 << 20)
			owner := budget.Reservation()
			const pressure = 32 << 10
			if !owner.Reserve(pressure) {
				t.Fatal("pressure marker")
			}
			scoped, end := s.WithReadBudget(t.Context(), budget)
			defer end()
			closeFailure := errors.New("synthetic actual descriptor close failure")
			closes := 0
			scoped.publicationFileClose = func(f *os.File) error { closes++; return errors.Join(f.Close(), closeFailure) }
			err = config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) (err error) {
				q, e := scoped.openDurableQuota(g)
				if e != nil {
					return e
				}
				defer func() { err = errors.Join(err, q.Close()) }()
				info, e := q.home.Root.Lstat(path)
				if e != nil {
					return e
				}
				mode, e := q.publishedProtocol(t.Context(), path, info)
				if mode != 0 || !errors.Is(e, closeFailure) || corrupt && !errors.Is(e, ErrDurableStorageRecovery) {
					t.Fatalf("classification escaped close failure mode=%d err=%v", mode, e)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if used, _ := budget.Charged(); used != pressure || closes != 1 {
				t.Fatalf("read loan/close count %d/%d", used, closes)
			}
			after, err := os.ReadFile(filepath.Join(s.home, path))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("authority changed %v", err)
			}
		})
	}
}

func TestPublicationRootedReadClosePreservesCanceledErrorAndExactLoan(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "successful-read", true: "canceled-read"}[canceled], func(t *testing.T) {
			s := newTestStore(t)
			path := filepath.Join("pending", "original.json")
			if err := os.MkdirAll(filepath.Join(s.home, "pending"), 0700); err != nil {
				t.Fatal(err)
			}
			original := []byte("exact original private authority")
			if err := os.WriteFile(filepath.Join(s.home, path), original, 0600); err != nil {
				t.Fatal(err)
			}
			budget := agentapi.NewNativeReadBudget(8 << 20)
			owner := budget.Reservation()
			const pressure = 32 << 10
			if !owner.Reserve(pressure) {
				t.Fatal("pressure marker")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if canceled {
				cancel()
			}
			scoped, end := s.WithReadBudget(ctx, budget)
			closeFailure := errors.New("synthetic rooted actual close failure")
			closes := 0
			scoped.publicationFileClose = func(f *os.File) error { closes++; return errors.Join(f.Close(), closeFailure) }
			if err := config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) error {
				raw, release, err := scoped.rootedPublicationBytes(g, path)
				if raw != nil || release != nil || !errors.Is(err, closeFailure) || canceled && !errors.Is(err, context.Canceled) {
					t.Fatalf("read authority escaped failed close %q %v %v", raw, release != nil, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			end()
			end()
			if used, _ := budget.Charged(); used != pressure || closes != 1 {
				t.Fatalf("read loan/close count %d/%d", used, closes)
			}
			after, err := os.ReadFile(filepath.Join(s.home, path))
			if err != nil || !bytes.Equal(original, after) {
				t.Fatalf("original changed %v", err)
			}
		})
	}
}

func TestPublicationEvidenceEOFClosingFailureKeepsCommittedMigration(t *testing.T) {
	s := newTestStore(t)
	fixture := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	fixture.History = &PendingHistory{Version: 1}
	id := fixture.Bundle.ArchiveSessionID
	if err := s.SavePending(id, fixture); err != nil {
		t.Fatal(err)
	}
	pending, err := PreparePublicationV2(fixture, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePending(id, pending); err != nil {
		t.Fatal(err)
	}
	published, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(pending, fixture.Bundle.Capture.CapturedAt); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.home, evidencePath(id))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := os.ReadFile(s.pendingPath(id))
	if err != nil {
		t.Fatal(err)
	}
	closeFailure := errors.New("synthetic evidence enumeration close failure")
	enumerations := 0
	s.publicationFileClose = func(file *os.File) error {
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil {
			return errors.Join(statErr, closeErr)
		}
		if info.IsDir() {
			enumerations++
			return errors.Join(closeErr, closeFailure)
		}
		return closeErr
	}
	if err = s.SettlePublicationMigration(id, pending); !errors.Is(err, closeFailure) {
		t.Fatalf("EOF masked actual close error %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || enumerations != 1 {
		t.Fatalf("migration evidence removed before checked enumeration %v %d", err, enumerations)
	}
	remaining, err := os.ReadFile(s.pendingPath(id))
	if err != nil || !bytes.Equal(descriptor, remaining) {
		t.Fatalf("pending original changed %v", err)
	}
	s.publicationFileClose = nil
	if err = s.SettlePublicationMigration(id, pending); err != nil {
		t.Fatal("owned retry", err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful checked retry retained migration %v", err)
	}
}

func TestPublicationMigrationFinalRootCloseReportsCompletedCleanup(t *testing.T) {
	s := newTestStore(t)
	fixture := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	fixture.History = &PendingHistory{Version: 1}
	id := fixture.Bundle.ArchiveSessionID
	if err := s.SavePending(id, fixture); err != nil {
		t.Fatal(err)
	}
	pending, err := PreparePublicationV2(fixture, PublicationPredecessor{State: PredecessorAbsent}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SavePending(id, pending); err != nil {
		t.Fatal(err)
	}
	published, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveCommittedPublication(pending, fixture.Bundle.Capture.CapturedAt); err != nil {
		t.Fatal(err)
	}

	reg := registration(t)
	reg.ArchiveSessionID = id
	reg.NativeSessionID = fixture.Bundle.NativeSessionID
	reg.ProjectID = fixture.Bundle.ProjectID
	reg.Harness = fixture.Bundle.Capture.Harness
	if err = s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveRequest(id, "synthetic owed request", fixture.Bundle.Capture.CapturedAt); err != nil {
		t.Fatal(err)
	}
	paths := []string{s.pendingPath(id), filepath.Join(s.home, "published", id+".json"), filepath.Join(s.home, "requests", id+".json")}
	originals := make([][]byte, len(paths))
	for i, path := range paths {
		originals[i], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	closeFailure := errors.New("synthetic final owned root close failure")
	closes := 0
	s.publicationRootClose = func(root *os.Root) error { closes++; return errors.Join(root.Close(), closeFailure) }
	if err = s.SettlePublicationMigration(id, pending); !errors.Is(err, closeFailure) {
		t.Fatalf("callback lost final close error %v", err)
	}
	if closes != 1 {
		t.Fatalf("owned root closes %d", closes)
	}
	// Enumeration and durable removal already completed; final close failure
	// cannot promise a rollback. Caller still retains its full retry descriptor.
	if _, err = os.Stat(filepath.Join(s.home, evidencePath(id))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed evidence removal %v", err)
	}
	for i, path := range paths {
		after, e := os.ReadFile(path)
		if e != nil || !bytes.Equal(originals[i], after) {
			t.Fatalf("retained retry authority changed %s: %v", path, e)
		}
	}
	s.publicationRootClose = nil
	if err = s.SettlePublicationMigration(id, pending); err != nil {
		t.Fatal("idempotent owned cleanup retry", err)
	}
	restored, err := s.LoadPublishedState(id)
	if err != nil {
		t.Fatal(err)
	}
	if restored.state.Commit == nil || restored.state.Commit.MetadataSHA256 != pending.Commit.MetadataSHA256 {
		t.Fatal("full selecting commit lost")
	}
}
