package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// qualificationInventory validates every reachable immutable node and exact
// subtree counts. It returns leaf sets, so differing balanced tree packing does
// not make a correct bulk construction look different from ordinary COW writes.
func qualificationInventory(t *testing.T, store storage.ObjectStore, root catalog.ObjectRef) map[string]json.RawMessage {
	t.Helper()
	out := map[string]json.RawMessage{}
	var visit func(catalog.ObjectRef) uint64
	visit = func(ref catalog.ObjectRef) uint64 {
		if ref.Key == "" {
			return 0
		}
		raw, err := store.Get(t.Context(), ref.Key)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > 128<<10 || !storage.VerifySHA256(raw, ref.SHA256) {
			t.Fatal("invalid fixture node bounds/hash")
		}
		var n qualificationNode
		if err = json.Unmarshal(raw, &n); err != nil {
			t.Fatal(err)
		}
		if len(n.Leaves) > 32 || len(n.Children) > 32 || len(n.Leaves) > 0 && len(n.Children) > 0 || len(n.Leaves)+len(n.Children) == 0 {
			t.Fatal("invalid fixture node shape")
		}
		var count uint64
		for i, v := range n.Leaves {
			if _, found := out[v.Key]; found || i > 0 && n.Leaves[i-1].Key >= v.Key {
				t.Fatal("duplicate/unordered fixture leaf")
			}
			out[v.Key] = v.Value
			count++
		}
		for i, b := range n.Children {
			if i > 0 && n.Children[i-1].Max >= b.Max {
				t.Fatal("unordered fixture branch")
			}
			actual := visit(b.Ref)
			if actual != b.Count {
				t.Fatal("incorrect fixture subtree count")
			}
			count += actual
		}
		if count != n.Count {
			t.Fatal("incorrect fixture node count")
		}
		return count
	}
	visit(root)
	return out
}

func qualificationNormalizeLeaf(t *testing.T, raw []byte) []byte {
	t.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	// Mutations have fresh opaque revision IDs; normalize only those IDs. All
	// metadata refs, summaries, derived rows and child aggregates stay exact.
	value["Revision"] = json.RawMessage(`"normalized"`)
	if entry, ok := value["Entry"]; ok {
		value["Entry"] = qualificationNormalizeLeaf(t, entry)
	}
	if picker, ok := value["Picker"]; ok {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(picker, &row); err != nil {
			t.Fatal(err)
		}
		row["Revision"] = json.RawMessage(`"normalized"`)
		value["Picker"] = qualificationJSON(t, row)
	}
	return qualificationJSON(t, value)
}

func TestPickerQualificationBulkMatchesProductionConstruction(t *testing.T) {
	metadata := qualificationMetadata(140, 0)
	// Prefix ties are not physical-path order. Both the bulk key codec and real
	// constructor are compared to the independent raw component UI oracle.
	metadata[1].SessionID = "a"
	metadata[2].SessionID = "a-b"
	for i := range metadata {
		metadata[i].SourceBundle.Key = "sessions/" + metadata[i].Harness.Name + "/" + metadata[i].SessionID + "/source." + metadata[i].SourceBundle.SHA256 + ".jsonl.gz"
	}
	source := storagetest.NewMemoryStore()
	for _, m := range metadata {
		key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		qualificationPut(t, source, key, qualificationJSON(t, m))
		qualificationPut(t, source, m.SourceBundle.Key, []byte("synthetic source placeholder\n"))
	}
	legacy := privateCatalogFromLegacy(t, source)
	before, _, err := legacy.Writer.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scoped := &scopedMigrationCLIStore{legacy.ObjectStore.(*qualifiedCLIStore)}
	cfg := destination.Config{Provider: "s3", Bucket: "private-picker-qualification", ArchiveFormat: destination.FormatCatalogV4}
	u, err := catalog.OpenPickerUpgrade(t.Context(), scoped, cfg, scoped, "")
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, e := u.Step(t.Context(), 17)
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	if err = u.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = u.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, _, err := legacy.Writer.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.Receipts != before.Receipts {
		t.Fatal("actual upgrade forgot acknowledged receipt universe")
	}
	bulk := newQualificationFixture(t, metadata)
	bulkHead, _, err := bulk.store.Writer.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	productionRoots := []catalog.ObjectRef{after.Identity, after.Capture, after.Activity, after.Project}
	bulkRoots := []catalog.ObjectRef{bulkHead.Identity, bulkHead.Capture, bulkHead.Activity, bulkHead.Project}
	for i, root := range productionRoots {
		expected := qualificationInventory(t, scoped, root)
		actual := qualificationInventory(t, bulk.raw, bulkRoots[i])
		if len(expected) != len(actual) {
			t.Fatalf("tree%d missing leaves: actual%d expected%d", i, len(actual), len(expected))
		}
		for key, raw := range expected {
			if !bytes.Equal(qualificationNormalizeLeaf(t, raw), qualificationNormalizeLeaf(t, actual[key])) {
				t.Fatalf("tree%d semantic mismatch at%s", i, key)
			}
		}
	}
	// The actual constructor's verified metadata refs and all body bytes must
	// match the complete bulk fixture; they are not borrowed from one seed.
	for _, m := range metadata {
		key, _ := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
		entry, _, e := legacy.Writer.Find(t.Context(), key)
		if e != nil {
			t.Fatal(e)
		}
		body, e := bulk.raw.Get(t.Context(), entry.Metadata.Key)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(body, qualificationJSON(t, m)) {
			t.Fatal("bulk fixture metadata reference/body mismatch")
		}
	}
	snapshot, err := catalog.OpenSnapshot(context.Background(), scoped, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := catalog.PickerQuery{Scope: catalog.PickerScope{RepoKey: qualificationRepo, ProjectIDs: []string{archive.ProjectID(qualificationProject)}}, Membership: catalog.PickerBrowse, Limit: 7}
	var got []archive.Metadata
	next := ""
	for {
		page, e := snapshot.PickerPage(t.Context(), q, next)
		if e != nil {
			t.Fatal(e)
		}
		for _, item := range page.Rows {
			hydrated, e := snapshot.HydratePicker(t.Context(), item.Reference)
			if e != nil {
				t.Fatal(e)
			}
			got = append(got, hydrated.Row.Entry.Summary)
		}
		next = page.Next
		if !page.More {
			break
		}
	}
	expected := qualificationOracle(metadata, "list")
	var gotIDs, wantIDs []string
	for _, m := range got {
		gotIDs = append(gotIDs, m.SessionID)
	}
	for _, m := range expected {
		wantIDs = append(wantIDs, m.SessionID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("actual constructor pages differ from exhaustive raw order: got%v want%v", gotIDs, wantIDs)
	}
}
