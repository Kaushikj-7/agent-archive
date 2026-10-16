package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestNativeOwnerCanceledPlanDoesNotCheckpointRequestedCoverage(t *testing.T) {
	for _, mode := range []os.FileMode{0o755, 0o700} {
		t.Run("mode_"+strconv.FormatUint(uint64(mode), 8), func(t *testing.T) {
			home, userHome := t.TempDir(), t.TempDir()
			must(t, os.Chmod(home, mode))
			native := filepath.Join(userHome, ".codex")
			must(t, os.MkdirAll(filepath.Join(native, "sessions"), 0700))
			lookup, err := discovery.NewCodexRolloutLookup(t.Context(), state.OpenReadOnly(home), []string{native})
			must(t, err)
			// Schedule coverage before interruption, without ever setting ObserveReadOnly.
			_, _ = lookup.Thread(t.Context(), "11111111-1111-4111-8111-111111111111")
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			before := snapshotTree(t, home)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = buildBackfillPlanWithLookup(ctx, env, home, userHome, config.Config{}, backfill.Filters{}, nil, func(context.Context, *state.Store, config.Config, Env) (*discovery.CodexRolloutLookup, []string, error) {
				return lookup, []string{native}, nil
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled plan: %v", err)
			}
			if snapshotTree(t, home) != before {
				t.Fatal("canceled preview checkpointed requested coverage")
			}
			if used, _ := lookup.NativeReadBudget().Charged(); used != 0 {
				t.Fatalf("preview leaked %d source bytes", used)
			}
		})
	}
}

func TestNativeOwnerReturningDestinationIncludesResolvedChildren(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	store, err := state.Open(home)
	must(t, err)
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cfg := config.Config{Storage: credentialsTestConfig(), ImportedHarnesses: []string{"codex"}, Archive: archive.Config{Enabled: true}}
	for _, tc := range []struct {
		id     string
		native bool
		parent string
		dest   string
	}{
		{"native-child", true, "external-parent", cfg.DestinationID()},
		{"linked-child", false, "external-parent", cfg.DestinationID()},
		{"other-destination", true, "external-parent", "other"},
	} {
		must(t, store.SaveRegistration(archive.SessionRegistration{ArchiveSessionID: tc.id, NativeSessionID: tc.id, Harness: archive.Harness{Name: "codex"}, ProjectRoot: project, ProjectID: archive.ProjectID(project), Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch("batch"), NativeChild: tc.native, ParentSessionID: tc.parent, DestinationID: tc.dest, SessionStartedAt: at, RegisteredAt: at, AdmittedAt: at}))
	}
	count, err := sessionsAdmittedInto(home, cfg)
	must(t, err)
	if count != 1 {
		t.Fatalf("independent returning owners = %d, want 1", count)
	}
}
