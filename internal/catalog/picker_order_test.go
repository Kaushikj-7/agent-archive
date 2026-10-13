package catalog

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestPickerMembershipTieOrderMatchesSeparateRawComponents(t *testing.T) {
	w, _ := fixture(t)
	type ordered struct {
		key     string
		harness string
		session string
	}
	groups := map[string][]ordered{}
	for _, repo := range []string{"", archive.RepoKey("https://example.com/ties")} {
		for _, harness := range []string{"a", "a-", "a0", "claude", "claude-code", "Claude-Code"} {
			for _, session := range []string{"a", "a-", "a0", "z"} {
				m := mutation(t, w, session)
				m = pickerMetadata(t, w, m, func(a *archive.Metadata) { a.Harness.Name = harness; a.RepoKey = repo })
				m.SessionKey, _ = archive.MetadataObjectKey(harness, session)
				m.Next.Revision = "revision"
				r, err := BuildPickerRow(m.SessionKey, *m.Next)
				if err != nil {
					t.Fatal(err)
				}
				keys, err := PickerMembershipKeys(m.SessionKey, *r)
				if err != nil {
					t.Fatal(err)
				}
				for _, key := range keys {
					parts := strings.Split(key, "/")
					prefix := strings.Join(parts[:6], "/")
					// Repository-present and missing cases are separate universes.
					groups[repo+":"+prefix] = append(groups[repo+":"+prefix], ordered{key, harness, session})
				}
			}
		}
	}
	for prefix, rows := range groups {
		t.Run(prefix, func(t *testing.T) {
			want := append([]ordered(nil), rows...)
			// This is the exhaustive caller's separate component comparator,
			// independent of the physical object-key spelling or key encoding.
			sort.Slice(want, func(i, j int) bool {
				if want[i].harness != want[j].harness {
					return want[i].harness < want[j].harness
				}
				return want[i].session < want[j].session
			})
			sort.Slice(rows, func(i, j int) bool { return rows[i].key > rows[j].key })
			for i := range rows {
				if rows[i].harness != want[i].harness || rows[i].session != want[i].session {
					t.Fatalf("rank %d got %s/%s want %s/%s", i, rows[i].harness, rows[i].session, want[i].harness, want[i].session)
				}
				if i > 0 && rows[i].key == rows[i-1].key {
					t.Fatal("distinct physical identities collided")
				}
			}
		})
	}
}

func TestPickerCandidateTiePagesSurviveRewriteCountsAndDeletion(t *testing.T) {
	w, _ := fixture(t)
	h := CatalogHead{Protocol: 11, Schema: 5}
	mutations := []CatalogMutation{
		pickerAliasMutation(t, w, "claude-code", "a"),
		pickerAliasMutation(t, w, "claude", "a-"),
		pickerAliasMutation(t, w, "claude", "a0"),
		pickerAliasMutation(t, w, "claude", "a"),
	}
	for _, m := range mutations {
		h = pickerCandidate(t, w, h, m, "revision-"+m.ID)
	}
	check := func(want []string) {
		t.Helper()
		for _, tier := range []string{"all", "636c61756465"} {
			prefix := "!picker/v1/handoff/global/-/" + tier + "/"
			s := pickerIdentitySnapshot(w, h)
			var got []string
			cursor := ""
			for {
				page, err := s.Query(t.Context(), Query{Index: ProjectIndex, Lower: prefix, Upper: strings.TrimSuffix(prefix, "/") + "0", Reverse: true}, cursor, 1)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range page.Rows {
					got = append(got, row.Entry.Picker.Harness+"/"+row.Entry.Picker.SessionID)
				}
				if page.Next == "" {
					break
				}
				cursor = page.Next
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s got %v want %v", tier, got, want)
			}
		}
	}
	check([]string{"claude/a", "claude/a-", "claude/a0", "claude-code/a"})
	child := pickerAliasMutation(t, w, "claude", "child")
	child = pickerMetadata(t, w, child, func(m *archive.Metadata) { m.ParentSessionID = "a" })
	h = pickerCandidate(t, w, h, child, "child")
	check([]string{"claude/a", "claude/a-", "claude/a0", "claude-code/a"})
	parent, err := w.find(t.Context(), h.Identity, mutations[3].SessionKey)
	if err != nil || parent.Entry.Picker.OrdinaryChildren != 1 {
		t.Fatal("parent counter", err)
	}
	changed := mutations[1]
	changed.ExpectedRevision = "revision-" + changed.ID
	changed.ID = "rewrite-zero"
	changed = pickerMetadata(t, w, changed, func(m *archive.Metadata) { n := 0; m.Counts.Turns = &n; m.ProjectID = "moved" })
	h = pickerCandidate(t, w, h, changed, "zero")
	check([]string{"claude/a", "claude/a0", "claude-code/a"})
	deleted := mutations[3]
	deleted.ExpectedRevision = "revision-" + deleted.ID
	deleted.ID = "delete-parent"
	deleted.Next = nil
	h = pickerCandidate(t, w, h, deleted, "deleted")
	check([]string{"claude/a0", "claude-code/a"})
	// Exact canonical-group identity lookup is independent of ordering suffixes.
	lower, upper, err := PickerIdentityRange("claude", "a")
	if err != nil {
		t.Fatal(err)
	}
	page, err := pickerIdentitySnapshot(w, h).Query(t.Context(), Query{Index: ProjectIndex, Lower: lower, Upper: upper}, "", 100)
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Entry.Picker.Harness != "claude-code" {
		t.Fatal("alias lookup", err)
	}
}
