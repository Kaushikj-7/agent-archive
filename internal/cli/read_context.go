package cli

import (
	"context"
	"io"
	"os"
	"sync"
)

// readCommandContext cancels remote reads when the user interrupts. It has
// no whole-command deadline: each provider read/page has its own budget, so
// browsing and scanning a large archive may take as long as needed.
func readCommandContext(env interface {
	interrupts() (<-chan os.Signal, func())
}) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	signals, stop := env.interrupts()
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-signals:
			cancel()
		case <-ctx.Done():
		}
	}()
	var once sync.Once
	return ctx, func() { once.Do(func() { cancel(); stop(); <-done }) }
}

// writeOutput treats a writer's short successful write as failure.
func writeOutput(w io.Writer, data []byte) error {
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}
