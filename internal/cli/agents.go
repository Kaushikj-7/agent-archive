package cli

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"slices"
)

// Production composition happens once, without filesystem or runtime probes.

var productionAgents = builtin.NewBuiltins()

func (e Env) agentRegistry() *builtin.Registry {
	if e.Agents != nil {
		return e.Agents
	}
	return productionAgents
}

func registryFor(deps interface{}) *builtin.Registry {
	if d, ok := deps.(interface{ agentRegistry() *builtin.Registry }); ok {
		return d.agentRegistry()
	}
	return productionAgents
}

func catalogFor(deps interface{}) agentmeta.Catalog { return registryFor(deps).Catalog() }

func launchSupported(c agentmeta.Catalog, name string) bool {
	d, ok := c.Lookup(name)
	if !ok {
		return false
	}
	return slices.Contains(d.Operations, agentmeta.Launch)
}

func parsersFor(deps any) agentapi.ParsersLookup { return registryFor(deps) }

func previewsFor(deps any) agentapi.PreviewsLookup { return registryFor(deps) }

func (e Env) runtimeLookup() agentapi.RuntimeLookup { return e.agentRegistry() }

func (e Env) launcherLookup() agentapi.LauncherLookup { return e.agentRegistry() }

func (e Env) setupNames() []string {
	names := agentmeta.SetupNames(e.agentRegistry().Catalog())
	if e.MuseRootCaptureQualified {
		return names
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name != "muse" {
			out = append(out, name)
		}
	}
	return out
}
