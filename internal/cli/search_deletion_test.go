package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestContentSearchNamedRefreshRemovesOnlyConfirmedAbsentIdentity(t *testing.T) {
	for _, mode := range []string{"legacy", "catalog"} {
		cases := []string{"deleted", "other_harness", "invalid_metadata"}
		if mode == "legacy" {
			cases = append(cases, "transient")
		} else {
			cases = append(cases, "missing_metadata_body")
		}
		for _, failure := range cases {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				env, objects, id := publishedFixture(t)
				var remote storage.ObjectStore = objects
				if mode == "catalog" {
					remote = privateCatalogFromLegacy(t, objects)
				}
				env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
				code, seeded, diag := searchJSONRun(t, env, "visible", "--all-projects", "--refresh", "--session", id, "--harness", "codex")
				if code != 0 || len(seeded.Results) != 1 {
					t.Fatalf("seed: code=%d results=%d diagnostics=%s", code, len(seeded.Results), diag)
				}
				key, err := archive.MetadataObjectKey("codex", id)
				must(t, err)
				harness := "codex"
				if failure == "other_harness" {
					// The same session ID is still indexed under its real harness.
					harness = "claude"
				} else if authority, ok := remote.(*catalog.Store); ok {
					entry, revision, findErr := authority.Writer.Find(t.Context(), key)
					must(t, findErr)
					if entry == nil {
						t.Fatal("fixture lacks authoritative live identity")
					}
					switch failure {
					case "deleted":
						_, err = authority.Writer.Commit(t.Context(), catalog.CatalogMutation{ID: "synthetic-search-delete", SessionKey: key, ExpectedRevision: revision, Next: nil})
					case "missing_metadata_body":
						// A missing immutable body is not proof that the live identity is gone.
						err = authority.ObjectStore.Delete(t.Context(), entry.Metadata.Key)
					case "invalid_metadata":
						err = authority.ObjectStore.Put(t.Context(), entry.Metadata.Key, []byte("broken"))
					}
					must(t, err)
				} else {
					switch failure {
					case "deleted":
						err = objects.Delete(t.Context(), key)
					case "invalid_metadata":
						err = objects.Put(t.Context(), key, []byte("broken"))
					case "transient":
						remote = &searchMetadataReadFailure{MemoryStore: objects, key: key}
					}
					must(t, err)
				}
				code, refreshed, diag := searchJSONRun(t, env, "visible", "--all-projects", "--refresh", "--session", id, "--harness", harness)
				if code != 1 || refreshed.Error == nil || refreshed.Refresh.Completed {
					t.Fatalf("refresh: code=%d document=%+v diagnostics=%s", code, refreshed, diag)
				}
				absent := failure == "deleted" || failure == "other_harness"
				if (refreshed.Error.Code == "source_deleted") != absent {
					t.Fatalf("absence classification: %+v", refreshed.Error)
				}
				wantHits := 1
				if failure == "deleted" {
					wantHits = 0
				}
				combinedHits := wantHits
				if failure == "other_harness" {
					// The same flag scopes the combined query to the absent harness.
					combinedHits = 0
				}
				if len(refreshed.Results) != combinedHits {
					t.Fatalf("combined refresh returned %d hits, want %d", len(refreshed.Results), combinedHits)
				}
				code, cached, diag := searchJSONRun(t, env, "visible", "--all-projects")
				if code != 0 || len(cached.Results) != wantHits {
					t.Fatalf("cached query: code=%d results=%d want=%d diagnostics=%s", code, len(cached.Results), wantHits, diag)
				}
				if failure == "deleted" {
					if cached.Generation <= seeded.Generation || cached.Coverage.Known != 0 || cached.Coverage.Indexed != 0 {
						t.Fatalf("deletion left work or cursor authority: %+v", cached)
					}
				} else if len(cached.Results) == 1 && (cached.Results[0].Harness != "codex" || cached.Results[0].Session != id) {
					t.Fatalf("qualified identity changed: %+v", cached.Results[0])
				}
				if failure != "deleted" && cached.Generation != seeded.Generation {
					t.Fatalf("unchanged indexed identity invalidated cursors: before=%d after=%d", seeded.Generation, cached.Generation)
				}
			})
		}
	}
}

type searchMetadataReadFailure struct {
	*storagetest.MemoryStore
	key string
}

func (s *searchMetadataReadFailure) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	if key == s.key {
		return nil, "", errors.New("synthetic temporary metadata failure")
	}
	return s.MemoryStore.GetLimitedVersioned(ctx, key, limit)
}
