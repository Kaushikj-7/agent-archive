package state

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

const publicationThread = "11111111-1111-4111-8111-111111111111"

func publicationFixture(t *testing.T, revision string, at time.Time) PendingPublication {
	t.Helper()
	b := archive.SourceBundle{SchemaVersion: archive.HistorySourceSchemaVersion, ArchiveSessionID: "publication-fixture", NativeSessionID: publicationThread, ProjectID: "synthetic-project", Capture: archive.SourceCapture{Harness: archive.Harness{Name: "codex"}, AdapterName: "codex", AdapterVersion: "synthetic-v1", FilterVersion: archive.FilterVersion, CapturedAt: at, SourceFormat: "codex-jsonl"}, NativeRecords: []map[string]any{{"type": "event_msg", "payload": map[string]any{"type": "task_started"}}}, Ordinals: []uint64{1}, History: &archive.SourceHistory{ActiveRolloutID: revision, ThreadID: publicationThread, Spans: []archive.HistorySpan{{RolloutID: revision, ThreadID: publicationThread, EndRecord: 1, StartOrdinal: 1, EndOrdinal: 2}}}}
	compressed, err := archive.BuildCompressedSource(b)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(b, compressed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	ref := archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
	m, err := archive.BuildMetadataWithAnalysis(b, archive.Analysis{}, nil, "synthetic-machine", at, at, ref, archive.ParserInfo{Name: "codex", Version: "synthetic-v1"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey, err := archive.MetadataObjectKey("codex", b.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	return PendingPublication{Bundle: b, SourceKey: key, SourceSHA256: ref.SHA256, SourceBytes: compressed.Bytes, MetadataKey: metadataKey, MetadataBytes: raw}
}

func preservePublication(t *testing.T, p *PendingPublication, prior PendingPublication, revision string) {
	t.Helper()
	var m archive.Metadata
	if err := json.Unmarshal(p.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	m.History.Preserved = append(m.History.Preserved, archive.RevisionReference{RevisionID: revision, CapturedAt: prior.Bundle.Capture.CapturedAt, Source: prior.SourceReference(), FilterVersion: prior.Bundle.Capture.FilterVersion, SourceSchemaVersion: prior.Bundle.SchemaVersion})
	var err error
	p.MetadataBytes, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	p.Sources = append(p.Sources, PublicationSource{Reference: prior.SourceReference(), Bytes: prior.SourceBytes})
}

func TestPublicationJournalRetainsCompleteSelectionAcrossRestart(t *testing.T) {
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	old := publicationFixture(t, publicationThread, at)
	next := publicationFixture(t, "22222222-2222-4222-8222-222222222222", at.Add(time.Hour))
	preservePublication(t, &next, old, publicationThread)
	next.History = &PendingHistory{Version: 1, ExpectedMetadataSHA256: publicationSHA256(old.MetadataBytes)}
	pending, err := PreparePublicationV2(next, PublicationPredecessor{State: PredecessorPresent, Body: old.MetadataBytes, Bundle: old.Bundle}, "destination", "admission", "policy", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t)
	if err := store.SavePending(next.Bundle.ArchiveSessionID, pending); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(store.Home())
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := reopened.LoadPending(next.Bundle.ArchiveSessionID)
	if err != nil || !found || got.ValidatePublication() != nil || len(got.Sources) != 2 {
		t.Fatal(got.Commit, found, err)
	}
	published, err := reopened.LoadPublishedState(next.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveCommittedPublication(got, at); err != nil {
		t.Fatal(err)
	}
	reloaded, err := reopened.LoadPublishedState(next.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := reloaded.CommittedSources()
	if err != nil || len(refs) != 2 || refs[0] != next.SourceReference() || refs[1] != old.SourceReference() {
		t.Fatal(refs, err)
	}
	// A newer declined candidate cannot erase the committed source set.
	if err := reloaded.Save(old.Bundle, at, CacheStatusDeclined); err != nil {
		t.Fatal(err)
	}
	refs, err = reloaded.CommittedSources()
	if err != nil || len(refs) != 2 {
		t.Fatal(refs, err)
	}
}

func TestPublicationRejectsEvictionMissingContinuityAndLocalCorruption(t *testing.T) {
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	old := publicationFixture(t, publicationThread, at)
	next := publicationFixture(t, "22222222-2222-4222-8222-222222222222", at.Add(time.Hour))
	prior := PublicationPredecessor{State: PredecessorPresent, Body: old.MetadataBytes, Bundle: old.Bundle}
	if _, err := PreparePublication(next, prior, "d", "a", "p", PublicationCapture); err == nil {
		t.Fatal("revision eviction accepted")
	}
	same := publicationFixture(t, publicationThread, at.Add(time.Hour))
	if _, err := PreparePublication(same, prior, "d", "a", "p", PublicationCapture); err == nil {
		t.Fatal("missing provider continuity accepted")
	}
	prior.SameRevisionContinuity = &PublicationContinuity{PreviousSourceSHA256: old.SourceSHA256, NextSourceSHA256: same.SourceSHA256}
	sealed, err := PreparePublication(same, prior, "d", "a", "p", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	sealed.SourceBytes[0] ^= 1
	if sealed.ValidatePublication() == nil {
		t.Fatal("corrupt replay accepted")
	}
	sealed.SourceBytes[0] ^= 1
	sealed.MetadataBytes = []byte(`{}`)
	if sealed.ValidatePublication() == nil {
		t.Fatal("incomplete metadata accepted")
	}
}

func TestPublicationDigestCanonicalOrderAndPrivateContext(t *testing.T) {
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	current := publicationFixture(t, publicationThread, at)
	first := publicationFixture(t, "22222222-2222-4222-8222-222222222222", at.Add(-time.Hour))
	second := publicationFixture(t, "33333333-3333-4333-8333-333333333333", at.Add(-2*time.Hour))
	preservePublication(t, &current, first, first.Bundle.History.ActiveRolloutID)
	preservePublication(t, &current, second, second.Bundle.History.ActiveRolloutID)
	a, _, err := archive.PublicationIdentity(current.MetadataBytes, "d", "a", "p", string(PublicationCapture))
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(current.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	m.History.Preserved[0], m.History.Preserved[1] = m.History.Preserved[1], m.History.Preserved[0]
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := archive.PublicationIdentity(raw, "d", "a", "p", string(PublicationCapture))
	if err != nil || a != b {
		t.Fatal("reference order changed semantic identity", err)
	}
	for _, changed := range [][4]string{{"different", "a", "p", string(PublicationCapture)}, {"d", "different", "p", string(PublicationCapture)}, {"d", "a", "different", string(PublicationCapture)}, {"d", "a", "p", string(PublicationMetadata)}} {
		digest, _, err := archive.PublicationIdentity(raw, changed[0], changed[1], changed[2], changed[3])
		if err != nil || digest == a {
			t.Fatal("context was not bound", changed, err)
		}
	}
}

func TestPublicationPreservedReferenceCapDoesNotEvict(t *testing.T) {
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	pending := publicationFixture(t, publicationThread, at)
	for i := range archive.MaxPreservedRevisions {
		id := fmt.Sprintf("%08x-2222-4222-8222-222222222222", i+1)
		prior := publicationFixture(t, id, at.Add(-time.Duration(i+1)*time.Hour))
		preservePublication(t, &pending, prior, id)
	}
	sealed, err := PreparePublication(pending, PublicationPredecessor{State: PredecessorAbsent}, "d", "a", "p", PublicationCapture)
	if err != nil || len(sealed.Sources) != 65 {
		t.Fatal("64 preserved revisions rejected", err)
	}
	extra := publicationFixture(t, "ffffffff-2222-4222-8222-222222222222", at.Add(-100*time.Hour))
	preservePublication(t, &pending, extra, extra.Bundle.History.ActiveRolloutID)
	if _, err := PreparePublication(pending, PublicationPredecessor{State: PredecessorAbsent}, "d", "a", "p", PublicationCapture); err == nil {
		t.Fatal("overflow accepted")
	}
	if sealed.ValidatePublication() != nil || len(sealed.Sources) != 65 {
		t.Fatal("cap changed previous exact journal")
	}
}

func TestPublicationRemoteCacheCannotInventPredecessorAfterRestart(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	pending := publicationFixture(t, publicationThread, at)
	published, err := store.LoadPublishedState(pending.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.Save(pending.Bundle, at, CacheStatusPublished); err != nil {
		t.Fatal(err)
	}
	if err := published.CacheMetadata(pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	if got := published.PublicationPredecessor(); got.State != PredecessorUnknown {
		t.Fatal("remote cache invented predecessor", got.State)
	}
	if err := published.Save(pending.Bundle, at, CacheStatusDeclined); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.LoadPublishedState(pending.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.PublicationPredecessor(); got.State != PredecessorUnknown {
		t.Fatal("restart forgot unknown authority", got.State)
	}
	sealed, err := PreparePublication(pending, PublicationPredecessor{State: PredecessorUnknown}, "d", "a", "p", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.SaveCommittedPublication(sealed, at); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.PublicationPredecessor(); got.State != PredecessorPresent || string(got.Body) != string(pending.MetadataBytes) {
		t.Fatal("verified local commit did not restore exact authority", got.State)
	}
}

func TestPublicationPromotesExactPreservedRevisionWithoutEviction(t *testing.T) {
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	old := publicationFixture(t, publicationThread, at)
	current := publicationFixture(t, "22222222-2222-4222-8222-222222222222", at.Add(time.Hour))
	preservePublication(t, &current, old, publicationThread)
	reverted := publicationFixture(t, publicationThread, at)
	preservePublication(t, &reverted, current, current.Bundle.History.ActiveRolloutID)
	sealed, err := PreparePublication(reverted, PublicationPredecessor{State: PredecessorPresent, Body: current.MetadataBytes, Bundle: current.Bundle}, "d", "a", "p", PublicationCapture)
	if err != nil || len(sealed.Sources) != 2 {
		t.Fatal("exact preserved promotion failed", err)
	}
	changed := publicationFixture(t, publicationThread, at.Add(2*time.Hour))
	preservePublication(t, &changed, current, current.Bundle.History.ActiveRolloutID)
	if _, err := PreparePublication(changed, PublicationPredecessor{State: PredecessorPresent, Body: current.MetadataBytes, Bundle: current.Bundle}, "d", "a", "p", PublicationCapture); err == nil {
		t.Fatal("changed promoted evidence silently replaced old preserved source")
	}
}

func TestPublicationMetadataCompletionRestoresMissingLocalCache(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pending := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	pending.MetadataOnly = true
	sealed, err := PreparePublication(pending, PublicationPredecessor{State: PredecessorUnknown}, "d", "a", "p", PublicationMetadata)
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.LoadPublishedState(pending.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveCommittedPublication(sealed, pending.Bundle.Capture.CapturedAt); err != nil {
		t.Fatal("exact-next completion couldn't restore local cache", err)
	}
	if refs, err := published.CommittedSources(); err != nil || len(refs) != 1 {
		t.Fatal(refs, err)
	}
}

func TestPublicationLegacyMetadataSaveDoesNotKeepStaleCommit(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pending := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	sealed, err := PreparePublication(pending, PublicationPredecessor{State: PredecessorAbsent}, "d", "a", "p", PublicationCapture)
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.LoadPublishedState(pending.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveCommittedPublication(sealed, pending.Bundle.Capture.CapturedAt); err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(pending.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	m.MetadataDerivedAt = m.MetadataDerivedAt.Add(time.Hour)
	pending.MetadataBytes, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := published.SaveRepublishedMetadata(pending, m.MetadataDerivedAt); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.LoadPublishedState(pending.Bundle.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if refs, err := reloaded.CommittedSources(); err != nil || len(refs) != 1 || refs[0] != pending.SourceReference() {
		t.Fatal("legacy method left stale source-set digest", refs, err)
	}
	if prior := reloaded.PublicationPredecessor(); prior.State != PredecessorPresent || string(prior.Body) != string(pending.MetadataBytes) {
		t.Fatal("committed legacy method lost exact authority", prior.State)
	}
}

func TestPublicationRejectsMissingMetadataIdentityAndTimestamps(t *testing.T) {
	for _, field := range []string{"machine_id", "native_session_id", "project_id", "started_at", "captured_at", "metadata_derived_at"} {
		t.Run(field, func(t *testing.T) {
			pending := publicationFixture(t, publicationThread, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
			var m map[string]any
			if err := json.Unmarshal(pending.MetadataBytes, &m); err != nil {
				t.Fatal(err)
			}
			delete(m, field)
			var err error
			pending.MetadataBytes, err = json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := PreparePublication(pending, PublicationPredecessor{State: PredecessorAbsent}, "d", "a", "p", PublicationCapture); err == nil {
				t.Fatal("incomplete metadata sealed")
			}
		})
	}
}

func TestPublicationRemoteReferencesCanExceedInlineReplayBudget(t *testing.T) {
	at := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	pending := publicationFixture(t, publicationThread, at)
	old := publicationFixture(t, "22222222-2222-4222-8222-222222222222", at.Add(-time.Hour))
	preservePublication(t, &pending, old, old.Bundle.History.ActiveRolloutID)
	var m archive.Metadata
	if err := json.Unmarshal(pending.MetadataBytes, &m); err != nil {
		t.Fatal(err)
	}
	m.SourceBundle.CompressedBytes = 80 << 20
	m.History.Preserved[0].Source.CompressedBytes = 80 << 20
	var err error
	pending.MetadataBytes, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	pending.SourceBytes, pending.Sources = nil, nil
	pending.SourceSize = m.SourceBundle.CompressedBytes
	pending.MetadataOnly = true
	sealed, err := PreparePublication(pending, PublicationPredecessor{State: PredecessorAbsent}, "d", "a", "p", PublicationMetadata)
	if err != nil || len(sealed.Sources) != 2 || sealed.ValidatePublication() != nil {
		t.Fatal("remote-only total was confused with inline replay cap", err)
	}
	m.History.Preserved[0].Source.CompressedBytes = (128 << 20) + 1
	pending.MetadataBytes, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PreparePublication(pending, PublicationPredecessor{State: PredecessorAbsent}, "d", "a", "p", PublicationMetadata); err == nil {
		t.Fatal("per-object bound was not enforced")
	}
}
