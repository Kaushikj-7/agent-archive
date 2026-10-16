package collector

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestPublicationConflictingWinnerRetainsJournalAndRepairsAuthoritativeListing(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal(result, err)
	}
	pending, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found || pending.Commit == nil {
		t.Fatal(found, err)
	}
	var winner map[string]any
	if err := json.Unmarshal(pending.MetadataBytes, &winner); err != nil {
		t.Fatal(err)
	}
	winner["metadata_derived_at"] = at.Add(time.Hour).Format(time.RFC3339)
	body, err := json.Marshal(winner)
	if err != nil {
		t.Fatal(err)
	}
	remote.failMetadata = false
	if err := remote.MemoryStore.Put(t.Context(), pending.MetadataKey, body); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	result, err = Run(t.Context(), local, remote, opts)
	if err != nil || !errors.Is(result.Errors[reg.ArchiveSessionID], storage.ErrPublicationConflict) || len(result.Published) != 0 {
		t.Fatal(result, err)
	}
	got, err := remote.Get(t.Context(), pending.MetadataKey)
	if err != nil || string(got) != string(body) {
		t.Fatal("stale pending replaced winner", err)
	}
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("uncommitted request acknowledged", found, err)
	}
	repairs, err := local.ListingRepairs(32)
	if err != nil {
		t.Fatal(err)
	}
	// The conflict leaves a fresh repair journal; the previous pass's repair used
	// the authoritative winner. Check its immutable listing identity directly.
	hints, err := remote.List(t.Context(), listingindex.V3Prefix)
	if err != nil {
		t.Fatal(err)
	}
	winnerIndexed := false
	for _, hint := range hints {
		revision, err := listingindex.ParseRevision(hint.Key)
		if err != nil {
			t.Fatal(err)
		}
		if revision.Hash == storage.SHA256Hex(body) {
			winnerIndexed = true
		}
		if revision.Hash == pending.Commit.MetadataSHA256 {
			t.Fatal("obsolete pending body indexed")
		}
	}
	if !winnerIndexed {
		t.Fatal("authoritative listing wasn't repaired", repairs)
	}

}

type localCommitFaultStore struct {
	*storagetest.MemoryStore
	publishedPath string
	fail          bool
}

func (s *localCommitFaultStore) Put(ctx context.Context, key string, body []byte) error {
	if err := s.MemoryStore.Put(ctx, key, body); err != nil {
		return err
	}
	if s.fail && filepath.Base(key) == "metadata.json" {
		if err := os.Mkdir(s.publishedPath, 0700); err != nil {
			return err
		}
		s.fail = false
	}
	return nil
}

