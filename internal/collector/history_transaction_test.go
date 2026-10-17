package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func frozenHistoryFixture(t *testing.T) (*sessionScan, state.PendingPublication, *storagetest.MemoryStore, archive.SourceReference) {
	t.Helper()
	reg := registration(t, "/synthetic/native.jsonl")
	reg.NativeSessionID = "11111111-1111-4111-8111-111111111111"
	local := newTestStore(t)
	cloud := storagetest.NewMemoryStore()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "codex", AdapterVersion: "0.15.0", FilterVersion: archive.FilterVersion, CapturedAt: at}, NativeRecords: []map[string]any{{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "active"}}}}
	packed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(bundle, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	active := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	previous := bundle
	previous.Capture.CapturedAt = at.Add(-time.Hour)
	previous.NativeRecords = []map[string]any{{"type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "previous"}}}
	oldPacked, err := archive.BuildCompressedSource(previous)
	if err != nil {
		t.Fatal(err)
	}
	oldKey, err := archive.SourceObjectKey(previous, oldPacked.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	oldRef := archive.SourceReference{Key: oldKey, SHA256: oldPacked.SHA256, CompressedBytes: len(oldPacked.Bytes)}
	stage, err := local.StagePendingSource(reg.ArchiveSessionID, oldRef, oldPacked.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	metadata := archive.Metadata{MachineID: "m", MetadataDerivedAt: at, SchemaVersion: archive.HistoryMetadataSchemaVersion, SessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Harness: reg.Harness, CapturedAt: at, FilterVersion: archive.FilterVersion, SourceBundle: active, History: &archive.RevisionHistory{CurrentRevision: reg.NativeSessionID, Preserved: []archive.RevisionReference{{RevisionID: "22222222-2222-4222-8222-222222222222", CapturedAt: previous.Capture.CapturedAt, Source: oldRef}}}}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	pending := state.PendingPublication{Bundle: bundle, SourceKey: active.Key, SourceSHA256: active.SHA256, SourceBytes: packed.Bytes, MetadataKey: metadataKey, MetadataBytes: encoded, History: &state.PendingHistory{Version: 1, Sources: []state.PendingSource{stage}}}
	if err := local.SavePending(reg.ArchiveSessionID, pending); err != nil {
		t.Fatal(err)
	}
	for key, raw := range map[string][]byte{active.Key: packed.Bytes, oldRef.Key: oldPacked.Bytes} {
		if err := cloud.Put(t.Context(), key, raw); err != nil {
			t.Fatal(err)
		}
	}
	scan := &sessionScan{ctx: t.Context(), local: local, remote: cloud, reg: reg, now: at}
	return scan, pending, cloud, oldRef
}

func TestFrozenHistoryMetadataRefusesObservedConflictAndAcceptsCommittedRetry(t *testing.T) {
	t.Parallel()
	scan, pending, cloud, _ := frozenHistoryFixture(t)
	committed, err := scan.checkFrozenHistoryMetadata(pending)
	if err != nil || committed {
		t.Fatal(committed, err)
	}
	var prior archive.Metadata
	if err := json.Unmarshal(pending.MetadataBytes, &prior); err != nil {
		t.Fatal(err)
	}
	prior.MetadataDerivedAt = scan.now.Add(-time.Minute)
	raw, err := json.Marshal(prior)
	if err != nil {
		t.Fatal(err)
	}
	if err := cloud.Put(t.Context(), pending.MetadataKey, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := scan.checkFrozenHistoryMetadata(pending); !errors.Is(err, errHistoryMetadataConflict) {
		t.Fatal(err)
	}
	pending.History.ExpectedMetadataSHA256 = metadataSHA(raw)
	if _, err := scan.checkFrozenHistoryMetadata(pending); err != nil {
		t.Fatal(err)
	}
	prior.MetadataDerivedAt = scan.now.Add(time.Minute)
	changed, _ := json.Marshal(prior)
	if err := cloud.Put(t.Context(), pending.MetadataKey, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := scan.checkFrozenHistoryMetadata(pending); !errors.Is(err, errHistoryMetadataConflict) {
		t.Fatal(err)
	}
	if err := cloud.Put(t.Context(), pending.MetadataKey, pending.MetadataBytes); err != nil {
		t.Fatal(err)
	}
	committed, err = scan.checkFrozenHistoryMetadata(pending)
	if err != nil || !committed {
		t.Fatal(committed, err)
	}
	if err := scan.verifyHistoryReadback(pending); err != nil {
		t.Fatal(err)
	}
	if err := cloud.Delete(t.Context(), pending.History.Sources[0].Reference.Key); err != nil {
		t.Fatal(err)
	}
	if err := scan.verifyHistoryReadback(pending); err == nil {
		t.Fatal("acknowledged final metadata with missing preserved source")
	}
}

func TestFrozenHistoryMissingOrCorruptStageRecoversOnlyValidatedRemoteBytes(t *testing.T) {
	t.Parallel()
	scan, pending, cloud, _ := frozenHistoryFixture(t)
	stage := pending.History.Sources[0]
	stagePath := filepath.Join(scan.local.Home(), "sessions", scan.id(), "pending-sources", stage.Name)
	for _, corrupt := range []bool{false, true} {
		if corrupt {
			if err := os.WriteFile(stagePath, []byte("corrupt synthetic stage"), 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.Remove(stagePath); err != nil {
				t.Fatal(err)
			}
		}
		if err := scan.recoverHistoryStages(pending); err != nil {
			t.Fatal(err)
		}
		got, err := scan.local.ReadPendingSource(scan.id(), stage)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := cloud.Get(t.Context(), stage.Reference.Key)
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatal(err)
		}
	}
}

type oversizedHistoryStore struct {
	*storagetest.MemoryStore
	oversizedKey string
	seenLimit    int64
}

func (s *oversizedHistoryStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if key == s.oversizedKey {
		s.seenLimit = limit
		return nil, storage.ErrObjectTooLarge
	}
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

func (s *oversizedHistoryStore) Get(context.Context, string) ([]byte, error) {
	panic("unbounded history GET")
}

func (s *oversizedHistoryStore) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	if key == s.oversizedKey {
		s.seenLimit = limit
		return nil, "", storage.ErrObjectTooLarge
	}
	return s.MemoryStore.GetLimitedVersioned(ctx, key, limit)
}

func TestOversizedHistoryMetadataAndRemoteStageLeaveFrozenWorkUntouched(t *testing.T) {
	t.Parallel()
	scan, pending, cloud, old := frozenHistoryFixture(t)
	journal := filepath.Join(scan.local.Home(), "pending", scan.id()+".json")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	limited := &oversizedHistoryStore{MemoryStore: cloud, oversizedKey: pending.MetadataKey}
	scan.remote = limited
	if _, err := scan.checkFrozenHistoryMetadata(pending); !errors.Is(err, storage.ErrObjectTooLarge) || limited.seenLimit != historyMetadataLimit {
		t.Fatal(err, limited.seenLimit)
	}
	limited.oversizedKey = old.Key
	stage := pending.History.Sources[0]
	if err := os.Remove(filepath.Join(scan.local.Home(), "sessions", scan.id(), "pending-sources", stage.Name)); err != nil {
		t.Fatal(err)
	}
	if err := scan.recoverHistoryStages(pending); !errors.Is(err, storage.ErrObjectTooLarge) || limited.seenLimit != int64(old.CompressedBytes) {
		t.Fatal(err, limited.seenLimit)
	}
	after, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("frozen descriptor changed", err)
	}
	if _, err := scan.local.ReadPendingSource(scan.id(), stage); err == nil {
		t.Fatal("oversized remote source became a local stage")
	}
}

func TestSequentialHistoryPreparationPreservesCaptureAndPrivacyObligation(t *testing.T) {
	t.Parallel()
	scan, pending := privacyJournal(t)
	defer scan.releaseRetained()
	var metadata archive.Metadata
	if err := json.Unmarshal(pending.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	input := &pending.History.Inputs[1]
	bundle, err := scan.loadHistoryInput(metadata, *input)
	if err != nil {
		t.Fatal(err)
	}
	captured := bundle.Capture.CapturedAt
	bundle.Capture.FilterVersion = "14"
	bundle.NativeRecords[len(bundle.NativeRecords)-1]["api_key"] = "sk-abcdefghijklmnopqrstuv"
	packed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(bundle, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	prior := archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}
	stage, err := scan.local.StagePendingSource(scan.id(), prior, packed.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	old := input.Reference
	input.Reference, input.FilterVersion = prior, "14"
	for i := range pending.History.Sources {
		if pending.History.Sources[i].Reference == old {
			pending.History.Sources[i] = stage
		}
	}
	for i := range metadata.History.Preserved {
		if metadata.History.Preserved[i].RevisionID == input.RevisionID {
			metadata.History.Preserved[i].Source, metadata.History.Preserved[i].FilterVersion = prior, "14"
		}
	}
	pending.MetadataBytes, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	pending = freshLegacyHistoryOriginal(t, scan, pending)
	for pending.History.Preparing {
		if err := scan.advanceHistoryPreparation(&pending); err != nil {
			t.Fatal(err)
		}
	}
	if err := scan.advanceHistoryPreparation(&pending); err != nil {
		t.Fatal(err)
	}
	if pending.History.Preparing || pending.History.PrivacyCursor != len(pending.History.Inputs) || len(pending.History.Retired) != 1 || !pending.History.Retired[0].PrivacySensitive || pending.History.Retired[0].Reference != prior {
		t.Fatal(pending.History)
	}
	if err := json.Unmarshal(pending.MetadataBytes, &metadata); err != nil {
		t.Fatal(err)
	}
	if !metadata.History.Preserved[0].CapturedAt.Equal(captured) || metadata.History.Preserved[0].Source == prior {
		t.Fatal(metadata.History)
	}
	saved, found, err := scan.local.LoadPublicationPending(scan.id())
	if err != nil || !found {
		t.Fatal(found, err)
	}
	before := bytes.Clone(saved.MetadataBytes)
	if err := scan.advanceHistoryPreparation(&saved); err != nil || !bytes.Equal(before, saved.MetadataBytes) {
		t.Fatal("finished preparation changed frozen bytes", err)
	}
}
