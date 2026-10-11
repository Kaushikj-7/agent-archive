package backfill

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

// fsCalls counts an environment's real store reads by kind.
type fsCalls struct {
	readDirs []string
	lstats   int
	opens    []string
}

// countCalls makes env's store reads count into calls while *on is set.
func countCalls(env *Environment, on *bool) *fsCalls {
	calls := &fsCalls{}
	env.ReadDir = func(path string) ([]fs.DirEntry, error) {
		if *on {
			calls.readDirs = append(calls.readDirs, path)
		}
		return os.ReadDir(path)
	}
	env.Lstat = func(path string) (fs.FileInfo, error) {
		if *on {
			calls.lstats++
		}
		return os.Lstat(path)
	}
	env.Open = func(path string) (io.ReadCloser, error) {
		if *on {
			calls.opens = append(calls.opens, path)
		}
		return os.Open(path)
	}
	return calls
}

// A grown store is not listed again in full: with 1,000 observed rollouts,
// added rollouts cost the listings of the folders that changed and their
// header reads, beside compare's renewal of each observation.
func TestAddedSourcesListOnlyChangedFolders(t *testing.T) {
	rollout := func(tr *tree, day, id, cwd string) string {
		return tr.write(filepath.Join("home", ".codex", "sessions", "2026", "10", day, "rollout-2026-10-01T10-00-00-"+id+".jsonl"), codexTranscript(id, id, cwd, fixedNow))
	}
	for _, tc := range []struct {
		name   string
		add    func(tr *tree, cwd string) []string
		listed []string // folders listed again, under sessions/2026
	}{
		{name: "added in a day folder", listed: []string{"10/05"}, add: func(tr *tree, cwd string) []string {
			return []string{rollout(tr, "05", "00000000-0000-0000-0000-0000000000f1", cwd)}
		}},
		{name: "new day folder", listed: []string{"10", "10/11"}, add: func(tr *tree, cwd string) []string {
			return []string{rollout(tr, "11", "00000000-0000-0000-0000-0000000000f1", cwd), rollout(tr, "11", "00000000-0000-0000-0000-0000000000f2", cwd)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTree(t)
			cwd := tr.repo("home/other")
			for day := 1; day <= 10; day++ {
				for n := range 100 {
					rollout(tr, fmt.Sprintf("%02d", day), fmt.Sprintf("00000000-0000-0000-0000-%012d", day*1000+n), cwd)
				}
			}
			env := tr.env()
			on := false
			calls := countCalls(&env, &on)
			inv := inventoryBaseline(t, env)
			observed := len(inv.stamps)
			if observed < 1000 {
				t.Fatalf("baseline observed %d paths", observed)
			}
			added := tc.add(tr, cwd)
			on = true
			got, ok := inv.added(t.Context())
			if !ok || len(got.candidates) != len(added) || len(got.incomplete) != 0 {
				t.Fatalf("added = %+v, %v; want %d", got, ok, len(added))
			}
			sessions := tr.path("home/.codex/sessions")
			var listed []string
			for _, path := range calls.readDirs {
				if rel, under := strings.CutPrefix(path, filepath.Join(sessions, "2026")+string(filepath.Separator)); under {
					listed = append(listed, rel)
				} else if strings.HasPrefix(path, sessions) {
					t.Fatalf("listed unchanged %s again", path)
				}
			}
			// The other listings are the absent stores, one each.
			if !reflect.DeepEqual(listed, tc.listed) || len(calls.readDirs) > len(tc.listed)+8 {
				t.Fatalf("listed %v (all %v), want %v", listed, calls.readDirs, tc.listed)
			}
			if !reflect.DeepEqual(calls.opens, added) {
				t.Fatalf("read headers of %v, want %v", calls.opens, added)
			}
			// compare renews each observation once; the walk adds a few.
			if calls.lstats > observed+16 {
				t.Fatalf("%d lstats for %d observations", calls.lstats, observed)
			}
		})
	}
}

// Additions are judged by the apps' own discovery: a new Cursor chat in a
// project folder a planned chat had names that folder, while a text copy
// beside a JSONL chat, which discovery passes over, adds nothing.
func TestAddedCursorChatsAreDiscoveredAsCursorDoes(t *testing.T) {
	tr := newTree(t)
	slug := cursorSlug(tr.repo("home/site"))
	transcripts := filepath.Join("home", ".cursor", "projects", slug, "agent-transcripts")
	tr.write(filepath.Join(transcripts, "k1", "k1.jsonl"), cursorTranscript)
	tr.write(filepath.Join("home", ".cursor", "projects", "other-slug", "agent-transcripts", "o1", "o1.jsonl"), cursorTranscript)
	env := tr.env()
	inv := inventoryBaseline(t, env)
	tr.write(filepath.Join(transcripts, "k1.txt"), "user: hi\n")
	added := tr.write(filepath.Join(transcripts, "k2", "k2.jsonl"), cursorTranscript)
	got, ok := inv.added(t.Context())
	if !ok || len(got.candidates) != 1 || len(got.incomplete) != 0 {
		t.Fatalf("added = %+v, %v", got, ok)
	}
	c := got.candidates[0]
	if c.Session.NativeID != "k2" || c.WorkspaceKey != slug || c.Source.Path != added {
		t.Fatalf("candidate %+v", c)
	}
	planned := []*work{{t: &transcript{harness: harnessCursor, cursorSlug: slug}}}
	if root := classifyAddedSource(newResolver(env, config.Config{}, Filters{}), addedSourceSlugs(planned), c); root.unknown {
		t.Fatalf("chat in a planned project folder read as unknown: %+v", root)
	}
}

// A folder that cannot be listed, one that appeared or one the baseline
// could not list either, leaves its app's store incomplete, a gap of that
// app only.
func TestAddedFolderThatCannotBeListedIsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name       string
		unreadable string
	}{
		{name: "appeared", unreadable: "home/.codex/sessions/2026/10/09"},
		{name: "never listed", unreadable: "home/.codex/sessions/2026/08/01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, env, _, _, _ := addedSourcesFixture(t)
			tr.mkdir("home/.codex/sessions/2026/08/01")
			unreadable := tr.path(tc.unreadable)
			env.ReadDir = func(path string) ([]fs.DirEntry, error) {
				if path == unreadable {
					return nil, fs.ErrPermission
				}
				return os.ReadDir(path)
			}
			inv := inventoryBaseline(t, env)
			addedRollout(tr, "00000000-0000-0000-0000-0000000000e1", tr.repo("home/other"), addedOtherURL)
			got, ok := inv.added(t.Context())
			if !ok || !got.incomplete["codex"] || len(got.incomplete) != 1 {
				t.Fatalf("added = %+v, %v", got, ok)
			}
		})
	}
}

