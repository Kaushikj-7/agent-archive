package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// injectPublishedMetadata preserves the leading protocol/summary and frozen
// selecting proof while injecting the exact unsafe body a reader must refuse.
// Production CacheMetadata cannot persist this deliberately invalid selection.
func injectPublishedMetadata(t *testing.T, local *state.Store, id string, previous, next []byte) {
	t.Helper()
	path := publishedPath(local, id)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	oldToken, err := json.Marshal(previous)
	if err != nil {
		t.Fatal(err)
	}
	newToken, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	oldField := append([]byte(`"metadata_bytes":`), oldToken...)
	newField := append([]byte(`"metadata_bytes":`), newToken...)
	if bytes.Count(raw, oldField) != 1 {
		t.Fatal("unsafe fixture did not find exact selecting metadata field")
	}
	if err := os.WriteFile(path, bytes.Replace(raw, oldField, newField, 1), 0600); err != nil {
		t.Fatal(err)
	}
}

const grownTranscript = codexTranscript + "\n" + `{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"more"}}` + "\n"

// editPublishedState rewrites session-1's published state as raw JSON, to
// stand in for state an older build wrote.
func editPublishedState(t *testing.T, local *state.Store, edit func(state map[string]any)) {
	t.Helper()
	const id = "session-1"
	data, err := os.ReadFile(publishedPath(local, id))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	// These fixtures model legacy published files, which had neither the new
	// selecting source-set identity nor a source-set preparation projection.
	// Malformed selecting controls are injected directly by their own tests.
	for _, field := range []string{"publication_version", "source_set_commit5", "commit", "sources", "predecessor_unknown", "preparation", "payloads", "privacy_receipts", "settled_privacy", "cleanup"} {
		delete(state, field)
	}
	edit(state)
	if data, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publishedPath(local, id), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// olderSourceSchema marks the cached bundle as written under source schema 1,
// which this build can no longer serialize: what every session's cache looks
// like right after a SourceSchemaVersion bump.
func olderSourceSchema(state map[string]any) {
	state["bundle"].(map[string]any)["schema_version"] = 1
}

// withoutRecordedSource drops publishedSnapshot.Source, as state written
// before it existed has none.
func withoutRecordedSource(state map[string]any) {
	if last, ok := state["last_published"].(map[string]any); ok {
		delete(last, "source")
	}
}

// publishThenGrow publishes session-1 at t0, then grows its transcript and
// queues a stop request, and returns the first publication's source key.
func publishThenGrow(t *testing.T, local *state.Store, store storage.ObjectStore, t0 time.Time) string {
	t.Helper()
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	if result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil || len(result.Published) != 1 {
		t.Fatalf("first publication: %#v %v", result, err)
	}
	firstKey := fetchMetadata(t, store, "codex", "session-1").SourceBundle.Key
	writeTranscript(t, filepath.Dir(path), "codex.jsonl", grownTranscript)
	if err := local.SaveRequest("session-1", "stop", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return firstKey
}

func assertRepublishedSuperseding(t *testing.T, local *state.Store, store storage.ObjectStore, now time.Time, wantSuperseded []string) {
	t.Helper()
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("republish: %#v %v", result, err)
	}
	if pending, err := local.HasPending("session-1"); err != nil || pending {
		t.Fatalf("publication left pending: %v %v", pending, err)
	}
	superseded, err := local.LoadSuperseded("session-1")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, s := range superseded {
		keys = append(keys, s.Key)
	}
	if strings.Join(keys, ",") != strings.Join(wantSuperseded, ",") {
		t.Fatalf("superseded = %v, want %v", keys, wantSuperseded)
	}
	// The source key the new publication recorded is the one its metadata
	// names, so the publication after it supersedes the right object.
	current := fetchMetadata(t, store, "codex", "session-1").SourceBundle
	if source, found, err := local.LoadLastPublishedSource("session-1"); err != nil || !found || source != current {
		t.Fatalf("recorded source = %#v %v %v, want %#v", source, found, err, current)
	}
}

// A source schema bump leaves every cached bundle at a schema this build
// cannot serialize. Publishing must still complete and record the object it
// really replaced, rather than failing after the upload on every pass.
func TestPublishAfterSourceSchemaBumpSupersedesUploadedKey(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	firstKey := publishThenGrow(t, local, store, t0)
	editPublishedState(t, local, olderSourceSchema)
	assertRepublishedSuperseding(t, local, store, t0.Add(time.Hour), []string{firstKey})
}

// State written before the source reference was recorded falls back to the
// cached metadata, which names exactly the uploaded object.
func TestPublishWithOlderStateReadsSupersededKeyFromCachedMetadata(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	firstKey := publishThenGrow(t, local, store, t0)
	editPublishedState(t, local, func(state map[string]any) {
		olderSourceSchema(state)
		withoutRecordedSource(state)
	})
	assertRepublishedSuperseding(t, local, store, t0.Add(time.Hour), []string{firstKey})
}

// With neither record, the exact predecessor is unknown: retain the readable
// remote publication and pending work instead of guessing replacement authority.
func TestPublishWithUnknownPreviousSourceRetainsPending(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	first := publishThenGrow(t, local, store, t0)
	editPublishedState(t, local, func(state map[string]any) {
		olderSourceSchema(state)
		withoutRecordedSource(state)
		delete(state, "metadata_bytes")
	})
	result, err := Run(t.Context(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return t0.Add(time.Hour) }})
	if err != nil || !errors.Is(result.Errors["session-1"], storage.ErrPublicationConflict) || len(result.Published) != 0 {
		t.Fatal(result, err)
	}
	if pending, err := local.HasPending("session-1"); err != nil || !pending {
		t.Fatal("unknown predecessor lost pending work", pending, err)
	}
	if got := fetchMetadata(t, store, "codex", "session-1").SourceBundle.Key; got != first {
		t.Fatal("unknown predecessor overwrote authority", got)
	}
}

