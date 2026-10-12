package reader

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type syntheticCatalogStore struct{ *storagetest.MemoryStore }

func (*syntheticCatalogStore) CatalogAtomicQualification() error { return nil }

func remoteReaderFixture(t *testing.T, count int) (*catalog.Store, *storagetest.MemoryStore) {
	t.Helper()
	raw := &syntheticCatalogStore{storagetest.NewMemoryStore()}
	legacy := storagetest.NewMemoryStore()
	w, err := catalog.New(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range count {
		id := fmt.Sprintf("session-%04d", i)
		source := []byte("synthetic source " + id)
		sha := storage.SHA256Hex(source)
		sourceKey := "sessions/claude/" + id + "/source." + sha + ".jsonl.gz"
		m := archive.Metadata{SchemaVersion: 1, SessionID: id, ParentSessionID: fixtureRemoteParent(i), Replay: fixtureRemoteReplay(i), Harness: archive.Harness{Name: "claude"}, ProjectID: "project", Title: "Résumé #212", CapturedAt: baseTime.Add(time.Duration(i) * time.Minute), SourceBundle: archive.SourceReference{Key: sourceKey, SHA256: sha, CompressedBytes: len(source)}}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.MetadataObjectKey("claude", id)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []*storagetest.MemoryStore{raw.MemoryStore, legacy} {
			if err = s.Put(t.Context(), sourceKey, source); err != nil {
				t.Fatal(err)
			}
		}
		if err = legacy.Put(t.Context(), key, data); err != nil {
			t.Fatal(err)
		}
		ref, err := w.PutImmutable(t.Context(), catalog.KindMetadata, data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Commit(t.Context(), catalog.CatalogMutation{ID: "fixture/" + id, SessionKey: key, Next: &catalog.CatalogEntry{Metadata: ref, Summary: m}}); err != nil {
			t.Fatal(err)
		}
	}
	c := w.Coordinator()
	owner, err := c.Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Activate(t.Context(), owner, "private synthetic cutover"); err != nil {
		t.Fatal(err)
	}
	if err = c.Release(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	remote, err := catalog.Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	return remote, legacy
}

func TestRemoteRootSelectionMatchesOracleWithoutCanonicalList(t *testing.T) {
	remote, legacy := remoteReaderFixture(t, 160)
	measured := storagetest.NewMeasuredStore(remote, 0)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range []QueryOrder{CaptureOrder, ActivityOrder} {
		q := MetadataQuery{Filter: Filter{Replays: ReplaysHidden}, Limit: 10, Order: order, TopLevelOnly: true}
		result, err := SelectMetadata(t.Context(), measured, "sessions", q, ListOptions{Cache: cache})
		if err != nil {
			t.Fatal(err)
		}
		expected, err := SelectMetadata(t.Context(), legacy, "sessions", q, ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result.Sessions, expected.Sessions) || result.Hidden != expected.Hidden || result.TotalMatched != expected.TotalMatched {
			t.Fatalf("oracle mismatch got %+v expected %+v", result, expected)
		}
		for _, row := range result.Sessions {
			key := row.Harness.Name + "/" + row.SessionID
			if result.Children[key] != expected.Children[key] {
				t.Fatal("child count differs")
			}
		}
		if metrics := measured.Metrics(); metrics.Lists != 0 {
			t.Fatalf("canonical LIST %+v", metrics)
		}
	}
	measured.Reset()
	if _, err = SelectMetadata(t.Context(), measured, "sessions", MetadataQuery{Filter: Filter{From: baseTime.Add(1000 * time.Hour)}, Limit: 10}, ListOptions{Cache: cache}); err != nil {
		t.Fatal(err)
	}
	if metrics := measured.Metrics(); metrics.Lists != 0 || metrics.Gets > 10 {
		t.Fatalf("future bounded query %+v", metrics)
	}
}

func TestRemoteSummaryDeltaDeletesAndUnknownRootRebuilds(t *testing.T) {
	remote, _ := remoteReaderFixture(t, 40)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err = c.RefreshRemote(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := c.Query(t.Context(), CatalogQuery{})
	if err != nil || before.Total != 40 {
		t.Fatal(before.Total, err)
	}
	key := "sessions/claude/session-0001/metadata.json"
	if err = remote.DeleteSession(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if err = c.RefreshRemote(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := c.Query(t.Context(), CatalogQuery{})
	if err != nil || after.Total != 39 {
		t.Fatal(after.Total, err)
	}
	for _, row := range after.Rows {
		if row.Key == key {
			t.Fatal("deleted summary resurrected")
		}
	}
	if _, err = c.db.ExecContext(t.Context(), "UPDATE remote_root SET root=?", []byte(`{"Key":"catalog-v4/nodes/missing.json","SHA256":"`+strings.Repeat("0", 64)+`"}`)); err != nil {
		t.Fatal(err)
	}
	if err = c.RefreshRemote(t.Context()); err != nil {
		t.Fatal("unknown root rebuild", err)
	}
	after, err = c.Query(t.Context(), CatalogQuery{})
	if err != nil || after.Total != 39 {
		t.Fatal(after.Total, err)
	}
}

func TestRemoteRefreshRepairsTupleDamageAndMissingRows(t *testing.T) {
	remote, _ := remoteReaderFixture(t, 40)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := OpenSessionCatalog(t.Context(), cache, remote, ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err = c.RefreshRemote(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"UPDATE sessions SET search='damaged' WHERE key=(SELECT min(key) FROM sessions)",
		"UPDATE sessions SET summary_hash='' WHERE key=(SELECT min(key) FROM sessions)",
		"UPDATE sessions SET key='sessions/claude/missing/metadata.json' WHERE key=(SELECT min(key) FROM sessions)",
		"DELETE FROM sessions WHERE key=(SELECT min(key) FROM sessions)",
	} {
		if _, err = c.db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
		if err = c.RefreshRemote(t.Context()); err != nil {
			t.Fatal("repair", statement, err)
		}
		page, err := c.Query(t.Context(), CatalogQuery{})
		if err != nil || page.Total != 40 {
			t.Fatal("repaired universe", statement, page.Total, err)
		}
		for _, row := range page.Rows {
			if row.Key == "sessions/claude/missing/metadata.json" {
				t.Fatal("unverified row survived")
			}
		}
	}
}

func fixtureRemoteParent(i int) string {
	if i%4 == 0 {
		return "session-0001"
	}
	return ""
}

func fixtureRemoteReplay(i int) *archive.Replay {
	if i%7 == 0 {
		return &archive.Replay{}
	}
	return nil
}

func TestCatalogCountIntRejectsOverflow(t *testing.T) {
	if _, err := catalogCountInt(^uint64(0)); err == nil {
		t.Fatal("count wrapped local integer")
	}
	if got, err := catalogCountInt(50); err != nil || got != 50 {
		t.Fatal(got, err)
	}
}

func TestRemoteAuthenticatedCountOverflowRefusesSelection(t *testing.T) {
	remote, _ := remoteReaderFixture(t, 2)
	head, etag, err := remote.Writer.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	maximum := uint64(^uint(0) >> 1)
	// Hash-valid parent aggregate received from a provider may exceed a local int.
	// Count must reject it before selected metadata or partial output is exposed.
	raw, err := json.Marshal(map[string]any{"Count": maximum + 2, "children": []map[string]any{
		{"Max": "0", "Ref": head.Capture, "Count": uint64(1)},
		{"Max": "9", "Ref": head.Capture, "Count": maximum + 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	head.Capture, err = remote.Writer.PutImmutable(t.Context(), catalog.KindNodes, raw)
	if err != nil {
		t.Fatal(err)
	}
	if head.Epoch != head.PublicationEpoch || head.GCLease != "" {
		t.Fatal("fixture is not a fresh publication state")
	}
	// Normal writer CAS also leaves this empty; the provider's new exact
	// body/version response supplies its publication witness when observed.
	head.PublicationWitness = catalog.HeadWitness{}
	head.CommittedAt = time.Time{}
	encoded, err := json.Marshal(head)
	if err != nil {
		t.Fatal(err)
	}
	provider := remote.ObjectStore.(storage.ConditionalPutter)
	nextETag, err := provider.PutConditional(t.Context(), catalog.HeadKey, encoded, storage.PutCondition{MatchETag: etag})
	if err != nil {
		t.Fatal(err)
	}
	observed, observedETag, err := remote.Writer.Head(t.Context())
	if err != nil || observedETag != nextETag || nextETag == etag || observed.PublicationWitness.ETag != nextETag || observed.Capture != head.Capture {
		t.Fatal("fresh provider publication was not authenticated", err)
	}
	snapshot, err := catalog.OpenSnapshot(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := selectBoundedCatalog(t.Context(), snapshot, MetadataQuery{Order: CaptureOrder, Limit: 50, Filter: Filter{Replays: ReplaysIncluded}}, ListOptions{})
	if err == nil || !strings.Contains(err.Error(), "integer capacity") || len(result.Sessions) != 0 {
		t.Fatal("oversized authenticated count exposed selection", err)
	}
}
