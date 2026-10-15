package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// formattedRows formats sessions as list does, at pickerNow.
func formattedRows(sessions []archive.Metadata, format listFormatOptions) []listRow {
	format.Now = pickerNow
	return formatSessionRows(sessions, format)
}

// HARNESS and PROJECT are left out when every row has the same value, and
// named for the heading; PR appears only when a row has one.
func TestColumnsAreHiddenWhenConstant(t *testing.T) {
	t.Parallel()
	same := pickerSessions(3, oneProject)
	laid, constants := listFormatOptions{GroupByProject: true, Numbered: true}.withColumns(formattedRows(same, listFormatOptions{}))
	if !laid.HideHarness || !laid.HideProject || laid.GroupByProject || laid.ShowPR || strings.Join(constants, "|") != "app|codex" {
		t.Fatalf("one project, one harness: %+v, constants %q", laid, constants)
	}

	varied := pickerSessions(3, func(i int) string { return []string{"app", "web", "app"}[i] })
	varied[1].Harness.Name = "claude"
	laid, constants = listFormatOptions{GroupByProject: true}.withColumns(formattedRows(varied, listFormatOptions{}))
	if laid.HideHarness || laid.HideProject || !laid.GroupByProject || len(constants) != 0 {
		t.Fatalf("varied rows: %+v, constants %q", laid, constants)
	}

	// Each column is judged on its own.
	varied[1].Harness.Name = "codex"
	laid, constants = listFormatOptions{}.withColumns(formattedRows(varied, listFormatOptions{}))
	if !laid.HideHarness || laid.HideProject || strings.Join(constants, "|") != "codex" {
		t.Fatalf("one harness, two projects: %+v, constants %q", laid, constants)
	}

	// A single row shares its value with nothing, and the verbose table keeps
	// every column.
	laid, _ = listFormatOptions{}.withColumns(formattedRows(same[:1], listFormatOptions{}))
	if laid.HideHarness || laid.HideProject {
		t.Fatalf("one row: %+v", laid)
	}
	laid, constants = listFormatOptions{Verbose: true}.withColumns(formattedRows(same, listFormatOptions{}))
	if laid.HideHarness || laid.HideProject || len(constants) != 0 {
		t.Fatalf("verbose: %+v, constants %q", laid, constants)
	}

	// An unknown project is not worth naming.
	unknown := pickerSessions(2, func(int) string { return "" })
	for i := range unknown {
		unknown[i].ProjectID = ""
	}
	if laid, constants = (listFormatOptions{}).withColumns(formattedRows(unknown, listFormatOptions{})); !laid.HideProject || strings.Join(constants, "|") != "codex" {
		t.Fatalf("unknown project: %+v, constants %q", laid, constants)
	}
}

// The table draws the columns it was laid out with.
func TestTableLeavesOutHiddenColumns(t *testing.T) {
	t.Parallel()
	sessions := pickerSessions(2, oneProject)
	sessions[0].GitActivity = []archive.GitEvent{{Kind: archive.GitEventPRCreated, PRNumber: 213}}
	rows := formattedRows(sessions, listFormatOptions{})
	format, _ := listFormatOptions{Numbered: true}.withColumns(rows)
	var out bytes.Buffer
	if err := printSessionTable(&out, rows, format); err != nil {
		t.Fatal(err)
	}
	header, _, _ := strings.Cut(out.String(), "\n")
	if fields := strings.Fields(header); strings.Join(fields, " ") != "# WHEN PR ID TITLE" {
		t.Fatalf("header %q\n%s", header, out.String())
	}
	if !strings.Contains(out.String(), "#213") {
		t.Fatalf("no PR:\n%s", out.String())
	}
	// With no PR on screen there is no PR column.
	rows = formattedRows(pickerSessions(2, oneProject), listFormatOptions{})
	format, _ = listFormatOptions{Numbered: true}.withColumns(rows)
	out.Reset()
	if err := printSessionTable(&out, rows, format); err != nil {
		t.Fatal(err)
	}
	if header, _, _ := strings.Cut(out.String(), "\n"); strings.Contains(header, "PR") {
		t.Fatalf("header %q", header)
	}
}

