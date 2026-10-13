package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/terminal"
)

type browserOperationResult struct {
	rows       []listRow
	more       bool
	err        error
	generation uint64
}
type browserOperation struct {
	cancel context.CancelFunc
	done   chan browserOperationResult
}

func (l *sessionPicker) operationContext() context.Context {
	if l.ctx != nil {
		return l.ctx
	}
	return context.Background()
}

// stopOperations cancels and joins every owned read before the command closes
// its snapshot or native providers. A cancelled result is never cached.
func (l *sessionPicker) stopOperations() {
	l.generation++
	if op := l.operation; op != nil {
		op.cancel()
		<-op.done
		l.operation = nil
	}
}

func (l *sessionPicker) prepareSearch(rows []listRow) {
	if l.words() == "" || l.searchRead || l.searchFailure != nil || (l.search == nil && l.searchContext == nil) {
		return
	}
	if l.searchContext == nil {
		l.searched, l.searchRead = withTableNumbers(rows, l.search()), true
		l.memo = nil
		return
	}
	if l.operation == nil {
		ctx, cancel := context.WithCancel(l.operationContext())
		generation := l.generation
		op := &browserOperation{cancel: cancel, done: make(chan browserOperationResult, 1)}
		l.operation = op
		loader, legacy := l.searchContext, l.search
		go func() {
			var result []listRow
			var err error
			if loader != nil {
				result, err = loader(ctx)
			} else {
				result = legacy()
				err = ctx.Err()
			}
			op.done <- browserOperationResult{rows: result, err: err, generation: generation}
		}()
	}
	select {
	case result := <-l.operation.done:
		l.operation.cancel()
		l.operation = nil
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
func (l *sessionPicker) loadOlder(p *prompter, stdout io.Writer) (bool, error) {
	l.stopOperations()
	l.operationCanceled = false
	ctx, cancel := context.WithCancel(l.operationContext())
	defer cancel()
	done := make(chan browserOperationResult, 1)
	loader := l.older
	if loader.LoadContext == nil {
		return loader.Load()
	}
	go func() {
		var more bool
		var err error
		if loader.LoadContext != nil {
			more, err = loader.LoadContext(ctx)
		} else {
			more, err = loader.Load()
		}
		done <- browserOperationResult{more: more, err: err}
	}()
	typedAhead := l.keys != nil && l.keys.buffered() || l.keys == nil && p.in.Buffered() > 0
	terminal.Println(stdout, "Loading "+loader.Label+"… Esc cancels")
	for {
		select {
		case result := <-done:
			return result.more, result.err
		case <-ctx.Done():
			cancel()
			<-done
			return false, ctx.Err()
		default:
		}
		if typedAhead {
			select {
			case result := <-done:
				return result.more, result.err
			case <-ctx.Done():
				cancel()
				<-done
				return false, ctx.Err()
			}
		}
		if l.keys == nil {
			ready := p.in.Buffered() > 0
			if !ready {
				var err error
				ready, err = browserLineReady(p.source)
				if err != nil {
					cancel()
					<-done
					return false, err
				}
			}
			if !ready {
				continue
			}
			answer, err := p.line("")
			if err != nil {
				cancel()
				<-done
				return false, err
			}
			switch strings.ToLower(strings.TrimSpace(answer)) {
			case "q", "quit":
				l.operationQuit = true
			case "a":
				l.toggled = true
			case "", "cancel":
			default:
				continue
			}
			l.operationCanceled = true
			cancel()
			result := <-done
			if result.err != nil && !errors.Is(result.err, context.Canceled) {
				return false, result.err
			}
			return true, nil
		}
		k, err := l.keys.nextWait(50 * time.Millisecond)
		if err != nil {
			cancel()
			<-done
			return false, err
		}
		if k.kind == keyEscape || k.kind == keyEndOfInput || (k.kind == keyRune && (k.r == 'q' || k.r == 'a')) {
			l.operationCanceled = true
			cancel()
			result := <-done
			if result.err != nil && !errors.Is(result.err, context.Canceled) {
				return false, result.err
			}
			if k.kind == keyEndOfInput || k.kind == keyRune && k.r == 'q' {
				l.operationQuit = true
			}
			if k.kind == keyRune && k.r == 'a' {
				l.toggled = true
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

func (l *sessionPicker) waitSearch(rows []listRow) error {
	for l.operation != nil {
		select {
		case <-l.operationContext().Done():
			l.stopOperations()
			return l.operationContext().Err()
		case result := <-l.operation.done:
			l.operation.cancel()
			l.operation = nil
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
	return nil
}

// readBrowserLine services completed search reads while waiting for cooked
// input. A completed search asks the caller to redraw without consuming input.
func (l *sessionPicker) readBrowserLine(p *prompter, question string, rows []listRow) (string, bool, error) {
	terminal.Print(p.out, question)
	for {
		if err := l.operationContext().Err(); err != nil {
			return "", false, err
		}
		pending := l.operation != nil
		l.prepareSearch(rows)
		if pending && l.operation == nil {
			return "", true, nil
		}
		ready := p.in.Buffered() > 0
		if !ready {
			var err error
			ready, err = browserLineReady(p.source)
			if err != nil {
				return "", false, err
			}
		}
		if ready {
			answer, err := p.line("")
			return answer, false, err
		}
	}
}

func (l *sessionPicker) toggleScope(p *prompter, stdout io.Writer, choices *scopeChoices) error {
	l.stopOperations()
	l.operationCanceled = false
	if choices.rowsForContext != nil {
		previous, target := l.older, 1-choices.current
		l.older = &browserLoadAction{Label: "scope sessions", LoadContext: func(ctx context.Context) (bool, error) {
			choices.built[target] = nil
			choices.choiceContext(ctx, target)
			if err := ctx.Err(); err != nil {
				choices.built[target] = nil
				return false, err
			}
			return true, nil
		}}
		_, err := l.loadOlder(p, stdout)
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
