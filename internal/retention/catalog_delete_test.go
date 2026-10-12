package retention

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type retentionCatalogFixture struct{ *storagetest.MemoryStore }

func (*retentionCatalogFixture) CatalogAtomicQualification() error { return nil }

func TestWholeSessionCatalogDeletionRetainsSnapshotSources(t *testing.T) {
	underlying := &retentionCatalogFixture{storagetest.NewMemoryStore()}
	remote, err := catalog.Wrap(underlying)
	if err != nil {
		t.Fatal(err)
	}
	source := []byte("private synthetic source")
	key := "sessions/claude/retained/source." + storage.SHA256Hex(source) + ".jsonl.gz"
	if err = remote.Put(t.Context(), key, source); err != nil {
		t.Fatal(err)
	}
	metadata := archive.Metadata{SchemaVersion: 1, SessionID: "retained", Harness: archive.Harness{Name: "claude"}, CapturedAt: time.Now(), SourceBundle: archive.SourceReference{Key: key, SHA256: storage.SHA256Hex(source), CompressedBytes: len(source)}}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey := "sessions/claude/retained/metadata.json"
	if err = remote.Publication("publication", metadataKey, "").Put(t.Context(), metadataKey, raw); err != nil {
		t.Fatal(err)
	}
	// The measured wrapper must preserve transactional deletion authority.
	store := storagetest.NewMeasuredStore(remote, 0)
	if err = DeleteWholeSession(t.Context(), store, "claude", "retained"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get(t.Context(), metadataKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("metadata still visible", err)
	}
	if _, err = underlying.Get(t.Context(), key); err != nil {
		t.Fatal("source deleted before snapshot expiry", err)
	}
	if err = store.Delete(t.Context(), key); !errors.Is(err, catalog.ErrGCRequired) {
		t.Fatal("unfenced physical source delete permitted", err)
	}
}
