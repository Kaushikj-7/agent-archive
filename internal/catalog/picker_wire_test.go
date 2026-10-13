package catalog

import (
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestPickerWireRequiresExplicitNonNullFacts(t *testing.T) {
	w, _ := fixture(t)
	m := mutation(t, w, "wire")
	m.Next.Revision = "revision"
	r, err := BuildPickerRow(m.SessionKey, *m.Next)
	if err != nil {
		t.Fatal(err)
	}
	m.Next.Picker = r
	raw, err := json.Marshal(compactEntry(m.Next))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := PickerMembershipKeys(m.SessionKey, *r)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(envelope["Picker"], &fields); err != nil {
		t.Fatal(err)
	}
	if _, err = DecodePickerRow(keys[0], raw); err != nil {
		t.Fatal(err)
	}
	// The schema declares every fact except PromptCount required, even when
	// its legitimate value is empty, false, or zero.
	for name := range fields {
		for _, mode := range []string{"missing", "null"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				changed := make(map[string]json.RawMessage, len(fields))
				for k, v := range fields {
					changed[k] = v
				}
				if mode == "missing" {
					delete(changed, name)
				} else {
					changed[name] = json.RawMessage("null")
				}
				picker, marshalErr := json.Marshal(changed)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				envelope["Picker"] = picker
				bad, marshalErr := json.Marshal(envelope)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if _, decodeErr := DecodePickerRow(keys[0], bad); decodeErr == nil {
					t.Fatal("malformed picker facts accepted")
				}
				if _, decodeErr := decodeRow(item{Key: keys[0], Value: bad}, ProjectIndex); decodeErr == nil {
					t.Fatal("snapshot accepted malformed picker facts")
				}
			})
		}
	}
	fields["PromptCount"] = json.RawMessage("null")
	envelope["Picker"], _ = json.Marshal(fields)
	bad, _ := json.Marshal(envelope)
	if _, err = DecodePickerRow(keys[0], bad); err == nil {
		t.Fatal("unknown prompts accepted explicit null count")
	}
}

