package reader

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func legacyRevision(t *testing.T, key string, data []byte, validator string) listingindex.Revision {
	t.Helper()
	r, err := listingindex.NewRevision(key, data, validator)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimPrefix(r.Key, listingindex.V3Prefix), "/")
	r.Key = listingindex.V2Prefix + parts[2] + "/" + parts[0] + "/" + parts[1] + "/" + strings.Join(parts[3:], "")
	r, err = listingindex.ParseRevision(r.Key)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func legacyPointer(r listingindex.Revision) string {
	parts := strings.Split(r.MetadataKey, "/")
	return "listing/by-session-v2/" + parts[1] + "/" + parts[2] + "/" + storage.SHA256Hex([]byte(r.Key))
}

// putRevisionFixture models the retired v2 writer only in migration tests.
func putRevisionFixture(ctx context.Context, store storage.ObjectStore, r listingindex.Revision) error {
	if strings.HasPrefix(r.Key, listingindex.V2Prefix) {
		if err := store.Put(ctx, legacyPointer(r), []byte(r.Key)); err != nil {
			return err
		}
		return store.Put(ctx, r.Key, nil)
	}
	return listingindex.PutRevision(ctx, store, r)
}

// Model the old pointer-first gap exactly: B completes after A's pointer PUT
// but before A's hint PUT. Successful v2 cleanup leaves A's hint pointerless.
func oldPointerGap(t *testing.T, ctx context.Context, s *storagetest.MemoryStore, key string, data []byte, validator string) {
	t.Helper()
	a := legacyRevision(t, key, data, validator)
	if err := s.Put(ctx, legacyPointer(a), []byte(a.Key)); err != nil {
		t.Fatal(err)
	}
	b := legacyRevision(t, key, data, validator)
	if err := putRevisionFixture(ctx, s, b); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.DeleteRevision(ctx, s, a); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, a.Key, nil); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.DeleteRevision(ctx, s, b); err != nil {
		t.Fatal(err)
	}
}

func TestRebuildAndRetentionReclaimLegacyPointerlessHints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storagetest.NewMemoryStore()
	key := putSession(t, s, "codex", "legacy-gap", baseTime)
	data, validator, err := s.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		oldPointerGap(t, ctx, s, key, data, validator)
	}
	hints, _ := s.List(ctx, listingindex.V2Prefix)
	pointers, _ := s.List(ctx, "listing/by-session-v2/")
	if len(hints) != 20 || len(pointers) != 0 {
		t.Fatalf("old gap not reproduced: hints=%d pointers=%d", len(hints), len(pointers))
	}
	if _, err := RebuildIndex(ctx, s, "sessions"); err != nil {
		t.Fatal(err)
	}
	hints, _ = s.List(ctx, listingindex.V2Prefix)
	current, _ := s.List(ctx, listingindex.V3Prefix)
	if len(hints) != 0 || len(current) != 1 {
		t.Fatalf("live migration did not converge: legacy=%d current=%d", len(hints), len(current))
	}
	scanned := false
	if _, err := ListRecent(ctx, s, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scanned = true }}); err != nil || scanned {
		t.Fatalf("coverage lost: scan=%v err=%v", scanned, err)
	}
	for range 20 {
		oldPointerGap(t, ctx, s, key, data, validator)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.DeleteSession(ctx, s, "codex", "legacy-gap"); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{listingindex.V2Prefix, listingindex.V3Prefix, "listing/by-session-v2/"} {
		remaining, _ := s.List(ctx, prefix)
		if len(remaining) != 0 {
			t.Fatalf("retention left %s artifacts: %d", prefix, len(remaining))
		}
	}
}

type migrationCountingStore struct {
	*opaqueListingStore
	deletes []string
}

func (s *migrationCountingStore) Delete(ctx context.Context, key string) error {
	s.deletes = append(s.deletes, key)
	return s.opaqueListingStore.Delete(ctx, key)
}

