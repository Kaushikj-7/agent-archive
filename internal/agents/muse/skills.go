package muse

import (
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/skillconfig"
)

// Skills reuses shared installation, ownership and expected-byte planning.
// Evidence roots only prove installation, never native eligibility or use.
func Skills() agentapi.SkillProvider {
	return skillProvider{Provider: skillconfig.Provider{ManagedSuffix: ".agents/skills"}}
}

type skillProvider struct{ skillconfig.Provider }

func (skillProvider) EvidenceRoots(l agentapi.SkillLocations) []agentapi.SkillRoot {
	var roots []agentapi.SkillRoot
	add := func(base, suffix, scope string, project bool) {
		if !validSkillRoot(base) {
			return
		}
		userBoundary, projectBoundary := base, ""
		if project {
			userBoundary, projectBoundary = "", base
		}
		roots = append(roots, agentapi.SkillRoot{Path: filepath.Join(base, suffix), Scope: scope, UserBoundary: userBoundary, ProjectBoundary: projectBoundary})
	}
	if validSkillRoot(l.UserHome) {
		if base, ok := configuredSkillRoot(l.Environment, "XDG_CONFIG_HOME", filepath.Join(l.UserHome, ".config")); ok {
			add(base, "muse/skills", "user_muse_config", false)
		}
		add(l.UserHome, ".agents/skills", "user_agents", false)
		add(l.UserHome, ".claude/skills", "user_claude_foreign", false)
		if base, ok := configuredSkillRoot(l.Environment, "CODEX_HOME", filepath.Join(l.UserHome, ".codex")); ok {
			add(base, "skills", "user_codex_foreign", false)
		}
	}
	add(l.ProjectRoot, ".agents/skills", "project_agents", true)
	add(l.ProjectRoot, ".codex/skills", "project_codex_foreign", true)
	add(l.ProjectRoot, ".claude/skills", "project_claude_foreign", true)
	return roots
}

func configuredSkillRoot(env *agentapi.RuntimeEnvironment, key, fallback string) (string, bool) {
	if env == nil || env.LookupEnv == nil {
		return "", false
	}
	value, set := env.LookupEnv(key)
	if !set {
		value = fallback
	}
	return value, validSkillRoot(value)
}

func validSkillRoot(path string) bool {
	return len(path) > 0 && len(path) <= 4096 && utf8.ValidString(path) && !strings.ContainsRune(path, 0) && filepath.IsAbs(path)
}
