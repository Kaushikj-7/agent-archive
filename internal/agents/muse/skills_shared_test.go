package muse_test

import (
	"bytes"
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/agentskills"
)

func TestMuseSharedSkillsDeduplicateThroughExistingRenderer(t *testing.T) {
	t.Parallel()
	registry := builtin.NewBuiltins()
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
