package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func handoffOlderChoices(t *testing.T) *scopeChoices {
	t.Helper()
	f := newHandoffFixture(t, false)
	var sessions []archive.Metadata
	for i := range 121 {
		project, repo := "other", "other-repo"
		if i < 60 {
			project, repo = "here", "here-repo"
		}
		sessions = append(sessions, archive.Metadata{SessionID: fmt.Sprintf("%032x", i+1), ProjectID: project, RepoKey: repo,
			Harness: archive.Harness{Name: "codex"}, Title: fmt.Sprintf("Task %d", i), CapturedAt: f.env.now().Add(-time.Duration(i) * time.Hour)})
	}
	// The older archive child remains searchable without being a picker row.
	sessions = append(sessions, archive.Metadata{SessionID: "child-session", ProjectID: "here", RepoKey: "here-repo",
		Harness: archive.Harness{Name: "codex"}, ParentSessionID: sessions[59].SessionID, Title: "older child needle"})
	picker := handoffPicker{ctx: context.Background(), env: f.env, home: f.home, archiveRead: true}
	return pagedHandoffChoices(sessionScope{Label: "here", RepoKey: "here-repo", ProjectIDs: []string{"here"}},
		listFormatOptions{Now: f.env.now(), Numbered: true, Children: childCounts(sessions)}, picker, nil, sessions)
}

func TestHandoffOlderRowsPreserveScopeAndExhaustiveSearch(t *testing.T) {
	t.Parallel()
	choices := handoffOlderChoices(t)
	if got := choices.shown(); len(got.rows) != 50 || got.total != 60 || !choices.loadOlder.Available() {
		t.Fatalf("initial scoped view: %+v", got)
	}
	if got := choices.shown().search(); len(got) != 61 || got[59].Title != "Task 59" || got[60].Parent == "" {
		t.Fatalf("complete scoped search: %+v", got)
	}
	more, err := choices.loadOlder.Load()
	if err != nil || more || choices.loadOlder.Available() || len(choices.shown().rows) != 60 {
		t.Fatalf("exhausted scope: more=%v error=%v rows=%d", more, err, len(choices.shown().rows))
	}
	choices.toggle()
	if got := choices.shown(); len(got.rows) != 100 || got.total != 121 || !choices.loadOlder.Available() {
		t.Fatalf("widened view: %+v", got)
	}
	more, err = choices.loadOlder.Load()
	if err != nil || more || choices.loadOlder.Available() || len(choices.shown().rows) != 121 {
		t.Fatalf("exhausted all: more=%v error=%v rows=%d", more, err, len(choices.shown().rows))
	}
	for i, row := range choices.shown().rows {
		if row.Index != i+1 || row.Title != fmt.Sprintf("Task %d", i) {
			t.Fatalf("row %d lost order or numbering: %+v", i, row)
		}
	}
	choices.toggle()
	if len(choices.shown().rows) != 60 || choices.loadOlder.Available() || len(choices.shown().search()) != 61 {
		t.Fatal("returning to the exhausted scope changed its universe")
	}
}

func TestHandoffOlderActionReturnsAfterScopeToggle(t *testing.T) {
	t.Parallel()
	choices := handoffOlderChoices(t)
	picker := &sessionPicker{env: fixedTerminal{100, 30}, older: choices.loadOlder}
	screens := keyScreens(t, picker, choices, "hand off", "o", "a", "o", "a", "q")
	var loaded, scopedDone, allMore, allDone bool
	for _, screen := range screens {
		hasOlder := strings.Contains(screen, "o · Older sessions")
		switch {
		case strings.Contains(screen, "here · 60 sessions") && strings.Contains(screen, "Showing 50 of 60"):
			loaded = loaded || hasOlder
		case strings.Contains(screen, "here · 60 sessions") && !strings.Contains(screen, "Showing 50 of 60"):
			scopedDone = scopedDone || !hasOlder
		case strings.Contains(screen, "All projects · 121 sessions") && strings.Contains(screen, "Showing 100 of 121"):
			allMore = allMore || hasOlder
		case strings.Contains(screen, "All projects · 121 sessions") && !strings.Contains(screen, "Showing 100 of 121"):
			allDone = allDone || !hasOlder
		}
	}
	if !loaded || !scopedDone || !allMore || !allDone {
		t.Fatalf("action availability initial=%v scopeDone=%v allMore=%v allDone=%v screens=%q", loaded, scopedDone, allMore, allDone, screens)
	}
}

func TestHandoffOlderLoadsUnpublishedRowsOnlyOnRequest(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	must(t, os.Chtimes(filepath.Join(f.project, "codex.jsonl"), f.env.now(), f.env.now()))
	for i := range defaultListLimit {
		f.addSession(t, "codex", fmt.Sprintf("newer-%d", i), fmt.Sprintf("Newer task %d", i), f.env.now().Add(time.Duration(i+1)*time.Hour))
	}
	// An old empty transcript makes the initial count unknown, but must not
	// leave the load action available after it has been inspected and skipped.
	f.addSession(t, "codex", "older-empty", "", f.env.now().Add(-time.Hour))
	out, errOut, code := runPicker(t, f.env, "q\n", "--source", "local")
	if code != 0 || !strings.Contains(out, "o · Older sessions") || strings.Contains(out, "Fix the flaky widget test") {
		t.Fatalf("initial view: code=%d stderr=%s output=%s", code, errOut, out)
	}
	out, errOut, code = runPicker(t, f.env, "o\nq\n", "--source", "local")
	if code != 0 || !strings.Contains(out, "Fix the flaky widget test") || !strings.Contains(out, "51 session(s).") {
		t.Fatalf("expanded local view: code=%d stderr=%s output=%s", code, errOut, out)
	}
	last := out[strings.LastIndex(out, "Fix the flaky widget test"):]
	if strings.Contains(last, "o · Older sessions") || strings.Contains(last, "older-empty") {
		t.Fatalf("empty candidate did not exhaust: %s", last)
	}
}
