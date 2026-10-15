package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func prepareSearchBrowser(t *testing.T) (Env, storage.ObjectStore, string) {
	t.Helper()
	env, objects, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"search", "--refresh", "--session", id, "--harness", "codex"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("refresh %d %s %s", code, &out, &errOut)
	}
	return env, objects, id
}

func TestSearchBrowserKeysAndLinesKeepExactBodyReference(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		t.Run(map[bool]string{false: "lines", true: "keys"}[keyed], func(t *testing.T) {
			env, _, id := prepareSearchBrowser(t)
			var fake *fakeKeys
			input := "1\nn\np\nb\nq\n"
			if keyed {
				fake = newFakeKeys("1\r", "n", "p", "b", "q")
				input = ""
			}
			out, errOut, code := runOnTerminal(t, env, fake, input, "search", "visible", "--all-projects")
			if code != 0 {
				t.Fatalf("%d %s %s", code, errOut, out)
			}
			for _, want := range []string{id, "Reference:", "[selected · assistant]", "visible", "Hand off current session", "no further matching body passage."} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q: %s", want, out)
				}
			}
			if strings.Contains(out, "words to filter") || strings.Contains(out, "/ filter ·") {
				t.Fatalf("metadata filter exposed: %s", out)
			}
			if !strings.Contains(out, leaveAltScreenSequence) {
				t.Fatal("screen not restored")
			}
		})
	}
}

func TestSearchBrowserQueryAndPaintNeverOpenArchive(t *testing.T) {
	env, _, _ := prepareSearchBrowser(t)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Error("query or paint opened archive")
		return nil, errors.New("offline")
	}
	out, errOut, code := runOnTerminal(t, env, newFakeKeys("/", "\x1b", "\x1b[B", "\x1b[6~", "q"), "", "search", `"visible"`, "--all-projects")
	if code != 0 || !strings.Contains(out, "visible") {
		t.Fatalf("%d %s %s", code, errOut, out)
	}
}

func TestSearchBrowserPipedAndJSONNeverReadInput(t *testing.T) {
	env, _, _ := prepareSearchBrowser(t)
	env.IsTerminal = func(any) bool { return false }
	var out, errOut bytes.Buffer
	if code := Run([]string{"search", "visible", "--all-projects"}, failingSearchInput{t}, &out, &errOut, env); code != 0 {
		t.Fatalf("%d %s", code, &errOut)
	}
	env.IsTerminal = func(any) bool { return true }
	out.Reset()
	if code := Run([]string{"search", "visible", "--all-projects", "--json"}, failingSearchInput{t}, &out, &errOut, env); code != 0 {
		t.Fatalf("%d %s", code, &errOut)
	}
	if strings.Contains(out.String(), enterAltScreenSequence) {
		t.Fatal("JSON entered browser")
	}
}

type failingSearchInput struct{ t *testing.T }

func (r failingSearchInput) Read([]byte) (int, error) {
	r.t.Error("noninteractive search read input")
	return 0, io.EOF
}

