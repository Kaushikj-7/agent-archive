package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

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

// print writes the table: one line for the question, a row for each session
// (ID, harness, project when the rows span several, when, PR when a row has
// one, title), and the exact next command.
func (c candidateList) print(w io.Writer) {
	var b strings.Builder
	where := ""
	if c.label != "" {
		where = " in " + archive.DisplayLine(c.label)
	}
	fmt.Fprintf(&b, "agent-archive: %s: %q matches %d sessions%s; pass one ID:\n", c.command, queryLabel(c.query), c.total, where)
	showPR := false
	for _, row := range c.rows {
		showPR = showPR || row.PR != ""
	}
	showProject := distinctProjects(c.rows) > 1
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, row := range c.rows {
		// The ID is stored data, like the title, and may hold control
		// characters. Nothing is left long or invisible either: an agent
		// reads this.
		cells := []string{"  " + cappedLine(row.ShortID, 12), cappedLine(row.Harness, 20)}
		if showProject {
			cells = append(cells, cappedLine(row.Project, 30))
		}
		cells = append(cells, row.When)
		if showPR {
			cells = append(cells, row.PR)
		}
		title := cappedLine(row.Title, matchFieldWidth)
		if row.Parent != "" {
			title += subagentNote(row)
		}
		terminal.Println(tw, strings.Join(append(cells, title), "\t"))
	}
	// The table is built in memory, where writes cannot fail.
	_ = tw.Flush()
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
