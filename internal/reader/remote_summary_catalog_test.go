package reader

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRemoteSummaryCatalogReusesDeltaAndRepairsLocalRows(t *testing.T) {
	t.Parallel()
	remote, _ := remoteReaderFixture(t, 80)
	measured := storagetest.NewMeasuredStore(remote, 0)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opts := ListOptions{Cache: cache}
	want, err := CatalogSummaries(t.Context(), remote, Filter{Replays: ReplaysIncluded})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"cold", "unchanged", "damaged", "deleted"} {
		if phase == "damaged" {
			c, err := OpenSessionCatalog(t.Context(), cache, measured, opts)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.db.ExecContext(t.Context(), "UPDATE sessions SET search='damaged' WHERE key=(SELECT min(key) FROM sessions)")
			_ = c.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		if phase == "deleted" {
			if err := remote.DeleteSession(t.Context(), "sessions/claude/session-0001/metadata.json"); err != nil {
				t.Fatal(err)
			}
			want, err = CatalogSummaries(t.Context(), remote, Filter{Replays: ReplaysIncluded})
			if err != nil {
				t.Fatal(err)
			}
		}
		measured.Reset()
		got, err := CatalogSummariesWithOptions(t.Context(), measured, Filter{Replays: ReplaysIncluded}, opts)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s summaries differ from exhaustive oracle: %v", phase, err)
		}
		if metrics := measured.Metrics(); metrics.Lists != 0 {
			t.Fatalf("%s performed canonical LIST: %+v", phase, metrics)
		} else {
			t.Logf("%s remote costs: %+v (local refresh still scans all rows)", phase, metrics)
		}
	}
}

func TestRemoteCompleteSelectionMatchesExhaustiveFallback(t *testing.T) {
	t.Parallel()
	remote, legacy := remoteReaderFixture(t, 32)
	rewriteRemoteFixtureMetadata(t, remote, legacy, "sessions/claude/session-0002/metadata.json", func(m *archive.Metadata) {
		m.ProjectID = "alternate"
		m.NativeChild = true
		m.SkillsUsed = []archive.SkillUse{{Name: "selected"}}
	})
	measured := storagetest.NewMeasuredStore(remote, 0)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, topLevel := range []bool{false, true} {
		for _, order := range []QueryOrder{CaptureOrder, ActivityOrder} {
			for _, skill := range []string{"", "selected"} {
				for _, scope := range []string{"", "alternate", "missing"} {
					var scopeMatch func(archive.Metadata) bool
					if scope != "" {
						scopeMatch = func(m archive.Metadata) bool { return m.ProjectID == scope }
					}
					opts := ListOptions{ScopeMatch: scopeMatch}
					q := MetadataQuery{Filter: Filter{Harness: "claude", Replays: ReplaysIncluded, Skill: skill}, Order: order, TopLevelOnly: topLevel, Limit: 5}
					want, err := SelectMetadata(t.Context(), remote, "sessions", q, opts)
					if err != nil {
						t.Fatal(err)
					}
					legacyResult, err := SelectMetadata(t.Context(), legacy, "sessions", q, opts)
					if err != nil || !reflect.DeepEqual(want.Sessions, legacyResult.Sessions) {
						t.Fatal("uncached catalog selection differs from legacy identities/order", err)
					}
					// Outside historically counts eligible roots in catalog
					// selection and all sessions in the exhaustive legacy path.
					// Pin existing catalog counters; do not change that contract
					// as a side effect of reusing local summaries.
					if want.Outside != legacyResult.Outside {
						t.Logf("existing Outside count difference: topLevel=%v skill=%q scope=%q catalog=%d legacy=%d", topLevel, skill, scope, want.Outside, legacyResult.Outside)
					}
					opts.Cache = cache
					for range 2 {
						got, err := SelectMetadata(t.Context(), measured, "sessions", q, opts)
						if err != nil || !reflect.DeepEqual(got, want) {
							t.Fatalf("cached complete selection differs (topLevel=%v order=%v skill=%s scope=%s): got %+v want %+v error %v", topLevel, order, skill, scope, got, want, err)
						}
					}
				}
			}
		}
	}

	got, err := ListMetadataWithOptions(t.Context(), measured, "sessions/claude", Filter{Replays: ReplaysIncluded}, ListOptions{Cache: cache})
	want, wantErr := ListMetadataWithOptions(t.Context(), legacy, "sessions/claude", Filter{Replays: ReplaysIncluded}, ListOptions{})
	if err != nil || wantErr != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("complete metadata differs from oracle", err, wantErr)
	}
	if measured.Metrics().Lists != 0 {
		t.Fatal("complete fallback used canonical LIST")
	}
}

