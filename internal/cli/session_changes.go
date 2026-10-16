package cli

import (
	"context"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
)

const sessionChangeLimit = 256

const sessionChangeSlice = 20 * time.Millisecond

// SessionChangeBatch carries captured destination and bounded publication identities.
// These are hints to recheck authority, never a guarantee of continued existence.
type SessionChangeBatch struct {
	DestinationID string
	Sessions      []collector.PublicationCandidate
}

// SessionChangeSink performs cooperative bounded local invalidation only. It must
// honor cancellation and must not fetch sources, parse transcripts, or retain
// mutable caller slices. No goroutine can preempt an arbitrary implementation.
type SessionChangeSink interface {
	Hint(context.Context, SessionChangeBatch) error
}

type sessionChangeConsumer struct {
	sink       SessionChangeSink
	diagnostic func(context.Context, error)
}

type passChanges struct {
	home        string
	destination string
	namespace   string
	enabled     bool
}

// dispatchSessionChanges reserves a slice for each of at most two consumers.
// It remains synchronous so exiting capture commands leave no detached workers.
func dispatchSessionChanges(ctx context.Context, destination string, candidates []collector.PublicationCandidate, consumers []sessionChangeConsumer) {
	if destination == "" || len(candidates) == 0 || len(consumers) == 0 {
		return
	}
	bounded := make([]collector.PublicationCandidate, 0, min(sessionChangeLimit, len(candidates)))
	seen := make(map[collector.PublicationCandidate]bool)
	for _, candidate := range candidates[:min(len(candidates), 4*sessionChangeLimit)] {
		if candidate.Harness == "" || candidate.ArchiveSessionID == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		bounded = append(bounded, candidate)
		if len(bounded) == sessionChangeLimit {
			break
		}
	}
	if len(bounded) == 0 {
		return
	}
	consumers = consumers[:min(len(consumers), 2)]
	// Reserve an aggregate budget as disjoint per-consumer slices. Each child
	// gets its own reservation: one consumer cannot spend another's deadline.
	remaining := time.Duration(len(consumers)) * sessionChangeSlice
	for _, consumer := range consumers {
		if ctx.Err() != nil {
			return
		}
		allowance := min(sessionChangeSlice, remaining)
		remaining -= allowance
		slice, done := context.WithTimeout(ctx, allowance)
		err := consumer.sink.Hint(slice, SessionChangeBatch{DestinationID: destination, Sessions: append([]collector.PublicationCandidate(nil), bounded...)})
		if err != nil && consumer.diagnostic != nil && slice.Err() == nil {
			consumer.diagnostic(slice, err)
		}
		done()
	}
}
