package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

const codexTranscript = `{"type":"turn_context","model":"gpt-test"}
{"type":"response_item","id":"m1","payload":{"type":"message","role":"assistant","content":"visible"}}`

func writeTranscript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func registration(t *testing.T, transcriptPath string) archive.SessionRegistration {
	t.Helper()
	return archive.SessionRegistration{
		ArchiveSessionID: "session-1", NativeSessionID: "native-1", ProjectID: "project-1", ProjectRoot: "/p",
		Harness: archive.Harness{Name: "codex"}, TranscriptPath: transcriptPath,
		SessionStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		RegisteredAt:     time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
	}
}

func newTestStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := openTestStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func fetchMetadata(t *testing.T, store storage.ObjectStore, harness, sessionID string) archive.Metadata {
	t.Helper()
	key, err := archive.MetadataObjectKey(harness, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func fetchBundle(t *testing.T, store storage.ObjectStore, metadata archive.Metadata) archive.SourceBundle {
	t.Helper()
	data, err := store.Get(context.Background(), metadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(data), archive.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

type metadataFailStore struct {
	*storagetest.MemoryStore
	failMetadata bool
}

func (s *metadataFailStore) Put(ctx context.Context, key string, data []byte) error {
	if s.failMetadata && filepath.Base(key) == "metadata.json" {
		return errors.New("injected metadata failure")
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func TestRunPublishesNewSession(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 || result.Published[0] != "session-1" || len(result.Errors) != 0 {
		t.Fatalf("result=%#v", result)
	}
	metadata := fetchMetadata(t, store, "codex", "session-1")
	if !metadata.CapturedAt.Equal(now) {
		t.Fatalf("captured_at=%s want=%s", metadata.CapturedAt, now)
	}
	if err := metadata.ValidateSourceReference(); err != nil {
		t.Fatalf("published metadata has no verified source reference: %v", err)
	}
}

func TestRunSkipsUnchangedSessionAndReusesCapturedAt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	first := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return first }}); err != nil {
		t.Fatal(err)
	}
	firstMetadata := fetchMetadata(t, store, "codex", "session-1")

	second := first.Add(10 * time.Minute)
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return second }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "session-1" || len(result.Published) != 0 {
		t.Fatalf("result=%#v", result)
	}
	secondMetadata := fetchMetadata(t, store, "codex", "session-1")
	if !secondMetadata.CapturedAt.Equal(firstMetadata.CapturedAt) {
		t.Fatalf("captured_at changed on an unchanged scan: %s -> %s", firstMetadata.CapturedAt, secondMetadata.CapturedAt)
	}
	if !secondMetadata.MetadataDerivedAt.Equal(firstMetadata.MetadataDerivedAt) {
		t.Fatalf("metadata republished though nothing changed: derived_at %s -> %s", firstMetadata.MetadataDerivedAt, secondMetadata.MetadataDerivedAt)
	}
}

func TestRunRateLimitsRepublishAndReusesFirstDetectedCapturedAt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}

	// New evidence arrives, but well within the minimum upload interval.
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	tDetected := t0.Add(30 * time.Second)
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return tDetected }, MinUploadInterval: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// Held for the interval: reported as waiting, due one interval after
	// the last publication, and not as unchanged.
	if len(result.Waiting) != 1 || len(result.Skipped) != 0 || len(result.Published) != 0 || !result.NextReadyAt.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("expected a publication waiting for the interval, got %#v", result)
	}
	// A pass that finds it still waiting (the resume path) reports the same.
	if again, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return tDetected.Add(time.Minute) }, MinUploadInterval: 3 * time.Minute}); err != nil || len(again.Waiting) != 1 || len(again.Skipped) != 0 || !again.NextReadyAt.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("still-waiting pass: %#v err=%v", again, err)
	}
	// Metadata in storage must still be the original, unpublished candidate.
	unchanged := fetchMetadata(t, store, "codex", "session-1")
	if !unchanged.CapturedAt.Equal(t0) {
		t.Fatalf("rate-limited candidate must not be published: captured_at=%s", unchanged.CapturedAt)
	}

	// No further content change; enough time has now passed to publish the
	// pending candidate, which must carry the timestamp of when it was first
	// detected, not this run's time.
	tPublish := t0.Add(4 * time.Minute)
	result, err = Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return tPublish }, MinUploadInterval: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 {
		t.Fatalf("expected the withheld candidate to publish, got %#v", result)
	}
	published := fetchMetadata(t, store, "codex", "session-1")
	if !published.CapturedAt.Equal(tDetected) {
		t.Fatalf("captured_at=%s want=%s (first detection time, not publish time)", published.CapturedAt, tDetected)
	}
}

func TestRunLeavesRequestPendingOnScanErrorAndIsolatesOtherSessions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newTestStore(t)
	// A transcript with no recognized safe records fails the filter on every
	// pass. (A missing transcript used to stand in here; it is now a recorded
	// capture gap instead of an error, see TestMissingTranscript*.)
	badReg := registration(t, writeTranscript(t, dir, "unsafe.jsonl", `{"type":"unrecognized_record"}`+"\n"))
	badReg.ArchiveSessionID, badReg.NativeSessionID = "bad-session", "bad-native"
	if err := local.SaveRegistration(badReg); err != nil {
		t.Fatal(err)
	}
	goodPath := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	if err := local.SaveRegistration(registration(t, goodPath)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := local.SaveRequest("bad-session", "stop", now); err != nil {
		t.Fatal(err)
	}

	store := storagetest.NewMemoryStore()
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 || result.Published[0] != "session-1" {
		t.Fatalf("good session did not publish: %#v", result)
	}
	if _, ok := result.Errors["bad-session"]; !ok {
		t.Fatalf("expected an error for bad-session, got %#v", result)
	}
	requests, err := local.LoadRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].ArchiveSessionID != "bad-session" {
		t.Fatalf("request should remain pending for retry: %#v", requests)
	}
}

