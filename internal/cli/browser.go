package cli

import (
	"context"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// browserMode is what Enter does with the chosen row.
type browserMode int

const (
	// browseSessions shows the session's details, then goes back to the list.
	browseSessions browserMode = iota
	// pickSession returns the row to the caller.
	pickSession
)

// browserSpec is what differs between the places a person chooses a session:
// where the rows come from, what Enter does, and the heading's verb. Every
// other part of the browser (the rows and their columns, the scope heading
// and `a`, the `/` filter, paging and scrolling) is the same code for every
// caller.
type browserLoadAction struct {
	Label string
	Load  func() (bool, error)
}

type browserSpec struct {
	LoadOlder *browserLoadAction
	Notice    string
	Mode      browserMode
	// Verb is what Enter does, which leads the heading ("Hand off", "Show").
	// Empty for none. Lower-cased, it is what the line-mode prompt says
	// Enter does when the browser picks.
	Verb string
	// Choices are the rows, in the working directory's scope and in all
	// projects, or in one list (rowChoices) for a caller whose rows are
	// already chosen.
	Choices *scopeChoices
	// Query is the words the browser opens with in its filter, which the
	// person can edit; empty opens it unfiltered.
	Query string
	// Command names the command in an error message.
	Command string
	// Store and NoPager are for browseSessions: the archive the details are
	// read from, and whether to page them.
	Store   storage.ObjectStore
	NoPager bool
}

// rowChoices is the choices of a browser over rows already chosen, with no
// scope to change: an ambiguous word's matches.
func rowChoices(rows []listRow, format listFormatOptions) *scopeChoices {
	return newScopeChoices(sessionScope{}, format, false, func(sessionScope) scopeView {
		return scopeView{rows: rows, total: len(rows)}
	})
}

// runBrowser is the one place a person chooses a session, for every command
// that asks: the alternate-screen browser, reading keys on a terminal that
// can be (`/` filters the rows as it is typed), or lines when none can.
//
// In browseSessions mode it shows the sessions' details until the person
// quits, then prints the last one viewed, and returns no row. In pickSession
// mode it returns the row Enter chose, with picked set; picked is false when
// the person quit. code is the exit code when the browser failed, else 0. When
// it picks on a terminal read by keys, what the person typed ahead is handed
// back for the prompts after it to read (prompter.handBack).
func runBrowser(ctx context.Context, env sessionBrowserDependencies, p *prompter, stdout, stderr io.Writer, spec browserSpec) (row listRow, picked bool, code int) {
	screen := enterAltScreen(stdout, env)
	// Only a screen that clears can draw a page again in place.
	var redraw func()
	if screen.clears() {
		redraw = screen.clear
	}
	keys := browserKeys(env, p, screen)
	// Deferred as well, so not even a panic leaves the terminal on the
	// alternate screen, or without echo; leave and close do nothing the
	// second time.
	defer screen.leave()
	if keys != nil {
		defer keys.close()
	}
	if spec.LoadOlder == nil {
		spec.LoadOlder = spec.Choices.loadOlder
	}
	list := &sessionPicker{older: spec.LoadOlder, boundedNotice: spec.Notice, env: env, clear: redraw, keys: keys, verb: spec.Verb, filter: spec.Query, filtering: spec.Query != "" && keys != nil}
	var err error
	if spec.Mode == browseSessions {
		b := &sessionBrowser{env: env, prompt: p, stdout: stdout, stderr: stderr, store: spec.Store, format: spec.Choices.format, noPager: spec.NoPager, screen: screen, keys: keys, list: list}
		err = b.run(ctx, spec.Choices)
		b.screen.leave()
		if err == nil && b.last != nil {
			renderSessionSummary(stdout, *b.last, b.summaryOptions(true))
		}
	} else {
		row, picked, err = list.pickScoped(p, stdout, spec.Choices, strings.ToLower(spec.Verb))
		if keys != nil && picked && p.handBack != nil {
			keys.handBack(p.handBack)
		}
		screen.leave()
	}
	if err != nil {
		terminal.Printf(stderr, "agent-archive: %s: %v\n", spec.Command, err)
		return listRow{}, false, 1
	}
	return row, picked, 0
}
