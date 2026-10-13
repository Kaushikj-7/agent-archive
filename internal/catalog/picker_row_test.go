package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func pickerCandidate(t *testing.T, w *Writer, h CatalogHead, m CatalogMutation, revision string) CatalogHead {
	t.Helper()
	frozen, digest, err := freezeMutation(m)
	if err != nil {
		t.Fatal(err)
	}
	owner := "candidate/" + revision
	if err = w.Coordinator().Admit(t.Context(), owner, digest, nil); err != nil {
		t.Fatal(err)
	}
	admitted := context.WithValue(t.Context(), writeAuthorityKey{}, writeAuthority{writer: w, owner: owner, digest: digest})
	next, err := w.prepareCommit(admitted, h, frozen, revision, digest, false)
	if completeErr := w.Coordinator().Complete(t.Context(), owner, digest); completeErr != nil {
		t.Fatal(completeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func pickerMetadata(t *testing.T, w *Writer, m CatalogMutation, edit func(*archive.Metadata)) CatalogMutation {
	t.Helper()
	edit(&m.Next.Summary)
	raw, err := json.Marshal(m.Next.Summary)
	if err != nil {
		t.Fatal(err)
	}
	m.Next.Metadata, err = w.PutImmutable(t.Context(), KindMetadata, raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func pickerLeaves(t *testing.T, w *Writer, h CatalogHead) map[string]PickerRow {
	t.Helper()
	result := map[string]PickerRow{}
	var visit func(ObjectRef)
	visit = func(ref ObjectRef) {
		if ref.Key == "" {
			return
		}
		n, err := w.readNode(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range n.Children {
			visit(c.Ref)
		}
		for _, l := range n.Leaves {
			if !strings.HasPrefix(l.Key, "!picker/") {
				continue
			}
			r, err := DecodePickerRow(l.Key, l.Value)
			if err != nil {
				t.Fatal(err)
			}
			result[l.Key] = *r
		}
	}
	visit(h.Project)
	return result
}

func TestPickerMembershipsMatchExhaustiveClassification(t *testing.T) {
	t.Parallel()
	for _, repo := range []string{"", archive.RepoKey("https://example.com/synthetic.git")} {
		for _, prompt := range []int{-1, 0, 2} {
			for _, kind := range []string{"root", "child", "native-child", "replay"} {
				t.Run(fmt.Sprintf("%s/%d/%s", repo, prompt, kind), func(t *testing.T) {
					t.Parallel()
					w, _ := fixture(t)
					m := mutation(t, w, "session")
					m = pickerMetadata(t, w, m, func(a *archive.Metadata) {
						a.RepoKey = repo
						if prompt >= 0 {
							a.Counts.Turns = &prompt
						}
						switch kind {
						case "child":
							a.ParentSessionID = "parent"
						case "native-child":
							a.NativeChild = true
						case "replay":
							a.Replay = &archive.Replay{}
						}
					})
					h := pickerCandidate(t, w, CatalogHead{Protocol: 11, Schema: 5}, m, "revision")
					leaves := pickerLeaves(t, w, h)
					expected := 12
					if prompt == 0 {
						expected = 6
					}
					if kind != "root" {
						expected = 0
					}
					if len(leaves) != expected {
						t.Fatalf("membership count %d want %d", len(leaves), expected)
					}
					for k, r := range leaves {
						if r.RepoKey != repo || r.SessionKey != m.SessionKey {
							t.Fatal("lost exact facts")
						}
						if repo != "" && strings.Contains(k, "project-missing/") {
							t.Fatal("repository conflict fallback")
						}
						if prompt == 0 && strings.Contains(k, "/handoff/") {
							t.Fatal("zero prompts eligible")
						}
					}
				})
			}
		}
	}
}

func TestPickerOverflowRewriteRemovalAndChildCounts(t *testing.T) {
	t.Parallel()
	w, _ := fixture(t)
	parent := largeLinkedMutation(t, w, "parent")
	parent = pickerMetadata(t, w, parent, func(a *archive.Metadata) {
		a.RepoKey = archive.RepoKey("https://example.com/one")
		a.Name = strings.Repeat("雪", 300)
		a.Title = strings.Repeat("<", 300)
		a.ProjectName = strings.Repeat("&", 300)
	})
	h := pickerCandidate(t, w, CatalogHead{Protocol: 11, Schema: 5}, parent, "parent-v1")
	old := pickerLeaves(t, w, h)
	if len(old) != 12 {
		t.Fatal(len(old))
	}
	record, err := w.find(t.Context(), h.Identity, parent.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	if record.Entry.SummaryOverflow == "" || record.Entry.Picker.RepoKey == "" {
		t.Fatal("overflow lost facts")
	}
	child := mutation(t, w, "child")
	child = pickerMetadata(t, w, child, func(a *archive.Metadata) { a.ParentSessionID = "parent" })
	h = pickerCandidate(t, w, h, child, "child-v1")
	for _, r := range pickerLeaves(t, w, h) {
		if r.OrdinaryChildren != 1 || r.Revision != "parent-v1" {
			t.Fatal("child counts not propagated")
		}
	}
	child.ExpectedRevision = "child-v1"
	child.Next = nil
	child.ID = "delete-child"
	h = pickerCandidate(t, w, h, child, "child-v2")
	for _, r := range pickerLeaves(t, w, h) {
		if r.OrdinaryChildren != 0 {
			t.Fatal("child count not removed")
		}
	}
	parent.ExpectedRevision = "parent-v1"
	parent.ID = "rewrite-parent"
	parent = pickerMetadata(t, w, parent, func(a *archive.Metadata) { a.RepoKey = ""; a.ProjectID = "new-project"; n := 0; a.Counts.Turns = &n })
	h = pickerCandidate(t, w, h, parent, "parent-v2")
	leaves := pickerLeaves(t, w, h)
	if len(leaves) != 6 {
		t.Fatal(len(leaves))
	}
	for k := range old {
		if _, ok := leaves[k]; ok && strings.Contains(k, "/repo/") {
			t.Fatal("stale membership")
		}
	}
	parent.Next = nil
	parent.ID = "delete-parent"
	parent.ExpectedRevision = "parent-v2"
	h = pickerCandidate(t, w, h, parent, "parent-v3")
	if len(pickerLeaves(t, w, h)) != 0 {
		t.Fatal("delete retained membership")
	}
}

func TestPickerBoundedCodecAndForgedFacts(t *testing.T) {
	t.Parallel()
	w, _ := fixture(t)
	m := mutation(t, w, "bounded")
	m = pickerMetadata(t, w, m, func(a *archive.Metadata) { a.Name = strings.Repeat("雪", 400); a.Title = strings.Repeat("\x01", 400) })
	m.Next.Revision = "revision"
	r, err := BuildPickerRow(m.SessionKey, *m.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Name) > 256 || len(r.Title) > 256 {
		t.Fatal("labels unbounded")
	}
	raw, _ := json.Marshal(r)
	if len(raw) > PickerRowBytes {
		t.Fatal("row unbounded")
	}
	var decoded PickerRow
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	bad := append(raw[:len(raw)-1], []byte(",\"Unknown\":1}")...)
	if json.Unmarshal(bad, &decoded) == nil {
		t.Fatal("unknown field accepted")
	}
	m.Next.Picker = r
	r.RepoKey = archive.RepoKey("https://example.com/forged")
	if _, err = w.prepareCommit(t.Context(), CatalogHead{Protocol: 11, Schema: 5}, m, "revision", "digest", false); err == nil {
		t.Fatal("forged facts accepted")
	}
	if _, err = w.prepareCommit(t.Context(), CatalogHead{Protocol: 10, Schema: 4}, m, "revision", "digest", false); err == nil {
		t.Fatal("old protocol emitted picker")
	}
	r.RepoKey = ""
	m.Next.Picker = r
	raw, _ = json.Marshal(compactEntry(m.Next))
	keys, err := PickerMembershipKeys(m.SessionKey, *r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodePickerRow(keys[0]+"x", raw); err == nil {
		t.Fatal("wrong membership key accepted")
	}
	if _, err = DecodePickerRow(keys[0], raw); err != nil {
		t.Fatal(err)
	}
	// Immutable checksum mismatch is still selection authority failure.
	if _, err = decodeEntryBody(*compactEntry(m.Next), []byte("wrong")); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatal(err)
	}
}

func TestPickerRawHarnessTieAndCanonicalFilter(t *testing.T) {
	t.Parallel()
	w, _ := fixture(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var keys []string
	for _, harness := range []string{"claude", "Claude-Code"} {
		m := mutation(t, w, "tie")
		m = pickerMetadata(t, w, m, func(a *archive.Metadata) { a.Harness.Name = harness; a.CapturedAt = at })
		m.SessionKey, _ = archive.MetadataObjectKey(harness, "tie")
		m.Next.Revision = "revision"
		r, err := BuildPickerRow(m.SessionKey, *m.Next)
		if err != nil {
			t.Fatal(err)
		}
		if r.CanonicalHarness != "claude" {
			t.Fatal(r.CanonicalHarness)
		}
		all, err := PickerMembershipKeys(m.SessionKey, *r)
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, all[0])
	}
	if keys[0] >= keys[1] {
		t.Fatal("raw alias ascending identity tie not retained under reverse scan")
	}
}

func TestPickerFirstRangeDoesNotReadMetadataBodies(t *testing.T) {
	t.Parallel()
	w, store := fixture(t)
	m := largeLinkedMutation(t, w, "large")
	h := pickerCandidate(t, w, CatalogHead{Protocol: 11, Schema: 5}, m, "revision")
	counted := &overflowReadStore{qualifiedStore: store}
	reader, err := New(counted)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &Snapshot{writer: reader, head: h, started: time.Now(), cache: &NodeCache{}}
	prefix := "!picker/v1/browse/global/-/all/"
	page, err := snapshot.Query(t.Context(), Query{Index: ProjectIndex, Lower: prefix, Upper: strings.TrimSuffix(prefix, "/") + "0", Reverse: true}, "", 1)
	if err != nil || len(page.Rows) != 1 {
		t.Fatal(err, len(page.Rows))
	}
	if counted.bodyReads.Load() != 0 || page.Rows[0].Entry.Picker == nil {
		t.Fatal("first range hydrated body")
	}
}

func TestPickerCandidateConflictReceiptAndFrozenInput(t *testing.T) {
	t.Parallel()
	w, _ := fixture(t)
	m := mutation(t, w, "session")
	frozen, digest, err := freezeMutation(m)
	if err != nil {
		t.Fatal(err)
	}
	m.Next.Summary.ProjectID = "caller mutation"
	h := pickerCandidate(t, w, CatalogHead{Protocol: 11, Schema: 5}, frozen, "revision")
	revision, err := w.receipt(t.Context(), h, m.ID, digest)
	if err != nil || revision != "revision" {
		t.Fatal(revision, err)
	}
	if _, err = w.receipt(t.Context(), h, m.ID, "different"); !errors.Is(err, ErrMutationReuse) {
		t.Fatal(err)
	}
	if _, err = w.prepareCommit(t.Context(), h, frozen, "other", "digest", false); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	for _, r := range pickerLeaves(t, w, h) {
		if r.ProjectID != "project" {
			t.Fatal("caller mutation changed frozen facts")
		}
	}
}
