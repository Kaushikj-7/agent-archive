package retention

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// hookDuringDeleteStore runs a hook on the first remote delete: the moment
// retention has already decided a session expired, from a snapshot of the
// pending requests, and has not yet forgotten it locally.
type hookDuringDeleteStore struct {
	storage.ObjectStore
	once sync.Once
	hook func()
}

func (h *hookDuringDeleteStore) Delete(ctx context.Context, key string) error {
	h.once.Do(h.hook)
	return h.ObjectStore.Delete(ctx, key)
}

func finalResponse(t *testing.T, at time.Time) archive.SupplementalEvidence {
	t.Helper()
	filtered, _, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{{
		Kind: archive.EvidenceKindFinalResponse, ObservedAt: at, Provenance: "hook:codex:stop",
		Payload: map[string]any{"event_name": "Stop", "turn_id": "late-turn", "text": "a final answer written mid-expiry"},
	}})
	if err != nil || len(filtered) != 1 {
		t.Fatalf("filter: %v", err)
	}
	return filtered[0]
}

// A hook writes a request after retention snapshotted the pending requests
// and deleted the session's objects, but before it forgot the session. The
// request and its evidence must survive, and the next collector pass must
// retain that evidence as pending rather than lose it. Exact replacement
// authority for an intentionally deleted sidecar requires the deletion journal.
func TestHookRequestWrittenMidExpiryRetainsPendingAfterRemoteDeletion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newTestStore(t)
	memory := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := local.SaveRegistration(registration("s1", writeTranscript(t, dir, "s1.jsonl", codexTranscript))); err != nil {
		t.Fatal(err)
	}
	collect(t, local, memory, t0)

	expiry := t0.Add(91 * 24 * time.Hour)
	var hookErr error
	store := &hookDuringDeleteStore{ObjectStore: memory, hook: func() {
		hookErr = local.SaveRequest("s1", "stop", expiry, finalResponse(t, expiry))
	}}
	result := sweep(t, local, store, expiry, Options{})
	if hookErr != nil {
		t.Fatalf("the hook's request write failed: %v", hookErr)
	}
	if len(result.Errors) != 0 || len(result.DeletedSessions) != 0 {
		t.Fatalf("the session was expired over a request written mid-expiry: %#v", result)
	}
	if registered(t, local) != 1 {
		t.Fatal("the registration was forgotten")
	}
	requests, err := local.LoadRequests()
	if err != nil || len(requests) != 1 || len(requests[0].HookEvidence) != 1 {
		t.Fatalf("the hook's request was lost: %#v %v", requests, err)
	}

	// A known-present predecessor becoming absent is a conflict, not permission
	// to infer first publication. Keep the complete filtered snapshot and request
	// for deletion-journal reconciliation in the maintenance milestone.
	resultAfter := collect(t, local, memory, expiry)
	if !errors.Is(resultAfter.Errors["s1"], storage.ErrPublicationConflict) || len(resultAfter.Published) != 0 {
		t.Fatal("deleted predecessor was guessed absent", resultAfter.Errors)
	}
	pending, found, err := local.LoadPending("s1")
	if err != nil || !found || pending.Commit == nil || pending.Commit.Predecessor != state.PredecessorPresent {
		t.Fatal("publication evidence lost", found, err)
	}
	if !pending.Bundle.Capture.CapturedAt.Equal(expiry) {
		t.Fatal("new evidence lost its capture time")
	}
	kept := false
	for _, item := range pending.Bundle.SupplementalEvidence {
		if item.Kind == archive.EvidenceKindFinalResponse && item.Payload["turn_id"] == "late-turn" {
			kept = true
		}
	}
	if !kept {
		t.Fatal("mid-expiry final response lost")
	}
	if _, found, err := local.LoadRequest("s1"); err != nil || !found {
		t.Fatal("uncommitted request acknowledged", found, err)
	}
	if _, err := memory.Get(t.Context(), "sessions/codex/s1/metadata.json"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("sidecar republished without authority", err)
	}

}

