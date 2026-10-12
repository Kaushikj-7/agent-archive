package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
)

const importDay = 24 * time.Hour

// setupImportAnswers set Claude Code up in ~/src/web-app, storing in S3.
// Setup asks nothing about importing.
func setupImportAnswers() string {
	return strings.Join([]string{"", "specific", "2", "", "s3-existing", "work", "2", ""}, "\n") + "\n"
}

// setupYesArgs set Claude Code up in ~/src/web-app, storing in S3, with
// setup --yes.
var setupYesArgs = []string{"--yes", "--provider", "s3", "--bucket", "team-archive", "--aws-profile", "work", "--region", "us-east-1", "--apps", "claude", "--project", "~/src/web-app"}

// newImportOfferFixture is a Mac with Claude Code, run from ~/src/web-app,
// with sessions there that started 2, 6, and 9 days ago, and one in a
// project setup leaves out.
func newImportOfferFixture(t *testing.T) *screenFixture {
	t.Helper()
	f := newScreenFixture(t)
	f.withApps(t, "claude")
	f.inWebApp(t)
	f.pastSession(t, "two", "src/web-app", screenNow.Add(-2*importDay))
	f.pastSession(t, "six", "src/web-app", screenNow.Add(-6*importDay))
	f.pastSession(t, "nine", "src/web-app", screenNow.Add(-9*importDay))
	f.pastSession(t, "other", "src/api", screenNow.Add(-2*importDay))
	return f
}

func (f *screenFixture) runSetup(t *testing.T, input string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if code := Run(append([]string{"setup"}, args...), strings.NewReader(input), &out, &out, f.env); code != 0 {
		t.Fatalf("setup exit %d\n%s", code, &out)
	}
	return out.String()
}

// importedSessions lists, sorted, the native IDs of the sessions imports
// registered.
func importedSessions(t *testing.T, home string) []string {
	t.Helper()
	batches, err := backfill.LoadBatches(home)
	must(t, err)
	var ids []string
	for _, b := range batches {
		parents, _ := importRegistrations(t, home, b.ID)
		for _, reg := range parents {
			ids = append(ids, reg.NativeSessionID)
		}
	}
	slices.Sort(ids)
	return ids
}

// Setup, interactive or --yes, imports the included project's sessions of
// the last 7 days without asking, as one import record that backfill
// history lists, and says one older session is left to backfill.
func TestSetupImportsTheLastSevenDays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		input string
		args  []string
	}{
		{"interactive", setupImportAnswers(), nil},
		{"yes", "", setupYesArgs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newImportOfferFixture(t)
			out := f.runSetup(t, tc.input, tc.args...)
			for _, want := range []string{
				"Imported 2 sessions from the last 7 days (Claude Code 2). Uploading in the background.\n",
				"1 older session: run agent-archive backfill to import it.\n",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q:\n%s", want, out)
				}
			}
			for _, unwanted := range []string{"Import these sessions?", "Sessions already open are not captured", "Import sessions from before setup", "Recent sessions were not imported"} {
				if strings.Contains(out, unwanted) {
					t.Fatalf("output has %q:\n%s", unwanted, out)
				}
			}
			if ids := importedSessions(t, f.home); strings.Join(ids, ",") != "six,two" {
				t.Fatalf("imported %v, want six and two", ids)
			}
			batches, err := backfill.LoadBatches(f.home)
			must(t, err)
			if len(batches) != 1 || batches[0].CompletedAt == nil || batches[0].Filters.SinceArg != "7d" || len(batches[0].ProjectsAdded) != 0 {
				t.Fatalf("import record %+v", batches)
			}
			var history bytes.Buffer
			if code := Run([]string{"backfill", "history"}, strings.NewReader(""), &history, &history, f.env); code != 0 || !strings.Contains(history.String(), batches[0].ID) {
				t.Fatalf("history exit %d\n%s", code, &history)
			}
		})
	}
}

