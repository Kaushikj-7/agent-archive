package muse

import "github.com/wangjohn/agent-archive/internal/agents/skillconfig"

// Skills reuses the shared managed installation and ownership planner. Evidence
// roots only request installed inventory; neither presence nor a successful
// installation establishes native activation, eligibility, invocation or read.
// Configuration-local, foreign and plugin roots require separately injected
// locations and qualified owner/trust/activation evidence before use.
func Skills() skillconfig.Provider {
	return skillconfig.Provider{ManagedSuffix: ".agents/skills", Roots: []skillconfig.Root{
		{Suffix: ".agents/skills", Scope: "user_agents"},
		{Suffix: ".agents/skills", Scope: "project_agents", Project: true},
	}}
}
