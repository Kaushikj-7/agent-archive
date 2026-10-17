package cli

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"slices"
)

func runtimeObservations(env currentSessionDependencies) []agentapi.AgentRuntime {
	return env.runtimeLookup().Observations(agentapi.RuntimeEnvironment{LookupEnv: env.lookupEnv})
}

func launchEnvironmentKeys(env interface{ runtimeLookup() agentapi.RuntimeLookup }) []string {
	return slices.Concat(env.runtimeLookup().SessionEnvironmentKeys(), []string{envTrace, archive.ReplayEnv})
}

func projectRuntime(env currentSessionDependencies, harness string) string {
	for _, observation := range runtimeObservations(env) {
		if observation.ProjectLatest && (harness == "" || harness == string(observation.Agent)) {
			return string(observation.Agent)
		}
	}
	return ""
}
