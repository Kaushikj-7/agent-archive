package collector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	localfiles "github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type collectorCatalogStore struct {
	*storagetest.MemoryStore
	local  *state.Store
	t      *testing.T
	failed bool
	frozen string
}

func (*collectorCatalogStore) CatalogAtomicQualification() error { return nil }

func (s *collectorCatalogStore) PutConditional(ctx context.Context, key string, data []byte, c storage.PutCondition) (string, error) {
	if strings.HasPrefix(key, "sessions/") {
		p, found, err := s.local.LoadPending("session-1")
		if err != nil || !found || p.Catalog == nil {
			s.t.Fatal("source uploaded before durable mutation", err)
		}
		if s.frozen != "" && s.frozen != p.Catalog.ID {
			s.t.Fatal("retry changed mutation ID")
		}
		s.frozen = p.Catalog.ID
	}
	if key == catalog.HeadKey && !s.failed {
		s.failed = true
		return "", errors.New("interrupted before head commit")
	}
	return s.MemoryStore.PutConditional(ctx, key, data, c)
}

func TestCollectorCatalogRetryFreezesMutationBeforeUpload(t *testing.T) {
	local := newTestStore(t)
	cfg, _, err := config.Load(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Storage.ArchiveFormat = destination.FormatCatalogV4
	if err = config.Save(local.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	raw := &collectorCatalogStore{MemoryStore: storagetest.NewMemoryStore(), local: local, t: t}
	remote, err := catalog.WrapConfigured(raw, cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
	if err = local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	guard, err := localfiles.LockCollectorGuard(raw.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { guard.Release() }()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at }, CollectorGuard: guard, CatalogDestination: config.DestinationID(cfg.Storage)}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) == 0 {
		t.Fatal("interrupted head was acknowledged")
	}
	pending, found, err := local.LoadPending("session-1")
	if err != nil || !found || pending.Catalog == nil || pending.Catalog.Recovery == nil || pending.Catalog.ID != raw.frozen {
		t.Fatal("frozen mutation missing after crash", err)
	}
	seal, err := remote.Writer.Coordinator().Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = remote.Writer.Coordinator().HeldBarrier(seal).Hold(t.Context()); !errors.Is(err, catalog.ErrAdmissionClosed) {
		t.Fatal("unfinished publication did not block GC", err)
	}
	// A genuinely fresh Store and a reacquired actual flock operate exclusively
	// on the same private persisted pending bytes, even after admission seals.
	guard.Release()
	guard, err = localfiles.LockCollectorGuard(raw.local.Home())
	if err != nil {
		t.Fatal(err)
	}
	remote, err = catalog.WrapConfigured(raw, cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	opts.CollectorGuard = guard
	at = at.Add(time.Hour)
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("retry: %+v %v", result, err)
	}
	if found, err = local.HasPending("session-1"); err != nil || found {
		t.Fatal("acknowledged retry stayed pending", err)
	}
	if removals, err := guard.JournalRemovals(); err != nil || len(removals) != 0 {
		t.Fatal("completed receipt cleanup remained pending", err)
	}
	if err = remote.Writer.Coordinator().Release(t.Context(), seal); err != nil {
		t.Fatal("resumed owner did not drain", err)
	}
	if _, err = raw.Get(t.Context(), pending.MetadataKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("canonical metadata was published")
	}
	if _, err = remote.Get(t.Context(), pending.MetadataKey); err != nil {
		t.Fatal("catalog metadata unavailable", err)
	}
	loaded, _, err := config.Load(local.Home())
	if err != nil || !loaded.DurableStorageProtection || loaded.SchemaVersion != 11 {
		t.Fatal("durable catalog writer fence missing", err)
	}
	rawConfig, err := json.Marshal(loaded)
	if err != nil || !strings.Contains(string(rawConfig), `"writer":"catalog-v4-v11"`) {
		t.Fatal("missing exact forward catalog writer marker", err)
	}
	var roundTrip config.Config
	if err = json.Unmarshal(rawConfig, &roundTrip); err != nil || roundTrip.SchemaVersion != 11 || !roundTrip.DurableStorageProtection {
		t.Fatal("catalog writer roundtrip", err)
	}
}

type historyCatalogStore struct{ *storagetest.MemoryStore }

func (*historyCatalogStore) CatalogAtomicQualification() error { return nil }

func TestFrozenHistoryUsesCatalogAuthority(t *testing.T) {
	scan, pending, cloud, old := frozenHistoryFixture(t)
	remote, err := catalog.Wrap(&historyCatalogStore{cloud})
	if err != nil {
		t.Fatal(err)
	}
	scan.remote = remote
	id, err := catalog.NewMutationID()
	if err != nil {
		t.Fatal(err)
	}
	pending.Catalog = &state.CatalogPublication{Protocol: 10, ID: id}
	if err = scan.local.SavePending(scan.id(), pending); err != nil {
		t.Fatal(err)
	}
	if err = scan.upload(pending); err != nil {
		t.Fatal(err)
	}
	committed, err := scan.checkFrozenHistoryMetadata(pending)
	if err != nil || !committed {
		t.Fatal("catalog authority not recognized", err)
	}
	if err = scan.verifyHistoryReadback(pending); err != nil {
		t.Fatal(err)
	}
	if _, err = cloud.Get(t.Context(), pending.MetadataKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("history canonical metadata dual-written")
	}
	if _, err = cloud.Get(t.Context(), old.Key); err != nil {
		t.Fatal("preserved source lost", err)
	}
	// Default legacy machinery cannot replay a catalog mutation descriptor.
	scan.remote = cloud
	if _, err = scan.publishPending(scan.ctx, pending); err == nil {
		t.Fatal("legacy destination admitted catalog pending")
	}
}

// A crash after the catalog head commits but before acknowledgement leaves
// a listing repair journal. Restart must not publish legacy discovery hints.
func TestCatalogListingRepairDoesNotPublishLegacyIndex(t *testing.T) {
	t.Parallel()
	scan, pending, cloud, _ := frozenHistoryFixture(t)
	remote, err := catalog.Wrap(&historyCatalogStore{cloud})
	if err != nil {
		t.Fatal(err)
	}
	if err = scan.local.SaveRegistration(scan.reg); err != nil {
		t.Fatal(err)
	}
	if err = remote.Publication("committed-before-crash", pending.MetadataKey, "").Put(t.Context(), pending.MetadataKey, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	if err = scan.local.SaveListingRepair(scan.id(), state.ListingRepair{MetadataKey: pending.MetadataKey, DestinationID: scan.reg.DestinationID}); err != nil {
		t.Fatal(err)
	}
	p := &pass{ctx: t.Context(), local: scan.local, remote: remote, result: Result{Errors: make(map[string]error)}}
	p.repairListingIndex()
	if len(p.result.Errors) != 0 {
		t.Fatal(p.result.Errors)
	}
	objects, err := cloud.List(t.Context(), "listing/")
	if err != nil || len(objects) != 0 {
		t.Fatal("catalog repair published legacy index", len(objects), err)
	}
	repairs, err := scan.local.ListingRepairs(32)
	if err != nil || len(repairs) != 0 {
		t.Fatal("catalog repair journal remained", err)
	}
}