func TestRemoteSummaryCatalogUnavailableCacheUsesCompleteFallback(t *testing.T) {
	t.Parallel()
	remote, _ := remoteReaderFixture(t, 8)
	root := t.TempDir()
	cache, err := OpenMetadataCache(root)
	if err != nil {
		t.Fatal(err)
	}
	// An unavailable disposable index must not hide fresh catalog sessions.
	if err = os.WriteFile(filepath.Join(filepath.Dir(cache.dir), "catalog"), []byte("unavailable"), 0600); err != nil {
		t.Fatal(err)
	}
	want, err := CatalogSummaries(t.Context(), remote, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := CatalogSummariesWithOptions(t.Context(), remote, Filter{}, ListOptions{Cache: cache})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("unavailable cache lost complete fallback", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := CatalogSummariesWithOptions(ctx, remote, Filter{}, ListOptions{Cache: cache}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was hidden by fallback", err)
	}
}

func TestRemoteSummaryCatalogDoesNotHideAuthorityFailure(t *testing.T) {
	t.Parallel()
	remote, _ := remoteReaderFixture(t, 8)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = CatalogSummariesWithOptions(t.Context(), remote, Filter{}, ListOptions{Cache: cache}); err != nil {
		t.Fatal(err)
	}
	if err = remote.ObjectStore.Put(t.Context(), catalog.HeadKey, []byte("corrupt head")); err != nil {
		t.Fatal(err)
	}
	if got, err := CatalogSummariesWithOptions(t.Context(), remote, Filter{}, ListOptions{Cache: cache}); err == nil || len(got) != 0 {
		t.Fatal("cached summaries hid remote authority failure", len(got), err)
	}
}

func rewriteRemoteFixtureMetadata(t *testing.T, remote *catalog.Store, legacy *storagetest.MemoryStore, key string, change func(*archive.Metadata)) {
	t.Helper()
	raw, err := legacy.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	change(&m)
	raw, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	_, revision, err := remote.Writer.Find(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := remote.Writer.PutImmutable(t.Context(), catalog.KindMetadata, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = remote.Writer.Commit(t.Context(), catalog.CatalogMutation{ID: "rewrite/" + key, SessionKey: key, ExpectedRevision: revision, Next: &catalog.CatalogEntry{Metadata: ref, Summary: m}}); err != nil {
		t.Fatal(err)
	}
	if err = legacy.Put(t.Context(), key, raw); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteSummaryModelFilterRetainsObservationOnlyEvidence(t *testing.T) {
	t.Parallel()
	remote, legacy := remoteReaderFixture(t, 8)
	rewriteRemoteFixtureMetadata(t, remote, legacy, "sessions/claude/session-0002/metadata.json", func(m *archive.Metadata) {
		var observations archive.ObservationAccumulator
		if err := observations.Add("observed-model", 1, 2); err != nil {
			t.Fatal(err)
		}
		m.TokenObservations = observations.Summary()
	})
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Populate the local projection first; it deliberately omits observations.
	if _, err := CatalogSummariesWithOptions(t.Context(), remote, Filter{}, ListOptions{Cache: cache}); err != nil {
		t.Fatal(err)
	}
	filter := Filter{Model: "observed-model", Replays: ReplaysIncluded}
	got, err := CatalogSummariesWithOptions(t.Context(), remote, filter, ListOptions{Cache: cache})
	if err != nil || len(got) != 1 || got[0].SessionID != "session-0002" {
		t.Fatal("projection hid observation-only model match", got, err)
	}
	q := MetadataQuery{Filter: filter, Limit: 5}
	actual, err := SelectMetadata(t.Context(), remote, "sessions", q, ListOptions{Cache: cache})
	expected, expectedErr := SelectMetadata(t.Context(), legacy, "sessions", q, ListOptions{})
	if err != nil || expectedErr != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatal("model selection differs from exhaustive oracle", actual, expected, err, expectedErr)
	}
	q.IncludeRootChildren = true
	actual, err = SelectMetadata(t.Context(), remote, "sessions", q, ListOptions{Cache: cache})
	expected, expectedErr = SelectMetadata(t.Context(), remote, "sessions", q, ListOptions{})
	if err != nil || expectedErr != nil || len(expected.Sessions) != 1 || !reflect.DeepEqual(actual, expected) {
		t.Fatal("root-child model selection differs from uncached oracle", actual, expected, err, expectedErr)
	}
}
