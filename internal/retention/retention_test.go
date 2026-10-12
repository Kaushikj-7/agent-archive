package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
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

func registration(id, transcriptPath string) archive.SessionRegistration {
	return archive.SessionRegistration{
		ArchiveSessionID: id, NativeSessionID: "native-" + id, ProjectID: "project-1", ProjectRoot: "/p",
		Harness: archive.Harness{Name: "codex"}, TranscriptPath: transcriptPath,
		SessionStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		RegisteredAt:     time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
	}
}

func newTestStore(t *testing.T) *state.Store {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(home, config.Config{MachineID: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// publishTwice registers a session, publishes it, then republishes changed
// content well past the collector's rate limit, so the first snapshot
// becomes superseded. It returns the first (now superseded) source key.
func publishTwice(t *testing.T, local *state.Store, store storage.ObjectStore, id, dir string, t0 time.Time) (firstKey string) {
	t.Helper()
	path := writeTranscript(t, dir, id+".jsonl", codexTranscript)
	reg := registration(id, path)
	reg.ArchiveSessionID = id
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	bindings := builtin.NewBuiltins()
	if _, err := collector.Run(context.Background(), local, store, collector.Options{Sources: bindings, Parsers: bindings, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	firstMeta := fetchMetadata(t, store, id)

	writeTranscript(t, dir, id+".jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	t1 := t0.Add(10 * time.Minute)
	bindings = builtin.NewBuiltins()
	if _, err := collector.Run(context.Background(), local, store, collector.Options{Sources: bindings, Parsers: bindings, MachineID: "m", Now: func() time.Time { return t1 }}); err != nil {
		t.Fatal(err)
	}
	return firstMeta.SourceBundle.Key
}

func fetchMetadata(t *testing.T, store storage.ObjectStore, sessionID string) archive.Metadata {
	t.Helper()
	key, err := archive.MetadataObjectKey("codex", sessionID)
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

func TestSweepRespectsGracePeriodBeforeDeletingSupersededSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	firstKey := publishTwice(t, local, store, "s1", dir, t0)
	supersededAt := t0.Add(10 * time.Minute)

	// Sweep soon after supersession: still within the grace period.
	soon := supersededAt.Add(time.Hour)
	result, err := Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return soon }, GracePeriod: 24 * time.Hour}))
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedSnapshots != 0 {
		t.Fatalf("result=%#v", result)
	}
	if _, err := store.Get(context.Background(), firstKey); err != nil {
		t.Fatalf("superseded source must survive within the grace period: %v", err)
	}

	// The predecessor survives indefinitely, even after its grace period.
	later := supersededAt.Add(25 * time.Hour)
	result, err = Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return later }, GracePeriod: 24 * time.Hour}))
	if err != nil || len(result.Errors) != 0 || result.DeletedSnapshots != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := store.Get(context.Background(), firstKey); err != nil {
		t.Fatal(err)
	}
	// A third publication makes the first source eligible, but retains second.
	secondKey := fetchMetadata(t, store, "s1").SourceBundle.Key
	publishThird(t, local, store, "s1", dir, later)
	result, err = Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return later.Add(25 * time.Hour) }}))
	if err != nil || len(result.Errors) != 0 || result.DeletedSnapshots != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := store.Get(context.Background(), firstKey); err == nil {
		t.Fatal("older source survives")
	}
	if _, err := store.Get(context.Background(), secondKey); err != nil {
		t.Fatal("predecessor deleted", err)
	}

}

func TestSweepNeverDeletesTheCurrentSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", dir, t0)
	currentMeta := fetchMetadata(t, store, "s1")

	far := t0.Add(365 * 24 * time.Hour)
	if _, err := Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return far }, GracePeriod: time.Hour})); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), currentMeta.SourceBundle.Key); err != nil {
		t.Fatalf("the current source must never be deleted: %v", err)
	}
}

