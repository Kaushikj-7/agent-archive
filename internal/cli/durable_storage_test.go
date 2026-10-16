package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/state/statetest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFuturePublishedStatusGrantsNoPublicationAuthority(t *testing.T) {
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "published", "foreign.json"), []byte(`{"commit":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	skips := 0
	sessions := statusSessions{store: store, skip: func(id string, err error) {
		if id != "foreign" || !errors.Is(err, state.ErrDurableStorageRecovery) {
			t.Fatalf("skip: %s %v", id, err)
		}
		skips++
	}}
	reg := archive.SessionRegistration{ArchiveSessionID: "foreign", Harness: archive.Harness{Name: "claude"}}
	var app appStatus
	var pair projectCaptureStatus
	var readback verificationOutcome
	sessions.addSession(&app, &pair, reg, config.Config{}, home, nil, &readback)
	sessions.addPublication(&app, &pair, reg, config.Config{}, home, &readback)
	if skips != 2 || app.CapturedLocally || app.Published || app.PublishedSessions != 0 || pair.CapturedLocally || pair.Published {
		t.Fatalf("refusal gained authority: skips=%d app=%+v pair=%+v", skips, app, pair)
	}
}

func TestStatusSourceOnlyRecoveryWithoutRegistration(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err = config.Save(home, config.Config{MachineID: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	data := []byte("original")
	sum := sha256.Sum256(data)
	if _, err = store.StagePendingSource("orphan", archive.SourceReference{SHA256: hex.EncodeToString(sum[:]), CompressedBytes: len(data), Key: "synthetic"}, data); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var view statusView
	sessions := readSessionStatus(&view, config.Config{}, home, store)
	if len(sessions.regs) != 0 || view.Collector.PendingCount != 1 || len(view.Warnings) == 0 {
		t.Fatalf("source-only status: %+v regs=%v", view.Collector, sessions.regs)
	}
	after, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("status changed config", err)
	}
}

func TestStatusAnonymousGenerationAndPendingTempsAreOwed(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err = config.Save(home, config.Config{MachineID: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if err = config.WithDurableStorage(home, func(g config.DurableStorageGuard) error { return g.CheckHome(home) }); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"pending", "generation-recovery"} {
		if err = os.MkdirAll(filepath.Join(home, dir), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(home, dir, ".pending-123"), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var view statusView
	sessions := readSessionStatus(&view, config.Config{}, home, store)
	if len(sessions.regs) != 0 || view.Collector.PendingCount != 2 || len(view.Warnings) < 2 {
		t.Fatalf("anonymous work omitted: %+v %v", view.Collector, view.Warnings)
	}
}

func TestStatusRegisteredGenerationReceiptClassification(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Save(home, config.Config{MachineID: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	reg := saveImportedSession(t, store, time.Now(), "previous", t.TempDir())
	if err := statetest.SavePublished(store, reg.ArchiveSessionID, archive.SourceBundle{ArchiveSessionID: reg.ArchiveSessionID, Capture: archive.SourceCapture{Harness: reg.Harness, CapturedAt: time.Now()}}, time.Now(), state.CacheStatusPublished); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "generation-recovery", "previous.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	complete := `{"version":1,"key":{"Agent":"codex","NativeID":"native"},"previous":"previous","next":"next","complete":true}`
	for _, tc := range []struct {
		raw  string
		want int
	}{{complete, 0}, {strings.Replace(complete, `"complete":true`, `"complete":false`, 1), 1}, {"{", 1}, {complete, 0}} {
		if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
			t.Fatal(err)
		}
		var view statusView
		readSessionStatus(&view, config.Config{}, home, store)
		if view.Collector.PendingCount != tc.want {
			t.Fatalf("registered generation census: want%d got%+v warnings%v", tc.want, view.Collector, view.Warnings)
		}
		if tc.want > 0 && len(view.Warnings) == 0 {
			t.Fatal("owed generation omitted warning")
		}
	}
	// A session already pending for ordinary publication still needs the warning,
	// but its additional recovery namespace must not count it twice.
	if err := os.Remove(filepath.Join(home, "published", "previous.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	var view statusView
	readSessionStatus(&view, config.Config{}, home, store)
	if view.Collector.PendingCount != 1 || !strings.Contains(strings.Join(view.Warnings, "\n"), "generation recovery obligation") {
		t.Fatalf("already-pending generation lost warning or double counted: %+v %v", view.Collector, view.Warnings)
	}

}

func TestSetupWriterPreservesStorageOnlyFence(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	current := config.Config{RetentionDays: 90, MachineID: "synthetic"}
	if err := config.Save(home, current); err != nil {
		t.Fatal(err)
	}
	proposed := current
	proposed.DurableStorageProtection = true
	unlock, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := protectSetupWriter(home, current, proposed); err != nil {
		t.Fatal(err)
	}
	actual, found, err := config.Load(home)
	if err != nil || !found || !actual.DurableStorageProtection || actual.SchemaVersion != 7 || actual.GenerationProtection || actual.CodexHistoryProtection {
		t.Fatalf("storage-only rollback fence omitted: %+v %v %v", actual, found, err)
	}
}