// A parser upgrade over a cached bundle this build cannot reproduce skips the
// metadata-only refresh instead of failing the session; normal capture goes
// on and publishes current metadata.
func TestParserUpgradeOverUnreproducibleBundleDoesNotFailSession(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", ParserVersion: "one", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	editPublishedState(t, local, olderSourceSchema)
	now = now.Add(time.Hour)
	opts.ParserVersion = "two"
	result, err := Run(context.Background(), local, store, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if got := fetchMetadata(t, store, "codex", "session-1").Parser.Version; got != "two" {
		t.Fatalf("published parser = %q", got)
	}
}

// A publication stamped in the future by a wrong clock must not hold back
// the next one until that date: it defers by at most one interval.
func TestFutureLastPublicationDefersByAtMostOneInterval(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	future := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return future }}); err != nil {
		t.Fatal(err)
	}
	// The clock is corrected and the transcript grows, with no hook asking
	// for a flush.
	writeTranscript(t, filepath.Dir(path), "codex.jsonl", grownTranscript)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }}
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 0 {
		t.Fatalf("expected one interval's deferral: %#v %v", result, err)
	}
	now = now.Add(opts.minUploadInterval())
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 1 {
		t.Fatalf("still deferred after one interval: %#v %v", result, err)
	}
}

// A pending publication already carrying a far-future ReadyAt is capped at
// one interval from now, durably, so it publishes once that has passed.
func TestPendingReadyAtInTheFutureIsCapped(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }}
	if _, err := Run(context.Background(), local, store, opts); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, filepath.Dir(path), "codex.jsonl", grownTranscript)
	now = now.Add(time.Minute)
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	pending, found, err := local.LoadPending("session-1")
	if err != nil || !found {
		t.Fatalf("no rate-limited publication: %v %v", found, err)
	}
	pending.ReadyAt = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := local.SavePending("session-1", pending); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	now = now.Add(opts.minUploadInterval())
	if result, err := Run(context.Background(), local, store, opts); err != nil || len(result.Published) != 1 {
		t.Fatalf("far-future ReadyAt still holds the publication: %#v %v", result, err)
	}
}

func corruptFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`{"archive_session_id":"trunc`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// One truncated state file fails only its own session: it is moved aside and
// reported, every other session is published, and the pass completes.
func TestCorruptStateFilesAreQuarantinedPerSession(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	dir := t.TempDir()
	if err := local.SaveRegistration(registration(t, writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"))); err != nil {
		t.Fatal(err)
	}
	other := registration(t, writeTranscript(t, dir, "other.jsonl", codexTranscript+"\n"))
	other.ArchiveSessionID, other.NativeSessionID = "session-2", "native-2"
	if err := local.SaveRegistration(other); err != nil {
		t.Fatal(err)
	}
	// session-2's request, an unrelated request, a registration, and a
	// subagent candidate, each truncated.
	corruptFile(t, requestPath(local, "session-2"))
	corruptFile(t, requestPath(local, "orphan"))
	corruptFile(t, registrationPath(local, "broken"))
	corruptFile(t, subagentCandidatePath(local, "child"))

	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("one corrupt file failed the whole pass: %v", err)
	}
	if strings.Join(result.Published, ",") != "session-1,session-2" {
		t.Fatalf("published = %v", result.Published)
	}
	for _, id := range []string{"session-2", "orphan", "broken", "child"} {
		if !errors.Is(result.Errors[id], state.ErrQuarantined) {
			t.Fatalf("%s: error = %v, want state.ErrQuarantined", id, result.Errors[id])
		}
	}
	want := []string{"registrations/broken.json", "requests/orphan.json", "requests/session-2.json", "subagent-candidates/child.json"}
	status, err := local.LoadStatus()
	if err != nil || len(status.QuarantinedFiles) != len(want) {
		t.Fatalf("status quarantined = %v %v", status.QuarantinedFiles, err)
	}
	for i, rel := range want {
		if _, err := os.Stat(filepath.Join(local.Home(), rel)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is still in place: %v", rel, err)
		}
		if got := status.QuarantinedFiles[i]; !strings.HasPrefix(got, rel+".") || !strings.HasSuffix(got, ".corrupt") {
			t.Fatalf("quarantined as %q, want %s.<time>%s", got, rel, ".corrupt")
		}
		if _, err := os.Stat(filepath.Join(local.Home(), status.QuarantinedFiles[i])); err != nil {
			t.Fatalf("not quarantined: %v", err)
		}
	}
	if status.PendingCount != 1 {
		t.Fatalf("pending = %d, want the unreadable registration counted", status.PendingCount)
	}
	// Valid sessions keep scanning; unresolved identity evidence from the
	// quarantined registration leaves explicit recovery incomplete.
	if err := local.SaveRequest("session-2", "stop", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }}); err != nil || len(result.Errors) != 1 || !errors.Is(result.Errors["session-index"], state.ErrSessionIndexRecoveryRequired) {
		t.Fatalf("second pass: %#v %v", result, err)
	}
}

