package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// printAppStatus writes status APP: the overall state, then one app in
// full: its line, every session it is uploading, its gaps, a table of its
// projects, and the Storage section. It returns the command's exit code: 1 when the app is not
// one this installation captures or imported.
func printAppStatus(out, errOut io.Writer, view statusView, name string, sc statusScreen) int {
	s := sc.style
	if !view.configured {
		sc.printHeader(out, view)
		return 0
	}
	var row statusRow
	var app appStatus
	found := false
	for _, candidate := range view.Apps {
		if candidate.Name == name {
			app, row, found = candidate, sc.appRow(candidate, 0), true
		}
	}
	for _, candidate := range view.importedApps {
		if candidate.Name == name && !found {
			app, row, found = candidate, sc.importedAppRow(candidate, 0), true
		}
	}
	if !found {
		captured := make([]string, 0, len(view.Apps))
		for _, other := range view.Apps {
			captured = append(captured, other.Name)
		}
		terminal.Printf(errOut, "agent-archive: status: %s isn't one of the apps this installation captures (%s). Run agent-archive setup to add it.\n", appName(name), friendlyApps(captured))
		return 1
	}
	sc.printHeader(out, view)
	terminal.Printf(out, "\n%s\n", s.bold("Capture"))
	sc.printRows(out, []statusRow{row})
	if lines := sizeLimitedLines(app); len(lines) > 0 {
		terminal.Printf(out, "\n%s\n", s.bold("Stopped at a size limit"))
		for _, line := range lines {
			terminal.Println(out, line)
		}
	}
	if table := sc.projectTable(app); len(table) > 0 {
		terminal.Printf(out, "\n%s\n", s.bold("Projects"))
		for _, line := range table {
			terminal.Println(out, line)
		}
	}
	// Storage as the default status shows it: the headline above leaves
	// the last pass's errors, and whether storage failed on it, to this
	// section.
	terminal.Printf(out, "\n%s\n", s.bold("Storage"))
	sc.printRows(out, sc.storageRows(view))
	sc.printNotes(out, view)
	if sc.verbose {
		terminal.Printf(out, "\n%s\n", s.bold("Details"))
		printAppDetails(out, app)
	}
	terminal.Println(out)
	if sc.verbose {
		terminal.Printf(out, "%s %s\n", s.dim("As JSON:"), s.cmd("agent-archive status --json"))
		return 0
	}
	sc.printFooter(out, "agent-archive status", "agent-archive status "+name+" --verbose")
	return 0
}

// sizeLimitedLines lists an app's sessions whose capture stopped at a size
// limit: each one's ID, the limit and its size, then how to go on.
func sizeLimitedLines(app appStatus) []string {
	if len(app.SizeLimitedSessions) == 0 {
		return nil
	}
	lines := make([]string, 0, len(app.SizeLimitedSessions)+1)
	for _, session := range app.SizeLimitedSessions {
		line := "  " + archive.DisplayLine(session.ArchiveSessionID)
		switch {
		case session.Limit != "" && session.SizeBytes > 0:
			line += fmt.Sprintf("  %s, over the %s limit", formatBytes(session.SizeBytes), session.Limit)
		case session.Limit != "":
			line += "  over the " + session.Limit + " limit"
		default:
			line += "  over a transcript size limit"
		}
		lines = append(lines, line)
	}
	return append(lines, "  Each keeps its last published snapshot. To keep capturing the work, continue it in a new session (agent-archive handoff SESSION_ID carries the context there).")
}

// projectTable is status APP's table of an app's projects: for each, the
// sessions its hooks captured, those imported, those uploading, and how far
// read-back has got. A project no configured pair owns (a configuration
// without projects) has no read-back of its own.
func (sc statusScreen) projectTable(app appStatus) []string {
	rows := [][]string{{"Project", "Sessions", "Imported", "Uploading", "Read-back"}}
	for _, pair := range app.Projects {
		rows = append(rows, []string{sc.path(pair.ProjectRoot), strconv.Itoa(pair.sessions), strconv.Itoa(pair.imported), strconv.Itoa(pair.uploading), pairProgress(pair)})
	}
	for _, other := range app.otherProjects {
		rows = append(rows, []string{sc.path(other.ProjectRoot), strconv.Itoa(other.sessions), strconv.Itoa(other.imported), strconv.Itoa(other.uploading), "-"})
	}
	if len(rows) == 1 {
		return nil
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], visibleWidth(cell))
		}
	}
	lines := make([]string, 0, len(rows))
	for i, row := range rows {
		var line strings.Builder
		line.WriteString("  ")
		for j, cell := range row {
			if j == len(row)-1 {
				line.WriteString(cell)
				break
			}
			line.WriteString(cell + strings.Repeat(" ", widths[j]-visibleWidth(cell)+3))
		}
		text := line.String()
		if i == 0 {
			text = "  " + sc.style.dim(strings.TrimPrefix(text, "  "))
		}
		lines = append(lines, text)
	}
	return lines
}
