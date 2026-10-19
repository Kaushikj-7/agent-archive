package state

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/config"
	"io"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
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

// inspectPendingRoot inspects one pending root without an atomic witness.
func (s *Store) inspectPendingRoot(home *local.RootedHome, name string, remaining *int, legacy bool) ([]DurableStorageObligation, bool, error) {
	matched := false
	return s.inspectPendingRootWitnessed(home, name, remaining, legacy, &matched)
}

// validateDurableReadPending validates pending state without a native import ACK.
func (s *Store) validateDurableReadPending(id string, history bool, owner *publicationReadOwner) error {
	return s.validateDurableReadPendingSelection(id, history, owner, nil)
}

// writeContext writes through the durable quota without a commit check.
func (q *durableQuota) writeContext(ctx context.Context, path string, n int64, write func(io.Writer) error) error {
	return q.writeWithHeadroomContext(ctx, path, n, write, nil, 0)
}
