package collector

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agents/cursor"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// plantedSecret is a credential an earlier filter kept in the retained
// snapshot and the current one redacts.
const plantedSecret = "hunter2hunter2"

// editRetainedRecords applies edit to the native records of session-1's
// cached and last published bundles, as an earlier release's filter would
// have left them.
func editRetainedRecords(t *testing.T, store *state.Store, edit func([]map[string]any) []map[string]any) {
	t.Helper()
	const id = "session-1"
	var file publishedFile
	if err := local.Read(publishedPath(store, id), &file); err != nil {
		t.Fatal(err)
	}
	file.Bundle.NativeRecords = edit(file.Bundle.NativeRecords)
	if file.LastPublished != nil && !file.LastPublished.SameAsBundle {
		file.LastPublished.Bundle.NativeRecords = edit(file.LastPublished.Bundle.NativeRecords)
	}
	if err := local.Write(publishedPath(store, id), file); err != nil {
		t.Fatal(err)
	}
}

// plantSecret puts plantedSecret in the retained assistant message.
func plantSecret(records []map[string]any) []map[string]any {
	for _, record := range records {
		if payload, ok := record["payload"].(map[string]any); ok {
			payload["content"] = "visible DB_PASSWORD=" + plantedSecret
		}
	}
	return records
}

// truncatedCodexTranscript is codexTranscript with its assistant message
// gone, as a truncated or compacted file holds it.
const truncatedCodexTranscript = `{"type":"turn_context","model":"gpt-test"}`

// publishedSnapshot is the source bundle the session's metadata points at.
func publishedSnapshot(t *testing.T, cloud *storagetest.MemoryStore) archive.SourceBundle {
	t.Helper()
	return fetchBundle(t, cloud, fetchMetadata(t, cloud, "codex", "session-1"))
}

// assertRefilteredSnapshot checks that the published snapshot is the
// retained one filtered by the current rules: every record kept, the
// planted secret redacted, the original capture time.
func assertRefilteredSnapshot(t *testing.T, cloud *storagetest.MemoryStore, capturedAt time.Time) {
	t.Helper()
	bundle := publishedSnapshot(t, cloud)
	text := recordsText(bundle)
	if len(bundle.NativeRecords) != 2 || !strings.Contains(text, "visible") {
		t.Fatalf("published snapshot lost the retained evidence: %s", text)
	}
	if strings.Contains(text, plantedSecret) {
		t.Fatalf("published snapshot was not filtered by the current rules: %s", text)
	}
	if bundle.Capture.FilterVersion != archive.FilterVersion {
		t.Fatalf("published snapshot has filter version %q, want %q", bundle.Capture.FilterVersion, archive.FilterVersion)
	}
	if !bundle.Capture.CapturedAt.Equal(capturedAt) {
		t.Fatalf("captured_at = %s, want %s", bundle.Capture.CapturedAt, capturedAt)
	}
}

// assertRewriteGap checks the session stands in a transcript_rewritten gap
// that the next pass leaves alone.
func assertRewriteGap(t *testing.T, store *state.Store, cloud *storagetest.MemoryStore, at time.Time) {
	t.Helper()
	if reason, blocked, err := store.LoadBlocked("session-1"); err != nil || !blocked || reason != state.BlockedReasonTranscriptRewritten {
		t.Fatalf("blocked=%v reason=%q err=%v", blocked, reason, err)
	}
	before := fetchMetadata(t, cloud, "codex", "session-1").SourceBundle.SHA256
	result, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("the pass after the gap was recorded did work: result=%#v err=%v", result, err)
	}
	if fetchMetadata(t, cloud, "codex", "session-1").SourceBundle.SHA256 != before {
		t.Fatal("the pass after the gap was recorded replaced the snapshot")
	}
}