func TestRunFoldsHookEvidenceAndCompletesRequest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}

	// Transcript content is unchanged, but a stop hook delivers new
	// supplemental evidence (a final response). That alone must count as new
	// evidence worth republishing.
	t1 := t0.Add(10 * time.Minute)
	evidence := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t1, Provenance: "hook", Payload: map[string]any{"turn_id": "t1"}}
	if err := local.SaveRequest("session-1", "stop", t1, evidence); err != nil {
		t.Fatal(err)
	}

	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 {
		t.Fatalf("expected hook evidence to trigger a republish: %#v", result)
	}
	requests, err := local.LoadRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Fatalf("request should be completed after being folded in: %#v", requests)
	}
}

func TestRunUnsafeTranscriptNeverPublishes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// A record with no recognized type and no role is an unknown record;
	// combined with nothing else retainable, NewSourceBundle refuses it.
	path := writeTranscript(t, dir, "codex.jsonl", `{"type":"unknown_kind","x":1}`)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 0 || len(result.Errors) != 1 {
		t.Fatalf("result=%#v", result)
	}
	if _, err := store.List(context.Background(), "sessions"); err != nil {
		t.Fatal(err)
	} else if objs, _ := store.List(context.Background(), "sessions"); len(objs) != 0 {
		t.Fatalf("no object should have been uploaded: %#v", objs)
	}
}

func TestRunSurvivesRestartAcrossRateLimitedPass(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	local1, err := openTestStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := local1.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), local1, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	tDetected := t0.Add(30 * time.Second)
	if result, err := Run(context.Background(), local1, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return tDetected }}); err != nil {
		t.Fatal(err)
	} else if len(result.Waiting) != 1 {
		t.Fatalf("expected a publication waiting before restart: %#v", result)
	}

	// Simulate a process restart: a fresh Store handle over the same
	// root directory, as a newly started collector process would construct.
	local2, err := openTestStore(root)
	if err != nil {
		t.Fatal(err)
	}
	tPublish := t0.Add(4 * time.Minute)
	result, err := Run(context.Background(), local2, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return tPublish }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 {
		t.Fatalf("withheld candidate should survive restart and publish: %#v", result)
	}
	published := fetchMetadata(t, store, "codex", "session-1")
	if !published.CapturedAt.Equal(tDetected) {
		t.Fatalf("captured_at=%s want=%s across restart", published.CapturedAt, tDetected)
	}
}

