package cli

import (
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Regression: 2026-09 review B-21. The import prompt's edit used to accept
// any retention and write it archive-wide: edit, 7, y left the archive at
// 7 days, and the next pass deleted every older session, hook-captured ones
// too. It only raises now; a lower answer keeps the retention and says to
// shorten it in setup.
func TestBackfillEditNeverShortensRetention(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	backdateTranscripts(t, f)
	out, errOut, code := f.importRun(t, strings.NewReader("edit\n7\ny\n"), true, "--background")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errOut)
	}
	if !strings.Contains(out, "Retention stays at 90 days: a shorter period would delete sessions") {
		t.Fatalf("no refusal:\n%s", out)
	}
	cfg, _, err := config.Load(f.data)
	if err != nil || cfg.RetentionDays != 90 {
		t.Fatalf("retention %d, %v", cfg.RetentionDays, err)
	}
	if batch, _ := loadBatch(t, f.data); batch.Retention != nil {
		t.Fatalf("batch records a retention change: %+v", batch.Retention)
	}
}

// B-21: with retention off, edit has nothing to keep longer and turns
// nothing on.
func TestBackfillEditWithRetentionOff(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	cfg, _, err := config.Load(f.data)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RetentionDays = 0
	if err := config.Save(f.data, cfg); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := f.importRun(t, strings.NewReader("edit\ny\n"), true, "--background")
	if code != 0 || !strings.Contains(out, "Keep forever: there is no automatic expiry to extend") {
		t.Fatalf("code %d\n%s\n%s", code, out, errOut)
	}
	if cfg, _, _ = config.Load(f.data); cfg.RetentionDays != 0 {
		t.Fatalf("retention turned on: %d", cfg.RetentionDays)
	}
}

// B-21: the batch records the retention an edit raised from, and undo puts
// it back, saying so first; a later undo never changes it again.
func TestBackfillUndoRestoresRaisedRetention(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	backdateTranscripts(t, f)
	if _, errOut, code := f.importRun(t, strings.NewReader("edit\n365\ny\n"), true, "--background"); code != 0 {
		t.Fatalf("import: %d %s", code, errOut)
	}
	batch, _ := loadBatch(t, f.data)
	if batch.Retention == nil || batch.Retention.From != 90 || batch.Retention.To != 365 {
		t.Fatalf("batch retention %+v", batch.Retention)
	}
	out, errOut, code := f.undoRun(t, nil, false, "--yes", "--restore-retention")
	if code != 0 {
		t.Fatalf("undo: %d %s", code, errOut)
	}
	if !strings.Contains(out, "Retention goes back from 365 to 90 days") || !strings.Contains(out, "restored retention to 90 days") {
		t.Fatalf("undo output:\n%s", out)
	}
	cfg, _, _ := config.Load(f.data)
	if cfg.RetentionDays != 90 {
		t.Fatalf("retention after undo %d", cfg.RetentionDays)
	}
	if batch, _ = loadBatch(t, f.data); batch.Retention == nil || !batch.Retention.Restored {
		t.Fatalf("batch retention %+v", batch.Retention)
	}
}

// B-21: retention changed in setup after the import is the person's; undo
// leaves it and says so.
func TestBackfillUndoKeepsRetentionChangedSince(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	backdateTranscripts(t, f)
	if _, errOut, code := f.importRun(t, strings.NewReader("edit\n365\ny\n"), true, "--background"); code != 0 {
		t.Fatalf("import: %d %s", code, errOut)
	}
	cfg, _, _ := config.Load(f.data)
	cfg.RetentionDays = 200
	if err := config.Save(f.data, cfg); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := f.undoRun(t, nil, false, "--yes", "--restore-retention")
	if code != 0 || !strings.Contains(out, "Retention stays at 200 days") {
		t.Fatalf("undo: %d\n%s\n%s", code, out, errOut)
	}
	if cfg, _, _ = config.Load(f.data); cfg.RetentionDays != 200 {
		t.Fatalf("retention %d", cfg.RetentionDays)
	}
}

