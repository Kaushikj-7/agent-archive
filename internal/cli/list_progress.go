package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/wangjohn/agent-archive/internal/statsfmt"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// listProgress counts what a listing has done so far, for its spinner: the
// sessions the bucket listing has found as its pages arrive, then the
// metadata sidecars read of those it needs. Both are reported from several
// goroutines at once.
type listProgress struct {
	mu   sync.Mutex
	seen map[string]struct{}

	found atomic.Int64
	done  atomic.Int64
	total atomic.Int64
}

func newListProgress() *listProgress {
	return &listProgress{seen: make(map[string]struct{})}
}

type listProgressKey struct{}

// withListProgress has ctx carry p: the store reports listed pages to it,
// and readers that build their own reader.ListOptions find it with
// listReadProgress.
func withListProgress(ctx context.Context, p *listProgress) context.Context {
	ctx = storage.WithListObserver(ctx, p.observe)
	return context.WithValue(ctx, listProgressKey{}, p)
}

// listReadProgress is the reader.ListOptions Progress of the listProgress ctx
// carries, or nil.
func listReadProgress(ctx context.Context) func(done, total int) {
	if p, ok := ctx.Value(listProgressKey{}).(*listProgress); ok {
		return p.read
	}
	return nil
}

// observe counts the sessions among one listed page: a session has one
// metadata sidecar. A key is counted once however many listings see it, as a
// command may list the same prefix again (a fallback after a failed read).
func (p *listProgress) observe(objects []storage.Object) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, object := range objects {
		if !strings.HasPrefix(object.Key, archiveSessionsPrefix+"/") || !strings.HasSuffix(object.Key, "/metadata.json") {
			continue
		}
		if _, ok := p.seen[object.Key]; ok {
			continue
		}
		p.seen[object.Key] = struct{}{}
		p.found.Add(1)
	}
}

// read records done of total sidecars read. Calls arrive out of order, so it
// keeps the highest done.
func (p *listProgress) read(done, total int) {
	p.total.Store(int64(total))
	for cur := p.done.Load(); int64(done) > cur; cur = p.done.Load() {
		if p.done.CompareAndSwap(cur, int64(done)) {
			break
		}
	}
}

// label is the spinner's text: the plain label until something is counted,
// then how many sessions the listing has scanned (every session in the
// bucket, not only those that will match), then how many of the sidecars it
// needs have been loaded.
func (p *listProgress) label(base string) string {
	if total := p.total.Load(); total > 0 {
		return fmt.Sprintf("%s loading %s of %s", base, statsfmt.CommaInt(p.done.Load()), statsfmt.CommaInt(total))
	}
	if found := p.found.Load(); found > 0 {
		return fmt.Sprintf("%s scanned %s", base, statsfmt.CommaInt(found))
	}
	return base
}

// startListActivity is startActivity with a label that shows p's counts.
func startListActivity(out io.Writer, base string, p *listProgress) (stop func()) {
	span := trace.Start(spinnerSpan)
	sp := activityStyle(out).spinLabelEvery(out, func() string { return p.label(base) }, spinnerInterval)
	return func() {
		sp.stop()
		span.End()
	}
}

// startListProgress starts list's "Listing sessions…" spinner with the
// counts of the progress it returns; a --json listing draws none.
func startListProgress(stdout io.Writer, jsonOut bool) (*listProgress, func()) {
	p := newListProgress()
	if jsonOut {
		return p, func() {}
	}
	return p, startListActivity(stdout, "Listing sessions…", p)
}
