package discovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

func TestRolloutLookupCurrentWALIdentityRefreshAndPrivateCleanup(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
	other := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 2, "sessions")
	otherPath := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+other+".jsonl")
	raw, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), other, id))
	if err := os.WriteFile(otherPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	db := hintDatabase(t, root, true)
	addHint(t, db, id, path, at)
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.Close() }()
	set, err := lookup.Thread(t.Context(), id)
	if err != nil || set.Current == nil || set.Current.Path != path || len(set.Candidates) != 1 {
		t.Fatal(set, err)
	}
	if err := lookup.Check(t.Context(), id, set.Revision); err != nil {
		t.Fatalf("%v; cause: %v", err, errors.Unwrap(err))
	}
	// Unrelated commits do not invalidate the current locator token.
	addHint(t, db, "unrelated", "unused", at.Add(time.Minute))
	if err := lookup.Check(t.Context(), id, set.Revision); err != nil {
		t.Fatalf("%v; cause: %v", err, errors.Unwrap(err))
	}
	// Later unrelated commits revalidate and replay into the same private view.
	privatePath := lookup.indexes[root].snapshot.path
	for n := range 8 {
		addHint(t, db, fmt.Sprintf("unrelated-%d", n), "unused", at.Add(time.Minute))
		if err := lookup.Check(t.Context(), id, set.Revision); err != nil {
			t.Fatalf("append %d: %v; cause: %v", n, err, errors.Unwrap(err))
		}
		if lookup.indexes[root].snapshot.path != privatePath {
			t.Fatal("second whole refresh")
		}
	}
	// The actual changed selection is read from the incrementally replayed SQL.

	if _, err := db.ExecContext(t.Context(), "UPDATE threads SET rollout_path=? WHERE id=?", otherPath, id); err != nil {
		t.Fatal(err)
	}
	if err := lookup.Check(t.Context(), id, set.Revision); agentapi.Failure(err) != agentapi.Changed {
		t.Fatal(err)
	}
	var privatePaths []string
	for _, view := range lookup.indexes {
		if view.snapshot != nil {
			privatePaths = append(privatePaths, view.snapshot.dir)
		}
	}
	if err := lookup.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range privatePaths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if _, err := lookup.Thread(t.Context(), id); !errors.Is(err, agentapi.ErrClosed) {
		t.Fatal(err)
	}
	// A new pass observes the newly committed selection.
	next, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Close() }()
	set, err = next.Thread(t.Context(), id)
	if err != nil || set.Current == nil || set.Current.Path != otherPath {
		t.Fatal(set, err)
	}
}

func TestRolloutLookupRefusesWrongThreadAndOutsideCurrentPaths(t *testing.T) {
	t.Parallel()
	for _, outside := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong_thread", true: "outside"}[outside], func(t *testing.T) {
			t.Parallel()
			store, cfg, at, root := fixture(t)
			id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
			otherRoot := root
			if outside {
				otherRoot = t.TempDir()
			}
			other := writeRollout(t, otherRoot, cfg.Archive.Projects[0].Root, at, 2, "sessions")
			path := filepath.Join(otherRoot, "sessions", "rollout-2026-10-01T12-00-00-"+other+".jsonl")
			db := hintDatabase(t, root, true)
			addHint(t, db, id, path, at)
			lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lookup.Close() }()
			if _, err := lookup.Thread(t.Context(), id); agentapi.Failure(err) != agentapi.Unsafe {
				t.Fatal(err)
			}
		})
	}
}

func TestRolloutLookupObservedFactsAreHintsAndRevisionChanges(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at, 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
	header := sourcefacts.ReadHeader(t.Context(), root, path)
	if header.Identity == nil {
		t.Fatal(header)
	}
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.Close() }()
	identity := *header.Identity
	source := SourceDescriptor{Kind: archive.SourceKindFile, Root: root, Locator: path, StableKey: id}
	lookup.Observe(source, Fingerprint{Size: 1}, &identity)
	initial, err := lookup.Thread(t.Context(), id)
	if err != nil || initial.Current != nil || len(initial.Candidates) != 1 {
		t.Fatal(initial, err)
	}
	identity.RootID = "caller-mutation"
	if err := lookup.Check(t.Context(), id, initial.Revision); err != nil {
		t.Fatal(err)
	}
	lookup.Observe(source, Fingerprint{Size: 2}, header.Identity)
	if err := lookup.Check(t.Context(), id, initial.Revision); agentapi.Failure(err) != agentapi.Changed {
		t.Fatal(err)
	}
	physical, err := lookup.Rollout(t.Context(), id)
	if err != nil || len(physical) != 1 || physical[0].Path != path {
		t.Fatal(physical, err)
	}
}

func TestRolloutLookupBoundsCurrentIDsAndCancellation(t *testing.T) {
	t.Parallel()
	store, _, _, root := fixture(t)
	lookup, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lookup.Close() }()
	for i := range maxCachedThreads + 2 {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012x", i)
		if _, err := lookup.Thread(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	if len(lookup.threads) > maxCachedThreads {
		t.Fatal("unbounded pass current cache")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := lookup.Thread(ctx, "11111111-1111-1111-1111-111111111111"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRolloutLookupTypedCopyOwnsOptionalIdentityValues(t *testing.T) {
	t.Parallel()
	store, _, _, root := fixture(t)
	l, err := NewCodexRolloutLookup(t.Context(), store, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	ordinal := uint64(7)
	identity := codexmeta.CodexIdentity{ThreadID: "thread", RolloutID: "physical", ForkOrdinal: &ordinal, HistoryBase: &codexmeta.CodexHistoryPosition{RolloutID: "base", EndOrdinal: 7}}
	path := filepath.Join(root, "sessions", "ordinary.jsonl")
	l.Observe(SourceDescriptor{Root: root, Locator: path}, Fingerprint{}, &identity)
	ordinal = 9
	identity.HistoryBase.EndOrdinal = 9
	got := l.observations[path].identity
	if *got.ForkOrdinal != 7 || got.HistoryBase.EndOrdinal != 7 {
		t.Fatal("caller altered pass evidence")
	}
}