func TestRunRetriesPersistedBytesAndDoesNotAcknowledgeNewerRequest(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local1, err := openTestStore(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := local1.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	firstEvidence := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t0, Provenance: "hook", Payload: map[string]any{"turn_id": "first"}}
	if err := local1.SaveRequest("session-1", "stop", t0, firstEvidence); err != nil {
		t.Fatal(err)
	}
	store := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	result, err := Run(context.Background(), local1, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }, Retry: storage.RetryPolicy{MaxAttempts: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected injected publication failure: %#v", result)
	}
	pending, found, err := local1.LoadPending("session-1")
	if err != nil || !found || !pending.Attempted {
		t.Fatalf("pending=%#v found=%v err=%v", pending, found, err)
	}
	if completed, err := local1.CompleteRequest("session-1", pending.RequestToken); err != nil || !completed {
		t.Fatalf("simulate pre-crash acknowledgement: completed=%v err=%v", completed, err)
	}

	// Both the transcript and request advance before restart. Retrying the
	// interrupted transaction must still use the already-rendered bytes, and
	// must not acknowledge the request revision it did not cover.
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"later","payload":{"type":"message","role":"user","content":"later"}}`)
	t1 := t0.Add(time.Minute)
	secondEvidence := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t1, Provenance: "hook", Payload: map[string]any{"turn_id": "second"}}
	if err := local1.SaveRequest("session-1", "session_end", t1, secondEvidence); err != nil {
		t.Fatal(err)
	}
	store.failMetadata = false
	local2, err := openTestStore(home)
	if err != nil {
		t.Fatal(err)
	}
	result, err = Run(context.Background(), local2, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }, Retry: storage.RetryPolicy{MaxAttempts: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 {
		t.Fatalf("persisted publication was not retried: %#v", result)
	}
	gotSource, err := store.Get(context.Background(), pending.SourceKey)
	if err != nil || !bytes.Equal(gotSource, pending.SourceBytes) {
		t.Fatalf("source retry changed persisted bytes: err=%v", err)
	}
	gotMetadata, err := store.Get(context.Background(), pending.MetadataKey)
	if err != nil || !bytes.Equal(gotMetadata, pending.MetadataBytes) {
		t.Fatalf("metadata retry changed persisted bytes: err=%v", err)
	}
	requests, err := local2.LoadRequests()
	if err != nil || len(requests) != 1 || requests[0].Token == pending.RequestToken {
		t.Fatalf("newer request was incorrectly acknowledged: %#v err=%v", requests, err)
	}
}

func TestStopRequestFlushesRateLimitAndPreservesEarlierHookEvidence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	e0 := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t0, Provenance: "hook", Payload: map[string]any{"turn_id": "first"}}
	if err := local.SaveRequest("session-1", "stop", t0, e0); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	t1 := t0.Add(30 * time.Second)
	if result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }}); err != nil || len(result.Published) != 0 {
		t.Fatalf("expected an active rate limit: result=%#v err=%v", result, err)
	}
	t2 := t1.Add(time.Second)
	e1 := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t2, Provenance: "hook", Payload: map[string]any{"turn_id": "second"}}
	if err := local.SaveRequest("session-1", "stop", t2, e1); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t2 }})
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("stop request did not flush debounce: result=%#v err=%v", result, err)
	}
	bundle := fetchBundle(t, store, fetchMetadata(t, store, "codex", "session-1"))
	if len(bundle.SupplementalEvidence) != 2 {
		t.Fatalf("hook evidence was lost across scans: %#v", bundle.SupplementalEvidence)
	}
}

// A session that is being used generates a lifecycle event on every prompt.
// That evidence must ride the normal debounce, not force an upload each time.
func TestPromptEvidenceRidesTheUploadIntervalAndIsPublished(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := local.SaveEvidence("session-1", "sessionstart", t0, lifecycleEvidence(t0, "SessionStart")); err != nil {
		t.Fatal(err)
	}
	if result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil || len(result.Published) != 1 {
		t.Fatalf("first publication is never rate limited: result=%#v err=%v", result, err)
	}
	if requests, _ := local.LoadRequests(); len(requests) != 0 {
		t.Fatalf("deferred request was not acknowledged by its publication: %#v", requests)
	}
	beforePrompts, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	const prompts = 5
	var at time.Time
	for i := 1; i <= prompts; i++ {
		at = t0.Add(time.Duration(i) * 10 * time.Second)
		if err := local.SaveEvidence("session-1", "userpromptsubmit", at, lifecycleEvidence(at, "UserPromptSubmit")); err != nil {
			t.Fatal(err)
		}
		result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at }, MinUploadInterval: 3 * time.Minute})
		if err != nil || len(result.Published) != 0 || len(result.Waiting) != 1 {
			t.Fatalf("prompt %d forced an upload inside the interval: result=%#v err=%v", i, result, err)
		}
	}
	if objects, err := store.List(context.Background(), ""); err != nil || !reflect.DeepEqual(objects, beforePrompts) {
		t.Fatalf("prompts uploaded new objects: %v err=%v", objects, err)
	}
	requests, err := local.LoadRequests()
	if err != nil || len(requests) != 1 || !requests[0].Deferred || len(requests[0].HookEvidence) != prompts || !requests[0].RequestedAt.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("prompt evidence must accumulate on one deferred request: %#v err=%v", requests, err)
	}
	tPublish := t0.Add(4 * time.Minute)
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return tPublish }, MinUploadInterval: 3 * time.Minute})
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("deferred evidence was never published: result=%#v err=%v", result, err)
	}
	bundle := fetchBundle(t, store, fetchMetadata(t, store, "codex", "session-1"))
	if len(bundle.SupplementalEvidence) != prompts+1 {
		t.Fatalf("published bundle lost prompt evidence: %#v", bundle.SupplementalEvidence)
	}
	if requests, _ := local.LoadRequests(); len(requests) != 0 {
		t.Fatalf("deferred request outlived its publication: %#v", requests)
	}
}

// A stop after prompt evidence is still the debounce flush it always was, and
// the prompt evidence it follows is published with it.
func TestStopAfterPromptEvidenceFlushesImmediately(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(10 * time.Second)
	if err := local.SaveEvidence("session-1", "userpromptsubmit", t1, lifecycleEvidence(t1, "UserPromptSubmit")); err != nil {
		t.Fatal(err)
	}
	if result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }, MinUploadInterval: 3 * time.Minute}); err != nil || len(result.Published) != 0 {
		t.Fatalf("prompt evidence bypassed the interval: result=%#v err=%v", result, err)
	}
	t2 := t1.Add(10 * time.Second)
	if err := local.SaveRequest("session-1", "stop", t2, lifecycleEvidence(t2, "Stop")); err != nil {
		t.Fatal(err)
	}
	requests, err := local.LoadRequests()
	if err != nil || len(requests) != 1 || requests[0].Deferred || !requests[0].RequestedAt.Equal(t2) || len(requests[0].Reasons) != 2 {
		t.Fatalf("stop did not make the pending request urgent: %#v err=%v", requests, err)
	}
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t2 }, MinUploadInterval: 3 * time.Minute})
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("stop request did not flush debounce: result=%#v err=%v", result, err)
	}
	bundle := fetchBundle(t, store, fetchMetadata(t, store, "codex", "session-1"))
	if len(bundle.SupplementalEvidence) != 2 {
		t.Fatalf("prompt evidence was lost by the flush: %#v", bundle.SupplementalEvidence)
	}
	if requests, _ := local.LoadRequests(); len(requests) != 0 {
		t.Fatalf("request outlived its publication: %#v", requests)
	}
}

func lifecycleEvidence(at time.Time, name string) archive.SupplementalEvidence {
	return archive.SupplementalEvidence{
		Kind: archive.EvidenceKindLifecycleHook, ObservedAt: at,
		Provenance: "hook:codex:" + strings.ToLower(name), Payload: map[string]any{"event_name": name},
	}
}

func TestRunPreservesLastGoodSnapshotAcrossTranscriptRewrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	before := fetchMetadata(t, store, "codex", "session-1")
	writeTranscript(t, dir, "codex.jsonl", `{"type":"turn_context","model":"gpt-test"}`)
	t1 := t0.Add(10 * time.Minute)
	if err := local.SaveRequest("session-1", "stop", t1); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("rewrite should be a recorded gap, not a failure: %#v", result)
	}
	after := fetchMetadata(t, store, "codex", "session-1")
	if after.SourceBundle.SHA256 != before.SourceBundle.SHA256 {
		t.Fatalf("rewrite replaced richer last-good source: before=%s after=%s", before.SourceBundle.SHA256, after.SourceBundle.SHA256)
	}
	requests, err := local.LoadRequests()
	if err != nil || len(requests) != 0 {
		t.Fatalf("blocked request should be acknowledged: %#v err=%v", requests, err)
	}
	if scanPending, err := local.ScanPending("session-1"); err != nil || scanPending {
		t.Fatalf("blocked session left scan pending: %v err=%v", scanPending, err)
	}
	reason, blocked, err := local.LoadBlocked("session-1")
	if err != nil || !blocked || reason != state.BlockedReasonTranscriptRewritten {
		t.Fatalf("blocked=%v reason=%q err=%v", blocked, reason, err)
	}
	if _, at, found, err := local.LoadLastPublished("session-1"); err != nil || !found || !at.Equal(t0) {
		t.Fatalf("last published snapshot was not retained: found=%v at=%s err=%v", found, at, err)
	}

	// Nothing changed: the next pass is a no-op, not a repeated failure.
	t2 := t1.Add(10 * time.Minute)
	if err := local.SaveRequest("session-1", "end", t2); err != nil {
		t.Fatal(err)
	}
	result, err = Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t2 }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("unchanged blocked session was reprocessed: result=%#v err=%v", result, err)
	}
	status, err := local.LoadStatus()
	if err != nil || status.LastError != "" || status.PendingCount != 0 {
		t.Fatalf("blocked session reported as pending or failed: %+v err=%v", status, err)
	}
	if requests, err := local.LoadRequests(); err != nil || len(requests) != 0 {
		t.Fatalf("request on unchanged blocked session not acknowledged: %#v err=%v", requests, err)
	}
	if scanPending, err := local.ScanPending("session-1"); err != nil || scanPending {
		t.Fatalf("scan left pending: %v err=%v", scanPending, err)
	}
	if fetchMetadata(t, store, "codex", "session-1").SourceBundle.SHA256 != before.SourceBundle.SHA256 {
		t.Fatal("last-good source changed on a no-op pass")
	}

	// Once the transcript again extends the retained snapshot, capture resumes.
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"more"}}`)
	t3 := t2.Add(10 * time.Minute)
	result, err = Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t3 }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("restored transcript was not republished: result=%#v err=%v", result, err)
	}
	if _, blocked, err := local.LoadBlocked("session-1"); err != nil || blocked {
		t.Fatalf("session still blocked after republish: blocked=%v err=%v", blocked, err)
	}
}

