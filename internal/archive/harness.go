package archive

import (
	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

// Established capture harness constants, by the canonical
// name the archive records and compares.
const (
	HarnessClaude = string(agentmeta.Claude)
	HarnessCodex  = string(agentmeta.Codex)
	HarnessCursor = string(agentmeta.Cursor)
)

// CanonicalHarness returns the name the archive uses for a harness name from
// a hook, a flag, a configuration, or stored metadata: trimmed, lower-cased,
// with an alias read as its canonical name ("Claude-Code" is "claude"). An
// unknown name comes back normalized the same way, not rejected; KnownHarness
// says whether the identity is recognized; implemented ports determine support.
// Harness comparisons use it so two spellings of one application agree.
func CanonicalHarness(name string) string {
	return agentmeta.Canonical(agentmeta.Builtins(), name)
}

// KnownHarness returns CanonicalHarness(name) and whether it is an
// application identity recognized by this build; this does not grant capture.
func KnownHarness(name string) (string, bool) {
	name = CanonicalHarness(name)
	_, known := agentmeta.Builtins().Lookup(name)
	return name, known
}