// A request that cannot be read for any other reason stays where it is and
// holds its session back, without failing the pass.
func TestUnreadableRequestHoldsOnlyItsSession(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	if err := local.SaveRegistration(registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n"))); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := local.SaveRequest("session-1", "stop", now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(requestPath(local, "session-1"), 0); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }})
	if err != nil || result.Errors["session-1"] == nil || errors.Is(result.Errors["session-1"], state.ErrQuarantined) || len(result.Published) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if _, err := os.Stat(requestPath(local, "session-1")); err != nil {
		t.Fatalf("unreadable request was moved: %v", err)
	}
}

// A local failure partway through one session is that session's error; the
// pass goes on to the others and still records its status.
func TestMidPassLocalFailureIsPerSession(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	dir := t.TempDir()
	if err := local.SaveRegistration(registration(t, writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"))); err != nil {
		t.Fatal(err)
	}
	stuck := registration(t, writeTranscript(t, dir, "stuck.jsonl", codexTranscript+"\n"))
	stuck.ArchiveSessionID, stuck.NativeSessionID = "session-0", "native-0"
	if err := local.SaveRegistration(stuck); err != nil {
		t.Fatal(err)
	}
	// The scan journal cannot be written for session-0: a non-empty
	// directory occupies its path.
	if err := os.MkdirAll(filepath.Join(local.Home(), "pending-scans", "session-0.json", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("one session's local failure ended the pass: %v", err)
	}
	if result.Errors["session-0"] == nil || strings.Join(result.Published, ",") != "session-1" {
		t.Fatalf("%#v", result)
	}
	if status, err := local.LoadStatus(); err != nil || !status.LastScanAt.Equal(now) || status.PendingCount != 1 {
		t.Fatalf("status = %#v %v", status, err)
	}
}

// A pass whose context has expired stops before the next session, leaving
// its work pending, instead of failing every remaining session.
func TestExpiredPassContextLeavesSessionsPending(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	if err := local.SaveRegistration(registration(t, writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n"))); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := local.SaveRequest("session-1", "stop", now); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	labels := &mutableLabels{}
	result, err := Run(ctx, local, store, Options{Sources: refuseNativeSources{t}, Parsers: testParsers, Labels: mutableLabelLookup{labels}, MachineID: "m", Now: func() time.Time { return now }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if labels.calls != 0 {
		t.Fatal("canceled collector started label transport")
	}
	if status, err := local.LoadStatus(); err != nil || status.PendingCount != 1 {
		t.Fatalf("status = %#v %v", status, err)
	}
}

// A hook-supplied transcript path naming a FIFO fails its session at once
// instead of blocking the pass on an open that waits for a writer. Not
// parallel: the pass must end within ten seconds, which a busy parallel run
// could exceed without any FIFO blocking it.
func TestFIFOTranscriptFailsWithoutBlockingThePass(t *testing.T) {
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	dir := t.TempDir()
	if err := local.SaveRegistration(registration(t, writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"))); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := registration(t, fifo)
	blocked.ArchiveSessionID, blocked.NativeSessionID = "session-0", "native-0"
	if err := local.SaveRegistration(blocked); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }})
		done <- outcome{result, err}
	}()
	select {
	case got := <-done:
		if got.err != nil || !errors.Is(got.result.Errors["session-0"], errNotRegularFile) || strings.Join(got.result.Published, ",") != "session-1" {
			t.Fatalf("%#v %v", got.result, got.err)
		}
	case <-time.After(10 * time.Second):
		// Unblock the stuck open so the goroutine can end.
		if f, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
		t.Fatal("pass blocked opening a FIFO transcript")
	}
}

func TestOpenRegularFileRefusesNonRegularPaths(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, fifo, "/dev/null"} {
		if f, err := openRegularFile(path); !errors.Is(err, errNotRegularFile) {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("%s: err = %v", path, err)
		}
	}
	regular := writeTranscript(t, dir, "t.jsonl", codexTranscript)
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	f, err := openRegularFile(link)
	if err != nil {
		t.Fatalf("symlink to a regular file: %v", err)
	}
	_ = f.Close()
}

// A pass clears the atomic-write temporaries a crashed writer left, and only
// those: one young enough to belong to a write in progress stays.
func TestRunRemovesStaleWriteTemporaries(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	stale := filepath.Join(local.Home(), "requests", ".pending-stale")
	fresh := filepath.Join(local.Home(), "published", ".pending-fresh")
	nested := filepath.Join(local.Home(), "sessions", "session-1", ".pending-stale")
	for _, path := range []string{stale, fresh, nested} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, path := range []string{stale, nested} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Run(context.Background(), local, storagetest.NewMemoryStore(), Options{Sources: testSources, Parsers: testParsers, MachineID: "m"})
	if !errors.Is(err, state.ErrDurableStorageRecovery) || !errors.Is(result.Errors["durable-storage"], state.ErrDurableStorageRecovery) || len(result.Published) != 0 {
		t.Fatalf("young anonymous published remainder not retained as owed: %+v %v", result, err)
	}
	// Global census refuses before hygiene: none of these originals are lost.
	for _, path := range []string{stale, fresh, nested} {
		if raw, err := os.ReadFile(path); err != nil || string(raw) != "partial" {
			t.Fatalf("owed temporary changed: %s %v", path, err)
		}
	}
	// Resolve only the injected young obstruction, then exercise actual hygiene.
	if err := os.Remove(fresh); err != nil {
		t.Fatal(err)
	}
	if result, err := Run(t.Context(), local, storagetest.NewMemoryStore(), Options{Sources: testSources, Parsers: testParsers, MachineID: "m"}); err != nil || len(result.Errors) != 0 {
		t.Fatal(result, err)
	}
	for _, path := range []string{stale, nested} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale temporary %s survived: %v", path, err)
		}
	}

}

// A publication whose upload keeps failing is retried every pass, and its
// pending file holds the whole compressed source and bundle: marking it
// attempted is written once, not again on every retry.
func TestRetriedPublicationDoesNotRewriteItsPendingFile(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at }, Retry: storage.RetryPolicy{MaxAttempts: 1}}
	pendingFile := filepath.Join(local.Home(), "pending", "session-1.json")
	var written os.FileInfo
	for pass := range 3 {
		result, err := Run(context.Background(), local, remote, opts)
		if err != nil || len(result.Errors) != 1 {
			t.Fatalf("pass %d: result=%#v err=%v", pass, result, err)
		}
		pending, found, err := local.LoadPending("session-1")
		if err != nil || !found || !pending.Attempted {
			t.Fatalf("pass %d: pending attempted=%t found=%t err=%v", pass, pending.Attempted, found, err)
		}
		info, err := os.Stat(pendingFile)
		if err != nil {
			t.Fatal(err)
		}
		// Every save renames a new file into place.
		if written != nil && !os.SameFile(written, info) {
			t.Fatalf("pass %d rewrote the pending publication", pass)
		}
		written = info
		at = at.Add(time.Minute)
	}
	remote.failMetadata = false
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Published) != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestPendingRecoveryStillPublishesAlreadyAdmittedRequest(t *testing.T) {
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest("session-1", "stop", now); err != nil {
		t.Fatal(err)
	}
	if err := local.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	complete, err := local.RecoverSessionIndexScheduled(context.Background(), time.Nanosecond)
	if err != nil || complete {
		t.Fatalf("bounded local stage: complete=%v err=%v", complete, err)
	}
	result, err := Run(context.Background(), local, store, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }, SkipSessionIndexRecovery: true})
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("queued publication under pending recovery: %#v %v", result, err)
	}
	var marker struct {
		Complete bool `json:"complete"`
	}
	data, err := os.ReadFile(filepath.Join(local.Home(), "session-index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &marker); err != nil || marker.Complete {
		t.Fatalf("publication certified incomplete recovery: %#v %v", marker, err)
	}
}