// A transcript emptied after it was published is a rewrite: a recorded gap
// that keeps the last published snapshot and acknowledges the request, not
// a failure on every pass.
//
// Regression: 2026-09 pre-release review, collector bug 3.
func TestTranscriptEmptiedAfterPublicationIsARewriteGap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	store := newTestStore(t)
	if err := store.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	cloud := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	before := fetchMetadata(t, cloud, "codex", "session-1")

	writeTranscript(t, dir, "codex.jsonl", "")
	for i, at := range []time.Time{t0.Add(10 * time.Minute), t0.Add(20 * time.Minute)} {
		if err := store.SaveRequest("session-1", "stop", at); err != nil {
			t.Fatal(err)
		}
		result, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at }})
		if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
			t.Fatalf("pass %d: emptied transcript should be a recorded gap: result=%#v err=%v", i, result, err)
		}
		if requests, err := store.LoadRequests(); err != nil || len(requests) != 0 {
			t.Fatalf("pass %d: request on an emptied transcript not acknowledged: %#v err=%v", i, requests, err)
		}
		if reason, blocked, err := store.LoadBlocked("session-1"); err != nil || !blocked || reason != state.BlockedReasonTranscriptRewritten {
			t.Fatalf("pass %d: blocked=%v reason=%q err=%v", i, blocked, reason, err)
		}
		if status, err := store.LoadStatus(); err != nil || status.LastError != "" {
			t.Fatalf("pass %d: status reports a failure: %+v err=%v", i, status, err)
		}
	}
	if after := fetchMetadata(t, cloud, "codex", "session-1"); after.SourceBundle.SHA256 != before.SourceBundle.SHA256 {
		t.Fatal("an emptied transcript replaced the published snapshot")
	}

	// Content that again extends the snapshot resumes capture.
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"more"}}`)
	t3 := t0.Add(30 * time.Minute)
	result, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t3 }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("refilled transcript was not republished: result=%#v err=%v", result, err)
	}
	if _, blocked, err := store.LoadBlocked("session-1"); err != nil || blocked {
		t.Fatalf("session still blocked after republish: blocked=%v err=%v", blocked, err)
	}
}

// nativeEvidenceExtends compares filtered records, so records filtered by
// different versions never read as a rewrite; a rewrite across an upgrade is
// found from the transcript instead (see refilter.go).
func TestNativeEvidenceExtendsYieldsToVersionChange(t *testing.T) {
	t.Parallel()
	previous := archive.SourceBundle{
		NativeRecords: []map[string]any{{"a": 1}, {"b": 2}},
		Capture:       archive.SourceCapture{AdapterVersion: "1"},
	}
	candidate := archive.SourceBundle{
		NativeRecords: []map[string]any{{"a": 1}},
		Capture:       archive.SourceCapture{AdapterVersion: "1"},
	}
	if nativeEvidenceExtends(claude.Filter{}, previous, candidate) {
		t.Fatal("same-version truncation must still be caught")
	}
	candidate.Capture.AdapterVersion = "2"
	if !nativeEvidenceExtends(claude.Filter{}, previous, candidate) {
		t.Fatal("adapter version change must not read as a rewrite")
	}
}

// simulateFilterUpgrade rewrites session-1's local state as an earlier
// release with filter version "0" would have left it: the cached and last
// published bundles, and the scan signature, so the next pass reads the
// transcript again as it does after a real upgrade.
func simulateFilterUpgrade(t *testing.T, store *state.Store) {
	t.Helper()
	simulateFilterUpgradeOf(t, store, "session-1")
}

// simulateFilterUpgradeOf is simulateFilterUpgrade for the session id.
func simulateFilterUpgradeOf(t *testing.T, store *state.Store, id string) {
	t.Helper()
	var file publishedFile
	if err := local.Read(publishedPath(store, id), &file); err != nil {
		t.Fatal(err)
	}
	file.Bundle.Capture.FilterVersion = "0"
	if file.LastPublished != nil {
		file.LastPublished.Bundle.Capture.FilterVersion = "0"
	}
	if err := local.Write(publishedPath(store, id), file); err != nil {
		t.Fatal(err)
	}
	signature, found, err := store.LoadScanSignature(id)
	if err != nil || !found {
		t.Fatalf("no scan signature to upgrade: found=%v err=%v", found, err)
	}
	signature.FilterVersion = "0"
	if err := store.SaveScanSignature(id, signature); err != nil {
		t.Fatal(err)
	}
}

// A filter or adapter upgrade re-filters every session, but a transcript
// that has not changed since it was captured holds no new activity: the
// republished snapshot keeps its capture time, which retention and the
// reader's date filters and ordering go by. A transcript that did change
// under the new filter is captured at the time it was read.
//
// Regression: 2026-09 pre-release review, collector bug 1.
func TestFilterUpgradeKeepsCaptureTimeOfUnchangedTranscript(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	store := newTestStore(t)
	if err := store.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	cloud := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	// Model the old release's *remote* source as well as its local cache.
	// Rewriting only local state leaves remote metadata pointing at a source
	// already made by the current filter, so there is no old predecessor.
	published, err := store.LoadPublishedState("session-1")
	if err != nil {
		t.Fatal(err)
	}
	oldBundle, _, found := published.LastPublished()
	if !found {
		t.Fatal("missing initial publication")
	}
	oldBundle.Capture.FilterVersion = "0"
	oldCompressed, err := archive.BuildCompressedSource(oldBundle)
	if err != nil {
		t.Fatal(err)
	}
	priorSource, err := archive.SourceObjectKey(oldBundle, oldCompressed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	oldRef := archive.SourceReference{Key: priorSource, SHA256: oldCompressed.SHA256, CompressedBytes: len(oldCompressed.Bytes)}
	oldMeta := fetchMetadata(t, cloud, "codex", "session-1")
	oldMeta.SourceBundle = oldRef
	oldMeta.FilterVersion = "0"
	oldMetaBytes, err := json.Marshal(oldMeta)
	if err != nil {
		t.Fatal(err)
	}
	metaKey, _ := archive.MetadataObjectKey("codex", "session-1")
	if err := cloud.Put(context.Background(), priorSource, oldCompressed.Bytes); err != nil {
		t.Fatal(err)
	}
	if err := cloud.Put(context.Background(), metaKey, oldMetaBytes); err != nil {
		t.Fatal(err)
	}
	if err := published.SavePublication(oldBundle, t0, oldRef, oldMetaBytes); err != nil {
		t.Fatal(err)
	}
	simulateFilterUpgrade(t, store)
	t1 := t0.Add(20 * 24 * time.Hour)
	// A failed local ledger write must leave this publication pending so the
	// old-filter source remains eligible for cleanup after the fault clears.
	ledgerPath := filepath.Join(store.Home(), "superseded", "session-1.json")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ledgerPath, 0o700); err != nil {
		t.Fatal(err)
	}
	failed, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }})
	if err != nil || failed.Errors["session-1"] == nil {
		t.Fatalf("ledger fault must report a session error: %#v %v", failed, err)
	}
	if pending, err := store.HasPending("session-1"); err != nil || !pending {
		t.Fatalf("ledger fault lost the pending publication: pending=%v err=%v", pending, err)
	}
	if err := os.Remove(ledgerPath); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("filter upgrade should republish: result=%#v err=%v", result, err)
	}
	metadata := fetchMetadata(t, cloud, "codex", "session-1")
	if !metadata.CapturedAt.Equal(t0) {
		t.Fatalf("captured_at = %s after re-filtering an unchanged transcript, want %s", metadata.CapturedAt, t0)
	}
	if bundle := fetchBundle(t, cloud, metadata); bundle.Capture.FilterVersion != archive.FilterVersion || !bundle.Capture.CapturedAt.Equal(t0) {
		t.Fatalf("republished bundle: filter %q captured %s", bundle.Capture.FilterVersion, bundle.Capture.CapturedAt)
	}
	if ledger, err := store.LoadSuperseded("session-1"); err != nil || len(ledger) != 1 || ledger[0].Key != priorSource || !ledger[0].PrivacySensitive {
		t.Fatalf("filter upgrade predecessor not marked for privacy cleanup: %#v %v; prior=%s current=%s", ledger, err, priorSource, metadata.SourceBundle.Key)
	}

	// New activity arriving with the upgrade is captured when it was read.
	simulateFilterUpgrade(t, store)
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"more"}}`)
	t2 := t1.Add(24 * time.Hour)
	result, err = Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t2 }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("changed transcript should republish: result=%#v err=%v", result, err)
	}
	if got := fetchMetadata(t, cloud, "codex", "session-1").CapturedAt; !got.Equal(t2) {
		t.Fatalf("captured_at = %s for a transcript that changed, want %s", got, t2)
	}
}