func TestPickerScopePagesTrackClassificationAndOrderChanges(t *testing.T) {
	w, _ := fixture(t)
	repo := archive.RepoKey("https://example.com/exact")
	a := mutation(t, w, "alpha")
	a = pickerMetadata(t, w, a, func(m *archive.Metadata) { m.RepoKey = repo })
	b := mutation(t, w, "beta")
	b = pickerMetadata(t, w, b, func(m *archive.Metadata) { m.CapturedAt = m.CapturedAt.Add(time.Hour); n := 0; m.Counts.Turns = &n })
	c := mutation(t, w, "gamma")
	c = pickerMetadata(t, w, c, func(m *archive.Metadata) {
		m.CapturedAt = m.CapturedAt.Add(2 * time.Hour)
		m.RepoKey = archive.RepoKey("https://example.com/conflict")
	})
	h := CatalogHead{Protocol: 11, Schema: 5}
	for _, m := range []CatalogMutation{a, b, c} {
		h = pickerCandidate(t, w, h, m, "initial-"+m.Next.Summary.SessionID)
	}
	check := func(scope, membership string, want []string) {
		t.Helper()
		s := &Snapshot{writer: w, head: h, started: time.Now(), cache: &NodeCache{}}
		prefix := "!picker/v1/" + membership + "/" + scope + "/all/"
		page, err := s.Query(t.Context(), Query{Index: ProjectIndex, Lower: prefix, Upper: prefix[:len(prefix)-1] + "0", Reverse: true}, "", 100)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, r := range page.Rows {
			got = append(got, r.Entry.Summary.SessionID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s/%s got %v want %v", scope, membership, got, want)
		}
	}
	project := "project-any/" + hex.EncodeToString([]byte("project"))
	missing := "project-missing/" + hex.EncodeToString([]byte("project"))
	exactRepo := "repo/" + hex.EncodeToString([]byte(repo))
	check("global/-", "browse", []string{"gamma", "beta", "alpha"})
	check("global/-", "handoff", []string{"gamma", "alpha"})
	check(project, "browse", []string{"gamma", "beta", "alpha"})
	check(missing, "browse", []string{"beta"})
	check(exactRepo, "browse", []string{"alpha"})
	// Root-to-child, child-to-replay-root and replay-to-native-child updates
	// must remove every prior ordinary membership, regardless of scope.
	for i, kind := range []string{"child", "replay", "native-child", "root"} {
		a.ID = "transition-" + kind
		a.ExpectedRevision = "initial-alpha"
		if i > 0 {
			a.ExpectedRevision = "transition-" + []string{"child", "replay", "native-child", "root"}[i-1]
		}
		a = pickerMetadata(t, w, a, func(m *archive.Metadata) {
			m.ParentSessionID = ""
			m.NativeChild = false
			m.Replay = nil
			switch kind {
			case "child":
				m.ParentSessionID = "beta"
			case "replay":
				m.Replay = &archive.Replay{}
			case "native-child":
				m.NativeChild = true
			}
		})
		h = pickerCandidate(t, w, h, a, a.ID)
		want := []string{}
		if kind == "root" {
			want = append(want, "alpha")
		}
		check(exactRepo, "browse", want)
		check(exactRepo, "handoff", want)
	}
}

func TestPickerResolutionMatchesPinnedInlineAndOverflowIdentity(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		name := "inline"
		if overflow {
			name = "overflow"
		}
		t.Run(name, func(t *testing.T) {
			w, store := fixture(t)
			m := mutation(t, w, "selected")
			if overflow {
				m = largeLinkedMutation(t, w, "selected")
			}
			m = pickerMetadata(t, w, m, func(a *archive.Metadata) { a.Title = "full verified title" })
			h := pickerCandidate(t, w, CatalogHead{Protocol: 11, Schema: 5}, m, "revision")
			counted := &overflowReadStore{qualifiedStore: store}
			reader, err := New(counted)
			if err != nil {
				t.Fatal(err)
			}
			s := &Snapshot{writer: reader, head: h, started: time.Now(), cache: &NodeCache{}}
			prefix := "!picker/v1/browse/global/-/all/"
			page, err := s.Query(t.Context(), Query{Index: ProjectIndex, Lower: prefix, Upper: "!picker/v1/browse/global/-/all0"}, "", 1)
			if err != nil || len(page.Rows) != 1 {
				t.Fatal(err, len(page.Rows))
			}
			row := page.Rows[0]
			identity, err := s.Find(t.Context(), m.SessionKey)
			if err != nil || (identity.SummaryOverflow != "") != overflow {
				t.Fatal("identity fixture", err)
			}
			if row.Entry.SummaryOverflow != overflowMetadata || counted.bodyReads.Load() != 0 {
				t.Fatal("picker was not lazy")
			}
			for _, change := range []struct {
				name string
				edit func(*Row)
			}{
				{"label", func(r *Row) { r.Entry.Picker.Title = "forged" }},
				{"reference", func(r *Row) {
					r.Entry.Metadata.SHA256 = storage.SHA256Hex([]byte("foreign"))
					r.Entry.Metadata.Key = "catalog-v4/metadata/" + r.Entry.Metadata.SHA256 + ".json"
					r.Entry.Picker.Metadata = r.Entry.Metadata
				}},
				{"revision", func(r *Row) { r.Entry.Revision = "foreign"; r.Entry.Picker.Revision = "foreign" }},
				{"identity", func(r *Row) { r.Key, _ = archive.MetadataObjectKey("claude", "foreign") }},
				{"summary", func(r *Row) { r.Entry.Summary.Title = "forged" }},
			} {
				t.Run(change.name, func(t *testing.T) {
					forged := row
					picker := *row.Entry.Picker
					forged.Entry.Picker = &picker
					change.edit(&forged)
					if _, _, err := s.ResolveRow(t.Context(), forged); err == nil {
						t.Fatal("forged row resolved")
					}
					if counted.bodyReads.Load() != 0 {
						t.Fatal("forged row read body")
					}
				})
			}
			resolved, raw, err := s.ResolveRow(t.Context(), row)
			if err != nil {
				t.Fatal("legitimate picker resolution", err)
			}
			if !storage.VerifySHA256(raw, row.Entry.Metadata.SHA256) || resolved.Entry.Summary.Title != "full verified title" || counted.bodyReads.Load() != 1 {
				t.Fatal("body authority not verified")
			}
			full, ok, err := s.ResolvedMetadata(t.Context(), resolved)
			if err != nil || !ok || full.Title != "full verified title" {
				t.Fatal("missing body provenance", err)
			}
			if _, ok, err := s.ResolvedMetadata(t.Context(), resolved); err != nil || ok {
				t.Fatal("body provenance reused", err)
			}
		})
	}
}
