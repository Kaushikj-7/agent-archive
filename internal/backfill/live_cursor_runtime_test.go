package backfill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/nativefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestLiveCursorPublicImportPublishesStagedBytesAfterNativeDeletion(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only Unix SHM ownership refuses root")
	}
	r, c, cfg, database := settledCursorFixture(t)
	writer := startCursorWriter(t, database, true)
	writer.do("unselected")
	result, err := r.Run([]Candidate{c})
	if err != nil || len(result.Sessions) != 1 {
		t.Fatalf("public live admission %+v %v", result, err)
	}
	id := result.Sessions[0]
	read, closeRead := r.Store.WithReadBudget(t.Context(), agentapi.NewNativeReadBudget(128<<20))
	reg, found, err := read.LoadRegistration(id)
	if err != nil || !found {
		t.Fatal(reg, found, err)
	}
	stage, found, err := read.LoadImportStage(t.Context(), reg)
	if err != nil || !found || stage.Version != 3 || stage.CursorFacts == nil {
		t.Fatal(stage.Version, found, err)
	}
	closeRead()
	writer.kill()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(database + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	guard, err := local.LockCollectorGuard(r.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()
	remote := storagetest.NewMemoryStore()
	reopened := state.OpenReadOnly(r.Home)
	got, err := collector.Run(t.Context(), reopened, remote, collector.Options{CollectorGuard: guard, Sources: testSources, Parsers: testSources, MachineID: "synthetic", Now: func() time.Time { return r.AdmittedAt.Add(time.Minute) }, AcceptSession: cfg.AcceptSession})
	defer got.CloseStagedResults()
	if err != nil || len(got.Errors) != 0 || len(got.Published) != 1 {
		t.Fatalf("live staged restart %+v %v", got, err)
	}
	summary, found, err := reopened.LoadPublishedSummary(id)
	if err != nil || !found || !summary.Published {
		t.Fatal(summary, found, err)
	}
	for _, dir := range []string{"temporary-reservations", "temporary-scratch"} {
		entries, err := os.ReadDir(filepath.Join(r.Home, dir))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatal("runtime left scratch", dir, entries)
		}
	}
}

// The observer mutates only test-owned scratch, while a large synthetic SQLite
// image keeps the actual copy open long enough to exercise a cleanup failure.
func TestLiveCursorPublicFailureRetainsRetryableCleanupCustody(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only Unix SHM ownership refuses root")
	}
	r, c, _, database := settledCursorFixture(t)
	padding := openCursorWriter(t, database, false)
	if _, err := padding.ExecContext(t.Context(), "INSERT INTO ItemTable(key,value) VALUES('padding',zeroblob(16777216))"); err != nil {
		t.Fatal(err)
	}
	if err := padding.Close(); err != nil {
		t.Fatal(err)
	}
	writer := startCursorWriter(t, database, true)
	writer.do("unselected")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	created := make(chan string, 1)
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				entries, _ := os.ReadDir(filepath.Join(r.Home, "temporary-scratch"))
				for _, entry := range entries {
					folder := filepath.Join(r.Home, "temporary-scratch", entry.Name())
					if info, err := os.Stat(filepath.Join(folder, "native.db")); err != nil || info.Size() == 0 {
						continue
					}
					path := filepath.Join(folder, "unexpected")
					if os.WriteFile(path, []byte("synthetic cleanup blocker"), 0600) == nil {
						created <- path
						return
					}
				}
			}
		}
	}()
	result, err := r.Run([]Candidate{c})
	if err == nil || len(result.Sessions) != 0 {
		t.Fatalf("failed cleanup admitted %+v %v", result, err)
	}
	var cleanup *nativefacts.LiveCaptureCleanupError
	if !errors.As(err, &cleanup) {
		t.Fatalf("Run lost retry custody: %v", err)
	}
	var blocker string
	select {
	case blocker = <-created:
	case <-ctx.Done():
		t.Fatal("scratch failure observer did not run")
	}
	if e := cleanup.Close(); e == nil {
		t.Fatal("retry ignored retained unknown child")
	}
	entries, e := os.ReadDir(filepath.Join(r.Home, "temporary-reservations"))
	if e != nil || len(entries) != 1 {
		t.Fatal("failed retry lost receipt", entries, e)
	}
	id, _, e := r.Store.EnsureArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.Cursor, NativeID: c.NativeSessionID})
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{filepath.Join(r.Home, "registrations", id+".json"), filepath.Join(r.Home, "sessions", id, "import-stage", "stage.json")} {
		if _, e := os.Stat(path); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("cleanup failure escaped a stage/registration", path, e)
		}
	}
	if e = os.Remove(blocker); e != nil {
		t.Fatal(e)
	}
	copied := *cleanup
	if e = cleanup.Close(); e != nil {
		t.Fatal(e)
	}
	if e = copied.Close(); e != nil {
		t.Fatal("copied custody double close", e)
	}
	for _, dir := range []string{"temporary-reservations", "temporary-scratch"} {
		entries, e := os.ReadDir(filepath.Join(r.Home, dir))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			t.Fatal(e)
		}
		if len(entries) != 0 {
			t.Fatal("successful retry retained obligation", dir, entries)
		}
	}
}