// publishCodexSession publishes codexTranscript at t0 and returns the
// transcript's directory.
func publishCodexSession(t *testing.T, store *state.Store, cloud *storagetest.MemoryStore, t0 time.Time) string {
	t.Helper()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	if err := store.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A transcript truncated since it was captured, first read by a release
// with a new filter, cannot be compared record for record with the retained
// snapshot. Publishing it would lose what it no longer holds; keeping the
// snapshot as it was would keep what the earlier filter let through. The
// snapshot is filtered again by the current rules and republished with its
// capture time, and the rewrite is recorded as a gap.
//
// Regression: #70.
func TestFilterUpgradeRefiltersTheSnapshotOfATruncatedTranscript(t *testing.T) {
	t.Parallel()
	store, cloud := newTestStore(t), storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := publishCodexSession(t, store, cloud, t0)

	editRetainedRecords(t, store, plantSecret)
	simulateFilterUpgrade(t, store)
	writeTranscript(t, dir, "codex.jsonl", truncatedCodexTranscript)
	t1 := t0.Add(20 * 24 * time.Hour)
	if err := store.SaveRequest("session-1", "stop", t1); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("the refiltered snapshot should be published: result=%#v err=%v", result, err)
	}
	assertRefilteredSnapshot(t, cloud, t0)
	if requests, err := store.LoadRequests(); err != nil || len(requests) != 0 {
		t.Fatalf("request not acknowledged: %#v err=%v", requests, err)
	}
	assertRewriteGap(t, store, cloud, t1.Add(time.Hour))
}

// A session already standing in a transcript_rewritten gap when the filter
// changes is the same case: its published snapshot is filtered again and
// republished, not replaced by the poorer transcript, and the gap stands.
//
// Regression: #70.
func TestFilterUpgradeRefiltersTheSnapshotOfARewriteGap(t *testing.T) {
	t.Parallel()
	store, cloud := newTestStore(t), storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := publishCodexSession(t, store, cloud, t0)

	writeTranscript(t, dir, "codex.jsonl", truncatedCodexTranscript)
	t1 := t0.Add(time.Hour)
	if result, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }}); err != nil || len(result.Published) != 0 {
		t.Fatalf("the truncation should be a gap: result=%#v err=%v", result, err)
	}
	if _, blocked, _ := store.LoadBlocked("session-1"); !blocked {
		t.Fatal("the truncation was not recorded as a gap")
	}

	editRetainedRecords(t, store, plantSecret)
	simulateFilterUpgrade(t, store)
	t2 := t1.Add(20 * 24 * time.Hour)
	result, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t2 }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("the refiltered snapshot should be published: result=%#v err=%v", result, err)
	}
	assertRefilteredSnapshot(t, cloud, t0)
	assertRewriteGap(t, store, cloud, t2.Add(time.Hour))
}

// A retained snapshot the current filter refuses is not republished: the
// rewritten transcript, filtered by the current rules, replaces it as it
// did before snapshots were filtered again, and the pass says why.
func TestFilterUpgradeFallsBackWhenTheSnapshotCannotBeRefiltered(t *testing.T) {
	t.Parallel()
	store, cloud := newTestStore(t), storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := publishCodexSession(t, store, cloud, t0)

	editRetainedRecords(t, store, func([]map[string]any) []map[string]any {
		return []map[string]any{{"type": "not-a-codex-record"}}
	})
	simulateFilterUpgrade(t, store)
	writeTranscript(t, dir, "codex.jsonl", truncatedCodexTranscript)
	t1 := t0.Add(time.Hour)
	result, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }})
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("the transcript should replace the snapshot: result=%#v err=%v", result, err)
	}
	if warning := result.Errors["session-1"]; warning == nil || !strings.Contains(warning.Error(), "filter the retained snapshot") {
		t.Fatalf("no warning names the refused snapshot: %v", warning)
	}
	if bundle := publishedSnapshot(t, cloud); len(bundle.NativeRecords) != 1 || bundle.Capture.FilterVersion != archive.FilterVersion {
		t.Fatalf("published %d records under filter %q, want the transcript's 1", len(bundle.NativeRecords), bundle.Capture.FilterVersion)
	}
}

