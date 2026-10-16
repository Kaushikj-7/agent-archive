package agentapi

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"sync"
)

// DetachRetainedEnvelope gives supplemental observations and capture facts an
// independent owner; native records/text/ordinals remain borrowed from input.
func DetachRetainedEnvelope(ctx context.Context, bundle archive.SourceBundle, budget *NativeReadBudget) (archive.SourceBundle, func(), error) {
	reservation := budget.Reservation()
	envelope := bundle
	envelope.NativeRecords, envelope.NativeText, envelope.Ordinals = nil, nil, nil
	const scratch = 32 << 10
	if !reservation.Reserve(scratch) {
		return archive.SourceBundle{}, nil, ErrReadBudget
	}
	n, err := jsonwire.Bound(ctx, envelope, reservation.Available()/3)
	reservation.Release(scratch)
	if err != nil {
		return archive.SourceBundle{}, nil, err
	}
	if !reservation.Reserve(3 * n) {
		return archive.SourceBundle{}, nil, ErrReadBudget
	}
	raw, err := json.Marshal(envelope)
	var out archive.SourceBundle
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	if err != nil {
		reservation.Release(3 * n)
		return archive.SourceBundle{}, nil, err
	}
	reservation.Release(2 * n)
	out.NativeRecords, out.NativeText, out.Ordinals = bundle.NativeRecords, bundle.NativeText, bundle.Ordinals
	var once sync.Once
	return out, func() { once.Do(func() { reservation.Release(n) }) }, nil
}