func TestMixedRevisionCleanupSharesOneBudgetAndResumes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := &migrationCountingStore{opaqueListingStore: newOpaqueListingStore()}
	key := putSession(t, s, "codex", "mixed", baseTime)
	data, validator, err := s.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		r, err := listingindex.NewRevision(key, data, validator)
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			r = legacyRevision(t, key, data, validator)
		}
		if err := putRevisionFixture(ctx, s, r); err != nil {
			t.Fatal(err)
		}
	}
	s.reset()
	s.deletes = nil
	if _, err := RebuildIndex(ctx, s, "sessions"); err == nil || !strings.Contains(err.Error(), "cleanup remains pending") {
		t.Fatalf("missing bounded remainder: %v", err)
	}
	hints, _ := listRevisionHeaders(ctx, s)
	if len(hints) != 9 {
		t.Fatalf("retired other than32 mixed candidates: remaining=%d", len(hints))
	}
	// Lexical order retires20 legacy pairs plus12 v3 hints:32 candidates,
	// exactly52 auxiliary DELETE calls, not a hidden32-per-format budget.
	if len(s.deletes) != 52 {
		t.Fatalf("cleanup DELETE calls=%d, want52", len(s.deletes))
	}
	for _, key := range s.deletes {
		if !strings.HasPrefix(key, "listing/") {
			t.Fatalf("cleanup deleted canonical/source %q", key)
		}
	}
	_, gets := s.counts()
	if len(gets) != 1 || gets[0] != key {
		t.Fatalf("hint cleanup downloaded bodies: %v", gets)
	}
	if _, err := RebuildIndex(ctx, s, "sessions"); err != nil {
		t.Fatal(err)
	}
	hints, _ = listRevisionHeaders(ctx, s)
	if len(hints) != 1 || !strings.HasPrefix(hints[0].Key, listingindex.V3Prefix) {
		t.Fatalf("retry did not converge: %v", hints)
	}
}

type singleHintInterleaveStore struct {
	*storagetest.MemoryStore
	beforeHint func()
	prefixes   []string
}

func (s *singleHintInterleaveStore) Put(ctx context.Context, key string, data []byte) error {
	if strings.HasPrefix(key, listingindex.V3Prefix) && s.beforeHint != nil {
		hook := s.beforeHint
		s.beforeHint = nil
		hook()
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func (s *singleHintInterleaveStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	s.prefixes = append(s.prefixes, prefix)
	return s.MemoryStore.List(ctx, prefix)
}

func TestSessionAddressedPublicationClosesPointerGapWithoutGlobalDiscovery(t *testing.T) {
	ctx := context.Background()
	s := &singleHintInterleaveStore{MemoryStore: storagetest.NewMemoryStore()}
	key := putSession(t, s, "codex", "single", baseTime)
	data, _, err := s.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		s.beforeHint = func() {
			if err := listingindex.PublishRevision(ctx, s.MemoryStore, key, data); err != nil {
				t.Fatal(err)
			}
		}
		if err := listingindex.PublishRevision(ctx, s, key, data); err != nil {
			t.Fatal(err)
		}
	}
	if err := listingindex.PublishRevision(ctx, s, key, data); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range s.prefixes {
		if prefix != listingindex.V3Prefix+"codex/single/" {
			t.Fatalf("normal publication globally discovered %q", prefix)
		}
	}
	hints, _ := s.MemoryStore.List(ctx, listingindex.V3Prefix)
	if len(hints) != 1 {
		t.Fatalf("successful overlaps did not converge: %d", len(hints))
	}
	pointers, _ := s.MemoryStore.List(ctx, "listing/by-session-v2/")
	if len(pointers) != 0 {
		t.Fatal("new publication recreated two-object ownership")
	}
}

func TestLegacyPartialPointerNeverDeletesUnrelatedClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storagetest.NewMemoryStore()
	other := putSession(t, s, "codex", "other", baseTime)
	data, validator, err := s.GetVersioned(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	r := legacyRevision(t, other, data, validator)
	if err := putRevisionFixture(ctx, s, r); err != nil {
		t.Fatal(err)
	}
	corrupt := "listing/by-session-v2/codex/target/" + fmt.Sprintf("%064x", 1)
	if err := s.Put(ctx, corrupt, []byte(r.Key)); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.DeleteSession(ctx, s, "codex", "target"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, r.Key); err != nil {
		t.Fatalf("followed unrelated pointer: %v", err)
	}
}

func TestLegacyPointerCannotClaimSessionAddressedHint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storagetest.NewMemoryStore()
	key := putSession(t, s, "codex", "target", baseTime)
	data, validator, err := s.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	r, err := listingindex.NewRevision(key, data, validator)
	if err != nil {
		t.Fatal(err)
	}
	if err := listingindex.PutRevision(ctx, s, r); err != nil {
		t.Fatal(err)
	}
	pointer := legacyPointer(r)
	if err := s.Put(ctx, pointer, []byte(r.Key)); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.RetireSnapshot(ctx, s, key, []storage.Object{{Key: pointer}}); err != nil {
		t.Fatal(err)
	}
	remaining, err := s.List(ctx, "listing/by-session-v2/")
	if err != nil || len(remaining) != 0 {
		t.Fatalf("legacy pointer survived successful cleanup: %v %v", remaining, err)
	}
	if _, err := s.Get(ctx, r.Key); err != nil {
		t.Fatalf("legacy pointer authorized deletion of v3 hint: %v", err)
	}
	if got, err := s.Get(ctx, key); err != nil || string(got) != string(data) {
		t.Fatal("auxiliary cleanup changed canonical metadata")
	}
}

