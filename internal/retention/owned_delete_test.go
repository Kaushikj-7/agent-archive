package retention

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type retainedDeletionHookStore struct {
	*storagetest.MemoryStore
	hook       func()
	failed     bool
	failSource bool
}

func (s *retainedDeletionHookStore) Delete(ctx context.Context, key string) error {
	if strings.HasSuffix(key, "/metadata.json") && s.hook != nil {
		hook := s.hook
		s.hook = nil
		hook()
	}
	if s.failSource && strings.Contains(key, "/source.") && !s.failed {
		s.failed = true
		return errors.New("synthetic source delete failure")
	}
	return s.MemoryStore.Delete(ctx, key)
}

func TestRetentionRestoresExactBaselineWithoutNativeAndKeepsNewHookOwed(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	remote := &retainedDeletionHookStore{MemoryStore: storagetest.NewMemoryStore()}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registration("exact-retained", writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	result := collect(t, local, remote, at)
	if len(result.Errors) > 0 {
		t.Fatal(result.Errors)
	}
	key, _ := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	before, err := remote.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	expiry := at.Add(91 * 24 * time.Hour)
	remote.hook = func() {
		if err := local.SaveRequest(reg.ArchiveSessionID, "stop", expiry, finalResponse(t, expiry)); err != nil {
			t.Error(err)
		}
		if err := os.Remove(reg.TranscriptPath); err != nil {
			t.Error(err)
		}
	}
	resultSweep := sweep(t, local, remote, expiry, Options{})
	if len(resultSweep.Errors) > 0 || len(resultSweep.DeletedSessions) > 0 {
		t.Fatal(resultSweep)
	}
	if _, err := remote.Get(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("selector not removed", err)
	}
	j, found, err := local.LoadSessionDeletion(reg)
	if err != nil || !found || j.Phase != state.DeletionAbsent {
		t.Fatal(j.Phase, found, err)
	}
	for _, ref := range j.References {
		if _, err := local.ReadDeletionSource(reg.ArchiveSessionID, ref); err != nil {
			t.Fatal("original source not retained", err)
		}
	}
	restarted, err := state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	for iteration := range 2 {
		result = collect(t, restarted, remote, expiry)
		if result.Errors[reg.ArchiveSessionID] != nil {
			t.Fatalf("iteration %d: %v", iteration, result.Errors)
		}
	}
	after, err := remote.Get(t.Context(), key)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("restoration changed exact metadata or capture age", err)
	}
	req, found, err := restarted.LoadRequest(reg.ArchiveSessionID)
	if err != nil || !found || len(req.HookEvidence) != 1 {
		t.Fatal("baseline restoration acknowledged uncovered hook", found, err)
	}
	j, found, err = restarted.LoadSessionDeletion(reg)
	if err != nil || found {
		t.Fatal("completed restoration kept replay authority", j.Phase, found, err)
	}
}

func TestOwnedDeletionResumesAfterSourceFailureWithoutNative(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	remote := &retainedDeletionHookStore{MemoryStore: storagetest.NewMemoryStore(), failSource: true}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registration("delete-restart", writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if result := collect(t, local, remote, at); len(result.Errors) > 0 {
		t.Fatal(result.Errors)
	}
	expiry := at.Add(91 * 24 * time.Hour)
	first := sweep(t, local, remote, expiry, Options{})
	if first.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal("source failure not reported")
	}
	if err := os.Remove(reg.TranscriptPath); err != nil {
		t.Fatal(err)
	}
	restarted, err := state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	second := sweep(t, restarted, remote, expiry, Options{})
	if len(second.Errors) > 0 || len(second.DeletedSessions) != 1 {
		t.Fatal(second)
	}
	j, found, err := restarted.LoadSessionDeletion(reg)
	if err != nil || !found || !j.LocalRemoved || j.Phase != state.DeletionCleaned {
		t.Fatal(j, found, err)
	}
	if refs, err := remote.List(t.Context(), "sessions/codex/"+reg.ArchiveSessionID+"/"); err != nil || len(refs) > 0 {
		t.Fatal(refs, err)
	}
	if owed, err := restarted.DurableStorageObligations(); err != nil || len(owed) > 0 {
		t.Fatal(owed, err)
	}
}

func TestExplicitUndoCannotRestoreRetainedOrNativeSources(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	remote := &retainedDeletionHookStore{MemoryStore: storagetest.NewMemoryStore()}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registration("undo-no-restore", writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if result := collect(t, local, remote, at); len(result.Errors) > 0 {
		t.Fatal(result.Errors)
	}
	expiry := at.Add(91 * 24 * time.Hour)
	remote.hook = func() {
		if err := local.SaveRequest(reg.ArchiveSessionID, "stop", expiry, finalResponse(t, expiry)); err != nil {
			t.Error(err)
		}
	}
	if result := sweep(t, local, remote, expiry, Options{}); len(result.Errors) > 0 {
		t.Fatal(result.Errors)
	}
	if err := DeleteOwnedSession(t.Context(), local, remote, reg, state.RemovalReasonUndo, expiry); err != nil {
		t.Fatal(err)
	}
	if result := collect(t, local, remote, expiry); !errors.Is(result.Errors[reg.ArchiveSessionID], state.ErrRemovalPending) || len(result.Published) > 0 {
		t.Fatal("undo resurrected retained/native source", result)
	}
	key, _ := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if _, err := remote.Get(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("undo selector restored", err)
	}
}

type deletionSourceBoundaryStore struct {
	*storagetest.MemoryStore
	beforeSource func([]byte) []byte
	deletes      int
}

func (s *deletionSourceBoundaryStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	data, err := s.MemoryStore.GetLimited(ctx, key, limit)
	if err == nil && strings.Contains(key, "/source.") && s.beforeSource != nil {
		fn := s.beforeSource
		s.beforeSource = nil
		data = fn(data)
	}
	return data, err
}
func (s *deletionSourceBoundaryStore) Delete(ctx context.Context, key string) error {
	s.deletes++
	return s.MemoryStore.Delete(ctx, key)
}

func TestOwnedDeletionRefusesSourceWinnerOrIntentChangesBeforeMetadataRemoval(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"source", "winner", "intent"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			local := newTestStore(t)
			remote := &deletionSourceBoundaryStore{MemoryStore: storagetest.NewMemoryStore()}
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			reg := registration("boundary-"+kind, writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript))
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			if result := collect(t, local, remote, at); len(result.Errors) > 0 {
				t.Fatal(result.Errors)
			}
			key, _ := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
			original, err := remote.Get(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			winner := original
			remote.beforeSource = func(data []byte) []byte {
				switch kind {
				case "source":
					data = bytes.Clone(data)
					data[0] ^= 1
				case "winner":
					winner = append(bytes.Clone(original), ' ')
					if err := remote.MemoryStore.Put(t.Context(), key, winner); err != nil {
						t.Fatal(err)
					}
				case "intent":
					path := filepath.Join(local.Home(), "session-deletions", reg.ArchiveSessionID, "intent.json")
					body, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, append(body, ' '), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return data
			}
			if err := DeleteOwnedSession(t.Context(), local, remote, reg, state.RemovalReasonRetention, at.Add(time.Hour)); err == nil {
				t.Fatal("changed deletion authority accepted")
			}
			if remote.deletes != 0 {
				t.Fatal("refusal deleted remote evidence", remote.deletes)
			}
			after, err := remote.Get(t.Context(), key)
			if err != nil || !bytes.Equal(after, winner) {
				t.Fatal("refusal changed selecting winner", err)
			}
		})
	}
}
