package agentapi

import "github.com/wangjohn/agent-archive/internal/filechange"

// SkillLocations contains observed locations, without filesystem operations.
type SkillLocations struct {
	UserHome        string
	ProjectRoot     string
	ClaudeDirectory string
	// Environment is an optional fixed snapshot of XDG_CONFIG_HOME and CODEX_HOME.
	// Nil means unknown, distinct from a known-unset variable.
	Environment *RuntimeEnvironment
}

// SkillRoot declares one purpose-specific inventory location and its boundary.
type SkillRoot struct {
	Path            string
	Scope           string
	UserBoundary    string
	ProjectBoundary string
}

// SkillDestination declares managed installation conventions separately from evidence.
type SkillDestination struct {
	Boundary          string
	Directory         string
	ClaudeFrontmatter bool
}

// SkillAction selects a typed skill mutation.
type SkillAction uint8

// SkillInstall, SkillRefresh and SkillRemove select planning operations without performing writes.
const (
	SkillInstall SkillAction = iota + 1
	SkillRefresh
	SkillRemove
)

// SkillPlanRequest supplies caller-observed bytes and shared rendered templates.
type SkillPlanRequest struct {
	Action   SkillAction
	File     HookFile
	Content  []byte
	DataHome string
}

// SkillInspectionRequest is independent of mutation planning.
type SkillInspectionRequest struct {
	File     HookFile
	Content  []byte
	DataHome string
}

// SkillInspection preserves absent, foreign, unreadable and stale distinctions.
type SkillInspection struct {
	State  HookState
	Stale  bool
	Reason string
}

// SkillProvider owns native managed locations and evidence inventory declarations.
// Plan and Inspect are pure: shared orchestration performs all reads and writes.
type SkillProvider interface {
	Destination(SkillLocations) SkillDestination
	EvidenceRoots(SkillLocations) []SkillRoot
	Plan(SkillPlanRequest) ([]filechange.Change, error)
	Inspect(SkillInspectionRequest) (SkillInspection, error)
}

// SkillsLookup projects only implemented skill providers.
type SkillsLookup interface {
	LookupSkills(string) (SkillProvider, bool)
	SkillAgents() []string
}
