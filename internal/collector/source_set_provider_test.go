package collector

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestOrdinarySourceSetAttemptKeepsCapturedProvider(t *testing.T) {
	t.Parallel()
	for _, nestedPublication := range []bool{false, true} {
		t.Run(map[bool]string{false: "source-verifier", true: "nested-publication"}[nestedPublication], func(t *testing.T) {
			t.Parallel()
			local, actual, reg, opts, pending, _ := ordinaryPrivacyFixture(t)
			if err := actual.Delete(t.Context(), pending.SourceKey); err != nil {
				t.Fatal(err)
			}
			if err := actual.MemoryStore.Put(t.Context(), pending.MetadataKey, pending.MetadataBytes); err != nil {
				t.Fatal(err)
			}
			published, err := local.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			scan := newSessionScan(t.Context(), local, actual, reg, state.Request{}, published, opts.Now(), opts)
			defer scan.releaseRetained()
			end, err := scan.beginPublicationAttempt()
			if err != nil {
				t.Fatal(err)
			}
			defer end()
			ambient := storagetest.NewMemoryStore()
			if err := ambient.Put(t.Context(), pending.SourceKey, pending.SourceBytes); err != nil {
				t.Fatal(err)
			}
			winner := []byte(`{"synthetic":"unrelated ambient metadata"}`)
			if err := ambient.Put(t.Context(), pending.MetadataKey, winner); err != nil {
				t.Fatal(err)
			}
			scan.remote = ambient
			if nestedPublication {
				if outcome, err := scan.sourceSetPublishPending(pending); err != nil || outcome != outcomePublished {
					t.Fatal("nested publication retargeted captured provider", outcome, err)
				}
			} else {
				receipt, err := scan.sourceSetUpload(pending)
				if err != nil || receipt == nil {
					t.Fatal("captured publication did not complete", err)
				}
				if _, _, err := receipt.Consume(pending.MetadataKey, pending.MetadataBytes); err != nil {
					t.Fatal(err)
				}
			}
			source, err := actual.Get(t.Context(), pending.SourceKey)
			if err != nil || !bytes.Equal(source, pending.SourceBytes) {
				t.Fatal("ambient checksum hid missing source in captured provider", err)
			}
			selected, err := actual.Get(t.Context(), pending.MetadataKey)
			if err != nil || !bytes.Equal(selected, pending.MetadataBytes) {
				t.Fatal("captured metadata changed", err)
			}
			unrelated, err := ambient.Get(t.Context(), pending.MetadataKey)
			if err != nil || !bytes.Equal(unrelated, winner) {
				t.Fatal("ambient winner was touched", err)
			}
		})
	}
}

type selectedReadbackStore struct {
	*storagetest.MemoryStore
	metadataKey string
	sourceKey   string
}

func (s *selectedReadbackStore) Put(ctx context.Context, key string, body []byte) error {
	if key == s.metadataKey || key == s.sourceKey {
		return errors.New("already-selected retry attempted source or metadata upload")
	}
	return s.MemoryStore.Put(ctx, key, body)
}

func TestFreshSelectedSourceSetAcknowledgmentRequiresActualReferences(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "source-missing"}[missing], func(t *testing.T) {
			t.Parallel()
			local, actual, reg, opts, pending, raw := ordinaryPrivacyFixture(t)
			if err := actual.MemoryStore.Put(t.Context(), pending.MetadataKey, pending.MetadataBytes); err != nil {
				t.Fatal(err)
			}
			if missing {
				if err := actual.Delete(t.Context(), pending.SourceKey); err != nil {
					t.Fatal(err)
				}
			}
			remote := &selectedReadbackStore{MemoryStore: actual.MemoryStore, metadataKey: pending.MetadataKey, sourceKey: pending.SourceKey}
			published, err := local.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			scan := newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, opts.Now(), opts)
			defer scan.releaseRetained()
			outcome, err := scan.sourceSetAcknowledgePublication(pending, false)
			if missing {
				if !errors.Is(err, storage.ErrNotFound) {
					t.Fatal("missing actual source was acknowledged", outcome, err)
				}
				current, found, readErr := local.LoadPublicationPending(reg.ArchiveSessionID)
				if readErr != nil || !found || current.Commit == nil {
					t.Fatal("unverified pending owner lost", found, readErr)
				}
				body, readErr := os.ReadFile(filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json"))
				if readErr != nil || !bytes.Equal(body, raw) {
					t.Fatal("missing-source retry changed pending evidence", readErr)
				}
				return
			}
			if err != nil || outcome != outcomePublished {
				t.Fatal("verified fresh-frame retry did not drain", outcome, err)
			}
			if found, err := local.HasPending(reg.ArchiveSessionID); err != nil || found {
				t.Fatal("verified owner remains pending", found, err)
			}
			selected, err := remote.Get(t.Context(), pending.MetadataKey)
			if err != nil || !bytes.Equal(selected, pending.MetadataBytes) {
				t.Fatal("selected metadata changed", err)
			}
		})
	}
}
