package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// linked is a change that gives a session the pull requests it was linked to.
func linked(numbers ...int) func(*archive.Metadata) {
	return func(m *archive.Metadata) {
		for _, n := range numbers {
			m.PullRequests = append(m.PullRequests, archive.PullRequestLink{Repository: "wangjohn/agent-archive", Number: n})
		}
	}
}

// subagentOf makes a session a subagent of parent.
func subagentOf(parent string) func(*archive.Metadata) {
	return func(m *archive.Metadata) { m.ParentSessionID = parent }
}

// all runs each change, in order.
func all(changes ...func(*archive.Metadata)) func(*archive.Metadata) {
	return func(m *archive.Metadata) {
		for _, change := range changes {
			change(m)
		}
	}
}

func named(name string) func(*archive.Metadata) {
	return func(m *archive.Metadata) { m.Name = name }
}

func onBranch(branch string) func(*archive.Metadata) {
	return func(m *archive.Metadata) { m.Branch = branch }
}

// list "<words>" matches every word in some field: the name, the branch, the
// project, a PR, and the harness, case-insensitively.
func TestListWordsMatchAcrossFields(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "linux001", "Add a systemd unit", a.label, all(named("Implement Linux support"), linked(212), onBranch("feature/linux-port")))
	a.add(t, "linux002", "Notes on Linux", a.label, named("Linux notes"))
	a.add(t, "other001", "Unrelated", a.label)
	a.add(t, "bill0001", "Invoice export", "billing", all(named("Export totals"), linked(300)))
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"linux", []string{"linux001", "linux002"}},
		{"LINUX 212", []string{"linux001"}}, // name and PR
		{"#212", []string{"linux001"}},
		{"212 systemd", []string{"linux001"}},    // PR and title
		{"linux-port", []string{"linux001"}},     // branch
		{"linux windows", nil},                   // a word nothing has
		{"codex export", []string{"bill0001"}},   // harness and name, in another project
		{"#300", []string{"bill0001"}},           // a PR of another project, found everywhere
		{"billing totals", []string{"bill0001"}}, // project and name
	} {
		out, errOut, code := a.runList(t, tc.query)
		if code != 0 {
			t.Fatalf("%q: code=%d stderr=%s", tc.query, code, errOut)
		}
		if got := namedIDs(out, "linux001", "linux002", "other001", "bill0001"); !sameStrings(got, tc.want) {
			t.Errorf("%q listed %v, want %v:\n%s", tc.query, got, tc.want, out)
		}
	}
}

// An in-scope match shadows the others, and the footer counts the matches
// elsewhere; --all-projects, or a word naming the project, finds them.
func TestListSearchShadowsOutsideMatchesAndCountsThem(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "mine0001", "Shared words here", a.label)
	a.add(t, "bill0001", "Shared words invoice", "billing")
	a.add(t, "bill0002", "Shared words refunds", "billing")
	out, errOut, code := a.runList(t, "shared words")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if got := namedIDs(out, "mine0001", "bill0001", "bill0002"); !sameStrings(got, []string{"mine0001"}) {
		t.Fatalf("listed %v:\n%s", got, out)
	}
	note := "1 match in " + a.label + " (2 more in other projects: --all-projects or a project name finds them)"
	if !strings.HasSuffix(out, note+"\n") {
		t.Fatalf("no footer note %q:\n%s", note, out)
	}
	out, _, _ = a.runList(t, "shared words", "--all-projects")
	if got := namedIDs(out, "mine0001", "bill0001", "bill0002"); len(got) != 3 || strings.Contains(out, "more in other projects") {
		t.Fatalf("--all-projects listed %v:\n%s", got, out)
	}
	out, _, _ = a.runList(t, "shared words billing")
	if got := namedIDs(out, "mine0001", "bill0001", "bill0002"); !sameStrings(got, []string{"bill0001", "bill0002"}) {
		t.Fatalf("a project name listed %v:\n%s", got, out)
	}
	// With nothing in the scope, everywhere answers, under the fallback heading.
	out, _, _ = a.runList(t, "refunds")
	heading, _, _ := strings.Cut(out, "\n")
	if got := namedIDs(out, "mine0001", "bill0001", "bill0002"); !sameStrings(got, []string{"bill0002"}) || !strings.HasPrefix(heading, "Nothing in "+a.label+" · showing all projects") {
		t.Fatalf("listed %v, heading %q:\n%s", got, heading, out)
	}
}

