package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
)

// addArchived publishes an archived session of project (by name), cloned from
// the fixture's own sidecar. In the project of the fixture's checkout when
// project is that checkout's name.
func (f pickerFixture) addArchived(t *testing.T, id, title, project string) {
	t.Helper()
	f.addArchivedWith(t, id, title, project, nil)
}

// addArchivedWith is addArchived with change applied to the metadata before
// it is published.
func (f pickerFixture) addArchivedWith(t *testing.T, id, title, project string, change func(*archive.Metadata)) {
	t.Helper()
	key, err := archive.MetadataObjectKey("codex", f.id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := f.mem.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.SessionID, m.Title, m.ProjectName = id, title, project
	m.ProjectID = archive.ProjectID("/elsewhere/" + project)
	if project == filepath.Base(f.project) {
		m.ProjectID = archive.ProjectID(f.project)
	}
	m.CapturedAt = m.CapturedAt.Add(-time.Minute)
	if change != nil {
		change(&m)
	}
	// The copy was last active when it was captured, which dates and orders
	// its row, as change left it.
	ended := m.CapturedAt
	m.EndedAt = &ended
	key, err = archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.mem.Put(context.Background(), key, encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.RebuildIndex(context.Background(), f.mem, archiveSessionsPrefix); err != nil {
		t.Fatal(err)
	}
}

// addEmptyProject configures a project root that has no sessions.
func (f pickerFixture) addEmptyProject(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	cfg, found, err := config.Load(f.home)
	if err != nil || !found {
		t.Fatalf("config: %v %v", found, err)
	}
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: true, ActivatedAt: time.Unix(0, 0)})
	if err := config.Save(f.home, cfg); err != nil {
		t.Fatal(err)
	}
	return root
}

// pickerHeadings are the heading lines of a line-mode picker's output: the
// ones that name a scope, as the picker words them, without the verb that
// leads them and the screen's control sequences.
func pickerHeadings(out string) []string {
	var headings []string
	out = strings.NewReplacer(enterAltScreenSequence, "", clearScreenSequence, "").Replace(out)
	for line := range strings.SplitSeq(out, "\n") {
		// The prompt before it is answered from input, so it has no newline.
		if _, after, ok := strings.Cut(line, "or q to quit: "); ok {
			line = after
		}
		line = strings.TrimPrefix(line, "Hand off · ")
		if strings.Contains(line, " sessions · ") || strings.HasPrefix(line, "Nothing in ") {
			headings = append(headings, line)
		}
	}
	return headings
}

// The handoff picker opens on the working directory's repository, names it,
// and a, typed alone, shows all projects and back.
func TestHandoffPickerTogglesTheScopeWithA(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.addArchived(t, "bill0001", "Invoice export", "billing")
	label := filepath.Base(f.project)
	out, errOut, code := runPicker(t, f.env, "a\na\nq\n")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	want := []string{
		label + " · 3 sessions · codex · a all projects",
		"All projects · 4 sessions · codex · a " + label,
		label + " · 3 sessions · codex · a all projects",
	}
	if got := pickerHeadings(out); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("headings\n%s\nwant\n%s\n\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"), out)
	}
	// The other project's session appears on the second screen only.
	first, second, _ := strings.Cut(out, "All projects")
	if strings.Contains(first, "bill0001") || !strings.Contains(second, "bill0001") {
		t.Fatalf("the scope offers another project's session, or all projects do not:\n%s", out)
	}
	// Columns every row shares are named in the heading, not drawn; the
	// project varies across all projects, so it is drawn there.
	if strings.Contains(first, "PROJECT") || !strings.Contains(second, "PROJECT") {
		t.Fatalf("PROJECT column:\n%s", out)
	}
}

