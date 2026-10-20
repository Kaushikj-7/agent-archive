package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// directReads is every key recorded as read so far.
func (s *recordingStore) directReads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.gets)
}

// directSession is one synthetic session a direct-search fixture publishes.
type directSession struct {
	native  string
	project int
	text    string
}

// directSearchFixture publishes sessions in order, an hour apart (so the last
// is newest), across two configured projects, with indexing declined and no
// local index. It returns each session's archive ID and source key.
func directSearchFixture(t *testing.T, sessions []directSession) (Env, *recordingStore, []string, []string) {
	t.Helper()
	home := t.TempDir()
	projects := [2]string{t.TempDir(), t.TempDir()}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	must(t, os.Chmod(home, 0700))
	cfg := config.Config{MachineID: "machine-1", Storage: credentialsTestConfig(), SearchIndexingDeclined: true, Archive: archive.Config{SchemaVersion: 1, MachineID: "machine-1", Enabled: true}}
	for _, root := range projects {
		cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: true, ActivatedAt: start})
	}
	must(t, config.Save(home, cfg))
	mem := storagetest.NewMemoryStore()
	objects := &recordingStore{ObjectStore: mem}
	var ids, sources []string
	for i, s := range sessions {
		at := start.Add(time.Duration(i+1) * time.Hour)
		dir := t.TempDir()
		transcript := filepath.Join(dir, "codex.jsonl")
		record, err := json.Marshal(map[string]any{"type": "response_item", "id": "m1", "payload": map[string]any{"type": "message", "role": "assistant", "content": s.text}})
		must(t, err)
		must(t, os.WriteFile(transcript, append([]byte("{\"type\":\"turn_context\",\"model\":\"gpt-test\"}\n"), record...), 0600))
		payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": s.native, "cwd": projects[s.project], "transcript_path": transcript}
		must(t, handleTestHookEvent(home, "codex", payload, at))
		env := testEnv(t, home, at)
		env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return mem, nil }
		var out, errOut bytes.Buffer
		if code := runSyncCommand(nil, &out, &errOut, env); code != 0 {
			t.Fatalf("sync %s: %d %s", s.native, code, &errOut)
		}
		local, err := state.Open(home)
		must(t, err)
		regs, err := local.LoadRegistrations()
		must(t, err)
		for _, reg := range regs {
			if reg.NativeSessionID == s.native {
				ids = append(ids, reg.ArchiveSessionID)
			}
		}
		if len(ids) != i+1 {
			t.Fatalf("registration of %s missing: %+v", s.native, regs)
		}
		key, err := archive.MetadataObjectKey("codex", ids[i])
		must(t, err)
		data, err := mem.Get(t.Context(), key)
		must(t, err)
		var metadata archive.Metadata
		must(t, json.Unmarshal(data, &metadata))
		sources = append(sources, metadata.SourceBundle.Key)
	}
	env := testEnv(t, home, start.Add(time.Duration(len(sessions)+1)*time.Hour))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects, nil }
	env.WorkingDir = func() (string, error) { return projects[0], nil }
	env.directSearchPause = func() time.Duration { return time.Hour }
	return env, objects, ids, sources
}

// assertNoSearchIndex fails when anything was written under the search
// directory: a direct search keeps no text on this machine.
func assertNoSearchIndex(t *testing.T, env Env) {
	t.Helper()
	home, err := env.readHome()
	must(t, err)
	entries, err := os.ReadDir(filepath.Join(home, "search"))
	if err == nil && len(entries) > 0 || err != nil && !os.IsNotExist(err) {
		t.Fatalf("direct search wrote %v %v", entries, err)
	}
}

