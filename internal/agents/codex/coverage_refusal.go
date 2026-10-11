package codex

import "github.com/wangjohn/agent-archive/internal/agentapi"

// coverageIncomplete is the refusal the source gives while the rollout lookup
// has not yet proven which rollout files make up a thread's history: its
// lineage cannot be selected, a retained proof may miss a newer segment, or
// its historical candidates cannot be listed. It is named at its source with
// agentapi.CauseHistoryCoverage, so the collector defers the scan until a
// later pass completes coverage instead of failing it.
func coverageIncomplete(message string) error {
	return agentapi.WithCause(sourceFailure(agentapi.Unavailable, message), agentapi.CauseHistoryCoverage)
}

// coverageUnproven reports that set does not prove the thread's rollouts for
// this pass: coverage has not completed, or it rests on an earlier epoch's
// retained proof with no current locator, which can lack a segment created
// since. Without a lookup there is no coverage to wait for.
func (p *relatedSourcePass) coverageUnproven(set agentapi.CodexRolloutSet) bool {
	return p.env.CodexRollouts != nil && (!set.Complete || set.Retained && set.Current == nil)
}

// coverageRefusal refuses a history the set's rollouts do not resolve (a
// missing or ambiguous dependency, tip or original segment). While coverage
// is unproven that is a coverage refusal a later pass clears; on a proof of
// this pass it is a failure.
func (p *relatedSourcePass) coverageRefusal(set agentapi.CodexRolloutSet, message string) error {
	if p.coverageUnproven(set) {
		return coverageIncomplete(message)
	}
	return sourceFailure(agentapi.Unavailable, message)
}