// Subagents are the last tier: top-level sessions answer first, even from
// another project, and a subagent shows only when nothing top-level matches,
// labelled with its parent.
func TestListSubagentsAreTheLastTier(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "parent01", "Orchestrate the release", a.label)
	a.add(t, "sub00001", "Review and fix PR #208", a.label, subagentOf("parent01"))
	a.add(t, "sub00002", "Check the changelog", "billing", subagentOf("parent01"))
	a.add(t, "bill0001", "Review the invoice", "billing")

	// A top-level session elsewhere beats a subagent in scope.
	out, _, _ := a.runList(t, "review")
	if got := namedIDs(out, "parent01", "sub00001", "sub00002", "bill0001"); !sameStrings(got, []string{"bill0001"}) {
		t.Fatalf("review listed %v:\n%s", got, out)
	}
	// Only a subagent matches: found, with its parent named.
	out, errOut, code := a.runList(t, "208")
	if code != 0 || !sameStrings(namedIDs(out, "sub00001", "sub00002", "bill0001"), []string{"sub00001"}) {
		t.Fatalf("208: code=%d stderr=%s\n%s", code, errOut, out)
	}
	if !strings.Contains(out, "subagent of parent01") {
		t.Fatalf("a subagent is not labelled with its parent:\n%s", out)
	}
	// Subagents in scope come before subagents elsewhere.
	a.add(t, "sub00003", "Check the docs", a.label, subagentOf("parent01"))
	out, _, _ = a.runList(t, "check")
	if got := namedIDs(out, "sub00002", "sub00003"); !sameStrings(got, []string{"sub00003"}) {
		t.Fatalf("check listed %v:\n%s", got, out)
	}
	out, _, _ = a.runList(t, "check", "--all-projects")
	if got := namedIDs(out, "sub00002", "sub00003"); len(got) != 2 {
		t.Fatalf("--all-projects check listed %v:\n%s", got, out)
	}
}

// An exact session ID is the answer, whatever else contains it.
func TestListExactSessionIDWins(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "abcd1234", "The real one", a.label)
	a.add(t, "wxyz9999", "Notes about abcd1234", a.label)
	out, _, _ := a.runList(t, "abcd1234")
	if got := namedIDs(out, "abcd1234", "wxyz9999"); !sameStrings(got, []string{"abcd1234"}) {
		t.Fatalf("listed %v:\n%s", got, out)
	}
	out, _, _ = a.runList(t, "abcd12")
	if got := namedIDs(out, "abcd1234", "wxyz9999"); len(got) != 2 {
		t.Fatalf("a prefix is only a match: %v:\n%s", got, out)
	}
}

