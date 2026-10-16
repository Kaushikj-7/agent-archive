package backfill

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestLiveCursorProofStagesAfterNativeDisappears(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only Unix SHM ownership refuses root")
	}
	r, c, _, database := settledCursorFixture(t)
	writer := startCursorWriter(t, database, true)
	writer.do("unselected")
	id, _, err := r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Cursor, NativeID: c.NativeSessionID})
	if err != nil {
		t.Fatal(err)
	}
	reg := r.registration(c, id, "")
	budget := agentapi.NewNativeReadBudget(128 << 20)
	original, err := settledTestRequest(t, r, reg, *c.reviewedCursor, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	store, closeStore := r.Store.WithReadBudget(t.Context(), budget)
	defer closeStore()
	live, err := store.NewLiveCaptureRequest(original)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := live.Close(); err != nil {
			t.Error(err)
		}
	}()
	proof, err := testSources.CaptureLiveCursor(live)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := proof.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, dir := range []string{"temporary-reservations", "temporary-scratch"} {
		entries, err := os.ReadDir(filepath.Join(r.Home, dir))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatal("proof escaped with outstanding scratch", dir, entries)
		}
	}
	writer.kill()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(database + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	digest, err := store.SaveLiveCursorImportStage(t.Context(), reg, original, proof)
	if err != nil || digest == "" {
		t.Fatal("first stage after source deletion", digest, err)
	}
	stage, found, err := store.LoadImportStage(t.Context(), reg)
	if err != nil || !found || stage.Version != 3 || stage.CursorFacts == nil || stage.CursorFacts.Chat != *c.reviewedCursor {
		t.Fatal(stage.Version, found, err)
	}
	// A different original request cannot borrow this proof even with the same ledger.
	other, err := settledTestRequest(t, r, reg, *c.reviewedCursor, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err = store.SaveLiveCursorImportStage(t.Context(), reg, other, proof); err == nil {
		t.Fatal("live proof substituted another original request")
	}
}

func TestLiveCursorIssuerRejectsDifferentBudget(t *testing.T) {
	r, c, _, _ := settledCursorFixture(t)
	id, _, err := r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Cursor, NativeID: c.NativeSessionID})
	if err != nil {
		t.Fatal(err)
	}
	reg := r.registration(c, id, "")
	original, err := settledTestRequest(t, r, reg, *c.reviewedCursor, agentapi.NewNativeReadBudget(128<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	store, closeStore := r.Store.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(128<<20))
	defer closeStore()
	if _, err = store.NewLiveCaptureRequest(original); !errors.Is(err, state.ErrReadySelectionFrozen) {
		t.Fatal(err)
	}
}