func TestSearchBrowserResultRetentionAndExplicitRerun(t *testing.T) {
	env, _, _ := prepareSearchBrowser(t)
	home, _ := env.readHome()
	cfg, _, _ := config.Load(home)
	namespace, dir := contentSearchLocation(home, cfg)
	index, err := search.OpenRead(context.Background(), dir, namespace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	q := search.Query{Text: "visible", Limit: 1}
	page, err := index.Search(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	s := &contentSearchBrowser{env: env, index: index, opts: contentSearchOptions{query: q}, page: page, hits: page.Hits, pages: 1, choices: rowChoices(nil, listFormatOptions{Numbered: true})}
	s.installRows()
	s.stale = true
	s.rerun = true
	if _, err = s.load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.stale || s.pages != 1 || len(s.hits) != 1 || len(s.byKey) != 1 {
		t.Fatalf("rerun retained old state: %+v", s)
	}
	s.pages = 3
	if _, err = s.load(t.Context()); err != nil || len(s.hits) != 1 {
		t.Fatalf("retention cap failed: %v", err)
	}
}

// cancellationSearchStore completes only after cancellation. The browser must
// join it before returning or replacing the selected reference.
type cancellationSearchStore struct {
	storage.ObjectStore
	started chan struct{}
	stopped chan struct{}
}

func (s *cancellationSearchStore) GetLimited(ctx context.Context, _ string, _ int64) ([]byte, error) {
	close(s.started)
	<-ctx.Done()
	close(s.stopped)
	return nil, ctx.Err()
}

func TestSearchSelectedReadCancelsAndJoinsBeforeReturning(t *testing.T) {
	for _, action := range []string{"q", "n", "b", "h"} {
		t.Run(action, func(t *testing.T) {
			env, objects, _ := prepareSearchBrowser(t)
			home, _ := env.readHome()
			cfg, _, _ := config.Load(home)
			namespace, dir := contentSearchLocation(home, cfg)
			index, err := search.OpenRead(t.Context(), dir, namespace)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = index.Close() }()
			page, err := index.Search(t.Context(), search.Query{Text: "visible"})
			if err != nil || len(page.Hits) != 1 || page.Hits[0].Reference == nil {
				t.Fatalf("page: %+v %v", page, err)
			}
			blocked := &cancellationSearchStore{ObjectStore: objects, started: make(chan struct{}), stopped: make(chan struct{})}
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return blocked, nil }
			fake := newFakeKeys(later(20*time.Millisecond, action))
			keys := startKeys(fake)
			defer keys.close()
			b := &sessionBrowser{search: &contentSearchBrowser{env: env, cfg: cfg, namespace: namespace}, keys: keys, prompt: newPrompter(strings.NewReader(""), io.Discard), stdout: io.Discard, list: &sessionPicker{keys: keys}}
			_, err = b.readSearchSelection(t.Context(), *page.Hits[0].Reference, page.Hits[0].Reference.Start)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("read: %v", err)
			}
			select {
			case <-blocked.started:
			default:
				t.Fatal("read never started")
			}
			select {
			case <-blocked.stopped:
			default:
				t.Fatal("read was not joined")
			}
			if action == "q" {
				if !b.list.operationQuit {
					t.Fatal("quit lost")
				}
			} else if b.search.pendingAction != action {
				t.Fatalf("replacement action lost: %q", b.search.pendingAction)
			}
		})
	}
}

func TestSearchHandoffPreservesTerminalAndBufferedDestinationAnswer(t *testing.T) {
	for _, keyed := range []bool{false, true} {
		t.Run(map[bool]string{false: "lines", true: "keys"}[keyed], func(t *testing.T) {
			env, _, id := prepareSearchBrowser(t)
			fake := (*fakeKeys)(nil)
			input := "1\nh\np\n"
			if keyed {
				fake = newFakeKeys("1\r", "h")
				input = "p\n"
			}
			out, errOut, code := runOnTerminal(t, env, fake, input, "search", "visible", "--all-projects")
			if code != 0 {
				t.Fatalf("%d %s %s", code, errOut, out)
			}
			restored := strings.LastIndex(out, leaveAltScreenSequence)
			prompt := strings.Index(out, "Continue in")
			if restored < 0 || prompt < restored {
				t.Fatalf("handoff prompt before terminal restore: %s", out)
			}
			if !strings.Contains(out, id) || !strings.Contains(out, "source: archive") || !strings.Contains(out, "visible") {
				t.Fatalf("selected archive session not handed off: %s", out)
			}
		})
	}
}

func TestSearchRowsKeepRankFullIdentityAndEscapeText(t *testing.T) {
	s := &contentSearchBrowser{hits: []search.Hit{
		{Harness: "codex", SessionID: "same-session", Title: "body-only first", Snippet: "needle\x1b[2J\u202e", MatchCount: 3},
		{Harness: "claude", SessionID: "same-session", Title: "second", Snippet: "needle"},
	}, page: search.Page{Next: "more"}, pages: 1, choices: rowChoices(nil, listFormatOptions{Numbered: true, NarrowHint: "o next results"})}
	s.installRows()
	c := s.choices.shown()
	if c.total != -1 || !c.truncated {
		t.Fatal("unknown total was presented as exact")
	}
	if len(s.byKey) != 2 || c.rows[0].HarnessKey != "codex" || c.rows[1].HarnessKey != "claude" {
		t.Fatalf("identity/order lost: %+v", c.rows)
	}
	if strings.ContainsAny(c.rows[0].searchSnippet, "\x1b\u202e") {
		t.Fatalf("unsafe snippet: %q", c.rows[0].searchSnippet)
	}
	if c.rows[0].pickerSelection != nil || c.rows[1].pickerSelection != nil {
		t.Fatal("search manufactured picker authority")
	}
	if _, ok := matchBrowseRow("same-session", c.rows); ok {
		t.Fatal("ambiguous cross-harness ID selected")
	}
	if row, ok := matchBrowseRow("2", c.rows); !ok || row.selectionKey != childKey("claude", "same-session") {
		t.Fatal("numeric selection lost qualified identity")
	}
}