// The PR column is the last pull request the session linked, else the last it
// created (git_activity).
func TestPRColumnIsTheLastLinkedOrCreatedPullRequest(t *testing.T) {
	t.Parallel()
	at := func(kind archive.GitEventKind, n int) archive.GitEvent {
		return archive.GitEvent{Kind: kind, PRNumber: n}
	}
	for _, tc := range []struct {
		name   string
		events []archive.GitEvent
		linked []archive.PullRequestLink
		want   string
	}{
		{"none", nil, nil, ""},
		{"a commit and a push", []archive.GitEvent{{Kind: archive.GitEventCommit}, {Kind: archive.GitEventPush}}, nil, ""},
		{"one created", []archive.GitEvent{at(archive.GitEventPRCreated, 213)}, nil, "#213"},
		{"the last created wins", []archive.GitEvent{at(archive.GitEventPRCreated, 7), at(archive.GitEventCommit, 0), at(archive.GitEventPRCreated, 213)}, nil, "#213"},
		{"a merge is not a creation", []archive.GitEvent{at(archive.GitEventPRCreated, 7), at(archive.GitEventPRMerged, 9)}, nil, "#7"},
		{"a linked one", nil, []archive.PullRequestLink{{Repository: "o/r", Number: 41}}, "#41"},
		{"the last linked wins", nil, []archive.PullRequestLink{{Repository: "o/r", Number: 41}, {Repository: "o/r", Number: 52}}, "#52"},
		{"a linked one beats a created one", []archive.GitEvent{at(archive.GitEventPRCreated, 213)}, []archive.PullRequestLink{{Repository: "o/r", Number: 41}}, "#41"},
	} {
		m := archive.Metadata{SessionID: "pr000001", GitActivity: tc.events, PullRequests: tc.linked}
		if got := formattedRows([]archive.Metadata{m}, listFormatOptions{})[0].PR; got != tc.want {
			t.Errorf("%s: PR %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The scope can be switched only when there is one to go back to.
func TestScopeChoicesSwitchOnlyWhenThereIsAScope(t *testing.T) {
	t.Parallel()
	rowsFor := func(s sessionScope) scopeView {
		sessions := pickerSessions(4, func(i int) string { return []string{"app", "app", "web", "web"}[i] })
		shown := s.filter(sessions)
		return scopeView{rows: formattedRows(shown, listFormatOptions{Numbered: true}), total: len(shown)}
	}
	app := sessionScope{Label: "app", Dir: "/w/app", ProjectIDs: []string{"id-app"}}
	c := newScopeChoices(app, listFormatOptions{}, false, rowsFor)
	if !c.canToggle() || len(c.shown().rows) != 2 || !strings.HasSuffix(c.shown().heading, "a all projects") {
		t.Fatalf("scope: %+v", c.shown())
	}
	c.toggle()
	if len(c.shown().rows) != 4 || !strings.HasPrefix(c.shown().heading, "All projects · 4 sessions") || !strings.HasSuffix(c.shown().heading, "a app") {
		t.Fatalf("all projects: %+v", c.shown())
	}
	c.toggle()
	if len(c.shown().rows) != 2 {
		t.Fatalf("back to the scope: %+v", c.shown())
	}

	// --all-projects opens on everything and can go back.
	c = newScopeChoices(app.everything(), listFormatOptions{}, false, rowsFor)
	if len(c.shown().rows) != 4 || !c.canToggle() {
		t.Fatalf("--all-projects: %+v", c.shown())
	}

	// A scope with nothing in it falls back, and has nowhere to switch to.
	nothing := sessionScope{Label: "gone", Dir: "/w/gone", ProjectIDs: []string{"id-gone"}}
	c = newScopeChoices(nothing, listFormatOptions{}, false, rowsFor)
	if c.canToggle() || len(c.shown().rows) != 4 || !strings.HasPrefix(c.shown().heading, "Nothing in gone · showing all projects · 4 sessions") {
		t.Fatalf("fallback: %+v", c.shown())
	}
	c.toggle()
	if len(c.shown().rows) != 4 {
		t.Fatal("toggled past a fallback")
	}

	// A name with a newline or an escape sequence is one clean row.
	odd := sessionScope{Label: "a\npp\x1b[31m", Dir: "/w/app", ProjectIDs: []string{"id-app"}}
	c = newScopeChoices(odd, listFormatOptions{}, false, rowsFor)
	if h := c.shown().heading; strings.ContainsAny(h, "\n\x1b") || !strings.HasPrefix(h, "a pp") {
		t.Fatalf("heading %q", h)
	}

	// Outside any project there is no scope to name or switch; the heading
	// only names the column the rows share.
	c = newScopeChoices(sessionScope{All: true}, listFormatOptions{}, false, rowsFor)
	if c.canToggle() || c.shown().heading != "4 sessions · codex" || len(c.shown().rows) != 4 {
		t.Fatalf("no scope: %+v", c.shown())
	}
}

// goldenSessions are five sessions of agent-archive on Claude Code, two of
// which created a pull request. The content is invented.
func goldenSessions() []archive.Metadata {
	titles := []string{
		"Fix the retry budget in the upload queue",
		"Stop rereading the config on every hook call",
		"Add a status line for paused collection",
		"Investigate slow listings on a large archive",
		"Rename the export flag and update the docs",
	}
	prs := []int{213, 0, 212, 0, 204}
	sessions := make([]archive.Metadata, len(titles))
	for i, title := range titles {
		sessions[i] = archive.Metadata{
			SessionID:   []string{"d7a77938", "36a7d5ee", "76941c69", "5b0c1e22", "9f3a64d0"}[i] + strings.Repeat("0", 24),
			Title:       title,
			Harness:     archive.Harness{Name: "claude"},
			ProjectName: "agent-archive",
			ProjectID:   "project-agent-archive",
			CapturedAt:  pickerNow.Add(-[]time.Duration{30 * time.Second, 4 * time.Minute, 38 * time.Minute, 5 * time.Hour, 50 * time.Hour}[i]),
		}
		if prs[i] > 0 {
			sessions[i].GitActivity = []archive.GitEvent{{Kind: archive.GitEventPRCreated, PRNumber: prs[i]}}
		}
	}
	return sessions
}

// Regenerate with `go test ./internal/cli -run TestScopedListGolden -update`
// and review the diff.
func TestScopedListGolden(t *testing.T) {
	t.Parallel()
	sessions := goldenSessions()
	scope := sessionScope{Label: "agent-archive", RepoKey: scopeKey, Dir: "/w/agent-archive", ProjectIDs: []string{"project-agent-archive"}}
	sessions = append(sessions, archive.Metadata{SessionID: "bill0001" + strings.Repeat("0", 24), Title: "Invoice export", Harness: archive.Harness{Name: "claude"},
		ProjectName: "billing", ProjectID: "project-billing", CapturedAt: pickerNow.Add(-time.Hour)})
	format := listFormatOptions{Now: pickerNow, GroupByProject: true}
	choices := newScopeChoices(scope, format, true, archiveRows(sessions, defaultListLimit, format))
	var out bytes.Buffer
	if err := printListTable(&out, choices.shown(), 0); err != nil {
		t.Fatal(err)
	}
	golden.Check(t, filepath.Join("testdata", "browse", "list-scoped.txt"), out.Bytes())
}

// Regenerate with `go test ./internal/cli -run TestScopedHandoffGolden -update`
// and review the diff.
func TestScopedHandoffGolden(t *testing.T) {
	t.Parallel()
	sessions := goldenSessions()
	scope := sessionScope{Label: "agent-archive", RepoKey: scopeKey, Dir: "/w/agent-archive", ProjectIDs: []string{"project-agent-archive"}}
	format := listFormatOptions{Now: pickerNow, GroupByProject: true, Numbered: true, NarrowHint: "Narrow with --harness, or name a session: agent-archive handoff SESSION_ID."}
	rows := make([]handoffPickerRow, len(sessions))
	for i, m := range sessions {
		// The first two are registered here, the first active just now; the
		// last is on this machine only.
		rows[i] = handoffPickerRow{metadata: m, active: m.CapturedAt, registered: i < 2, notUploaded: i == 4}
	}
	choices := newScopeChoices(scope, format, false, func(sessionScope) scopeView {
		return scopeView{rows: formatHandoffRows(rows, format), total: len(rows)}
	})
	var out bytes.Buffer
	picker := &sessionPicker{env: fixedTerminal{100, 30}}
	if _, _, err := picker.pickScoped(newPrompter(strings.NewReader("q\n"), &out), &out, choices, "hand off"); err != nil {
		t.Fatal(err)
	}
	golden.Check(t, filepath.Join("testdata", "browse", "handoff-scoped.txt"), out.Bytes())
}
