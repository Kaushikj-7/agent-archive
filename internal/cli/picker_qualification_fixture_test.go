package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// This wire builder is test-only. It writes a complete immutable universe once,
// avoiding millions of obsolete COW nodes. The format adapter and production
// construction equivalence test are separate from the independent UI oracle.
const qualificationProject = "/synthetic/qualification"

const qualificationRepo = "repo-1111111111111111"

type qualificationLeaf struct {
	Key   string          `json:"Key"`
	Value json.RawMessage `json:"Value"`
}

type qualificationBranch struct {
	Max   string            `json:"Max"`
	Ref   catalog.ObjectRef `json:"Ref"`
	Count uint64            `json:"Count"`
}

type qualificationNode struct {
	Count    uint64                `json:"Count"`
	Leaves   []qualificationLeaf   `json:"leaves,omitempty"`
	Children []qualificationBranch `json:"children,omitempty"`
}

type qualificationFixture struct {
	head     catalog.CatalogHead
	raw      *qualifiedCLIStore
	store    *catalog.Store
	metadata []archive.Metadata
	manifest string
	height   int
}

func qualificationMetadata(size, generation int) []archive.Metadata {
	out := make([]archive.Metadata, size)
	base := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	source := []byte("synthetic source placeholder\n")
	for i := range out {
		id := fmt.Sprintf("%08x-session", i)
		parent := ""
		if i%20 >= 3 {
			parent = fmt.Sprintf("%08x-session", i/20*20)
		}
		projectID, repo, projectName := archive.ProjectID(qualificationProject), qualificationRepo, "Qualification"
		if i >= 1200 {
			projectID, repo, projectName = "unrelated", "repo-2222222222222222", "Unrelated"
		}
		// Keep membership classes disjoint and include known repository conflicts.
		if i%7 == 0 {
			repo = ""
		}
		if i%17 == 0 {
			repo = "repo-2222222222222222"
		}
		var replay *archive.Replay
		if i%31 == 0 {
			replay = &archive.Replay{}
		}
		var turns *int
		if i%20 == 2 {
			positive := 1
			turns = &positive
		}
		if i%13 == 0 {
			zero := 0
			turns = &zero
		}
		captured, title := base.Add(-time.Duration(i/3)*time.Minute), fmt.Sprintf("Q%06d", i)
		if generation != 0 {
			captured, title = captured.Add(time.Hour), fmt.Sprintf("G%06d", i)
		}
		m := archive.Metadata{SchemaVersion: 1, SessionID: id, Harness: archive.Harness{Name: "codex"}, CapturedAt: captured, Title: title, ProjectName: projectName, ProjectID: projectID, RepoKey: repo, ParentSessionID: parent, Replay: replay, Counts: archive.Counts{Turns: turns}, SourceBundle: archive.SourceReference{Key: fmt.Sprintf("sessions/codex/%s/source.%s.jsonl.gz", id, storage.SHA256Hex(source)), SHA256: storage.SHA256Hex(source), CompressedBytes: len(source)}}
		out[i] = m
	}
	return out
}

func qualificationJSON(tb testing.TB, value any) []byte {
	tb.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		tb.Fatal(err)
	}
	return raw
}

func qualificationPut(tb testing.TB, store storage.ObjectStore, key string, raw []byte) {
	tb.Helper()
	if err := store.Put(context.Background(), key, raw); err != nil {
		tb.Fatal(err)
	}
}

func qualificationRef(tb testing.TB, store storage.ObjectStore, kind string, raw []byte) catalog.ObjectRef {
	tb.Helper()
	sum := storage.SHA256Hex(raw)
	ref := catalog.ObjectRef{Key: "catalog-v4/" + kind + "/" + sum + ".json", SHA256: sum}
	qualificationPut(tb, store, ref.Key, raw)
	return ref
}

func qualificationTree(tb testing.TB, store storage.ObjectStore, leaves []qualificationLeaf) (catalog.ObjectRef, int) {
	tb.Helper()
	if len(leaves) == 0 {
		return catalog.ObjectRef{}, 0
	}
	slices.SortFunc(leaves, func(a, b qualificationLeaf) int { return strings.Compare(a.Key, b.Key) })
	var layer []qualificationBranch
	for start := 0; start < len(leaves); {
		end := start
		encodedBytes := 64 // Count/leaves framing, including decimal count and commas.
		for end < len(leaves) && end-start < 32 {
			leafBytes := len(qualificationJSON(tb, leaves[end])) + 1
			if encodedBytes+leafBytes > 128<<10 {
				break
			}
			encodedBytes += leafBytes
			end++
		}
		if end == start {
			tb.Fatal("fixture leaf exceeds production node limit")
		}
		n := qualificationNode{Count: uint64(end - start), Leaves: leaves[start:end]}
		raw := qualificationJSON(tb, n)
		if len(raw) > 128<<10 {
			tb.Fatal("fixture leaf framing exceeds production node limit")
		}
		layer = append(layer, qualificationBranch{Max: leaves[end-1].Key, Ref: qualificationRef(tb, store, "nodes", raw), Count: n.Count})
		start = end
	}
	height := 1
	for len(layer) > 1 {
		var next []qualificationBranch
		for start := 0; start < len(layer); start += 32 {
			end := min(start+32, len(layer))
			n := qualificationNode{Children: layer[start:end]}
			for _, b := range n.Children {
				n.Count += b.Count
			}
			raw := qualificationJSON(tb, n)
			if len(raw) > 128<<10 {
				tb.Fatal("fixture branch exceeds production node limit")
			}
			next = append(next, qualificationBranch{Max: layer[end-1].Max, Ref: qualificationRef(tb, store, "nodes", raw), Count: n.Count})
		}
		layer = next
		height++
	}
	return layer[0].Ref, height
}

