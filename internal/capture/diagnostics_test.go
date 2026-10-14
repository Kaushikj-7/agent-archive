package capture

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// twoProjectConfig saves a configuration with both roots, including only
// those listed in included, and returns its projects.
func twoProjectConfig(t *testing.T, home string, included ...string) []archive.ProjectActivation {
	t.Helper()
	activated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var projects []archive.ProjectActivation
	for _, root := range []string{"/work/kept", "/work/excluded"} {
		in := false
		for _, r := range included {
			in = in || r == root
		}
		projects = append(projects, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: in, ActivatedAt: activated})
	}
	cfg := config.Config{
		MachineID: "machine-1", Storage: credentialsTestConfig(),
		Archive: archive.Config{SchemaVersion: 1, MachineID: "machine-1", Enabled: true, Projects: projects},
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	return projects
}

func diagnosticFor(root string, at time.Time) Diagnostic {
	return Diagnostic{Code: DiagnosticSetupInProgress, Harness: "claude", ProjectRoot: root, ObservedAt: at}
}

func storedRoots(t *testing.T, home string) map[string]bool {
	t.Helper()
	diagnostics, err := ReadDiagnostics(home)
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]bool{}
	for _, d := range diagnostics {
		roots[d.ProjectRoot] = true
	}
	return roots
}