// A filter upgrade that arrives with new evidence other than the
// transcript's (hook evidence, a changed skill observation), or on a source
// that can't be trusted unchanged on a stat (a Cursor text transcript), is
// captured when it was read.
func TestFilterUpgradeWithNewEvidenceIsCapturedNow(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	t1 := t0.Add(20 * 24 * time.Hour)
	type provider = func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error)
	inventory := func(name string) provider {
		return func(_ archive.SessionRegistration, observedAt time.Time) ([]archive.SupplementalEvidence, error) {
			return []archive.SupplementalEvidence{{Kind: archive.EvidenceKindSkillInventory, ObservedAt: observedAt, Provenance: "filesystem", Payload: map[string]any{"coverage": "installed_only", "skills": []any{map[string]any{"name": name}}}}}, nil
		}
	}
	for _, tc := range []struct {
		name    string
		first   provider
		upgrade func(t *testing.T, store *state.Store) provider
	}{
		{"a request with hook evidence", nil, func(t *testing.T, store *state.Store) provider {
			t.Helper()
			if err := store.SaveRequest("session-1", "stop", t1, lifecycleEvidence(t1, "Stop")); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{"a changed skill observation", inventory("one"), func(t *testing.T, store *state.Store) provider {
			t.Helper()
			if err := store.SaveRequest("session-1", "stop", t1); err != nil {
				t.Fatal(err)
			}
			return inventory("two")
		}},
		{"a Cursor text transcript", nil, func(t *testing.T, store *state.Store) provider {
			t.Helper()
			signature, _, err := store.LoadScanSignature("session-1")
			if err != nil {
				t.Fatal(err)
			}
			signature.SourceFormat = cursorTextSourceFormat
			if err := store.SaveScanSignature("session-1", signature); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
			store := newTestStore(t)
			if err := store.SaveRegistration(registration(t, path)); err != nil {
				t.Fatal(err)
			}
			cloud := storagetest.NewMemoryStore()
			if _, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }, SupplementalEvidence: tc.first}); err != nil {
				t.Fatal(err)
			}
			simulateFilterUpgrade(t, store)
			provider := tc.upgrade(t, store)
			result, err := Run(context.Background(), store, cloud, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }, SupplementalEvidence: provider})
			if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
				t.Fatalf("upgrade should republish: result=%#v err=%v", result, err)
			}
			if got := fetchMetadata(t, cloud, "codex", "session-1").CapturedAt; !got.Equal(t1) {
				t.Fatalf("captured_at = %s, want %s", got, t1)
			}
		})
	}
}

