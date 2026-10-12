package reader

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRemoteNativeChildBoundsFallbackAndIndependentDateSeed(t *testing.T) {
	remote, legacy, cutoff := rootRemoteFixture(t, 16)
	writer, err := catalog.New(remote)
	if err != nil {
		t.Fatal(err)
	}
	// The child loses its conversation link but remains a positive child and
	// its own current date seed. No missing parent can be inferred from that fact.
	for _, id := range []string{"session-0000", "session-0001"} {
		key, err := archive.MetadataObjectKey("claude", id)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := legacy.Get(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		var m archive.Metadata
		if err = json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		m.Replay = nil
		if id == "session-0001" {
			m.NativeChild = true
			m.ParentSessionID = ""
			m.CapturedAt = cutoff
		}
		raw, err = json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err = legacy.Put(t.Context(), key, raw); err != nil {
			t.Fatal(err)
		}
		ref, err := writer.PutImmutable(t.Context(), catalog.KindMetadata, raw)
		if err != nil {
			t.Fatal(err)
		}
		_, revision, err := writer.Find(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = writer.Commit(t.Context(), catalog.CatalogMutation{ID: "native-window/" + id, SessionKey: key, ExpectedRevision: revision, Next: &catalog.CatalogEntry{Metadata: ref, Summary: m}}); err != nil {
			t.Fatal(err)
		}
	}
	measured := storagetest.NewMeasuredStore(remote, 0)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	queries := []MetadataQuery{
		{Filter: Filter{From: cutoff, Replays: ReplaysHidden}, TopLevelOnly: true, Limit: 50},
		{Filter: Filter{Harness: "claude", From: cutoff, Replays: ReplaysHidden}, TopLevelOnly: true, Limit: 50},
		{Filter: Filter{From: cutoff, Replays: ReplaysHidden}, IncludeRootChildren: true},
	}
	for _, warm := range []bool{false, true} {
		for _, query := range queries {
			got, err := SelectMetadata(t.Context(), measured, "sessions", query, ListOptions{Cache: cache})
			if err != nil {
				t.Fatal(err)
			}
			want, err := SelectMetadata(t.Context(), legacy, "sessions", query, ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Sessions, want.Sessions) || got.Hidden != want.Hidden || got.TotalMatched != want.TotalMatched || !reflect.DeepEqual(got.Children, want.Children) {
				t.Fatalf("warm=%v query=%+v native child oracle differs: got=%+v want=%+v", warm, query, got, want)
			}
			if query.IncludeRootChildren {
				found := false
				for _, m := range got.Sessions {
					if m.SessionID == "session-0001" {
						found = m.NativeChild && m.ParentSessionID == ""
					}
				}
				if !found {
					t.Fatal("unresolved child lost its independent date seed")
				}
			}
		}
	}
	if measured.Metrics().Lists != 0 {
		t.Fatal("native selection used canonical LIST")
	}
}
