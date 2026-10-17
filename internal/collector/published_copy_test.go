package collector

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A remote cache body may support derivation, but cannot authorize replacing
// a legacy publication whose exact predecessor was never retained locally.
func TestParserUpgradeOverLegacyStateRetainsDerivedPendingWithoutGuessingPredecessor(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	firstKey := publishThenGrow(t, local, store, t0)
	editPublishedState(t, local, func(state map[string]any) {
		withoutRecordedSource(state)
		delete(state, "metadata_bytes")
	})
	now := t0.Add(time.Hour)
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", ParserVersion: "upgraded", Now: func() time.Time { return now }})
	if err != nil || !errors.Is(result.Errors["session-1"], storage.ErrPublicationConflict) || len(result.Published) != 0 {
		t.Fatalf("scan: %#v %v", result, err)
	}
	pending, found, err := local.LoadPending("session-1")
	if err != nil || !found || pending.Commit == nil || pending.Commit.Predecessor != state.PredecessorUnknown {
		t.Fatal("derived evidence not pending", found, err)
	}
	var next archive.Metadata
	if err := json.Unmarshal(pending.MetadataBytes, &next); err != nil {
		t.Fatal(err)
	}
	if next.Parser.Version != "upgraded" || next.SourceBundle.Key == firstKey {
		t.Fatal("grown current-parser evidence not retained")
	}
	if superseded, err := local.LoadSuperseded("session-1"); err != nil || len(superseded) != 0 {
		t.Fatal("uncommitted predecessor retired", superseded, err)
	}
	if current := fetchMetadata(t, store, "codex", "session-1"); current.SourceBundle.Key != firstKey {
		t.Fatal("remote cache guessed as replacement authority")
	}
}
