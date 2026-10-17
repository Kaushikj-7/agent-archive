package state

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestDeletionLoadRefusesCancellationAfterMetadataOpen(t *testing.T) {
	t.Parallel()
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
	before, err := os.ReadFile(filepath.Join(s.home, deletionPath(reg.ArchiveSessionID)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	scoped, closeScope := s.WithReadBudget(ctx, budget)
	defer closeScope()
	opened := false
	var bytesRead int64
	scoped.deletionFileOpen = func(root *os.Root, name string) (*os.File, error) {
		f, err := root.Open(name)
		if err == nil && name == "metadata.json" {
			opened = true
			cancel()
		}
		return f, err
	}
	scoped.publicationFileClose = func(f *os.File) error {
		if opened && filepath.Base(f.Name()) == "metadata.json" {
			bytesRead, _ = f.Seek(0, io.SeekCurrent)
		}
		return f.Close()
	}
	if _, _, err := scoped.LoadSessionDeletion(reg); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled metadata read returned deletion authority", err)
	}
	if bytesRead != 0 {
		t.Fatal("cancelled read consumed retained metadata", bytesRead)
	}
	if !opened {
		t.Fatal("metadata open boundary not reached")
	}
	after, err := os.ReadFile(filepath.Join(s.home, deletionPath(reg.ArchiveSessionID)))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("cancelled read changed durable intent", err)
	}
	closeScope()
	reservation := budget.Reservation()
	if !reservation.Reserve(agentapi.PassReadBudget) {
		t.Fatal("cancelled deletion read leaked shared reservation")
	}
	reservation.Release(agentapi.PassReadBudget)
	loaded, found, err := s.LoadSessionDeletion(reg)
	if err != nil || !found || !bytes.Equal(loaded.MetadataBytes, p.MetadataBytes) {
		t.Fatal("fresh retry lost exact deletion metadata", found, err)
	}
}

func TestDeletionSaveBindsExplicitCallerToRetainedControlRead(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	reg := registration(t)
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	j, err := s.PrepareSessionDeletion(reg, RemovalReasonRetention, nil, time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.home, deletionPath(reg.ArchiveSessionID))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opened := false
	var bytesRead int64
	s.deletionFileOpen = func(root *os.Root, name string) (*os.File, error) {
		f, err := root.Open(name)
		if err == nil && name == "intent.json" {
			opened = true
			cancel()
		}
		return f, err
	}
	s.publicationFileClose = func(f *os.File) error {
		if filepath.Base(f.Name()) == "intent.json" {
			bytesRead, _ = f.Seek(0, io.SeekCurrent)
		}
		return f.Close()
	}
	j.Phase = DeletionDeleting
	if err := s.saveSessionDeletionContext(ctx, j, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("explicit cancellation lost", err)
	}
	if !opened || bytesRead != 0 {
		t.Fatal("explicit caller cancellation did not stop actual control read", opened, bytesRead)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("cancelled transition changed durable intent", err)
	}
	s.deletionFileOpen = nil
	s.publicationFileClose = nil
	if err := s.saveSessionDeletionContext(t.Context(), j, nil); err != nil {
		t.Fatal("fresh exact transition retry failed", err)
	}
	replay, found, err := s.LoadSessionDeletion(reg)
	if err != nil || !found || replay.Phase != DeletionDeleting {
		t.Fatal("exact fresh transition not durable", found, err)
	}
}
