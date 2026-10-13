package catalog

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Raw alias sources belong to their exact physical session, not to a canonical
// spelling chosen by the lookup group.
func pickerAliasMutation(t *testing.T, w *Writer, harness, id string) CatalogMutation {
	t.Helper()
	m := mutation(t, w, id)
	source, err := w.store.Get(t.Context(), m.Next.Summary.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey(harness, id)
	if err != nil {
		t.Fatal(err)
	}
	sourceKey := strings.TrimSuffix(key, "metadata.json") + "source." + m.Next.Summary.SourceBundle.SHA256 + ".jsonl.gz"
	if err = w.store.Put(t.Context(), sourceKey, source); err != nil {
		t.Fatal(err)
	}
	m = pickerMetadata(t, w, m, func(a *archive.Metadata) { a.Harness.Name = harness; a.SourceBundle.Key = sourceKey })
	m.SessionKey = key
	m.ID = "alias-" + harness + "-" + id
	return m
}

func pickerIdentitySnapshot(w *Writer, h CatalogHead) *Snapshot {
	return &Snapshot{writer: w, head: h, started: time.Now(), cache: &NodeCache{}}
}

func TestPickerCanonicalLookupPreservesAliasAmbiguity(t *testing.T) {
	t.Parallel()
	w, store := fixture(t)
	h := CatalogHead{Protocol: 11, Schema: 5}
	lower, upper, err := PickerIdentityRange("claude", "same")
	if err != nil {
		t.Fatal(err)
	}
	count, err := pickerIdentitySnapshot(w, h).Count(t.Context(), Query{Index: ProjectIndex, Lower: lower, Upper: upper})
	if err != nil || count != 0 {
		t.Fatal(count, err)
	}
	var mutations []CatalogMutation
	for i, harness := range []string{"Claude-Code", "claude"} {
		m := pickerAliasMutation(t, w, harness, "same")
		mutations = append(mutations, m)
		h = pickerCandidate(t, w, h, m, "revision-"+harness)
		counted := &overflowReadStore{qualifiedStore: store}
		reader, err := New(counted)
		if err != nil {
			t.Fatal(err)
		}
		s := pickerIdentitySnapshot(reader, h)
		count, err = s.Count(t.Context(), Query{Index: ProjectIndex, Lower: lower, Upper: upper})
		if err != nil || count != uint64(i+1) {
			t.Fatal(count, err)
		}
		page, err := s.Query(t.Context(), Query{Index: ProjectIndex, Lower: lower, Upper: upper}, "", 2)
		if err != nil || len(page.Rows) != i+1 {
			t.Fatal(err, len(page.Rows))
		}
		if counted.bodyReads.Load() != 0 {
			t.Fatal("canonical lookup hydrated metadata")
		}
		if i == 0 {
			if page.Rows[0].Key != m.SessionKey || page.Rows[0].Entry.Picker.Harness != harness {
				t.Fatal("unique raw alias was substituted")
			}
			if _, _, err = s.ResolveRow(t.Context(), page.Rows[0]); err != nil {
				t.Fatal("unique pinned alias hydration", err)
			}
		} else {
			if page.Rows[0].Key == page.Rows[1].Key || page.Rows[0].Entry.Metadata == page.Rows[1].Entry.Metadata {
				t.Fatal("conflicting aliases silently deduplicated")
			}
		}
	}
	m := mutations[0]
	m.ExpectedRevision = "revision-Claude-Code"
	m.Next = nil
	m.ID = "delete-alias"
	h = pickerCandidate(t, w, h, m, "delete-revision")
	count, err = pickerIdentitySnapshot(w, h).Count(t.Context(), Query{Index: ProjectIndex, Lower: lower, Upper: upper})
	if err != nil || count != 1 {
		t.Fatal("deleted alias retained", count, err)
	}
	// Exact session-boundary prefix excludes adjacent IDs and same ID in another harness.
	for _, other := range []struct{ harness, id string }{{"codex", "same"}, {"claude", "same-more"}} {
		m := pickerAliasMutation(t, w, other.harness, other.id)
		h = pickerCandidate(t, w, h, m, m.ID)
	}
	count, err = pickerIdentitySnapshot(w, h).Count(t.Context(), Query{Index: ProjectIndex, Lower: lower, Upper: upper})
	if err != nil || count != 1 {
		t.Fatal("canonical group crossed boundary", count, err)
	}
}

func TestPickerCanonicalLookupIncludesAllLiveClassifications(t *testing.T) {
	t.Parallel()
	w, _ := fixture(t)
	h := CatalogHead{Protocol: 11, Schema: 5}
	for _, kind := range []string{"zero", "child", "native-child", "replay", "overflow"} {
		t.Run(kind, func(t *testing.T) {
			m := mutation(t, w, kind)
			if kind == "overflow" {
				m = largeLinkedMutation(t, w, kind)
			}
			m = pickerMetadata(t, w, m, func(a *archive.Metadata) {
				a.ProjectID = ""
				a.RepoKey = ""
				n := 0
				a.Counts.Turns = &n
				switch kind {
				case "child":
					a.ParentSessionID = "zero"
				case "native-child":
					a.NativeChild = true
				case "replay":
					a.Replay = &archive.Replay{}
				}
			})
			h = pickerCandidate(t, w, h, m, "revision-"+kind)
			lower, upper, err := PickerIdentityRange("claude", kind)
			if err != nil {
				t.Fatal(err)
			}
			s := pickerIdentitySnapshot(w, h)
			page, err := s.Query(t.Context(), Query{Index: ProjectIndex, Lower: lower, Upper: upper}, "", 2)
			if err != nil || len(page.Rows) != 1 {
				t.Fatal(kind, len(page.Rows), err)
			}
			if page.Rows[0].Entry.Picker == nil || page.Rows[0].Entry.SummaryOverflow != overflowMetadata {
				t.Fatal("lookup copy lost compact facts")
			}
		})
	}
	lower, upper, _ := PickerIdentityRange("claude", "zero")
	page, err := pickerIdentitySnapshot(w, h).Query(t.Context(), Query{Index: ProjectIndex, Lower: lower, Upper: upper}, "", 2)
	if err != nil || page.Rows[0].Entry.Picker.OrdinaryChildren != 1 {
		t.Fatal("canonical parent counter copy not updated", err)
	}

	// Scope/prompt rewrites replace the same lookup copy while preserving live counters.
	rewritten := mutation(t, w, "zero")
	rewritten = pickerMetadata(t, w, rewritten, func(a *archive.Metadata) { a.ProjectID = "changed-project"; n := 2; a.Counts.Turns = &n })
	rewritten.ExpectedRevision = "revision-zero"
	rewritten.ID = "rewrite-zero"
	h = pickerCandidate(t, w, h, rewritten, "rewritten-zero")
	page, err = pickerIdentitySnapshot(w, h).Query(t.Context(), Query{Index: ProjectIndex, Lower: lower, Upper: upper}, "", 2)
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Entry.Picker.Revision != "rewritten-zero" || page.Rows[0].Entry.Picker.ProjectID != "changed-project" || page.Rows[0].Entry.Picker.PromptState != "positive" || page.Rows[0].Entry.Picker.OrdinaryChildren != 1 {
		t.Fatal("lookup rewrite lost facts/counters", err)
	}
	live := map[string]bool{}
	if err = w.markTree(t.Context(), h.Project, live, map[string]bool{}, 0); err != nil {
		t.Fatal("GC lookup codec traversal", err)
	}
	parent, err := w.find(t.Context(), h.Identity, "sessions/claude/zero/metadata.json")
	if err != nil || !live[parent.Entry.Metadata.Key] {
		t.Fatal("GC lost metadata reached through copies", err)
	}
	// Default Project query still visits each live identity exactly once.
	page, err = pickerIdentitySnapshot(w, h).Query(t.Context(), Query{Index: ProjectIndex}, "", 100)
	if err != nil || len(page.Rows) != 5 {
		t.Fatal("lookup copies changed default Project range", len(page.Rows), err)
	}
}

func TestPickerCanonicalLookupCodecAndRangeRefuseCorruption(t *testing.T) {
	t.Parallel()
	w, _ := fixture(t)
	m := pickerAliasMutation(t, w, "Claude-Code", "session")
	m.Next.Revision = "revision"
	row, err := BuildPickerRow(m.SessionKey, *m.Next)
	if err != nil {
		t.Fatal(err)
	}
	m.Next.Picker = row
	key, err := PickerIdentityKey(*row)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := encodeTreeValue(key, m.Next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodePickerRow(key, raw); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{key + "00", strings.Replace(key, "/v1/", "/v2/", 1), strings.Replace(key, hex.EncodeToString([]byte("session")), hex.EncodeToString([]byte("other")), 1)} {
		if _, err = DecodePickerRow(bad, raw); err == nil {
			t.Fatal("forged identity key accepted", bad)
		}
		if _, err = decodeRow(item{Key: bad, Value: raw}, ProjectIndex); err == nil {
			t.Fatal("snapshot accepted forged identity key")
		}
	}
	full, _ := json.Marshal(m.Next)
	if _, err = DecodePickerRow(key, full); err == nil {
		t.Fatal("noncompact lookup envelope accepted")
	}
	for _, invalid := range []struct{ harness, id string }{{"", "session"}, {"claude", ""}, {"claude", "../session"}, {string([]byte{0xff}), "session"}, {"claude", strings.Repeat("s", 1024)}} {
		if _, _, err = PickerIdentityRange(invalid.harness, invalid.id); err == nil {
			t.Fatal("invalid identity range accepted")
		}
	}
	lower, upper, err := PickerIdentityRange("Claude-Code", "session")
	if err != nil {
		t.Fatal(err)
	}
	same, _, _ := PickerIdentityRange("claude", "session")
	if lower != same || !(key >= lower && key < upper) || upper != strings.TrimSuffix(lower, "/")+"0" {
		t.Fatal("canonical range grammar mismatch")
	}
}
