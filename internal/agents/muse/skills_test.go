package muse

import (
	"bytes"
	"reflect"
	"strings"
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
	if len(roots) != 5 || roots[0].Path != want.Directory || roots[2].Path != "/synthetic/project/.agents/skills" || roots[0].UserBoundary != l.UserHome || roots[2].ProjectBoundary != l.ProjectRoot {
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

func TestSkillEvidenceRootsRespectExactEnvironmentSnapshot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		env    *agentapi.RuntimeEnvironment
		config string
		codex  string
	}{
		{"unknown", nil, "", ""},
		{"unset", &agentapi.RuntimeEnvironment{LookupEnv: func(string) (string, bool) { return "", false }}, "/synthetic/home/.config/muse/skills", "/synthetic/home/.codex/skills"},
		{"custom", &agentapi.RuntimeEnvironment{LookupEnv: func(k string) (string, bool) {
			if k == "XDG_CONFIG_HOME" {
				return "/synthetic/xdg", true
			}
			return "/synthetic/codex", true
		}}, "/synthetic/xdg/muse/skills", "/synthetic/codex/skills"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			roots := Skills().EvidenceRoots(agentapi.SkillLocations{UserHome: "/synthetic/home", Environment: tc.env})
			got := map[string]string{}
			for _, r := range roots {
				got[r.Scope] = r.Path
				if r.ProjectBoundary != "" {
					t.Fatal("user root became project")
				}
			}
			if got["user_muse_config"] != tc.config || got["user_codex_foreign"] != tc.codex {
				t.Fatalf("roots %+v", roots)
			}
			for _, r := range Skills().EvidenceRoots(agentapi.SkillLocations{ProjectRoot: "/synthetic/project", Environment: tc.env}) {
				if r.UserBoundary != "" {
					t.Fatal("project-only observation invented user root")
				}
			}
		})
	}
	for _, value := range []string{"", "relative", "/invalid\x00root", "/invalid\xffroot", "/" + strings.Repeat("x", 4096)} {
		env := &agentapi.RuntimeEnvironment{LookupEnv: func(string) (string, bool) { return value, true }}
		roots := Skills().EvidenceRoots(agentapi.SkillLocations{UserHome: "/synthetic/home", Environment: env})
		if len(roots) != 2 {
			t.Fatalf("invalid configured root %q: %+v", value, roots)
		}
		if len(Skills().EvidenceRoots(agentapi.SkillLocations{UserHome: value, ProjectRoot: value, Environment: env})) != 0 {
			t.Fatal("invalid home/project resolved against cwd")
		}
	}
}