// New directory entries are bounded like header reads: past the bound the
// change is unattributed for every app.
func TestAddedListingIsBounded(t *testing.T) {
	for _, tc := range []struct {
		limit int
		ok    bool
	}{{limit: 5, ok: true}, {limit: 4}} {
		t.Run(strconv.Itoa(tc.limit), func(t *testing.T) {
			limit := recoveryAddedEntryLimit
			recoveryAddedEntryLimit = tc.limit
			t.Cleanup(func() { recoveryAddedEntryLimit = limit })
			tr, env, _, _, _ := addedSourcesFixture(t)
			inv := inventoryBaseline(t, env)
			other := tr.repo("home/other")
			// New month and day folders and three rollouts: five new entries.
			for n := range 3 {
				addedRollout(tr, fmt.Sprintf("00000000-0000-0000-0000-0000000000e%d", n), other, addedOtherURL)
			}
			if got, ok := inv.added(t.Context()); ok != tc.ok || ok && len(got.candidates) != 3 {
				t.Fatalf("added = %+v, %v; want %v", got, ok, tc.ok)
			}
		})
	}
}

// A removed observed transcript stays strict even beside an addition in the
// same folder.
func TestAddedSourcesBesideARemovalStayStrict(t *testing.T) {
	tr, env, _, _, _ := addedSourcesFixture(t)
	inv := inventoryBaseline(t, env)
	if err := os.Remove(tr.path(filepath.Join("home", codexFile("00000000-0000-0000-0000-000000000011")))); err != nil {
		t.Fatal(err)
	}
	tr.write(filepath.Join("home", codexFile("00000000-0000-0000-0000-0000000000e1")), codexTranscript("00000000-0000-0000-0000-0000000000e1", "00000000-0000-0000-0000-0000000000e1", tr.repo("home/other"), fixedNow))
	if _, ok := inv.added(t.Context()); ok {
		t.Fatal("removal beside an addition read as only added")
	}
}

// Another spelling of an observed transcript (a symlinked store) is that
// transcript, not an added one; a copy is added.
func TestAddedSourcesKnowOtherSpellingsOfObservedFiles(t *testing.T) {
	tr, env, _, _, _ := addedSourcesFixture(t)
	inv := inventoryBaseline(t, env)
	file := tr.path(filepath.Join("home", codexFile("00000000-0000-0000-0000-000000000011")))
	link := tr.path("home/linked")
	if err := os.Symlink(filepath.Dir(file), link); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	copied := tr.write(filepath.Join("home", "copy", filepath.Base(file)), string(body))
	if !inv.known(file) || !inv.known(filepath.Join(link, filepath.Base(file))) || inv.known(copied) || inv.known(filepath.Join(link, "missing.jsonl")) {
		t.Fatal("known did not tell observed transcripts by file identity")
	}
}

