package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Keep the standard encoder as the independent budget/reference path. The
// bounded synthetic tree crosses fanout and byte-limit boundaries without
// repeating the 100k qualification campaign on the development machine.
func qualificationReferenceTree(t *testing.T, store *storagetest.MemoryStore, leaves []qualificationLeaf) (catalog.ObjectRef, int) {
	t.Helper()
	slices.SortFunc(leaves, func(a, b qualificationLeaf) int { return strings.Compare(a.Key, b.Key) })
	var layer []qualificationBranch
	for start := 0; start < len(leaves); {
		end, encodedBytes := start, 64
		for end < len(leaves) && end-start < 32 {
			leafBytes := len(qualificationJSON(t, leaves[end])) + 1
			if encodedBytes+leafBytes > 128<<10 {
				break
			}
			encodedBytes += leafBytes
			end++
		}
		if end == start {
			t.Fatal("reference leaf exceeds production limit")
		}
		node := qualificationNode{Count: uint64(end - start), Leaves: leaves[start:end]}
		raw := qualificationJSON(t, node)
		if len(raw) > 128<<10 {
			t.Fatal("reference framing exceeds production limit")
		}
		layer = append(layer, qualificationBranch{Max: leaves[end-1].Key, Ref: qualificationRef(t, store, "nodes", raw), Count: node.Count})
		start = end
	}
	height := 1
	for len(layer) > 1 {
		var next []qualificationBranch
		for start := 0; start < len(layer); start += 32 {
			end := min(start+32, len(layer))
			node := qualificationNode{Children: layer[start:end]}
			for _, b := range node.Children {
				node.Count += b.Count
			}
			raw := qualificationJSON(t, node)
			next = append(next, qualificationBranch{Max: layer[end-1].Max, Ref: qualificationRef(t, store, "nodes", raw), Count: node.Count})
		}
		layer = next
		height++
	}
	return layer[0].Ref, height
}

func TestQualificationTreeWireMatchesCanonicalEncoder(t *testing.T) {
	var leaves []qualificationLeaf
	for i := range 96 {
		// Typed encoding includes HTML/Unicode/control escapes and varied payload
		// sizes, including values which make a node hit the byte boundary first.
		value := qualificationJSON(t, map[string]any{"message": "<>&\u2028\n\t" + strings.Repeat("x", 7000+i*17), "index": i})
		leaf := qualificationLeaf{Key: fmt.Sprintf("%04d/\"\\\n", i), Value: json.RawMessage(value)}
		if got, want := qualificationLeafSize(t, leaf), len(qualificationJSON(t, leaf)); got != want {
			t.Fatalf("leaf %d size %d, want %d", i, got, want)
		}
		leaves = append(leaves, leaf)
	}
	empty := qualificationLeaf{Key: "nil value"}
	if got, want := qualificationLeafSize(t, empty), len(qualificationJSON(t, empty)); got != want {
		t.Fatalf("nil size %d, want %d", got, want)
	}
	oldStore, newStore := storagetest.NewMemoryStore(), storagetest.NewMemoryStore()
	started := time.Now()
	oldRoot, oldHeight := qualificationReferenceTree(t, oldStore, slices.Clone(leaves))
	t.Logf("reference wire encoder: %s", time.Since(started))
	started = time.Now()
	newRoot, newHeight := qualificationTree(t, newStore, slices.Clone(leaves))
	t.Logf("reused canonical value budget: %s", time.Since(started))
	if oldRoot != newRoot || oldHeight != newHeight {
		t.Fatal("wire optimization changed root reference or tree height")
	}
	oldObjects, err := oldStore.List(t.Context(), "")
	must(t, err)
	newObjects, err := newStore.List(t.Context(), "")
	must(t, err)
	if len(oldObjects) != len(newObjects) {
		t.Fatal("wire optimization changed node count")
	}
	for _, object := range oldObjects {
		oldBytes, err := oldStore.Get(t.Context(), object.Key)
		must(t, err)
		newBytes, err := newStore.Get(t.Context(), object.Key)
		must(t, err)
		if !bytes.Equal(oldBytes, newBytes) {
			t.Fatal("wire optimization changed immutable node bytes")
		}
	}
}
