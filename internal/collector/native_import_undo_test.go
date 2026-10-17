package collector

import (
	"bytes"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Generic undo must not consume the acknowledged evidence of a historical
// receipt, even after its original typed payload has become a retained successor.
func TestNativeImportReleasedUndoPreservesAcknowledgedSuccessor(t *testing.T) {
	f := newHistoricalNativeStage(t)
	result, err := Run(t.Context(), f.store, f.remote, f.opts)
	closeStagedControlResult(result)
	if err != nil || len(result.Errors) != 0 {
		t.Fatal("typed release", err, result.Errors)
	}
	f.opts.ParserVersion = "historical-undo-successor"
	f.opts.Now = func() time.Time { return f.at.Add(time.Hour) }
	result, err = Run(t.Context(), f.store, f.remote, f.opts)
	closeStagedControlResult(result)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal("retained successor", err, result.Errors)
	}
	key, err := archive.MetadataObjectKey("muse", f.reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.remote.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SaveRequest(f.reg.ArchiveSessionID, "feedback", f.at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	request, found, err := f.store.LoadRequest(f.reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal("later request", err)
	}
	undoErr := retention.DeleteOwnedSession(t.Context(), f.store, f.remote, f.reg, state.RemovalReasonUndo, f.at.Add(3*time.Hour))
	after, readErr := f.remote.Get(t.Context(), key)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("generic undo changed acknowledged selecting evidence: undo=%v read=%v", undoErr, readErr)
	}
	if undoErr == nil {
		t.Fatal("generic historical undo was accepted")
	}
	current, found, err := f.store.LoadRequest(f.reg.ArchiveSessionID)
	if err != nil || !found || current.Token != request.Token {
		t.Fatal("independent request changed", err)
	}
	if _, err := ReadLocalBundle(t.Context(), f.store.Home(), f.reg, f.at.Add(3*time.Hour), "", f.opts.Sources); err != nil {
		t.Fatal("retained evidence changed", err)
	}
}
