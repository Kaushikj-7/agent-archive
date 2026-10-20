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

// labelReads runs a picker's label reads ahead of the row loop that consumes
// them in order. It changes only when a read happens: the loop still decides,
// one row at a time, whether to use a result, and counts it against its
// budgets as it would have counted reading it itself.
type labelReads struct {
	reads  map[localLabelKey]*labelRead
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// startLabelReads starts reading wants, in order, with at most
// handoffLabelReaders at once. Fewer than two wants need no workers, so it
// returns nil, which take and stop accept.
func startLabelReads(ctx context.Context, read labelReadFunc, wants []labelWant) *labelReads {
	if len(wants) < 2 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &labelReads{reads: make(map[localLabelKey]*labelRead, len(wants)), cancel: cancel}
	type job struct {
		want   labelWant
		result *labelRead
	}
	queue := make(chan job, len(wants))
	for _, w := range wants {
		if _, dup := r.reads[w.key]; dup {
			continue
		}
		result := &labelRead{done: make(chan struct{})}
		r.reads[w.key] = result
		queue <- job{want: w, result: result}
	}
	close(queue)
	for range min(handoffLabelReaders, len(r.reads)) {
		r.wg.Go(func() {
			for j := range queue {
				if err := ctx.Err(); err != nil {
					j.result.err = err
				} else {
					j.result.metadata, j.result.hasPrompt, j.result.err = read(ctx, j.want.reg, j.want.active)
				}
				close(j.result.done)
			}
		})
	}
	return r
}

// take returns the read started for key, waiting for it to finish. ok is
// false when none was started, and the caller reads the label itself.
func (r *labelReads) take(key localLabelKey) (metadata archive.Metadata, hasPrompt bool, err error, ok bool) {
	if r == nil {
		return archive.Metadata{}, false, nil, false
	}
	result, ok := r.reads[key]
	if !ok {
		return archive.Metadata{}, false, nil, false
	}
	delete(r.reads, key)
	<-result.done
	return result.metadata, result.hasPrompt, result.err, true
}

// stop cancels the reads nobody took and waits for every worker, so no read
// outlives the operation that started it.
func (r *labelReads) stop() {
	if r == nil {
		return
	}
	r.cancel()
	r.wg.Wait()
}
