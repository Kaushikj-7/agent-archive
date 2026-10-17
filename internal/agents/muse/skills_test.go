package muse

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
	"github.com/wangjohn/agent-archive/internal/skillownership"
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
)

func TestSkillsConformance(t *testing.T) {
	t.Parallel()
	agenttest.Skills(t, Skills(), []byte(skillownership.Marker+"\narchive command"))
}

func TestSkillsShareInstallationButSeparateInventory(t *testing.T) {
	t.Parallel()
	l := agentapi.SkillLocations{UserHome: "/synthetic/home", ProjectRoot: "/synthetic/project", ClaudeDirectory: "/synthetic/foreign"}
	want := codex.Skills().Destination(l)
	if Skills().Destination(l) != want || cursor.Skills().Destination(l) != want {
		t.Fatal("shared installation diverged")
	}
	roots := Skills().EvidenceRoots(l)
	if len(roots) != 2 || roots[0].Path != want.Directory || roots[1].Path != "/synthetic/project/.agents/skills" || roots[0].UserBoundary != l.UserHome || roots[1].ProjectBoundary != l.ProjectRoot {
		t.Fatalf("roots: %+v", roots)
	}
	if got := Skills().EvidenceRoots(agentapi.SkillLocations{}); len(got) != 0 {
		t.Fatalf("invented roots: %+v", got)
	}
	roots[0].Path = "mutated"
	if reflect.DeepEqual(roots, Skills().EvidenceRoots(l)) {
		t.Fatal("shared caller state")
	}
}

func TestSkillPlansPreserveForeignFilesAndExpectedBytes(t *testing.T) {
	t.Parallel()
	owned := []byte(skillownership.Marker + "\narchive command")
	file := agentapi.HookFile{Path: "/synthetic/home/.agents/skills/archive/SKILL.md", Present: true, Regular: true, Mode: 0640, Bytes: owned}
	next := append(bytes.Clone(owned), []byte("\nupdated")...)
	changes, err := Skills().Plan(agentapi.SkillPlanRequest{Action: agentapi.SkillRefresh, File: file, Content: next})
	if err != nil || len(changes) != 1 || !bytes.Equal(changes[0].Before, owned) || !bytes.Equal(changes[0].After, next) || changes[0].Mode != 0640 {
		t.Fatalf("expected-byte refresh: %+v %v", changes, err)
	}
	for _, content := range [][]byte{[]byte("foreign skill"), []byte(`{"mcpServers":{"foreign":{}},"hooks":{"foreign":[]}}`)} {
		file.Bytes = content
		for _, action := range []agentapi.SkillAction{agentapi.SkillInstall, agentapi.SkillRefresh, agentapi.SkillRemove} {
			changes, err = Skills().Plan(agentapi.SkillPlanRequest{Action: action, File: file, Content: next})
			if err != nil || len(changes) != 0 {
				t.Fatalf("foreign file changed: %+v %v", changes, err)
			}
		}
	}
	file.Bytes = owned
	file.Regular = false
	changes, err = Skills().Plan(agentapi.SkillPlanRequest{Action: agentapi.SkillRemove, File: file})
	if err != nil || len(changes) != 0 {
		t.Fatal("nonregular file removed")
	}
}
