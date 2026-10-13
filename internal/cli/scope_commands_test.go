package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// scopedArchive is a published archive of one configured project, run in (its
// working directory), and sessions of other projects cloned from the
// project's own sidecar, so every field is one the filter wrote.
type scopedArchive struct {
	env   Env
	mem   *storagetest.MemoryStore
	dir   string
	label string
	// id is the project's own session.
	id   string
	base archive.Metadata
}

func newScopedArchive(t *testing.T) scopedArchive {
	t.Helper()
	env, mem, id := publishedFixture(t)
	home, err := env.Home()
	if err != nil {
		t.Fatal(err)
	}
	reg, found, err := state.OpenReadOnly(home).LoadRegistration(id)
	if err != nil || !found {
		t.Fatalf("registration: %v %v", found, err)
	}
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mem.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var base archive.Metadata
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatal(err)
	}
	env.WorkingDir = func() (string, error) { return reg.ProjectRoot, nil }
	return scopedArchive{env: env, mem: mem, dir: reg.ProjectRoot, label: filepath.Base(reg.ProjectRoot), id: id, base: base}
}

// add publishes a session of project (by name), one hour older than the
// last, unless change sets its time. A project named after the working
// directory's gets its project ID.
func (a scopedArchive) add(t *testing.T, id, title, project string, change ...func(*archive.Metadata)) {
	t.Helper()
	a.addMetadata(t, id, title, project, change...)
	// A listing reads the newest sessions from the index, which sync wrote.
	if _, err := reader.RebuildIndex(context.Background(), a.mem, archiveSessionsPrefix); err != nil {
		t.Fatal(err)
	}
}

// addMetadata writes the same sidecar as add. Bulk fixtures rebuild their
// listing index once after all sidecars are present.
func (a scopedArchive) addMetadata(t *testing.T, id, title, project string, change ...func(*archive.Metadata)) {
	t.Helper()
	m := a.base
	m.SessionID, m.Title, m.ProjectName = id, title, project
	m.ProjectID = archive.ProjectID("/elsewhere/" + project)
	if project == a.label {
		m.ProjectID = archive.ProjectID(a.dir)
	}
	m.CapturedAt = a.base.CapturedAt.Add(-time.Hour)
	for _, c := range change {
		c(&m)
	}
	key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.mem.Put(context.Background(), key, encoded); err != nil {
		t.Fatal(err)
	}
}

// standard holds, besides the project's own session, one more of its own and
// two of another project.
func (a scopedArchive) standard(t *testing.T) {
	t.Helper()
	a.add(t, "mine0002", "Second task here", a.label)
	a.add(t, "bill0001", "Invoice export", "billing")
	a.add(t, "bill0002", "Refund rules", "billing")
}

// addProject configures another project root, which has no sessions.
func (a scopedArchive) addProject(t *testing.T, root string) {
	t.Helper()
	home, err := a.env.Home()
	if err != nil {
		t.Fatal(err)
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found {
		t.Fatalf("config: %v %v", found, err)
	}
	cfg.Archive.Projects = append(cfg.Archive.Projects, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: true, ActivatedAt: time.Unix(0, 0)})
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
}

// runList runs list piped, and returns what it printed.
func (a scopedArchive) runList(t *testing.T, args ...string) (out, errOut string, code int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code = Run(append([]string{"list"}, args...), nil, &stdout, &stderr, a.env)
	return stdout.String(), stderr.String(), code
}

// namedIDs lists which of ids a listing names.
func namedIDs(out string, ids ...string) []string {
	var named []string
	for _, id := range ids {
		if strings.Contains(out, id[:minShortSessionID]) {
			named = append(named, id)
		}
	}
	return named
}

func sameStrings(got, want []string) bool { return strings.Join(got, ",") == strings.Join(want, ",") }

