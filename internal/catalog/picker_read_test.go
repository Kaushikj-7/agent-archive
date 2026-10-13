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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func pickerReaderFixture(t *testing.T) (*Snapshot, *Writer, *qualifiedStore, []archive.Metadata) {
	t.Helper()
	w, store := fixture(t)
	var metadata []archive.Metadata
	for i := range 96 {
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
		if _, err := w.Commit(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	completePickerUpgrade(t, w, store)
	s, err := OpenSnapshot(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, w, store, metadata
}
func testPickerRepo(c byte) string {
	return archive.RepoKey("https://example.com/" + string(c) + ".git")
}
func pickerOracle(ms []archive.Metadata, q PickerQuery) []string {
	var filtered []archive.Metadata
	for _, m := range ms {
		if m.IsChild() || m.Replay != nil || q.Membership == "handoff" && m.Counts.Turns != nil && *m.Counts.Turns == 0 {
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
		for _, member := range []string{"browse", "handoff"} {
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
	q := PickerQuery{Scope: PickerScope{All: true}, Membership: "browse", Limit: 2}
	p, err := s.PickerPage(t.Context(), q, "")
	if err != nil {
		t.Fatal(err)
	}
	wrong := q
	wrong.Membership = "handoff"
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
	reader.Close()
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
	s, _, _, _ := pickerReaderFixture(t)
	for _, q := range []PickerQuery{{Scope: PickerScope{All: true}, Membership: "browse", Limit: 101}, {Scope: PickerScope{All: true, RepoKey: testPickerRepo('a')}, Membership: "browse", Limit: 5}, {Membership: "arbitrary", Limit: 5}} {
		p, err := s.PickerPage(t.Context(), q, "")
		if !errors.Is(err, ErrPickerUnsupported) || p.Empty {
			t.Fatalf("%+v %v", p, err)
		}
	}
	q := PickerQuery{Scope: PickerScope{All: true}, Membership: "browse", Limit: 5, Budget: PickerBudget{Nodes: 1}}
	p, err := s.PickerPage(t.Context(), q, "")
	if !errors.Is(err, ErrPickerBudget) || p.Empty || len(p.Rows) > 0 {
		t.Fatalf("%+v %v", p, err)
	}
	q.Budget = PickerBudget{Bytes: 1}
	if _, err = s.PickerPage(t.Context(), q, ""); !errors.Is(err, ErrPickerBudget) {
		t.Fatal(err)
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
	if !r.Found || r.Identity.Harness != "claude" || r.Item.Row.SessionKey != alias.SessionKey || r.Item.Row.Root || !r.Item.Row.Replay || r.Item.Row.PromptState != "zero" {
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
