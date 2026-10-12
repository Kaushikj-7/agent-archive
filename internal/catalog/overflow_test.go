package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func largeLinkedMutation(t *testing.T, w *Writer, id string) CatalogMutation {
	t.Helper()
	m := mutation(t, w, id)
	for i := range 2000 {
		m.Next.Summary.LinkedSessions = append(m.Next.Summary.LinkedSessions, archive.LinkedSessionReference{SessionID: fmt.Sprintf("linked-%04d", i), Relationship: "subagent", Status: archive.LinkedSessionPublished, ObservedAt: m.Next.Summary.CapturedAt})
	}
	raw, err := json.Marshal(m.Next.Summary)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= maxNodeBytes || len(raw) >= 32<<20 {
		t.Fatal("fixture did not exercise supported metadata overflow", len(raw))
	}
	var decoded archive.Metadata
	if err = json.Unmarshal(raw, &decoded); err != nil || len(decoded.LinkedSessions) != 2000 {
		t.Fatal("invalid metadata fixture", err)
	}
	m.Next.Metadata, err = w.PutImmutable(t.Context(), KindMetadata, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.verifyEntry(t.Context(), m.SessionKey, m.Next); err != nil {
		t.Fatal("metadata fixture authority invalid", err)
	}
	return m
}

func TestPublicationSupportsMetadataLargerThanTreeLeaf(t *testing.T) {
	w, _ := fixture(t)
	m := largeLinkedMutation(t, w, "large-linked")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatalf("valid 2000-link metadata publication failed: %v", err)
	}
}

type overflowReadStore struct {
	*qualifiedStore
	bodyReads atomic.Int64
}

func (s *overflowReadStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if strings.HasPrefix(key, "catalog-v4/metadata/") {
		s.bodyReads.Add(1)
	}
	return s.qualifiedStore.GetLimited(ctx, key, limit)
}