// The table hides subagents before --limit, counts them in the footer, and
// gives a parent a hint of how many it has; --json keeps every row.
func TestListHidesSubagentsAndJSONKeepsThem(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "parent01", "Orchestrate the release", a.label)
	a.add(t, "parent02", "A separate task", a.label)
	for i := range 3 {
		a.add(t, fmt.Sprintf("child%03d1", i), fmt.Sprintf("Child task %d", i), a.label, subagentOf("parent01"))
	}
	a.add(t, "child0042", "Child of the other", a.label, subagentOf("parent02"))
	ids := []string{a.id, "parent01", "parent02", "child0001", "child0011", "child0021", "child0042"}

	out, errOut, code := a.runList(t)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if got := namedIDs(out, ids...); !sameStrings(got, []string{a.id, "parent01", "parent02"}) {
		t.Fatalf("listed %v:\n%s", got, out)
	}
	if !strings.Contains(out, "3 sessions (4 subagent sessions hidden; search to find one).") {
		t.Fatalf("footer:\n%s", out)
	}
	if !strings.Contains(out, "Orchestrate the release · 3 subagents") || !strings.Contains(out, "A separate task · 1 subagent ") {
		t.Fatalf("the parents carry no hint:\n%s", out)
	}
	if strings.Contains(out, "Child task") {
		t.Fatalf("a subagent is listed:\n%s", out)
	}

	// --limit counts top-level rows, however many subagents are newer.
	out, _, _ = a.runList(t, "--limit", "2")
	if got := namedIDs(out, ids...); len(got) != 2 {
		t.Fatalf("--limit 2 listed %v:\n%s", got, out)
	}
	if !strings.Contains(out, "Showing 2 of 3 session(s) (4 subagent sessions hidden; search to find one).") {
		t.Fatalf("limited footer:\n%s", out)
	}

	// --json keeps every row, subagents included, and --limit counts them.
	out, errOut, code = a.runList(t, "--json")
	var doc listDocument
	if err := json.Unmarshal([]byte(out), &doc); code != 0 || err != nil {
		t.Fatalf("--json: code=%d err=%v stderr=%s\n%s", code, err, errOut, out)
	}
	if len(doc.Sessions) != 7 || doc.Returned != 7 {
		t.Fatalf("--json returned %d sessions, want 7", len(doc.Sessions))
	}
	children := 0
	for _, m := range doc.Sessions {
		if m.ParentSessionID != "" {
			children++
		}
	}
	if children != 4 {
		t.Fatalf("--json holds %d subagents, want 4", children)
	}
}

// list "<words>" --json is the same document as list --json, narrowed.
func TestListQueryJSONKeepsTheDocumentShape(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "mine0001", "Shared words here", a.label)
	a.add(t, "bill0001", "Shared words invoice", "billing")
	keys := func(args ...string) (map[string]any, listDocument) {
		t.Helper()
		out, errOut, code := a.runList(t, append([]string{"--json"}, args...)...)
		if code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errOut)
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			t.Fatal(err)
		}
		var doc listDocument
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatal(err)
		}
		return raw, doc
	}
	plain, _ := keys()
	searched, doc := keys("shared words")
	if got, want := slices.Sorted(maps.Keys(searched)), slices.Sorted(maps.Keys(plain)); !slices.Equal(got, want) {
		t.Fatalf("keys %v, want %v", got, want)
	}
	if doc.Version != listSchemaVersion || len(doc.Sessions) != 1 || doc.Sessions[0].SessionID != "mine0001" || doc.Returned != 1 {
		t.Fatalf("document: %+v", doc)
	}
	if doc.Scope == nil || doc.Scope.OutsideMatches != 1 || doc.Scope.FellBack || doc.Scope.AllProjects {
		t.Fatalf("scope: %+v", doc.Scope)
	}
	// Nothing in the scope: all projects answer, and the scope says so.
	_, doc = keys("invoice")
	if len(doc.Sessions) != 1 || doc.Sessions[0].SessionID != "bill0001" || doc.Scope == nil || !doc.Scope.FellBack {
		t.Fatalf("fallback: %+v %+v", doc.Sessions, doc.Scope)
	}
	// A query nothing matches is an empty document, not an error.
	_, doc = keys("no such words")
	if doc.Sessions == nil || len(doc.Sessions) != 0 || doc.Returned != 0 {
		t.Fatalf("empty: %+v", doc)
	}
	// With --json, a query narrows subagents like any session: the tier keeps them.
	a.add(t, "sub00001", "Reviewer of the release", a.label, subagentOf("mine0001"))
	if _, doc = keys("reviewer"); len(doc.Sessions) != 1 || doc.Sessions[0].SessionID != "sub00001" {
		t.Fatalf("subagent tier: %+v", doc.Sessions)
	}
}

// --limit applies after the tiers: a query's matches are cut to it.
func TestListQueryLimitCutsTheMatches(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	for i := range 4 {
		a.add(t, fmt.Sprintf("many000%d", i), fmt.Sprintf("Shared words %d", i), a.label)
	}
	out, _, _ := a.runList(t, "shared", "--limit", "2")
	if got := namedIDs(out, "many0000", "many0001", "many0002", "many0003"); len(got) != 2 || !strings.Contains(out, "Showing 2 of 4 session(s).") {
		t.Fatalf("listed %v:\n%s", got, out)
	}
}