// The import comes before the steps that say how to check capture.
func TestSetupImportsBeforeNextSteps(t *testing.T) {
	t.Parallel()
	f := newImportOfferFixture(t)
	out := f.runSetup(t, setupImportAnswers())
	saved := strings.Index(out, "Setup complete")
	imported := strings.Index(out, "Imported 2 sessions")
	steps := strings.Index(out, "Check progress with agent-archive status.")
	another := strings.Index(out, "Another machine:")
	if saved < 0 || imported < saved || steps < imported || another < steps {
		t.Fatalf("order saved=%d imported=%d steps=%d another=%d:\n%s", saved, imported, steps, another, out)
	}
}

// backfill undo of setup's import removes exactly its sessions and leaves
// the project setup chose.
func TestSetupImportUndo(t *testing.T) {
	t.Parallel()
	f := newImportOfferFixture(t)
	f.runSetup(t, "", setupYesArgs...)
	batches, err := backfill.LoadBatches(f.home)
	must(t, err)
	if len(batches) != 1 {
		t.Fatalf("import records %+v", batches)
	}
	var undoOut bytes.Buffer
	if code := Run([]string{"backfill", "undo", "--yes", batches[0].ID}, strings.NewReader(""), &undoOut, &undoOut, f.env); code != 0 {
		t.Fatalf("undo exit %d\n%s", code, &undoOut)
	}
	if left := importedSessions(t, f.home); len(left) != 0 {
		t.Fatalf("still registered after undo: %v", left)
	}
	cfg, _, err := config.Load(f.home)
	must(t, err)
	if len(cfg.Archive.Projects) != 1 || includedProjects(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != filepath.Join(f.userHome, "src", "web-app") {
		t.Fatalf("projects after undo %+v", cfg.Archive.Projects)
	}
}

// claudeSession writes a Claude Code session run from cwd, which need not
// exist, into the folder slug of ~/.claude/projects.
func (f *screenFixture) claudeSession(t *testing.T, id, cwd, slug string, start time.Time) {
	t.Helper()
	records := fmt.Sprintf(`{"type":"user","uuid":"a","sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"role":"user","content":"please check it"}}
{"type":"assistant","uuid":"b","sessionId":%q,"timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":"Checked."}]}}
`, id, cwd, start.Format(time.RFC3339), id, start.Add(time.Minute).Format(time.RFC3339))
	path := filepath.Join(f.userHome, ".claude", "projects", slug, id+".jsonl")
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte(records), 0o600))
}

// Setup's import never adds a project: recent sessions in a folder setup
// did not include, a temporary folder, or a worktree that is gone are
// neither imported nor added.
func TestSetupImportAddsNoProjects(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.project(t, "src/web-app")
	f.pastSession(t, "web", "src/web-app", screenNow.Add(-importDay))
	f.pastSession(t, "repo", "src/api", screenNow.Add(-importDay))
	f.claudeSession(t, "plain", filepath.Join(f.userHome, "notes"), "notes", screenNow.Add(-importDay))
	must(t, os.MkdirAll(filepath.Join(f.userHome, "notes"), 0o700))
	scratch := filepath.Join(f.env.TempDir(), "scratch")
	must(t, os.MkdirAll(scratch, 0o700))
	f.claudeSession(t, "temp", scratch, "tmp-scratch", screenNow.Add(-importDay))
	f.claudeSession(t, "gone", filepath.Join(f.userHome, "src", "web-app-feature"), "web-app-feature", screenNow.Add(-importDay))
	out := f.runSetup(t, "", setupYesArgs...)
	if !strings.Contains(out, "Imported 1 session from the last 7 days (Claude Code 1).") || strings.Contains(out, "older session") {
		t.Fatalf("output:\n%s", out)
	}
	if ids := importedSessions(t, f.home); strings.Join(ids, ",") != "web" {
		t.Fatalf("imported %v", ids)
	}
	cfg, _, err := config.Load(f.home)
	must(t, err)
	if len(cfg.Archive.Projects) != 1 || len(cfg.ImportedHarnesses) != 0 {
		t.Fatalf("projects %+v, imported apps %v", cfg.Archive.Projects, cfg.ImportedHarnesses)
	}
	batches, err := backfill.LoadBatches(f.home)
	must(t, err)
	if len(batches) != 1 || len(batches[0].ProjectsAdded) != 0 || len(batches[0].ProjectsKeptOut) != 0 || len(batches[0].AppsAdded) != 0 {
		t.Fatalf("import record %+v", batches)
	}
}