// --project names another project for the picker, and applies to every
// selection, not only --latest.
func TestHandoffProjectNarrowsThePicker(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.addArchived(t, "bill0001", "Invoice export", "billing")
	f.addArchived(t, "bill0002", "Refund rules", "billing")
	for _, project := range []string{"billing", "Billing"} {
		out, errOut, code := runPicker(t, f.env, "q\n", "--project", project)
		if code != 0 {
			t.Fatalf("--project %s: code=%d stderr=%s", project, code, errOut)
		}
		if !strings.Contains(out, "bill0001") || !strings.Contains(out, "bill0002") || strings.Contains(out, f.notUploaded[:minShortSessionID]) || strings.Contains(out, f.both[:minShortSessionID]) {
			t.Fatalf("--project %s:\n%s", project, out)
		}
		if got := pickerHeadings(out); len(got) != 1 || !strings.HasPrefix(got[0], project+" · 2 sessions") {
			t.Fatalf("--project %s: headings %q", project, got)
		}
	}
	// A directory gives that directory's repository.
	out, _, code := runPicker(t, f.env, "q\n", "--project", f.project)
	if code != 0 || !strings.Contains(out, f.both[:minShortSessionID]) || strings.Contains(out, "bill0001") {
		t.Fatalf("--project DIR: code=%d\n%s", code, out)
	}
	out, _, code = runPicker(t, f.env, "q\n", "--all-projects")
	if code != 0 || !strings.Contains(out, "bill0001") || !strings.Contains(out, f.both[:minShortSessionID]) {
		t.Fatalf("--all-projects: code=%d\n%s", code, out)
	}
	if _, errOut, code := runPicker(t, f.env, "q\n", "--all-projects", "--project", "billing"); code != 2 || !strings.Contains(errOut, "--project and --all-projects") {
		t.Fatalf("both flags: code=%d stderr=%s", code, errOut)
	}
}

// An empty scope falls back to all projects, and the heading says so.
func TestHandoffPickerFallsBackWhenTheScopeIsEmpty(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.addArchived(t, "bill0001", "Invoice export", "billing")
	empty := f.addEmptyProject(t, "empty-project")
	f.env.WorkingDir = func() (string, error) { return empty, nil }
	out, errOut, code := runPicker(t, f.env, "q\n")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	want := "Nothing in empty-project · showing all projects · 4 sessions · codex"
	if got := pickerHeadings(out); len(got) != 1 || got[0] != want {
		t.Fatalf("headings %q, want %q\n%s", got, want, out)
	}
}

// Outside any project every session shows, with no scope named.
func TestHandoffPickerOutsideAnyProjectShowsEverySession(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.addArchived(t, "bill0001", "Invoice export", "billing")
	elsewhere := t.TempDir()
	f.env.WorkingDir = func() (string, error) { return elsewhere, nil }
	out, errOut, code := runPicker(t, f.env, "q\n")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	for _, id := range []string{f.notUploaded, f.archiveOnly, f.both, "bill0001"} {
		if !strings.Contains(out, id[:minShortSessionID]) {
			t.Fatalf("%s is not listed:\n%s", id, out)
		}
	}
	if strings.Contains(out, "All projects") || strings.Contains(out, " · a ") {
		t.Fatalf("a scope is offered outside any project:\n%s", out)
	}
}

// A title looks in the scope first: its one match there is the answer, and a
// note says how many more match elsewhere; everywhere is searched when the
// scope has none, and --all-projects searches everything at once.
func TestHandoffTitleSearchesTheScopeFirst(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	label := filepath.Base(f.project)
	f.addArchived(t, "alpha001", "Shared words alpha", label)
	f.addArchived(t, "beta0001", "Shared words beta", "billing")
	f.addArchived(t, "gamma001", "Gamma only here", "billing")
	resolve := func(query string, change func(*handoffOptions)) (opts handoffOptions, errOut string, code int, done bool) {
		t.Helper()
		opts = handoffOptions{sessionID: query, source: "auto"}
		if change != nil {
			change(&opts)
		}
		var out, errBuf bytes.Buffer
		code, done = resolveHandoffQuery(&opts, f.home, false, newTypedInput(strings.NewReader("")), &out, &errBuf, f.env)
		return opts, errBuf.String(), code, done
	}

	opts, errOut, code, done := resolve("shared words", nil)
	note := "1 match in " + label + " (1 more in other projects: --all-projects or a project name finds them)"
	if done || code != 0 || opts.sessionID != "alpha001" || strings.TrimSpace(errOut) != note {
		t.Fatalf("scoped: session %q, code %d done %v, stderr %q", opts.sessionID, code, done, errOut)
	}

	// Nothing in scope: the title is searched everywhere, with no note.
	opts, errOut, code, done = resolve("gamma only", nil)
	if done || code != 0 || opts.sessionID != "gamma001" || errOut != "" {
		t.Fatalf("everywhere: session %q, code %d done %v, stderr %q", opts.sessionID, code, done, errOut)
	}

	// --all-projects puts both on an even footing: two matches, no terminal.
	_, errOut, code, done = resolve("shared words", func(o *handoffOptions) { o.allProjects = true })
	if !done || code != 1 || !strings.Contains(errOut, `"shared words" matches 2 sessions`) || strings.Contains(errOut, "more in other projects") {
		t.Fatalf("--all-projects: code %d done %v, stderr %q", code, done, errOut)
	}

	// --project names the scope: billing's own title answers there.
	opts, errOut, code, done = resolve("shared words", func(o *handoffOptions) { o.project = "billing" })
	note = "1 match in billing (1 more in other projects: --all-projects or a project name finds them)"
	if done || code != 0 || opts.sessionID != "beta0001" || strings.TrimSpace(errOut) != note {
		t.Fatalf("--project: session %q, code %d done %v, stderr %q", opts.sessionID, code, done, errOut)
	}
}

