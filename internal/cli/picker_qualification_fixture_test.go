package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
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

// qualificationLeafSize counts the wire produced by qualificationJSON without
// re-encoding a complete entry just for the node budget. Fixture values already
// come from that canonical encoder; the final node still uses it and checks its
// actual bytes against the production limit.
func qualificationLeafSize(tb testing.TB, leaf qualificationLeaf) int {
	tb.Helper()
	valueBytes := len(leaf.Value)
	if valueBytes == 0 {
		valueBytes = len("null")
	}
	return len(qualificationJSON(tb, leaf.Key)) + valueBytes + len(`{"Key":,"Value":}`)
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
			leafBytes := qualificationLeafSize(tb, leaves[end]) + 1
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

func buildQualificationFixture(tb testing.TB, metadata []archive.Metadata) qualificationFixture {
	tb.Helper()
	raw := &qualifiedCLIStore{storagetest.NewMemoryStore()}
	head := qualificationSeed(tb, raw)
	entries := make([]catalog.CatalogEntry, len(metadata))
	entryWire := make([]json.RawMessage, len(metadata))
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
		entryWire[i] = qualificationJSON(tb, entries[i])
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
				leaves = append(leaves, qualificationLeaf{k, entryWire[i]})
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
	var privateMetadata []archive.Metadata
	if err := json.Unmarshal(manifest, &privateMetadata); err != nil {
		tb.Fatal(err)
	}
	return qualificationFixture{head: head, raw: raw, store: wrapped, metadata: privateMetadata, manifest: storage.SHA256Hex(manifest), height: height}
}

// qualificationSeeds retains wire bytes only. Writable stores, providers and
// nested metadata never escape from a seed into a command or another test.
var qualificationSeeds = struct {
	sync.Mutex
	fixtures map[string]qualificationFrozenFixture
}{fixtures: make(map[string]qualificationFrozenFixture)}

type qualificationFrozenFixture struct {
	objects  storagetest.MemorySnapshot
	head     []byte
	metadata []byte
	manifest string
	height   int
}

func freezeQualificationFixture(tb testing.TB, fixture qualificationFixture) qualificationFrozenFixture {
	tb.Helper()
	snapshot, err := storagetest.SnapshotMemoryStore(tb.Context(), fixture.raw.MemoryStore)
	if err != nil {
		tb.Fatal(err)
	}
	return qualificationFrozenFixture{objects: snapshot, head: qualificationJSON(tb, fixture.head), metadata: qualificationJSON(tb, fixture.metadata), manifest: fixture.manifest, height: fixture.height}
}

func (seed qualificationFrozenFixture) clone(tb testing.TB) qualificationFixture {
	tb.Helper()
	memory, err := seed.objects.Clone(tb.Context())
	if err != nil {
		tb.Fatal(err)
	}
	fixture := qualificationFixture{raw: &qualifiedCLIStore{memory}, manifest: seed.manifest, height: seed.height}
	if err := json.Unmarshal(seed.head, &fixture.head); err != nil {
		tb.Fatal(err)
	}
	if err := json.Unmarshal(seed.metadata, &fixture.metadata); err != nil {
		tb.Fatal(err)
	}
	fixture.store, err = catalog.Wrap(fixture.raw)
	if err != nil {
		tb.Fatal(err)
	}
	return fixture
}

// newQualificationFixture caches ordinary universes, then returns a writable
// deep copy. The manifest keys all metadata fields, including scope, ordering,
// overflow, generation and source references. Large performance universes retain
// the uncached constructor so one-use 10k/100k seeds do not live for the whole
// package process or add a second frozen copy to their setup peak.
func newQualificationFixture(tb testing.TB, metadata []archive.Metadata) qualificationFixture {
	tb.Helper()
	if len(metadata) > 1400 {
		return buildQualificationFixture(tb, metadata)
	}
	key := storage.SHA256Hex(qualificationJSON(tb, metadata))
	seed := func() qualificationFrozenFixture {
		qualificationSeeds.Lock()
		defer qualificationSeeds.Unlock()
		seed, ok := qualificationSeeds.fixtures[key]
		if !ok {
			seed = freezeQualificationFixture(tb, buildQualificationFixture(tb, metadata))
			qualificationSeeds.fixtures[key] = seed
		}
		return seed
	}()
	return seed.clone(tb)
}

func cloneQualificationFixture(tb testing.TB, fixture qualificationFixture) qualificationFixture {
	tb.Helper()
	// Each command owns its map, provider and metadata. MemoryStore's internal
	// immutable bodies can be reused without another multi-gigabyte wire copy.
	memory, err := storagetest.CloneMemoryStore(tb.Context(), fixture.raw.MemoryStore)
	if err != nil {
		tb.Fatal(err)
	}
	cloned := qualificationFixture{raw: &qualifiedCLIStore{memory}, manifest: fixture.manifest, height: fixture.height}
	if err := json.Unmarshal(qualificationJSON(tb, fixture.head), &cloned.head); err != nil {
		tb.Fatal(err)
	}
	if err := json.Unmarshal(qualificationJSON(tb, fixture.metadata), &cloned.metadata); err != nil {
		tb.Fatal(err)
	}
	cloned.store, err = catalog.Wrap(cloned.raw)
	if err != nil {
		tb.Fatal(err)
	}
	return cloned
}
