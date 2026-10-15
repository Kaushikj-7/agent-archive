package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

type browserOperationResult struct {
	rows       []listRow
	more       bool
	err        error
	generation uint64
	panicValue any
}

type browserOperation struct {
	cancel context.CancelFunc
	done   chan browserOperationResult
}

// stopOperations cancels and joins every owned read before the command closes
// its snapshot or native providers. A cancelled result is never cached.
func (l *sessionPicker) stopOperations() {
	l.generation++
	if op := l.operation; op != nil {
		op.cancel()
		result := <-op.done
		l.operation = nil
		l.noteSearchError(result.err)
		result.checked()
	}
}

func (l *sessionPicker) prepareSearchContext(ctx context.Context, rows []listRow) {
	if l.words() == "" || l.searchRead || l.searchFailure != nil || (l.search == nil && l.searchContext == nil) {
		return
	}
	if l.searchContext == nil {
		l.searched, l.searchRead = withTableNumbers(rows, l.search()), true
		l.memo = nil
		return
	}
	if l.operation == nil {
		ctx, cancel := context.WithCancel(ctx)
		generation := l.generation
		op := &browserOperation{cancel: cancel, done: make(chan browserOperationResult, 1)}
		l.operation = op
		loader := l.searchContext
		go runBrowserRead(op.done, func() browserOperationResult {
			result, err := loader(ctx)
			return browserOperationResult{rows: result, err: err, generation: generation}
		})
	}
	select {
	case result := <-l.operation.done:
		l.operation.cancel()
		l.operation = nil
		l.noteSearchError(result.err)
		result.checked()
		if result.generation != l.generation {
			return
		}
		if result.err != nil {
			l.searchFailure = result.err
		} else {
			l.searched, l.searchRead = withTableNumbers(rows, result.rows), true
		}
		l.memo, l.cache, l.bottom = nil, nil, nil
	default:
	}
}

// loadOlder runs provider work outside rendering and continues servicing keys.
// Esc cancels this read; quitting or changing scope cancels before returning.
func (l *sessionPicker) loadOlderContext(ctx context.Context, p *prompter, stdout io.Writer) (bool, error) {
	l.stopOperations()
	l.operationCanceled = false
	redraw := l.loadingFrame
	l.loadingFrame = nil
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan browserOperationResult, 1)
	loader := l.older
	if loader.LoadContext == nil {
		return loader.Load()
	}
	go runBrowserRead(done, func() browserOperationResult {
		more, err := loader.LoadContext(ctx)
		return browserOperationResult{more: more, err: err}
	})
	joined := false
	join := func() browserOperationResult { result := <-done; joined = true; result.checked(); return result }
	defer func() {
		cancel()
		if !joined {
			join()
		}
	}()
	typedAhead := l.keys != nil && l.keys.buffered() || l.keys == nil && p.in.Buffered() > 0
	message := "Loading " + loader.Label + "… Esc cancels"
	if redraw != nil && l.keys != nil {
		// Reading keys, the list is drawn again with the message as its
		// footer line, rather than under the prompt, which scrolls it.
		redraw(message)
	} else {
		terminal.Println(stdout, message)
	}
	for {
		select {
		case result := <-done:
			joined = true
			result.checked()
			return result.more, result.err
		case <-ctx.Done():
			cancel()
			join()
			return false, ctx.Err()
		default:
		}
		cancelled, readable, err := false, true, error(nil)
		switch {
		case typedAhead && l.keys != nil:
			// The keys typed ahead are handled after the load, but an
			// Esc pressed while it runs still stops it.
			cancelled, readable, err = l.loadInputAhead()
		case typedAhead:
			readable = false
		default:
			cancelled, err = l.loadInput(p)
		}
		if !readable {
			select {
			case result := <-done:
				joined = true
				result.checked()
				return result.more, result.err
			case <-ctx.Done():
				cancel()
				join()
				return false, ctx.Err()
			}
		}
		if err != nil {
			cancel()
			join()
			return false, err
		}
		if cancelled {
			l.operationCanceled = true
			if l.keys != nil {
				// What was left of a wheel burst would scroll past the end,
				// and load again, at once.
				l.keys.dropMoves()
			}
			if l.toggled {
				// The scope's own load says so where this one did.
				l.loadingFrame = redraw
			}
			cancel()
			result := join()
			if result.err != nil && !errors.Is(result.err, context.Canceled) {
				return false, result.err
			}
			return true, nil
		}
	}
}