// Several matches in the scope are counted as matches, and listed for the
// caller to choose from.
func TestHandoffTitleNoteCountsSeveralMatches(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	label := filepath.Base(f.project)
	f.addArchived(t, "alpha001", "Shared words alpha", label)
	f.addArchived(t, "alpha002", "Shared words alpha again", label)
	f.addArchived(t, "beta0001", "Shared words beta", "billing")
	opts := handoffOptions{sessionID: "shared words", source: "auto"}
	var out, errOut bytes.Buffer
	code, done := resolveHandoffQuery(&opts, f.home, false, newTypedInput(strings.NewReader("")), &out, &errOut, f.env)
	note := "2 matches in " + label + " (1 more in other projects: --all-projects or a project name finds them)"
	if !done || code != 1 || !strings.HasPrefix(errOut.String(), note+"\n") || !strings.Contains(errOut.String(), `"shared words" matches 2 sessions`) {
		t.Fatalf("code %d done %v, stderr %q", code, done, errOut.String())
	}
}

// A title answered on this machine does not ask the archive for a count: this
// machine's sessions come first, with no network.
func TestHandoffTitleInScopeOnThisMachineNeverOpensTheArchive(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	opens := takeArchiveOffline(&f.env)
	out, errOut, code := runHandoff(t, f.env, "not uploaded")
	if code != 0 || !strings.Contains(out, "Not uploaded yet") || *opens != 0 {
		t.Fatalf("code=%d opens=%d stderr=%s", code, *opens, errOut)
	}
	if strings.Contains(errOut, "more in other projects") {
		t.Fatalf("a count of matches nobody searched for:\n%s", errOut)
	}
}

// The dot follows activeSourceWindow, the window that makes handoff ask about
// a shared checkout: a session this machine saw active within it is live, and one
// that was not, or that this machine does not run, is not.
func TestHandoffRowsMarkLiveSessionsByActiveSourceWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	row := func(active time.Duration, registered bool) handoffPickerRow {
		at := now.Add(-active)
		return handoffPickerRow{metadata: archive.Metadata{SessionID: "live0001", Title: "A task", CapturedAt: at}, active: at, registered: registered}
	}
	for _, tc := range []struct {
		name string
		row  handoffPickerRow
		live bool
	}{
		{"just now", row(10*time.Second, true), true},
		{"the last moment of the window", row(activeSourceWindow, true), true},
		{"a second past the window", row(activeSourceWindow+time.Second, true), false},
		{"long ago", row(3*time.Hour, true), false},
		{"archived only, captured just now", row(0, false), false},
		{"never active", handoffPickerRow{metadata: archive.Metadata{SessionID: "live0002"}, registered: true}, false},
	} {
		got := formatHandoffRows([]handoffPickerRow{tc.row}, listFormatOptions{Now: now})
		if got[0].Live != tc.live {
			t.Errorf("%s: Live = %v, want %v", tc.name, got[0].Live, tc.live)
		}
	}
}

// Through the picker, a live session's WHEN reads "live" in place of its
// time, and no title carries a mark for it.
func TestHandoffPickerDrawsTheLiveDot(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	now := f.env.now()
	live := f.addSession(t, "codex", "native-live", "Working right now", now.Add(-30*time.Second))
	f.addSession(t, "codex", "native-idle", "Waiting on me", now.Add(-3*time.Minute))
	out, errOut, code := runPicker(t, f.env, "q\n")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if fields := strings.Fields(pickerLine(t, out, live)); len(fields) < 2 || fields[1] != liveWhen || !strings.Contains(strings.Join(fields, " "), " Working right now") {
		t.Fatalf("live row: %q", fields)
	}
	if strings.Count(out, " "+liveWhen+" ") != 1 || strings.Contains(out, "●") {
		t.Fatalf("one live row, no dot:\n%s", out)
	}
	if fields := strings.Fields(pickerLine(t, out, f.archiveOnly)); len(fields) < 2 || fields[1] == liveWhen {
		t.Fatalf("idle row: %q", fields)
	}
}
