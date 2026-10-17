package collector

import (
	"reflect"
	"sort"
)

// A hint orders only already loaded startup work. Every candidate still passes
// the unchanged root, consent, source, currentness and guarded admission checks.
func (p *pass) orderStartupCandidates() {
	if p.ctx == nil || p.ctx.Err() != nil || len(p.startupCandidates) < 2 || len(p.startupCandidates) > 128 || p.opts.NativeLocatorFactory == nil || p.opts.sourcePasses == nil || p.opts.sourcePasses.env.ReadBudget == nil {
		return
	}
	ordering, ok := p.opts.NativeLocators.(interface{ PendingOrder() []string })
	if !ok {
		return
	}
	// Mirror the factory's immutable query domain, including ordinary pending
	// owners whose hints are valid but whose execution slots must not move.
	count := len(p.startupCandidates)
	for _, reg := range p.registrations {
		if p.opts.AcceptSession != nil && !p.opts.AcceptSession(reg) {
			continue
		}
		if pendingNativeEligible(reg, p.opts) {
			count++
		}
	}
	if count > 8192 {
		return
	}
	// PendingOrder returns at most the existing 8192-owner vector. Borrow its
	// strings and charge headers plus conservative rank-map scratch before any
	// allocation, using the same 128-byte/query scratch allowance as the factory.
	charge := int64(8192)*int64(reflect.TypeFor[string]().Size()) + int64(count)*128 + 4096
	reservation := p.opts.sourcePasses.env.ReadBudget.Reservation()
	if !reservation.Reserve(charge) {
		return
	}
	defer reservation.Release(charge)
	rank := make(map[string]int, count)
	for _, candidate := range p.startupCandidates {
		rank[candidate.intent.NativeSessionID] = -1
	}
	for _, reg := range p.registrations {
		if p.opts.AcceptSession != nil && !p.opts.AcceptSession(reg) {
			continue
		}
		if pendingNativeEligible(reg, p.opts) {
			rank[reg.NativeSessionID] = -1
		}
	}
	order := ordering.PendingOrder()
	if len(order) == 0 || len(order) > 8192 {
		return
	}
	for i, id := range order {
		if p.ctx.Err() != nil {
			return
		}
		previous, known := rank[id]
		if !known || previous >= 0 {
			return // Unknown or duplicate hints never modify the loaded work.
		}
		rank[id] = i
	}
	if p.ctx.Err() != nil {
		return
	}
	sort.SliceStable(p.startupCandidates, func(i, j int) bool {
		a := rank[p.startupCandidates[i].intent.NativeSessionID]
		b := rank[p.startupCandidates[j].intent.NativeSessionID]
		if (a >= 0) != (b >= 0) {
			return a >= 0
		}
		return a >= 0 && a < b
	})
}