func qualificationInverse(key string) string {
	raw := []byte(key)
	for i := range raw {
		raw[i] = ^raw[i]
	}
	return hex.EncodeToString(raw)
}

func qualificationOrderKeys(key string, m archive.Metadata) [3][]string {
	capture := m.CapturedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
	activity := listingindex.ActivityTime(m).UTC().Format("2006-01-02T15:04:05.000000000Z")
	inverse := qualificationInverse(key)
	ordinary := [3]string{capture + "/" + inverse, activity + "/" + capture + "/" + inverse, "project/" + hex.EncodeToString([]byte(m.ProjectID)) + "/" + key}
	var result [3][]string
	prefix := catalog.OrderPrefix(m.Replay != nil)
	if m.IsChild() {
		prefix = "!children/ordinary/"
		if m.Replay != nil {
			prefix = "!children/replay/"
		}
	}
	for i, k := range ordinary {
		result[i] = []string{k, prefix + k}
	}
	if m.ParentSessionID != "" {
		result[2] = append(result[2], catalog.ChildPrefix(m.Harness.Name, m.ParentSessionID, m.Replay != nil)+ordinary[0])
	}
	return result
}

func newQualificationFixture(tb testing.TB, metadata []archive.Metadata) qualificationFixture {
	tb.Helper()
	raw := &qualifiedCLIStore{storagetest.NewMemoryStore()}
	head := qualificationSeed(tb, raw)
	entries := make([]catalog.CatalogEntry, len(metadata))
	children := map[string][2]uint64{}
	for _, m := range metadata {
		if m.ParentSessionID != "" {
			n := children[m.Harness.Name+"/"+m.ParentSessionID]
			which := 0
			if m.Replay != nil {
				which = 1
			}
			n[which]++
			children[m.Harness.Name+"/"+m.ParentSessionID] = n
		}
	}
	source := []byte("synthetic source placeholder\n")
	manifest := qualificationJSON(tb, metadata)
	for i, m := range metadata {
		body := qualificationJSON(tb, m)
		ref := qualificationRef(tb, raw, "metadata", body)
		qualificationPut(tb, raw, m.SourceBundle.Key, source)
		n := children[m.Harness.Name+"/"+m.SessionID]
		entry := catalog.CatalogEntry{Revision: fmt.Sprintf("fixture-%08d", i), Metadata: ref, Summary: m, OrdinaryChildren: n[0], ReplayChildren: n[1]}
		entries[i] = qualificationEntry(tb, entry)
	}
	var roots [4]catalog.ObjectRef
	height := 0
	for tree := range roots {
		var leaves []qualificationLeaf
		for i, m := range metadata {
			key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
			if err != nil {
				tb.Fatal(err)
			}
			entry := entries[i]
			if tree == 0 {
				leaves = append(leaves, qualificationLeaf{key, qualificationJSON(tb, struct {
					Revision string
					Entry    *catalog.CatalogEntry
				}{entry.Revision, &entry})})
				continue
			}
			order := qualificationOrderKeys(key, m)
			for _, k := range order[tree-1] {
				leaves = append(leaves, qualificationLeaf{k, qualificationJSON(tb, entry)})
			}
			if tree == 3 {
				leaves = append(leaves, qualificationPickerLeaves(tb, key, entry)...)
			}
		}
		var h int
		roots[tree], h = qualificationTree(tb, raw, leaves)
		height = max(height, h)
	}
	head.Identity, head.Capture, head.Activity, head.Project = roots[0], roots[1], roots[2], roots[3]
	// This is a synthetic seed, not an acknowledged user publication. No forged
	// publication witness/receipt is created. The real activated coordinator and
	// stronger format descriptor are retained; all user data roots are complete.
	head.PublicationWitness = catalog.HeadWitness{}
	head.PredecessorWitness = catalog.HeadWitness{}
	head.Previous = catalog.ObjectRef{}
	qualificationPut(tb, raw, catalog.HeadKey, qualificationJSON(tb, head))
	wrapped, err := catalog.Wrap(raw)
	if err != nil {
		tb.Fatal(err)
	}
	return qualificationFixture{head: head, raw: raw, store: wrapped, metadata: metadata, manifest: storage.SHA256Hex(manifest), height: height}
}
