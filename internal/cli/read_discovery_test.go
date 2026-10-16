package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type blockedReadDiscoveryStore struct {
	storage.ObjectStore
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
	once     sync.Once
	lists    atomic.Int32
}

func (s *blockedReadDiscoveryStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	s.lists.Add(1)
	s.once.Do(func() { close(s.started) })
	defer close(s.finished)
	select {
	case <-s.release:
		return s.ObjectStore.List(ctx, prefix)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestReadCommandsOverlapScopeWithFreshHeaders(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"list", "list-no-catalog", "show", "show-query", "show-query-no-catalog", "handoff"} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			env, mem, id := publishedFixture(t)
			store := &blockedReadDiscoveryStore{ObjectStore: mem, started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
			scopeDir := t.TempDir()
			env.WorkingDir = func() (string, error) { return scopeDir, nil }
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
			env.repoKey = func(string) string {
				select {
				case <-store.started:
				case <-time.After(5 * time.Second):
					t.Error("scope lookup ran before fresh headers started")
				}
				close(store.release)
				return ""
			}
			if command == "show-query-no-catalog" || command == "list-no-catalog" {
				if listCache(env, false) == nil {
					t.Fatal("cache unavailable")
				}
				home, err := env.Home()
				must(t, err)
				must(t, os.WriteFile(filepath.Join(home, "cache", "catalog"), []byte("unavailable disposable catalog"), 0600))
			}
			args := []string{command}
			if command == "list-no-catalog" {
				args = []string{"list"}
			}
			if command == "show-query" || command == "show-query-no-catalog" {
				args = []string{"show", id[:8], "--json"}
			}
			stdin := strings.NewReader("q\n")
			var stdout, stderr bytes.Buffer
			env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&stdout) }
			// A line-mode picker keeps this test independent of the host's terminal.
			env.openKeys = func(io.Reader) (keyTerminal, bool) { return nil, false }
			if code := Run(args, stdin, &stdout, &stderr, env); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if n := store.lists.Load(); n != 1 {
				t.Fatalf("fresh discoveries=%d, want one", n)
			}
		})
	}
}

func TestReadDiscoveryCancelsAndJoinsOnLocalError(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	home, err := env.Home()
	if err != nil {
		t.Fatal(err)
	}
	store := &blockedReadDiscoveryStore{ObjectStore: mem, started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	calls := 0
	env.Home = func() (string, error) {
		calls++
		if calls == 1 {
			return home, nil
		} // Opening the disposable cache.
		select {
		case <-store.started:
		case <-time.After(5 * time.Second):
			t.Error("headers did not start")
		}
		return "", errors.New("local scope unavailable")
	}
	var stderr bytes.Buffer
	_, code := resolveShowQuery(t.Context(), store, env, nil, io.Discard, &stderr, "", id[:8], nil, true, true)
	if code != 1 || !strings.Contains(stderr.String(), "resolve home: local scope unavailable") || strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	select {
	case <-store.finished:
	default:
		t.Fatal("discovery worker outlived the command")
	}
}

func TestHandoffPickerReusesItsOpenStoreAfterSelection(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	opens := 0
	open := f.env.OpenStore
	f.env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) { opens++; return open(cfg) }
	// Archive-only means the resolver must read the chosen source from the store.
	_, stderr, code := runPicker(t, f.env, "1\n", "--source", "archive")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	if opens != 1 {
		t.Fatalf("store opens=%d, want picker open only", opens)
	}
}

func TestHandoffActivityPreparationPreservesScopeAndMissingObservations(t *testing.T) {
	t.Parallel()
	for _, narrowed := range []bool{false, true} {
		name := "all-projects"
		if narrowed {
			name = "archive-outside-local-scope"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newHandoffFixture(t, true)
			key, err := archive.MetadataObjectKey("codex", f.id)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := f.mem.Get(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			var metadata archive.Metadata
			if err = json.Unmarshal(raw, &metadata); err != nil {
				t.Fatal(err)
			}
			metadata.RepoKey = "github.com/example/elsewhere"
			raw, err = json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			must(t, f.mem.Put(t.Context(), key, raw))
			if narrowed {
				// Keep the initial scope nonempty so the browser does not deliberately
				// broaden to all projects after finding no rows in this repository.
				pickerFixture{handoffFixture: f}.addArchivedWith(t, "scoped-only", "Other scoped task", "current", func(m *archive.Metadata) { m.RepoKey = "github.com/example/current" })
				f.env.WorkingDir = func() (string, error) { return f.project, nil }
				f.env.repoKey = func(string) string { return "github.com/example/current" }
			}
			calls := 0
			bindings := []builtin.Integration{}
			for _, descriptor := range productionAgents.Catalog().All() {
				binding, ok := productionAgents.Lookup(string(descriptor.ID))
				if !ok {
					t.Fatal(descriptor.ID)
				}
				binding.Descriptor = agentmeta.Descriptor{ID: descriptor.ID}
				if descriptor.ID == agentmeta.Codex {
					// A missing native observation is a completed read, not a cache miss.
					binding.Sources = activityFailureSource{SourceProvider: binding.Sources, calls: &calls}
				}
				bindings = append(bindings, binding)
			}
			registry, err := builtin.New(productionAgents.Catalog(), bindings)
			if err != nil {
				t.Fatal(err)
			}
			f.env.Agents = registry
			var args []string
			if !narrowed {
				args = []string{"--all-projects"}
			}
			_, stderr, code := runPicker(t, f.env, "q\n", args...)
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr)
			}
			want := 1
			if narrowed {
				want = 0
			}
			if calls != want {
				t.Fatalf("native activity calls=%d want=%d", calls, want)
			}
		})
	}
}

func TestHandoffBrowserScopeRebuildRefreshesPreparedActivity(t *testing.T) {
	t.Parallel()
	for _, missing := range []bool{false, true} {
		name := "changed"
		if missing {
			name = "newly-available"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newHandoffFixture(t, true)
			regs, err := state.OpenReadOnly(f.home).LoadRegistrations()
			must(t, err)
			archived, err := reader.ListMetadataWithOptions(t.Context(), f.mem, archiveSessionsPrefix, reader.Filter{}, reader.ListOptions{})
			must(t, err)
			path := filepath.Join(f.project, "codex.jsonl")
			raw, err := os.ReadFile(path)
			must(t, err)
			if missing {
				must(t, os.Remove(path))
			}
			activity := pickerActivities(t.Context(), f.env, regs)
			picker := handoffPicker{ctx: t.Context(), env: f.env, home: f.home, source: "auto", archiveRead: true, activity: activity, activityRead: map[pickerIdentity]bool{pickerID("codex", f.id): true}, localLabels: map[localLabelKey]localLabelResult{}}
			choices := pagedHandoffChoices(sessionScope{Label: "current", Dir: f.project, All: true}, listFormatOptions{Now: f.env.now()}, picker, regs, archived)
			initial := choices.shown()
			if len(initial.rows) != 1 {
				t.Fatalf("initial rows=%d", len(initial.rows))
			}
			initialActive := initial.rows[0].Active
			if missing {
				must(t, os.WriteFile(path, raw, 0600))
			}
			newer := f.env.now().Add(time.Hour)
			must(t, os.Chtimes(path, newer, newer))
			choices.toggle()
			rebuilt := choices.shown()
			if len(rebuilt.rows) != 1 || !rebuilt.rows[0].Active.Equal(newer) || rebuilt.rows[0].Active.Equal(initialActive) {
				t.Fatalf("scope rebuild retained prepared activity: before=%s rows=%+v want=%s", initialActive, rebuilt.rows, newer)
			}
		})
	}
}