// A blank query is a usage error; a query nothing matches says so.
func TestListQueryEdges(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	for _, blank := range []string{"   ", ""} {
		if out, errOut, code := a.runList(t, blank); code != 2 || out != "" || !strings.Contains(errOut, "search words are empty") {
			t.Fatalf("blank %q: code=%d stdout=%q stderr=%q", blank, code, out, errOut)
		}
	}
	if out, _, code := a.runList(t, "nothing like this"); code != 0 || !strings.Contains(out, `No archived sessions match "nothing like this".`) {
		t.Fatalf("no match: code=%d\n%s", code, out)
	}
	// Flags may follow the query.
	if _, errOut, code := a.runList(t, "words", "--limit", "1"); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

// On a terminal, list "<words>" opens the browser with the words already in
// its filter, over the matches; clearing the filter lists every session.
func TestListQueryOpensTheBrowserWithTheWordsInItsFilter(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "mine0001", "Shared words here", a.label)
	a.add(t, "mine0002", "Different thing", a.label)
	out, code := ttyRunAllowingInput(t, a.env, "\n\n", "list", "shared")
	if code != 0 || !strings.Contains(out, "Enter number") {
		t.Fatalf("code=%d\n%s", code, out)
	}
	screens := strings.Split(out, clearScreenSequence)
	if len(screens) < 3 {
		t.Fatalf("%d screens:\n%s", len(screens), out)
	}
	if got := namedIDs(screens[1], "mine0001", "mine0002"); !sameStrings(got, []string{"mine0001"}) || !strings.Contains(screens[1], `"shared" matches 1`) {
		t.Fatalf("opening screen shows %v:\n%s", got, screens[1])
	}
	if got := namedIDs(screens[2], "mine0001", "mine0002"); !sameStrings(got, []string{"mine0001", "mine0002"}) || strings.Contains(screens[2], "matches") {
		t.Fatalf("screen with the filter cleared shows %v:\n%s", got, screens[2])
	}
}

// runShow runs show piped, and returns what it printed.
func (a scopedArchive) runShow(t *testing.T, args ...string) (out, errOut string, code int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code = Run(append([]string{"show"}, args...), nil, &stdout, &stderr, a.env)
	return stdout.String(), stderr.String(), code
}

// show "<words>" with one in-scope match reads it and says how many match
// elsewhere; with none in scope, everywhere answers.
func TestShowWordsSearchTheScopeFirst(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "mine0001", "Shared words here", a.label)
	a.add(t, "bill0001", "Shared words invoice", "billing")
	a.add(t, "bill0002", "Only billing words", "billing")
	out, errOut, code := a.runShow(t, "shared words", "--json")
	if code != 0 || !strings.Contains(out, `"session_id": "mine0001"`) {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	note := "1 match in " + a.label + " (1 more in other projects: --all-projects or a project name finds them)"
	if strings.TrimSpace(errOut) != note {
		t.Fatalf("stderr %q, want %q", errOut, note)
	}
	out, errOut, code = a.runShow(t, "only billing", "--json")
	if code != 0 || !strings.Contains(out, `"session_id": "bill0002"`) || errOut != "" {
		t.Fatalf("everywhere: code=%d stderr=%s\n%s", code, errOut, out)
	}
}

