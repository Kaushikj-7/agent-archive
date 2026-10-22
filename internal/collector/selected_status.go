package collector

import (
	"slices"

	"github.com/wangjohn/agent-archive/internal/state"
)

// selectedStatus updates existing owner facts without pretending that a scoped
// publication observed the whole collection queue or its native aggregates.
func (p *pass) selectedStatus(previous, current state.Status) state.Status {
	selected := make(map[string]bool)
	known := make(map[string]bool, len(p.registrations))
	current.PendingCount = 0
	for _, reg := range p.registrations {
		id := reg.ArchiveSessionID
		known[id] = true
		accepted := p.opts.AcceptSession == nil || p.opts.AcceptSession(reg)
		if p.opts.selectsSession(reg) && accepted {
			selected[id] = true
		}
		_, requested, requestErr := p.local.LoadRequest(id)
		owed, err := p.local.Outstanding(reg, requested)
		// Policy can hold a first-stage import without settling its retained
		// evidence. Unknown local obligations also remain conservatively owed.
		if requestErr != nil || err != nil || p.unreadable[id] || p.durableCounted[id] || (accepted || owed.StagedImport) && owed.Pending() {
			current.PendingCount++
		}
	}
	// Unreadable registrations and orphaned durable owners remain pending too;
	// both inventories already belong to this pass and need no source reads.
	for id := range p.registered {
		if !known[id] {
			known[id] = true
			current.PendingCount++
		}
	}
	for _, obligation := range p.durableObligations {
		if !known[obligation.SessionID] {
			known[obligation.SessionID] = true
			current.PendingCount++
		}
	}
	current.DeferredSessions = preserveUnselected(previous.DeferredSessions, current.DeferredSessions, selected)
	current.DeferredSince = preserveUnselected(previous.DeferredSince, current.DeferredSince, selected)
	current.NextRetryAt = preserveUnselected(previous.NextRetryAt, current.NextRetryAt, selected)
	current.UnreachedSessions = preserveUnselected(previous.UnreachedSessions, current.UnreachedSessions, selected)
	current.SessionIssues = preserveUnselected(previous.SessionIssues, current.SessionIssues, selected)
	current.SessionIssueReasons = preserveUnselected(previous.SessionIssueReasons, current.SessionIssueReasons, selected)
	current.SubagentIssues = preserveUnselected(previous.SubagentIssues, current.SubagentIssues, selected)
	for _, code := range current.SessionIssues {
		if current.IssueCounts == nil {
			current.IssueCounts = map[string]int{}
		}
		current.IssueCounts[code]++
	}
	// The queue's newcomer ordering still uses the last whole-pass observation.
	// Each retry's own saved next time independently records a selected attempt.
	current.LastScanAt = previous.LastScanAt
	current.WaitingSubagents = previous.WaitingSubagents
	current.RunningSubagents = previous.RunningSubagents
	current.PendingNativeLinks = previous.PendingNativeLinks
	current.NativeLinkPendingReasons = previous.NativeLinkPendingReasons
	problems := current.LastErrors
	current.SetLastErrors(previous.LastErrors...)
	if len(previous.LastErrors) == 0 && previous.LastError != "" {
		current.AddLastError(previous.LastError)
	}
	for _, problem := range problems {
		if !slices.Contains(current.LastErrors, problem) {
			current.AddLastError(problem)
		}
	}
	return current
}

func preserveUnselected[T any](previous, current map[string]T, selected map[string]bool) map[string]T {
	for id, value := range previous {
		if !selected[id] {
			if current == nil {
				current = make(map[string]T)
			}
			current[id] = value
		}
	}
	return current
}
