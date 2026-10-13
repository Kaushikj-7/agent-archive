package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// pickerMeter retains all qualified capabilities while forbidding hidden scans
// and tracking metadata bodies separately from node/head/coordinator requests.
type pickerMeter struct {
	*qualifiedStore
	metadata    int
	lists       int
	nodes       int
	head        int
	coordinator int
	bytes       int64
}

func (m *pickerMeter) observe(key string, n int) {
	m.bytes += int64(n)
	switch {
	case strings.HasPrefix(key, "catalog-v4/metadata/"):
		m.metadata++
	case strings.HasPrefix(key, "catalog-v4/nodes/"):
		m.nodes++
	case key == HeadKey:
		m.head++
	default:
		m.coordinator++
	}
}

func (m *pickerMeter) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	b, e := m.qualifiedStore.GetLimited(ctx, key, limit)
	m.observe(key, len(b))
	return b, e
}

func (m *pickerMeter) GetCatalogVersion(ctx context.Context, key string, limit int64) ([]byte, storage.CatalogObjectVersion, error) {
	b, v, e := m.qualifiedStore.GetCatalogVersion(ctx, key, limit)
	m.observe(key, len(b))
	return b, v, e
}

func (m *pickerMeter) Get(ctx context.Context, key string) ([]byte, error) {
	b, e := m.qualifiedStore.Get(ctx, key)
	m.observe(key, len(b))
	return b, e
}

func (m *pickerMeter) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	m.lists++
	return nil, errors.New("unexpected LIST in picker request")
}

// Bulk construction writes each live node once. It neither times construction
// nor retains the millions of dead nodes produced by per-entry COW insertion.
func bulkPickerTree(t *testing.T, ctx context.Context, w *Writer, leaves []item, unary int) ObjectRef {
	t.Helper()
	slices.SortFunc(leaves, func(a, b item) int { return strings.Compare(a.Key, b.Key) })
	var layer []branch
	for start := 0; start < len(leaves); start += fanout {
		end := min(start+fanout, len(leaves))
		n := node{Leaves: leaves[start:end]}
		n.Count = nodeCount(n)
		ref, e := w.putJSON(ctx, KindNodes, n, maxNodeBytes)
		if e != nil {
			t.Fatal(e)
		}
		layer = append(layer, branch{Max: leaves[end-1].Key, Ref: ref, Count: n.Count})
	}
	for len(layer) > 1 {
		var next []branch
		for start := 0; start < len(layer); start += fanout {
			end := min(start+fanout, len(layer))
			n := node{Children: layer[start:end]}
			n.Count = nodeCount(n)
			ref, e := w.putJSON(ctx, KindNodes, n, maxNodeBytes)
			if e != nil {
				t.Fatal(e)
			}
			next = append(next, branch{Max: layer[end-1].Max, Ref: ref, Count: n.Count})
		}
		layer = next
	}
	if len(layer) == 0 {
		return ObjectRef{}
	}
	root := layer[0]
	for range unary {
		n := node{Count: root.Count, Children: []branch{root}}
		ref, e := w.putJSON(ctx, KindNodes, n, maxNodeBytes)
		if e != nil {
			t.Fatal(e)
		}
		root.Ref = ref
	}
	return root.Ref
}