func TestSweepDeletesWholeSessionPastRetentionWindow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	path := writeTranscript(t, dir, "s1.jsonl", codexTranscript)
	if err := local.SaveRegistration(registration("s1", path)); err != nil {
		t.Fatal(err)
	}
	bindings := builtin.NewBuiltins()
	if _, err := collector.Run(context.Background(), local, store, collector.Options{Sources: bindings, Parsers: bindings, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	meta := fetchMetadata(t, store, "s1")

	past := t0.Add(91 * 24 * time.Hour)
	forever, err := Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return past }, SessionMaxAge: 0}))
	if err != nil || len(forever.DeletedSessions) != 0 || len(forever.PrunedSessions) != 0 {
		t.Fatalf("forever expired evidence: %+v %v", forever, err)
	}
	regsForever, err := local.LoadRegistrations()
	if err != nil || len(regsForever) != 1 {
		t.Fatalf("forever lost ownership: %v %v", regsForever, err)
	}
	if _, err := store.Get(context.Background(), meta.SourceBundle.Key); err != nil {
		t.Fatal("forever lost active source", err)
	}
	result, err := Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return past }, SessionMaxAge: 90 * 24 * time.Hour}))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.DeletedSessions) != 1 || result.DeletedSessions[0] != "s1" {
		t.Fatalf("result=%#v", result)
	}
	if _, err := store.Get(context.Background(), meta.SourceBundle.Key); err == nil {
		t.Fatal("expected the source to be deleted")
	}
	metaKey, _ := archive.MetadataObjectKey("codex", "s1")
	if _, err := store.Get(context.Background(), metaKey); err == nil {
		t.Fatal("expected the metadata to be deleted")
	}
	regs, err := local.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("expected local state forgotten: regs=%#v err=%v", regs, err)
	}
}

func TestSweepWithinRetentionWindowLeavesSessionAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	path := writeTranscript(t, dir, "s1.jsonl", codexTranscript)
	if err := local.SaveRegistration(registration("s1", path)); err != nil {
		t.Fatal(err)
	}
	bindings := builtin.NewBuiltins()
	if _, err := collector.Run(context.Background(), local, store, collector.Options{Sources: bindings, Parsers: bindings, MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}

	soon := t0.Add(time.Hour)
	result, err := Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return soon }, SessionMaxAge: 90 * 24 * time.Hour}))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.DeletedSessions) != 0 {
		t.Fatalf("result=%#v", result)
	}
	regs, err := local.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("regs=%#v err=%v", regs, err)
	}
}

// failingDeleteStore fails Delete for one specific key, to test that one
// session's failure does not block another's cleanup.
type failingDeleteStore struct {
	storage.ObjectStore
	failKey string
}

func (f failingDeleteStore) Delete(ctx context.Context, key string) error {
	if key == f.failKey {
		return errors.New("simulated delete failure")
	}
	return f.ObjectStore.Delete(ctx, key)
}

func TestSweepIsolatesOneSessionsFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	local := newTestStore(t)
	memStore := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	firstKeyA := publishTwice(t, local, memStore, "a", dir, t0)
	_ = publishTwice(t, local, memStore, "b", dir, t0)

	publishThird(t, local, memStore, "a", dir, t0.Add(time.Hour))
	publishThird(t, local, memStore, "b", dir, t0.Add(time.Hour))
	store := failingDeleteStore{ObjectStore: memStore, failKey: firstKeyA}
	later := t0.Add(10*time.Minute + 25*time.Hour)
	result, err := Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return later }, GracePeriod: 24 * time.Hour}))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Errors["a"]; !ok {
		t.Fatalf("expected session a to report an error: %#v", result)
	}
	if result.DeletedSnapshots != 1 {
		t.Fatalf("session b should still have been cleaned up: %#v", result)
	}
}

func publishThird(t *testing.T, local *state.Store, store storage.ObjectStore, id, dir string, at time.Time) {
	t.Helper()
	path := filepath.Join(dir, id+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n"+`{"type":"response_item","id":"m3","payload":{"type":"message","role":"user","content":"third"}}`)...), 0600); err != nil {
		t.Fatal(err)
	}
	bindings := builtin.NewBuiltins()
	result, err := collector.Run(context.Background(), local, store, collector.Options{Sources: bindings, Parsers: bindings, MachineID: "m", Now: func() time.Time { return at }})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
}

func TestSweepFailsClosedWithUnreadableCurrentMetadata(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	at := time.Now()
	dir := t.TempDir()
	first := publishTwice(t, local, store, "s1", dir, at)
	second := fetchMetadata(t, store, "s1").SourceBundle.Key
	// Two superseded snapshots past their grace period, so the sweep would
	// delete the older one if it could trust the current pointer.
	publishThird(t, local, store, "s1", dir, at.Add(time.Hour))
	key, _ := archive.MetadataObjectKey("codex", "s1")
	if err := store.Put(context.Background(), key, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	result, err := Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return at.Add(48 * time.Hour) }}))
	if err != nil || result.Errors["s1"] == nil || result.DeletedSnapshots != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	for _, k := range []string{first, second} {
		if _, err := store.Get(context.Background(), k); err != nil {
			t.Fatal(k, err)
		}
	}
}