// With nothing from the last 7 days and nothing older, setup says nothing
// about importing.
func TestSetupSaysNothingWithNothingToImport(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.project(t, "src/web-app")
	// A session in a project setup leaves out is not imported.
	f.pastSession(t, "other", "src/api", screenNow.Add(-2*time.Hour))
	out := f.runSetup(t, "", setupYesArgs...)
	if strings.Contains(out, "Imported") || strings.Contains(out, "older session") || strings.Contains(out, "Recent sessions") {
		t.Fatalf("output:\n%s", out)
	}
	if ids := importedSessions(t, f.home); len(ids) != 0 {
		t.Fatalf("imported %v", ids)
	}
}

// Only older sessions: nothing is imported, and the older ones are named.
func TestSetupNamesOnlyOlderSessions(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.project(t, "src/web-app")
	f.pastSession(t, "nine", "src/web-app", screenNow.Add(-9*importDay))
	f.pastSession(t, "ten", "src/web-app", screenNow.Add(-10*importDay))
	out := f.runSetup(t, "", setupYesArgs...)
	if strings.Contains(out, "Imported") || !strings.Contains(out, "2 older sessions: run agent-archive backfill to import them.\n") {
		t.Fatalf("output:\n%s", out)
	}
	if ids := importedSessions(t, f.home); len(ids) != 0 {
		t.Fatalf("imported %v", ids)
	}
}

// While capture is paused, setup imports nothing: backfill would refuse it.
//
// The same rerun without the pause imports the session, so the paused case
// reaches the import with something to import. (The session is the app's,
// Claude Code: one of another app would be left out by the app filter
// whether or not capture is paused.)
func TestSetupWhilePausedImportsNothing(t *testing.T) {
	t.Parallel()
	for _, paused := range []bool{true, false} {
		t.Run(fmt.Sprintf("paused=%v", paused), func(t *testing.T) {
			t.Parallel()
			f := newScreenFixture(t)
			f.withApps(t, "claude")
			f.inWebApp(t)
			f.runSetup(t, "", setupYesArgs...)
			f.env.Now = func() time.Time { return screenNow.Add(time.Minute) }
			if paused {
				var out bytes.Buffer
				if code := Run([]string{"pause"}, nil, &out, &out, f.env); code != 0 {
					t.Fatalf("pause: %s", &out)
				}
			}
			f.pastSession(t, "one", "src/web-app", screenNow.Add(-2*time.Hour))
			got := f.runSetup(t, "", setupYesArgs...)
			ids := importedSessions(t, f.home)
			if !paused {
				if !strings.Contains(got, "Imported 1 session") || len(ids) != 1 {
					t.Fatalf("rerun did not import (%v):\n%s", ids, got)
				}
				return
			}
			if strings.Contains(got, "Imported") || strings.Contains(got, "older session") || strings.Contains(got, "Recent sessions") || !strings.Contains(got, "agent-archive resume") {
				t.Fatalf("import while paused:\n%s", got)
			}
			if len(ids) != 0 {
				t.Fatalf("imported %v", ids)
			}
		})
	}
}