func TestMixedNamespacesShareOpaqueCoverageAndLegacyReaderFallsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newOpaqueListingStore()
	key := putSession(t, s, "codex", "namespaces", baseTime)
	data, validator, err := s.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	v2 := legacyRevision(t, key, data, validator)
	if err := putRevisionFixture(ctx, s, v2); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.PublishRevision(ctx, s, key, data); err != nil {
		t.Fatal(err)
	}
	s.reset()
	scan := false
	got, err := ListRecent(ctx, s, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scan = true }})
	_, gets := s.counts()
	if err != nil || scan || got.TotalMatched != 1 || len(got.Sessions) != 1 || len(gets) != 1 || gets[0] != key {
		t.Fatalf("mixed coverage: result=%+v scan=%v gets=%v err=%v", got, scan, gets, err)
	}
	if err := listingindex.DeleteRevision(ctx, s, v2); err != nil {
		t.Fatal(err)
	}
	objects, err := s.List(ctx, "sessions")
	if err != nil {
		t.Fatal(err)
	}
	// A prior v2 reader sees no v2 revision for this current canonical header.
	if _, _, err := selectListingRevisions(objects, map[RevisionID]listingindex.Revision{}, Filter{}, 1, ListOptions{}); err == nil {
		t.Fatal("legacy discovery falsely claims current coverage")
	}
}

func TestAbsentMixedCleanupAndPartialLegacyPointersResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storagetest.NewMemoryStore()
	key := putSession(t, s, "codex", "absent", baseTime)
	data, validator, err := s.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		r, err := listingindex.NewRevision(key, data, validator)
		if err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			r = legacyRevision(t, key, data, validator)
		}
		if err := putRevisionFixture(ctx, s, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.DeleteSession(ctx, s, "codex", "absent"); err == nil {
		t.Fatal("retention discarded unfinished cleanup")
	}
	hints, _ := listRevisionHeaders(ctx, s)
	if len(hints) != 8 {
		t.Fatalf("retention budget doubled across formats: %d", len(hints))
	}
	partial := legacyRevision(t, key, data, validator)
	if err := s.Put(ctx, legacyPointer(partial), []byte(partial.Key)); err != nil {
		t.Fatal(err)
	}
	if _, err := RebuildIndex(ctx, s, "sessions"); err != nil {
		t.Fatal(err)
	}
	hints, _ = listRevisionHeaders(ctx, s)
	pointers, _ := s.List(ctx, "listing/by-session-v2/")
	if len(hints) != 0 || len(pointers) != 0 {
		t.Fatalf("canonical-absent rebuild left artifacts: hints=%d pointers=%d", len(hints), len(pointers))
	}
}

type filesystemListingStore struct {
	*opaqueListingStore
}

func (s *filesystemListingStore) Put(ctx context.Context, key string, data []byte) error {
	for component := range strings.SplitSeq(key, "/") {
		if len(component) > 255 {
			return fmt.Errorf("object component exceeds 255 bytes")
		}
	}
	return s.opaqueListingStore.Put(ctx, key, data)
}

func TestRebuildKeepsIndexedCoverageWithFilesystemComponents(t *testing.T) {
	ctx := context.Background()
	s := &filesystemListingStore{newOpaqueListingStore()}
	key := putSession(t, s, "codex", "filesystem", baseTime)
	data, _, err := s.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.RepoKey = strings.Repeat("b", 64)
	data, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, key, data); err != nil {
		t.Fatal(err)
	}
	_, validator, err := s.GetVersioned(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	legacy := legacyRevision(t, key, data, validator)
	if err := putRevisionFixture(ctx, s.opaqueListingStore, legacy); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimPrefix(legacy.Key, listingindex.V2Prefix), "/")
	oldV3 := listingindex.V3Prefix + parts[1] + "/" + parts[2] + "/" + parts[0] + "/" + parts[3]
	if err := s.opaqueListingStore.Put(ctx, oldV3, nil); err != nil {
		t.Fatal(err)
	}
	for _, rebuild := range []bool{false, true} {
		if rebuild {
			if _, err := RebuildIndex(ctx, s, "sessions"); err != nil {
				t.Fatal(err)
			}
			hints, err := listRevisionHeaders(ctx, s)
			if err != nil || len(hints) != 1 || len(strings.Split(hints[0].Key, "/")) < 7 {
				t.Fatalf("hints=%v err=%v", hints, err)
			}
		}
		s.reset()
		scan := false
		got, err := ListRecent(ctx, s, "sessions", Filter{}, 1, ListOptions{CompatibilityScan: func(string) { scan = true }})
		_, gets := s.counts()
		if err != nil || scan || got.TotalMatched != 1 || len(got.Sessions) != 1 || len(gets) != 1 || gets[0] != key {
			t.Fatalf("rebuild=%v result=%+v scan=%v gets=%v err=%v", rebuild, got, scan, gets, err)
		}
	}
}
