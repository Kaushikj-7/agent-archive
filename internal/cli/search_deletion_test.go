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

type searchMetadataFailure string

const (
	searchMetadataDeleted      searchMetadataFailure = "deleted"
	searchMetadataOtherHarness searchMetadataFailure = "other_harness"
	searchMetadataInvalid      searchMetadataFailure = "invalid_metadata"
	searchMetadataTransient    searchMetadataFailure = "transient"
	searchMetadataBodyMissing  searchMetadataFailure = "missing_metadata_body"
)

func TestContentSearchNamedRefreshRemovesOnlyConfirmedAbsentIdentity(t *testing.T) {
	for _, mode := range []string{"legacy", "catalog"} {
		cases := []searchMetadataFailure{searchMetadataDeleted, searchMetadataOtherHarness, searchMetadataInvalid}
		if mode == "legacy" {
			cases = append(cases, searchMetadataTransient)
		} else {
			cases = append(cases, searchMetadataBodyMissing)
		}
		for _, failure := range cases {
			t.Run(mode+"/"+string(failure), func(t *testing.T) {
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
				if failure == searchMetadataOtherHarness {
					// The same session ID is still indexed under its real harness.
					harness = "claude"
				} else if authority, ok := remote.(*catalog.Store); ok {
					entry, revision, findErr := authority.Writer.Find(t.Context(), key)
					must(t, findErr)
					if entry == nil {
						t.Fatal("fixture lacks authoritative live identity")
					}
					switch failure {
					case searchMetadataDeleted:
						_, err = authority.Writer.Commit(t.Context(), catalog.CatalogMutation{ID: "synthetic-search-delete", SessionKey: key, ExpectedRevision: revision, Next: nil})
					case searchMetadataBodyMissing:
						// A missing immutable body is not proof that the live identity is gone.
						err = authority.ObjectStore.Delete(t.Context(), entry.Metadata.Key)
					case searchMetadataInvalid:
						err = authority.ObjectStore.Put(t.Context(), entry.Metadata.Key, []byte("broken"))
					}
					must(t, err)
				} else {
					switch failure {
					case searchMetadataDeleted:
						err = objects.Delete(t.Context(), key)
					case searchMetadataInvalid:
						err = objects.Put(t.Context(), key, []byte("broken"))
					case searchMetadataTransient:
						remote = &searchMetadataReadFailure{MemoryStore: objects, key: key}
					}
					must(t, err)
				}
				code, refreshed, diag := searchJSONRun(t, env, "visible", "--all-projects", "--refresh", "--session", id, "--harness", harness)
				if code != 1 || refreshed.Error == nil || refreshed.Refresh.Completed {
					t.Fatalf("refresh: code=%d document=%+v diagnostics=%s", code, refreshed, diag)
				}
				absent := failure == searchMetadataDeleted || failure == searchMetadataOtherHarness
				if (refreshed.Error.Code == "source_deleted") != absent {
					t.Fatalf("absence classification: %+v", refreshed.Error)
				}
				wantHits := 1
				if failure == searchMetadataDeleted {
					wantHits = 0
				}
				combinedHits := wantHits
				if failure == searchMetadataOtherHarness {
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
				if failure == searchMetadataDeleted {
					if cached.Generation <= seeded.Generation || cached.Coverage.Known != 0 || cached.Coverage.Indexed != 0 {
						t.Fatalf("deletion left work or cursor authority: %+v", cached)
					}
				} else if len(cached.Results) == 1 && (cached.Results[0].Harness != "codex" || cached.Results[0].Session != id) {
					t.Fatalf("qualified identity changed: %+v", cached.Results[0])
				}
				if failure != searchMetadataDeleted && cached.Generation != seeded.Generation {
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
