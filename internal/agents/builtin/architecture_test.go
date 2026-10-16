package builtin

import (
	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
	"testing"
)

func TestAgentArchitectureBoundaries(t *testing.T) {
	t.Parallel()
	const prefix = "github.com/wangjohn/agent-archive/internal/"
	for _, name := range []string{"agentmeta", "agentapi", "archive", "destination", "sourceidentity", "agents/nativecodec"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, all := importgraph.Imports(t, prefix+name)
			importgraph.Forbid(t, name+" transitively", all, "os/exec", "net", "net/http", prefix+"credentials", prefix+"storage", prefix+"cli", prefix+"capture", prefix+"collector", prefix+"backfill")
		})
	}
	for _, name := range []string{"agents/claude", "agents/codex", "agents/cursor", "agents/muse"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, all := importgraph.Imports(t, prefix+name)
			importgraph.Forbid(t, name+" transitively", all, prefix+"cli", prefix+"capture", prefix+"collector", prefix+"backfill")
		})
	}
}
