package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type syntheticCutover struct {
	valid bool
	id    string
}

func (a *syntheticCutover) VerifyCatalogCutover(_ context.Context, src, dst destination.Config) (CutoverProof, error) {
	if !a.valid {
		return CutoverProof{}, errors.New("synthetic credentials still writable")
	}
	return CutoverProof{ID: a.id, Source: destinationIdentity(src), Destination: destinationIdentity(dst), Protocol: 10}, nil
}

func migrationFixture(t *testing.T, count int) (*storagetest.MemoryStore, *qualifiedStore, destination.Config, destination.Config, *syntheticCutover) {
	t.Helper()
	sourceWriter, source := fixture(t)
	for i := range count {
		m := mutation(t, sourceWriter, fmt.Sprintf("session-%04d", i))
		raw, err := json.Marshal(m.Next.Summary)
		if err != nil {
			t.Fatal(err)
		}
		if err = source.Put(t.Context(), m.SessionKey, raw); err != nil {
			t.Fatal(err)
		}
	}
	target := &qualifiedStore{storagetest.NewMemoryStore()}
	src := destination.Config{Provider: "synthetic", Bucket: "source", Prefix: "old"}
	dst := destination.Config{Provider: "synthetic", Bucket: "source", Prefix: "new", ArchiveFormat: destination.FormatCatalogV4}
	return source.MemoryStore, target, src, dst, &syntheticCutover{true, "reviewed-private-credential-revocation"}
}

