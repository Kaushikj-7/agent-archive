package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// candidateList is the table `handoff` and `show` print to stderr when several
// sessions match and nothing can ask which: a person reading a log, or an
// agent that must choose, with what it needs to tell the rows apart and the
// command to run next.
type candidateList struct {
	// command is the command that searched: handoff or show.
	command string
	// query is what was searched for, as typed.
	query string
	// label names the scope the matches are in, "" when they are not in one.
	label string
	// total is how many sessions match; rows are the first of them.
	total int
	rows  []listRow
	// next is the command that takes one of the rows, from its ID.
	next func(row listRow) string
	// listFlags are the options of the search that `list` repeats.
	listFlags []string
}

// quotedWord is s as one shell word: in double quotes, as the commands in the
// help are written, unless it holds a character they would expand.
func quotedWord(s string) string {
	if !strings.ContainsAny(s, "\"$`\\!") {
		return `"` + s + `"`
	}
	return shellWord(s)
}

// print writes the table: one line for the question, the matching sessions
// in the piped list's columns (printCandidateRows), and the exact next
// command.
func (c candidateList) print(w io.Writer) {
	var b strings.Builder
	where := ""
	if c.label != "" {
		where = " in " + archive.DisplayLine(c.label)
	}
	fmt.Fprintf(&b, "agent-archive: %s: %q matches %d sessions%s; pass one ID:\n", c.command, queryLabel(c.query), c.total, where)
	printCandidateRows(&b, c.rows)
	if len(c.rows) < c.total {
		fmt.Fprintf(&b, "  ... and %d more; add words, a PR number, or --harness to narrow\n", c.total-len(c.rows))
	}
	if len(c.rows) > 0 && c.next != nil {
		fmt.Fprintf(&b, "Next: %s\n", c.next(c.rows[0]))
		// A query cut for display, or cleaned of control characters, would
		// not be the one searched, so no command is offered for it.
		if c.query == archive.DisplayLine(c.query) && len(c.query) <= handoffQueryWidth {
			fmt.Fprintf(&b, "      (or: agent-archive list %s%s --json)\n", quotedWord(c.query), strings.Join(append([]string{""}, c.listFlags...), " "))
		}
	}
	terminal.Print(w, b.String())
}

// candidateTitleWidth is the most display columns of a title a list of
// matches shows. A captured title or name is at most 129 characters, so a
// real one is never cut; a sidecar written by something else can be any
// length, and is.
const candidateTitleWidth = 160

// printCandidateRows writes rows as `list | cat` does, each line indented
// two columns under the question: WHEN AGENT PROJECT PR ID TITLE, with the
// title last and whole (tableHeader, tableCells). PROJECT is left out when
// the rows share one and PR when none has one; AGENT is the harness name
// --harness takes. Every cell is stored data, which may hold control
// characters or be any length, so each is cut to one bounded line: an agent
// reads this.
func printCandidateRows(w io.Writer, rows []listRow) {
	if len(rows) == 0 {
		return
	}
	opts := listFormatOptions{
		HideProject: distinctProjects(rows) <= 1,
		ShowPR:      slices.ContainsFunc(rows, func(r listRow) bool { return r.PR != "" }),
	}
	tw := &columnWriter{out: w}
	terminal.Println(tw, "  "+strings.Join(tableHeader(opts), "\t"))
	for _, r := range rows {
		r.ShortID = cappedLine(r.ShortID, 80)
		r.Harness = cappedLine(r.Harness, 20)
		r.Project = cappedLine(r.Project, 30)
		r.Title = cappedLine(r.Title, candidateTitleWidth)
		terminal.Println(tw, "  "+strings.Join(tableCells(r, opts), "\t"))
	}
	// The table is built in memory, where writes cannot fail.
	_ = tw.Flush()
}

// distinctProjects is how many projects rows come from, told apart by
// project ID where they have one, else by name.
func distinctProjects(rows []listRow) int {
	seen := map[string]bool{}
	for _, r := range rows {
		key := "name:" + r.Project
		if r.ProjectID != "" {
			key = "id:" + r.ProjectID
		}
		seen[key] = true
	}
	return len(seen)
}
