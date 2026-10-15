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
	diagnostic func(error)
}

type passChanges struct {
	home        string
	destination string
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
	aggregate, cancel := context.WithTimeout(ctx, time.Duration(len(consumers))*sessionChangeSlice)
	defer cancel()
	for _, consumer := range consumers {
		if aggregate.Err() != nil {
			return
		}
		slice, done := context.WithTimeout(aggregate, sessionChangeSlice)
		err := consumer.sink.Hint(slice, SessionChangeBatch{DestinationID: destination, Sessions: append([]collector.PublicationCandidate(nil), bounded...)})
		done()
		if err != nil && consumer.diagnostic != nil {
			consumer.diagnostic(err)
		}
	}
}
