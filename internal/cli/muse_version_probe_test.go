package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestMuseVersionProbeOverridesUpdatesAndKeepsFixedArgv(t *testing.T) {
	// The fake executable observes only synthetic variables, never native Muse.
	t.Setenv("MUSE_NO_AUTO_UPDATE", "stale")
	t.Setenv("MUSE_CONFIG_DIR", "synthetic-preserved")
	path := filepath.Join(t.TempDir(), "fake muse")
	script := `#!/bin/sh
[ "$#" = 1 ] && [ "$1" = --version ] || exit 21
[ "$MUSE_NO_AUTO_UPDATE" = 1 ] || exit 22
[ "$MUSE_CONFIG_DIR" = synthetic-preserved ] || exit 23
printf 'Muse Code 1.4.9 (1.4.9-R6297.1)'
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	got, ok := (versionHost{}).ProbeVersion(agentapi.VersionProbe{Kind: agentapi.VersionMuseCLI, Path: path})
	if !ok || got != "Muse Code 1.4.9 (1.4.9-R6297.1)" {
		t.Fatalf("probe %q %v", got, ok)
	}
	if os.Getenv("MUSE_NO_AUTO_UPDATE") != "stale" {
		t.Fatal("host environment changed")
	}
}

func TestMuseVersionProbeRejectsMultilineAndOverflow(t *testing.T) {
	t.Parallel()
	for _, script := range []string{"printf 'one\\ntwo'", "printf '%s' '" + strings.Repeat("x", 4097) + "'", "exit 1"} {
		path := filepath.Join(t.TempDir(), "fake")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if got, ok := (versionHost{}).ProbeVersion(agentapi.VersionProbe{Kind: agentapi.VersionMuseCLI, Path: path}); ok || got != "" {
			t.Fatalf("accepted %d output bytes", len(got))
		}
	}
}
