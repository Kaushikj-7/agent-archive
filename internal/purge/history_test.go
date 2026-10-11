package purge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func historyFixture(t *testing.T) (*storagetest.MemoryStore, archive.Metadata, []string, time.Time) {
	t.Helper()
	store := storagetest.NewMemoryStore()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	m := archive.Metadata{SchemaVersion: archive.HistoryMetadataSchemaVersion, SessionID: session, NativeSessionID: "11111111-1111-4111-8111-111111111111", ProjectID: "project", Harness: archive.Harness{Name: "codex"}, CapturedAt: now, FilterVersion: "10", History: &archive.RevisionHistory{CurrentRevision: "11111111-1111-4111-8111-111111111111"}}
	var keys []string
	for index, hex := range []string{"a", "b", "c", "d"} {
		key := "sessions/codex/" + session + "/source." + strings.Repeat(hex, 64) + ".jsonl.gz"
		keys = append(keys, key)
		data := compressedHeader(t, "9")
		if err := store.Put(context.Background(), key, data); err != nil {
			t.Fatal(err)
		}
		ref := archive.SourceReference{Key: key, SHA256: strings.Repeat(hex, 64), CompressedBytes: len(data)}
		switch index {
		case 0:
			m.SourceBundle = ref
		case 1, 2:
			id := "22222222-2222-4222-8222-222222222222"
			if index == 2 {
				id = "33333333-3333-4333-8333-333333333333"
			}
			m.History.Preserved = append(m.History.Preserved, archive.RevisionReference{RevisionID: id, CapturedAt: now, Source: ref, FilterVersion: "9"})
		}
	}
	saveHistory(t, store, m)
	return store, m, keys, now
}

func saveHistory(t *testing.T, store *storagetest.MemoryStore, m archive.Metadata) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "sessions/codex/"+session+"/metadata.json", data); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeProtectsCompleteHistoryInBothModes(t *testing.T) {
	for _, mode := range []Mode{ModeUnreferenced, ModeOldFilter} {
		t.Run(string(mode), func(t *testing.T) {
			store, m, keys, now := historyFixture(t)
			// A legacy reference without filter provenance is inspected, not deleted.
			m.History.Preserved[0].FilterVersion = ""
			saveHistory(t, store, m)
			plan, err := Inventory(context.Background(), store, "destination", "bucket", "", mode, "10", now)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Candidates) != 1 || plan.Candidates[0].Key != keys[3] {
				t.Fatalf("candidates: %+v", plan.Candidates)
			}
			if mode == ModeOldFilter && len(plan.CurrentOldSessions) != 2 {
				t.Fatalf("old live references: %+v", plan.CurrentOldSessions)
			}
			report := Report{PlanDigest: plan.Digest, Remaining: []string{keys[3]}}
			if err := Apply(context.Background(), store, plan, &report, func(Report) error { return nil }); err != nil {
				t.Fatal(err)
			}
			for _, key := range keys[:3] {
				if _, err := store.Get(context.Background(), key); err != nil {
					t.Fatalf("live source deleted: %s", key)
				}
			}
			if _, err := store.Get(context.Background(), keys[3]); err == nil {
				t.Fatal("unreferenced source survived")
			}
		})
	}
}

func TestApplyProtectsNewPreservedReference(t *testing.T) {
	store, m, keys, now := historyFixture(t)
	plan, err := Inventory(context.Background(), store, "destination", "bucket", "", ModeUnreferenced, "", now)
	if err != nil {
		t.Fatal(err)
	}
	m.History.Preserved = append(m.History.Preserved, archive.RevisionReference{RevisionID: "44444444-4444-4444-8444-444444444444", CapturedAt: now, Source: archive.SourceReference{Key: keys[3], SHA256: strings.Repeat("d", 64), CompressedBytes: len(compressedHeader(t, "9"))}})
	saveHistory(t, store, m)
	report := Report{PlanDigest: plan.Digest, Remaining: []string{keys[3]}}
	saves := 0
	if err := Apply(context.Background(), store, plan, &report, func(Report) error { saves++; return nil }); err == nil {
		t.Fatal("deleted newly preserved source")
	}
	if len(report.Remaining) != 1 || saves != 0 {
		t.Fatalf("report advanced: %+v", report)
	}
	if _, err := store.Get(context.Background(), keys[3]); err != nil {
		t.Fatal(err)
	}
}

type invalidHistoryKind string

const (
	missingHistorySource   invalidHistoryKind = "missing"
	foreignHistorySource   invalidHistoryKind = "cross-session"
	duplicateHistorySource invalidHistoryKind = "duplicate"
	invalidHistoryRevision invalidHistoryKind = "invalid revision"
)

func TestHistoryAmbiguityBlocksInventoryAndApply(t *testing.T) {
	for _, kind := range []invalidHistoryKind{missingHistorySource, foreignHistorySource, duplicateHistorySource, invalidHistoryRevision} {
		t.Run(string(kind), func(t *testing.T) {
			store, m, keys, now := historyFixture(t)
			plan, err := Inventory(context.Background(), store, "destination", "bucket", "", ModeUnreferenced, "", now)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case missingHistorySource:
				if err := store.Delete(context.Background(), keys[1]); err != nil {
					t.Fatal(err)
				}
			case foreignHistorySource:
				m.History.Preserved[0].Source.Key = strings.Replace(keys[1], session, strings.Repeat("e", 32), 1)
			case duplicateHistorySource:
				m.History.Preserved[1].Source = m.History.Preserved[0].Source
			case invalidHistoryRevision:
				m.History.Preserved[0].RevisionID = "invalid"
			}
			saveHistory(t, store, m)
			if _, err := Inventory(context.Background(), store, "destination", "bucket", "", ModeUnreferenced, "", now); err == nil {
				t.Fatal("invalid history planned")
			}
			report := Report{PlanDigest: plan.Digest, Remaining: []string{keys[3]}}
			if err := Apply(context.Background(), store, plan, &report, func(Report) error { return nil }); err == nil {
				t.Fatal("invalid history deleted source")
			}
			if _, err := store.Get(context.Background(), keys[3]); err != nil {
				t.Fatal(err)
			}
		})
	}
}