// With no included project, or no app, there is nothing to import from, and
// nothing is said.
func TestSetupImportNeedsProjectsAndApps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*config.Config)
	}{
		{"no included project", func(cfg *config.Config) {
			for i := range cfg.Archive.Projects {
				cfg.Archive.Projects[i].Included = false
			}
		}},
		{"no app", func(cfg *config.Config) { cfg.Harnesses = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newScreenFixture(t)
			f.installed(t)
			f.pastSession(t, "one", "src/web-app", screenNow.Add(-2*time.Hour))
			cfg, _, err := config.Load(f.home)
			must(t, err)
			tc.change(&cfg)
			must(t, config.Save(f.home, cfg))
			var out bytes.Buffer
			userHome, err := f.env.userHomeDir()
			must(t, err)
			importRecentSessions(newPrompter(strings.NewReader(""), &out), &out, f.home, userHome, f.env)
			if out.Len() != 0 {
				t.Fatalf("output:\n%s", &out)
			}
			if ids := importedSessions(t, f.home); len(ids) != 0 {
				t.Fatalf("imported %v", ids)
			}
		})
	}
}

// A plan that fails leaves setup complete and names the retry.
func TestSetupImportPlanningErrorKeepsSetup(t *testing.T) {
	t.Parallel()
	f := newImportOfferFixture(t)
	// An index entry for one of the sessions that cannot be read: the plan
	// cannot tell whether that session is archived.
	key, err := agentmeta.NewSessionKey("claude", "two")
	must(t, err)
	sum := sha256.Sum256(key.Encoding())
	index := filepath.Join(f.home, "sessions-v1", hex.EncodeToString(sum[:])+".json")
	must(t, os.MkdirAll(filepath.Dir(index), 0o700))
	must(t, os.WriteFile(index, []byte("{"), 0o600))
	out := f.runSetup(t, "", setupYesArgs...)
	if !strings.Contains(out, "Recent sessions were not imported: check the archive: ") || !strings.Contains(out, ". Run agent-archive backfill --since 7d to retry.\n") || !strings.Contains(out, "Check progress with agent-archive status.") {
		t.Fatalf("output:\n%s", out)
	}
	assertSetupKept(t, f)
	if ids := importedSessions(t, f.home); len(ids) != 0 {
		t.Fatalf("imported %v", ids)
	}
}

// An import that fails while registering keeps the setup it follows and
// names the retry, which imports what is left.
func TestSetupImportFailureKeepsSetupAndNamesTheRetry(t *testing.T) {
	t.Parallel()
	f := newImportOfferFixture(t)
	f.env.backfillHoldSteps = 1
	crashed := false
	f.env.backfillCheckpoint = func(step string) error {
		if step == "registered" && !crashed {
			crashed = true
			return errors.New("simulated crash")
		}
		return nil
	}
	out := f.runSetup(t, setupImportAnswers())
	if !strings.Contains(out, "Recent sessions were not imported: simulated crash. Run agent-archive backfill --since 7d to retry.\n") || strings.Contains(out, "Imported") {
		t.Fatalf("no retry:\n%s", out)
	}
	assertSetupKept(t, f)
	f.env.backfillCheckpoint = nil
	var rerun bytes.Buffer
	args := []string{"backfill", "--yes", "--background", "--since", "7d"}
	if code := Run(args, strings.NewReader(""), &rerun, &rerun, f.env); code != 0 {
		t.Fatalf("retry exit %d\n%s", code, &rerun)
	}
	if ids := importedSessions(t, f.home); !slices.Contains(ids, "six") || !slices.Contains(ids, "two") || slices.Contains(ids, "nine") {
		t.Fatalf("imported %v\n%s", ids, &rerun)
	}
}

func assertSetupKept(t *testing.T, f *screenFixture) {
	t.Helper()
	cfg, found, err := config.Load(f.home)
	must(t, err)
	if !found || !cfg.Archive.Enabled || includedProjects(cfg.Archive.Projects) != 1 {
		t.Fatalf("setup not kept: %+v", cfg)
	}
	if _, e := os.Stat(draftPath(f.home)); !os.IsNotExist(e) {
		t.Fatalf("draft left: %v", e)
	}
}
