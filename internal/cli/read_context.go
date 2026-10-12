package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// readCommandContext cancels remote reads when the user interrupts. It has
// no whole-command deadline: each provider read/page has its own budget, so
// browsing and scanning a large archive may take as long as needed.
type readSignalSource interface {
	interrupts() (<-chan os.Signal, func())
}

type readPhaseKey struct{}

// readPhaseOwner keeps one view context while signal ownership changes phases.
type readPhaseOwner struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	env       readSignalSource
	closed    bool
	watchStop func()
}

func readCommandContext(env readSignalSource) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	owner := &readPhaseOwner{ctx: ctx, cancel: cancel, env: env}
	ctx = context.WithValue(ctx, readPhaseKey{}, owner)
	owner.ctx = ctx
	owner.Resume()
	return ctx, owner.Close
}

func readPhase(ctx context.Context) *readPhaseOwner {
	owner, _ := ctx.Value(readPhaseKey{}).(*readPhaseOwner)
	return owner
}

// Pause unregisters and joins the watcher without ending retained view authority.
func (o *readPhaseOwner) Pause() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pauseLocked()
}

func (o *readPhaseOwner) pauseLocked() {
	if o.watchStop != nil {
		o.watchStop()
		o.watchStop = nil
	}
}

func (o *readPhaseOwner) Resume() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.ctx.Err() != nil || o.watchStop != nil {
		return
	}
	signals, stop := o.env.interrupts()
	paused, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-signals:
			o.cancel()
		case <-o.ctx.Done():
		case <-paused:
		}
	}()
	o.watchStop = func() { close(paused); stop(); <-done }
}

func (o *readPhaseOwner) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	o.cancel()
	o.pauseLocked()
}

// writeOutput treats a writer's short successful write as failure.
func writeOutput(w io.Writer, data []byte) error {
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

// writeCompactJSON preserves machine commands' compact JSON contract, while
// escaping display controls and checking the entire document was delivered.
func writeCompactJSON(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeOutput(w, append(archive.DisplayJSON(data), '\n'))
}
