package backfill

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
)

// Import fixtures receive the same actual capability subset as CLI composition.
// Extension tests may still layer their own discovery on this base.
type importFixtureDiscovery struct{ *builtin.Registry }

func testImportDiscovery() agentapi.DiscoveryLookup {
	return importFixtureDiscovery{builtin.NewBuiltins()}
}

func (d importFixtureDiscovery) DiscoveryAgents() []string {
	var names []string
	for _, name := range d.Registry.DiscoveryAgents() {
		if _, ok := d.LookupImport(name); ok {
			names = append(names, name)
		}
	}
	return names
}