func TestStableSupplementalObservationDoesNotRepublish(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	now := t0
	provider := func(_ archive.SessionRegistration, observedAt time.Time) ([]archive.SupplementalEvidence, error) {
		return []archive.SupplementalEvidence{{Kind: archive.EvidenceKindSkillInventory, ObservedAt: observedAt, Provenance: "filesystem", Payload: map[string]any{"coverage": "installed_only", "skills": []any{map[string]any{"name": "review", "sha256": "abc"}}}}}, nil
	}
	options := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }, SupplementalEvidence: provider}
	if _, err := Run(context.Background(), local, store, options); err != nil {
		t.Fatal(err)
	}
	first := fetchMetadata(t, store, "codex", "session-1")
	now = t0.Add(10 * time.Minute)
	result, err := Run(context.Background(), local, store, options)
	if err != nil || len(result.Published) != 0 {
		t.Fatalf("poll timestamp manufactured a publication: result=%#v err=%v", result, err)
	}
	second := fetchMetadata(t, store, "codex", "session-1")
	if !second.MetadataDerivedAt.Equal(first.MetadataDerivedAt) {
		t.Fatalf("stable inventory changed metadata timestamp: %s -> %s", first.MetadataDerivedAt, second.MetadataDerivedAt)
	}
}

func TestChangedSupplementalInventoryPreservesEarlierObservation(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	first := archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillInventory, ObservedAt: t0, Provenance: "filesystem", Payload: map[string]any{"coverage": "installed_only", "skills": []any{map[string]any{"name": "one"}}}}
	second := archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillInventory, ObservedAt: t0.Add(time.Hour), Provenance: "filesystem", Payload: map[string]any{"coverage": "installed_only", "skills": []any{map[string]any{"name": "two"}}}}
	merged := mergeSupplementalEvidence([]archive.SupplementalEvidence{first}, []archive.SupplementalEvidence{second})
	if len(merged) != 2 || !merged[0].ObservedAt.Equal(t0) {
		t.Fatalf("inventory history was not preserved: %#v", merged)
	}
}

func TestRunUpgradesAndCompletesLegacyTokenlessRequest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	store := newTestStore(t)
	if err := store.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := local.Write(requestPath(store, "session-1"), state.Request{ArchiveSessionID: "session-1", Reasons: []string{"stop"}, RequestedAt: now}); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), store, storagetest.NewMemoryStore(), Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }})
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("legacy request was not processed: result=%#v err=%v", result, err)
	}
	requests, err := store.LoadRequests()
	if err != nil || len(requests) != 0 {
		t.Fatalf("legacy request remained pending: %#v err=%v", requests, err)
	}
}

// A raw transcript past the raw ceiling is refused on its size alone, without
// being read.
func TestRunRejectsTranscriptAboveCollectionLimit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "large.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: no disk space, and a read of it would take seconds.
	if err := file.Truncate(maxRawBytes(archive.HarnessCodex, DefaultMaxTranscriptBytes) + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("oversized transcript should be a recorded gap, not a failure: %#v", result)
	}
	if reason, blocked, err := local.LoadBlocked("session-1"); err != nil || !blocked || reason != state.BlockedReasonTranscriptTooLarge {
		t.Fatalf("blocked=%v reason=%q err=%v", blocked, reason, err)
	}
	signature, _, err := local.LoadScanSignature("session-1")
	want := state.SizeLimitGap{Kind: state.SizeLimitRaw, LimitBytes: archive.MaxCodexRawTranscriptBytes, SizeBytes: archive.MaxCodexRawTranscriptBytes + 1}
	if err != nil || signature.SizeLimit == nil || *signature.SizeLimit != want || signature.FailedRawLimit != archive.MaxCodexRawTranscriptBytes {
		t.Fatalf("gap does not name its limit: %+v err=%v", signature, err)
	}
	if status, err := local.LoadStatus(); err != nil || len(status.LastErrors) != 1 || !strings.Contains(status.LastErrors[0], "1 session(s) stopped being captured at a size limit (Codex raw 512 MiB)") {
		t.Fatalf("status problem does not name the limit: %q err=%v", status.LastErrors, err)
	}
	objects, _ := store.List(context.Background(), "sessions")
	if len(objects) != 0 {
		t.Fatalf("oversized transcript uploaded objects: %#v", objects)
	}
}