// Several matches without a terminal print the shared candidate table, and
// the exact next command.
func TestShowSeveralMatchesPrintTheCandidateTable(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "wobbly01", "Fix wobbly retention test", a.label, linked(213))
	a.add(t, "wobbly02", "Fix wobbly hook-lock test", a.label)
	a.add(t, "wobbly03", "Reviewer for the wobbly fix", a.label, subagentOf("wobbly01"))
	out, errOut, code := a.runShow(t, "wobbly")
	if code != 1 || out != "" {
		t.Fatalf("code=%d stdout=%q stderr=%s", code, out, errOut)
	}
	lines := strings.Split(strings.TrimSpace(errOut), "\n")
	if want := fmt.Sprintf(`agent-archive: show: "wobbly" matches 2 sessions in %s; pass one ID:`, a.label); lines[0] != want {
		t.Fatalf("first line %q, want %q\n%s", lines[0], want, errOut)
	}
	if !strings.Contains(errOut, "wobbly01  codex") || !strings.Contains(errOut, "#213") || !strings.Contains(errOut, "Fix wobbly retention test") || strings.Contains(errOut, "Reviewer") {
		t.Fatalf("rows:\n%s", errOut)
	}
	if !strings.Contains(errOut, "\nNext: agent-archive show wobbly01\n") || !strings.HasSuffix(errOut, "      (or: agent-archive list \"wobbly\" --json)\n") {
		t.Fatalf("next command:\n%s", errOut)
	}
	// The list command repeats the search's --harness, so it is the same search.
	if _, errOut, _ := a.runShow(t, "wobbly", "--harness", "codex"); !strings.HasSuffix(errOut, "      (or: agent-archive list \"wobbly\" --harness codex --json)\n") {
		t.Fatalf("--harness is not repeated:\n%s", errOut)
	}
	// The ID it names is read directly.
	if out, _, code := a.runShow(t, "wobbly01", "--json"); code != 0 || !strings.Contains(out, `"session_id": "wobbly01"`) {
		t.Fatalf("the ID is not read: code=%d\n%s", code, out)
	}
}

// show reads an exact full ID without listing, and a subagent is found by
// words only when nothing top-level matches.
func TestShowSubagentOnlyAsTheLastTier(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "parent01", "Orchestrate the release", a.label)
	a.add(t, "sub00001", "Reviewer of PR #208", a.label, subagentOf("parent01"))
	out, errOut, code := a.runShow(t, "#208", "--json")
	if code != 0 || !strings.Contains(out, `"session_id": "sub00001"`) {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	a.add(t, "bill0001", "Reviewer for billing", "billing")
	out, _, code = a.runShow(t, "reviewer", "--json")
	if code != 0 || !strings.Contains(out, `"session_id": "bill0001"`) {
		t.Fatalf("a top-level session elsewhere should win: code=%d\n%s", code, out)
	}
}

// handoff's several-matches table has the PR column and ends with the exact
// next command; a subagent row says whose it is.
func TestHandoffCandidateTableHasPRsAndTheNextCommand(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.addArchivedWith(t, "wobbly01", "Fix wobbly retention test", "", linked(213))
	f.addArchivedWith(t, "wobbly02", "Fix wobbly hook-lock test", "", nil)
	f.addArchivedWith(t, "wobbly03", "Reviewer for the wobbly fix", "", subagentOf("wobbly01"))
	out, errOut, code := runHandoff(t, f.env, "wobbly", "--all-projects")
	if code != 1 {
		t.Fatalf("code=%d stderr=%s out=%s", code, errOut, out)
	}
	if !strings.HasPrefix(errOut, `agent-archive: handoff: "wobbly" matches 2 sessions; pass one ID:`+"\n") {
		t.Fatalf("heading:\n%s", errOut)
	}
	line := func(id string) string {
		for l := range strings.SplitSeq(errOut, "\n") {
			if strings.Contains(l, id) {
				return l
			}
		}
		return ""
	}
	if l := line("wobbly01"); !strings.Contains(l, "codex") || !strings.Contains(l, "#213") || !strings.Contains(l, "Fix wobbly retention test") {
		t.Fatalf("PR column: %q\n%s", l, errOut)
	}
	if strings.Contains(errOut, "Reviewer") {
		t.Fatalf("a subagent is listed with top-level matches:\n%s", errOut)
	}
	next := "Next: agent-archive handoff wobbly0"
	if !strings.Contains(errOut, next) || !strings.Contains(errOut, " --harness codex\n      (or: agent-archive list \"wobbly\" --all-projects --json)\n") {
		t.Fatalf("next command:\n%s", errOut)
	}

	// Only subagents match: they are listed, labelled with their parent.
	f.addArchivedWith(t, "revwr001", "Reviewer one for the zebra", "", subagentOf("wobbly01"))
	f.addArchivedWith(t, "revwr002", "Reviewer two for the zebra", "", subagentOf("wobbly02"))
	_, errOut, code = runHandoff(t, f.env, "zebra", "--all-projects")
	if code != 1 || !strings.Contains(errOut, "subagent of wobbly01") || !strings.Contains(errOut, "subagent of wobbly02") {
		t.Fatalf("code=%d\n%s", code, errOut)
	}
}