func TestWholeSessionDeletionFailureNeverLeavesDanglingPointer(t *testing.T) {
	t.Parallel()
	for _, failMetadata := range []bool{true, false} {
		t.Run(strconv.FormatBool(failMetadata), func(t *testing.T) {
			t.Parallel()
			local := newTestStore(t)
			mem := storagetest.NewMemoryStore()
			at := time.Now()
			publishTwice(t, local, mem, "s1", t.TempDir(), at)
			meta := fetchMetadata(t, mem, "s1")
			key, _ := archive.MetadataObjectKey("codex", "s1")
			failKey := meta.SourceBundle.Key
			if failMetadata {
				failKey = key
			}
			store := failingDeleteStore{ObjectStore: mem, failKey: failKey}
			opts := Options{Now: func() time.Time { return at.Add(100 * 24 * time.Hour) }, SessionMaxAge: 90 * 24 * time.Hour}
			result, err := Sweep(context.Background(), local, store, agreeing(opts))
			if err != nil || result.Errors["s1"] == nil {
				t.Fatalf("%#v %v", result, err)
			}
			if _, err := mem.Get(context.Background(), key); err == nil {
				if _, err := mem.Get(context.Background(), meta.SourceBundle.Key); err != nil {
					t.Fatal("dangling pointer")
				}
			} else if failMetadata {
				t.Fatal("metadata deleted despite failure")
			}
			result, err = Sweep(context.Background(), local, mem, agreeing(opts))
			if err != nil || len(result.Errors) != 0 || len(result.DeletedSessions) != 1 {
				t.Fatalf("retry: %#v %v", result, err)
			}
			objects, err := mem.List(context.Background(), "sessions/codex/s1/")
			if err != nil || len(objects) != 0 {
				t.Fatalf("%#v %v", objects, err)
			}
		})
	}
}

func TestRetentionProtectsNewerRemoteCaptureAndClockRollbackPredecessor(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	at := time.Now().UTC()
	dir := t.TempDir()
	first := publishTwice(t, local, remote, "s1", dir, at)
	second := fetchMetadata(t, remote, "s1").SourceBundle.Key
	publishThird(t, local, remote, "s1", dir, at.Add(time.Hour))
	ledger, err := local.LoadSuperseded("s1")
	if err != nil {
		t.Fatal(err)
	}
	// The second supersession happened after clock correction, but is newer in order.
	ledger[0].SupersededAt = at.Add(2 * time.Hour)
	ledger[1].SupersededAt = at
	for _, entry := range ledger {
		if err := local.RemoveSuperseded("s1", entry.Key); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range ledger {
		if err := local.RecordSuperseded("s1", entry.Key, entry.SupersededAt); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Sweep(context.Background(), local, remote, agreeing(Options{Now: func() time.Time { return at.Add(48 * time.Hour) }}))
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if _, err := remote.Get(context.Background(), first); err == nil {
		t.Fatal("older snapshot retained")
	}
	if _, err := remote.Get(context.Background(), second); err != nil {
		t.Fatal("immediate predecessor deleted", err)
	}
	// Simulate remote success with stale local acknowledgement before expiry.
	metadata := fetchMetadata(t, remote, "s1")
	metadata.CapturedAt = at.Add(95 * 24 * time.Hour)
	key, _ := archive.MetadataObjectKey("codex", "s1")
	data, _ := json.Marshal(metadata)
	if err := remote.Put(context.Background(), key, data); err != nil {
		t.Fatal(err)
	}
	result, err = Sweep(context.Background(), local, remote, agreeing(Options{Now: func() time.Time { return at.Add(100 * 24 * time.Hour) }, SessionMaxAge: 90 * 24 * time.Hour}))
	if err != nil || len(result.Errors) != 0 || len(result.DeletedSessions) != 0 {
		t.Fatalf("deleted newer remote evidence: %#v %v", result, err)
	}
	if _, err := remote.Get(context.Background(), key); err != nil {
		t.Fatal(err)
	}
}

// pointCurrentAt rewrites a session's remote metadata so it references key,
// simulating a publication that made key the current source.
func pointCurrentAt(t *testing.T, store storage.ObjectStore, id, key string) {
	t.Helper()
	metadata := fetchMetadata(t, store, id)
	sha := strings.TrimSuffix(strings.TrimPrefix(key, fmt.Sprintf("sessions/codex/%s/source.", id)), ".jsonl.gz")
	if len(sha) != 64 {
		t.Fatalf("unexpected source key %q", key)
	}
	if _, err := store.Get(context.Background(), key); errors.Is(err, storage.ErrNotFound) {
		if err := store.Put(context.Background(), key, []byte("snapshot")); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	metadata.SourceBundle = archive.SourceReference{Key: key, SHA256: sha, CompressedBytes: 8}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey, _ := archive.MetadataObjectKey("codex", id)
	if err := store.Put(context.Background(), metadataKey, data); err != nil {
		t.Fatal(err)
	}
}

// A -> B -> A -> C: A is superseded twice. The immediate predecessor of C is
// A, so the sweep must retain A and expire B, not the reverse.
func TestSweepKeepsTruePredecessorAfterContentReversion(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	a := publishTwice(t, local, store, "s1", t.TempDir(), t0) // ledger [A]; current B
	b := fetchMetadata(t, store, "s1").SourceBundle.Key
	c := fmt.Sprintf("sessions/codex/s1/source.%s.jsonl.gz", strings.Repeat("c", 64))

	// Content reverts to A, superseding B; then C supersedes A again.
	t2 := t0.Add(time.Hour)
	if err := local.RecordSuperseded("s1", b, t2); err != nil {
		t.Fatal(err)
	}
	pointCurrentAt(t, store, "s1", a)
	t3 := t2.Add(time.Hour)
	if err := local.RecordSuperseded("s1", a, t3); err != nil {
		t.Fatal(err)
	}
	pointCurrentAt(t, store, "s1", c)

	result, err := Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return t3.Add(25 * time.Hour) }, GracePeriod: 24 * time.Hour}))
	if err != nil || len(result.Errors) != 0 || result.DeletedSnapshots != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	if _, err := store.Get(context.Background(), a); err != nil {
		t.Fatal("true predecessor A deleted:", err)
	}
	if _, err := store.Get(context.Background(), b); err == nil {
		t.Fatal("older snapshot B retained")
	}
	if _, err := store.Get(context.Background(), c); err != nil {
		t.Fatal("current C deleted:", err)
	}
	ledger, err := local.LoadSuperseded("s1")
	if err != nil || len(ledger) != 1 || ledger[0].Key != a {
		t.Fatalf("ledger=%#v err=%v", ledger, err)
	}
}

