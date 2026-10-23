package state

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

const durableObligationCacheBytes int64 = 1 << 20

// DurableStorageObligationsOwned returns a complete charged result or explicit
// incompleteness. Close after every result/ID use; callers retaining IDs beyond
// that boundary must detach them into their own result owner.
func (s *Store) DurableStorageObligationsOwned(ctx context.Context) ([]DurableStorageObligation, func(), error) {
	budget := s.resourceBudget
	if budget == nil {
		budget = agentapi.NewNativeReadBudget(128 << 20)
	}
	s, ctx, closeContext := s.withDurableContext(ctx)
	read, closeRead := s.WithReadBudget(ctx, budget)
	out, err := read.durableStorageObligations(ctx)
	var once sync.Once
	return out, func() {
		once.Do(func() {
			closeRead()
			closeContext()
		})
	}, err
}

func (s *Store) appendDurableObligations(out []DurableStorageObligation, values ...DurableStorageObligation) ([]DurableStorageObligation, error) {
	for _, value := range values {
		if err := s.durableContext().Err(); err != nil {
			return out, err
		}
		if s.resourceBudget == nil {
			// The compatibility API has no close handle. Keep its legacy
			// detached values, with a finite standalone result allowance.
			if int64(len(out)+1)*(32+255) > 128<<20 {
				return out, errStateBudget
			}
			out = append(out, value)
			continue
		}
		owner := s.readReservation()
		charge := int64(len(value.SessionID)) + 64
		capacity := cap(out)
		if len(out) == capacity {
			capacity = max(8, 2*capacity)
			charge += int64(capacity) * 32
		}
		if !owner.Reserve(charge) {
			return out, errStateBudget
		}
		*s.resourceReleases = append(*s.resourceReleases, func() { owner.Release(charge) })
		if capacity != cap(out) {
			next := make([]DurableStorageObligation, len(out), capacity)
			copy(next, out)
			out = next
		}
		value.SessionID = strings.Clone(value.SessionID)
		out = append(out, value)
	}
	return out, nil
}

func (s *Store) copyDurableObligations(values []DurableStorageObligation) ([]DurableStorageObligation, error) {
	return s.appendDurableObligations(nil, values...)
}

// Cached names are bounded separately from completion. Scoped cache storage is
// charged to the same ledger and invalidated before its captured owner closes.
func (s *Store) cacheDurableObligations(values []DurableStorageObligation, generation uint64) {
	cache := s.durableInspection
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.generation != generation || !cache.summaryValid || cache.valid {
		return
	}
	charge := int64(0)
	if len(values) > 0 {
		charge = 512
	}
	for _, value := range values {
		charge += 32 + int64(len(value.SessionID))
	}
	if charge > durableObligationCacheBytes {
		return
	}
	owner := s.readReservation()
	if s.resourceBudget != nil && (charge > owner.Available() || !owner.Reserve(charge)) {
		return
	}
	var cached []DurableStorageObligation
	if len(values) > 0 {
		cached = make([]DurableStorageObligation, len(values))
	}
	for i, value := range values {
		value.SessionID = strings.Clone(value.SessionID)
		cached[i] = value
	}
	cache.generation++
	cache.obligations = cached
	cache.budget = s.resourceBudget
	cache.valid = !cache.legacyTemps
	if s.resourceBudget != nil {
		generation := cache.generation
		var once sync.Once
		release := func() { once.Do(func() { owner.Release(charge) }) }
		cache.release = release
		*s.resourceReleases = append(*s.resourceReleases, func() {
			cache.mu.Lock()
			if cache.generation == generation {
				cache.valid = false
				cache.obligations = nil
				cache.release = nil
				cache.budget = nil
			}
			cache.mu.Unlock()
			release()
		})
	}
}

// clearObligations releases only the optional cached names. Independently
// returned vectors keep their captured charges and remain valid until close.
// The caller holds cache.mu; fixed summary observations remain reusable.
func (cache *durableInspectionCache) clearObligations() {
	cache.generation++
	cache.valid = false
	cache.obligations = nil
	cache.budget = nil
	if cache.release != nil {
		cache.release()
		cache.release = nil
	}
}

func (s *Store) beginDurableObligations(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	// Optional names from an earlier result must not crowd this mandatory
	// census. Drop their exact owner before any allocations or refusals.
	if s.resourceBudget != nil && s.durableInspection != nil {
		cache := s.durableInspection
		cache.mu.Lock()
		cache.clearObligations()
		cache.mu.Unlock()
	}
	return nil
}