// A subagent answers a handoff only when no top-level session matches, in the
// scope first.
func TestHandoffSubagentIsTheLastTier(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.addArchivedWith(t, "parent01", "Orchestrate the release", "billing", nil)
	f.addArchivedWith(t, "subbill1", "Auditor of the ledger", "billing", subagentOf("parent01"))
	f.addArchivedWith(t, "subhere1", "Auditor of the docs", filepath.Base(f.project), subagentOf("parent01"))
	opts := handoffOptions{sessionID: "auditor", source: "auto"}
	var out, errOut bytes.Buffer
	code, done := resolveHandoffQuery(&opts, f.home, false, newTypedInput(strings.NewReader("")), &out, &errOut, f.env)
	if done || code != 0 || opts.sessionID != "subhere1" {
		t.Fatalf("in scope: session %q code %d done %v stderr %q", opts.sessionID, code, done, errOut.String())
	}
	opts = handoffOptions{sessionID: "ledger", source: "auto"}
	errOut.Reset()
	code, done = resolveHandoffQuery(&opts, f.home, false, newTypedInput(strings.NewReader("")), &out, &errOut, f.env)
	if done || code != 0 || opts.sessionID != "subbill1" {
		t.Fatalf("elsewhere: session %q code %d done %v stderr %q", opts.sessionID, code, done, errOut.String())
	}
	// A top-level session elsewhere beats a subagent in scope.
	f.addArchivedWith(t, "bill0009", "Auditor for billing", "billing", nil)
	opts = handoffOptions{sessionID: "auditor", source: "auto"}
	errOut.Reset()
	code, done = resolveHandoffQuery(&opts, f.home, false, newTypedInput(strings.NewReader("")), &out, &errOut, f.env)
	if done || code != 0 || opts.sessionID != "bill0009" {
		t.Fatalf("top-level elsewhere: session %q code %d done %v stderr %q", opts.sessionID, code, done, errOut.String())
	}
}

// handoff words match across fields, a PR number among them, in this machine's
// sessions first.
func TestHandoffWordsMatchAcrossFieldsAndPRs(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.addArchivedWith(t, "linux001", "Add a systemd unit", filepath.Base(f.project), all(named("Implement Linux support"), linked(212), onBranch("feature/linux-port")))
	f.addArchivedWith(t, "linux002", "Linux notes", filepath.Base(f.project), nil)
	// A session whose ID starts with 212 is not PR 212. The fixture's other
	// IDs are random, so this pins what one of them could do by chance.
	f.addArchivedWith(t, "212b0000", "Unrelated notes", filepath.Base(f.project), nil)
	for _, query := range []string{"linux 212", "#212", "212", "linux-port systemd", "LINUX SUPPORT"} {
		opts := handoffOptions{sessionID: query, source: "auto"}
		var out, errOut bytes.Buffer
		code, done := resolveHandoffQuery(&opts, f.home, false, newTypedInput(strings.NewReader("")), &out, &errOut, f.env)
		if done || code != 0 || opts.sessionID != "linux001" {
			t.Errorf("%q: session %q code %d done %v stderr %q", query, opts.sessionID, code, done, errOut.String())
		}
	}
}