// Run in a project, list shows that project's sessions under a heading that
// names it, leaves out the columns every row shares, and says how to see the
// rest. --all-projects lists everything.
func TestListInsideAProjectListsThatProjectsSessions(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.standard(t)
	all := []string{a.id, "mine0002", "bill0001", "bill0002"}
	out, errOut, code := a.runList(t)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if got := namedIDs(out, all...); !sameStrings(got, []string{a.id, "mine0002"}) {
		t.Fatalf("listed %v:\n%s", got, out)
	}
	heading, _, _ := strings.Cut(out, "\n")
	if want := a.label + " · 2 sessions · codex · --all-projects lists every project"; heading != want {
		t.Fatalf("heading %q, want %q", heading, want)
	}
	if strings.Contains(out, "PROJECT") || strings.Contains(out, "HARNESS") {
		t.Fatalf("a column every row shares is shown:\n%s", out)
	}

	out, _, _ = a.runList(t, "--all-projects")
	if got := namedIDs(out, all...); !sameStrings(got, all) {
		t.Fatalf("--all-projects listed %v:\n%s", got, out)
	}
	if heading, _, _ := strings.Cut(out, "\n"); heading != "All projects · 4 sessions · codex" {
		t.Fatalf("--all-projects heading %q", heading)
	}
	if !strings.Contains(out, "PROJECT") || strings.Contains(out, "HARNESS") {
		t.Fatalf("columns:\n%s", out)
	}
}

// Outside any project, every session shows, grouped by project as before.
func TestListOutsideAnyProjectShowsEverySession(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.standard(t)
	elsewhere := t.TempDir()
	a.env.WorkingDir = func() (string, error) { return elsewhere, nil }
	out, errOut, code := a.runList(t)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	all := []string{a.id, "mine0002", "bill0001", "bill0002"}
	if got := namedIDs(out, all...); !sameStrings(got, all) {
		t.Fatalf("listed %v:\n%s", got, out)
	}
	for _, heading := range []string{"All projects", "--all-projects"} {
		if strings.Contains(out, heading) {
			t.Fatalf("a scope is named outside any project:\n%s", out)
		}
	}
	if !strings.Contains(out, "billing (2)") || !strings.Contains(out, a.label+" (2)") {
		t.Fatalf("not grouped by project:\n%s", out)
	}
}

