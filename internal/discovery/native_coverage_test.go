package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

func collectCoverageDirectory(t *testing.T, c *coverageInventory, root, path string, validation bool) {
	t.Helper()
	adapter := codexAdapter{}
	d := directory{Root: root, Path: path}
	for {
		batch, err := adapter.Enumerate(t.Context(), root, path, d.Offset)
		if err != nil {
			t.Fatal(err)
		}
		if batch.coverage == nil {
			t.Fatal("missing all-entry digest")
		}
		if validation {
			if !checkCoverageMembers(t.Context(), &catalog{Coverage: c, Cache: map[string]cached{}}, &Health{}, adapter, nil, batch.Entries) {
				t.Fatal("member probe refused")
			}
			c.validateBatch(d, *batch.coverage, batch.Complete)
		} else {
			c.recordBatch(d, *batch.coverage, batch.Continuation, batch.Complete, true)
		}
		if batch.Complete {
			break
		}
		if batch.Continuation == d.Offset {
			t.Fatal("no coverage progress")
		}
		d.Offset = batch.Continuation
	}
}

func TestCoverageRejectsUnrelatedHeaderRewriteWithUnchangedDirectoryStamp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "sessions")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "unrelated.jsonl")
	if err := os.WriteFile(path, []byte("unrelated thread"), 0600); err != nil {
		t.Fatal(err)
	}
	c := newCoverage([]string{root})
	c.request("requested")
	c.restart()
	collectCoverageDirectory(t, c, root, "sessions", false)
	collectCoverageDirectory(t, c, root, "archived_sessions", false)
	c.beginValidation()
	before := directoryCoverageStamp(root, "sessions")
	if err := os.WriteFile(path, []byte("now belongs to requested thread"), 0600); err != nil {
		t.Fatal(err)
	}
	changed := time.Now().Add(time.Second)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	if after := directoryCoverageStamp(root, "sessions"); after != before {
		t.Fatal("fixture changed directory metadata")
	}
	collectCoverageDirectory(t, c, root, "sessions", true)
	collectCoverageDirectory(t, c, root, "archived_sessions", true)
	c.Validation = nil
	c.FinalOffset = len(c.Directories)
	if c.finishValidation() || c.Requests["requested"].CompleteEpoch != 0 {
		t.Fatal("certified incomplete thread inventory")
	}
}

func TestCoverageNeedsWholeRequestedEpochAndValidation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	c := newCoverage([]string{root})
	c.Directories["started"] = coverageDirectory{}
	if !c.request("thread") {
		t.Fatal("request refused")
	}
	c.observe(SourceDescriptor{Locator: "early"}, Fingerprint{}, codexmeta.CodexIdentity{ThreadID: "thread"})
	if len(c.Requests["thread"].Candidates) != 0 {
		t.Fatal("midround fact claimed fresh coverage")
	}
	c.restart()
	c.observe(SourceDescriptor{Root: root, Locator: filepath.Join(root, "sessions", "current")}, Fingerprint{}, codexmeta.CodexIdentity{ThreadID: "thread"})
	collectCoverageDirectory(t, c, root, "sessions", false)
	collectCoverageDirectory(t, c, root, "archived_sessions", false)
	if c.Requests["thread"].CompleteEpoch != 0 {
		t.Fatal("observation certified before validation")
	}
	c.beginValidation()
	collectCoverageDirectory(t, c, root, "sessions", true)
	collectCoverageDirectory(t, c, root, "archived_sessions", true)
	c.Validation = nil
	c.FinalOffset = len(c.Directories)
	if !c.finishValidation() || c.Requests["thread"].CompleteEpoch != c.Epoch {
		t.Fatal(c)
	}
	c.restart()
	if c.Requests["thread"].CompleteEpoch != 0 {
		t.Fatal("old coverage survived new epoch")
	}
}

func TestCoverageRequestAndCandidateOverflowNeverCertifiesAbsence(t *testing.T) {
	t.Parallel()
	c := newCoverage([]string{"/synthetic"})
	for i := range maxCoverageRequests {
		if !c.request(fmt.Sprintf("thread-%d", i)) {
			t.Fatal(i)
		}
	}
	if c.request("overflow") {
		t.Fatal("unbounded requested inventory")
	}
	c.restart()
	for i := range 65 {
		c.observe(SourceDescriptor{Locator: fmt.Sprintf("/synthetic/%d", i)}, Fingerprint{}, codexmeta.CodexIdentity{ThreadID: "thread-0"})
	}
	if !c.Requests["thread-0"].Overflow || len(c.Requests["thread-0"].Candidates) != 64 {
		t.Fatal(c.Requests["thread-0"])
	}
	c.beginValidation()
	c.finishValidation()
	if c.Requests["thread-0"].CompleteEpoch != 0 {
		t.Fatal("overflow certified complete")
	}
}

func TestRequestedCoverageConvergesThroughSharedDiscoveryPasses(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(-time.Hour), 1, "sessions")
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lookup.Thread(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
	complete := false
	for pass := range 8 {
		current, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
		if err != nil {
			t.Fatal(err)
		}
		_, err = run(context.Background(), store, cfg, Options{Now: func() time.Time { return at.Add(time.Duration(pass) * time.Minute) }, Rollouts: current}, syntheticSupport)
		if err != nil {
			t.Fatal(err)
		}
		set, err := current.Thread(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		complete = set.Complete
		if complete && len(set.Candidates) != 1 {
			t.Fatal(set)
		}
		if err := current.Close(); err != nil {
			t.Fatal(err)
		}
		if complete {
			break
		}
	}
	if !complete {
		t.Fatal("requested coverage did not converge")
	}
}

func TestRequestedCoverageKeepsMatchesBeyondPrunableObservationCache(t *testing.T) {
	t.Parallel()
	store, _, _, root := fixture(t)
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	lookup.coverage.request("wanted")
	for i := range maxCatalog {
		path := filepath.Join(root, "sessions", fmt.Sprintf("%d.jsonl", i))
		lookup.Observe(SourceDescriptor{Kind: "", Root: root, Locator: path}, Fingerprint{}, &codexmeta.CodexIdentity{ThreadID: "other", RolloutID: strconv.Itoa(i)})
	}
	path := filepath.Join(root, "sessions", "late.jsonl")
	lookup.Observe(SourceDescriptor{Kind: "", Root: root, Locator: path}, Fingerprint{}, &codexmeta.CodexIdentity{ThreadID: "wanted", RolloutID: "physical"})
	if len(lookup.observations) > maxCatalog || len(lookup.coverage.Requests["wanted"].Candidates) != 1 {
		t.Fatal("prunable cache discarded requested evidence")
	}
	_ = lookup.Close()
}

func TestCompleteCoverageRefusesMissingOrInconsistentEpochProof(t *testing.T) {
	t.Parallel()
	c := newCoverage([]string{"/synthetic"})
	c.request("thread")
	c.Phase = coverageComplete
	request := c.Requests["thread"]
	request.CompleteEpoch = c.Epoch
	c.Requests["thread"] = request
	if err := c.validate([]string{"/synthetic"}); err == nil {
		t.Fatal("restored complete flag certified no enumeration")
	}
	lookup := &CodexRolloutLookup{coverage: c, byThread: map[string]map[string]struct{}{}, observations: map[string]rolloutObservation{}}
	set, err := lookup.withCandidates(t.Context(), "thread", agentapi.CodexRolloutSet{})
	if err != nil {
		t.Fatal(err)
	}
	if set.Complete {
		t.Fatal("unvalidated proof issued completeness")
	}
}
