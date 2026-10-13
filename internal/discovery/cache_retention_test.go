package discovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"path/filepath"
	"testing"
	"time"
)

func TestRetainedHintsPruneWithoutResettingMigrationProof(t *testing.T) {
	t.Parallel()
	store, _, at, root := fixture(t)
	coverage := newCoverage([]string{root})
	coverage.request("wanted")
	path := filepath.Join(root, "sessions", "wanted.jsonl")
	req := coverage.Requests["wanted"]
	req.Candidates[path] = coverageCandidate{Source: SourceDescriptor{Root: root, Locator: path}, Identity: codexmeta.CodexIdentity{ThreadID: "wanted", RolloutID: "physical"}}
	coverage.Requests["wanted"] = req
	c := catalog{Version: catalogVersion, Roots: []string{root}, Cache: map[string]cached{}, Queue: []directory{{Root: root, Path: "sessions", Offset: 7}}, Coverage: coverage, Recovery: sourcefacts.RecoveryInventory{Context: "migration-context", Cursor: 7}}
	for n := range 8191 {
		c.Cache[fmt.Sprintf("%s/sessions/%d.jsonl", root, n)] = cached{Checked: at}
	}
	c.Cache[path] = cached{Checked: at.Add(-time.Hour)}
	catalogPath := filepath.Join(store.Home(), "discovery-catalog.json")
	if e := local.WriteCompact(catalogPath, c); e != nil {
		t.Fatal(e)
	}
	l, e := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if e != nil {
		t.Fatal(e)
	}
	if l.catalog == nil || len(l.catalog.Cache) != 8192 {
		t.Fatal("old bounded catalog reset")
	}
	proof := func(v any) []byte {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	before := proof(struct {
		Queue    []directory
		Coverage *coverageInventory
		Recovery sourcefacts.RecoveryInventory
	}{l.catalog.Queue, l.coverage, l.catalog.Recovery})
	pruneCatalog(l.catalog)
	if len(l.catalog.Cache) != maxObservationCache {
		t.Fatal("retention limit not applied")
	}
	if _, ok := l.catalog.Cache[path]; ok {
		t.Fatal("fixture did not evict expendable matching observation")
	}
	if e := local.WriteCompact(catalogPath, *l.catalog); e != nil {
		t.Fatal(e)
	}
	if e := l.Close(); e != nil {
		t.Fatal(e)
	}
	restored, e := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = restored.Close() }()
	after := proof(struct {
		Queue    []directory
		Coverage *coverageInventory
		Recovery sourcefacts.RecoveryInventory
	}{restored.catalog.Queue, restored.coverage, restored.catalog.Recovery})
	if !bytes.Equal(before, after) {
		t.Fatal("queue/coverage/recovery changed during pruning")
	}
	if len(restored.coverage.Requests["wanted"].Candidates) != 1 {
		t.Fatal("requested candidate lost")
	}
	if len(restored.catalog.Cache) != maxObservationCache {
		t.Fatal("restored cache reset")
	}
}

func TestRetainedHintsKeepTwoThousandWarmSourcesAndFreshProgress(t *testing.T) {
	if testing.Short() {
		t.Skip("full 2000 warm-source retention; weekly and exact candidates")
	}
	t.Parallel()
	testRetainedHintsWarmSourcesAndFreshProgress(t, 2000)
}

func TestRetainedHintsKeepSmallWarmSourcesAndFreshProgress(t *testing.T) {
	t.Parallel()
	testRetainedHintsWarmSourcesAndFreshProgress(t, 64)
}

func testRetainedHintsWarmSourcesAndFreshProgress(t *testing.T, count int) {
	t.Helper()
	store, cfg, at, root := fixture(t)
	for n := range count {
		writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), n+1, "sessions")
	}
	adapter := &measuredCoverageAdapter{SourceAdapter: codexAdapter{supported: syntheticSupport}}
	finish := func() {
		t.Helper()
		for pass := range 100 {
			health, err := runWithAdapters(t.Context(), store, cfg, Options{Now: func() time.Time { return at }}, []SourceAdapter{adapter})
			if err != nil {
				t.Fatal(err)
			}
			if !health.Pending {
				return
			}
			if pass == 99 {
				t.Fatal("warm source enumeration did not converge")
			}
		}
	}
	finish()
	first := adapter.headers
	finish()
	if adapter.headers != first {
		t.Fatal("retention cap reread a fitting warm source set", adapter.headers-first)
	}
	writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), count+1, "sessions")
	finish()
	if adapter.headers-first != 1 {
		t.Fatal("fresh source failed to progress independently", adapter.headers-first)
	}
}
