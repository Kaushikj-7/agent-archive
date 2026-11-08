package backfill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
)

func exactUndoConfirmedBatch(t *testing.T) (*NativeImportUndoPlan, string, Batch) {
	t.Helper()
	store, batch, scope := nativeImportConfirmationControl(t)
	raw, err := os.ReadFile(batchPath(store.Home(), batch.ID))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	batch, err = ConfirmNativeImportBatch(t.Context(), store, batch, hex.EncodeToString(digest[:]), scope)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanNativeImportUndo(t.Context(), store, batch.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(plan.Close)
	return plan, store.Home(), batch
}

func TestNativeImportExactBatchUndoStopsUnadmittedSelection(t *testing.T) {
	t.Parallel()
	plan, home, batch := exactUndoConfirmedBatch(t)
	absent, independent := plan.Unowned()
	if absent != 1 || independent != 0 || len(plan.Sessions()) != 0 {
		t.Fatal("incorrect preview", absent, independent, plan.Sessions())
	}
	copied := *plan
	if _, err := copied.Apply(t.Context(), time.Now()); err == nil {
		t.Fatal("copied plan acquired capability")
	}
	if count, err := plan.Apply(t.Context(), time.Now()); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	raw, actual, err := readNativeImportUndoBatch(t.Context(), home, batch.ID)
	if err != nil || actual.UndoneAt == nil {
		t.Fatal("batch not stopped before effects", err)
	}
	if _, err := plan.store.OpenConfirmedNativeImportBatch(t.Context(), batch.ID); err == nil {
		t.Fatal("undone batch could admit new owner")
	}
	retry, err := PlanNativeImportUndo(t.Context(), plan.store, batch.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Close()
	if _, err := retry.Apply(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(batchPath(home, batch.ID))
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("retry changed sticky control", err)
	}
	plan.Close()
	if _, err := plan.Apply(t.Context(), time.Now()); err == nil {
		t.Fatal("closed plan acquired capability")
	}
}

func TestNativeImportExactBatchUndoRefusesChangedControl(t *testing.T) {
	t.Parallel()
	plan, home, batch := exactUndoConfirmedBatch(t)
	batch.Sessions = []string{"independent-later-progress"}
	if err := SaveBatch(home, batch); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(batchPath(home, batch.ID))
	if err != nil {
		t.Fatal(err)
	}
	if count, err := plan.Apply(t.Context(), time.Now()); err == nil || count != 0 {
		t.Fatal("stale displayed plan stopped changed batch", count, err)
	}
	after, err := os.ReadFile(batchPath(home, batch.ID))
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("refusal lost current control", err)
	}
}

func TestNativeImportExactBatchUndoRefusesMixedHarnessesWithoutMutation(t *testing.T) {
	t.Parallel()
	store, batch, scope := nativeImportConfirmationControl(t)
	batch.Filters.Harnesses = []string{"codex", "muse"}
	scope.Filters.Harnesses = []string{"codex", "muse"}
	// Arrange a closed existing mixed control, not source or first-owner authority.
	frozen, err := freezeNativeImportBatch(t.Context(), batch, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Write(batchPath(store.Home(), batch.ID), frozen); err != nil {
		t.Fatal(err)
	}
	path := batchPath(store.Home(), batch.ID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if plan, err := PlanNativeImportUndo(t.Context(), store, batch.ID, ""); err == nil {
		plan.Close()
		t.Fatal("mixed batch undo accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("mixed control changed", err)
	}
}