// Regression (PR #53 review): an undo that stopped after saving the
// configuration, before recording in the batch what it excluded and
// restored, left no record of either. Its rerun called the restored
// retention "changed after the import", and once setup included the
// projects and raised retention again, the next undo excluded them and
// shortened retention a second time. The rerun now records both.
func TestBackfillUndoRecordsWhatAnInterruptedRunChanged(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	backdateTranscripts(t, f)
	if _, errOut, code := f.importRun(t, strings.NewReader("edit\n365\ny\n"), true, "--background"); code != 0 {
		t.Fatalf("import: %d %s", code, errOut)
	}
	if _, errOut, code := f.undoRun(t, nil, false, "--yes", "--restore-retention"); code != 0 {
		t.Fatalf("undo: %d %s", code, errOut)
	}
	// The run stopped before recording its configuration changes.
	batch, _ := loadBatch(t, f.data)
	if len(batch.ProjectsExcluded) == 0 || batch.Retention == nil {
		t.Fatalf("batch %+v", batch)
	}
	batch.ProjectsExcluded, batch.Retention.Restored = nil, false
	if err := backfill.SaveBatch(f.data, batch); err != nil {
		t.Fatal(err)
	}

	out, errOut, code := f.undoRun(t, nil, false, "--yes", "--restore-retention")
	if code != 0 || strings.Contains(out, "Retention stays at") {
		t.Fatalf("rerun: %d\n%s\n%s", code, out, errOut)
	}
	if batch, _ = loadBatch(t, f.data); len(batch.ProjectsExcluded) == 0 || !batch.Retention.Restored {
		t.Fatalf("the rerun did not record the changes: %+v", batch)
	}

	// Setup includes the projects again and raises retention back.
	cfg, _, _ := config.Load(f.data)
	for i := range cfg.Archive.Projects {
		cfg.Archive.Projects[i].Included = true
	}
	cfg.RetentionDays = 365
	if err := config.Save(f.data, cfg); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code = f.undoRun(t, nil, false, "--yes", "--restore-retention"); code != 0 {
		t.Fatalf("later undo: %d %s", code, errOut)
	}
	after, _, _ := config.Load(f.data)
	if after.RetentionDays != 365 || includedProjects(after.Archive.Projects) != len(after.Archive.Projects) {
		t.Fatalf("a later undo changed setup's choices again: retention %d, projects %+v", after.RetentionDays, after.Archive.Projects)
	}
}

// addOldHookSession registers a hook-captured session in the first
// configured project, admitted days ago in the current destination: one a
// shorter retention deletes although it is not from any import.
func addOldHookSession(t *testing.T, f *backfillFixture, days int) {
	t.Helper()
	cfg, _, err := config.Load(f.data)
	if err != nil || len(cfg.Archive.Projects) == 0 {
		t.Fatalf("config %+v, %v", cfg.Archive.Projects, err)
	}
	root := cfg.Archive.Projects[0].Root
	store, err := state.Open(f.data)
	if err != nil {
		t.Fatal(err)
	}
	admitted := backfillNow.Add(-time.Duration(days) * 24 * time.Hour).UTC()
	if _, err := store.RegisterNewSession(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: "old-hook-session"}, func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{
			ArchiveSessionID: id,
			NativeSessionID:  "old-hook-session",
			ProjectID:        archive.ProjectID(root),
			ProjectRoot:      root,
			Harness:          archive.Harness{Name: "claude"},
			SessionStartedAt: admitted.Add(-time.Hour),
			StartedAtSource:  archive.StartedAtSourceTranscript,
			RegisteredAt:     admitted,
			AdmittedAt:       admitted,
			Origin:           archive.SessionOriginHook,
			DestinationID:    cfg.DestinationID(),
		}
	}); err != nil {
		t.Fatal(err)
	}
}

// Orchestrator decision on PR #53: restoring a shorter retention deletes
// sessions that are not from the import (here a hook-captured one 200 days
// old), so `undo --yes` leaves retention as it is and says how to restore
// it; `--restore-retention` then restores it, as its own later run.
func TestBackfillUndoYesKeepsRetentionUnlessAsked(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	backdateTranscripts(t, f)
	if _, errOut, code := f.importRun(t, strings.NewReader("edit\n365\ny\n"), true, "--background"); code != 0 {
		t.Fatalf("import: %d %s", code, errOut)
	}
	addOldHookSession(t, f, 200)
	batch, _ := loadBatch(t, f.data)

	out, errOut, code := f.undoRun(t, nil, false, "--yes")
	if code != 0 {
		t.Fatalf("undo: %d %s\n%s", code, errOut, out)
	}
	want := "Retention stays at 365 days: --yes does not shorten it. To put back\n    the 90 days from before the import, which deletes 1 session older than 90 days, run\n    agent-archive backfill undo " + batch.ID + " --restore-retention"
	if !strings.Contains(out, want) || strings.Contains(out, "restored retention") {
		t.Fatalf("undo output:\n%s", out)
	}
	if cfg, _, _ := config.Load(f.data); cfg.RetentionDays != 365 {
		t.Fatalf("--yes shortened retention to %d", cfg.RetentionDays)
	}
	if batch, _ = loadBatch(t, f.data); batch.UndoneAt == nil || batch.Retention == nil || batch.Retention.Restored {
		t.Fatalf("batch %+v, retention %+v", batch, batch.Retention)
	}

	// Rerunning --yes to finish changes nothing and repeats the way out.
	out, _, code = f.undoRun(t, nil, false, "--yes")
	if code != 0 || !strings.Contains(out, "nothing left to undo") || !strings.Contains(out, "--restore-retention") {
		t.Fatalf("rerun: %d\n%s", code, out)
	}
	if cfg, _, _ := config.Load(f.data); cfg.RetentionDays != 365 {
		t.Fatalf("rerun shortened retention to %d", cfg.RetentionDays)
	}

	out, errOut, code = f.undoRun(t, nil, false, batch.ID, "--yes", "--restore-retention")
	if code != 0 || !strings.Contains(out, "Restored retention to 90 days") {
		t.Fatalf("restore: %d %s\n%s", code, errOut, out)
	}
	if cfg, _, _ := config.Load(f.data); cfg.RetentionDays != 90 {
		t.Fatalf("retention after --restore-retention %d", cfg.RetentionDays)
	}
	if batch, _ = loadBatch(t, f.data); !batch.Retention.Restored {
		t.Fatalf("restore not recorded: %+v", batch.Retention)
	}
}