func (l *sessionPicker) nextBrowserKey() (key, error) {
	if l.operation != nil {
		return l.keys.nextWait(50 * time.Millisecond)
	}
	return l.keys.next()
}

func (l *sessionPicker) waitSearchContext(ctx context.Context, rows []listRow) error {
	for l.operation != nil {
		select {
		case <-ctx.Done():
			l.stopOperations()
			return ctx.Err()
		case result := <-l.operation.done:
			l.operation.cancel()
			l.operation = nil
			l.noteSearchError(result.err)
			result.checked()
			if result.generation != l.generation {
				continue
			}
			if result.err != nil {
				l.searchFailure = result.err
			} else {
				l.searched, l.searchRead = withTableNumbers(rows, result.rows), true
			}
			l.memo, l.cache, l.bottom = nil, nil, nil
		}
	}
	return l.invalidView
}

// readBrowserLine services completed search reads while waiting for cooked
// input. A completed search asks the caller to redraw without consuming input.
func (l *sessionPicker) readBrowserLineContext(ctx context.Context, p *prompter, question string, rows []listRow) (string, bool, error) {
	terminal.Print(p.out, question)
	for {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		pending := l.operation != nil
		l.prepareSearchContext(ctx, rows)
		if l.invalidView != nil {
			return "", false, l.invalidView
		}
		if pending && l.operation == nil {
			return "", true, nil
		}
		answer, complete, err := l.pollBrowserLine(p)
		if err != nil || complete {
			return answer, false, err
		}
	}
}

// pendingNumberAnswer preserves prebuffered post-selection input while keeping
// a live numeric submission responsive during a search.
func (l *sessionPicker) pendingNumberAnswerContext(ctx context.Context, p *prompter, rows []listRow, answer string) (string, error) {
	if numericBrowserAnswer(answer) && l.operation != nil && p.in.Buffered() == 0 {
		answer, _, err := l.waitNumberSearchContext(ctx, p, rows, answer)
		return answer, err
	}
	return answer, nil
}

// waitNumberSearch keeps a numeric answer pending while the universe loads,
// without taking away the person's ability to clear, edit, or quit the search.
func (l *sessionPicker) waitNumberSearchContext(ctx context.Context, p *prompter, rows []listRow, answer string) (string, bool, error) {
	for l.operation != nil {
		next, redraw, err := l.readBrowserLineContext(ctx, p, "Searching; Enter clears, words edit, q quits: ", rows)
		if err != nil {
			return "", false, err
		}
		if redraw {
			return answer, true, nil
		}
		if _, e := strconv.Atoi(next); e == nil {
			answer = next
			continue
		}
		return next, false, nil
	}
	return answer, true, nil
}

func (l *sessionPicker) toggleScopeContext(ctx context.Context, p *prompter, stdout io.Writer, choices *scopeChoices) error {
	l.stopOperations()
	l.operationCanceled = false
	if choices.rowsForContext != nil {
		previous, target := l.older, 1-choices.current
		l.older = &browserLoadAction{Label: "scope sessions", LoadContext: func(ctx context.Context) (bool, error) {
			if choices.validate != nil {
				if err := choices.validate(ctx); err != nil {
					return false, err
				}
			}
			choices.built[target] = nil
			choice := choices.choiceContext(ctx, target)
			if err := ctx.Err(); err != nil {
				choices.built[target] = nil
				return false, err
			}
			if choice.loadError != nil {
				return false, choice.loadError
			}
			return true, nil
		}}
		_, err := l.loadOlderContext(ctx, p, stdout)
		l.older = previous
		if err != nil {
			return err
		}
	}
	if !l.operationCanceled && !l.operationQuit {
		choices.toggle()
	}
	return nil
}