func bulkPickerFixture(t *testing.T, size, unary int) (*Snapshot, *pickerMeter) {
	t.Helper()
	w, store := fixture(t)
	seed := mutation(t, w, "bulk-seed")
	if _, e := w.Commit(t.Context(), seed); e != nil {
		t.Fatal(e)
	}
	completePickerUpgrade(t, w, store)
	h, _, e := w.Head(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	entry, e1, err := w.Find(t.Context(), seed.SessionKey)
	if err != nil || e1 == "" {
		t.Fatal(err)
	}
	leaves := make([]item, 0, size+50)
	for i := range size {
		full := *entry
		full.Summary = seed.Next.Summary
		full.SummaryOverflow = ""
		full.Summary.SessionID = fmt.Sprintf("bulk-%06d", i)
		full.Summary.ProjectID = "unrelated"
		if i < 50 {
			full.Summary.RepoKey = testPickerRepo('a')
			full.Summary.ProjectID = "wanted"
		}
		key, _ := archive.MetadataObjectKey("claude", full.Summary.SessionID)
		row, e := BuildPickerRow(key, full)
		if e != nil {
			t.Fatal(e)
		}
		full.Picker = row
		keys, e := PickerMembershipKeys(key, *row)
		if e != nil {
			t.Fatal(e)
		}
		// All unrelated entries contribute an authentic global membership. The
		// fixed 50 relevant entries also contribute their repository membership.
		for _, k := range keys {
			if strings.HasPrefix(k, "!picker/v1/browse/global/-/all/") || strings.HasPrefix(k, "!picker/v1/browse/repo/") && strings.Contains(k, "/all/") {
				raw, e := encodeTreeValue(k, full)
				if e != nil {
					t.Fatal(e)
				}
				leaves = append(leaves, item{Key: k, Value: raw})
			}
		}
	}
	digest := storage.SHA256Hex([]byte("synthetic bulk tree"))
	if e = w.Coordinator().Admit(t.Context(), "bulk-tree", digest, nil); e != nil {
		t.Fatal(e)
	}
	admitted := context.WithValue(t.Context(), writeAuthorityKey{}, writeAuthority{writer: w, owner: "bulk-tree", digest: digest})
	h.Project = bulkPickerTree(t, admitted, w, leaves, unary)
	if e = w.Coordinator().Complete(t.Context(), "bulk-tree", digest); e != nil {
		t.Fatal(e)
	}
	h.PublicationWitness = HeadWitness{}
	raw, e := json.Marshal(h)
	if e != nil {
		t.Fatal(e)
	}
	if e = store.Put(t.Context(), HeadKey, raw); e != nil {
		t.Fatal(e)
	}
	meter := &pickerMeter{qualifiedStore: store}
	s, e := OpenSnapshot(t.Context(), meter, nil)
	if e != nil {
		t.Fatal(e)
	}
	return s, meter
}

func TestPickerFirstPageWorkDoesNotScaleWithUnrelatedArchive(t *testing.T) {
	sizes := []int{10000, 100000}
	if testing.Short() {
		sizes = []int{1000, 10000}
	}
	for _, size := range sizes {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			s, meter := bulkPickerFixture(t, size, 0)
			q := PickerQuery{Scope: PickerScope{RepoKey: testPickerRepo('a')}, Membership: PickerBrowse, Limit: 10}
			p, e := s.PickerPage(t.Context(), q, "")
			if e != nil {
				t.Fatal(e)
			}
			if len(p.Rows) != 10 || p.Total != 50 || !p.More {
				t.Fatalf("page %+v", p)
			}
			if meter.metadata != 0 || meter.lists != 0 || meter.head != 1 || meter.coordinator != 1 || meter.nodes > 30 || p.Cost.Nodes > 30 || meter.bytes > 2<<20 {
				t.Fatalf("unbounded startup size %d: meter %+v cost %+v", size, meter, p.Cost)
			}
			t.Logf("archive=%d projectRoot=%s head=%d coordinator=%d nodeGET=%d nodeVisits=%d bytes=%d bodyGET=%d LIST=%d", size, s.head.Project.SHA256, meter.head, meter.coordinator, meter.nodes, p.Cost.Nodes, meter.bytes, meter.metadata, meter.lists)
			beforeHead, beforeCoord := meter.head, meter.coordinator
			if _, e = s.PickerPage(t.Context(), q, p.Next); e != nil {
				t.Fatal(e)
			}
			if meter.head != beforeHead+1 || meter.coordinator != beforeCoord+1 {
				t.Fatal("continuation did not use one fresh logical observation")
			}
		})
	}
}

func TestPickerUnaryTreeBudgetIsEnforcedBeforeFalseResults(t *testing.T) {
	s, meter := bulkPickerFixture(t, 100, 30)
	q := PickerQuery{Scope: PickerScope{RepoKey: testPickerRepo('a')}, Membership: PickerBrowse, Limit: 10, Budget: PickerBudget{Nodes: 10}}
	p, e := s.PickerPage(t.Context(), q, "")
	if !errors.Is(e, ErrPickerBudget) || p.Empty || len(p.Rows) > 0 || meter.nodes > 10 {
		t.Fatalf("%+v %+v %v", p, meter, e)
	}
	q.Budget = PickerBudget{Nodes: 256}
	p, e = s.PickerPage(t.Context(), q, "")
	if e != nil || len(p.Rows) != 10 || p.Total != 50 {
		t.Fatalf("%+v %v", p, e)
	}
}

func TestPickerMissingAndCorruptedNodesRefuseEmptyProof(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(strconv.FormatBool(corrupt), func(t *testing.T) {
			s, meter := bulkPickerFixture(t, 100, 0)
			if corrupt {
				if e := meter.Put(t.Context(), s.head.Project.Key, []byte("corrupted")); e != nil {
					t.Fatal(e)
				}
			} else {
				if e := meter.Delete(t.Context(), s.head.Project.Key); e != nil {
					t.Fatal(e)
				}
			}
			p, e := s.PickerPage(t.Context(), PickerQuery{Scope: PickerScope{All: true}, Membership: PickerBrowse, Limit: 10}, "")
			if e == nil || p.Empty {
				t.Fatalf("%+v %v", p, e)
			}
		})
	}
}

func TestPickerOverflowFirstScreenDoesNotReadFullMetadata(t *testing.T) {
	_, _, store, _ := pickerReaderFixture(t)
	meter := &pickerMeter{qualifiedStore: store}
	s, e := OpenSnapshot(t.Context(), meter, nil)
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.PickerPage(t.Context(), PickerQuery{Scope: PickerScope{All: true}, Membership: PickerBrowse, Limit: 100}, "")
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, item := range p.Rows {
		if item.Row.SessionID == "picker-007" {
			found = true
			if len(item.Row.Title) > PickerLabelBytes {
				t.Fatal("unbounded label")
			}
		}
	}
	if !found || meter.metadata != 0 || meter.lists != 0 {
		t.Fatalf("overflow first screen found=%v meter=%+v", found, meter)
	}
}
