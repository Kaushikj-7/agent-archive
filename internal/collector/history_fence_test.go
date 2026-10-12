package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestHistoryPublicationRemainsFencedAndMalformedRefilterRefuses(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := storagetest.NewMemoryStore()
	reg := registration(t, "/synthetic/native.jsonl")
	sourceKey := "sessions/codex/session-1/source.protected.jsonl.gz"
	metadataKey := "sessions/codex/session-1/metadata.json"
	protected := []byte("protected source bytes")
	prior := archive.Metadata{SchemaVersion: archive.HistoryMetadataSchemaVersion, History: &archive.RevisionHistory{CurrentRevision: "11111111-1111-4111-8111-111111111111"}}
	meta, _ := json.Marshal(prior)
	if e := store.Put(ctx, sourceKey, protected); e != nil {
		t.Fatal(e)
	}
	if e := store.Put(ctx, metadataKey, meta); e != nil {
		t.Fatal(e)
	}
	next, _ := json.Marshal(archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion})
	s := sessionScan{ctx: ctx, reg: reg, remote: store}
	pending := state.PendingPublication{MetadataKey: metadataKey, MetadataBytes: next}
	if _, e := s.publishPending(s.ctx, pending); !errors.Is(e, archive.ErrHistoryMutationPending) {
		t.Fatalf("remote protected history: %v", e)
	}
	history := archive.SourceBundle{SchemaVersion: archive.HistorySourceSchemaVersion, History: &archive.SourceHistory{}}
	if _, e := refilterBundle(ctx, reg, nil, history); e == nil {
		t.Fatalf("refilter protection: %v", e)
	}
	pending.Bundle = history
	if _, e := s.publishPending(s.ctx, pending); !errors.Is(e, archive.ErrHistoryMutationPending) {
		t.Fatalf("new history publication: %v", e)
	}
	for key, want := range map[string][]byte{metadataKey: meta, sourceKey: protected} {
		got, e := store.Get(ctx, key)
		if e != nil || !bytes.Equal(got, want) {
			t.Fatalf("protected artifact changed: %s %v", key, e)
		}
	}
}

func TestHistoryCachedStateRefusesMaintenanceBeforeWrites(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	p, e := local.LoadPublishedState("session-1")
	if e != nil {
		t.Fatal(e)
	}
	b := archive.SourceBundle{SchemaVersion: archive.HistorySourceSchemaVersion, History: &archive.SourceHistory{}}
	if e := p.Save(b, time.Now(), state.CacheStatusPublished); e != nil {
		t.Fatal(e)
	}
	s := sessionScan{published: p}
	if _, e := s.run(s.ctx); !errors.Is(e, archive.ErrHistoryMutationPending) {
		t.Fatalf("cached history maintenance: %v", e)
	}
}
