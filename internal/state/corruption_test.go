package state

import (
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"
)

// Every entry a Store owns has a decided answer to "what if this file no
// longer decodes?". A directory added without one fails here, before it can
// become a file that fails its session on every pass with no way out.
func TestEveryOwnedEntryHasACorruptionPolicy(t *testing.T) {
	owned := OwnedEntries()
	for _, entry := range owned {
		if _, ok := corruptionPolicies[entry]; !ok {
			t.Errorf("%s has no corruption policy", entry)
		}
	}
	for entry := range corruptionPolicies {
		if !slices.Contains(owned, entry) {
			t.Errorf("corruption policy for %s, which the store does not own", entry)
		}
	}
}

func writeCorrupt(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"archive_se`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Pending journals can be the only evidence of an interrupted publication.
// Neither ordinary readers nor collector passes may forget unreadable bytes.
func TestCorruptPendingRemainsActionableInEveryPass(t *testing.T) {
	store := newTestStore(t)
	path := store.pendingPath("session-1")
	writeCorrupt(t, path)
	for _, reader := range []*Store{store, store.ForCollectorPass(), store.ForCollectorPass()} {
		if _, found, err := reader.LoadPending("session-1"); !found || err == nil || errors.Is(err, ErrQuarantined) {
			t.Fatalf("pending: %v %v", found, err)
		}
		if pending, err := reader.HasPending("session-1"); err != nil || !pending {
			t.Fatal(pending, err)
		}
	}
	if _, err := os.Stat(path); err != nil || store.LostPublication("session-1") {
		t.Fatal("pending evidence was moved", err)
	}
	legacy := quarantinePath(path)
	if err := os.WriteFile(legacy, []byte("older quarantined evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if !store.LostPublication("session-1") || len(store.QuarantinedFiles()) != 1 {
		t.Fatal("older quarantined evidence became invisible")
	}
}

// A journal of an unsupported shape remains evidence for a compatible reader.
func TestPendingOfAnotherShapeIsNeverMovedAside(t *testing.T) {
	pass := newTestStore(t).ForCollectorPass()
	path := pass.pendingPath("session-1")
	if err := os.WriteFile(path, []byte(`{"source_key":7,"source_bytes":"AAAA"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pass.LoadPending("session-1"); err == nil || errors.Is(err, ErrQuarantined) {
		t.Fatalf("err = %v, want a reported error that moved nothing", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a pending publication of another shape was moved: %v", err)
	}
	if pass.LostPublication("session-1") {
		t.Fatal("reported as lost")
	}
}

// Corruption stays bounded on lookup; explicit off-hot-path recovery preserves
// the admitted archive ID and durably distinguishes a genuinely absent key.
func TestCorruptSessionIndexIsRecoveredFromTheRegistration(t *testing.T) {
	store := newTestStore(t)
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native-1"}
	reg, err := store.RegisterNewSession(key, func(id string) archive.SessionRegistration {
		r := registration(t)
		r.ArchiveSessionID = id
		return r
	})
	if err != nil {
		t.Fatal(err)
	}
	writeCorrupt(t, qualifiedSessionIndexPath(store.home, key))
	if _, _, err := store.ArchiveSessionID(key); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("bounded lookup: %v", err)
	}
	if err := store.RequestSessionIndexRecovery(key); err != nil {
		t.Fatal(err)
	}
	if err := recoverytest.Exhaust(context.Background(), store, SessionIndexRecoverySlice, true); err != nil {
		t.Fatal(err)
	}
	id, found, err := store.ArchiveSessionID(key)
	if err != nil || !found || id != reg.ArchiveSessionID {
		t.Fatalf("recovered %q %v %v", id, found, err)
	}
	if id, created, err := store.EnsureArchiveSessionID(key); err != nil || created || id != reg.ArchiveSessionID {
		t.Fatalf("ensure = %q %v %v", id, created, err)
	}
	absent := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: "native-2"}
	writeCorrupt(t, nativeSessionIndexPath(store.home, absent.NativeID))
	if _, _, err := store.EnsureArchiveSessionID(absent); !errors.Is(err, ErrSessionIndexRecoveryRequired) {
		t.Fatalf("corrupt orphan: %v", err)
	}
	if err := store.RequestSessionIndexRecovery(absent); err != nil {
		t.Fatal(err)
	}
	if err := recoverytest.Exhaust(context.Background(), store, SessionIndexRecoverySlice, true); err != nil {
		t.Fatal(err)
	}
	if id, created, err := store.EnsureArchiveSessionID(absent); err != nil || !created || id == "" {
		t.Fatalf("fresh = %q %v %v", id, created, err)
	}
}

// A removal record that no longer decodes still says the session was
// removed, so backfill keeps skipping it.
func TestCorruptRemovalRecordStillCountsAsRemoved(t *testing.T) {
	store := newTestStore(t)
	if err := store.RecordRemoval("codex", "native-1", RemovalReasonRetention, time.Now()); err != nil {
		t.Fatal(err)
	}
	writeCorrupt(t, removalPath(store.home, "codex", "native-1"))
	if _, found, err := store.Removal("codex", "native-1"); err != nil || !found {
		t.Fatalf("%v %v", found, err)
	}
}