// The picker gives a parent a dim hint of its subagents and offers none of
// them as rows.
func TestHandoffPickerHintsAtSubagents(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.addArchivedWith(t, "parent01", "Orchestrate the release", filepath.Base(f.project), nil)
	f.addArchivedWith(t, "child001", "First child task", filepath.Base(f.project), subagentOf("parent01"))
	f.addArchivedWith(t, "child002", "Second child task", filepath.Base(f.project), subagentOf("parent01"))
	f.addArchivedWith(t, "other001", "A lone task", filepath.Base(f.project), nil)
	out, errOut, code := runPicker(t, f.env, "q\n")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if line := pickerLine(t, out, "parent01"); !strings.Contains(line, "Orchestrate the release · 2 subagents") {
		t.Fatalf("parent row: %q", line)
	}
	if line := pickerLine(t, out, "other001"); strings.Contains(line, "subagent") {
		t.Fatalf("a hint on a session with none: %q", line)
	}
	if strings.Contains(out, "child task") {
		t.Fatalf("a subagent is offered:\n%s", out)
	}
	// So does the picker that words matching several sessions open.
	f.addArchivedWith(t, "parent02", "Orchestrate the docs", filepath.Base(f.project), nil)
	out, errOut, code = runPicker(t, f.env, "q\n", "orchestrate")
	if code != 0 {
		t.Fatalf("words: code=%d stderr=%s", code, errOut)
	}
	if line := pickerLine(t, out, "parent01"); !strings.Contains(line, "Orchestrate the release · 2 subagents") {
		t.Fatalf("words: parent row: %q", line)
	}
}

// The browser, like the table, lists top-level sessions only, says how many
// subagent sessions it hides, and hints at a parent's.
func TestListBrowserHidesSubagents(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	a.add(t, "parent01", "Orchestrate the release", a.label)
	a.add(t, "child001", "First child task", a.label, subagentOf("parent01"))
	out, code := ttyRunAllowingInput(t, a.env, "q\n", "list")
	if code != 0 || !strings.Contains(out, "Enter number") {
		t.Fatalf("code=%d\n%s", code, out)
	}
	if strings.Contains(out, "child001") || !strings.Contains(out, "Orchestrate the release · 1 subagent") ||
		!strings.Contains(out, "(1 subagent session hidden; search to find one)") {
		t.Fatalf("browser:\n%s", out)
	}
}

// List and handoff retain exact short-ID precedence. Show combines the ID
// candidate with title matches, then applies the scope tiers.
func TestShortIDPrecedenceInListHandoffAndShow(t *testing.T) {
	t.Parallel()
	f := newPickerFixture(t)
	f.addArchivedWith(t, "abcd1234", "The real one", "billing", nil)
	f.addArchivedWith(t, "wxyz9999", "Notes about abcd1234", filepath.Base(f.project), nil)
	f.addArchivedWith(t, "fedc4321", "A subagent elsewhere", "billing", subagentOf("abcd1234"))
	f.addArchivedWith(t, "wxyz8888", "Notes about fedc4321", filepath.Base(f.project), nil)
	for query, want := range map[string]string{"abcd1234": "abcd1234", "fedc4321": "fedc4321"} {
		opts := handoffOptions{sessionID: query, source: "auto"}
		var out, errOut bytes.Buffer
		code, done := resolveHandoffQuery(&opts, f.home, false, newTypedInput(strings.NewReader("")), &out, &errOut, f.env)
		if done || code != 0 || opts.sessionID != want || errOut.Len() != 0 {
			t.Errorf("handoff %s: session %q code %d done %v stderr %q", query, opts.sessionID, code, done, errOut.String())
		}
	}

	a := newScopedArchive(t)
	a.add(t, "abcd1234", "The real one", "billing")
	a.add(t, "wxyz9999", "Notes about abcd1234", a.label)
	out, _, _ := a.runList(t, "abcd1234")
	if got := namedIDs(out, "abcd1234", "wxyz9999"); !sameStrings(got, []string{"abcd1234"}) {
		t.Fatalf("list listed %v:\n%s", got, out)
	}
	if out, _, code := a.runShow(t, "abcd1234", "--json"); code != 0 || !strings.Contains(out, `"session_id": "wxyz9999"`) {
		t.Fatalf("show: code=%d\n%s", code, out)
	}
}