func TestSweepRemovesVerifiedPrivacyPredecessorAfterGrace(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	predecessor := publishTwice(t, local, remote, "s1", t.TempDir(), t0)
	entries, err := local.LoadSuperseded("s1")
	if err != nil || len(entries) != 1 {
		t.Fatalf("ledger: %#v %v", entries, err)
	}
	if err := local.RecordSupersededWithPrivacy("s1", predecessor, entries[0].SupersededAt, true); err != nil {
		t.Fatal(err)
	}
	now := t0.Add(25 * time.Hour)
	result, err := Sweep(context.Background(), local, remote, agreeing(Options{Now: func() time.Time { return now }, PrivacyVerified: func(archive.SessionRegistration, archive.Metadata) bool { return false }}))
	if err != nil || len(result.Errors) != 0 || result.DeletedSnapshots != 0 {
		t.Fatalf("unverified: %#v %v", result, err)
	}
	if _, err := remote.Get(context.Background(), predecessor); err != nil {
		t.Fatalf("unverified source deleted: %v", err)
	}
	result, err = Sweep(context.Background(), local, remote, agreeing(Options{Now: func() time.Time { return now }, PrivacyVerified: func(archive.SessionRegistration, archive.Metadata) bool { return true }}))
	if err != nil || len(result.Errors) != 0 || result.DeletedSnapshots != 1 {
		t.Fatalf("verified: %#v %v", result, err)
	}
	if _, err := remote.Get(context.Background(), predecessor); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("predecessor remained: %v", err)
	}
	current := fetchMetadata(t, remote, "s1").SourceBundle.Key
	if _, err := remote.Get(context.Background(), current); err != nil {
		t.Fatalf("current source removed: %v", err)
	}
}