func TestPublicationLocalCommitFailureReplaysExactBytesAfterNativeDeletion(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
		t.Fatal(err)
	}
	remote := &localCommitFaultStore{MemoryStore: storagetest.NewMemoryStore(), publishedPath: publishedPath(local, reg.ArchiveSessionID), fail: true}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 {
		t.Fatal(result, err)
	}
	// The injected non-file published obstruction is itself a census refusal.
	// Remove that disposable fault before asking the owner to decode replay.
	if err := os.Remove(remote.publishedPath); err != nil {
		t.Fatal(err)
	}
	pending, found, err := local.LoadPublicationPending(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
		t.Fatal("request completed before local commit", found, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	result, err = Run(t.Context(), reopened, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	published, err := reopened.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := published.CommittedSources()
	if err != nil || len(refs) != 1 || refs[0] != pending.SourceReference() || string(published.Metadata()) != string(pending.MetadataBytes) {
		t.Fatal("full local commit lost frozen bytes", refs, err)
	}
	if _, found, err := reopened.LoadRequest(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("covered request not acknowledged", found, err)
	}
	if found, err := reopened.HasPending(reg.ArchiveSessionID); err != nil || found {
		t.Fatal("committed journal not released", found, err)
	}
}

func TestPublicationLegacyRemoteCacheKeepsUnknownPredecessor(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
	remote := storagetest.NewMemoryStore()
	at := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
	publishOnce(t, local, remote, reg, &opts)
	key := "sessions/codex/session-1/metadata.json"
	old, err := remote.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	editPublishedState(t, local, func(raw map[string]any) { delete(raw, "metadata_bytes") })
	legacy, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil || legacy.PublicationPredecessor().State != state.PredecessorUnknown {
		t.Fatal("legacy fixture lacks unknown predecessor", err)
	}
	stopAtNewCommit(t, local, reg, at.Add(time.Minute))
	at = at.Add(time.Hour)
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || !errors.Is(result.Errors[reg.ArchiveSessionID], storage.ErrPublicationConflict) {
		t.Fatal(result, err)
	}
	got, err := remote.Get(t.Context(), key)
	if err != nil || string(got) != string(old) {
		t.Fatal("legacy remote body guessed as predecessor", err)
	}
	reopened, err := state.Open(local.Home())
	if err != nil {
		t.Fatal(err)
	}
	published, err := reopened.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil || published.PublicationPredecessor().State != state.PredecessorUnknown {
		t.Fatal("legacy authority changed after restart", err)
	}
}

func TestPublicationListingRepairRefusesCorruptAuthoritativeSource(t *testing.T) {
	local := newTestStore(t)
	claudeSession(t, local, claudePromptLine+"\n")
	remote := &failingListingStore{MemoryStore: storagetest.NewMemoryStore(), fail: true, fault: errors.New("synthetic listing interruption")}
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	result := runAt(t, local, remote, at)
	if len(result.Published) != 1 {
		t.Fatal(result)
	}
	m := fetchMetadata(t, remote, "claude", "session-1")
	if err := remote.MemoryStore.Put(t.Context(), m.SourceBundle.Key, []byte("corrupt referenced source")); err != nil {
		t.Fatal(err)
	}
	remote.fail = false
	result = runAt(t, local, remote, at.Add(time.Minute))
	if !errors.Is(result.Errors["listing-maintenance"], storage.ErrChecksumMismatch) {
		t.Fatal("repair accepted corrupt winner", result)
	}
	repairs, err := local.ListingRepairs(32)
	if err != nil || len(repairs) != 1 {
		t.Fatal("repair intent lost", repairs, err)
	}
	hints, err := remote.List(t.Context(), listingindex.V3Prefix)
	if err != nil || len(hints) != 0 {
		t.Fatal("corrupt authoritative source was indexed", hints, err)
	}
}

type publicationContextChange string

const (
	publicationDestinationChange publicationContextChange = "destination"
	publicationAdapterChange     publicationContextChange = "adapter"
	publicationFilterChange      publicationContextChange = "filter"
)

func TestPublicationChangedDestinationOrPrivacyContextRetainsPending(t *testing.T) {
	for _, mode := range []publicationContextChange{publicationDestinationChange, publicationAdapterChange, publicationFilterChange} {
		t.Run(string(mode), func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
			at := reg.RegisteredAt.Add(time.Hour)
			opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
			result, err := Run(t.Context(), local, remote, opts)
			if err != nil || result.Errors[reg.ArchiveSessionID] == nil {
				t.Fatal(result, err)
			}
			pending, found, err := local.LoadPending(reg.ArchiveSessionID)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			switch mode {
			case publicationDestinationChange:
				reg.DestinationID = "different-destination"
			case publicationAdapterChange:
				pending.Bundle.Capture.AdapterVersion = "older-adapter-policy"
			case publicationFilterChange:
				pending.Bundle.Capture.FilterVersion = "older-filter-policy"
			}
			published, err := local.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			remote.failMetadata = false
			scan := newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, at, opts)
			if _, err := scan.publishPending(scan.ctx, pending); err == nil {
				t.Fatal("changed context admitted old bytes")
			}
			if _, err := remote.Get(t.Context(), pending.MetadataKey); !errors.Is(err, storage.ErrNotFound) {
				t.Fatal("changed context became authoritative", err)
			}
			if found, err := local.HasPending(reg.ArchiveSessionID); err != nil || !found {
				t.Fatal("changed context lost evidence", found, err)
			}
		})
	}
}