func TestRunOversizeTranscriptBlocksOnceAndRetainsSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	limit := int64(len(codexTranscript) + 16)
	options := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }, MaxTranscriptBytes: limit}
	if _, err := Run(context.Background(), local, store, options); err != nil {
		t.Fatal(err)
	}
	before := fetchMetadata(t, store, "codex", "session-1")

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n" + `{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"this record pushes the file over the limit"}}`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(10 * time.Minute)
	if err := local.SaveRequest("session-1", "stop", t1); err != nil {
		t.Fatal(err)
	}
	options.Now = func() time.Time { return t1 }
	result, err := Run(context.Background(), local, store, options)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("oversize should be a recorded gap: result=%#v err=%v", result, err)
	}
	if reason, blocked, err := local.LoadBlocked("session-1"); err != nil || !blocked || reason != state.BlockedReasonTranscriptTooLarge {
		t.Fatalf("blocked=%v reason=%q err=%v", blocked, reason, err)
	}
	if requests, err := local.LoadRequests(); err != nil || len(requests) != 0 {
		t.Fatalf("blocked request should be acknowledged: %#v err=%v", requests, err)
	}
	if scanPending, err := local.ScanPending("session-1"); err != nil || scanPending {
		t.Fatalf("blocked session left scan pending: %v err=%v", scanPending, err)
	}
	if _, at, found, err := local.LoadLastPublished("session-1"); err != nil || !found || !at.Equal(t0) {
		t.Fatalf("last published snapshot was not retained: found=%v at=%s err=%v", found, at, err)
	}
	if fetchMetadata(t, store, "codex", "session-1").SourceBundle.SHA256 != before.SourceBundle.SHA256 {
		t.Fatal("oversize pass changed the published source")
	}

	// Still oversize, nothing else changed: no work, no error, not pending.
	stat, err := os.Stat(publishedPath(local, "session-1"))
	if err != nil {
		t.Fatal(err)
	}
	t2 := t1.Add(10 * time.Minute)
	options.Now = func() time.Time { return t2 }
	result, err = Run(context.Background(), local, store, options)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("unchanged oversize session was reprocessed: result=%#v err=%v", result, err)
	}
	if again, err := os.Stat(publishedPath(local, "session-1")); err != nil || !again.ModTime().Equal(stat.ModTime()) || again.Size() != stat.Size() {
		t.Fatalf("published cache rewritten on a no-op pass: err=%v", err)
	}
	status, err := local.LoadStatus()
	if err != nil || !strings.Contains(status.LastError, "size limit") || status.PendingCount != 0 {
		t.Fatalf("blocked session reported as pending or failed: %+v err=%v", status, err)
	}

	// Raising the limit resumes capture against the retained snapshot.
	options.MaxTranscriptBytes = 0
	t3 := t2.Add(10 * time.Minute)
	options.Now = func() time.Time { return t3 }
	result, err = Run(context.Background(), local, store, options)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("transcript within limit was not republished: result=%#v err=%v", result, err)
	}
	if _, blocked, err := local.LoadBlocked("session-1"); err != nil || blocked {
		t.Fatalf("session still blocked after republish: blocked=%v err=%v", blocked, err)
	}
}

func TestForgetSessionRemovesRequestLock(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := store.SaveRegistration(registration(t, "/unused")); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRequest("session-1", "stop", now); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(store.Home(), "request-locks", "session-1.lock")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("request lock was not created: %v", err)
	}
	if err := store.ForgetSession("session-1", agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("codex")), NativeID: "native-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("request lock leaked after ForgetSession: %v", err)
	}
	if err := store.ForgetSession("session-1", agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("codex")), NativeID: "native-1"}); err != nil {
		t.Fatalf("forgetting twice must be a no-op: %v", err)
	}
}

func TestForgetSessionRemovesVerificationRecordAndEmptyDirectory(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	dir := store.SessionDir("session-1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verification.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.ForgetSession("session-1", agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("codex")), NativeID: "native-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("per-session directory leaked after ForgetSession: %v", err)
	}
	// Anything unexpected in the directory is preserved, not deleted blindly.
	other := store.SessionDir("session-2")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "notes.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.ForgetSession("session-2", agentmeta.SessionKey{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "notes.txt")); err != nil {
		t.Fatalf("unrelated file removed: %v", err)
	}
}

func TestRunIgnoresIncompleteFinalJSONLRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	before := fetchMetadata(t, store, "codex", "session-1")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n" + `{"type":"response_item","id":"partial"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0.Add(10 * time.Minute) }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("partial final record changed capture: result=%#v err=%v", result, err)
	}
	after := fetchMetadata(t, store, "codex", "session-1")
	if after.SourceBundle.SHA256 != before.SourceBundle.SHA256 {
		t.Fatalf("partial record changed source: %s -> %s", before.SourceBundle.SHA256, after.SourceBundle.SHA256)
	}
}

func TestSaveRequestCoalescesReasonsAndEvidence(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, "/unused")
	reg.ArchiveSessionID = "s1"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e1 := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t0, Provenance: "hook", Payload: map[string]any{"a": 1}}
	if err := local.SaveRequest("s1", "stop", t0, e1); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Second)
	e2 := archive.SupplementalEvidence{Kind: archive.EvidenceKindLifecycleHook, ObservedAt: t1, Provenance: "hook", Payload: map[string]any{"b": 2}}
	if err := local.SaveRequest("s1", "session_end", t1, e2); err != nil {
		t.Fatal(err)
	}
	requests, err := local.LoadRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("expected coalesced single request, got %d", len(requests))
	}
	req := requests[0]
	if !req.RequestedAt.Equal(t1) || len(req.Reasons) != 2 || len(req.HookEvidence) != 2 {
		t.Fatalf("req=%#v", req)
	}
}

// TestRunComposesWithLocalLock proves the intended usage pattern for a
// future scheduled caller: local.Lock(home) around Run prevents a second
// overlapping collector process from acting on the same home concurrently.
// Locking itself is internal/local's responsibility and is tested there.
func TestRunComposesWithLocalLock(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	unlock, err := local.Lock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	if _, err := local.Lock(home); !errors.Is(err, local.ErrBusy) {
		t.Fatalf("expected a second collector run to be excluded, got %v", err)
	}

	localStore, err := openTestStore(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), localStore, storagetest.NewMemoryStore(), Options{Sources: testSources, Parsers: testParsers, MachineID: "m"}); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureArchiveSessionIDPersistsAndReuses(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	id1, created1, err := local.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("codex")), NativeID: "native-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if !created1 || id1 == "" {
		t.Fatalf("id1=%q created1=%v", id1, created1)
	}
	id2, created2, err := local.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("codex")), NativeID: "native-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if created2 || id2 != id1 {
		t.Fatalf("expected reuse: id1=%q id2=%q created2=%v", id1, id2, created2)
	}
	id3, created3, err := local.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("codex")), NativeID: "native-xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if !created3 || id3 == id1 {
		t.Fatalf("expected a distinct fresh ID for a different native session: id3=%q", id3)
	}
}

