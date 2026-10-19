package state

import (
	"github.com/wangjohn/agent-archive/internal/config"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// registration is session-1, a codex session whose transcript these tests
// never read.
func registration(t *testing.T) archive.SessionRegistration {
	t.Helper()
	return archive.SessionRegistration{
		ArchiveSessionID: "session-1", NativeSessionID: "native-1", ProjectID: "project-1", ProjectRoot: "/p",
		Harness: archive.Harness{Name: "codex"}, TranscriptPath: "/unused",
		SessionStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		RegisteredAt:     time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := openTestStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func openTestStore(home string) (*Store, error) {
	if err := os.Chmod(home, 0700); err != nil {
		return nil, err
	}
	store, err := Open(home)
	if err != nil {
		return nil, err
	}
	_, found, err := config.Load(home)
	if err != nil {
		return nil, err
	}
	if !found {
		err = config.Save(home, config.Config{MachineID: "synthetic", RetentionDays: config.DefaultRetentionDays})
	}
	return store, err
}
