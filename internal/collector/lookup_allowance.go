package collector

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// beginCodexLookupAllowance gives this collector pass its own Codex lookup
// allowance (agentapi.CodexRolloutAllowance), separate from whatever the
// lookup's earlier users in the same pass, discovery or import
// registration, spent of it.
func beginCodexLookupAllowance(opts Options) {
	if allowance, ok := opts.CodexRollouts.(agentapi.CodexRolloutAllowance); ok {
		allowance.BeginCollection()
	}
}

// codexLookupSpent reports whether reg is a Codex session that this pass can
// no longer read because the pass's Codex lookup allowance is used up: every
// lookup its scan needs would be refused. Such a session is left for the next
// pass without being opened, so the rest of the pass (Claude Code and Cursor
// sessions) does not wait on scans bound to fail.
func (p *pass) codexLookupSpent(reg archive.SessionRegistration) bool {
	if reg.Harness.Name != archive.HarnessCodex {
		return false
	}
	allowance, ok := p.opts.CodexRollouts.(agentapi.CodexRolloutAllowance)
	return ok && allowance.AllowanceSpent()
}