// The collector must keep an IO error joined to its cancelled recovery context.
func TestCollectorRetainsRecoveryCheckpointFailure(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(strconv.FormatBool(broken), func(t *testing.T) {
			local := newTestStore(t)
			path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript+"\n")
			reg := registration(t, path)
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(reg)
			if err != nil {
				t.Fatal(err)
			}
			regPath := filepath.Join(local.Home(), "registrations", reg.ArchiveSessionID+".json")
			if err := os.Remove(regPath); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(regPath, 0600); err != nil {
				t.Fatal(err)
			}
			if broken {
				if err := os.Mkdir(filepath.Join(local.Home(), "session-index-recovery.json"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				var file *os.File
				var err error
				deadline := time.Now().Add(15 * time.Second)
				for time.Now().Before(deadline) {
					file, err = os.OpenFile(regPath, os.O_WRONLY|syscall.O_NONBLOCK, 0)
					if err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if err != nil {
					finished <- err
					return
				}
				// A successful writer open proves recovery reached the census
				// read. Cancel at that boundary instead of racing a short timeout
				// against marker staging and filesystem scheduling.
				cancel()
				replacement := regPath + ".replacement"
				err = os.WriteFile(replacement, data, 0600)
				if err == nil {
					err = os.Rename(replacement, regPath)
				}
				if err == nil {
					_, err = file.Write(data)
				}
				closeErr := file.Close()
				if err == nil {
					err = closeErr
				}
				finished <- err
			}()
			result, err := Run(ctx, local, storagetest.NewMemoryStore(), Options{Sources: testSources, Parsers: testParsers, MachineID: "m"})
			if writeErr := <-finished; writeErr != nil {
				t.Fatal(writeErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			status, err := local.LoadStatus()
			if err != nil {
				t.Fatal(err)
			}
			if broken {
				if !errors.Is(result.Errors["session-index"], context.Canceled) || !strings.Contains(result.Errors["session-index"].Error(), "session-index-recovery.json") || status.LastError != FailedSessionsProblem(1) {
					t.Fatalf("checkpoint error lost: %#v %q", result.Errors, status.LastErrors)
				}
			} else if len(result.Errors) != 0 || len(status.LastErrors) != 0 {
				t.Fatalf("ordinary pending warned: %#v %q", result.Errors, status.LastErrors)
			}
		})
	}
}