// A transcript that grew rather than shrank across an upgrade is not a
// rewrite: its candidate, with the new records, is what gets published.
func TestFilterUpgradePublishesAGrownTranscript(t *testing.T) {
	t.Parallel()
	store, cloud := newTestStore(t), storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := publishCodexSession(t, store, cloud, t0)

	simulateFilterUpgrade(t, store)
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"more"}}`)
	t1 := t0.Add(time.Hour)
	result, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("the grown transcript should publish: result=%#v err=%v", result, err)
	}
	if bundle := publishedSnapshot(t, cloud); len(bundle.NativeRecords) != 3 {
		t.Fatalf("published %d records, want the grown transcript's 3", len(bundle.NativeRecords))
	}
	if _, blocked, _ := store.LoadBlocked("session-1"); blocked {
		t.Fatal("a grown transcript was recorded as rewritten")
	}
}

// A Cursor text snapshot is filtered again as text.
func TestRefilterBundleFiltersCursorTextAgain(t *testing.T) {
	t.Parallel()
	reg := registration(t, "/unused")
	reg.Harness.Name = "cursor"
	adapter := cursor.Filter{}
	filtered, err := adapter.FilterText(strings.NewReader("user: fix the build\nassistant: done\n"), reg.SessionStartedAt)
	if err != nil {
		t.Fatal(err)
	}
	capturedAt := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, capturedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	bundle.NativeText[0].Content += "user: DB_PASSWORD=" + plantedSecret + "\n"
	refiltered, err := refilterBundle(t, context.Background(), reg, adapter, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(refiltered.NativeText) != 1 || !strings.Contains(refiltered.NativeText[0].Content, "fix the build") {
		t.Fatalf("refiltered text lost the retained evidence: %#v", refiltered.NativeText)
	}
	if strings.Contains(refiltered.NativeText[0].Content, plantedSecret) {
		t.Fatalf("refiltered text kept the secret: %q", refiltered.NativeText[0].Content)
	}
	if !refiltered.Capture.CapturedAt.Equal(capturedAt) {
		t.Fatalf("captured_at = %s, want %s", refiltered.Capture.CapturedAt, capturedAt)
	}
}

// A refiltered snapshot held back by the upload interval is published when
// it is due, and the next pass that reads the transcript records the
// rewrite: the snapshot it then compares with is filtered by the current
// rules, so the rewrite guard catches it.
func TestHeldBackRefilteredSnapshotStillEndsInARewriteGap(t *testing.T) {
	t.Parallel()
	store, cloud := newTestStore(t), storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := publishCodexSession(t, store, cloud, t0)

	editRetainedRecords(t, store, plantSecret)
	simulateFilterUpgrade(t, store)
	writeTranscript(t, dir, "codex.jsonl", truncatedCodexTranscript)
	pass := func(at time.Time) Result {
		t.Helper()
		result, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", MinUploadInterval: time.Hour, Now: func() time.Time { return at }})
		if err != nil || len(result.Errors) != 0 {
			t.Fatalf("pass at %s: result=%#v err=%v", at, result, err)
		}
		return result
	}
	if result := pass(t0.Add(time.Minute)); len(result.Waiting) != 1 {
		t.Fatalf("the refiltered snapshot should wait for the interval: %#v", result)
	}
	if result := pass(t0.Add(2 * time.Hour)); len(result.Published) != 1 {
		t.Fatalf("the refiltered snapshot should publish when due: %#v", result)
	}
	assertRefilteredSnapshot(t, cloud, t0)
	pass(t0.Add(3 * time.Hour))
	assertRewriteGap(t, store, cloud, t0.Add(4*time.Hour))
}

// A session in a rewrite gap whose transcript was restored, and has grown,
// by the time the filter changes publishes the transcript, not the older
// snapshot: filtered the same way, it extends the snapshot, so it is no
// rewrite, and the gap ends.
func TestFilterUpgradePublishesARestoredTranscript(t *testing.T) {
	t.Parallel()
	store, cloud := newTestStore(t), storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := publishCodexSession(t, store, cloud, t0)

	writeTranscript(t, dir, "codex.jsonl", truncatedCodexTranscript)
	t1 := t0.Add(time.Hour)
	if _, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }}); err != nil {
		t.Fatal(err)
	}
	if _, blocked, _ := store.LoadBlocked("session-1"); !blocked {
		t.Fatal("the truncation was not recorded as a gap")
	}

	simulateFilterUpgrade(t, store)
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"more"}}`)
	t2 := t1.Add(time.Hour)
	result, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t2 }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("the restored transcript should publish: result=%#v err=%v", result, err)
	}
	if bundle := publishedSnapshot(t, cloud); len(bundle.NativeRecords) != 3 {
		t.Fatalf("published %d records, want the restored transcript's 3", len(bundle.NativeRecords))
	}
	if _, blocked, _ := store.LoadBlocked("session-1"); blocked {
		t.Fatal("the gap outlived a restored transcript")
	}
}