func TestDeclinedIndexingSearchesTheBucketNewestFirst(t *testing.T) {
	t.Parallel()
	env, objects, ids, _ := directSearchFixture(t, []directSession{
		{"native-old", 0, "checksum mismatch in the old upload"},
		{"native-other", 1, "nothing to see"},
		{"native-new", 0, "a newer checksum failure"},
	})
	out, errOut, code := runOnTerminal(t, env, newFakeKeys("q"), "", "search", "checksum")
	if code != 0 {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
	for _, want := range []string{
		"! Without an index, search downloads sessions from your S3 bucket, newest first. Nothing is kept on this machine.",
		"It pauses after 60 minutes; across all 3 sessions a full search takes about a minute.",
		"To search instantly next time, run:\n  agent-archive setup --yes --search-indexing on\n",
		"Searched all 3 sessions · 2 matching sessions",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	newer, older := strings.Index(out, shortSessionID(ids[2])+"  1 match"), strings.Index(out, shortSessionID(ids[0])+"  1 match")
	if newer < 0 || older < 0 || newer > older {
		t.Fatalf("matches not printed newest first (%d, %d):\n%s", newer, older, out)
	}
	if strings.Contains(out, shortSessionID(ids[1])+"  ") {
		t.Fatalf("printed a session that does not match:\n%s", out)
	}
	if !strings.Contains(out, "checksum") || strings.Contains(out, "No search index yet") || strings.Contains(out, "Run search --refresh") {
		t.Fatalf("results browser did not open over what was found:\n%s", out)
	}
	if len(objects.directReads()) == 0 {
		t.Fatal("searched without reading the bucket")
	}
	assertNoSearchIndex(t, env)
}

func TestDirectSearchPausesAndNarrowsBeforeDownloadingOtherProjects(t *testing.T) {
	t.Parallel()
	env, objects, ids, sources := directSearchFixture(t, []directSession{
		{"native-a-old", 0, "checksum in project a"},
		{"native-b", 1, "checksum in project b"},
		{"native-a-new", 0, "checksum again in project a"},
	})
	env.directSearchPause = func() time.Duration { return 0 }
	out, errOut, code := runOnTerminal(t, env, newFakeKeys("q"), "2\n", "search", "checksum")
	if code != 0 {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
	for _, want := range []string{"Searched the newest 1 of 3 sessions · 1 matching session", "Keep searching the bucket?", "1) Keep going", "(2 sessions)", "Show the results so far", "Searched all 2 sessions · 2 matching sessions"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if slices.Contains(objects.directReads(), sources[1]) {
		t.Fatalf("downloaded the other project's source after narrowing: %v", objects.directReads())
	}
	for _, i := range []int{0, 2} {
		if !slices.Contains(objects.directReads(), sources[i]) {
			t.Fatalf("did not read %s: %v", ids[i], objects.directReads())
		}
	}
	assertNoSearchIndex(t, env)
}

func TestDirectSearchStopAtPauseEndsWithoutResults(t *testing.T) {
	t.Parallel()
	env, _, _, _ := directSearchFixture(t, []directSession{{"native-1", 0, "checksum one"}, {"native-2", 0, "checksum two"}})
	env.directSearchPause = func() time.Duration { return 0 }
	out, errOut, code := runOnTerminal(t, env, nil, "4\n", "search", "checksum")
	if code != 0 || !strings.Contains(out, "Searched the newest 1 of 2 sessions") || strings.Contains(out, "Searched all") {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
}

// stallingStore never finishes reading the named keys: a read of one waits
// until its context ends, as a stalled download would.
type stallingStore struct {
	storage.ObjectStore
	stall []string
}

func (s stallingStore) Get(ctx context.Context, key string) ([]byte, error) {
	if slices.Contains(s.stall, key) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.ObjectStore.Get(ctx, key)
}

func (s stallingStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if slices.Contains(s.stall, key) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.ObjectStore.(storage.LimitedGetter).GetLimited(ctx, key, limit)
}

func TestDirectSearchEscStopsADownloadInProgress(t *testing.T) {
	t.Parallel()
	env, objects, _, sources := directSearchFixture(t, []directSession{{"native-1", 0, "checksum one"}, {"native-2", 0, "checksum two"}})
	stalled := stallingStore{ObjectStore: objects.ObjectStore, stall: sources}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return stalled, nil }
	out, errOut, code := runOnTerminal(t, env, newFakeKeys(later(0, "\x1b")), "", "search", "checksum")
	if code != 0 || !strings.Contains(out, "Stopped before searching any of the 2 sessions") || strings.Contains(out, "Searched all") {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
	assertNoSearchIndex(t, env)
}

func TestDirectSearchNeverRunsForJSONOrWithAnIndex(t *testing.T) {
	t.Parallel()
	env, objects, _, sources := directSearchFixture(t, []directSession{{"native-1", 0, "checksum one"}})
	var out, errOut bytes.Buffer
	if code := Run([]string{"search", "checksum", "--json"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("json %d %s", code, &errOut)
	}
	if strings.Contains(out.String(), "Without an index") || slices.Contains(objects.directReads(), sources[0]) {
		t.Fatalf("JSON searched the bucket: %s %v", &out, objects.directReads())
	}
	piped, _, code := runOnTerminal(t, env, nil, "", "search", "checksum", "--json")
	if code != 0 || strings.Contains(piped, "Without an index") {
		t.Fatalf("terminal JSON searched the bucket: %d %s", code, piped)
	}
	out.Reset()
	indexing := env
	indexing.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects.ObjectStore, nil }
	if code := Run([]string{"search", "--refresh"}, nil, &out, &errOut, indexing); code != 0 {
		t.Fatalf("refresh %d %s", code, &errOut)
	}
	terminalOut, errText, code := runOnTerminal(t, env, newFakeKeys("q"), "", "search", "checksum")
	if code != 0 || strings.Contains(terminalOut, "Without an index") {
		t.Fatalf("searched the bucket with an index present: %d %s %s", code, errText, terminalOut)
	}
}

func TestDirectSearchFiltersMetadataLikeTheIndex(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	base := archive.Metadata{SessionID: "s", ProjectID: "p", Harness: archive.Harness{Name: "codex"}, CapturedAt: day}
	child, replay, other := base, base, base
	child.ParentSessionID = "parent"
	replay.Replay = &archive.Replay{}
	other.ProjectID = "elsewhere"
	all := sessionScope{All: true}
	project := sessionScope{Label: "p", Dir: "/p", ProjectIDs: []string{"p"}}
	for name, c := range map[string]struct {
		m     archive.Metadata
		q     search.Query
		scope sessionScope
		want  bool
	}{
		"plain":               {base, search.Query{Replay: search.Hide, Children: search.Hide}, all, true},
		"other harness":       {base, search.Query{Harness: "claude"}, all, false},
		"before since":        {base, search.Query{From: day.Add(time.Hour)}, all, false},
		"after until":         {base, search.Query{To: day.Add(-time.Hour)}, all, false},
		"hidden child":        {child, search.Query{Children: search.Hide}, all, false},
		"only children":       {base, search.Query{Children: search.Only}, all, false},
		"included child":      {child, search.Query{}, all, true},
		"hidden replay":       {replay, search.Query{Replay: search.Hide}, all, false},
		"only replays":        {replay, search.Query{Replay: search.Only}, all, true},
		"in project":          {base, search.Query{}, project, true},
		"outside the project": {other, search.Query{}, project, false},
	} {
		if got := directSearchCandidate(c.m, c.q, c.scope); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	for n, want := range map[int]string{0: "0", 999: "999", 2350: "2,350", 1234567: "1,234,567"} {
		if got := countText(n); got != want {
			t.Errorf("countText(%d) = %q", n, got)
		}
	}
	for minutes, want := range map[int]string{1: "about a minute", 5: "about 5 minutes", 47: "about 47 minutes", 60: "about an hour", 180: "about 3 hours"} {
		if got := aboutMinutes(minutes); got != want {
			t.Errorf("aboutMinutes(%d) = %q", minutes, got)
		}
	}
}
