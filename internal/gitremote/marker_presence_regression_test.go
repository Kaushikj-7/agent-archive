package gitremote

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

func TestMarkerPresenceInvalidatesAbsentDependency(t *testing.T) {
	gitOrSkip(t)
	scope, ok := identityObservationScope()
	if !ok {
		t.Fatal("identity observation scope unavailable")
	}
	root := t.TempDir()
	marker := filepath.Join(root, ".git")
	stamp, ok := repositoryStamp(marker)
	if !ok {
		t.Fatal("absent marker observation refused")
	}
	id := sourcefacts.RepositoryIdentity{Known: true, ObservationScope: scope, Dependencies: []sourcefacts.RepositoryDependency{{Path: marker, Stamp: stamp}}}
	if !ProjectIdentityCurrent(id) {
		t.Fatal("unchanged absence invalidated")
	}
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "unrelated")); err != nil {
		t.Fatal(err)
	}
	if !ProjectIdentityCurrent(id) {
		t.Fatal("unrelated locator invalidated dependency")
	}
	if err := os.Symlink(filepath.Join(root, "missing"), marker); err != nil {
		t.Fatal(err)
	}
	if ProjectIdentityCurrent(id) {
		t.Fatal("new dangling Git marker incorrectly preserved absent dependency")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if !ProjectIdentityCurrent(id) {
		t.Fatal("restored absence invalidated")
	}
}
