package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// printStatusIssues keeps detailed exceptions on an explicitly requested view.
// The existing detailed reader retains every recovery and publication check.
func printStatusIssues(out io.Writer, view statusView, sc statusScreen) {
	terminal.Printf(out, "%s\n", sc.style.bold("Agent Archive · Issues"))
	if view.State != "Ready" {
		sc.printNextStep(out, view)
	}
	var rows []statusRow
	for _, problem := range lastErrorRows(view.Collector) {
		rows = append(rows, statusRow{mark: sc.style.warnMark(), cells: []string{problem}})
	}
	rows = append(rows, sc.issueGroupRows(view)...)
	for _, app := range slices.Concat(view.Apps, view.importedApps) {
		rows = append(rows, sc.discoveryIssueRows(app)...)
		for _, gap := range app.CaptureGaps {
			// The groups above already cover the sessions the last pass
			// could not update.
			if strings.HasPrefix(gap.Detail, issueGapLead) {
				continue
			}
			rows = append(rows, statusRow{mark: sc.style.warnMark(), cells: []string{appName(app.Name), gap.Detail}})
		}
		// The rows above already name individual gaps. Share the detailed
		// view's remaining transcript and import notices without that aggregate.
		app.CaptureGaps = nil
		if notes := sc.gapNotes(app); len(notes) > 0 {
			rows = append(rows, statusRow{cells: []string{appName(app.Name)}, notes: notes})
		}
		if app.PreSetupSessions != nil {
			rows = append(rows, statusRow{cells: []string{fmt.Sprintf("%s: %d sessions from before setup were not captured", appName(app.Name), app.PreSetupSessions.Sessions)}, notes: sc.preSetupNotes(app)})
		}
		if app.VerificationDetail != "" {
			rows = append(rows, statusRow{cells: []string{appName(app.Name), app.VerificationDetail}})
		}
	}
	rows = append(rows, sc.diagnosticRows(view.CaptureDiagnostics)...)
	if recovery := view.IdentityRecovery; recovery != nil && !recovery.Complete {
		rows = append(rows, statusRow{mark: sc.style.warnMark(), cells: []string{"Local identity recovery is pending or unavailable."}, notes: []statusNote{{text: sc.style.cmd("agent-archive sync")}}})
	}
	if view.configured && view.PrivacyEvidence.State != "verified_private" {
		rows = append(rows, sc.privacyRow(view.PrivacyEvidence))
	}
	rows = append(rows, sc.noteRows(view)...)
	if len(rows) == 0 && view.State == "Ready" {
		terminal.Println(out, "No recorded issues.")
	} else {
		sc.printRows(out, rows)
	}
	terminal.Printf(out, "\n%s\n%s\n", sc.style.cmd("agent-archive status"), sc.style.cmd("agent-archive status --verbose"))
}

// issueGroupRows are the last pass's failed sessions, one row per kind of
// failure and reason, each naming a few of its sessions. A kind with
// nothing to do is information, not a warning.
func (sc statusScreen) issueGroupRows(view statusView) []statusRow {
	var rows []statusRow
	for _, group := range issueGroups(view.Collector, func(id string) bool { return view.subagentIssues[id] }) {
		mark := sc.info()
		if group.actionable() {
			mark = sc.style.warnMark()
		}
		rows = append(rows, statusRow{mark: mark, cells: []string{group.text()}, notes: []statusNote{{text: group.examples()}}})
	}
	return rows
}

func (sc statusScreen) discoveryIssueRows(app appStatus) []statusRow {
	if !discoveryEnabled(app) {
		return nil
	}
	sc.verbose = true
	row := statusRow{cells: []string{appName(app.Name), ""}}
	sc.addCodexDiscoveryRow(&row, app)
	row.notes = slices.DeleteFunc(row.notes, func(note statusNote) bool { return note.mark != sc.style.warnMark() })
	if len(row.notes) == 0 {
		return nil
	}
	return []statusRow{row}
}

func (sc statusScreen) diagnosticRows(diagnostics []capture.Diagnostic) []statusRow {
	var rows []statusRow
	for _, diagnostic := range diagnostics {
		location := ""
		if diagnostic.ProjectRoot != "" {
			location = " in " + sc.path(diagnostic.ProjectRoot)
		}
		what := "skipped a session"
		if capture.DiagnosticPartial(diagnostic.Code) {
			what = "left out part of a session"
		}
		rows = append(rows, statusRow{mark: sc.style.warnMark(), cells: []string{fmt.Sprintf("%s %s%s %s: %s", appName(diagnostic.Harness), what, location, relativeAge(sc.now, diagnostic.ObservedAt), capture.DiagnosticMessage(diagnostic.Code))}})
	}
	return rows
}
