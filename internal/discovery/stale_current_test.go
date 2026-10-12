package discovery

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Codex renames a rollout into archived_sessions/ (or back) and can update
// its thread index after the rename, or not at all. A current row naming a
// rollout that is no longer there is no evidence of where the thread is: it
// is passed over like an absent row rather than refusing the thread on every
// pass, and the thread's candidates still find the rollout by its ID.
func TestCurrentRowNamingAMovedRolloutIsPassedOver(t *testing.T) {
	lookup, ids, root := metadataFixture(t, 1)
	name := "rollout-2026-10-01T12-00-00-" + ids[0] + ".jsonl"
	old, archived := filepath.Join(root, "sessions", "nested", name), filepath.Join(root, "archived_sessions", name)
	if err := os.MkdirAll(filepath.Dir(archived), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, archived); err != nil {
		t.Fatal(err)
	}
	addHint(t, hintDatabase(t, root, true), ids[0], old, time.Now())

	set, err := lookup.Thread(t.Context(), ids[0])
	if err != nil || set.Current != nil {
		t.Fatalf("lookup: current=%v err=%v", set.Current, err)
	}
	if err := lookup.Check(t.Context(), ids[0], set.Revision); err != nil {
		t.Fatalf("lookup check: %v", err)
	}

	slice, err := lookup.MetadataInventory().(*metadataInventory).BeginValidationSlice(t.Context(), agentapi.CodexValidationLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slice.Close() }()
	set, err = slice.Thread(t.Context(), ids[0])
	if err != nil || set.Current != nil || !slices.ContainsFunc(set.Candidates, func(ref agentapi.SourceRef) bool { return ref.Path == archived }) {
		t.Fatalf("inventory: %+v %v", set, err)
	}
}
