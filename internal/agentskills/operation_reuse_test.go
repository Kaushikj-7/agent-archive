package agentskills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
)

type countedSkillPorts struct {
	agentapi.SkillsLookup
	enumerations int
	lookups      int
	inspections  int
}

func (p *countedSkillPorts) SkillAgents() []string {
	p.enumerations++
	return p.SkillsLookup.SkillAgents()
}

func (p *countedSkillPorts) LookupSkills(name string) (agentapi.SkillProvider, bool) {
	p.lookups++
	provider, ok := p.SkillsLookup.LookupSkills(name)
	return countedSkillProvider{SkillProvider: provider, ports: p}, ok
}

type countedSkillProvider struct {
	agentapi.SkillProvider
	ports *countedSkillPorts
}

func (p countedSkillProvider) Inspect(r agentapi.SkillInspectionRequest) (agentapi.SkillInspection, error) {
	p.ports.inspections++
	return p.SkillProvider.Inspect(r)
}

func TestInventoryProjectsProvidersOnceAndRendersEachTemplateOnce(t *testing.T) {
	ports := &countedSkillPorts{SkillsLookup: builtin.NewBuiltins()}
	renders := [2]int{}
	skills := []Skill{{Name: "synthetic", Render: func(destination Destination, _, _ string) []byte {
		renders[destination]++
		return []byte{byte(destination)}
	}}}
	files := inventorySkillFiles(ports, skills, t.TempDir(), "", "", "")
	if ports.enumerations != 1 || ports.lookups != 4 || renders != [2]int{1, 1} || len(files) != 4 {
		t.Fatalf("enumerations=%d lookups=%d renders=%v alternatives=%d", ports.enumerations, ports.lookups, renders, len(files))
	}
	// Inventory keeps separate owners, even for identical paths and templates.
	if files[1].Path != files[2].Path || files[1].Path != files[3].Path || files[1].Harnesses[0] == files[2].Harnesses[0] || files[2].Harnesses[0] == files[3].Harnesses[0] {
		t.Fatalf("lost shared-path ownership alternatives: %+v", files)
	}
	files[1].Content[0] = 99
	if files[2].Content[0] == 99 || files[3].Content[0] == 99 {
		t.Fatal("render reuse exposed aliased output content")
	}
}

func TestInstalledInspectsEverySharedPathOwner(t *testing.T) {
	home := t.TempDir()
	registry := builtin.NewBuiltins()
	for _, file := range Files(registry, home, "", []string{"claude", "codex", "cursor"}, exe, "") {
		if err := os.MkdirAll(filepath.Dir(file.Path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file.Path, file.Content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ports := &countedSkillPorts{SkillsLookup: registry}
	got := Installed(ports, home, "", "")
	if len(got) != 4 || ports.inspections != 8 {
		t.Fatalf("installed=%v inspections=%d; every ownership alternative must validate", got, ports.inspections)
	}
}

func TestObservationReuseEndsWithOperation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(path, []byte("first"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	observations := skillObservations{}
	first := observations.file(path, true)
	if err := os.WriteFile(path, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	same := observations.file(path, true)
	next := (skillObservations{}).file(path, true)
	if string(first.Bytes) != "first" || string(same.Bytes) != "first" || string(next.Bytes) != "second" || first.Mode != 0640 || same.Mode != 0640 {
		t.Fatalf("observation lifetime: first=%+v same=%+v next=%+v", first, same, next)
	}
}