// The known limitation of refiltering (see refilterRewritten): a restored
// transcript is recognized only when refiltering the snapshot reproduces
// what the current filter makes of the raw transcript. Where an upgrade
// changes a record's output, the restored transcript reads as rewritten:
// the snapshot is kept and republished, the gap stands, and records added
// to the transcript later are not captured. Nothing retained is lost.
func TestFilterUpgradeKeepsTheSnapshotOfARestoredTranscriptItCannotMatch(t *testing.T) {
	t.Parallel()
	store, cloud := newTestStore(t), storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := publishCodexSession(t, store, cloud, t0)

	writeTranscript(t, dir, "codex.jsonl", truncatedCodexTranscript)
	t1 := t0.Add(time.Hour)
	if _, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }}); err != nil {
		t.Fatal(err)
	}

	// What the earlier filter wrote for the assistant message differs from
	// what the current one makes of the raw transcript.
	const olderOutput = "visible, as an earlier filter wrote it"
	editRetainedRecords(t, store, func(records []map[string]any) []map[string]any {
		for _, record := range records {
			if payload, ok := record["payload"].(map[string]any); ok {
				payload["content"] = olderOutput
			}
		}
		return records
	})
	simulateFilterUpgrade(t, store)
	restored := codexTranscript + "\n" + `{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"more"}}`
	writeTranscript(t, dir, "codex.jsonl", restored)
	t2 := t1.Add(time.Hour)
	result, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t2 }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("the refiltered snapshot should be published: result=%#v err=%v", result, err)
	}
	if text := recordsText(publishedSnapshot(t, cloud)); !strings.Contains(text, olderOutput) || strings.Contains(text, "more") {
		t.Fatalf("published %q, want the refiltered snapshot", text)
	}
	if _, blocked, _ := store.LoadBlocked("session-1"); !blocked {
		t.Fatal("the restored transcript was recognized; update refilterRewritten's note on the limitation")
	}

	// Later records do not end the gap either.
	writeTranscript(t, dir, "codex.jsonl", restored+"\n"+`{"type":"response_item","id":"m3","payload":{"type":"message","role":"assistant","content":"later"}}`)
	t3 := t2.Add(time.Hour)
	result, err = Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t3 }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("records added after the gap were published: result=%#v err=%v", result, err)
	}
	if _, blocked, _ := store.LoadBlocked("session-1"); !blocked {
		t.Fatal("the gap ended; update refilterRewritten's note on the limitation")
	}
}

func TestRetainedRefilterPreservesCancellation(t *testing.T) {
	adapter, err := testAdapter("codex")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = refilterBundle(t, ctx, archive.SessionRegistration{}, adapter, archive.SourceBundle{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("refilter lost cancellation: %v", err)
	}
}
