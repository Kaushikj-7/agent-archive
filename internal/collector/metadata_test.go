package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type countedPublications struct {
	*storagetest.MemoryStore
	keys []string
}

func (s *countedPublications) Put(ctx context.Context, key string, data []byte) error {
	s.keys = append(s.keys, key)
	return s.MemoryStore.Put(ctx, key, data)
}

func TestParserUpgradeReusesSourceAfterNativeLogDisappears(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &countedPublications{MemoryStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", ParserVersion: "one", Now: func() time.Time { return now }}
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	old := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	source, _ := remote.Get(context.Background(), old.SourceBundle.Key)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	opts.ParserVersion = "two"
	remote.keys = nil
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	next := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if next.SourceBundle != old.SourceBundle || !next.CapturedAt.Equal(old.CapturedAt) || next.Parser.Version != "two" || !next.MetadataDerivedAt.Equal(now) {
		t.Fatalf("changed durable source or wrong summary: %+v", next)
	}
	key, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	if len(remote.keys) != 2 || remote.keys[0] != key {
		t.Fatalf("metadata-only upgrade wrote %v", remote.keys)
	}
	after, _ := remote.Get(context.Background(), old.SourceBundle.Key)
	if !bytes.Equal(source, after) {
		t.Fatal("source bytes changed")
	}
	// Restore identical native input: further scans do not write anything.
	writeTranscript(t, filepath.Dir(path), "session.jsonl", codexTranscript)
	remote.keys = nil
	now = now.Add(time.Hour)
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(remote.keys) != 0 {
		t.Fatalf("%#v %v %v", result, err, remote.keys)
	}
}

func TestParserMetadataRetryUsesSavedBytes(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", ParserVersion: "one", SkillEvidence: config.SkillEvidenceMetadata, Now: func() time.Time { return now }, Retry: storage.RetryPolicy{MaxAttempts: 1}}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	old := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	now = now.Add(time.Hour)
	opts.ParserVersion = "two"
	remote.failMetadata = true
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	pending, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("%v %v", found, err)
	}
	if pending.SkillEvidence != string(config.SkillEvidenceMetadata) || !pending.MetadataOnly {
		t.Fatalf("parser refresh lost skill policy: %+v", pending)
	}
	remote.failMetadata = false
	now = now.Add(24 * time.Hour)
	restarted, err := openTestStore(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	result, err = Run(context.Background(), restarted, remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	actual, err := remote.Get(context.Background(), pending.MetadataKey)
	if err != nil || !bytes.Equal(actual, pending.MetadataBytes) {
		t.Fatal("retry changed pending metadata")
	}
	next := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if next.SourceBundle != old.SourceBundle {
		t.Fatal("parser retry changed source")
	}
}

func TestMetadataUpgradePreservesNewerDeclinedCandidate(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	now := reg.RegisteredAt.Add(time.Hour)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", ParserVersion: "one", Now: func() time.Time { return now }}
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	bundle, at, _, err := local.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	richer := bundle
	richer.NativeRecords = append(richer.NativeRecords, map[string]any{"type": "event_msg", "message": "retained candidate"})
	richer.Capture.CapturedAt = now.Add(time.Minute)
	if err := statetest.SavePublished(local, reg.ArchiveSessionID, richer, at, state.CacheStatusDeclined); err != nil {
		t.Fatal(err)
	}
	opts.ParserVersion = "two"
	now = now.Add(time.Hour)
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	candidate, _, status, _, err := local.LoadPublished(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if status != state.CacheStatusDeclined || !candidate.Capture.CapturedAt.Equal(richer.Capture.CapturedAt) || len(candidate.NativeRecords) != len(richer.NativeRecords) {
		t.Fatal("metadata-only update discarded local evidence")
	}
	actual, _, _, err := local.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || len(actual.NativeRecords) != len(bundle.NativeRecords) {
		t.Fatal("publication ledger points to unpublished candidate")
	}
}

type countedGets struct {
	*storagetest.MemoryStore
	gets int
}

func (s *countedGets) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.MemoryStore.Get(ctx, key)
}

func (s *countedGets) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	s.gets++
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

func (s *countedGets) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	s.gets++
	return s.MemoryStore.GetVersioned(ctx, key)
}

const grownCodexTranscript = codexTranscript + "\n" + `{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"more"}}`

// publishOnce runs a first scan with parser "one" and returns what it published.
func publishOnce(t *testing.T, local *state.Store, remote storage.ObjectStore, reg archive.SessionRegistration, opts *Options) archive.Metadata {
	t.Helper()
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, remote, *opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	return fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
}

func putMetadata(t *testing.T, remote storage.ObjectStore, reg archive.SessionRegistration, metadata archive.Metadata) {
	t.Helper()
	key, err := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(context.Background(), key, encoded); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyMetadataMigrationFailureRetainsPendingWithoutReplacementAuthority(t *testing.T) {
	t.Parallel()
	cases := map[string]func(t *testing.T, remote *storagetest.MemoryStore, reg archive.SessionRegistration, published archive.Metadata){
		"missing remote metadata": func(t *testing.T, remote *storagetest.MemoryStore, reg archive.SessionRegistration, published archive.Metadata) {
			t.Helper()
			key, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
			if err := remote.Delete(context.Background(), key); err != nil {
				t.Fatal(err)
			}
		},
		"metadata from another machine": func(t *testing.T, remote *storagetest.MemoryStore, reg archive.SessionRegistration, published archive.Metadata) {
			t.Helper()
			published.MachineID = "elsewhere"
			putMetadata(t, remote, reg, published)
		},
		"metadata for a different source": func(t *testing.T, remote *storagetest.MemoryStore, reg archive.SessionRegistration, published archive.Metadata) {
			t.Helper()
			published.SourceBundle.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
			putMetadata(t, remote, reg, published)
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			local := newTestStore(t)
			dir := t.TempDir()
			reg := registration(t, writeTranscript(t, dir, "s.jsonl", codexTranscript))
			remote := storagetest.NewMemoryStore()
			now := reg.RegisteredAt.Add(time.Hour)
			opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", ParserVersion: "one", Now: func() time.Time { return now }}
			before := publishOnce(t, local, remote, reg, &opts)
			// Missing local exact bytes cannot authorize replacement from a guessed body.
			editPublishedState(t, local, func(raw map[string]any) { delete(raw, "metadata_bytes") })
			corrupt(t, remote, reg, before)

			writeTranscript(t, dir, "s.jsonl", grownCodexTranscript)
			now = now.Add(time.Hour)
			opts.ParserVersion = "two"
			key, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
			winner, winnerErr := remote.Get(t.Context(), key)
			result, err := Run(context.Background(), local, remote, opts)
			if err != nil || !errors.Is(result.Errors[reg.ArchiveSessionID], storage.ErrPublicationConflict) || len(result.Published) != 0 {
				t.Fatalf("unsafe legacy replacement: %#v %v", result, err)
			}
			pending, found, err := local.LoadPending(reg.ArchiveSessionID)
			if err != nil || !found || pending.Commit == nil || pending.Commit.Predecessor != state.PredecessorUnknown {
				t.Fatal("new evidence not retained pending", found, err)
			}
			var next archive.Metadata
			if err := json.Unmarshal(pending.MetadataBytes, &next); err != nil {
				t.Fatal(err)
			}
			if next.SourceBundle.SHA256 == before.SourceBundle.SHA256 || next.Parser.Version != "two" {
				t.Fatal("new filtered evidence lost")
			}
			after, afterErr := remote.Get(t.Context(), key)
			if string(winner) != string(after) || !errors.Is(afterErr, winnerErr) {
				t.Fatal("unknown or foreign winner overwritten", afterErr)
			}

		})
	}
}

func TestLegacyFailedParseFetchesWithoutDurableAuthorityOrRebuilding(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	remote := &countedGets{MemoryStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", ParserVersion: "one", Now: func() time.Time { return now }}
	published := publishOnce(t, local, remote, reg, &opts)
	editPublishedState(t, local, func(raw map[string]any) { delete(raw, "metadata_bytes") })
	// State written before metadata was cached also predates scan signatures;
	// without this the session would be skipped as unchanged, which a real
	// legacy install never is.
	if err := local.RemoveScanSignature(reg.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	published.Parser.Status = archive.ParserStatusFailed
	putMetadata(t, remote, reg, published)

	// This legacy fixture predates the leading summary. Normalize that existing
	// non-authority cache migration before checking read-only metadata retry.
	if _, _, err := local.ForCollectorPass().LoadPublishedSummary(reg.ArchiveSessionID); err != nil {
		t.Fatal(err)
	}
	prior, err := os.ReadFile(publishedPath(local, reg.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	for scan := 1; scan <= 2; scan++ {
		// Exercise the legacy read path again rather than an unchanged stat skip.
		if err := local.RemoveScanSignature(reg.ArchiveSessionID); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
		remote.gets = 0
		result, err := Run(context.Background(), local, remote, opts)
		if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
			t.Fatalf("scan %d: %#v %v", scan, result, err)
		}
		if remote.gets != 1 {
			t.Fatalf("scan %d performed %d remote reads, want 1", scan, remote.gets)
		}
		cached, err := local.PublishedMetadata(reg.ArchiveSessionID)
		if err != nil || len(cached) != 0 {
			t.Fatalf("scan %d minted durable metadata: %v", scan, err)
		}
		got, err := os.ReadFile(publishedPath(local, reg.ArchiveSessionID))
		if err != nil || !bytes.Equal(got, prior) {
			t.Fatalf("legacy state changed: %v\nbefore=%s\nafter=%s", err, prior, got)
		}
		local, err = state.Open(local.Home())
		if err != nil {
			t.Fatal(err)
		}
		published, err := local.LoadPublishedState(reg.ArchiveSessionID)
		if err != nil || published.PublicationPredecessor().State != state.PredecessorUnknown {
			t.Fatalf("unknown predecessor lost: %v", err)
		}

	}
}

func TestParserUpgradeWithNewContentPublishesOnce(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	dir := t.TempDir()
	reg := registration(t, writeTranscript(t, dir, "s.jsonl", codexTranscript))
	remote := &countedPublications{MemoryStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", ParserVersion: "one", MinUploadInterval: 3 * time.Minute, Now: func() time.Time { return now }}
	before := publishOnce(t, local, remote, reg, &opts)

	writeTranscript(t, dir, "s.jsonl", grownCodexTranscript)
	now = now.Add(10 * time.Minute)
	opts.ParserVersion = "two"
	remote.keys = nil
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	metadataKey, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	metadataWrites := 0
	for _, key := range remote.keys {
		if key == metadataKey {
			metadataWrites++
		}
	}
	if metadataWrites != 1 || len(remote.keys) != 3 {
		t.Fatalf("parser upgrade with new content wrote %v, want one source, canonical metadata, and v3 hint", remote.keys)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if remote.keys[0] != after.SourceBundle.Key || remote.keys[1] != metadataKey || !strings.HasPrefix(remote.keys[2], listingindex.V3Prefix) {
		t.Fatalf("publication order = %v, want source, canonical metadata, then v3 hint", remote.keys)
	}
	revision, err := listingindex.ParseRevision(remote.keys[2])
	if err != nil || revision.MetadataKey != metadataKey {
		t.Fatalf("listing revision = %+v, err = %v", revision, err)
	}
	raw, err := remote.Get(context.Background(), metadataKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := revision.ValidateMetadata(raw); err != nil {
		t.Fatalf("listing hint does not match published metadata: %v", err)
	}
	if after.SourceBundle.SHA256 == before.SourceBundle.SHA256 || after.Parser.Version != "two" {
		t.Fatalf("content publication missing or stale parser: %+v", after)
	}
	if _, found, err := local.LoadPending(reg.ArchiveSessionID); err != nil || found {
		t.Fatalf("content publication was rate-limited behind a metadata-only one: pending=%v err=%v", found, err)
	}
	if _, _, status, _, err := local.LoadPublished(reg.ArchiveSessionID); err != nil || status != state.CacheStatusPublished {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

// After a parser upgrade the refresh reads the live transcript to see
// whether normal capture will publish; when it will, capture uses that
// read rather than filtering the whole transcript again. Not parallel: it
// reads a package-wide counter.
func TestParserUpgradeWithNewContentReadsTheTranscriptOnce(t *testing.T) {
	local := newTestStore(t)
	dir := t.TempDir()
	reg := registration(t, writeTranscript(t, dir, "s.jsonl", codexTranscript))
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", ParserVersion: "one", Now: func() time.Time { return now }}
	published := publishOnce(t, local, remote, reg, &opts)

	writeTranscript(t, dir, "s.jsonl", grownCodexTranscript)
	now = now.Add(10 * time.Minute)
	opts.ParserVersion = "two"
	before := transcriptFilters.Load()
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	if filters := transcriptFilters.Load() - before; filters != 1 {
		t.Fatalf("the scan filtered the transcript %d times", filters)
	}
	if after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); after.Parser.Version != "two" || after.SourceBundle == published.SourceBundle {
		t.Fatalf("metadata = %+v", after)
	}
}

// A parser upgrade re-derives every session's metadata, but a transcript
// still at the state its last scan settled at, under the same filter, has
// nothing new to say: the refresh publishes metadata over the retained
// source without reading it. Not parallel: it reads a package-wide counter.
func TestParserUpgradeOverAnUnchangedTranscriptDoesNotReadIt(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	remote := storagetest.NewMemoryStore()
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", ParserVersion: "one", Now: func() time.Time { return now }}
	published := publishOnce(t, local, remote, reg, &opts)

	now = now.Add(10 * time.Minute)
	opts.ParserVersion = "two"
	before := transcriptFilters.Load()
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	if filters := transcriptFilters.Load() - before; filters != 0 {
		t.Fatalf("the refresh filtered the unchanged transcript %d times", filters)
	}
	if after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID); after.Parser.Version != "two" || after.SourceBundle != published.SourceBundle {
		t.Fatalf("metadata = %+v, want parser two over the same source", after)
	}
}

func TestBlockedSessionRegeneratesFromLastPublicationOnly(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	dir := t.TempDir()
	reg := registration(t, writeTranscript(t, dir, "s.jsonl", codexTranscript))
	remote := &countedPublications{MemoryStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", ParserVersion: "one", Now: func() time.Time { return now }}
	before := publishOnce(t, local, remote, reg, &opts)

	// A rewrite blocks the session; the blocked candidate is cached beside
	// the real publication.
	writeTranscript(t, dir, "s.jsonl", `{"type":"turn_context","model":"gpt-test"}`)
	now = now.Add(10 * time.Minute)
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if _, blocked, err := local.LoadBlocked(reg.ArchiveSessionID); err != nil || !blocked {
		t.Fatalf("blocked=%v err=%v", blocked, err)
	}
	blockedCandidate, _, _, _, err := local.LoadPublished(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(10 * time.Minute)
	opts.ParserVersion = "two"
	remote.keys = nil
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	metadataKey, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	if len(remote.keys) != 2 || remote.keys[0] != metadataKey {
		t.Fatalf("blocked session upgrade wrote %v", remote.keys)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.SourceBundle != before.SourceBundle || after.Parser.Version != "two" {
		t.Fatalf("regeneration did not use the real last publication: %+v", after)
	}
	if len(fetchBundle(t, remote, after).NativeRecords) != len(fetchBundle(t, remote, before).NativeRecords) {
		t.Fatal("regeneration published the blocked candidate")
	}
	reason, blocked, err := local.LoadBlocked(reg.ArchiveSessionID)
	if err != nil || !blocked || reason != state.BlockedReasonTranscriptRewritten {
		t.Fatalf("metadata-only publish cleared the block: blocked=%v reason=%q err=%v", blocked, reason, err)
	}
	cached, _, _, _, err := local.LoadPublished(reg.ArchiveSessionID)
	if err != nil || len(cached.NativeRecords) != len(blockedCandidate.NativeRecords) {
		t.Fatalf("metadata-only publish replaced the blocked comparison bundle: %v", err)
	}
}

func TestBlockedSessionWithoutPublicationSkipsRegeneration(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &countedGets{MemoryStore: storagetest.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "machine", ParserVersion: "one", MaxTranscriptBytes: 8, Now: func() time.Time { return now }}
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if reason, blocked, err := local.LoadBlocked(reg.ArchiveSessionID); err != nil || !blocked || reason != state.BlockedReasonTranscriptTooLarge {
		t.Fatalf("blocked=%v reason=%q err=%v", blocked, reason, err)
	}
	if _, _, found, err := local.LoadLastPublished(reg.ArchiveSessionID); err != nil || found {
		t.Fatalf("unexpected publication: found=%v err=%v", found, err)
	}

	now = now.Add(time.Hour)
	opts.ParserVersion = "two"
	remote.gets = 0
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 || remote.gets != 0 {
		t.Fatalf("blocked session without a publication was not skipped cleanly: %#v %v gets=%d", result, err, remote.gets)
	}
}

func (s *countedGets) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	s.gets++
	return s.MemoryStore.GetLimitedVersioned(ctx, key, limit)
}