func TestPublicationMalformedJournalBlocksScanAndRemainsOutstanding(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	at := reg.RegisteredAt.Add(time.Hour)
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json")
	body := []byte(`{"bundle":{"sche`)
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	remote := &putCountingStore{MemoryStore: storagetest.NewMemoryStore()}
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
	for range 2 {
		result, err := Run(t.Context(), local, remote, opts)
		if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 || remote.takePuts() != 0 {
			t.Fatal("malformed journal became absent work", result, err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != string(body) {
			t.Fatal("journal evidence changed", err)
		}
		if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
			t.Fatal("request completed prematurely", found, err)
		}
		work, err := local.Outstanding(reg, true)
		if err != nil || !work.Upload || !work.Pending() || !work.DefersExpiry() {
			t.Fatal("corrupt admitted evidence disappeared from work status", work, err)
		}
	}
}

func TestPublicationLostPublishedCacheCannotGuessPredecessor(t *testing.T) {
	for _, atomic := range []bool{false, true} {
		t.Run(map[bool]string{false: "in-place", true: "atomic-replacement"}[atomic], func(t *testing.T) {
			testPublicationLostPublishedCache(t, atomic)
		})
	}
}

func testPublicationLostPublishedCache(t *testing.T, atomic bool) {
	t.Helper()
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &putCountingStore{MemoryStore: storagetest.NewMemoryStore()}
	at := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
	result, err := Run(t.Context(), local, remote, opts)
	if err != nil || len(result.Published) != 1 {
		t.Fatal(result, err)
	}
	path := filepath.Join(local.Home(), "published", reg.ArchiveSessionID+".json")
	corrupt := []byte(`{"bundle":{"sche`)
	writePath := path
	if atomic {
		writePath = path + ".fixture"
	}
	if err := os.WriteFile(writePath, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if atomic {
		if err := os.Rename(writePath, path); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(local.Home(), "requests", reg.ArchiveSessionID+".json")
	requestBefore, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	key := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID).SourceBundle.Key
	before := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	remote.takePuts()
	for pass := range 3 {
		at = at.Add(time.Hour)
		result, err := Run(t.Context(), local, remote, opts)
		if pass > 0 {
			if !errors.Is(err, state.ErrDurableStorageRecovery) || !errors.Is(result.Errors["durable-storage"], state.ErrDurableStorageRecovery) || len(result.Published) != 0 {
				t.Fatal("lost predecessor census did not refuse", result, err)
			}
		} else if err != nil || !errors.Is(result.Errors[reg.ArchiveSessionID], state.ErrQuarantined) || len(result.Published) != 0 {
			t.Fatal("in-place corruption was not refused at session decode", result, err)
		}
		var retainedPath string
		{
			quarantined := local.QuarantinedFiles()
			if len(quarantined) != 1 {
				t.Fatal("corrupt selecting bytes lost during quarantine", quarantined)
			}
			retainedPath = filepath.Join(local.Home(), quarantined[0])
		}
		retained, readErr := os.ReadFile(retainedPath)
		if readErr != nil || string(retained) != string(corrupt) {
			t.Fatal("corrupt selecting evidence changed", readErr)
		}
		requestAfter, readErr := os.ReadFile(requestPath)
		if readErr != nil || string(requestBefore) != string(requestAfter) {
			t.Fatal("before-discovery refusal changed owed request", readErr)
		}
		if puts := remote.takePuts(); puts != 0 {
			t.Fatal("lost predecessor census performed remote writes", puts)
		}
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.SourceBundle.Key != key || !after.MetadataDerivedAt.Equal(before.MetadataDerivedAt) {
		t.Fatal("lost predecessor replaced authoritative metadata")
	}
	// The approved published census precedes session scanning. It keeps the
	// original corrupt body in place and cannot allocate replacement evidence.
	if _, err := os.Stat(filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replacement evidence allocated despite unknown predecessor", err)
	}
}

func TestPublicationNewRequestCannotReplaceObsoletePendingEvidence(t *testing.T) {
	for _, mode := range []publicationContextChange{publicationAdapterChange, publicationFilterChange, publicationDestinationChange} {
		t.Run(string(mode), func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
			at := reg.RegisteredAt.Add(time.Hour)
			opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
			if result, err := Run(t.Context(), local, remote, opts); err != nil || result.Errors[reg.ArchiveSessionID] == nil {
				t.Fatal(result, err)
			}
			pending, found, err := local.LoadPending(reg.ArchiveSessionID)
			if err != nil || !found {
				t.Fatal(found, err)
			}
			pending.Attempted = false
			switch mode {
			case publicationAdapterChange, publicationFilterChange:
				// Legacy unattempted journals must also survive policy changes.
				pending.JournalVersion, pending.Phase = 0, ""
				pending.Commit = nil
				pending.Sources = nil
				pending.Preparation, pending.Progress, pending.Cleanup = nil, nil, nil
				if mode == publicationAdapterChange {
					pending.Bundle.Capture.AdapterVersion = "obsolete"
				} else {
					pending.Bundle.Capture.FilterVersion = "obsolete"
				}
				// Produce actual matching legacy source/metadata bytes rather than
				// relabeling a current sealed body with an obsolete capture policy.
				packed, buildErr := archive.BuildCompressedSource(pending.Bundle)
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				key, buildErr := archive.SourceObjectKey(pending.Bundle, packed.SHA256)
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				pending.SourceKey, pending.SourceSHA256, pending.SourceSize, pending.SourceBytes = key, packed.SHA256, len(packed.Bytes), packed.Bytes
				var metadata archive.Metadata
				if err := json.Unmarshal(pending.MetadataBytes, &metadata); err != nil {
					t.Fatal(err)
				}
				metadata.SourceBundle = pending.SourceReference()
				metadata.FilterVersion, metadata.Adapter.Version = pending.Bundle.Capture.FilterVersion, pending.Bundle.Capture.AdapterVersion
				pending.MetadataBytes, err = json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
			case publicationDestinationChange:
				reg.DestinationID = "changed-destination"
				if err := local.SaveRegistration(reg); err != nil {
					t.Fatal(err)
				}
			}
			if err := local.SavePending(reg.ArchiveSessionID, pending); err != nil {
				t.Fatal(err)
			}
			pendingPath := filepath.Join(local.Home(), "pending", reg.ArchiveSessionID+".json")
			before, err := os.ReadFile(pendingPath)
			if err != nil {
				t.Fatal(err)
			}
			at = at.Add(time.Hour)
			if err := local.SaveRequest(reg.ArchiveSessionID, "stop", at); err != nil {
				t.Fatal(err)
			}
			remote.failMetadata = false
			result, err := Run(t.Context(), local, remote, opts)
			if err != nil || result.Errors[reg.ArchiveSessionID] == nil || len(result.Published) != 0 {
				t.Fatal("new request replaced incompatible pending evidence", result, err)
			}
			after, err := os.ReadFile(pendingPath)
			if err != nil || string(before) != string(after) {
				t.Fatal("pending evidence changed", err)
			}
			if _, found, err := local.LoadRequest(reg.ArchiveSessionID); err != nil || !found {
				t.Fatal("request acknowledged", found, err)
			}
		})
	}
}

type winnerAfterUploadStore struct {
	*storagetest.MemoryStore
	metadataKey   string
	readsAfterPut int
	winner        []byte
	cHash         string
	cETag         string
}

func (s *winnerAfterUploadStore) Put(ctx context.Context, key string, body []byte) error {
	if key == s.metadataKey {
		s.readsAfterPut = 1
	}
	return s.MemoryStore.Put(ctx, key, body)
}

func (s *winnerAfterUploadStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if key == s.metadataKey && s.readsAfterPut > 0 {
		s.readsAfterPut++
		if s.readsAfterPut == 3 {
			body, err := s.MemoryStore.GetLimited(ctx, key, limit)
			if err != nil {
				return nil, err
			}
			var m archive.Metadata
			if err := json.Unmarshal(body, &m); err != nil {
				return nil, err
			}
			m.SourceBundle.SHA256 = storage.SHA256Hex([]byte("missing authoritative source"))
			m.SourceBundle.Key = "sessions/claude/session-1/source." + m.SourceBundle.SHA256 + ".jsonl.gz"
			s.winner, err = json.Marshal(m)
			if err != nil {
				return nil, err
			}
			if err := s.MemoryStore.Put(ctx, key, s.winner); err != nil {
				return nil, err
			}
		}
	}
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

func TestPublicationChangedWinnerIsNotIndexedWithoutSourceVerification(t *testing.T) {
	local := newTestStore(t)
	claudeSession(t, local, claudePromptLine+"\n")
	remote := &winnerAfterUploadStore{MemoryStore: storagetest.NewMemoryStore(), metadataKey: "sessions/claude/session-1/metadata.json"}
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	result := runAt(t, local, remote, at)
	if !errors.Is(result.Errors["session-1"], storage.ErrPublicationConflict) || len(result.Published) != 0 {
		t.Fatal(result)
	}
	hints, err := remote.List(t.Context(), listingindex.V3Prefix)
	if err != nil || len(hints) != 1 || remote.cHash == "" || remote.cETag == "" {
		t.Fatal("exact C listing missing", hints, err)
	}
	for _, hint := range hints {
		revision, err := listingindex.ParseRevision(hint.Key)
		if err != nil || revision.Hash != remote.cHash || revision.ETag != remote.cETag || revision.Hash == storage.SHA256Hex(remote.winner) {
			t.Fatal("unverified winner indexed instead of exact C response", revision, err)
		}
	}
	if pending, found, err := local.LoadPublicationPending("session-1"); err != nil || !found || pending.Commit == nil || pending.Commit.MetadataSHA256 != remote.cHash {
		t.Fatal("D conflict lost selecting pending", found, err)
	}
	repairs, err := local.ListingRepairs(32)
	if err != nil || len(repairs) != 1 {
		t.Fatal("winner repair evidence lost", repairs, err)
	}
	result = runAt(t, local, remote, at.Add(time.Hour))
	if !errors.Is(result.Errors["listing-maintenance"], storage.ErrNotFound) {
		t.Fatal("winner source not checked on repair", result)
	}
}

type retainedComparatorSources struct {
	agentapi.SourcesLookup
	filter agentapi.TranscriptFilter
}

func (s retainedComparatorSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	provider, _, ok := s.SourcesLookup.LookupSources(name)
	return provider, s.filter, ok
}

type recordingRetainedFilter struct {
	agentapi.TranscriptFilter
	allow bool
	calls int
}

func (f *recordingRetainedFilter) EvidenceExtends(previous, candidate archive.SourceBundle) bool {
	f.calls++
	return f.allow && f.TranscriptFilter.EvidenceExtends(previous, candidate)
}

func historyPublication(t *testing.T, reg archive.SessionRegistration, at time.Time) state.PendingPublication {
	t.Helper()
	adapter, err := sourceAdapter(testSources, "codex")
	if err != nil {
		t.Fatal(err)
	}
	b := archive.SourceBundle{SchemaVersion: archive.HistorySourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "codex", AdapterVersion: adapter.Version(), FilterVersion: archive.FilterVersion, CapturedAt: at, SourceFormat: "codex-jsonl"}, NativeRecords: []map[string]any{{"type": "event_msg", "payload": map[string]any{"type": "task_started"}}}, Ordinals: []uint64{1}, History: &archive.SourceHistory{ActiveRolloutID: reg.NativeSessionID, ThreadID: reg.NativeSessionID, Spans: []archive.HistorySpan{{RolloutID: reg.NativeSessionID, ThreadID: reg.NativeSessionID, EndRecord: 1, StartOrdinal: 1, EndOrdinal: 2}}}}
	compressed, err := archive.BuildCompressedSource(b)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(b, compressed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ref := archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
	m, err := archive.BuildMetadataWithAnalysis(b, archive.Analysis{}, nil, "synthetic-machine", at, at, ref, archive.ParserInfo{Version: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey, err := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	return state.PendingPublication{Bundle: b, SourceKey: key, SourceSHA256: ref.SHA256, SourceBytes: compressed.Bytes, MetadataKey: metadataKey, MetadataBytes: raw}
}

func TestPublicationSealingUsesInjectedRetainedComparatorWithoutEnablingWriter(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(strconv.FormatBool(allow), func(t *testing.T) {
			local := newTestStore(t)
			reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
			reg.NativeSessionID = "11111111-1111-4111-8111-111111111111"
			at := reg.RegisteredAt.Add(time.Hour)
			old := historyPublication(t, reg, at)
			sealed, err := state.PreparePublicationV2(old, state.PublicationPredecessor{State: state.PredecessorAbsent}, "", "a", "p", state.PublicationCapture)
			if err != nil {
				t.Fatal(err)
			}
			published, err := local.LoadPublishedState(reg.ArchiveSessionID)
			if err != nil {
				t.Fatal(err)
			}
			if err := published.SaveCommittedPublication(sealed, at); err != nil {
				t.Fatal(err)
			}
			next := historyPublication(t, reg, at.Add(time.Hour))
			adapter, err := sourceAdapter(testSources, "codex")
			if err != nil {
				t.Fatal(err)
			}
			filter := &recordingRetainedFilter{TranscriptFilter: adapter, allow: allow}
			remote := storagetest.NewMemoryStore()
			scan := newSessionScan(t.Context(), local, remote, reg, state.Request{}, published, at, Options{Sources: retainedComparatorSources{SourcesLookup: testSources, filter: filter}})
			err = scan.sealPending(&next)
			if (err == nil) != allow || filter.calls != 1 {
				t.Fatal("comparator decision not honored", err, filter.calls)
			}
			if allow {
				if next.Commit == nil || next.ValidatePublication() != nil {
					t.Fatal("continuity not sealed")
				}
				if _, err := scan.publishPending(scan.ctx, next); !errors.Is(err, archive.ErrHistoryMutationPending) {
					t.Fatal("writer fence lifted", err)
				}
			}
		})
	}
}

type oversizedPublicationStore struct {
	*storagetest.MemoryStore
	unlimitedReads int
	limitedReads   int
}

func (s *oversizedPublicationStore) Get(context.Context, string) ([]byte, error) {
	s.unlimitedReads++
	return nil, errors.New("unbounded authoritative metadata read")
}

func (s *oversizedPublicationStore) GetLimited(_ context.Context, _ string, limit int64) ([]byte, error) {
	s.limitedReads++
	if limit != 32<<20 {
		return nil, errors.New("incorrect authoritative metadata bound")
	}
	return nil, storage.ErrObjectTooLarge
}

func TestPublicationRecoveryMetadataReadsUseAllocationBound(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "synthetic.jsonl", codexTranscript))
	remote := &metadataFailStore{MemoryStore: storagetest.NewMemoryStore(), failMetadata: true}
	at := reg.RegisteredAt.Add(time.Hour)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "synthetic-machine", Now: func() time.Time { return at }}
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if result, err := Run(t.Context(), local, remote, opts); err != nil || result.Errors[reg.ArchiveSessionID] == nil {
		t.Fatal(result, err)
	}
	pending, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveCommittedPublication(pending, at); err != nil {
		t.Fatal("fixture selecting save", pending.JournalVersion, pending.Phase, pending.ValidatePublication(), err)
	}
	// Model an actual legacy missing-body cache. A selecting protocol-2 writer
	// correctly refuses deleting the body bound by its existing commit.
	editPublishedState(t, local, func(raw map[string]any) { delete(raw, "metadata_bytes") })
	published, err = local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	bounded := &oversizedPublicationStore{MemoryStore: storagetest.NewMemoryStore()}
	scan := newSessionScan(t.Context(), local, bounded, reg, state.Request{}, published, at, opts)
	if err := scan.checkHistoryPublication(pending); !errors.Is(err, storage.ErrObjectTooLarge) {
		t.Fatal(err)
	}
	if _, usable, _ := scan.lastPublication(pending.MetadataKey); usable {
		t.Fatal("oversized legacy metadata adopted")
	}
	if bounded.unlimitedReads != 0 || bounded.limitedReads != 2 {
		t.Fatal("recovery bypassed metadata allocation bound", bounded.unlimitedReads, bounded.limitedReads)
	}
}

func (s *winnerAfterUploadStore) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	if key == s.metadataKey && s.readsAfterPut > 0 {
		s.readsAfterPut++
		if s.readsAfterPut == 3 {
			body, err := s.MemoryStore.GetLimited(ctx, key, limit)
			if err != nil {
				return nil, "", err
			}
			var m archive.Metadata
			if err := json.Unmarshal(body, &m); err != nil {
				return nil, "", err
			}
			m.SourceBundle.SHA256 = storage.SHA256Hex([]byte("missing authoritative source"))
			m.SourceBundle.Key = "sessions/claude/session-1/source." + m.SourceBundle.SHA256 + ".jsonl.gz"
			s.winner, err = json.Marshal(m)
			if err != nil {
				return nil, "", err
			}
			if err := s.MemoryStore.Put(ctx, key, s.winner); err != nil {
				return nil, "", err
			}
		}
	}
	body, etag, err := s.MemoryStore.GetLimitedVersioned(ctx, key, limit)
	if key == s.metadataKey && s.readsAfterPut == 2 && err == nil {
		s.cHash, s.cETag = storage.SHA256Hex(body), etag
	}
	return body, etag, err
}