func TestOverflowSnapshotLazyResolutionAndOwnedProvenance(t *testing.T) {
	w, raw := fixture(t)
	m := largeLinkedMutation(t, w, "large-linked")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	counted := &overflowReadStore{qualifiedStore: raw}
	snapshot, err := OpenSnapshot(t.Context(), counted, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := snapshot.Query(t.Context(), Query{Index: IdentityIndex}, "", 1)
	if err != nil || len(page.Rows) != 1 {
		t.Fatal("lazy row", err)
	}
	row := page.Rows[0]
	if row.Entry.SummaryOverflow != overflowMetadata || len(row.Entry.Summary.LinkedSessions) != 0 || counted.bodyReads.Load() != 0 {
		t.Fatal("query eagerly resolved or truncated full authority")
	}
	forged := row
	forged.Entry.Revision = "foreign"
	if _, _, err = snapshot.ResolveRow(t.Context(), forged); err == nil || counted.bodyReads.Load() != 0 {
		t.Fatal("foreign row resolved outside captured root", err)
	}
	resolved, body, err := snapshot.ResolveRow(t.Context(), row)
	if err != nil || len(resolved.Entry.Summary.LinkedSessions) != 2000 || !storage.VerifySHA256(body, row.Entry.Metadata.SHA256) || counted.bodyReads.Load() != 1 {
		t.Fatal("full resolution", err)
	}
	mutated := resolved
	mutated.Entry.Revision = "foreign"
	if _, _, err = snapshot.ResolvedMetadata(t.Context(), mutated); err == nil {
		t.Fatal("mutated row gained body authority")
	}
	other, err := OpenSnapshot(t.Context(), counted, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = other.ResolvedMetadata(t.Context(), resolved); err == nil {
		t.Fatal("foreign snapshot gained body authority")
	}
	full, reused, err := snapshot.ResolvedMetadata(t.Context(), resolved)
	if err != nil || !reused || len(full.LinkedSessions) != 2000 {
		t.Fatal("selected authority reuse", err)
	}
	full.LinkedSessions[0].SessionID = "caller-mutated"
	if resolved.Entry.Summary.LinkedSessions[0].SessionID == "caller-mutated" {
		t.Fatal("returned body aliases row")
	}
	if _, reused, err = snapshot.ResolvedMetadata(t.Context(), resolved); err != nil || reused {
		t.Fatal("body provenance consumed twice", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err = snapshot.ResolveRow(ctx, row); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled overflow read accepted", err)
	}
	snapshot.started = time.Now().Add(-SnapshotLifetime)
	if _, _, err = snapshot.ResolveRow(t.Context(), row); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("expired overflow read accepted", err)
	}
	if counted.bodyReads.Load() != 1 {
		t.Fatal("provenance validation read body again")
	}
}

func TestOverflowMalformedEnvelopeAndReducedMutationRefuse(t *testing.T) {
	w, raw := fixture(t)
	m := largeLinkedMutation(t, w, "large-linked")
	compact := *compactEntry(m.Next)
	for _, alter := range []func(*CatalogEntry){
		func(e *CatalogEntry) { e.SummaryOverflow = "future" },
		func(e *CatalogEntry) { e.Metadata.Key = "mutable/metadata" },
		func(e *CatalogEntry) { e.Summary.LinkedSessions = m.Next.Summary.LinkedSessions },
	} {
		bad := compact
		alter(&bad)
		b, err := json.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}
		var decoded CatalogEntry
		if err = json.Unmarshal(b, &decoded); err == nil {
			t.Fatal("malformed overflow accepted")
		}
	}
	counted := &namespaceWriteStore{qualifiedStore: raw}
	writer, err := New(counted)
	if err != nil {
		t.Fatal(err)
	}
	m.Next = &compact
	if _, err = writer.Commit(t.Context(), m); err == nil || counted.writes != 0 {
		t.Fatal("reduced mutation obtained admission", err, counted.writes)
	}
	body, err := raw.Get(t.Context(), compact.Metadata.Key)
	if err != nil {
		t.Fatal(err)
	}
	compact.Summary.ProjectID = "different"
	if _, err = decodeEntryBody(compact, body); err == nil {
		t.Fatal("ref/projection mismatch accepted")
	}
}

func TestOverflowDeletionDeltaGCAndIndexBounds(t *testing.T) {
	w, raw := fixture(t)
	m := largeLinkedMutation(t, w, "large-linked")
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	snapshot, err := OpenSnapshot(t.Context(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	prior := snapshot.Root()
	delta, err := snapshot.Delta(t.Context(), ObjectRef{})
	if err != nil || len(delta.Changed) != 1 {
		t.Fatal("initial delta", err)
	}
	row, _, err := snapshot.ResolveRow(t.Context(), delta.Changed[0])
	if err != nil || len(row.Entry.Summary.LinkedSessions) != 2000 {
		t.Fatal("complete delta", err)
	}
	if err = w.Collect(t.Context(), heldBarrier{}); err != nil {
		t.Fatal("overflow GC", err)
	}
	if _, err = raw.Get(t.Context(), m.Next.Metadata.Key); err != nil {
		t.Fatal("GC removed full authority", err)
	}
	if _, err = raw.Get(t.Context(), m.Next.Summary.SourceBundle.Key); err != nil {
		t.Fatal("GC removed source", err)
	}
	revision, err := w.Commit(t.Context(), CatalogMutation{ID: "large-delete", SessionKey: m.SessionKey, ExpectedRevision: row.Entry.Revision})
	if err != nil || revision == "" {
		t.Fatal("delete", err)
	}
	fresh, err := OpenSnapshot(t.Context(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	delta, err = fresh.Delta(t.Context(), prior)
	if err != nil || len(delta.Removed) != 1 || delta.Removed[0] != m.SessionKey {
		t.Fatal("deletion delta", err)
	}
	indexed := mutation(t, w, "large-index")
	indexed.Next.Summary.ProjectID = strings.Repeat("p", 4096)
	body, err := json.Marshal(indexed.Next.Summary)
	if err != nil {
		t.Fatal(err)
	}
	indexed.Next.Metadata, err = w.PutImmutable(t.Context(), KindMetadata, body)
	if err != nil {
		t.Fatal(err)
	}
	before, etag, err := w.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(t.Context(), indexed); err == nil || !strings.Contains(err.Error(), "index key exceeds bound") {
		t.Fatal("oversized index key accepted", err)
	}
	after, nextETag, err := w.Head(t.Context())
	if err != nil || before.Identity != after.Identity || etag != nextETag {
		t.Fatal("unsupported key published head", err)
	}
}

func TestMigrationCopiesOverflowWithoutProjectionAuthority(t *testing.T) {
	source, target, src, dst, authority := migrationFixture(t, 0)
	sourceWriter, err := New(&qualifiedStore{source})
	if err != nil {
		t.Fatal(err)
	}
	original := largeLinkedMutation(t, sourceWriter, "large-migration")
	body, err := json.Marshal(original.Next.Summary)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Put(t.Context(), original.SessionKey, body); err != nil {
		t.Fatal(err)
	}
	migration, err := OpenMigration(t.Context(), source, target, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, err := migration.Step(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if err = migration.Verify(t.Context()); err != nil {
		t.Fatal("full migration oracle", err)
	}
	if err = migration.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = migration.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := OpenSnapshot(t.Context(), target, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := snapshot.Query(t.Context(), Query{Index: IdentityIndex}, "", 1)
	if err != nil || len(page.Rows) != 1 {
		t.Fatal("migrated query", err)
	}
	if page.Rows[0].Entry.SummaryOverflow != overflowMetadata {
		t.Fatal("large migration emitted inline body")
	}
	row, raw, err := snapshot.ResolveRow(t.Context(), page.Rows[0])
	if err != nil || !bytes.Equal(raw, body) || len(row.Entry.Summary.LinkedSessions) != 2000 || !row.Entry.Summary.CapturedAt.Equal(original.Next.Summary.CapturedAt) {
		t.Fatal("migration lost full metadata/retention", err)
	}
	sourceBody, err := source.Get(t.Context(), original.Next.Summary.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	copied, err := target.Get(t.Context(), original.Next.Summary.SourceBundle.Key)
	if err != nil || !bytes.Equal(sourceBody, copied) {
		t.Fatal("migration source authority", err)
	}
	if err = migration.writer.Collect(t.Context(), heldBarrier{}); err != nil {
		t.Fatal(err)
	}
	if _, err = target.Get(t.Context(), row.Entry.Metadata.Key); err != nil {
		t.Fatal("GC lost migrated overflow body", err)
	}
}

func TestProtocolNineCannotGainProtocolTenAdmission(t *testing.T) {
	w, current := fixture(t)
	valid := mutation(t, w, "old-head")
	if _, err := w.Commit(t.Context(), valid); err != nil {
		t.Fatal(err)
	}
	head, _, err := w.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	head.Protocol = 9
	encoded, err := json.Marshal(head)
	if err != nil {
		t.Fatal(err)
	}
	if err = current.Put(t.Context(), HeadKey, encoded); err != nil {
		t.Fatal(err)
	}
	if _, _, err = w.Head(t.Context()); err == nil {
		t.Fatal("old head accepted")
	}
	_, raw := fixture(t)
	counted := &namespaceWriteStore{qualifiedStore: raw}
	adapter, err := Wrap(counted)
	if err != nil {
		t.Fatal(err)
	}
	old := admissions{Protocol: 9, Generation: 1, Mode: admissionCandidate, Owners: map[string]admission{}}
	body, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err = raw.Put(t.Context(), CoordinatorKey, body); err != nil {
		t.Fatal(err)
	}
	if err = adapter.Put(t.Context(), "sessions/claude/old/source."+storage.SHA256Hex([]byte("source"))+".jsonl.gz", []byte("source")); err == nil || counted.writes != 0 {
		t.Fatal("old protocol admitted source", err, counted.writes)
	}
	source, target, src, dst, authority := migrationFixture(t, 0)
	migration, err := OpenMigration(t.Context(), source, target, src, dst, authority)
	if err != nil {
		t.Fatal(err)
	}
	state := migration.State
	if err = state.validate(); err != nil {
		t.Fatal("current checkpoint fixture invalid", err)
	}
	state.Protocol = 9
	if err = state.validate(); err == nil {
		t.Fatal("old checkpoint accepted")
	}
}

func TestOverflowParentCountersRebaseAndReceipt(t *testing.T) {
	w, raw := fixture(t)
	parent := largeLinkedMutation(t, w, "overflow-parent")
	if _, err := w.Commit(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	activateFixture(t, w)
	before, err := OpenSnapshot(t.Context(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	children := make([]CatalogMutation, 2)
	for i := range children {
		child := mutation(t, w, fmt.Sprintf("overflow-child-%d", i))
		child.Next.Summary.ParentSessionID = parent.Next.Summary.SessionID
		children[i] = replaceFixtureBody(t, w, child)
	}
	start := make(chan struct{})
	results := make(chan error, len(children))
	for _, child := range children {
		go func(m CatalogMutation) { <-start; _, err := w.Commit(t.Context(), m); results <- err }(child)
	}
	close(start)
	for range children {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	for _, child := range children {
		if _, err := w.Commit(t.Context(), child); err != nil {
			t.Fatal("receipt retry", err)
		}
	}
	after, err := OpenSnapshot(t.Context(), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := after.Find(t.Context(), parent.SessionKey)
	if err != nil || entry == nil || entry.OrdinaryChildren != 2 || entry.ReplayChildren != 0 || entry.SummaryOverflow != overflowMetadata {
		t.Fatal("overflow counter rebase", entry, err)
	}
	resolved, _, err := after.ResolveRow(t.Context(), Row{Key: parent.SessionKey, Entry: *entry})
	if err != nil || len(resolved.Entry.Summary.LinkedSessions) != 2000 || resolved.Entry.Metadata != parent.Next.Metadata {
		t.Fatal("counter update changed body authority", err)
	}
	delta, err := after.Delta(t.Context(), before.Root())
	if err != nil || len(delta.Changed) != 3 {
		t.Fatal("counter-only update absent from delta", len(delta.Changed), err)
	}
	prefix := ChildPrefix(parent.Next.Summary.Harness.Name, parent.Next.Summary.SessionID, false)
	count, err := after.Count(t.Context(), Query{Index: ProjectIndex, Lower: prefix + "0", Upper: prefix + ":"})
	if err != nil || count != entry.OrdinaryChildren {
		t.Fatal("overflow counter differs from indexed oracle", count, err)
	}
}

func TestOverflowLostAcknowledgmentRetainsFullAuthority(t *testing.T) {
	base := &qualifiedStore{storagetest.NewMemoryStore()}
	provider := &lostStore{qualifiedStore: base}
	writer, err := New(provider)
	if err != nil {
		t.Fatal(err)
	}
	mutation := largeLinkedMutation(t, writer, "overflow-lost")
	if _, err = writer.Commit(t.Context(), mutation); err != nil {
		t.Fatal("lost acknowledgment readback", err)
	}
	restarted, err := New(provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.Commit(t.Context(), mutation); err != nil {
		t.Fatal("receipt retry", err)
	}
	activateFixture(t, restarted)
	snapshot, err := OpenSnapshot(t.Context(), provider, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := snapshot.Find(t.Context(), mutation.SessionKey)
	if err != nil || entry == nil {
		t.Fatal(err)
	}
	row, _, err := snapshot.ResolveRow(t.Context(), Row{Key: mutation.SessionKey, Entry: *entry})
	if err != nil || len(row.Entry.Summary.LinkedSessions) != 2000 {
		t.Fatal("lost acknowledgment changed full metadata", err)
	}
}
