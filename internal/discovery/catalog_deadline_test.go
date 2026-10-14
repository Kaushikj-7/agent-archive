package discovery

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

type deadlineCatalogAdapter struct {
	SourceAdapter
	cancelOuter context.CancelFunc
}

func (a deadlineCatalogAdapter) Enumerate(ctx context.Context, _, _ string, _ int64) (SourceBatch, error) {
	if a.cancelOuter != nil {
		a.cancelOuter()
	}
	<-ctx.Done()
	return SourceBatch{}, ctx.Err()
}

func TestCatalogCheckpointSurvivesObservationDeadlineAndReopensFairly(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	project := cfg.Archive.Projects[0].Root
	wanted := writeRollout(t, root, project, at.Add(time.Minute), 1, "sessions")
	fresh := writeRollout(t, root, project, at.Add(time.Minute), 2, "sessions")
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	lookup.remaining = time.Second + 20*time.Millisecond
	adapter := deadlineCatalogAdapter{SourceAdapter: codexAdapter{supported: syntheticSupport}}
	health, err := runWithAdapters(t.Context(), store, cfg, Options{Rollouts: lookup}, []SourceAdapter{adapter})
	if err != nil || !health.Pending {
		t.Fatalf("bounded observation lost its checkpoint: pending=%t err=%v", health.Pending, err)
	}
	if lookup.remaining <= 0 || lookup.remaining > time.Second {
		t.Fatalf("checkpoint did not consume the original pass allowance: %v", lookup.remaining)
	}
	path := filepath.Join(store.Home(), "discovery-catalog.json")
	var checkpoint catalog
	if err := local.Read(path, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.Queue) != 2 {
		t.Fatal("deadline erased the queue", checkpoint)
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
	if used, _ := lookup.NativeReadBudget().Charged(); used != 0 {
		t.Fatal("checkpoint owner leaked", used)
	}
	for range 4 {
		reopened, err := state.Open(store.Home())
		if err != nil {
			t.Fatal(err)
		}
		lookup, err = NewCodexRolloutLookup(t.Context(), reopened, []string{root})
		if err != nil {
			t.Fatal(err)
		}
		_, err = runWithAdapters(t.Context(), reopened, cfg, Options{Rollouts: lookup}, []SourceAdapter{codexAdapter{supported: syntheticSupport}})
		if err != nil {
			t.Fatal(err)
		}
		complete := lookup.catalog != nil && len(lookup.catalog.Queue) == 0
		for _, id := range []string{wanted, fresh} {
			set, err := lookup.Thread(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			complete = complete && len(set.Candidates) == 1
		}
		if err := lookup.Close(); err != nil {
			t.Fatal(err)
		}
		if used, _ := lookup.NativeReadBudget().Charged(); used != 0 {
			t.Fatal("reopened checkpoint owner leaked", used)
		}
		if complete {
			return
		}
	}
	t.Fatal("reopened observation did not converge")
}

func TestCatalogCheckpointStillStopsOnOuterCancellation(t *testing.T) {
	t.Parallel()
	store, cfg, _, root := fixture(t)
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.Close() }()
	path := filepath.Join(store.Home(), "discovery-catalog.json")
	checkpoint := catalog{Version: catalogVersion, Roots: []string{root}, Queue: []directory{{Root: root, Path: "sessions"}, {Root: root, Path: "archived_sessions"}}}
	if err := local.WriteCompact(path, checkpoint); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	adapter := deadlineCatalogAdapter{SourceAdapter: codexAdapter{supported: syntheticSupport}, cancelOuter: cancel}
	_, err = runWithAdapters(ctx, store, cfg, Options{Rollouts: lookup}, []SourceAdapter{adapter})
	if !errors.Is(err, context.Canceled) || errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("outer cancellation did not stop checkpoint with its own cause", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("canceled checkpoint replaced durable bytes", err)
	}
}