// Hooks and retention run in different processes with no ordering between
// them. Whatever the interleaving, a request SaveRequest reported as written
// is never lost to expiry, and a request refused because the session was
// already forgotten leaves no orphan behind.
//
// Not parallel: real sleeps spread the hook across the sweep, and the hook's
// SaveRequest waits at most a second for the request lock the sweep's forget
// holds, so a busy parallel run could make it fail with ErrBusy.
func TestConcurrentHookRequestIsNeverLostToExpiry(t *testing.T) {
	const rounds = 30
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	expiry := t0.Add(91 * 24 * time.Hour)
	kept, forgotten := 0, 0
	for round := range rounds {
		dir := t.TempDir()
		local := newTestStore(t)
		memory := storagetest.NewMemoryStore()
		id := fmt.Sprintf("s%02d", round)
		if err := local.SaveRegistration(registration(id, writeTranscript(t, dir, id+".jsonl", codexTranscript))); err != nil {
			t.Fatal(err)
		}
		collect(t, local, memory, t0)

		var wg sync.WaitGroup
		var saveErr, sweepErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, sweepErr = Sweep(context.Background(), local, memory, agreeing(Options{Now: func() time.Time { return expiry }, SessionMaxAge: retentionWindow}))
		}()
		evidence := finalResponse(t, expiry)
		go func() {
			defer wg.Done()
			// Spread the hook across the sweep so rounds land before the
			// snapshot, between it and the forget, and after the forget.
			time.Sleep(time.Duration(round) * 40 * time.Microsecond)
			saveErr = local.SaveRequest(id, "stop", expiry, evidence)
		}()
		wg.Wait()
		if sweepErr != nil {
			t.Fatal(sweepErr)
		}
		requests, err := local.LoadRequests()
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case saveErr == nil:
			if registered(t, local) != 1 || len(requests) != 1 {
				t.Fatalf("round %d: a request reported as written was lost (registered=%d requests=%d)", round, registered(t, local), len(requests))
			}
			kept++
		case errors.Is(saveErr, state.ErrSessionNotRegistered):
			if registered(t, local) != 0 || len(requests) != 0 {
				t.Fatalf("round %d: refused request left state behind (registered=%d requests=%d)", round, registered(t, local), len(requests))
			}
			forgotten++
		default:
			t.Fatalf("round %d: %v", round, saveErr)
		}
	}
	t.Logf("%d rounds: request kept the session %d times, arrived after it was forgotten %d times", rounds, kept, forgotten)
}

