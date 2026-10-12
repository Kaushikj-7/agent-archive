package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/backfill"
)

// Regression: 2026-09 review B-25. A relative --since (30d) was turned into
// a calendar day before batches were compared, so an import interrupted
// today could not be continued "with the same options" tomorrow: the rerun
// started a new import and left the first one partial. The batch now
// records the value as typed, and the same value continues it on any day.
func TestBackfillRelativeSinceContinuesNextDay(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	f.env.backfillHoldSteps = 3
	stopped := false
	f.env.backfillCheckpoint = func(step string) error {
		if step == "registered" && !stopped {
			stopped = true
			return errors.New("simulated crash")
		}
		return nil
	}
	if _, _, code := f.importRun(t, nil, false, "--yes", "--background", "--since", "30d"); code != 1 {
		t.Fatalf("first run: code %d", code)
	}
	b, _ := loadBatch(t, f.data)
	if b.CompletedAt != nil || b.Filters.SinceArg != "30d" || b.Filters.Since != "2026-08-24" {
		t.Fatalf("batch %+v", b)
	}

	f.env.backfillCheckpoint = nil
	f.env.Now = func() time.Time { return backfillNow.Add(24 * time.Hour) }
	out, errOut, code := f.importRun(t, nil, false, "--yes", "--background", "--since", "30d")
	if code != 0 {
		t.Fatalf("rerun: %d %s", code, errOut)
	}
	if !strings.Contains(out, "as import "+firstImport+".") || strings.Contains(out, "was interrupted") {
		t.Fatalf("the rerun did not continue %s:\n%s", firstImport, out)
	}
	b, _ = loadBatch(t, f.data)
	if b.CompletedAt == nil {
		t.Fatalf("batch %+v", b)
	}
}

// B-25: when the latest import was interrupted and this run's options
// would start another, the plan says so and gives the command that
// finishes the interrupted one.
func TestBackfillPointsAtAnInterruptedImport(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	stopped := false
	f.env.backfillCheckpoint = func(step string) error {
		if step == "registered" && !stopped {
			stopped = true
			return errors.New("simulated crash")
		}
		return nil
	}
	f.env.backfillHoldSteps = 3
	if _, _, code := f.importRun(t, nil, false, "--yes", "--background", "--since", "30d", "--harness", "claude", "--project", f.userHome+"/levenshtein"); code != 1 {
		t.Fatalf("first run: code %d", code)
	}
	f.env.backfillCheckpoint = nil
	out, errOut, code := f.command(t, "backfill", "--dry-run")
	if code != 0 {
		t.Fatalf("dry run: %d %s", code, errOut)
	}
	want := "Import " + firstImport + " was interrupted; this run, with other options, starts a new\nimport. To finish " + firstImport + " instead, run:\n  agent-archive backfill --harness claude --project ~/levenshtein --since 30d\n"
	if !strings.Contains(out, want) {
		t.Fatalf("plan lacks\n%s\n---\n%s", want, out)
	}
}

// A registration error after an earlier run crashed inside a hold still
// leaves the batch with every session registered under it: the error path
// rebuilds the batch from the registrations.
//
// Regression: backfill B3 review, 2026-09 (e589f65).
func TestBackfillErrorPathReconciles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Parallel()
	f, _ := newImportFixture(t)
	imports := filepath.Join(f.data, "imports")
	f.env.backfillHoldSteps = 3
	f.env.backfillCheckpoint = func(step string) error {
		if step == "staged" {
			return os.Chmod(imports, 0o500)
		}
		return nil
	}
	t.Cleanup(func() {
		_ = os.Chmod(imports, 0o700)
	})
	if _, _, code := f.importRun(t, nil, false, "--yes", "--background"); code != 1 {
		t.Fatal("the first run did not fail")
	}
	if err := os.Chmod(imports, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := loadBatch(t, f.data)
	crashed, _ := importRegistrations(t, f.data, firstImport)
	if len(b.Sessions) >= len(crashed) {
		t.Fatalf("no gap to repair: %d recorded, %d registered", len(b.Sessions), len(crashed))
	}

	f.env.backfillCheckpoint = func(step string) error {
		if step == "registered" {
			return errors.New("simulated registration error")
		}
		return nil
	}
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 1 || !strings.Contains(errOut, "simulated registration error") {
		t.Fatalf("second run: code %d, %s", code, errOut)
	}
	registered, _ := importRegistrations(t, f.data, firstImport)
	b, _ = loadBatch(t, f.data)
	if len(registered) <= len(crashed) || len(b.Sessions) != len(registered) || b.CompletedAt != nil {
		t.Fatalf("%d registered, batch records %d", len(registered), len(b.Sessions))
	}
}