// Interactively, undo shows the count and the question names it; declining
// changes nothing, and yes restores.
func TestBackfillUndoAsksBeforeShorteningRetention(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	backdateTranscripts(t, f)
	if _, errOut, code := f.importRun(t, strings.NewReader("edit\n365\ny\n"), true, "--background"); code != 0 {
		t.Fatalf("import: %d %s", code, errOut)
	}
	addOldHookSession(t, f, 200)

	out, _, code := f.undoRun(t, strings.NewReader("n\n"), true)
	if code != 0 || !strings.Contains(out, "Restoring retention to 90 days also deletes 1 other session older than that.") {
		t.Fatalf("declined: %d\n%s", code, out)
	}
	if cfg, _, _ := config.Load(f.data); cfg.RetentionDays != 365 {
		t.Fatalf("declining changed retention to %d", cfg.RetentionDays)
	}

	out, errOut, code := f.undoRun(t, strings.NewReader("y\n"), true)
	if code != 0 || !strings.Contains(out, "The next collector pass then deletes 1 session older than") || !strings.Contains(out, "restored retention to 90 days") {
		t.Fatalf("confirmed: %d %s\n%s", code, errOut, out)
	}
	if cfg, _, _ := config.Load(f.data); cfg.RetentionDays != 90 {
		t.Fatalf("retention after a confirmed undo %d", cfg.RetentionDays)
	}
}

// --restore-retention is for a whole import: with --project it is a usage
// error, and nothing is read or changed.
func TestBackfillUndoRestoreRetentionRejectsProject(t *testing.T) {
	t.Parallel()
	f, _ := newImportFixture(t)
	_, errOut, code := f.undoRun(t, nil, false, "--yes", "--restore-retention", "--project", f.userHome)
	if code != 2 || !strings.Contains(errOut, "--restore-retention") {
		t.Fatalf("code %d: %s", code, errOut)
	}
}

func TestBackfillForeverImportUndoKeepsPolicyUntilExplicitRestore(t *testing.T) {
	f, _ := newImportFixture(t)
	backdateTranscripts(t, f)
	out, errOut, code := f.importRun(t, strings.NewReader("edit\nforever\ny\n"), true, "--background")
	if code != 0 || !strings.Contains(out, "no automatic expiry") {
		t.Fatalf("import %d %s\n%s", code, errOut, out)
	}
	batch, _ := loadBatch(t, f.data)
	if batch.Retention == nil || batch.Retention.From != 90 || batch.Retention.To != 0 {
		t.Fatalf("journal %+v", batch.Retention)
	}
	addOldHookSession(t, f, 200)
	out, errOut, code = f.undoRun(t, nil, false, "--yes")
	if code != 0 || !strings.Contains(out, "Retention stays at Keep forever") || !strings.Contains(out, "deletes 1 session") {
		t.Fatalf("undo %d %s\n%s", code, errOut, out)
	}
	cfg, _, err := config.Load(f.data)
	if err != nil || cfg.RetentionDays != 0 || !cfg.RetentionProtection {
		t.Fatalf("undo shortened policy %+v %v", cfg, err)
	}
	out, errOut, code = f.undoRun(t, nil, false, batch.ID, "--yes", "--restore-retention")
	if code != 0 || !strings.Contains(out, "Restored retention to 90 days") {
		t.Fatalf("restore %d %s\n%s", code, errOut, out)
	}
	cfg, _, err = config.Load(f.data)
	if err != nil || cfg.RetentionDays != 90 || !cfg.RetentionProtection {
		t.Fatalf("restore %+v %v", cfg, err)
	}
}
