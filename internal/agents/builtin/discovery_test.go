package builtin

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/testutil/agenttest"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoveryConformance(t *testing.T) {
	t.Parallel()
	fixtures := map[string][]string{
		"claude": {".claude/projects/project/first.jsonl", ".claude/projects/project/second.jsonl"},
		"codex":  {".codex/sessions/day/rollout-first.jsonl", ".codex/sessions/day/rollout-second.jsonl"},
		"cursor": {".cursor/projects/project/agent-transcripts/first.txt", ".cursor/projects/project/agent-transcripts/second.txt"},
	}
	r := NewBuiltins()
	for _, name := range r.DiscoveryAgents() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if name == "muse" {
				p, _ := r.LookupDiscovery(name)
				if _, err := p.Discover(t.Context(), agentapi.DiscoveryRequest{Purpose: agentapi.DiscoveryImport}, func(agentapi.DiscoveryCandidate) error { t.Fatal("emitted unsupported import"); return nil }); err == nil {
					t.Fatal("handoff-only discovery accepted import")
				}
				return
			}
			home := t.TempDir()
			for _, rel := range fixtures[name] {
				path := filepath.Join(home, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("synthetic raw bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			p, ok := r.LookupDiscovery(name)
			if !ok {
				t.Fatal("missing discovery port")
			}
			agenttest.Discovery(t, p, agentapi.DiscoveryRequest{Purpose: agentapi.DiscoveryImport, Locations: agentapi.NativeLocations{UserHome: home}}, name)
		})
	}
}
