package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/local"
)

// holdCollectorAtImport makes an import find a collector pass running: when
// the import asks how long to wait for collector.lock, the lock is taken, the
// asked-for wait is recorded, and a short wait is returned instead.
func holdCollectorAtImport(t *testing.T, f *screenFixture) *[]time.Duration {
	t.Helper()
	var asked []time.Duration
	f.env.importCollectorWait = func(wait time.Duration) time.Duration {
		asked = append(asked, wait)
		release, err := local.NamedLock(f.home, "collector.lock")
		must(t, err)
		t.Cleanup(release)
		return 50 * time.Millisecond
	}
	return &asked
}

// Setup waits only briefly for a running collector pass: its import gives
// up, says once how to retry, and setup still succeeds.
func TestSetupImportGivesUpOnABusyCollector(t *testing.T) {
	t.Parallel()
	f := newImportOfferFixture(t)
	asked := holdCollectorAtImport(t, f)
	out := f.runSetup(t, "", setupYesArgs...)
	// Long enough to outlast a short pass the reloaded job starts, short
	// enough not to hold setup for minutes.
	if len(*asked) != 1 || (*asked)[0] != setupImportCollectorWait || setupImportCollectorWait < 10*time.Second || setupImportCollectorWait > 20*time.Second {
		t.Fatalf("setup waited %v for the collector, want %v (10s to 20s)", *asked, setupImportCollectorWait)
	}
	want := "Recent sessions were not imported: a collector pass is still running. Run agent-archive backfill --since 7d to retry.\n"
	if !strings.Contains(out, want) || strings.Count(out, "retry") != 1 || strings.Contains(out, "run backfill again") || strings.Contains(out, "Imported") {
		t.Fatalf("output:\n%s", out)
	}
	assertSetupKept(t, f)
	// Nothing was started: no import record, and setup.lock is free.
	batches, err := backfill.LoadBatches(f.home)
	must(t, err)
	if len(batches) != 0 {
		t.Fatalf("import records left: %+v", batches)
	}
	release, err := local.NamedLock(f.home, "setup.lock")
	if err != nil {
		t.Fatalf("setup.lock still held: %v", err)
	}
	release()
}

// backfill keeps its long wait for a running collector pass.
func TestBackfillWaitsLongForABusyCollector(t *testing.T) {
	t.Parallel()
	f := newImportOfferFixture(t)
	f.runSetup(t, "", setupYesArgs...)
	f.pastSession(t, "later", "src/web-app", screenNow.Add(-time.Hour))
	asked := holdCollectorAtImport(t, f)
	var out bytes.Buffer
	if code := Run([]string{"backfill", "--yes", "--background", "--since", "7d"}, strings.NewReader(""), &out, &out, f.env); code != 1 {
		t.Fatalf("backfill exit %d\n%s", code, &out)
	}
	if len(*asked) != 1 || (*asked)[0] != backfillCollectorWait {
		t.Fatalf("backfill waited %v for the collector, want %v", *asked, backfillCollectorWait)
	}
	if !strings.Contains(out.String(), "agent-archive: backfill: a collector pass is still running; run backfill again. Nothing was changed\n") {
		t.Fatalf("output:\n%s", &out)
	}
}