func TestSweepPrivacyPredecessorDeletionFailureRetries(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	predecessor := publishTwice(t, local, remote, "s1", t.TempDir(), t0)
	if err := local.RecordSupersededWithPrivacy("s1", predecessor, t0.Add(10*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	now := t0.Add(25 * time.Hour)
	opts := agreeing(Options{Now: func() time.Time { return now }, PrivacyVerified: func(archive.SessionRegistration, archive.Metadata) bool { return true }})
	blocked := failingDeleteStore{ObjectStore: remote, failKey: predecessor}
	result, err := Sweep(context.Background(), local, blocked, opts)
	if err != nil || len(result.Errors) == 0 {
		t.Fatalf("delete failure not reported: %#v %v", result, err)
	}
	if _, err := remote.Get(context.Background(), predecessor); err != nil {
		t.Fatalf("failed delete removed source: %v", err)
	}
	result, err = Sweep(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || result.DeletedSnapshots != 1 {
		t.Fatalf("retry: %#v %v", result, err)
	}
}

type replaceMetadataOnSecondRead struct {
	storage.ObjectStore
	key         string
	replacement []byte
	reads       int
}

func (s *replaceMetadataOnSecondRead) Get(ctx context.Context, key string) ([]byte, error) {
	if key == s.key {
		s.reads++
		if s.reads == 2 {
			if err := s.Put(ctx, key, s.replacement); err != nil {
				return nil, err
			}
		}
	}
	return s.ObjectStore.Get(ctx, key)
}

func TestSweepPrivacyPredecessorRechecksChangedMetadata(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	predecessor := publishTwice(t, local, remote, "s1", t.TempDir(), t0)
	if err := local.RecordSupersededWithPrivacy("s1", predecessor, t0.Add(10*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	meta := fetchMetadata(t, remote, "s1")
	meta.SourceBundle.Key = predecessor
	replacement, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey, _ := archive.MetadataObjectKey("codex", "s1")
	changed := &replaceMetadataOnSecondRead{ObjectStore: remote, key: metadataKey, replacement: replacement}
	result, err := Sweep(context.Background(), local, changed, agreeing(Options{Now: func() time.Time { return t0.Add(25 * time.Hour) }, PrivacyVerified: func(archive.SessionRegistration, archive.Metadata) bool { return true }}))
	if err != nil || len(result.Errors) == 0 {
		t.Fatalf("changed metadata not reported: %#v %v", result, err)
	}
	if _, err := remote.Get(context.Background(), predecessor); err != nil {
		t.Fatalf("newly current predecessor was deleted: %v", err)
	}
}

// countingStore counts Get calls, to assert a sweep skipped the remote
// metadata read when it could not have deleted anything.
type countingStore struct {
	storage.ObjectStore
	gets int
}

func (c *countingStore) Get(ctx context.Context, key string) ([]byte, error) {
	c.gets++
	return c.ObjectStore.Get(ctx, key)
}

func TestSweepSkipsRemoteReadWhenNothingIsExpirable(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	mem := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	first := publishTwice(t, local, mem, "s1", dir, t0) // ledger [first]: the predecessor
	supersededAt := t0.Add(10 * time.Minute)
	store := &countingStore{ObjectStore: mem}

	// Within grace, past grace (predecessor only), and within the session
	// retention window: nothing could be deleted, so no remote read.
	for _, opts := range []Options{
		{Now: func() time.Time { return supersededAt.Add(time.Hour) }, GracePeriod: 24 * time.Hour},
		{Now: func() time.Time { return supersededAt.Add(400 * 24 * time.Hour) }, GracePeriod: 24 * time.Hour},
		{Now: func() time.Time { return supersededAt.Add(30 * 24 * time.Hour) }, GracePeriod: 24 * time.Hour, SessionMaxAge: 90 * 24 * time.Hour},
	} {
		result, err := Sweep(context.Background(), local, store, agreeing(opts))
		if err != nil || len(result.Errors) != 0 || result.DeletedSnapshots != 0 || len(result.DeletedSessions) != 0 {
			t.Fatalf("%#v %v", result, err)
		}
	}
	if store.gets != 0 {
		t.Fatalf("idle sweep must not read remote metadata; gets=%d", store.gets)
	}

	// A second superseded snapshot past its grace period is deletable, so
	// the sweep must read the current pointer and act.
	publishThird(t, local, mem, "s1", dir, supersededAt.Add(time.Hour))
	result, err := Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return supersededAt.Add(30 * time.Hour) }, GracePeriod: 24 * time.Hour}))
	if err != nil || len(result.Errors) != 0 || result.DeletedSnapshots != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	if store.gets != 2 {
		t.Fatalf("expected the initial pointer read and immediate deletion recheck; gets=%d", store.gets)
	}
	if _, err := mem.Get(context.Background(), first); err == nil {
		t.Fatal("older snapshot retained")
	}

	// Locally expired session: the remote read is required to confirm.
	store.gets = 0
	result, err = Sweep(context.Background(), local, store, agreeing(Options{Now: func() time.Time { return supersededAt.Add(100 * 24 * time.Hour) }, SessionMaxAge: 90 * 24 * time.Hour}))
	if err != nil || len(result.Errors) != 0 || len(result.DeletedSessions) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	if store.gets < 1 {
		t.Fatalf("expiry must validate the remote pointer; gets=%d", store.gets)
	}
}