// Worker panics return to the owning browser goroutine so its deferred terminal
// restoration and resource cleanup run before the panic escapes.
func runBrowserRead(done chan<- browserOperationResult, load func() browserOperationResult) {
	var result browserOperationResult
	defer func() { result.panicValue = recover(); done <- result }()
	result = load()
}

func (r browserOperationResult) checked() {
	if r.panicValue != nil {
		panic(r.panicValue)
	}
}

// pollBrowserLine reads at most one available byte, so a partial pipe line
// cannot block operation completion, cancellation or terminal restoration.
func (l *sessionPicker) pollBrowserLine(p *prompter) (string, bool, error) {
	ready := p.in.Buffered() > 0
	if !ready {
		var err error
		ready, err = browserLineReady(p.source)
		if err != nil {
			return "", false, err
		}
	}
	if !ready {
		return "", false, nil
	}
	b, err := p.in.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) && l.lineInput != "" {
			answer := l.lineInput
			l.lineInput = ""
			p.noteLineEcho(answer)
			if p.guidedSpacing {
				terminal.Println(p.out)
			}
			return strings.TrimSpace(answer), true, nil
		}
		return "", false, fmt.Errorf("no more input: %w", err)
	}
	l.lineInput += string(b)
	if b != '\n' {
		return "", false, nil
	}
	answer := l.lineInput
	l.lineInput = ""
	p.noteLineEcho(answer)
	if p.guidedSpacing {
		terminal.Println(p.out)
	}
	return strings.TrimSpace(answer), true, nil
}

// loadInputAhead is loadInput for a load started with keys typed ahead,
// which it leaves to be handled after the load: Esc, q, and the end of
// input pressed while the load runs act on it, and other keys are queued
// after the keys typed ahead. readable is false when no key can be read
// until the keys waiting are handled.
func (l *sessionPicker) loadInputAhead() (cancelled, readable bool, err error) {
	if !l.keys.canReadAhead() {
		return false, false, nil
	}
	keys, err := l.keys.readAhead(50 * time.Millisecond)
	if err != nil {
		return false, true, err
	}
	for i, k := range keys {
		switch {
		case k.kind == keyEscape:
		case k.kind == keyEndOfInput || k.kind == keyRune && k.r == 'q':
			l.operationQuit = true
		default:
			continue
		}
		l.keys.queue(keys[i+1:])
		return true, true, nil
	}
	l.keys.queue(keys)
	return false, true, nil
}

func (l *sessionPicker) loadInput(p *prompter) (bool, error) {
	if l.keys == nil {
		answer, complete, err := l.pollBrowserLine(p)
		if err != nil || !complete {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "q", "quit":
			l.operationQuit = true
		case "a":
			l.toggled = true
		case "", "cancel":
		default:
			return false, nil
		}
		return true, nil
	}
	k, err := l.keys.nextWait(50 * time.Millisecond)
	if err != nil {
		return false, err
	}
	switch k.kind {
	case keyEscape:
		return true, nil
	case keyEndOfInput:
		l.operationQuit = true
		return true, nil
	case keyEnter, keyBackspace, keyUp, keyDown, keyLeft, keyRight, keyPageUp, keyPageDown, keyHome, keyEnd, keyResize:
		// These keys do not cancel an operation.
	case keyRune:
		switch k.r {
		case 'q':
			l.operationQuit = true
			return true, nil
		case 'a':
			l.toggled = true
			return true, nil
		}
	}
	return false, nil
}

// A changed or expired snapshot cannot regain valid status by clearing search.
func (l *sessionPicker) noteSearchError(err error) {
	if errors.Is(err, catalog.ErrStaleCursor) {
		l.invalidView = err
	}
}
