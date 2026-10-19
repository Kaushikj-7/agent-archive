package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestDeleteLocalDataRemovesScheduledRecoveryEvidence(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.Save(home, config.Config{MachineID: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if complete, err := store.RecoverSessionIndexScheduled(context.Background(), time.Second); err != nil || !complete {
		t.Fatalf("recovery: %v %v", complete, err)
	}
	// A nanosecond application allowance checkpoints an actual owner inventory.
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	fixtureHome, _, _ := collectFixture(t, now)
	reg := theRegistration(t, fixtureHome)
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSessionIndexRecoveryNeeded(); err != nil {
		t.Fatal(err)
	}
	if complete, err := store.RecoverSessionIndexScheduled(context.Background(), time.Nanosecond); err != nil || complete {
		t.Fatalf("checkpoint: %v %v", complete, err)
	}
	owned := []string{"session-membership.json", "session-membership.lock", "session-index-recovery.json"}
	for _, name := range owned {
		if _, err := os.Stat(filepath.Join(home, name)); err != nil {
			t.Fatalf("recovery did not create %s: %v", name, err)
		}
	}
	userPath := filepath.Join(home, "my-notes.txt")
	if err := os.WriteFile(userPath, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	leftover, err := removeLocalState(home)
	if err != nil || !slices.Equal(leftover, []string{"my-notes.txt"}) {
		t.Fatalf("leftover=%q err=%v", leftover, err)
	}
	for _, name := range owned {
		if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("owned evidence retained: %s %v", name, err)
		}
	}
	if data, err := os.ReadFile(userPath); err != nil || string(data) != "keep" {
		t.Fatalf("unrelated data changed: %q %v", data, err)
	}
}

// Every entry the collector's local store can create under the data
// directory is one uninstall --delete-local-data removes. The list comes from
// the store itself, so a directory it adds later cannot be left behind.
func TestDeleteLocalDataRemovesEveryLocalStoreEntry(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "agent-archive")
	for _, entry := range state.OwnedEntries() {
		path := filepath.Join(home, entry)
		if strings.HasSuffix(entry, ".json") {
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Join(path, "x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	leftover, err := removeLocalState(home)
	if err != nil || len(leftover) != 0 {
		t.Fatalf("leftover = %v, err = %v: add the entry to localStateEntries", leftover, err)
	}
	// The directory itself goes once uninstall has released its locks.
	if remaining, err := os.ReadDir(home); err != nil || len(remaining) != 0 {
		t.Fatalf("data directory still holds %v (%v)", remaining, err)
	}
}

// handListedLocalState is localStateEntries as it was before uninstall took
// the store's entries from state.OwnedEntries, when every name was listed by
// hand. uninstall --delete-local-data removes exactly these, no more and no
// fewer; an entry added to either list later is added here too, on purpose.
var handListedLocalState = []string{
	"session-membership.json", "session-membership.lock", "session-index-recovery.json", "session-index-absence.json",
	"config.json", "setup-draft.json", "setup-transaction.json",
	"registrations", "requests", "request-locks", "import-locks", "published", "pending", "sessions", "sessions-v1", "sessions-packed-v1", "superseded", "pending-scans", "scan-signatures", "subagent-candidates", "forgotten", "refresh-skips", "listing-repairs", "imports",
	"generation-heads", "generation-nodes", "generation-recovery",
	// Shared durable storage owns this local quota lock namespace.
	"temporary-quota", "session-deletions",
	machineRegistrationFile, "discovery-catalog.json", "discovery-catalog.lock", "discovery-health.json", "discovery-refused-starts.json", "status.json", "session-index.json", "storage-clock.json", "storage-health.json", "capture-diagnostics.json", "diagnostics.lock", "admission-intents", "admission-intents.lock", "admission-replay-cursor.json", "application-versions.json",
	"collector.lock", "collector-lock.json", "collector.log", "collector-error.log",
	// Optional content indexing owns its cache, lifecycle fence and scheduler logs.
	"search", "search-worker.lock", "search.log", "search-error.log",
	"cache", "handoffs", "purge-plans", "issued", "issued.lock", "revocations", "revocations.lock", ownKeyFile,
	// Added with the credentials file store (Linux): one file per R2 key.
	"credentials",
}

// The entries uninstall deletes, the store's now taken from
// state.OwnedEntries, are the set it deleted when all were listed by hand.
func TestDeleteLocalDataListIsTheHandListedOne(t *testing.T) {
	t.Parallel()
	got := slices.Compact(slices.Sorted(slices.Values(append(state.OwnedEntries(), localStateEntries...))))
	want := slices.Compact(slices.Sorted(slices.Values(handListedLocalState)))
	if !slices.Equal(got, want) {
		t.Fatalf("uninstall deletes %q\nwant %q", got, want)
	}
}

// removeLocalState removes every entry agent-archive makes, the files moved
// aside from them, and write temp files; it keeps the lock files uninstall
// still holds, and leaves a user's own files in place and reported, including
// ones named like agent-archive's but not made by it.
func TestDeleteLocalDataRemovesOnlyItsOwnEntries(t *testing.T) {
	t.Parallel()
	home := filepath.Join(t.TempDir(), "agent-archive")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range handListedLocalState {
		if !strings.Contains(name, ".") {
			if err := os.MkdirAll(filepath.Join(home, name, "x"), 0o700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		write(name)
		if name != "collector.lock" {
			write(name + ".20260925T010203Z.corrupt")
		}
	}
	// Packed shards contain private native identities and must be removed
	// recursively with the rest of local state.
	packedShard := filepath.Join(home, "sessions-packed-v1", "x", "00.json")
	if err := os.WriteFile(packedShard, []byte(`{"native_session_id":"private-native"}`), 0600); err != nil {
		t.Fatal(err)
	}

	write(".pending-123")
	locks := []string{"setup.lock", "hooks.lock", "admission-intents.lock", "issued.lock", "search-worker.lock"}
	for _, name := range locks {
		write(name)
	}
	user := []string{"my-notes.txt", "status.json.bak", "registrations-old", "cache2", "notes.corrupt", "stranger.json.20260925T010203Z.corrupt"}
	for _, name := range user {
		write(name)
	}

	leftover, err := removeLocalState(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(packedShard); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("packed identity shard survived deletion: %v", err)
	}

	slices.Sort(leftover)
	if want := slices.Sorted(slices.Values(user)); !slices.Equal(leftover, want) {
		t.Fatalf("leftover %q, want %q", leftover, want)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	remaining := []string{}
	for _, e := range entries {
		remaining = append(remaining, e.Name())
	}
	want := slices.Sorted(slices.Values(append(append([]string{"collector.lock"}, locks...), user...)))
	if !slices.Equal(remaining, want) {
		t.Fatalf("data directory holds %q, want %q", remaining, want)
	}
}
