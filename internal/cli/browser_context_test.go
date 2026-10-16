package cli

import (
	"context"
	"io"
)

// Context-free helpers keep older test fixtures compact; production passes the invocation context explicitly.
func (l *sessionPicker) operationContext() context.Context {
	if l.ctx != nil {
		return l.ctx
	}
	return context.Background()
}

func (l *sessionPicker) prepareSearch(rows []listRow) {
	l.prepareSearchContext(l.operationContext(), rows)
}

func (l *sessionPicker) waitSearch(rows []listRow) error {
	return l.waitSearchContext(l.operationContext(), rows)
}

func (l *sessionPicker) readBrowserLine(p *prompter, question string, rows []listRow) (string, bool, error) {
	return l.readBrowserLineContext(l.operationContext(), p, question, rows)
}

func (l *sessionPicker) loadOlder(p *prompter, stdout io.Writer) (bool, error) {
	return l.loadOlderContext(l.operationContext(), p, stdout)
}

func (l *sessionPicker) toggleScope(p *prompter, stdout io.Writer, choices *scopeChoices) error {
	return l.toggleScopeContext(l.operationContext(), p, stdout, choices)
}

func (l *sessionPicker) pickScoped(p *prompter, stdout io.Writer, choices *scopeChoices, action string) (listRow, bool, error) {
	return l.pickScopedContext(l.operationContext(), p, stdout, choices, action)
}

func (l *sessionPicker) pickRows(p *prompter, stdout io.Writer, rows []listRow, totalMatched int, truncated bool, format listFormatOptions, action string) (listRow, bool, error) {
	return l.pickRowsContext(l.operationContext(), p, stdout, rows, totalMatched, truncated, format, action)
}

func (l *sessionPicker) lineAnswer(answer string, onPage, rows []listRow, total int, truncated bool, format listFormatOptions) (listRow, bool, string, error) {
	return l.lineAnswerContext(l.operationContext(), answer, onPage, rows, total, truncated, format)
}

// readBurst waits for input, then takes whatever else is already there,
// and waits briefly for the rest of an escape sequence cut off at its end.
func (k *keyInput) readBurst() ([]byte, error) { return k.readBurstWait(-1) }
