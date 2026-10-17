package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestHistoryPublicationRemainsFencedAndMalformedRefilterRefuses(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := storagetest.NewMemoryStore()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "native.jsonl", codexTranscript))
	opts := Options{MachineID: "machine", Sources: testSources, Parsers: testParsers, Now: func() time.Time { return reg.RegisteredAt.Add(time.Hour) }}
	producer := storagetest.NewMemoryStore()
	ordinary := publishOnce(t, local, producer, reg, &opts)
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	bundle, at, _ := published.LastPublished()
	sourceBytes, err := producer.Get(ctx, ordinary.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
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
	s := newSessionScan(ctx, local, store, reg, state.Request{}, published, at.Add(time.Hour), opts)
	pending := state.PendingPublication{Bundle: bundle, SourceKey: ordinary.SourceBundle.Key, SourceSHA256: ordinary.SourceBundle.SHA256, SourceSize: ordinary.SourceBundle.CompressedBytes, SourceBytes: sourceBytes, MetadataKey: metadataKey, MetadataBytes: published.Metadata(), ReadyAt: at, Attempted: true}
	if e := s.checkHistoryPublicationBody(pending, meta); !errors.Is(e, archive.ErrHistoryMutationPending) {
		t.Fatalf("remote history body gate: %v", e)
	}
	// A refused metadata decode has established no remote conflict. Preserve
	// its budget classification so a later pass can retry the same evidence.
	endScope := s.readBudget().BeginScope(1)
	positionErr := (publicationTransactionStore{scan: s, pending: &pending}).ValidatePublicationPosition(ctx, metadataKey, meta)
	endScope()
	if !errors.Is(positionErr, agentapi.ErrReadBudget) || errors.Is(positionErr, storage.ErrPublicationConflict) {
		t.Fatalf("unread remote history position: %v", positionErr)
	}
	if _, e := s.publishPending(s.ctx, pending); !errors.Is(e, storage.ErrPublicationConflict) {
		t.Fatalf("foreign remote protected history: %v", e)
	}
	history := archive.SourceBundle{SchemaVersion: archive.HistorySourceSchemaVersion, History: &archive.SourceHistory{}}
	if _, e := refilterBundle(t, ctx, reg, nil, history); e == nil {
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
	before := snapshotMtimes(t, local.Home())
	s := sessionScan{ctx: t.Context(), local: local, reg: archive.SessionRegistration{ArchiveSessionID: "session-1"}, published: p}
	if _, e := s.run(s.ctx); !errors.Is(e, archive.ErrHistoryMutationPending) {
		t.Fatalf("cached history maintenance: %v", e)
	}
	if !reflect.DeepEqual(before, snapshotMtimes(t, local.Home())) {
		t.Fatal("fenced cached history maintenance wrote local state")
	}
}