func TestPublicationClosingProofRefusesChangedFrameBeforeLocalSave(t *testing.T) {
	for _, change := range []publicationClosingChange{publicationClosingChangeSingleUse, publicationClosingChangeDifferentSelection, publicationClosingChangeDestination, publicationClosingChangePolicy, publicationClosingChangeCancel, publicationClosingChangeNewAttempt} {
		t.Run(string(change), func(t *testing.T) {
			scan, pending := privacyJournal(t)
			defer scan.releaseRetained()
			endAttempt, attemptErr := scan.beginPublicationAttempt()
			if attemptErr != nil {
				t.Fatal(attemptErr)
			}
			defer endAttempt()
			if err := scan.remote.Put(t.Context(), pending.MetadataKey, pending.MetadataBytes); err != nil {
				t.Fatal(err)
			}
			for _, stage := range pending.History.Sources {
				raw, err := scan.local.ReadPendingSource(scan.id(), stage)
				if err != nil {
					t.Fatal(err)
				}
				if err := scan.sourceSetRemote().Put(t.Context(), stage.Reference.Key, raw); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := scan.closePublicationReadback(pending); !errors.Is(err, state.ErrDurableStorageRecovery) {
				t.Fatal("closing proof minted before actual full typed verification", err)
			}
			metadata, err := scan.frozenHistoryMetadata(pending)
			if err != nil {
				t.Fatal(err)
			}
			if err := scan.verifyHistoryReferences(pending, metadata); err != nil {
				t.Fatal(err)
			}
			proof, err := scan.closePublicationReadback(pending)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case publicationClosingChangeSingleUse:
				if err = proof.consume(scan, pending); err != nil {
					t.Fatal(err)
				}
			case publicationClosingChangeDifferentSelection:
				commit := *pending.Commit
				commit.MetadataSHA256 = storage.SHA256Hex([]byte("different selection in same session"))
				pending.Commit = &commit
			case publicationClosingChangeDestination:
				scan.reg.DestinationID = "foreign-destination-after-D"
			case publicationClosingChangePolicy:
				scan.opts.SkillEvidence = config.SkillEvidenceNone
			case publicationClosingChangeCancel:
				ctx, cancel := context.WithCancel(scan.ctx)
				cancel()
				scan.ctx = ctx
			case publicationClosingChangeNewAttempt:
				endAttempt()
				scan.remote = storagetest.NewMemoryStore()
				endNew, attemptErr := scan.beginPublicationAttempt()
				if attemptErr != nil {
					t.Fatal(attemptErr)
				}
				defer endNew()
			}
			if err = proof.consume(scan, pending); err == nil {
				t.Fatal("D proof acknowledged a changed frame", change)
			}
			if scan.published.Found() {
				t.Fatal("proof refusal selected local authority")
			}
		})
	}
}