// A crash inside a registration hold, here the batch file becoming
// unwritable, leaves registrations the batch file never recorded. The rerun
// rebuilds the batch from the registrations.
//
// Regression: backfill B3 review, 2026-09 (e589f65).
func TestBackfillBatchRebuiltFromRegistrations(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Parallel()
	f, _ := newImportFixture(t)
	imports := filepath.Join(f.data, "imports")
	f.env.backfillHoldSteps = 3
	f.env.backfillCheckpoint = func(step string) error {
		if step == "staged" {
			return os.Chmod(imports, 0o500)
		}
		return nil
	}
	t.Cleanup(func() {
		_ = os.Chmod(imports, 0o700)
	})
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 1 {
		t.Fatalf("the unwritable batch file did not stop the import: %s", errOut)
	}
	registered, _ := importRegistrations(t, f.data, firstImport)
	b, _ := loadBatch(t, f.data)
	if len(registered) == 0 || len(b.Sessions) >= len(registered) {
		t.Fatalf("setup failed to reproduce the gap: %d registered, %d recorded", len(registered), len(b.Sessions))
	}

	f.env.backfillCheckpoint = nil
	if err := os.Chmod(imports, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 0 {
		t.Fatalf("rerun: %s\n%s", errOut, out)
	}
	parents, _ := importRegistrations(t, f.data, firstImport)
	b, _ = loadBatch(t, f.data)
	if len(parents) != 12 || len(b.Sessions) != 12 || len(b.Subagents) != 2 || b.CompletedAt == nil {
		t.Fatalf("%d registered, batch %+v", len(parents), b)
	}
}

// A run that finds nothing left to import completes the interrupted import
// it would have continued, from the registrations.
//
// Regression: backfill B3 review, 2026-09 (e589f65).
func TestBackfillCompletesInterruptedImportWithNothingLeft(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 0 {
		t.Fatalf("import: %s", errOut)
	}
	b, _ := loadBatch(t, f.data)
	b.CompletedAt, b.Sessions = nil, b.Sessions[:2]
	if err := backfill.SaveBatch(f.data, b); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := f.importRun(t, nil, false, "--yes")
	if code != 0 || !strings.Contains(out, "Nothing to import.") || !strings.Contains(out, "Import "+firstImport+", which was interrupted, is complete: 12 sessions registered.") {
		t.Fatalf("code %d, %s\n%s", code, errOut, out)
	}
	b, _ = loadBatch(t, f.data)
	if b.CompletedAt == nil || len(b.Sessions) != 12 {
		t.Fatalf("batch %+v", b)
	}
	// Other filters would not have continued it, so they leave it alone.
	b.CompletedAt = nil
	if err := backfill.SaveBatch(f.data, b); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := f.importRun(t, nil, false, "--yes", "--harness", "claude"); strings.Contains(out, "is complete") {
		t.Fatalf("a run with other filters completed the import:\n%s", out)
	}
}

// Ctrl-C during registration stops between holds and leaves an interrupted
// import that a rerun finishes.
//
// Regression: backfill B3 review, 2026-09 (e589f65).
func TestBackfillInterruptedRegistration(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	signals := make(chan os.Signal, 1)
	f.env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() {} }
	f.env.backfillHoldSteps = 3
	f.env.backfillCheckpoint = func(step string) error {
		if step == "registered" && len(signals) == 0 {
			signals <- os.Interrupt
		}
		return nil
	}
	out, _, code := f.importRun(t, nil, false, "--yes")
	if code != 1 || !strings.Contains(out, "Stopped.") || !strings.Contains(out, "run agent-archive backfill again with the same options to finish it") {
		t.Fatalf("code %d:\n%s", code, out)
	}
	partial, _ := importRegistrations(t, f.data, firstImport)
	b, _ := loadBatch(t, f.data)
	if b.CompletedAt != nil || len(partial) == 0 || len(partial) == 12 || len(b.Sessions) != len(partial) {
		t.Fatalf("%d registered, batch %+v", len(partial), b)
	}

	f.env.backfillCheckpoint = nil
	f.env.Interrupts = nil
	if _, errOut, code := f.importRun(t, nil, false, "--yes", "--background"); code != 0 {
		t.Fatalf("rerun: %s", errOut)
	}
	b, _ = loadBatch(t, f.data)
	if b.CompletedAt == nil || len(b.Sessions) != 12 {
		t.Fatalf("batch %+v", b)
	}
}
