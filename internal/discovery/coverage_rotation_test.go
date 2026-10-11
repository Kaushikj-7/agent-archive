package discovery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

// Every settled result must be consumed before eviction. Permanently absent
// early IDs cannot take all 256 slots forever or displace fresh completed work
// before that later consumer receives it.
func TestRequestedCoverageOver256MissingAndFreshConverges(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	ids := make([]string, 320)
	for n := range ids {
		ids[n] = fmt.Sprintf("00000000-0000-0000-0000-%012d", n+1)
	}
	wanted := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), 321, "sessions")
	ids = append(ids, wanted)
	seen := map[string]bool{}
	for pass := range 12 {
		l, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
		if err != nil {
			t.Fatal(err)
		}
		_, err = run(t.Context(), store, cfg, Options{Now: func() time.Time { return at.Add(time.Duration(pass) * time.Minute) }, Rollouts: l}, syntheticSupport)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			set, e := l.Thread(t.Context(), id)
			if e != nil {
				t.Fatal(e)
			}
			if set.Complete {
				seen[id] = true
				if id == wanted && len(set.Candidates) != 1 {
					t.Fatal(set)
				}
			}
		}
		if len(l.coverage.Requests) > 256 || len(l.threads) > 256 {
			t.Fatal("unbounded requests")
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		if len(seen) == len(ids) {
			return
		}
	}
	t.Fatalf("coverage starved: %d/%d", len(seen), len(ids))
}

func TestCoverageRotationPreservesUnfinishedAndUndelivered(t *testing.T) {
	t.Parallel()
	c := newCoverage([]string{"/synthetic"})
	for i := range 256 {
		c.request(strconv.Itoa(i))
	}
	if c.request("fresh") {
		t.Fatal("evicted unfinished")
	}
	for key, r := range c.Requests {
		r.AttemptEpoch = 1
		r.CompleteEpoch = 1
		c.Requests[key] = r
	}
	if c.request("fresh") {
		t.Fatal("evicted undelivered")
	}
	r := c.Requests["0"]
	r.DeliveredEpoch = 1
	c.Requests["0"] = r
	if !c.request("fresh") {
		t.Fatal("completed work pinned capacity")
	}
	if _, found := c.Requests["0"]; found {
		t.Fatal("wrong victim")
	}
}

func TestPhysicalRolloutEnrollsAndFindsRequestedFactsBeyondCache(t *testing.T) {
	t.Parallel()
	store, _, _, root := fixture(t)
	l, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	physical := "11111111-1111-4111-8111-111111111111"
	if refs, err := l.Rollout(t.Context(), physical); err != nil || len(refs) != 0 {
		t.Fatal(refs, err)
	}
	if _, found := l.coverage.Requests[physical]; !found {
		t.Fatal("missing dependency not enrolled")
	}
	for n := range maxCatalog {
		l.Observe(SourceDescriptor{Root: root, Locator: filepath.Join(root, "sessions", fmt.Sprintf("%d.jsonl", n))}, Fingerprint{}, &codexmeta.CodexIdentity{ThreadID: "other", RolloutID: strconv.Itoa(n)})
	}
	path := filepath.Join(root, "sessions", "late.jsonl")
	thread := "22222222-2222-4222-8222-222222222222"
	l.Observe(SourceDescriptor{Root: root, Locator: path}, Fingerprint{}, &codexmeta.CodexIdentity{ThreadID: thread, RolloutID: physical})
	if _, found := l.observations[path]; found {
		t.Fatal("fixture entered prunable cache")
	}
	refs, err := l.Rollout(t.Context(), physical)
	if err != nil || len(refs) != 1 || refs[0] != (agentapi.SourceRef{Path: path, Key: thread}) {
		t.Fatal(refs, err)
	}
}

func TestCoverageByteCapsPrecedeAccumulationAndSerialization(t *testing.T) {
	t.Parallel()
	c := newCoverage([]string{"/synthetic"})
	for n := range 256 {
		c.request(fmt.Sprintf("thread-%d", n))
	}
	for n := range 256 {
		for k := range 64 {
			path := "/synthetic/sessions/" + strings.Repeat("x", 3500) + fmt.Sprintf("-%d-%d", n, k)
			c.observe(SourceDescriptor{Root: "/synthetic", Locator: path}, Fingerprint{}, codexmeta.CodexIdentity{ThreadID: fmt.Sprintf("thread-%d", n)}, true)
		}
	}
	if c.byteBound() > maxCoverageBytes {
		t.Fatal("total fact bound exceeded")
	}
	anyOverflow := false
	for _, r := range c.Requests {
		if r.byteBound() > maxCoverageRequestBytes {
			t.Fatal("per request byte cap exceeded")
		}
		anyOverflow = anyOverflow || r.Overflow
	}
	if !anyOverflow {
		t.Fatal("fixture never exhausted cap")
	}
	raw, err := json.Marshal(c)
	if err != nil || len(raw) > maxCoverageBytes {
		t.Fatal(len(raw), err)
	}
}

func TestRequestedCoverageRootSetChangeDropsOldProof(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	l, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), 1, "sessions")
	l.coverage.request(id)
	_, err = run(t.Context(), store, cfg, Options{Rollouts: l}, syntheticSupport)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	changed, err := NewCodexRolloutLookup(t.Context(), store, []string{root, t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = changed.Close() }()
	if changed.coverage.proofEpoch != 0 || len(changed.coverage.Requests) != 0 {
		t.Fatal("old root proof reused")
	}
}

func TestLookupUnchangedCloseDoesNotRewriteCatalog(t *testing.T) {
	t.Parallel()
	store, cfg, _, root := fixture(t)
	l, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(t.Context(), store, cfg, Options{Rollouts: l}, syntheticSupport); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Home(), "discovery-catalog.json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.coverageDirty {
		t.Fatal("saved checkpoint still dirty")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("unchanged close wrote catalog")
	}
}
