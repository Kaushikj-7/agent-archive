package collector

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type restorationRevocationStore struct {
	*restorationHookStore
	onSourceStat func()
}

func (s *restorationRevocationStore) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if strings.Contains(key, "/source.") && s.onSourceStat != nil {
		f := s.onSourceStat
		s.onSourceStat = nil
		f()
	}
	return s.MemoryStore.Stat(ctx, key)
}

func TestRetentionUndoRevokesRestorationBeforeMetadataPut(t *testing.T) {
	local := newTestStore(t)
	remote := &restorationRevocationStore{restorationHookStore: &restorationHookStore{MemoryStore: storagetest.NewMemoryStore()}}
	reg := registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	at := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", Now: func() time.Time { return at }}
	if result, err := Run(t.Context(), local, remote, opts); err != nil || result.Errors[reg.ArchiveSessionID] != nil {
		t.Fatal(result, err)
	}
	expiry := at.Add(91 * 24 * time.Hour)
	remote.hook = func() {
		if err := local.SaveRequest(reg.ArchiveSessionID, "stop", expiry); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(reg.TranscriptPath); err != nil {
			t.Fatal(err)
		}
	}
	if err := retention.DeleteOwnedSession(t.Context(), local, remote, reg, state.RemovalReasonRetention, expiry); !errors.Is(err, state.ErrDeletionWorkChanged) {
		t.Fatal(err)
	}
	opts.Now = func() time.Time { return expiry }
	if result, err := Run(t.Context(), local, remote, opts); err != nil || result.Errors[reg.ArchiveSessionID] != nil {
		t.Fatal(result, err)
	}
	key, _ := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if _, err := remote.Get(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("fixture selector not absent", err)
	}
	remote.onSourceStat = func() {
		j, err := local.PrepareSessionDeletion(reg, state.RemovalReasonUndo, nil, expiry, "")
		if err != nil || j.Reason != state.RemovalReasonUndo {
			t.Fatal("revoke restoration", j, err)
		}
	}
	result, runErr := Run(t.Context(), local, remote, opts)
	if remote.onSourceStat != nil {
		t.Fatal("revocation boundary not reached")
	}
	if runErr == nil && result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal("revoked restoration accepted")
	}
	if body, err := remote.Get(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("revoked restoration recreated metadata: bytes=%d err=%v result=%+v runErr=%v", len(body), err, result, runErr)
	}
}
