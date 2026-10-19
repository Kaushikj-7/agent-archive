package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

type deletionBoundaryCancellation struct {
	context.Context
	path   string
	cancel context.CancelFunc
}

func (c deletionBoundaryCancellation) Err() error {
	if _, err := os.Stat(c.path); err == nil {
		c.cancel()
	}
	return c.Context.Err()
}

// The real preparation path commits the observed boundary before cancellation;
// replay uses a new Store read scope, retaining the original admitted owner.
func interruptedDeletionPreparation(t *testing.T, reason RemovalReason, afterMetadata bool) (*Store, archive.SessionRegistration, PendingPublication, SessionDeletion) {
	t.Helper()
	s := newTestStore(t)
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	p := publicationFixture(t, "22222222-2222-4222-8222-222222222222", at)
	reg := registration(t)
	reg.ArchiveSessionID = p.Bundle.ArchiveSessionID
	reg.NativeSessionID = p.Bundle.NativeSessionID
	reg.ProjectID = p.Bundle.ProjectID
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	boundary := filepath.Join(s.home, deletionPath(reg.ArchiveSessionID))
	if afterMetadata {
		boundary = filepath.Join(s.home, deletionDirectory, reg.ArchiveSessionID, "sources", "metadata.json")
	}
	observed := deletionBoundaryCancellation{Context: ctx, path: boundary, cancel: cancel}
	scoped, closeScope := s.WithReadBudget(observed, agentapi.NewNativeReadBudget(agentapi.PassReadBudget))
	t.Cleanup(closeScope)
	if _, err := scoped.PrepareSessionDeletion(reg, reason, p.MetadataBytes, at.Add(time.Hour), ""); !errors.Is(err, context.Canceled) {
		t.Fatal("preparation did not interrupt at durable boundary", err)
	}
	if _, err := os.Stat(boundary); err != nil {
		t.Fatal("boundary not durable", err)
	}
	j, found, err := s.LoadSessionDeletion(reg)
	if err != nil || !found || j.Phase != DeletionPreparing {
		t.Fatal("partial preparation acquired ready authority", j.Phase, found, err)
	}
	return s, reg, p, j
}

func TestDeletionPreparationResumesAfterMetadataDurabilityCancellation(t *testing.T) {
	t.Parallel()
	s, reg, p, original := interruptedDeletionPreparation(t, RemovalReasonUndo, true)
	replay, err := s.PrepareSessionDeletion(reg, RemovalReasonUndo, p.MetadataBytes, original.At.Add(24*time.Hour), "")
	if err != nil {
		t.Fatal("fresh retry stranded exact durable metadata", err)
	}
	if replay.Phase != DeletionPrepared || !replay.At.Equal(original.At) || replay.IntentSHA256() != original.IntentSHA256() || !bytes.Equal(replay.MetadataBytes, p.MetadataBytes) {
		t.Fatal("retry reset immutable capture/intent evidence", replay)
	}
}

func TestDeletionPreparingPinsOwnerBeforeAnyRetainedMetadata(t *testing.T) {
	t.Parallel()
	s, reg, p, j := interruptedDeletionPreparation(t, RemovalReasonUndo, false)
	metadataPath := filepath.Join(s.home, deletionDirectory, reg.ArchiveSessionID, "sources", "metadata.json")
	if _, err := os.Stat(metadataPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("metadata preceded durable owner", err)
	}
	original, err := os.ReadFile(filepath.Join(s.home, deletionPath(reg.ArchiveSessionID)))
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	m.FilterVersion += "-different-policy"
	changedPolicy, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{append(bytes.Clone(p.MetadataBytes), ' '), changedPolicy} {
		if _, err := s.PrepareSessionDeletion(reg, RemovalReasonUndo, raw, j.At, ""); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatal("changed selecting wire/policy adopted", err)
		}
	}
	other := reg
	other.TranscriptPath += "-foreign"
	if _, err := s.PrepareSessionDeletion(other, RemovalReasonUndo, p.MetadataBytes, j.At, ""); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("changed admitted owner adopted", err)
	}
	if _, err := s.AdvanceSessionDeletion(j, DeletionDeleting); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("partial preparation authorized deletion", err)
	}
	if after, err := os.ReadFile(filepath.Join(s.home, deletionPath(reg.ArchiveSessionID))); err != nil || !bytes.Equal(original, after) {
		t.Fatal("refusal rewrote frozen intent", err)
	}
	replay, err := s.PrepareSessionDeletion(reg, RemovalReasonUndo, p.MetadataBytes, j.At.Add(time.Hour), "")
	if err != nil || replay.Phase != DeletionPrepared || !replay.At.Equal(j.At) {
		t.Fatal("exact retry failed", replay.Phase, err)
	}
}

func TestDeletionPreparingPreservesNewHookAndCannotRestore(t *testing.T) {
	t.Parallel()
	s, reg, p, j := interruptedDeletionPreparation(t, RemovalReasonRetention, false)
	if err := s.SaveRequest(reg.ArchiveSessionID, "late-hook", j.At.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	req, found, err := s.LoadRequest(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	before, err := os.ReadFile(s.requestPath(reg.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareRetentionRestoration(t.Context(), nil, reg, req, nil, "", "", ""); !errors.Is(err, ErrRemovalPending) {
		t.Fatal("preparing granted restoration", err)
	}
	if _, err := s.PrepareSessionDeletion(reg, RemovalReasonRetention, p.MetadataBytes, j.At, ""); !errors.Is(err, ErrDeletionWorkChanged) {
		t.Fatal("preparing ignored new hook", err)
	}
	if err := s.CancelUnstartedDeletion(reg, req); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRestoredDeletionStages(reg.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(s.requestPath(reg.ArchiveSessionID)); err != nil || !bytes.Equal(before, after) {
		t.Fatal("preparation cancellation acknowledged new hook", err)
	}
}

func TestDeletionPreparingUnknownStageRemainsRecovery(t *testing.T) {
	t.Parallel()
	s, reg, p, j := interruptedDeletionPreparation(t, RemovalReasonUndo, false)
	path := filepath.Join(s.home, deletionDirectory, reg.ArchiveSessionID, ".pending-foreign")
	unknown := []byte("unowned stage remains evidence")
	if err := os.WriteFile(path, unknown, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareSessionDeletion(reg, RemovalReasonUndo, p.MetadataBytes, j.At, ""); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("unknown stage adopted", err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(unknown, after) {
		t.Fatal("unknown stage removed", err)
	}
}