// A store reached through a symlink (archived_sessions or projects linked to
// another disk) gains entries without its link moving: the directory the link
// names is stamped too, so an addition there is found, alone or beside
// another addition.
func TestAddedSourcesInSymlinkedStores(t *testing.T) {
	const knownID, addedID = "00000000-0000-0000-0000-0000000000c1", "00000000-0000-0000-0000-0000000000c2"
	for _, tc := range []struct {
		name  string
		store string // linked store, under home
		file  func(id string) string
	}{
		{name: "codex archived", store: ".codex/archived_sessions", file: func(id string) string {
			return "rollout-2026-09-01T10-00-00-" + id + ".jsonl"
		}},
		{name: "claude projects", store: ".claude/projects", file: func(id string) string {
			return filepath.Join("slug-"+id, id+".jsonl")
		}},
	} {
		for _, beside := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s beside=%v", tc.name, beside), func(t *testing.T) {
				tr, env, _, _, _ := addedSourcesFixture(t)
				other := tr.repo("home/other")
				body := func(id string) string {
					if strings.HasPrefix(tc.store, ".claude") {
						return claudeTranscript(id, other, fixedNow)
					}
					return codexTranscript(id, id, other, fixedNow)
				}
				store := tr.path(filepath.Join("home", tc.store))
				target := tr.mkdir("elsewhere/store")
				if entries, err := os.ReadDir(store); err == nil {
					for _, e := range entries {
						if err := os.Rename(filepath.Join(store, e.Name()), filepath.Join(target, e.Name())); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := os.RemoveAll(store); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(store), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, store); err != nil {
					t.Fatal(err)
				}
				tr.write(filepath.Join("elsewhere", "store", tc.file(knownID)), body(knownID))
				inv := inventoryBaseline(t, env)
				want := []string{filepath.Join(store, tc.file(addedID))}
				tr.write(filepath.Join("elsewhere", "store", tc.file(addedID)), body(addedID))
				if beside {
					want = append(want, addedRollout(tr, "00000000-0000-0000-0000-0000000000e1", other, addedOtherURL))
				}
				got, ok := inv.added(t.Context())
				var paths []string
				for _, c := range got.candidates {
					paths = append(paths, c.Source.Path)
				}
				sort.Strings(paths)
				sort.Strings(want)
				if !ok || !reflect.DeepEqual(paths, want) || len(got.incomplete) != 0 {
					t.Fatalf("added = %v (%+v), %v; want %v", paths, got.incomplete, ok, want)
				}
			})
		}
	}
}

// The directory a symlinked store names stays strict: replaced, removed,
// or named no more because the link now names another, every recovery fails.
func TestReplacedSymlinkedStoresAreStrict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, store, target string)
	}{
		{name: "target replaced", change: func(t *testing.T, _, target string) {
			t.Helper()
			if err := os.RemoveAll(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "target removed", change: func(t *testing.T, _, target string) {
			t.Helper()
			if err := os.RemoveAll(target); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "link retargeted", change: func(t *testing.T, store, target string) {
			t.Helper()
			other := target + "-other"
			if err := os.Mkdir(other, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(store); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, store); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, env, _, _, _ := addedSourcesFixture(t)
			store := tr.path("home/.codex/archived_sessions")
			target := tr.mkdir("elsewhere/store")
			if err := os.RemoveAll(store); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, store); err != nil {
				t.Fatal(err)
			}
			inv := inventoryBaseline(t, env)
			if _, ok := inv.added(t.Context()); !ok {
				t.Fatal("unchanged symlinked store failed")
			}
			tc.change(t, store, target)
			if got, ok := inv.added(t.Context()); ok {
				t.Fatalf("added = %+v, ok after the store's folder changed", got)
			}
		})
	}
}

// A symlinked store that names no directory (a file, nothing, or a link loop)
// was listed as nothing or as unreadable, an app's own gap: unchanged, it is
// no change at all, and once it names a directory, that directory's
// transcripts are found.
func TestAddedSourcesInSymlinkedStoresNamingNoDirectory(t *testing.T) {
	const addedID = "00000000-0000-0000-0000-0000000000c3"
	for _, tc := range []struct {
		name  string
		named func(t *testing.T, tr *tree, target string) // what the link names
	}{
		{name: "file", named: func(_ *testing.T, tr *tree, _ string) { tr.write("elsewhere/store", "not a folder") }},
		{name: "missing", named: func(*testing.T, *tree, string) {}},
		{name: "loop", named: func(t *testing.T, _ *tree, target string) {
			t.Helper()
			if err := os.Symlink(target, target); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		for _, becomes := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s becomes=%v", tc.name, becomes), func(t *testing.T) {
				tr, env, _, _, _ := addedSourcesFixture(t)
				store := tr.path("home/.codex/archived_sessions")
				target := tr.path("elsewhere/store")
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatal(err)
				}
				tc.named(t, tr, target)
				if err := os.RemoveAll(store); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, store); err != nil {
					t.Fatal(err)
				}
				inv := inventoryBaseline(t, env)
				var want []string
				if becomes {
					if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					name := "rollout-2026-09-01T10-00-00-" + addedID + ".jsonl"
					tr.write(filepath.Join("elsewhere", "store", name), codexTranscript(addedID, addedID, tr.repo("home/other"), fixedNow))
					want = []string{filepath.Join(store, name)}
				}
				got, ok := inv.added(t.Context())
				var paths []string
				for _, c := range got.candidates {
					paths = append(paths, c.Source.Path)
				}
				if !ok || !reflect.DeepEqual(paths, want) || len(got.incomplete) != 0 {
					t.Fatalf("added = %v (%+v), %v; want %v", paths, got.incomplete, ok, want)
				}
			})
		}
	}
}