// The resurrection case, made deterministic: a hook decided the project was
// included from the configuration it read during setup's transaction; setup
// then committed the exclusion and pruned; only then does the hook write.
func TestPrunedProjectDiagnosticIsNotResurrectedByAStaleHook(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	twoProjectConfig(t, home, "/work/kept", "/work/excluded")
	if err := RecordDiagnostic(home, diagnosticFor("/work/excluded", at)); err != nil {
		t.Fatal(err)
	}

	projects := twoProjectConfig(t, home, "/work/kept") // setup commits the exclusion
	if err := PruneDiagnostics(home, projects); err != nil {
		t.Fatal(err)
	}
	if err := RecordDiagnostic(home, diagnosticFor("/work/excluded", at.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := RecordDiagnostic(home, diagnosticFor("/work/kept", at.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	roots := storedRoots(t, home)
	if roots["/work/excluded"] || !roots["/work/kept"] {
		t.Fatalf("stored roots after prune = %v", roots)
	}
}

// A hook never waits more than hookDiagnosticsWait for the lock; on timeout it
// drops the diagnostic and returns without error.
func TestHookDiagnosticWaitIsBoundedAndDropsOnTimeout(t *testing.T) {
	home := t.TempDir()
	twoProjectConfig(t, home, "/work/kept")
	release, err := local.NamedLock(home, DiagnosticsLockName)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = RecordDiagnostic(home, diagnosticFor("/work/kept", start))
	elapsed := time.Since(start)
	release()
	if err != nil {
		t.Fatalf("a busy lock must drop the diagnostic, not fail the hook: %v", err)
	}
	if elapsed < hookDiagnosticsWait || elapsed > hookDiagnosticsWait+200*time.Millisecond {
		t.Fatalf("hook waited %s, want about %s", elapsed, hookDiagnosticsWait)
	}
	if len(storedRoots(t, home)) != 0 {
		t.Fatal("a diagnostic was written without the lock")
	}
	// Uncontended, it records.
	if err := RecordDiagnostic(home, diagnosticFor("/work/kept", start)); err != nil || !storedRoots(t, home)["/work/kept"] {
		t.Fatalf("uncontended record failed: %v", err)
	}
}

// Setup's prune waits for a hook that holds the lock instead of racing it.
func TestPruneWaitsForAHookHoldingTheLock(t *testing.T) {
	home := t.TempDir()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	twoProjectConfig(t, home, "/work/kept", "/work/excluded")
	if err := RecordDiagnostic(home, diagnosticFor("/work/excluded", at)); err != nil {
		t.Fatal(err)
	}
	projects := twoProjectConfig(t, home, "/work/kept")
	release, err := local.NamedLock(home, DiagnosticsLockName)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		release()
	}()
	if err := PruneDiagnostics(home, projects); err != nil {
		t.Fatalf("prune gave up instead of waiting: %v", err)
	}
	if storedRoots(t, home)["/work/excluded"] {
		t.Fatal("prune did not remove the excluded project's diagnostic")
	}
}

// On a busy machine a hook can hold the lock for seconds: before #228 through
// its disk syncs, and still through a stalled rename. Setup's prune outwaits
// such a hook rather than failing, as it did with a 2s wait.
//
// Regression: 2026-09-30, TestSetupPruneAndHookDiagnosticRaceNeverResurrects
// failed 3 of 4 runs under load with prune=ErrBusy.
func TestPruneOutwaitsAHookHoldingTheLockForSeconds(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	twoProjectConfig(t, home, "/work/kept", "/work/excluded")
	if err := RecordDiagnostic(home, diagnosticFor("/work/excluded", at)); err != nil {
		t.Fatal(err)
	}
	projects := twoProjectConfig(t, home, "/work/kept")
	release, err := local.NamedLock(home, DiagnosticsLockName)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(3 * time.Second)
		release()
	}()
	if err := PruneDiagnostics(home, projects); err != nil {
		t.Fatalf("prune gave up on a hook holding the lock for 3s: %v", err)
	}
	if storedRoots(t, home)["/work/excluded"] {
		t.Fatal("prune did not remove the excluded project's diagnostic")
	}
}

// The race itself: setup commits an exclusion and prunes while a hook, which
// read the old configuration, records a diagnostic for the excluded project.
// Whatever the interleaving, the excluded project's diagnostic is gone at
// the end, and an included project's diagnostic is never lost to the prune.
//
// The hook's wait is raised for this test only, to setup's. At 50 ms a busy
// machine can make a hook drop its diagnostic by design, which would be
// indistinguishable here from a lost update; the bounded wait has its own
// test. A hook here can queue behind both the prune and the other hook, so it
// gets the same wait setup does.
func TestSetupPruneAndHookDiagnosticRaceNeverResurrects(t *testing.T) {
	const rounds = 40
	saved := hookDiagnosticsWait
	hookDiagnosticsWait = pruneDiagnosticsWait
	t.Cleanup(func() { hookDiagnosticsWait = saved })
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for round := range rounds {
		home := t.TempDir()
		twoProjectConfig(t, home, "/work/kept", "/work/excluded")
		if err := RecordDiagnostic(home, diagnosticFor("/work/excluded", at)); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var pruneErr, excludedErr, keptErr error
		wg.Add(3)
		go func() { // setup: commit, then prune
			defer wg.Done()
			pruneErr = PruneDiagnostics(home, twoProjectConfig(t, home, "/work/kept"))
		}()
		go func() { // a hook holding the pre-commit view of the excluded project
			defer wg.Done()
			time.Sleep(time.Duration(round) * 25 * time.Microsecond)
			excludedErr = RecordDiagnostic(home, diagnosticFor("/work/excluded", at.Add(time.Minute)))
		}()
		go func() { // a hook for a project that stays included
			defer wg.Done()
			time.Sleep(time.Duration(rounds-round) * 25 * time.Microsecond)
			keptErr = RecordDiagnostic(home, diagnosticFor("/work/kept", at.Add(time.Minute)))
		}()
		wg.Wait()
		if pruneErr != nil || excludedErr != nil || keptErr != nil {
			t.Fatalf("round %d: prune=%v excluded=%v kept=%v", round, pruneErr, excludedErr, keptErr)
		}
		roots := storedRoots(t, home)
		if roots["/work/excluded"] {
			t.Fatalf("round %d: an excluded project's diagnostic was resurrected", round)
		}
		if !roots["/work/kept"] {
			t.Fatalf("round %d: an included project's diagnostic was lost", round)
		}
	}
}

// A hook's diagnostic is synced to disk before diagnostics.lock is taken,
// because a sync can take seconds on a busy Mac and setup's prune waits only
// pruneDiagnosticsWait. Setup commits an exclusion and prunes while the hook
// syncs; the prune finds the lock free, and the hook, finding the file
// changed, builds its update again from the pruned file.
//
// Regression: the hook synced under the lock, and on a loaded machine the
// prune failed with "another collector or setup is running".
func TestPruneDoesNotWaitForAHookSyncingItsDiagnostic(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	twoProjectConfig(t, home, "/work/kept", "/work/excluded")
	if err := RecordDiagnostic(home, diagnosticFor("/work/excluded", at)); err != nil {
		t.Fatal(err)
	}
	update := recordUpdate(home, diagnosticFor("/work/kept", at.Add(time.Minute)))
	syncs := 0
	var lockErr, pruneErr error
	update.afterStage = func() {
		syncs++
		if syncs > 1 {
			return
		}
		release, err := local.NamedLock(home, DiagnosticsLockName)
		if err != nil {
			lockErr = err
			return
		}
		release()
		pruneErr = PruneDiagnostics(home, twoProjectConfig(t, home, "/work/kept"))
	}
	if err := update.run(home); err != nil {
		t.Fatal(err)
	}
	if lockErr != nil || pruneErr != nil {
		t.Fatalf("while the hook synced: lock=%v prune=%v", lockErr, pruneErr)
	}
	if syncs != 2 {
		t.Fatalf("the hook staged %d times, want its update built again once", syncs)
	}
	roots := storedRoots(t, home)
	if roots["/work/excluded"] || !roots["/work/kept"] {
		t.Fatalf("stored roots = %v", roots)
	}
}

// A hook that read the configuration before setup excluded its project, and
// syncs its diagnostic while setup commits and prunes, rechecks the
// configuration under the lock and writes nothing, whether or not the prune
// had anything to drop.
func TestAHookSyncingDuringThePruneDoesNotResurrectAnExcludedProject(t *testing.T) {
	t.Parallel()
	for _, earlier := range []bool{true, false} {
		t.Run(fmt.Sprintf("earlier diagnostic=%t", earlier), func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			twoProjectConfig(t, home, "/work/kept", "/work/excluded")
			if err := RecordDiagnostic(home, diagnosticFor("/work/kept", at)); err != nil {
				t.Fatal(err)
			}
			if earlier {
				if err := RecordDiagnostic(home, diagnosticFor("/work/excluded", at)); err != nil {
					t.Fatal(err)
				}
			}
			update := recordUpdate(home, diagnosticFor("/work/excluded", at.Add(time.Minute)))
			var pruneErr error
			pruned := false
			update.afterStage = func() {
				if !pruned {
					pruned = true
					pruneErr = PruneDiagnostics(home, twoProjectConfig(t, home, "/work/kept"))
				}
			}
			if err := update.run(home); err != nil {
				t.Fatal(err)
			}
			if pruneErr != nil {
				t.Fatalf("prune: %v", pruneErr)
			}
			roots := storedRoots(t, home)
			if roots["/work/excluded"] || !roots["/work/kept"] {
				t.Fatalf("stored roots = %v", roots)
			}
		})
	}
}

// A hook that records while setup's prune syncs is not lost: the prune finds
// the file changed under the lock and prunes what the hook left.
func TestAHookRecordingWhileThePruneSyncsIsNotLost(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	twoProjectConfig(t, home, "/work/kept", "/work/excluded")
	if err := RecordDiagnostic(home, diagnosticFor("/work/excluded", at)); err != nil {
		t.Fatal(err)
	}
	projects := twoProjectConfig(t, home, "/work/kept")
	update := pruneUpdate(projects)
	syncs := 0
	var hookErr error
	update.afterStage = func() {
		syncs++
		if syncs == 1 {
			hookErr = recordDiagnostic(home, diagnosticFor("/work/kept", at.Add(time.Minute)), true)
		}
	}
	if err := update.run(home); err != nil {
		t.Fatal(err)
	}
	if hookErr != nil {
		t.Fatalf("the hook could not record while the prune synced: %v", hookErr)
	}
	if syncs != 2 {
		t.Fatalf("the prune staged %d times, want it built again once", syncs)
	}
	roots := storedRoots(t, home)
	if roots["/work/excluded"] || !roots["/work/kept"] {
		t.Fatalf("stored roots = %v", roots)
	}
}

// An update that other writers overtake every time gives up after
// diagnosticsAttempts, as busy: a hook drops its diagnostic, and the prune
// reports it.
func TestAnUpdateOvertakenEveryTimeGivesUpAsBusy(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	twoProjectConfig(t, home, "/work/kept", "/work/excluded")
	if err := RecordDiagnostic(home, diagnosticFor("/work/excluded", at)); err != nil {
		t.Fatal(err)
	}
	update := pruneUpdate(twoProjectConfig(t, home, "/work/kept"))
	syncs := 0
	update.afterStage = func() {
		syncs++
		if err := RecordDiagnostic(home, diagnosticFor("/work/kept", at.Add(time.Duration(syncs)*time.Minute))); err != nil {
			t.Error(err)
		}
	}
	err := update.run(home)
	if !errors.Is(err, local.ErrBusy) || syncs != diagnosticsAttempts {
		t.Fatalf("err=%v after %d attempts, want ErrBusy after %d", err, syncs, diagnosticsAttempts)
	}
	entries, _ := os.ReadDir(home)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pending-") {
			t.Fatalf("a staged file was left behind: %s", entry.Name())
		}
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestExcludedProjectLeavesNoDiagnostic(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Now().UTC()
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	if err := HandleEvent(home, "cursor", map[string]any{"hook_event_name": "sessionStart", "conversation_id": "old", "workspace_roots": []any{"/private/excluded"}}, at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	ds, _ := ReadDiagnostics(home)
	if len(ds) != 0 {
		t.Fatalf("excluded paths recorded %+v", ds)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestResumeBeforeActivationRecordsActivationDiagnostic(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	at := time.Now().UTC()
	setUpTestConfig(t, home, "/work/widget", at.Add(time.Hour))
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "resume", "session_id": "old", "cwd": "/work/widget"}
	if err := HandleEvent(home, "claude", payload, at, WithDecoders(testDecoders)); err != nil {
		t.Fatal(err)
	}
	ds, _ := ReadDiagnostics(home)
	if len(ds) != 1 || ds[0].Code != DiagnosticPreActivationStart {
		t.Fatalf("diagnostics %+v", ds)
	}
}

// A diagnostic the collector records for a session names the configured
// project that includes the session's root, never the root itself, and
// nothing for a root no included project owns.
func TestSessionDiagnosticNamesOnlyTheIncludedProject(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		root, want string
	}{
		{"/work/kept", "/work/kept"},
		{"/work/kept/nested/dir", "/work/kept"},
		{"/work/excluded", ""},
		{"/elsewhere", ""},
	} {
		home := t.TempDir()
		twoProjectConfig(t, home, "/work/kept")
		if err := RecordSessionDiagnostic(home, DiagnosticRevisionLeftOut, "claude", c.root, at); err != nil {
			t.Fatal(err)
		}
		diagnostics, err := ReadDiagnostics(home)
		if err != nil {
			t.Fatal(err)
		}
		if c.want == "" {
			if len(diagnostics) != 0 {
				t.Errorf("%s: recorded %+v", c.root, diagnostics)
			}
			continue
		}
		if len(diagnostics) != 1 || diagnostics[0] != (Diagnostic{Code: DiagnosticRevisionLeftOut, Harness: "claude", ProjectRoot: c.want, ObservedAt: at}) {
			t.Errorf("%s: recorded %+v", c.root, diagnostics)
		}
		// The code's spelling is what status --json documents.
		if raw, err := os.ReadFile(DiagnosticsPath(home)); err != nil || !strings.Contains(string(raw), `"revision_left_out"`) {
			t.Errorf("%s: stored %s (%v)", c.root, raw, err)
		}
	}
}
