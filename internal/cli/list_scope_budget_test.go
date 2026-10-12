package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type scopeBudgetStore struct {
	*storagetest.MemoryStore
	gets       atomic.Int64
	sourceGets atomic.Int64
}

func (s *scopeBudgetStore) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	s.gets.Add(1)
	if !strings.HasSuffix(key, "/metadata.json") {
		s.sourceGets.Add(1)
	}
	return s.MemoryStore.GetVersioned(ctx, key)
}

func (s *scopeBudgetStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets.Add(1)
	if !strings.HasSuffix(key, "/metadata.json") {
		s.sourceGets.Add(1)
	}
	return s.MemoryStore.Get(ctx, key)
}

// Running inside a repository is the actual default scope. It includes other
// clones by RepoKey and legacy local metadata by ProjectID, before any body read.
func TestDefaultRepositoryScopeBodyBudgetAndEmptyFallback(t *testing.T) {
	t.Parallel()
	sizes := []int{10000, 20000}
	if testing.Short() {
		// Keep doubled archives beyond a provider page and two timestamp
		// cycles, including parent 1425 and far more than 50 scoped rows.
		sizes = []int{1600, 3200}
	}
	for _, size := range sizes {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			a := newScopedArchive(t)
			a.env.repoKey = func(string) string { return scopeKey }
			store := &scopeBudgetStore{MemoryStore: a.mem}
			a.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
			for i := range size {
				m := a.base
				m.SessionID = fmt.Sprintf("budget%08d", i)
				m.CapturedAt = m.CapturedAt.Add(time.Duration(i%713) * time.Second)
				m.RepoKey = otherScopeKey
				m.ProjectID = "other-project"
				switch i % 5 {
				case 0:
					m.RepoKey = ""
					m.ProjectID = archive.ProjectID(a.dir)
				case 1:
					m.RepoKey = scopeKey
					m.ProjectID = "another-clone"
				}
				if i%7 == 0 {
					m.ParentSessionID = "budget00001425"
				}
				data, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				if err = store.Put(ctx, key, data); err != nil {
					t.Fatal(err)
				}
				_, etag, err := store.MemoryStore.GetVersioned(ctx, key)
				if err != nil {
					t.Fatal(err)
				}
				r, err := listingindex.NewRevision(key, data, etag)
				if err != nil {
					t.Fatal(err)
				}
				if err = listingindex.PutRevision(ctx, store, r); err != nil {
					t.Fatal(err)
				}
			}
			oracle, err := reader.ListMetadataWithOptions(ctx, store, "sessions", reader.Filter{}, reader.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			scope, err := scopeFor(a.env, "", false)
			if err != nil {
				t.Fatal(err)
			}
			scoped := scope.filter(oracle)
			if len(scoped) <= 50 || len(scoped) == len(oracle) || childCounts(oracle)[childKey(a.base.Harness.Name, "budget00001425")] == 0 {
				t.Fatal("fixture needs omitted scoped rows, outside matches and children")
			}
			parentShown := false
			for _, m := range scoped[:50] {
				parentShown = parentShown || m.SessionID == "budget00001425"
			}
			if !parentShown {
				t.Fatal("fixture must show the parent whose child count is checked")
			}
			home, err := a.env.Home()
			if err != nil {
				t.Fatal(err)
			}
			for _, jsonOut := range []bool{true, false} {
				if err = os.RemoveAll(filepath.Join(home, "cache")); err != nil {
					t.Fatal(err)
				}
				for _, warm := range []bool{false, true} {
					store.gets.Store(0)
					store.sourceGets.Store(0)
					bodies, cached := 0, 0
					a.env.observeListBody = func(_ string, hit bool) {
						bodies++
						if hit {
							cached++
						}
					}
					args := []string{}
					if jsonOut {
						args = append(args, "--json")
					}
					out, stderr, code := a.runList(t, args...)
					if code != 0 {
						t.Fatalf("code=%d stderr=%s", code, stderr)
					}
					wantGets, wantCached := 50, 0
					if warm {
						wantGets, wantCached = 0, 50
					}
					if bodies != 50 || cached != wantCached || store.gets.Load() != int64(wantGets) || store.sourceGets.Load() != 0 {
						t.Fatalf("JSON=%v warm=%v bodies=%d cached=%d gets=%d source=%d stderr=%s", jsonOut, warm, bodies, cached, store.gets.Load(), store.sourceGets.Load(), stderr)
					}
					if jsonOut {
						var doc listDocument
						if err = json.Unmarshal([]byte(out), &doc); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(doc.Sessions, scoped[:50]) || doc.TotalMatched == nil || *doc.TotalMatched != len(scoped) || doc.Scope.OutsideMatches != len(oracle)-len(scoped) {
							t.Fatal("scoped JSON disagrees with exhaustive oracle")
						}
					} else {
						expected := append([]archive.Metadata(nil), oracle...)
						sortByActivity(expected)
						view := listViews{sessions: expected, listed: reader.RecentResult{TotalMatched: len(expected), Complete: true}, full: true, limit: 50}.view
						format := listFormatOptions{Now: a.env.now(), Projects: projectLabelsMust(t, a.env), GroupByProject: true, Children: childCounts(expected)}
						var expectedOut bytes.Buffer
						choices := listChoices(scope, format, false, expected, 50, view, "")
						if err := printListTable(&expectedOut, choices.shown()); err != nil {
							t.Fatal(err)
						}
						if out != expectedOut.String() {
							t.Fatal("text scope rows/child counts/footer disagree with exhaustive oracle")
						}

					}
				}
			}
			empty := filepath.Join(t.TempDir(), "empty-project")
			a.addProject(t, empty)
			a.env.WorkingDir = func() (string, error) { return empty, nil }
			a.env.repoKey = func(string) string { return "empty-origin" }
			for _, jsonOut := range []bool{true, false} {
				if err = os.RemoveAll(filepath.Join(home, "cache")); err != nil {
					t.Fatal(err)
				}
				for _, warm := range []bool{false, true} {
					store.gets.Store(0)
					store.sourceGets.Store(0)
					bodies := 0
					a.env.observeListBody = func(string, bool) { bodies++ }
					args := []string{}
					if jsonOut {
						args = append(args, "--json")
					}
					out, stderr, code := a.runList(t, args...)
					wantGets := int64(50)
					if warm {
						wantGets = 0
					}
					if code != 0 || bodies != 50 || store.gets.Load() != wantGets || store.sourceGets.Load() != 0 {
						t.Fatalf("fallback JSON=%v warm=%v code=%d bodies=%d gets=%d source=%d stderr=%s", jsonOut, warm, code, bodies, store.gets.Load(), store.sourceGets.Load(), stderr)
					}
					if jsonOut {
						var doc listDocument
						if err = json.Unmarshal([]byte(out), &doc); err != nil {
							t.Fatal(err)
						}
						if doc.Scope == nil || !doc.Scope.FellBack || !reflect.DeepEqual(doc.Sessions, oracle[:50]) {
							t.Fatal("empty JSON fallback changed")
						}
					} else if !strings.Contains(out, "Nothing in empty-project · showing all projects") {
						t.Fatalf("text fallback missing: %s", out)
					}
				}
			}
		})
	}
}

func projectLabelsMust(t *testing.T, env Env) map[string]string {
	t.Helper()
	home, err := env.Home()
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	return projectLabels(cfg)
}