// --project takes a directory or a project name, and is refused with
// --all-projects.
func TestListProjectTakesADirectoryOrAName(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.standard(t)
	billing := filepath.Join(t.TempDir(), "billing")
	a.addProject(t, billing)
	a.add(t, "bill0003", "Fresh billing work", "billing", func(m *archive.Metadata) { m.ProjectID = archive.ProjectID(billing) })
	for _, project := range []string{"billing", "BILLING"} {
		out, errOut, code := a.runList(t, "--project", project)
		if code != 0 {
			t.Fatalf("--project %s: code=%d stderr=%s", project, code, errOut)
		}
		if got := namedIDs(out, a.id, "mine0002", "bill0001", "bill0002", "bill0003"); !sameStrings(got, []string{"bill0001", "bill0002", "bill0003"}) {
			t.Fatalf("--project %s listed %v:\n%s", project, got, out)
		}
	}
	// A directory: its sessions by project ID (no origin remote here).
	if err := os.MkdirAll(billing, 0o755); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := a.runList(t, "--project", billing)
	if code != 0 || !sameStrings(namedIDs(out, "bill0001", "bill0003"), []string{"bill0003"}) {
		t.Fatalf("--project DIR: code=%d stderr=%s\n%s", code, errOut, out)
	}
	if out, errOut, code := a.runList(t, "--project", "billing", "--all-projects"); code != 2 || out != "" || !strings.Contains(errOut, "--project and --all-projects") {
		t.Fatalf("--project with --all-projects: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

// A scope that holds nothing falls back to all projects, and the heading
// says so.
func TestListWithNothingInTheScopeFallsBackAndSaysSo(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.standard(t)
	empty := filepath.Join(t.TempDir(), "empty-project")
	a.addProject(t, empty)
	a.env.WorkingDir = func() (string, error) { return empty, nil }
	out, errOut, code := a.runList(t)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	heading, _, _ := strings.Cut(out, "\n")
	if want := "Nothing in empty-project · showing all projects · 4 sessions · codex"; heading != want {
		t.Fatalf("heading %q, want %q", heading, want)
	}
	if got := namedIDs(out, a.id, "mine0002", "bill0001", "bill0002"); len(got) != 4 {
		t.Fatalf("listed %v", got)
	}
}

// list --json carries a scope object, and only that scope's sessions.
func TestListJSONCarriesTheScope(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.standard(t)
	read := func(args ...string) listDocument {
		t.Helper()
		out, errOut, code := a.runList(t, append([]string{"--json"}, args...)...)
		if code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errOut)
		}
		var doc listDocument
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return doc
	}
	ids := func(doc listDocument) []string {
		var ids []string
		for _, m := range doc.Sessions {
			ids = append(ids, m.SessionID)
		}
		return ids
	}

	doc := read()
	want := listScope{Label: a.label, OutsideMatches: 2}
	if doc.Version != 4 || doc.Scope == nil || *doc.Scope != want || len(doc.Sessions) != 2 || doc.Returned != 2 {
		t.Fatalf("scoped: version %d, scope %+v, sessions %v", doc.Version, doc.Scope, ids(doc))
	}
	raw, _, _ := a.runList(t, "--json")
	if !strings.Contains(raw, `"scope": {`) || !strings.Contains(raw, `"outside_matches": 2`) || !strings.Contains(raw, `"all_projects": false`) || !strings.Contains(raw, `"fell_back": false`) {
		t.Fatalf("scope object in the document:\n%s", raw)
	}

	doc = read("--all-projects")
	if doc.Scope == nil || *doc.Scope != (listScope{Label: a.label, AllProjects: true}) || len(doc.Sessions) != 4 {
		t.Fatalf("--all-projects: scope %+v, sessions %v", doc.Scope, ids(doc))
	}

	// --limit counts the scope's sessions.
	if doc = read("--limit", "1"); len(doc.Sessions) != 1 || doc.TotalMatched == nil || *doc.TotalMatched != 2 || !doc.Truncated {
		t.Fatalf("--limit 1: %+v", doc)
	}

	empty := filepath.Join(t.TempDir(), "empty-project")
	a.addProject(t, empty)
	a.env.WorkingDir = func() (string, error) { return empty, nil }
	doc = read()
	if doc.Scope == nil || *doc.Scope != (listScope{Label: "empty-project", AllProjects: true, FellBack: true}) || len(doc.Sessions) != 4 {
		t.Fatalf("fallback: scope %+v, sessions %v", doc.Scope, ids(doc))
	}

	// Outside any project there is no scope to report.
	elsewhere := t.TempDir()
	a.env.WorkingDir = func() (string, error) { return elsewhere, nil }
	doc = read()
	if doc.Scope != nil || len(doc.Sessions) != 4 {
		t.Fatalf("outside a project: scope %+v, sessions %v", doc.Scope, ids(doc))
	}
	if raw, _, _ := a.runList(t, "--json"); strings.Contains(raw, `"scope"`) {
		t.Fatalf("scope present outside a project:\n%s", raw)
	}
}

// Two worktrees of one repository are one scope: a session captured in
// another checkout of the repository is listed, and one of another repository
// is not, whatever its project.
func TestListScopeSpansCheckoutsOfOneRepository(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.env.repoKey = func(root string) string {
		if root == a.dir {
			return scopeKey
		}
		return ""
	}
	a.add(t, "tree0001", "Worktree task", "app-pr4", func(m *archive.Metadata) { m.RepoKey = scopeKey })
	a.add(t, "else0001", "Same folder, other repository", a.label, func(m *archive.Metadata) { m.RepoKey = otherScopeKey })
	out, errOut, code := a.runList(t, "--json")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	var doc listDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range doc.Sessions {
		got = append(got, m.SessionID)
	}
	// The project's own session has no repository key (an older one), so it
	// is in scope by its project ID.
	if !sameStrings(got, []string{a.id, "tree0001"}) {
		t.Fatalf("scoped sessions %v", got)
	}
}

// Run in a worktree called pr4, every view names the scope after the
// repository's main checkout, whatever sessions it read: the scoped and
// --all-projects documents (the latter from the index's newest page, which
// here holds none of the repository's sessions), one whose filters leave the
// scope empty, and both headings of the browser, before and after a switches
// the scope.
func TestScopeIsNamedAfterItsProjectInEveryView(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	root := t.TempDir()
	main := gitCheckout(t, filepath.Join(root, a.label))
	worktree := gitWorktree(t, main, filepath.Join(root, "pr4"))
	a.env.WorkingDir = func() (string, error) { return worktree, nil }
	a.env.repoKey = func(root string) string {
		if root == worktree {
			return scopeKey
		}
		return ""
	}
	inRepo := func(m *archive.Metadata) { m.RepoKey = scopeKey }
	a.add(t, "mine0002", "Second task here", a.label, inRepo)
	a.add(t, "mine0003", "Third task here", a.label, inRepo)
	a.add(t, "bill0001", "Invoice export", "billing")
	for _, args := range [][]string{{"--json"}, {"--json", "--all-projects"}, {"--json", "--limit", "1"}, {"--json", "--all-projects", "--limit", "1"}, {"--json", "--harness", "claude"}} {
		out, errOut, code := a.runList(t, args...)
		if code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errOut)
		}
		var doc listDocument
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Scope == nil || doc.Scope.Label != a.label {
			t.Fatalf("%v: scope %+v, want the label %q", args, doc.Scope, a.label)
		}
	}
	var headings []string
	for _, screen := range a.browserRun(t, []string{"a", "a", "q"}, "list", "--all-projects") {
		if strings.Contains(screen, "type a number") {
			headings = append(headings, headingOf(screen))
		}
	}
	want := []string{
		"All projects · 4 sessions · codex · a " + a.label,
		a.label + " · 2 sessions · codex · a all projects",
		"All projects · 4 sessions · codex · a " + a.label,
	}
	if strings.Join(headings, "\n") != strings.Join(want, "\n") {
		t.Fatalf("headings\n%s\nwant\n%s", strings.Join(headings, "\n"), strings.Join(want, "\n"))
	}
}

