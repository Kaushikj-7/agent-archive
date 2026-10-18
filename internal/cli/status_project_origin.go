package cli

import "github.com/wangjohn/agent-archive/internal/sourcefacts"

// projectOriginEvidencePending recognizes only the closed origin failure set.
// Health outcomes contain counts, never native paths or raw Git origin values.
func projectOriginEvidencePending(outcomes map[string]int) bool {
	for _, outcome := range []sourcefacts.RecoveryOutcome{
		sourcefacts.RecoveryOriginCycle, sourcefacts.RecoveryOriginMissing,
		sourcefacts.RecoveryOriginUnreadable, sourcefacts.RecoveryOriginConflict,
		sourcefacts.RecoveryOriginUnsupported, sourcefacts.RecoveryOriginUnresolved,
		sourcefacts.RecoveryOriginLimit, sourcefacts.RecoveryOriginChanged,
	} {
		if outcomes[string(outcome)] > 0 {
			return true
		}
	}
	return false
}
