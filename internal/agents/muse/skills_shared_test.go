package muse_test

import (
	"bytes"
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agents/muse"
	"github.com/wangjohn/agent-archive/internal/agentskills"
)

func TestMuseSharedSkillsDeduplicateThroughExistingRenderer(t *testing.T) {
	t.Parallel()
	// The production binding remains gated pending privacy/version approval.
	// This exercises the normal renderer with the real proposed provider.
	current := builtin.NewBuiltins()
	var bindings []builtin.Integration
	for _, d := range current.Catalog().All() {
		b, ok := current.Lookup(string(d.ID))
		if !ok {
			t.Fatal("missing binding")
		}
		b.Descriptor = agentmeta.Descriptor{ID: d.ID}
		if d.ID == agentmeta.Muse {
			b.Skills = muse.Skills()
		}
		bindings = append(bindings, b)
	}
	registry, err := builtin.New(agentmeta.Builtins(), bindings)
	if err != nil {
		t.Fatal(err)
	}
	one := agentskills.Files(registry, "/synthetic/home", "", []string{"codex"}, "/synthetic/archive", "")
	shared := agentskills.Files(registry, "/synthetic/home", "", []string{"codex", "cursor", "muse"}, "/synthetic/archive", "")
	if len(one) == 0 || len(shared) != len(one) {
		t.Fatalf("files %d/%d", len(one), len(shared))
	}
	for i, file := range shared {
		if file.Path != one[i].Path || !bytes.Equal(file.Content, one[i].Content) || !slices.Equal(file.Harnesses, []string{"codex", "cursor", "muse"}) {
			t.Fatalf("shared file differs: %+v", file)
		}
	}
}