// browserRun runs a command on a terminal that reads keys from chunks, and
// returns each screen it drew.
func (a scopedArchive) browserRun(t *testing.T, chunks []string, args ...string) []string {
	t.Helper()
	fake := newFakeKeys(chunks...)
	stdin := strings.NewReader("")
	var stdout, stderr bytes.Buffer
	env := a.env
	env.TerminalSize = fixedTerminal{120, 30}.terminalSize
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&stdout) }
	env.openKeys = func(io.Reader) (keyTerminal, bool) { return fake, true }
	if code := Run(args, stdin, &stdout, &stderr, env); code != 0 {
		t.Fatalf("%v: code=%d stderr=%s", args, code, stderr.String())
	}
	var screens []string
	for screen := range strings.SplitSeq(stdout.String(), clearScreenSequence) {
		screen, _, _ = strings.Cut(screen, leaveAltScreenSequence)
		screens = append(screens, screen)
	}
	return screens
}

// headingOf is the first line of a screen without its escape sequences.
func headingOf(screen string) string {
	screen = strings.TrimPrefix(screen, enterAltScreenSequence)
	line, _, _ := strings.Cut(screen, "\n")
	return line
}

// a, typed alone, switches between the scope and all projects in the browser,
// whichever command opened it, and the heading always names what is shown and
// the other choice.
func TestKeyBrowserTogglesTheScopeWithA(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.standard(t)
	for _, args := range [][]string{{"list"}, {"show"}} {
		screens := a.browserRun(t, []string{"a", "a", "q"}, args...)
		var headings []string
		for _, screen := range screens {
			if strings.Contains(screen, "sessions") && strings.Contains(screen, "type a number") {
				headings = append(headings, headingOf(screen))
			}
		}
		want := []string{
			a.label + " · 2 sessions · codex · a all projects",
			"All projects · 4 sessions · codex · a " + a.label,
			a.label + " · 2 sessions · codex · a all projects",
		}
		if strings.Join(headings, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%v: headings\n%s\nwant\n%s", args, strings.Join(headings, "\n"), strings.Join(want, "\n"))
		}
		// Everything is offered on the second screen, and only the scope on
		// the first.
		if !strings.Contains(screens[1], "bill0001") && !strings.Contains(screens[2], "bill0001") {
			t.Fatalf("%v: all projects lack a session of another project:\n%s", args, strings.Join(screens, "\n=====\n"))
		}
		if strings.Contains(screens[0], "bill0001") {
			t.Fatalf("%v: the scope offers a session of another project:\n%s", args, screens[0])
		}
	}
	// A typed "a" after other characters is part of what is typed.
	screens := a.browserRun(t, []string{"1", "a", "q"}, "list")
	for _, screen := range screens {
		if strings.Contains(headingOf(screen), "All projects") {
			t.Fatalf("a typed after a digit switched the scope:\n%s", screen)
		}
	}
}