// A registration that never received a transcript path can never be
// captured, so a request queued for it is not work the collector will do.
// Once the session is older than the retention window, that request no
// longer defers expiry: the session is forgotten locally, with zero bucket
// calls, and the request's hook text goes with it. The same request on a
// registration with a transcript path still defers expiry.
func TestQueuedRequestDoesNotKeepATranscriptlessSessionPastRetention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	withPath := registration("with-path", writeTranscript(t, dir, "with-path.jsonl", codexTranscript))
	withoutPath := registration("no-path", "")
	for _, reg := range []archive.SessionRegistration{withPath, withoutPath} {
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		if err := local.SaveRequest(reg.ArchiveSessionID, "stop", t0.Add(time.Minute), finalResponse(t, t0.Add(time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	store := &recordingStore{ObjectStore: storagetest.NewMemoryStore()}

	// Inside the window nothing expires, request or not.
	if result := sweep(t, local, store, t0.Add(retentionWindow-time.Hour), Options{}); len(result.PrunedSessions) != 0 || len(result.Errors) != 0 {
		t.Fatalf("expired inside the retention window: %#v", result)
	}

	result := sweep(t, local, store, t0.Add(retentionWindow+time.Hour), Options{})
	if len(result.Errors) != 0 || len(result.PrunedSessions) != 1 || result.PrunedSessions[0] != "no-path" || len(result.DeletedSessions) != 0 {
		t.Fatalf("result=%#v, want only no-path pruned", result)
	}
	if store.count() != 0 {
		t.Fatalf("a never-captured session cost %d bucket calls", store.count())
	}
	if _, found, _ := local.LoadRegistration("no-path"); found {
		t.Fatal("the transcript-less registration survived past retention")
	}
	if _, found, _ := local.LoadRegistration("with-path"); !found {
		t.Fatal("the request stopped deferring expiry for a session the collector can still capture")
	}
	requests, err := local.LoadRequests()
	if err != nil || len(requests) != 1 || requests[0].ArchiveSessionID != "with-path" {
		t.Fatalf("requests=%#v err=%v; the forgotten session's request must go with it", requests, err)
	}
}

// A registration whose transcript file exists but was never written has
// captured nothing either: a queued request no longer defers its expiry
// once it is older than the retention window, as for a registration with
// no path. The same request on a transcript with content still does.
//
// Regression: 2026-09 pre-release review, collector bug 4.
func TestQueuedRequestDoesNotKeepAnEmptyTranscriptSessionPastRetention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	withContent := registration("with-content", writeTranscript(t, dir, "with-content.jsonl", codexTranscript))
	empty := registration("empty", writeTranscript(t, dir, "empty.jsonl", ""))
	for _, reg := range []archive.SessionRegistration{withContent, empty} {
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		if err := local.SaveRequest(reg.ArchiveSessionID, "stop", t0.Add(time.Minute), finalResponse(t, t0.Add(time.Minute))); err != nil {
			t.Fatal(err)
		}
	}
	store := &recordingStore{ObjectStore: storagetest.NewMemoryStore()}

	if result := sweep(t, local, store, t0.Add(retentionWindow-time.Hour), Options{}); len(result.PrunedSessions) != 0 || len(result.Errors) != 0 {
		t.Fatalf("expired inside the retention window: %#v", result)
	}

	result := sweep(t, local, store, t0.Add(retentionWindow+time.Hour), Options{})
	if len(result.Errors) != 0 || len(result.PrunedSessions) != 1 || result.PrunedSessions[0] != "empty" || len(result.DeletedSessions) != 0 {
		t.Fatalf("result=%#v, want only empty pruned", result)
	}
	if store.count() != 0 {
		t.Fatalf("a never-captured session cost %d bucket calls", store.count())
	}
	if _, found, _ := local.LoadRegistration("with-content"); !found {
		t.Fatal("the request stopped deferring expiry for a session the collector can still capture")
	}
	requests, err := local.LoadRequests()
	if err != nil || len(requests) != 1 || requests[0].ArchiveSessionID != "with-content" {
		t.Fatalf("requests=%#v err=%v; the forgotten session's request must go with it", requests, err)
	}
}

// A first publication built before its transcript was emptied, and still
// waiting to upload, is work the collector will do: an empty transcript does
// not stop it from deferring expiry.
func TestPendingUploadKeepsAnEmptyTranscriptSessionPastRetention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registration("uploading", writeTranscript(t, dir, "uploading.jsonl", ""))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", t0.Add(time.Minute), finalResponse(t, t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := local.SavePending(reg.ArchiveSessionID, state.PendingPublication{
		SourceKey: "k", MetadataKey: "m", SourceSHA256: "4bf5122f344554c53bde2ebb8cd2b7e3d1600ad631c385a5d7cce23c7785459a", SourceBytes: []byte{1}, MetadataBytes: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	result := sweep(t, local, storagetest.NewMemoryStore(), t0.Add(retentionWindow+time.Hour), Options{})
	if len(result.Errors) != 0 || len(result.PrunedSessions) != 0 || len(result.DeletedSessions) != 0 {
		t.Fatalf("result=%#v, want nothing expired", result)
	}
	if pending, err := local.HasPending(reg.ArchiveSessionID); err != nil || !pending {
		t.Fatalf("the pending upload was swept: pending=%v err=%v", pending, err)
	}
}

// A session read from Cursor's database has no transcript path by design,
// but the collector still captures it: its queued request keeps deferring
// expiry past the retention window, as a file session's does.
func TestQueuedRequestKeepsACursorDatabaseSessionPastRetention(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reg := registration("cursor-db", "")
	reg.Harness.Name = "cursor"
	reg.SourceKind, reg.SourceKey = archive.SourceKindCursorSQLite, reg.NativeSessionID
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", t0.Add(time.Minute), finalResponse(t, t0.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	store := &recordingStore{ObjectStore: storagetest.NewMemoryStore()}
	result := sweep(t, local, store, t0.Add(retentionWindow+time.Hour), Options{})
	if len(result.Errors) != 0 || len(result.PrunedSessions) != 0 || len(result.DeletedSessions) != 0 {
		t.Fatalf("result=%#v, want nothing expired", result)
	}
	if _, found, _ := local.LoadRegistration(reg.ArchiveSessionID); !found {
		t.Fatal("unpublished work on a Cursor database session was deleted")
	}
}