type winnerDuringListingStore struct {
	*storagetest.MemoryStore
	metadataKey string
	winner      []byte
	changed     bool
}

func (s *winnerDuringListingStore) Put(ctx context.Context, key string, body []byte) error {
	if err := s.MemoryStore.Put(ctx, key, body); err != nil {
		return err
	}
	if !strings.HasPrefix(key, listingindex.V3Prefix) || s.changed {
		return nil
	}
	s.changed = true
	raw, err := s.GetLimited(ctx, s.metadataKey, 32<<20)
	if err != nil {
		return err
	}
	var m archive.Metadata
	if err = json.Unmarshal(raw, &m); err != nil {
		return err
	}
	m.SourceBundle.SHA256 = storage.SHA256Hex([]byte("missing winner during listing"))
	m.SourceBundle.Key = "sessions/claude/session-1/source." + m.SourceBundle.SHA256 + ".jsonl.gz"
	s.winner, err = json.Marshal(m)
	if err != nil {
		return err
	}
	return s.MemoryStore.Put(ctx, s.metadataKey, s.winner)
}

func TestPublicationWinnerDuringListingRetainsClosingObligations(t *testing.T) {
	local := newTestStore(t)
	claudeSession(t, local, claudePromptLine+"\n")
	remote := &winnerDuringListingStore{MemoryStore: storagetest.NewMemoryStore(), metadataKey: "sessions/claude/session-1/metadata.json"}
	result := runAt(t, local, remote, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	if !remote.changed || !errors.Is(result.Errors["session-1"], storage.ErrPublicationConflict) || len(result.Published) != 0 {
		t.Fatal("D did not close after listing mutation", result)
	}
	pending, found, err := local.LoadPublicationPending("session-1")
	if err != nil || !found || pending.Commit == nil {
		t.Fatal("pending lost after listing mutation", found, err)
	}
	repairs, err := local.ListingRepairs(32)
	if err != nil || len(repairs) != 1 {
		t.Fatal("listing obligation lost before D", repairs, err)
	}
	hints, err := remote.List(t.Context(), listingindex.V3Prefix)
	if err != nil || len(hints) != 1 {
		t.Fatal(hints, err)
	}
	revision, err := listingindex.ParseRevision(hints[0].Key)
	if err != nil || revision.Hash != pending.Commit.MetadataSHA256 || revision.Hash == storage.SHA256Hex(remote.winner) {
		t.Fatal("foreign unverified listing winner indexed", revision, err)
	}
	if published, err := local.LoadPublishedState("session-1"); err != nil || published.Found() {
		t.Fatal("closing conflict selected local authority", err)
	}
}

func TestPublicationAttemptKeepsActualProviderAcrossAmbientReplacement(t *testing.T) {
	scan, pending := privacyJournal(t)
	defer scan.releaseRetained()
	if err := scan.remote.Put(t.Context(), pending.MetadataKey, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	scan.remote = nonComparablePublicationStore{MemoryStore: scan.remote.(*storagetest.MemoryStore), opaque: map[string]string{"fixture": "valid"}}
	endAttempt, attemptErr := scan.beginPublicationAttempt()
	if attemptErr != nil {
		t.Fatal(attemptErr)
	}
	defer endAttempt()
	// Valid non-comparable wrapper fields do not affect private frame identity.
	scan.remote = storagetest.NewMemoryStore()
	for _, stage := range pending.History.Sources {
		raw, err := scan.local.ReadPendingSource(scan.id(), stage)
		if err != nil {
			t.Fatal(err)
		}
		if err := scan.sourceSetRemote().Put(t.Context(), stage.Reference.Key, raw); err != nil {
			t.Fatal(err)
		}
	}
	metadata, err := scan.frozenHistoryMetadata(pending)
	if err != nil {
		t.Fatal(err)
	}
	if err := scan.verifyHistoryReferences(pending, metadata); err != nil {
		t.Fatal(err)
	}
	proof, err := scan.closePublicationReadback(pending)
	if err != nil {
		t.Fatal("attempt silently retargeted to ambient provider", err)
	}
	if err = proof.consume(scan, pending); err != nil {
		t.Fatal(err)
	}
}

type nonComparablePublicationStore struct {
	*storagetest.MemoryStore
	opaque map[string]string
}

type publicationClosingChange string

const (
	publicationClosingChangeSingleUse          publicationClosingChange = "single-use"
	publicationClosingChangeDifferentSelection publicationClosingChange = "different-selection"
	publicationClosingChangeDestination        publicationClosingChange = "destination"
	publicationClosingChangePolicy             publicationClosingChange = "policy"
	publicationClosingChangeCancel             publicationClosingChange = "cancel"
	publicationClosingChangeNewAttempt         publicationClosingChange = "new-attempt"
)