func TestArchiveSessionIDRejectsPathLikeInputSafely(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	local, err := openTestStore(home)
	if err != nil {
		t.Fatal(err)
	}
	// A native session ID is harness-controlled input; it must not be usable
	// to escape the sessions/ directory even though it is only ever hashed,
	// not used directly as a path component.
	id, _, err := local.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("codex")), NativeID: "../../etc/passwd"})
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected a valid archive session ID")
	}
	// Checked against home, the store's actual base directory — not some
	// other, unrelated temp directory — so this would actually catch a
	// future regression that built a path from the native ID directly.
	if _, err := os.Stat(filepath.Join(home, "..", "..", "etc", "passwd")); err == nil {
		t.Fatal("unexpected file escape")
	}
}

func TestRunDeclinesPublishWhenSkillUseRequiredAndAbsent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }, RequireSkillUse: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("expected a policy decline, not a publish: %#v", result)
	}
	if objs, err := store.List(context.Background(), "sessions"); err != nil || len(objs) != 0 {
		t.Fatalf("nothing should have been uploaded: objs=%#v err=%v", objs, err)
	}

	// A later scan with unchanged content must not retry the publish just
	// because time passed (unlike a rate-limited candidate).
	later := now.Add(24 * time.Hour)
	result, err = Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return later }, RequireSkillUse: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("a decline must not auto-publish later: %#v", result)
	}
}

// TestRunDeclinedCandidateIsNotSpuriouslyRateLimitedOnLaterChange guards a
// regression where a declined candidate was cached with PublishedAt set to
// its capture time instead of a zero time. Since nothing was ever actually
// published, a later genuine content change (still within the rate-limit
// window) must not be mistaken for a retry of a real publish and withheld;
// it should be evaluated (and, here, declined again) immediately.
func TestRunDeclinedCandidateIsNotSpuriouslyRateLimitedOnLaterChange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }, RequireSkillUse: true}); err != nil {
		t.Fatal(err)
	}
	_, publishedAt, status, found, err := local.LoadPublished("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found || status != state.CacheStatusDeclined || !publishedAt.IsZero() {
		t.Fatalf("expected a zero-time declined cache entry: found=%v status=%v publishedAt=%v", found, status, publishedAt)
	}

	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"still no skill use"}}`)
	t1 := t0.Add(30 * time.Second)
	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }, RequireSkillUse: true, MinUploadInterval: 3 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	_, _, status2, found2, err := local.LoadPublished("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found2 || status2 != state.CacheStatusDeclined {
		t.Fatalf("expected an immediate re-decline, not a spurious rate limit: found=%v status=%v", found2, status2)
	}
}

func TestRunRecordsSupersededSourceOnRepublish(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	firstMetadata := fetchMetadata(t, store, "codex", "session-1")

	// Republish with new content well past the rate limit, so it actually
	// supersedes the first snapshot.
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	t1 := t0.Add(10 * time.Minute)
	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t1 }}); err != nil {
		t.Fatal(err)
	}

	superseded, err := local.LoadSuperseded("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(superseded) != 1 || superseded[0].Key != firstMetadata.SourceBundle.Key || !superseded[0].SupersededAt.Equal(t1) {
		t.Fatalf("superseded=%#v firstKey=%q", superseded, firstMetadata.SourceBundle.Key)
	}
	if superseded[0].PrivacySensitive {
		t.Fatal("ordinary republish marked as privacy-sensitive")
	}
	// The superseded object must still exist in storage (grace period).
	if _, err := store.Get(context.Background(), firstMetadata.SourceBundle.Key); err != nil {
		t.Fatalf("superseded source must remain until retention deletes it: %v", err)
	}
}

// TestRunFallsBackToCursorTextWhenJSONLIsUnrecognized guards against a
// regression where CursorAdapter.FilterText — fully implemented and unit
// tested in the archive package — was never actually reachable from the
// real collection pipeline. The spec notes Cursor's hook-provided
// transcript path can point to either a JSONL or a plain text transcript
// depending on version; a text one must still publish, not be silently
// dropped just because it isn't JSONL.
func TestRunFallsBackToCursorTextWhenJSONLIsUnrecognized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	textTranscript := "user: hello\nassistant: hi there\n"
	path := writeTranscript(t, dir, "cursor.txt", textTranscript)
	local := newTestStore(t)
	reg := registration(t, path)
	reg.Harness = archive.Harness{Name: "cursor"}
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	store := storagetest.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 || len(result.Errors) != 0 {
		t.Fatalf("expected the Cursor text transcript to publish via the text fallback: %#v", result)
	}
	metadata := fetchMetadata(t, store, "cursor", "session-1")
	if metadata.SourceBundle.Key == "" {
		t.Fatalf("expected a verified source reference: %#v", metadata)
	}
}

func TestRecordSupersededMovesRepeatedKeyToEnd(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	// A -> B -> A -> C: A is superseded twice, B once in between.
	for _, step := range []struct {
		key string
		at  time.Time
	}{
		{"sessions/codex/s1/source.a.jsonl.gz", t0},
		{"sessions/codex/s1/source.b.jsonl.gz", t0.Add(time.Hour)},
		{"sessions/codex/s1/source.a.jsonl.gz", t0.Add(2 * time.Hour)},
	} {
		if err := local.RecordSuperseded("s1", step.key, step.at); err != nil {
			t.Fatal(err)
		}
	}
	ledger, err := local.LoadSuperseded("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 2 || ledger[0].Key != "sessions/codex/s1/source.b.jsonl.gz" || ledger[1].Key != "sessions/codex/s1/source.a.jsonl.gz" {
		t.Fatalf("re-superseded key must move to the end of the ledger: %#v", ledger)
	}
	if !ledger[1].SupersededAt.Equal(t0.Add(2 * time.Hour)) {
		t.Fatalf("re-superseded key must refresh SupersededAt: %#v", ledger[1])
	}
}

func openTestStore(home string) (*state.Store, error) {
	if err := os.Chmod(home, 0700); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	store, err := state.Open(home)
	if err != nil {
		return nil, err
	}
	_, found, err := config.Load(home)
	if err != nil {
		return nil, err
	}
	if !found {
		err = config.Save(home, config.Config{MachineID: "synthetic"})
	}
	return store, err
}
