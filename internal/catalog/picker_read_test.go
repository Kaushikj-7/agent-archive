package catalog

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/testutil/fixturecost"
)

var pickerReaderSeed struct {
	once     sync.Once
	objects  storagetest.MemorySnapshot
	metadata []byte
}

func pickerReaderFixture(t *testing.T) (*Snapshot, *Writer, *qualifiedStore, []archive.Metadata) {
	t.Helper()
	// Exercise the real publication and complete upgrade once. Retain only
	// immutable bytes; every caller gets independently owned mutable authority,
	// bodies, metadata, node cache, and a newly started snapshot lifetime.
	pickerReaderSeed.once.Do(func() {
		_, _, store, metadata := buildPickerReaderFixture(t)
		freeze := fixturecost.Start(t, "picker_reader", "freeze", nil)
		seed, err := storagetest.SnapshotMemoryStore(t.Context(), store.MemoryStore)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		// Publish only a fully constructed seed. sync.Once also completes when
		// Fatal exits its initializer, so later callers must refuse that failure.
		pickerReaderSeed.objects = seed
		pickerReaderSeed.metadata = raw
		freeze.Finish()
	})
	if len(pickerReaderSeed.metadata) == 0 {
		t.Fatal("picker reader fixture initialization did not complete")
	}
	clone := fixturecost.Start(t, "picker_reader", "clone", map[string]int64{"store_clones": 1})
	clone.Add("store_clones", 1)
	memory, err := pickerReaderSeed.objects.Clone(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	store := &qualifiedStore{memory}
	w, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	var metadata []archive.Metadata
	if err := json.Unmarshal(pickerReaderSeed.metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSnapshot(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	clone.Add("rows", int64(len(metadata)))
	clone.Finish()
	return s, w, store, metadata
}

func buildPickerReaderFixture(t *testing.T) (*Snapshot, *Writer, *qualifiedStore, []archive.Metadata) {
	t.Helper()
	return buildPickerReaderFixtureSize(t, 96)
}

func buildPickerReaderFixtureSize(t *testing.T, size int) (*Snapshot, *Writer, *qualifiedStore, []archive.Metadata) {
	t.Helper()
	phase := fixturecost.Start(t, "picker_reader", "publication", map[string]int64{"commits": int64(size), "constructors": 1})
	phase.Add("commits", 0)
	phase.Add("constructors", 1)
	w, store := fixture(t)
	var metadata []archive.Metadata
	for i := range size {
		id := fmt.Sprintf("picker-%03d", i)
		m := mutation(t, w, id)
		harness := []string{"claude", "Claude-Code", "codex", "synthetic-unknown"}[i%4]
		m.Next.Summary.SourceBundle.Key = strings.Replace(m.Next.Summary.SourceBundle.Key, "sessions/claude/", "sessions/"+harness+"/", 1)
		if err := w.store.Put(t.Context(), m.Next.Summary.SourceBundle.Key, []byte("synthetic source "+id)); err != nil {
			t.Fatal(err)
		}
		m = pickerMetadata(t, w, m, func(md *archive.Metadata) {
			md.Harness.Name = []string{"claude", "Claude-Code", "codex", "synthetic-unknown"}[i%4]
			md.ProjectID = fmt.Sprintf("project-%d", i%5)
			if i%3 == 0 {
				md.RepoKey = testPickerRepo('a')
			}
			if i%3 == 1 {
				md.RepoKey = testPickerRepo('b')
			}
			md.CapturedAt = md.CapturedAt.Add(time.Duration(i%7) * time.Second)
			ended := md.CapturedAt.Add(time.Duration(i%11) * time.Second)
			md.EndedAt = &ended
			if i%4 == 0 {
				zero := 0
				md.Counts.Turns = &zero
			}
			if i%13 == 0 {
				md.ParentSessionID = "parent"
			}
			if i%17 == 0 {
				md.Replay = &archive.Replay{}
			}
			if i == 7 {
				md.Title = strings.Repeat("overflow", 2000)
			}
		})
		m.SessionKey, _ = archive.MetadataObjectKey(m.Next.Summary.Harness.Name, m.Next.Summary.SessionID)
		metadata = append(metadata, m.Next.Summary)
		phase.Add("commits", 1)
		if _, err := w.Commit(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	phase.Add("rows", int64(size))
	phase.Finish()
	completePickerUpgrade(t, store)
	s, err := OpenSnapshot(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, w, store, metadata
}

func testPickerRepo(c byte) string {
	return archive.RepoKey("https://example.com/" + string(c) + ".git")
}

func TestPickerReaderFixturesOwnAuthorityBodiesAndMetadata(t *testing.T) {
	first, _, changed, metadata := pickerReaderFixture(t)
	second, _, independent, otherMetadata := pickerReaderFixture(t)
	if first == second || first.cache == second.cache || first.nonce == second.nonce || first.secret == second.secret {
		t.Fatal("reader fixtures share snapshot authority or cache")
	}
	q := PickerQuery{Scope: PickerScope{All: true}, Membership: PickerBrowse, Limit: 1}
	page, err := second.PickerPage(t.Context(), q, "")
	if err != nil || len(page.Rows) != 1 {
		t.Fatalf("independent page %+v: %v", page, err)
	}
	keys := []string{HeadKey, CoordinatorKey, page.Rows[0].Row.Metadata.Key}
	for i, key := range keys {
		if i == 1 {
			if err := changed.Delete(t.Context(), key); err != nil {
				t.Fatal(err)
			}
		} else if err := changed.Put(t.Context(), key, []byte("modified fixture object")); err != nil {
			t.Fatal(err)
		}
	}
	metadata[0].Title = "modified caller metadata"
	*metadata[0].Counts.Turns = 99
	first.started = time.Now().Add(-SnapshotLifetime)
	first.cache.bytes = map[ObjectRef][]byte{first.head.Project: []byte("modified cache")}
	third, _, fresh, freshMetadata := pickerReaderFixture(t)
	seed, err := pickerReaderSeed.objects.Clone(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, store := range []*qualifiedStore{independent, fresh} {
		for _, key := range keys {
			raw, err := store.Get(t.Context(), key)
			expected, seedErr := seed.Get(t.Context(), key)
			if err != nil || seedErr != nil || !bytes.Equal(raw, expected) {
				t.Fatalf("fixture mutation crossed store boundary for %s: %v", key, err)
			}
		}
	}
	for _, ms := range [][]archive.Metadata{otherMetadata, freshMetadata} {
		if ms[0].Title == metadata[0].Title || *ms[0].Counts.Turns != 0 {
			t.Fatal("fixture mutation crossed metadata boundary")
		}
	}
	if err := third.ValidateRead(t.Context()); err != nil {
		t.Fatal("fixture mutation crossed lifetime boundary", err)
	}
	if _, err := second.HydratePicker(t.Context(), page.Rows[0].Reference); err != nil {
		t.Fatal("fixture mutation crossed selected-body authority boundary", err)
	}
}

func pickerOracle(ms []archive.Metadata, q PickerQuery) []string {
	var filtered []archive.Metadata
	for _, m := range ms {
		if m.IsChild() || m.Replay != nil || q.Membership == PickerHandoff && m.Counts.Turns != nil && *m.Counts.Turns == 0 {
			continue
		}
		if q.Harness != "" && archive.CanonicalHarness(m.Harness.Name) != archive.CanonicalHarness(q.Harness) {
			continue
		}
		match := q.Scope.All
		if !match {
			if q.Scope.RepoKey != "" && m.RepoKey != "" {
				match = q.Scope.RepoKey == m.RepoKey
			} else {
				match = slices.Contains(q.Scope.ProjectIDs, m.ProjectID)
			}
		}
		if match {
			filtered = append(filtered, m)
		}
	}
	slices.SortFunc(filtered, func(a, b archive.Metadata) int {
		// This explicitly reproduces the exhaustive archive activity/capture/raw
		// harness/session order, independently from pickerBefore and key encoding.
		if !a.EndedAt.Equal(*b.EndedAt) {
			if a.EndedAt.After(*b.EndedAt) {
				return -1
			}
			return 1
		}
		if !a.CapturedAt.Equal(b.CapturedAt) {
			if a.CapturedAt.After(b.CapturedAt) {
				return -1
			}
			return 1
		}
		if n := strings.Compare(a.Harness.Name, b.Harness.Name); n != 0 {
			return n
		}
		return strings.Compare(a.SessionID, b.SessionID)
	})
	var ids []string
	for _, m := range filtered {
		ids = append(ids, m.SessionID)
	}
	return ids
}

func TestPickerPagesMatchCompleteRepositoryOracle(t *testing.T) {
	s, _, _, ms := pickerReaderFixture(t)
	scopes := []PickerScope{{All: true}, {RepoKey: testPickerRepo('a'), ProjectIDs: []string{"project-1", "project-3", "project-1"}}, {ProjectIDs: []string{"project-0", "project-4"}}, {RepoKey: testPickerRepo('c')}, {}}
	for _, scope := range scopes {
		for _, member := range []PickerMembership{PickerBrowse, PickerHandoff} {
			for _, harness := range []string{"", "CLAUDE-CODE", "codex", "synthetic-unknown", "absent"} {
				for _, limit := range []int{1, 7, 100} {
					q := PickerQuery{Scope: scope, Membership: member, Harness: harness, Limit: limit}
					expected := pickerOracle(ms, q)
					var actual []string
					next := ""
					for {
						page, err := s.PickerPage(t.Context(), q, next)
						if err != nil {
							t.Fatal(err)
						}
						if page.Total != uint64(len(expected)) || page.Empty != (len(expected) == 0) {
							t.Fatalf("counts %+v expected %d", page, len(expected))
						}
						for _, item := range page.Rows {
							actual = append(actual, item.Row.SessionID)
						}
						if page.More != (page.Next != "") {
							t.Fatal("more lacks continuation")
						}
						if !page.More {
							break
						}
						next = page.Next
					}
					if !slices.Equal(actual, expected) {
						t.Fatalf("scope %+v member %s limit %d: got %v want %v", scope, member, limit, actual, expected)
					}
				}
			}
		}
	}
}

func TestPickerLookupIsUnfilteredAndHydrationHasVerifiedProvenance(t *testing.T) {
	s, _, _, ms := pickerReaderFixture(t)
	ids := []PickerIdentity{}
	for _, i := range []int{0, 7, 13, 17, 95} {
		ids = append(ids, PickerIdentity{Harness: ms[i].Harness.Name, SessionID: ms[i].SessionID})
	}
	ids = append(ids, PickerIdentity{Harness: "claude", SessionID: "absent"})
	batch, err := s.PickerLookup(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Cost.HeadObservations != 1 || batch.Cost.CoordinatorReads != 1 {
		t.Fatal(batch.Cost)
	}
	for i, r := range batch.Results {
		if r.Found != (i < len(ids)-1) {
			t.Fatal(r)
		}
	}
	item := batch.Results[1].Item
	item.Row.Title = "caller presentation change"
	if item.Row.PromptCount != nil {
		*item.Row.PromptCount = 9
	}
	h, err := s.HydratePicker(t.Context(), item.Reference)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Row.Entry.Summary.Title) <= PickerLabelBytes || len(h.Raw) == 0 {
		t.Fatal("hydration returned projection")
	}
	body, ok, err := s.ResolvedMetadata(t.Context(), h.Row)
	if err != nil || !ok || body.SessionID != ids[1].SessionID {
		t.Fatalf("provenance %v %v", ok, err)
	}
	if _, err = s.HydratePicker(t.Context(), PickerReference{}); err == nil {
		t.Fatal("forged reference accepted")
	}
}

func TestPickerCursorRejectsMutationScopeForeignLifetimeAndHead(t *testing.T) {
	s, w, store, _ := pickerReaderFixture(t)
	q := PickerQuery{Scope: PickerScope{All: true}, Membership: PickerBrowse, Limit: 2}
	p, err := s.PickerPage(t.Context(), q, "")
	if err != nil {
		t.Fatal(err)
	}
	wrong := q
	wrong.Membership = PickerHandoff
	if _, err = s.PickerPage(t.Context(), wrong, p.Next); !errors.Is(err, ErrStaleCursor) {
		t.Fatal(err)
	}
	wrong = q
	wrong.Limit = 3
	if _, err = s.PickerPage(t.Context(), wrong, p.Next); !errors.Is(err, ErrStaleCursor) {
		t.Fatal(err)
	}
	encoded, _ := base64.RawURLEncoding.DecodeString(p.Next)
	reader, err := zlib.NewReader(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	var token pickerCursor
	if err = json.Unmarshal(raw, &token); err != nil {
		t.Fatal(err)
	}
	token.Head.Schema++
	raw, _ = json.Marshal(token)
	var compressed bytes.Buffer
	encoder := zlib.NewWriter(&compressed)
	if _, err = encoder.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err = encoder.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PickerPage(t.Context(), q, base64.RawURLEncoding.EncodeToString(compressed.Bytes())); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("tampered format", err)
	}
	other, err := OpenSnapshot(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.PickerPage(t.Context(), q, p.Next); !errors.Is(err, ErrStaleCursor) {
		t.Fatal("foreign", err)
	}
	for _, bad := range []string{"%", strings.Repeat("a", (64<<10)+1)} {
		if _, err = s.PickerPage(t.Context(), q, bad); !errors.Is(err, ErrStaleCursor) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.PickerPage(ctx, q, ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s.started = time.Now().Add(-SnapshotLifetime)
	if _, err = s.PickerPage(t.Context(), q, p.Next); !errors.Is(err, ErrStaleCursor) {
		t.Fatal(err)
	}
	s.started = time.Now()
	m := mutation(t, w, "changed-head")
	if _, err = w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PickerPage(t.Context(), q, p.Next); !errors.Is(err, ErrStaleCursor) {
		t.Fatal(err)
	}
	if _, err = s.HydratePicker(t.Context(), p.Rows[0].Reference); !errors.Is(err, ErrStaleCursor) {
		t.Fatal(err)
	}
}

func TestPickerUnsupportedAndBudgetAreNotEmptyProofs(t *testing.T) {
	const invalidMembership PickerMembership = "arbitrary"
	var s *Snapshot
	var metadata []archive.Metadata
	if testing.Short() {
		// The first row is a child/replay; the second is an ordinary root.
		// This minimal prefix still uses real publication and guarded upgrade.
		// Keep the full 96-row fixture for non-short qualification and every
		// pagination/cursor test that calls pickerReaderFixture.
		s, _, _, metadata = buildPickerReaderFixtureSize(t, 2)
	} else {
		s, _, _, metadata = pickerReaderFixture(t)
	}
	for _, q := range []PickerQuery{{Scope: PickerScope{All: true}, Membership: PickerBrowse, Limit: 101}, {Scope: PickerScope{All: true, RepoKey: testPickerRepo('a')}, Membership: PickerBrowse, Limit: 5}, {Membership: invalidMembership, Limit: 5}} {
		p, err := s.PickerPage(t.Context(), q, "")
		if !errors.Is(err, ErrPickerUnsupported) || p.Empty {
			t.Fatalf("%+v %v", p, err)
		}
	}
	q := PickerQuery{Scope: PickerScope{All: true}, Membership: PickerBrowse, Limit: 5, Budget: PickerBudget{Nodes: 1}}
	p, err := s.PickerPage(t.Context(), q, "")
	if !errors.Is(err, ErrPickerBudget) || p.Empty || len(p.Rows) > 0 {
		t.Fatalf("%+v %v", p, err)
	}
	q.Budget = PickerBudget{Bytes: 1}
	if _, err = s.PickerPage(t.Context(), q, ""); !errors.Is(err, ErrPickerBudget) {
		t.Fatal(err)
	}
	// Count and candidate traversal each charge nodes, including cache hits.
	// Prove the smaller fixture has a real nonempty answer that exceeds both
	// deliberately exhausted budgets, rather than relying on an empty tree.
	q.Budget = PickerBudget{}
	p, err = s.PickerPage(t.Context(), q, "")
	if err != nil || p.Empty || len(p.Rows) == 0 || p.Cost.Nodes <= 1 || p.Cost.Bytes <= 1 {
		t.Fatalf("fixture no longer exercises budget exhaustion: %+v %v", p, err)
	}
	t.Logf("real fixture rows=%d; successful query nodes=%d bytes=%d", len(metadata), p.Cost.Nodes, p.Cost.Bytes)
}

func TestPickerQueryAndCursorPreserveOriginalJSONFieldNames(t *testing.T) {
	q := PickerQuery{Scope: PickerScope{All: true}, Membership: PickerBrowse, Limit: 2}
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	const original = `{"Scope":{"All":true,"RepoKey":"","ProjectIDs":null},"Membership":"browse","Harness":"","Limit":2,"Budget":{"Nodes":0,"Bytes":0}}`
	if string(raw) != original {
		t.Fatalf("query changed original cursor encoding: %s", raw)
	}
	raw, err = json.Marshal(pickerCursor{Query: q})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 5 || string(fields["Query"]) != original {
		t.Fatalf("cursor changed original envelope: %s", raw)
	}
	for _, name := range []string{"Nonce", "Head", "Query", "After", "Seal"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("cursor lost original field %s: %s", name, raw)
		}
	}
}

func publishPickerHarness(t *testing.T, w *Writer, id, harness string, edit func(*archive.Metadata)) CatalogMutation {
	t.Helper()
	m := mutation(t, w, id)
	m.ID = "publish-" + harness + "-" + id
	source := []byte("synthetic source " + id)
	m.Next.Summary.SourceBundle.Key = strings.Replace(m.Next.Summary.SourceBundle.Key, "sessions/claude/", "sessions/"+harness+"/", 1)
	if err := w.store.Put(t.Context(), m.Next.Summary.SourceBundle.Key, source); err != nil {
		t.Fatal(err)
	}
	m = pickerMetadata(t, w, m, func(a *archive.Metadata) {
		a.Harness.Name = harness
		if edit != nil {
			edit(a)
		}
	})
	m.SessionKey, _ = archive.MetadataObjectKey(harness, id)
	if _, err := w.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPickerCanonicalLookupPreservesAliasReferenceAndRejectsAmbiguity(t *testing.T) {
	_, w, store, _ := pickerReaderFixture(t)
	alias := publishPickerHarness(t, w, "alias-choice", "Claude-Code", func(a *archive.Metadata) {
		a.ParentSessionID = "parent"
		n := 0
		a.Counts.Turns = &n
		a.Replay = &archive.Replay{}
	})
	s, err := OpenSnapshot(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := s.PickerLookup(t.Context(), []PickerIdentity{{Harness: "CLAUDE", SessionID: "alias-choice"}})
	if err != nil {
		t.Fatal(err)
	}
	r := batch.Results[0]
	if !r.Found || r.Identity.Harness != "claude" || r.Item.Row.SessionKey != alias.SessionKey || r.Item.Row.Root || !r.Item.Row.Replay || r.Item.Row.PromptState != PickerPromptsZero {
		t.Fatalf("wrong alias classification %+v", r)
	}
	h, err := s.HydratePicker(t.Context(), r.Item.Reference)
	if err != nil {
		t.Fatal(err)
	}
	body, ok, err := s.ResolvedMetadata(t.Context(), h.Row)
	if err != nil || !ok || body.Harness.Name != "Claude-Code" || body.SourceBundle.Key != alias.Next.Summary.SourceBundle.Key {
		t.Fatalf("physical selection %v %v %+v", ok, err, body)
	}
	other, err := OpenSnapshot(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.HydratePicker(t.Context(), r.Item.Reference); err == nil {
		t.Fatal("foreign reference accepted")
	}
	canonical := publishPickerHarness(t, w, "alias-choice", "claude", nil)
	s, err = OpenSnapshot(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	batch, err = s.PickerLookup(t.Context(), []PickerIdentity{{Harness: "claude-code", SessionID: "alias-choice"}})
	if !errors.Is(err, ErrPickerIdentityAmbiguous) || len(batch.Results) > 0 {
		t.Fatalf("ambiguous aliases selected arbitrarily %+v %v", batch, err)
	}
	_, revision, err := w.Find(t.Context(), canonical.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Commit(t.Context(), CatalogMutation{ID: "delete-canonical-alias", SessionKey: canonical.SessionKey, ExpectedRevision: revision}); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSnapshot(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	batch, err = s.PickerLookup(t.Context(), []PickerIdentity{{Harness: "claude", SessionID: "alias-choice"}})
	if err != nil || !batch.Results[0].Found || batch.Results[0].Item.Row.SessionKey != alias.SessionKey {
		t.Fatalf("delete alias %v %+v", err, batch)
	}
}

func TestPickerRowsCarryIndexedParentCountsWithoutChildEnumeration(t *testing.T) {
	_, w, store, _ := pickerReaderFixture(t)
	publishPickerHarness(t, w, "actual-parent", "claude", nil)
	publishPickerHarness(t, w, "ordinary-child", "claude", func(a *archive.Metadata) { a.ParentSessionID = "actual-parent" })
	publishPickerHarness(t, w, "replay-child", "claude", func(a *archive.Metadata) { a.ParentSessionID = "actual-parent"; a.Replay = &archive.Replay{} })
	meter := &pickerMeter{qualifiedStore: store}
	s, err := OpenSnapshot(t.Context(), meter, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.PickerPage(t.Context(), PickerQuery{Scope: PickerScope{All: true}, Membership: PickerBrowse, Harness: "claude", Limit: 100}, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range page.Rows {
		if item.Row.SessionID == "actual-parent" {
			found = true
			if item.Row.OrdinaryChildren != 1 || item.Row.ReplayChildren != 1 {
				t.Fatalf("wrong indexed parent counts %+v", item.Row)
			}
		}
		if slices.Contains([]string{"ordinary-child", "replay-child"}, item.Row.SessionID) {
			t.Fatal("child appeared in root page")
		}
	}
	if !found || meter.metadata != 0 || meter.lists != 0 {
		t.Fatalf("parent found=%v meter=%+v", found, meter)
	}
}

func TestPickerScopeRangeCeilingCountsUniqueMemberships(t *testing.T) {
	s, _, _, _ := pickerReaderFixture(t)
	ids := make([]string, 32)
	for i := range ids {
		ids[i] = fmt.Sprintf("project-%d", i)
	}
	q := PickerQuery{Scope: PickerScope{ProjectIDs: append(slices.Clone(ids), ids...)}, Membership: PickerBrowse, Limit: 1}
	p, err := s.PickerPage(t.Context(), q, "")
	if err != nil || len(p.Rows) != 1 || !p.More {
		t.Fatalf("32 actual ranges %+v %v", p, err)
	}
	reordered := q
	slices.Reverse(reordered.Scope.ProjectIDs)
	if _, err = s.PickerPage(t.Context(), reordered, p.Next); err != nil {
		t.Fatal("normalized request changed cursor binding", err)
	}
	q.Scope.ProjectIDs = append(q.Scope.ProjectIDs, "one-more-distinct")
	if _, err = s.PickerPage(t.Context(), q, ""); !errors.Is(err, ErrPickerUnsupported) {
		t.Fatal("33 actual ranges accepted", err)
	}
	q.Scope = PickerScope{RepoKey: testPickerRepo('a'), ProjectIDs: ids}
	if _, err = s.PickerPage(t.Context(), q, ""); !errors.Is(err, ErrPickerUnsupported) {
		t.Fatal("repository adds an actual range", err)
	}
}

// finalPickerContext changes cancellation or lifetime on the last validation
// observed in an otherwise identical warm operation. It models authority
// expiring after all I/O has finished but before the result is returned.
type finalPickerContext struct {
	context.Context
	calls  int
	stop   int
	onStop func()
}

func (c *finalPickerContext) Err() error {
	c.calls++
	if c.calls == c.stop && c.onStop != nil {
		c.onStop()
	}
	return c.Context.Err()
}

func TestPickerFinalValidationDiscardsAllResults(t *testing.T) {
	s, _, _, _ := pickerReaderFixture(t)
	q := PickerQuery{Scope: PickerScope{All: true}, Membership: PickerBrowse, Limit: 2}
	p, err := s.PickerPage(t.Context(), q, "")
	if err != nil {
		t.Fatal(err)
	}
	operations := []struct {
		name string
		run  func(context.Context) (any, error)
	}{
		{"page", func(ctx context.Context) (any, error) { return s.PickerPage(ctx, q, "") }},
		{"empty-page", func(ctx context.Context) (any, error) {
			empty := q
			empty.Scope = PickerScope{}
			return s.PickerPage(ctx, empty, "")
		}},
		{"lookup", func(ctx context.Context) (any, error) {
			return s.PickerLookup(ctx, []PickerIdentity{{Harness: p.Rows[0].Row.Harness, SessionID: p.Rows[0].Row.SessionID}})
		}},
		{"hydration", func(ctx context.Context) (any, error) { return s.HydratePicker(ctx, p.Rows[0].Reference) }},
	}
	for _, op := range operations {
		for _, expired := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/expired=%v", op.name, expired), func(t *testing.T) {
				s.started = time.Now()
				// Warm all immutable paths before measuring the validation count.
				if _, err := op.run(t.Context()); err != nil {
					t.Fatal(err)
				}
				probe := &finalPickerContext{Context: t.Context()}
				if _, err := op.run(probe); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				want := error(context.Canceled)
				hook := cancel
				if expired {
					want = ErrStaleCursor
					hook = func() { s.started = time.Now().Add(-SnapshotLifetime) }
				}
				final := &finalPickerContext{Context: ctx, stop: probe.calls, onStop: hook}
				result, err := op.run(final)
				if !errors.Is(err, want) || !reflect.ValueOf(result).IsZero() || final.calls != probe.calls {
					t.Fatalf("final validation returned result=%+v error=%v calls=%d want=%d", result, err, final.calls, probe.calls)
				}
				s.started = time.Now()
			})
		}
	}
}

func TestPickerPrefixIdentityTiesMatchCompleteOrderAcrossRanges(t *testing.T) {
	w, store := fixture(t)
	at := time.Date(2026, 10, 1, 1, 2, 3, 4, time.UTC)
	var metadata []archive.Metadata
	for i, harness := range []string{"claude", "claude-code", "a", "a-", "a0", "z"} {
		for j, id := range []string{"a", "a-", "a0", "z"} {
			m := publishPickerHarness(t, w, id, harness, func(md *archive.Metadata) {
				md.CapturedAt = at
				md.EndedAt = &at
				md.ProjectID = fmt.Sprintf("tie-project-%d", (i+j)%3)
			})
			metadata = append(metadata, m.Next.Summary)
		}
	}
	completePickerUpgrade(t, store)
	s, err := OpenSnapshot(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Directly compare every pair, independently of the encoded membership key.
	for _, a := range metadata {
		for _, b := range metadata {
			ak, _ := archive.MetadataObjectKey(a.Harness.Name, a.SessionID)
			bk, _ := archive.MetadataObjectKey(b.Harness.Name, b.SessionID)
			left := PickerRow{Activity: at, CapturedAt: at, Harness: a.Harness.Name, SessionID: a.SessionID, SessionKey: ak}
			right := PickerRow{Activity: at, CapturedAt: at, Harness: b.Harness.Name, SessionID: b.SessionID, SessionKey: bk}
			want := a.Harness.Name < b.Harness.Name || a.Harness.Name == b.Harness.Name && a.SessionID < b.SessionID
			if pickerBefore(left, right) != want {
				t.Fatalf("tie comparison %s/%s vs %s/%s", a.Harness.Name, a.SessionID, b.Harness.Name, b.SessionID)
			}
		}
	}
	for _, scope := range []PickerScope{{All: true}, {ProjectIDs: []string{"tie-project-0", "tie-project-1", "tie-project-2"}}} {
		for _, limit := range []int{1, 5, 100} {
			q := PickerQuery{Scope: scope, Membership: PickerBrowse, Limit: limit}
			var actual []string
			for next := ""; ; {
				p, err := s.PickerPage(t.Context(), q, next)
				if err != nil {
					t.Fatal(err)
				}
				for _, item := range p.Rows {
					actual = append(actual, item.Row.Harness+"/"+item.Row.SessionID)
				}
				if !p.More {
					break
				}
				next = p.Next
			}
			ordered := slices.Clone(metadata)
			slices.SortFunc(ordered, func(a, b archive.Metadata) int {
				if n := strings.Compare(a.Harness.Name, b.Harness.Name); n != 0 {
					return n
				}
				return strings.Compare(a.SessionID, b.SessionID)
			})
			var want []string
			for _, md := range ordered {
				want = append(want, md.Harness.Name+"/"+md.SessionID)
			}
			if !slices.Equal(actual, want) {
				t.Fatalf("scope=%+v limit=%d got=%v want=%v", scope, limit, actual, want)
			}
		}
	}
}
