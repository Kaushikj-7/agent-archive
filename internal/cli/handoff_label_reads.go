package cli

import (
	"context"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// handoffLabelReaders bounds the native label reads the handoff picker runs
// at once. Each read filters and parses a whole transcript, so reading the
// rows one after another kept the picker blank for the sum of their times.
const handoffLabelReaders = 4

type labelReadFunc func(context.Context, archive.SessionRegistration, time.Time) (archive.Metadata, bool, error)

type labelWant struct {
	key    localLabelKey
	reg    archive.SessionRegistration
	active time.Time
}

type labelRead struct {
	done      chan struct{}
	metadata  archive.Metadata
	hasPrompt bool
	err       error
}

type labelJob struct {
	want   labelWant
	result *labelRead
}

// labelReads runs a picker's label reads ahead of the row loop that consumes
// them in order. It changes only when a read happens: the loop still decides,
// one row at a time, whether to use a result, and counts it against its
// budgets as it would have counted reading it itself. The loop plans the
// reads it would make if every session read had a prompt, and plans again
// after each one that does not.
type labelReads struct {
	parent context.Context
	read   labelReadFunc
	// started and taken are used only by the row loop's goroutine.
	started map[localLabelKey]*labelRead
	taken   map[localLabelKey]bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	mu      sync.Mutex
	queue   []labelJob
	workers int
}

func newLabelReads(ctx context.Context, read labelReadFunc) *labelReads {
	return &labelReads{parent: ctx, read: read, started: map[localLabelKey]*labelRead{}, taken: map[localLabelKey]bool{}}
}

// concurrentLabel reports whether reg's label may be read beside others. A
// Cursor database chat is read through its own snapshot of the whole
// database, so those are left to the row loop, one at a time.
func concurrentLabel(reg archive.SessionRegistration) bool {
	return reg.ReadsTranscriptFile()
}

// ensure starts, in order, each of wants not already started or taken, on at
// most handoffLabelReaders workers.
func (r *labelReads) ensure(wants []labelWant) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx == nil && len(wants) > 0 {
		r.ctx, r.cancel = context.WithCancel(r.parent)
	}
	for _, w := range wants {
		if _, ok := r.started[w.key]; ok || r.taken[w.key] {
			continue
		}
		result := &labelRead{done: make(chan struct{})}
		r.started[w.key] = result
		r.queue = append(r.queue, labelJob{want: w, result: result})
	}
	// Busy workers take queued jobs when they finish; idle slots start now.
	for range min(handoffLabelReaders-r.workers, len(r.queue)) {
		r.workers++
		r.wg.Go(r.work)
	}
}

func (r *labelReads) work() {
	for {
		r.mu.Lock()
		if len(r.queue) == 0 {
			r.workers--
			r.mu.Unlock()
			return
		}
		j := r.queue[0]
		r.queue = r.queue[1:]
		r.mu.Unlock()
		if err := r.ctx.Err(); err != nil {
			j.result.err = err
		} else {
			j.result.metadata, j.result.hasPrompt, j.result.err = r.read(r.ctx, j.want.reg, j.want.active)
		}
		close(j.result.done)
	}
}

// take returns the read started for key, waiting for it to finish. ok is
// false when none was started, and the caller reads the label itself.
func (r *labelReads) take(key localLabelKey) (metadata archive.Metadata, hasPrompt bool, err error, ok bool) {
	if r == nil {
		return archive.Metadata{}, false, nil, false
	}
	result, ok := r.started[key]
	if !ok {
		return archive.Metadata{}, false, nil, false
	}
	delete(r.started, key)
	r.taken[key] = true
	<-result.done
	return result.metadata, result.hasPrompt, result.err, true
}

// stop cancels the reads nobody took and waits for every worker, so no read
// outlives the operation that started it.
func (r *labelReads) stop() {
	if r == nil || r.cancel == nil {
		return
	}
	r.cancel()
	r.wg.Wait()
}
