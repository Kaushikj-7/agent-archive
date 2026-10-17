package retention

import (
	"bytes"
	"context"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func corruptFile(t *testing.T, local *state.Store, rel string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(local.Home(), rel), []byte(`{"trunc`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sessionObjects(t *testing.T, store storage.ObjectStore, id string) int {
	t.Helper()
	objects, err := store.List(context.Background(), "sessions/codex/"+id+"/")
	if err != nil {
		t.Fatal(err)
	}
	return len(objects)
}

// Corruption is not clean state loss: quarantine retains anonymous authority
// and global owed work must prevent guessing a replacement or deletion.
func TestCorruptPublishedAuthorityRemainsOwedBeforeRetention(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", dir, t0)
	if err := os.Remove(filepath.Join(dir, "s1.jsonl")); err != nil {
		t.Fatal(err)
	}
	before := sessionObjects(t, store, "s1")
	corruptFile(t, local, "published/s1.json")
	if r := collect(t, local, store, t0.Add(time.Hour)); !errors.Is(r.Errors["s1"], state.ErrQuarantined) || len(r.Published) != 0 {
		t.Fatalf("first quarantine: %+v", r)
	}
	quarantined := local.QuarantinedFiles()
	if len(quarantined) != 1 {
		t.Fatalf("quarantined %v", quarantined)
	}
	retained, err := os.ReadFile(filepath.Join(local.Home(), quarantined[0]))
	if err != nil || !bytes.Equal(retained, []byte(`{"trunc`)) {
		t.Fatalf("exact corrupt authority not retained: %q %v", retained, err)
	}
	bindings := builtin.NewBuiltins()
	result, err := collector.Run(t.Context(), local, store, collector.Options{Sources: bindings, Parsers: bindings, MachineID: "m", Now: func() time.Time { return t0.Add(2 * time.Hour) }})
	if !errors.Is(err, state.ErrDurableStorageRecovery) || len(result.Published) != 0 {
		t.Fatalf("anonymous owed retry: %+v %v", result, err)
	}
	swept := sweep(t, local, store, t0.Add(retentionWindow+time.Hour), Options{})
	if !errors.Is(swept.Errors["s1"], state.ErrDurableStorageRecovery) || len(swept.DeletedSessions) != 0 {
		t.Fatalf("corrupt authority deletion: %+v", swept)
	}
	if after := sessionObjects(t, store, "s1"); after != before {
		t.Fatalf("remote objects changed %d→%d", before, after)
	}
	again, err := os.ReadFile(filepath.Join(local.Home(), quarantined[0]))
	if err != nil || !bytes.Equal(again, retained) || registered(t, local) != 1 {
		t.Fatalf("owed authority changed %v", err)
	}
}

// A fresh legitimate registration root has no corrupt/orphan selecting proof.
// Complete remote authority is independently verified, restored, then expired
// through the actual retention deletion path once its original age is due.
func TestCleanAbsentPublishedAuthorityIsRestoredThenExpired(t *testing.T) {
	t.Parallel()
	producer := newTestStore(t)
	store := storagetest.NewMemoryStore()
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, producer, store, "s1", dir, t0)
	reg, found, err := producer.LoadRegistration("s1")
	if err != nil || !found {
		t.Fatal(found, err)
	}
	cfg, _, err := config.Load(producer.Home())
	if err != nil {
		t.Fatal(err)
	}
	local := newTestStore(t)
	if err := config.Save(local.Home(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "s1.jsonl")); err != nil {
		t.Fatal(err)
	}
	restored := collect(t, local, store, t0.Add(2*time.Hour))
	if len(restored.Errors) != 0 {
		t.Fatalf("clean restore: %+v", restored)
	}
	if summary, found, err := local.LoadPublishedSummary("s1"); err != nil || !found || !summary.Published || !summary.SourceSetComplete {
		t.Fatalf("verified complete baseline not restored: %+v %v %v", summary, found, err)
	}
	result := sweep(t, local, store, t0.Add(retentionWindow+time.Hour), Options{})
	if len(result.Errors) != 0 || len(result.DeletedSessions) != 1 {
		t.Fatalf("%+v", result)
	}
	if n := sessionObjects(t, store, "s1"); n != 0 {
		t.Fatalf("%d remote objects outlived retention", n)
	}
}

// A registration moved aside drops its session out of the registered set, so
// retention never swept it and its objects outlived retention in the bucket
// and on this machine. The session's remaining state marks it as an orphan,
// which is aged from its capture and deleted the same way.
func TestSessionWhoseRegistrationWasLostIsStillExpired(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", dir, t0)
	publishTwice(t, local, store, "s2", dir, t0)
	corruptFile(t, local, "registrations/s1.json")
	if r := collect(t, local, store, t0.Add(time.Hour)); !errors.Is(r.Errors["s1"], state.ErrQuarantined) {
		t.Fatalf("errors = %v", r.Errors)
	}
	// Within the window: nothing happens to either.
	if result := sweep(t, local, store, t0.Add(retentionWindow-time.Hour), Options{}); len(result.DeletedSessions) != 0 || len(result.Errors) != 0 {
		t.Fatalf("%#v", result)
	}
	if sessionObjects(t, store, "s1") == 0 {
		t.Fatal("an orphan was deleted before its window")
	}
	result := sweep(t, local, store, t0.Add(retentionWindow+time.Hour), Options{})
	if len(result.Errors) != 0 || len(result.DeletedSessions) != 2 {
		t.Fatalf("%#v", result)
	}
	if n := sessionObjects(t, store, "s1"); n != 0 {
		t.Fatalf("%d object(s) of the orphan outlived retention", n)
	}
	for _, dir := range []string{"published", "superseded"} {
		if _, err := os.Stat(filepath.Join(local.Home(), dir, "s1.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the orphan's %s state outlived retention: %v", dir, err)
		}
	}
}

// An orphan is expired by age like any other session, so a clock ahead of
// the storage service's deletes nothing of it either.
func TestOrphanIsKeptWhileTheClockIsAhead(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "orphan", t.TempDir(), t0)
	corruptFile(t, local, "registrations/orphan.json")
	if r := collect(t, local, store, t0.Add(time.Hour)); !errors.Is(r.Errors["orphan"], state.ErrQuarantined) {
		t.Fatalf("errors = %v", r.Errors)
	}
	at := t0.Add(retentionWindow + time.Hour)
	ahead := func(context.Context) (time.Time, error) { return at.Add(-2 * MaxClockSkew), nil }
	result, err := Sweep(context.Background(), local, store, Options{Now: func() time.Time { return at }, ServerClock: ahead, SessionMaxAge: retentionWindow})
	if err != nil || len(result.DeletedSessions) != 0 || !errors.Is(result.Held, ErrClockAhead) {
		t.Fatalf("%#v %v", result, err)
	}
	if sessionObjects(t, store, "orphan") == 0 {
		t.Fatal("a clock ahead deleted an orphan's objects")
	}
	if orphans, err := local.OrphanedSessions(nil); err != nil || len(orphans) != 1 {
		t.Fatalf("a clock ahead forgot an orphan: %v %v", orphans, err)
	}
}

// A registration that exists but cannot be read this time (a permission
// problem, not corruption) still owns its session: the session is not an
// orphan, and nothing of it is deleted however old it is.
func TestUnreadableRegistrationIsNotAnOrphan(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", t.TempDir(), t0)
	path := filepath.Join(local.Home(), "registrations", "s1.json")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	result := sweep(t, local, store, t0.Add(retentionWindow+time.Hour), Options{})
	if len(result.DeletedSessions) != 0 || result.Errors["s1"] == nil {
		t.Fatalf("%#v", result)
	}
	if sessionObjects(t, store, "s1") == 0 {
		t.Fatal("a session whose registration was only unreadable was deleted")
	}
	if _, err := os.Stat(filepath.Join(local.Home(), "published", "s1.json")); err != nil {
		t.Fatalf("its published state was forgotten: %v", err)
	}
}

// An orphan is not forgotten if its registration reappears before the sweep
// gets to it: a hook registering the native session again reuses its ID.
func TestOrphanRegisteredAgainIsKept(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", t.TempDir(), t0)
	if forgotten, err := local.ForgetOrphan("s1"); err != nil || forgotten {
		t.Fatalf("forgot a registered session: %v %v", forgotten, err)
	}
	if orphans, err := local.OrphanedSessions(nil); err != nil || len(orphans) != 0 {
		t.Fatalf("orphans = %v %v", orphans, err)
	}
}

// A sweep with nothing to delete reads each session's summary, not its
// source bundles: it runs after every collector pass, over every session.
// Not parallel: it reads a process-wide counter.
func TestSweepWithNothingToDeleteDecodesNoPublishedState(t *testing.T) {
	local := newTestStore(t)
	store := storagetest.NewMemoryStore()
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	for _, id := range []string{"s1", "s2", "s3"} {
		publishTwice(t, local, store, id, dir, t0)
	}
	loads := state.PublishedStateLoads()
	result := sweep(t, local, store, t0.Add(time.Hour), Options{})
	if len(result.Errors) != 0 || result.DeletedSnapshots != 0 {
		t.Fatalf("%#v", result)
	}
	if n := state.PublishedStateLoads() - loads; n != 0 {
		t.Fatalf("a sweep with nothing to delete decoded %d published states", n)
	}
}