// Outside any project there is no scope to switch, so a is typed like any
// other character.
func TestKeyBrowserHasNoTogglesOutsideAnyProject(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.standard(t)
	elsewhere := t.TempDir()
	a.env.WorkingDir = func() (string, error) { return elsewhere, nil }
	screens := a.browserRun(t, []string{"a", "q"}, "list")
	for _, screen := range screens {
		if strings.Contains(screen, "All projects") || strings.Contains(screen, "a all projects") {
			t.Fatalf("a scope is offered outside any project:\n%s", screen)
		}
	}
}

// With nothing in the scope the browser opens on all projects, says so, and
// has no scope to switch to.
func TestKeyBrowserFallsBackWhenTheScopeIsEmpty(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.standard(t)
	empty := filepath.Join(t.TempDir(), "empty-project")
	a.addProject(t, empty)
	a.env.WorkingDir = func() (string, error) { return empty, nil }
	screens := a.browserRun(t, []string{"a", "q"}, "list")
	var headings []string
	for _, screen := range screens {
		if strings.Contains(screen, "type a number") {
			headings = append(headings, headingOf(screen))
		}
	}
	want := "Nothing in empty-project · showing all projects · 4 sessions · codex"
	if len(headings) == 0 || headings[0] != want {
		t.Fatalf("headings %q, want %q first", headings, want)
	}
	for _, h := range headings {
		if h != want {
			t.Fatalf("the heading changed after a: %q", h)
		}
	}
}

// A directory named with --project is the scope even when it is in no
// project: it holds nothing, and the listing says so and shows everything.
func TestListProjectDirectoryInNoProjectFallsBack(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.standard(t)
	notes := filepath.Join(t.TempDir(), "notes")
	if err := os.MkdirAll(notes, 0o755); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := a.runList(t, "--project", notes)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if heading, _, _ := strings.Cut(out, "\n"); heading != "Nothing in notes · showing all projects · 4 sessions · codex" {
		t.Fatalf("heading %q", heading)
	}
}

// Opened on all projects with --all-projects, a when the scope holds nothing
// stays on all projects and says so, instead of leaving the browser.
func TestKeyBrowserToggleToAnEmptyScopeFallsBack(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.standard(t)
	empty := filepath.Join(t.TempDir(), "empty-project")
	a.addProject(t, empty)
	a.env.WorkingDir = func() (string, error) { return empty, nil }
	var headings []string
	for _, screen := range a.browserRun(t, []string{"a", "q"}, "list", "--all-projects") {
		if strings.Contains(screen, "type a number") {
			headings = append(headings, headingOf(screen))
		}
	}
	want := []string{
		"All projects · 4 sessions · codex · a empty-project",
		"Nothing in empty-project · showing all projects · 4 sessions · codex",
	}
	if strings.Join(headings, "\n") != strings.Join(want, "\n") {
		t.Fatalf("headings\n%s\nwant\n%s", strings.Join(headings, "\n"), strings.Join(want, "\n"))
	}
}
