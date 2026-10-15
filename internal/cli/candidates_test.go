package cli

import (
	"strings"
	"testing"
)

// The matches a command cannot ask about are the piped list's table:
// WHEN AGENT PROJECT PR ID TITLE, the title last and whole, an empty PR "-",
// PROJECT only when the rows span several, and every cell one plain line.
func TestCandidateRowsUseThePipedColumns(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("word ", 40) + "TAIL"
	rows := []listRow{
		{ShortID: "d7a77938", Harness: "claude", Project: "agent-archive", ProjectID: "p1", WhenShort: "2m", PR: "#505", Title: "Review historical project command", Children: 3},
		{ShortID: "36a7d5ee", Harness: "codex", Project: "billing\x1b[31m", ProjectID: "p2", WhenShort: "Mon", Title: long, Hint: "not yet uploaded"},
	}
	var b strings.Builder
	printCandidateRows(&b, rows)
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	want := []string{
		"  WHEN  AGENT   PROJECT        PR    ID        TITLE",
		"  2m    claude  agent-archive  #505  d7a77938  Review historical project command · 3 subagents",
		"  Mon   codex   billing[31m    -     36a7d5ee  " + cappedLine(long, candidateTitleWidth) + " · not yet uploaded",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines:\n%s", len(lines), b.String())
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d:\n got %q\nwant %q", i, lines[i], want[i])
		}
	}
	if strings.Contains(b.String(), "\x1b") || strings.Contains(b.String(), "TAIL") {
		t.Errorf("a cell is not one bounded plain line:\n%q", b.String())
	}

	// One project and no PR: neither column.
	b.Reset()
	rows[1].Project, rows[1].ProjectID, rows[0].PR = rows[0].Project, rows[0].ProjectID, ""
	printCandidateRows(&b, rows)
	if header := strings.SplitN(b.String(), "\n", 2)[0]; header != "  WHEN  AGENT   ID        TITLE" {
		t.Errorf("header %q:\n%s", header, b.String())
	}

	b.Reset()
	if printCandidateRows(&b, nil); b.Len() != 0 {
		t.Errorf("no rows printed %q", b.String())
	}
}