func TestMigrationResumesBoundedCheckpointsPreservesRetentionAndRollback(t *testing.T) {
	source, target, src, dst, authority := migrationFixture(t, 80)
	m, err := OpenMigration(t.Context(), source, target, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	done, err := m.Step(t.Context())
	if err != nil || done {
		t.Fatal("first batch", done, err)
	}
	if m.State.Cursor == "" || m.State.Copied > 64 {
		t.Fatal("unbounded/missing checkpoint", m.State)
	}
	if _, err = OpenSnapshot(t.Context(), target, nil); err == nil {
		t.Fatal("partial migration readable")
	}
	restarted, err := OpenMigration(t.Context(), source, target, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.State.Cursor != m.State.Cursor {
		t.Fatal("checkpoint not resumed")
	}
	for {
		done, err = restarted.Step(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if restarted.State.Phase != MigrationVerified || restarted.State.Copied != 80 {
		t.Fatal(restarted.State)
	}
	if err = restarted.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = restarted.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := OpenSnapshot(t.Context(), target, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := migrationRows(t.Context(), snapshot)
	if err != nil || len(rows) != 80 {
		t.Fatal(len(rows), err)
	}
	for _, row := range rows {
		original, err := source.Get(t.Context(), row.Key)
		if err != nil {
			t.Fatal(err)
		}
		copied, err := snapshot.ReadMetadata(t.Context(), row.Entry)
		if err != nil || string(original) != string(copied) {
			t.Fatal("retention/metadata bytes changed", err)
		}
	}
	if err = restarted.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenSnapshot(t.Context(), target, nil); err == nil {
		t.Fatal("rolled-back destination active")
	}
}

func TestMigrationRejectsIsolationQualificationStaleProofAndMixedNamespace(t *testing.T) {
	source, target, src, dst, authority := migrationFixture(t, 1)
	overlap := dst
	overlap.Prefix = "old/new"
	if _, err := OpenMigration(t.Context(), source, target, src, overlap, authority); err == nil {
		t.Fatal("nested destination accepted")
	}
	if _, err := OpenMigration(t.Context(), source, storagetest.NewMemoryStore(), src, dst, authority); !errors.Is(err, storage.ErrAtomicCatalogUnqualified) {
		t.Fatal(err)
	}
	authority.valid = false
	if _, err := OpenMigration(t.Context(), source, target, src, dst, authority); err == nil {
		t.Fatal("writable legacy credentials accepted")
	}
	objects, err := target.List(t.Context(), "")
	if err != nil || len(objects) != 0 {
		t.Fatal("failed proof changed target")
	}
	authority.valid = true
	m, err := OpenMigration(t.Context(), source, target, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	authority.id = "different authority"
	if _, err = m.Step(t.Context()); err == nil {
		t.Fatal("changed authority accepted")
	}
	authority.id = m.State.Proof
	for {
		done, err := m.Step(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if err = target.Put(t.Context(), "sessions/claude/mixed/metadata.json", []byte("legacy")); err != nil {
		t.Fatal(err)
	}
	if err = m.Activate(t.Context()); err == nil {
		t.Fatal("mixed namespace activated")
	}
}

func TestMigrationMissingSourceAndChangedCatalogCannotActivateOrRollback(t *testing.T) {
	source, target, src, dst, authority := migrationFixture(t, 1)
	objects, err := source.List(t.Context(), "sessions/")
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objects {
		if !metadataKey(obj.Key) {
			if err = source.Delete(t.Context(), obj.Key); err != nil {
				t.Fatal(err)
			}
		}
	}
	m, err := OpenMigration(t.Context(), source, target, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Step(t.Context()); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("missing source accepted", err)
	}
	source, target, src, dst, authority = migrationFixture(t, 1)
	m, err = OpenMigration(t.Context(), source, target, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, e := m.Step(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	if err = m.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = m.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	w, err := New(target)
	if err != nil {
		t.Fatal(err)
	}
	mutation := mutation(t, w, "after-cutover")
	if _, err = w.Commit(t.Context(), mutation); err != nil {
		t.Fatal(err)
	}
	if err = m.Rollback(t.Context()); err == nil {
		t.Fatal("rollback discarded new publications")
	}
}

func TestMigrationCheckpointCASRejectsConcurrentStaleRunner(t *testing.T) {
	source, target, src, dst, authority := migrationFixture(t, 80)
	first, err := OpenMigration(t.Context(), source, target, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := OpenMigration(t.Context(), source, target, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = first.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	stale.State.Phase = MigrationVerifying
	if err = stale.save(t.Context()); err == nil {
		t.Fatal("stale checkpoint overwritten")
	}
	actual, err := MigrationCheckpoint(t.Context(), target)
	if err != nil || actual.Cursor != first.State.Cursor {
		t.Fatal("fresh checkpoint lost", err)
	}
}

func TestMigrationCopiesPreservedHistoryWithOriginalCaptureDates(t *testing.T) {
	source, target, src, dst, authority := migrationFixture(t, 0)
	id := "00000000-0000-0000-0000-000000000001"
	captured := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	active := []byte("synthetic current revision")
	past := []byte("synthetic preserved revision")
	makeRef := func(raw []byte) archive.SourceReference {
		sha := storage.SHA256Hex(raw)
		key, err := archive.SourceObjectKey(archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: id, NativeSessionID: id, ProjectID: "project", Capture: archive.SourceCapture{Harness: archive.Harness{Name: "codex"}, AdapterName: "codex", CapturedAt: captured}}, sha)
		if err != nil {
			t.Fatal(err)
		}
		if err = source.Put(t.Context(), key, raw); err != nil {
			t.Fatal(err)
		}
		return archive.SourceReference{Key: key, SHA256: sha, CompressedBytes: len(raw)}
	}
	metadata := archive.Metadata{SchemaVersion: archive.HistoryMetadataSchemaVersion, SessionID: id, NativeSessionID: id, Harness: archive.Harness{Name: "codex"}, ProjectID: "project", CapturedAt: captured, SourceBundle: makeRef(active), History: &archive.RevisionHistory{CurrentRevision: id, Preserved: []archive.RevisionReference{{RevisionID: "00000000-0000-0000-0000-000000000002", CapturedAt: captured.Add(-24 * time.Hour), Source: makeRef(past)}}}}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Put(t.Context(), key, raw); err != nil {
		t.Fatal(err)
	}
	m, err := OpenMigration(t.Context(), source, target, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, e := m.Step(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	if err = m.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = m.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := OpenSnapshot(t.Context(), target, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := snapshot.Find(t.Context(), key)
	if err != nil || entry == nil {
		t.Fatal(err)
	}
	if !entry.Summary.CapturedAt.Equal(captured) || !entry.Summary.History.Preserved[0].CapturedAt.Equal(captured.Add(-24*time.Hour)) {
		t.Fatal("history retention provenance changed")
	}
	for _, ref := range []archive.SourceReference{metadata.SourceBundle, metadata.History.Preserved[0].Source} {
		copied, e := target.Get(t.Context(), ref.Key)
		if e != nil || !storage.VerifySHA256(copied, ref.SHA256) {
			t.Fatal("missing preserved source", e)
		}
	}
}

func TestMigrationEndpointAliasesCannotEvadeIsolation(t *testing.T) {
	source, target, _, _, authority := migrationFixture(t, 1)
	original := destination.Config{Provider: "r2", Bucket: "private-bucket", Prefix: "old", R2AccountID: "private-account"}
	for _, cfg := range []destination.Config{
		{Provider: "R2", Bucket: "private-bucket", Prefix: "/old/", R2Endpoint: "https://private-account.r2.cloudflarestorage.com/", ArchiveFormat: destination.FormatCatalogV4},
		{Provider: " r2 ", Bucket: "private-bucket", Prefix: "old/nested", R2Endpoint: "https://PRIVATE-ACCOUNT.r2.cloudflarestorage.com", ArchiveFormat: destination.FormatCatalogV4},
		{Provider: "r2", Bucket: "private-bucket", Prefix: "different", R2Endpoint: "https://private-account.r2.cloudflarestorage.com/invalid", ArchiveFormat: destination.FormatCatalogV4},
	} {
		if _, err := OpenMigration(t.Context(), source, target, original, cfg, authority); err == nil {
			t.Fatal("aliased or invalid endpoint passed isolation", cfg.Prefix)
		}
		objects, err := target.List(t.Context(), "")
		if err != nil || len(objects) != 0 {
			t.Fatal("isolation refusal wrote target", objects, err)
		}
	}
}
