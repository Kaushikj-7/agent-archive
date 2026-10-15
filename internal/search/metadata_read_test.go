package search

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestSearchMetadataAndContextUseFreshCatalogAuthority(t *testing.T) {
	a := newSearchArchive(t)
	m := a.session(t, "session")
	key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.raw.Get(t.Context(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("fixture unexpectedly has a canonical legacy sidecar: %v", err)
	}
	pinned := catalog.NewReadView(t.Context())
	if _, err = catalog.OpenSnapshot(pinned, a.remote, nil); err != nil {
		t.Fatal(err)
	}
	m.Name = "new catalog title"
	a.publish(t, m)
	selected, err := ReadCurrentMetadata(pinned, a.remote, key)
	if err != nil || selected.Name != m.Name {
		t.Fatalf("stale caller view %+v %v", selected, err)
	}
	parsers := builtin.NewBuiltins()
	p, err := LoadProjection(t.Context(), a.remote, parsers, "test/context", m, "", reader.Limits{})
	if err != nil || len(p.Passages) == 0 {
		t.Fatalf("projection %+v %v", p, err)
	}
	before := a.raw.sourceGets
	ref := p.Passages[0].Reference
	out, err := ReadContext(pinned, a.remote, parsers, ref, ref.Start, 4096, 2, 2, reader.Limits{})
	if err != nil || out.Selected.Text != p.Passages[0].Text || a.raw.sourceGets-before != 1 || a.raw.lists != 0 {
		t.Fatalf("catalog exact read %+v source_GET=%d lists=%d %v", out, a.raw.sourceGets-before, a.raw.lists, err)
	}
	a.mutate(t, key, nil)
	if _, err = ReadCurrentMetadata(pinned, a.remote, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("fresh deletion: %v", err)
	}
}

func TestSearchMetadataRejectsRootMovementDuringSelectedRead(t *testing.T) {
	a := newSearchArchive(t)
	m := a.session(t, "session")
	key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	a.raw.beforeRead = func(key string) {
		if !strings.Contains(key, "metadata/") {
			return
		}
		a.raw.beforeRead = nil
		m.Name = "new root"
		a.publish(t, m)
	}
	selected, err := ReadCurrentMetadata(t.Context(), a.remote, key)
	if !errors.Is(err, catalog.ErrStaleCursor) || selected.SessionID != "" {
		t.Fatalf("stale bytes escaped validation: %+v %v", selected, err)
	}
}

type failedBoundedMetadata struct {
	*storagetest.MemoryStore
	key       string
	raw       []byte
	limit     int64
	unbounded int
	sources   int
}

func (s *failedBoundedMetadata) Get(ctx context.Context, key string) ([]byte, error) {
	s.unbounded++
	return nil, errors.New("unbounded metadata read")
}

func (s *failedBoundedMetadata) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	s.limit = limit
	if key == s.key {
		return s.raw, "", storage.ErrObjectTooLarge
	}
	return s.MemoryStore.GetLimitedVersioned(ctx, key, limit)
}

func (s *failedBoundedMetadata) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if strings.Contains(key, "/source.") {
		s.sources++
	}
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

func TestSearchLegacyMetadataFailureNeverDecodesPartialBodyOrLoadsSource(t *testing.T) {
	m, _, objects := verifiedFixture(t, "codex", `{"type":"response_item","payload":{"type":"message","role":"user","content":"checksum"}}`)
	key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	failed := &failedBoundedMetadata{MemoryStore: objects, key: key, raw: raw}
	selected, err := ReadCurrentMetadata(t.Context(), failed, key)
	if err == nil || selected.SessionID != "" || failed.unbounded != 0 || failed.limit <= 0 || failed.limit+1 > 16<<20 {
		t.Fatalf("bounded failed body %+v limit=%d unbounded=%d %v", selected, failed.limit, failed.unbounded, err)
	}
	p, err := LoadProjection(t.Context(), objects, builtin.NewBuiltins(), "test/context", m, "", reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	ref := p.Passages[0].Reference
	out, err := ReadContext(t.Context(), failed, builtin.NewBuiltins(), ref, ref.Start, 4096, 0, 0, reader.Limits{})
	if !errors.Is(err, ErrUnavailable) || out.Selected.Text != "" || failed.sources != 0 || failed.unbounded != 0 {
		t.Fatalf("partial metadata interpreted %+v sources=%d %v", out, failed.sources, err)
	}
}
