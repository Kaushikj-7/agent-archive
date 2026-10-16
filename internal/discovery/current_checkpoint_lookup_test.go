package discovery

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestRolloutLookupRevalidatesUnchangedCurrentAfterWALCheckpoint(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
	db := hintDatabase(t, root, true)
	addHint(t, db, id, path, at)
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lookup.Close() })
	set, err := lookup.Thread(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Refresh once, then let unrelated app activity checkpoint and restart
	// the WAL while the selected transcript remains unchanged.
	addHint(t, db, "unrelated-before-checkpoint", "unused", at.Add(time.Minute))
	if err := lookup.Check(t.Context(), id, set.Revision); err != nil {
		t.Fatal(err)
	}
	oldPrivate := lookup.indexes[root].snapshot.dir
	if _, err := db.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	addHint(t, db, "unrelated-after-checkpoint", "unused", at.Add(2*time.Minute))
	if err := lookup.Check(t.Context(), id, set.Revision); err != nil {
		t.Fatalf("unchanged selected thread rejected after unrelated checkpoint: %v", err)
	}
	if _, err := os.Stat(oldPrivate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discarded private index was not cleaned up: %v", err)
	}
	// Prime the replacement's first refresh so the next checkpoint must take
	// the incremental failure/rebuild path when the actual locator changes.
	addHint(t, db, "unrelated-before-locator-change", "unused", at.Add(3*time.Minute))
	if err := lookup.Check(t.Context(), id, set.Revision); err != nil {
		t.Fatal(err)
	}
	if !lookup.indexes[root].refreshed {
		t.Fatal("replacement view did not reach incremental revalidation")
	}
	oldPrivate = lookup.indexes[root].snapshot.dir
	// Rebuilding the private view must still reject an actual locator change.
	if _, err := db.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	other := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 2, "sessions")
	otherPath := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+other+".jsonl")
	if _, err := db.ExecContext(t.Context(), "UPDATE threads SET rollout_path=? WHERE id=?", otherPath, id); err != nil {
		t.Fatal(err)
	}
	if err := lookup.Check(t.Context(), id, set.Revision); agentapi.Failure(err) != agentapi.Changed {
		t.Fatalf("changed selected thread accepted: %v", err)
	}
	view := lookup.indexes[root]
	if view == nil || view.snapshot == nil || view.unavailable {
		t.Fatal("changed locator was not checked through an available rebuilt index")
	}
	if view.snapshot.dir == oldPrivate {
		t.Fatal("changed locator was not checked through a rebuilt private index")
	}
	if _, err := os.Stat(oldPrivate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed-locator checkpoint left the old private index: %v", err)
	}
}
