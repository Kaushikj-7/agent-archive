package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shortDisplayFixtureDir bounds the actual synthetic display path before
// rendering. Post-render normalization cannot undo terminal wrapping.
// Keep terminal widths, erase sequences and golden expectations unchanged.
func shortDisplayFixtureDir(t *testing.T) string {
	t.Helper()
	//lint:ignore usetesting Short private paths preserve terminal wrapping; t.Cleanup removes this owned root.
	dir, err := os.MkdirTemp("/tmp", "agent-ui-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

// pairingDisplayFixtureHome preserves the old transcript's actual prompt
// geometry at every redraw width, before display-path normalization.
func pairingDisplayFixtureHome(t *testing.T) string {
	t.Helper()
	root := shortDisplayFixtureDir(t)
	home := filepath.Join(root, strings.Repeat("p", 64))
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	return home
}

// handoffDisplayFixturePaths bounds both the real default project and the
// typed output path before rendering, preserving the old transcript geometry.
func handoffDisplayFixturePaths(t *testing.T) (home, project, output string) {
	t.Helper()
	root := shortDisplayFixtureDir(t)
	home = filepath.Join(root, "archive")
	project = filepath.Join(root, strings.Repeat("p", 84))
	destination := filepath.Join(root, strings.Repeat("d", 84))
	for _, dir := range []string{home, project, destination} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return home, project, filepath.Join(destination, "handoff.md")
}
