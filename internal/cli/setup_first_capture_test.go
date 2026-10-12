package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/platform"
)

// Without a known app, setup keeps its individual app questions.
func TestOfferFirstCaptureAsksNothingWithoutKnownApps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		detect []string
	}{

		{"no apps", nil},
		{"only apps setup does not know", []string{"windsurf"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			p := newPrompter(strings.NewReader(""), &out)
			cfg := config.Config{}
			done, err := offerFirstCapture(allHarnesses, p, &cfg, tc.detect)
			if done || err != nil || out.Len() != 0 || len(cfg.Harnesses) != 0 || len(cfg.Archive.Projects) != 0 {
				t.Fatalf("done=%v err=%v cfg=%+v asked:\n%s", done, err, cfg, &out)
			}
		})
	}
}

// The repository setup was run from heads the project list, unless it is too
// broad to archive on one Enter: the home folder or a folder holding it, or a
// temporary folder or a folder holding one, however far up the repository
// is. Then there is no current project, nothing is pre-selected, and the
// refusal says why. A repository inside a temporary folder is an ordinary
// project, as backfill treats it.
func TestCurrentProjectRefusesOnlyBroadFolders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// gitAt lists folders under the case's root that hold a .git.
		gitAt []string
		// cwd is where setup runs, under the root.
		cwd string
		// want is the project's folder under the root, or "" for none.
		want string
		// refusal is what the one-line refusal must contain, or "" for none.
		refusal string
	}{
		{"home folder", []string{"home"}, "home", "", "as a project: it is your home folder. Enter the projects you want."},
		{"inside the home folder, repository above it", []string{"."}, "home/src/app", "", "as a project: it holds your home folder."},
		{"folder holding home", []string{"."}, ".", "", "as a project: it holds your home folder."},
		{"temporary folder", []string{"var/tmp"}, "var/tmp", "", "as a project: it is a temporary folder."},
		{"folder holding a temporary folder", []string{"var"}, "var/tmp/x", "", "as a project: it holds a temporary folder."},
		{"repository inside a temporary folder", []string{"var/tmp/x"}, "var/tmp/x", "var/tmp/x", ""},
		{"ordinary repository", []string{"work/app"}, "work/app/pkg", "work/app", ""},
		{"no repository", nil, "work/app", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, err := filepath.EvalSymlinks(t.TempDir())
			must(t, err)
			at := func(rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }
			for _, dir := range []string{"home/src/app", "var/tmp/x", "work/app/pkg"} {
				must(t, os.MkdirAll(at(dir), 0o700))
			}
			for _, dir := range tc.gitAt {
				must(t, os.MkdirAll(filepath.Join(at(dir), ".git"), 0o700))
			}
			env := Env{WorkingDir: func() (string, error) { return at(tc.cwd), nil }, BackfillTempDirs: []string{at("var/tmp")}}
			want := ""
			if tc.want != "" {
				want = at(tc.want)
			}
			got, refused := currentProject(env, at("home"))
			if got != want {
				t.Fatalf("currentProject = %q, want %q", got, want)
			}
			if (tc.refusal == "") != (refused == "") || !setupContainsText(refused, tc.refusal) || (refused != "" && !strings.HasPrefix(refused, "Not offering ")) {
				t.Fatalf("refusal = %q, want one containing %q", refused, tc.refusal)
			}
		})
	}
}

// The refusal names the folder from ~ and what to do.
func TestCurrentProjectRefusalNamesTheFolder(t *testing.T) {
	t.Parallel()
	home, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(home, ".git"), 0o700))
	env := Env{WorkingDir: func() (string, error) { return home, nil }, BackfillTempDirs: []string{}}
	_, refused := currentProject(env, home)
	if want := "Not offering ~ as a project: it is your home folder. Enter the projects you want."; refused != want {
		t.Fatalf("refusal = %q, want %q", refused, want)
	}
}

// Setup's temporary folders are backfill's: the operating system's defaults
// and $TMPDIR, from one list, so a repository at /tmp/x is treated alike by
// both, on macOS and on Linux.
func TestSetupAndBackfillShareOneTempFolderList(t *testing.T) {
	t.Parallel()
	for goos, refused := range map[platform.OS][]string{
		platform.Darwin: {"/tmp", "/private/tmp", "/scratch/tmp"},
		platform.Linux:  {"/tmp", "/var/tmp", "/scratch/tmp"},
	} {
		t.Run(string(goos), func(t *testing.T) {
			t.Parallel()
			env := Env{OS: goos, LookupEnv: func(key string) (string, bool) { return "/scratch/tmp", key == "TMPDIR" }}
			temps := env.backfillTempDirs()
			for _, want := range append(backfill.Environment{Sources: productionAgents, OS: goos}.DefaultTempDirs(), "/scratch/tmp") {
				if !slices.Contains(temps, want) {
					t.Fatalf("temporary folders %v lack %s", temps, want)
				}
			}
			if got := env.backfillEnvironment("/Users/alex", config.Config{}).TempDirs; !slices.Equal(got, temps) {
				t.Fatalf("backfill's list %v differs from setup's %v", got, temps)
			}
			for _, dir := range refused {
				if broadFolder(dir, "/Users/alex", temps) == "" {
					t.Errorf("%s is not refused as a temporary folder", dir)
				}
			}
			if broadFolder("/tmp/x", "/Users/alex", temps) != "" {
				t.Error("a repository inside a temporary folder was refused")
			}
		})
	}
}

// On a case-insensitive volume a folder spelled with another case is the
// same folder: a home folder <base>/Home holding a .git is still refused
// when setup runs from <base>/home. Skipped where the file system tells the
// two apart.
func TestBroadFolderIgnoresCaseOnCaseInsensitiveVolumes(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	home := filepath.Join(root, "Home")
	must(t, os.MkdirAll(filepath.Join(home, ".git"), 0o700))
	other := filepath.Join(root, "home")
	if _, err := os.Stat(other); err != nil {
		t.Skip("the file system tells Home and home apart")
	}
	if reason := broadFolder(other, home, nil); reason != "it is your home folder" {
		t.Fatalf("reason = %q", reason)
	}
	env := Env{WorkingDir: func() (string, error) { return other, nil }, BackfillTempDirs: []string{}}
	if got, refused := currentProject(env, home); got != "" || refused == "" {
		t.Fatalf("currentProject = %q, refused %q", got, refused)
	}
}

// A folder next to the home folder or a temporary folder, neither holding
// nor being one, is not broad.
func TestBroadFolderIgnoresNeighbors(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"/Users/alex/src/app", "/Users/alexandra", "/private/var/folders/xy/Tools/app"} {
		if reason := broadFolder(dir, "/Users/alex", []string{"/private/var/folders/xy/T"}); reason != "" {
			t.Errorf("broadFolder(%s) = %q", dir, reason)
		}
	}
}

// The answer guesses an ordinary folder.
func TestOfferFirstCaptureGuessesTheCurrentProject(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\n"), &out)
	cfg := config.Config{}
	done, err := offerFirstCapture(allHarnesses, p, &cfg, []string{"claude"})
	if !done || err != nil || len(cfg.Archive.Projects) != 0 || len(cfg.Harnesses) != 1 {
		t.Fatalf("done=%v err=%v cfg=%+v\n%s", done, err, cfg, &out)
	}
}

// Folders are compared with their symlinks resolved, as project roots are
// saved: a temporary folder reached through a link is still that folder.
func TestBroadFolderResolvesSymlinks(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	target := filepath.Join(root, "target")
	must(t, os.MkdirAll(filepath.Join(target, "app"), 0o700))
	link := filepath.Join(root, "link")
	must(t, os.Symlink(target, link))
	if broadFolder(target, "/Users/alex", []string{link}) == "" {
		t.Fatal("a temporary folder, named through a link, was taken for ordinary")
	}
	if broadFolder(link, filepath.Join(target, "app"), nil) == "" {
		t.Fatal("a link to a folder holding the home folder was taken for ordinary")
	}
}

// chooseCapture keeps a refused folder out of the project list by itself,
// not only through the first-run question: with apps already chosen, so that
// question is not asked, a home folder that is a repository is not
// pre-selected and its refusal is printed.
func TestChooseCaptureDoesNotPreselectABroadFolder(t *testing.T) {
	t.Parallel()
	userHome, project := t.TempDir(), t.TempDir()
	must(t, os.Mkdir(filepath.Join(userHome, ".git"), 0o700))
	env := setupTestEnv(t, t.TempDir(), userHome, newFakeKeychain(), time.Now())
	env.WorkingDir = func() (string, error) { return userHome, nil }
	env.DetectHarnesses = func(string) []string { return []string{"claude"} }
	cfg := config.Config{Harnesses: []string{"claude"}}
	var out bytes.Buffer
	// Keep the apps, then name the project.
	p := newPrompter(strings.NewReader("\n"+project+"\n\n"), &out)
	must(t, chooseCapture(p, &cfg, userHome, env, nil))
	resolved, err := filepath.EvalSymlinks(project)
	must(t, err)
	if len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != resolved {
		t.Fatalf("projects = %+v\n%s", cfg.Archive.Projects, &out)
	}
	if !setupContainsText(out.String(), "Not offering ~ as a project: it is your home folder.") {
		t.Fatalf("the refusal was not printed:\n%s", &out)
	}
}

// Selecting apps leaves project consent to the dedicated selector.
func TestOfferFirstCaptureLeavesProjectConsentSeparate(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\n"), &out)
	cfg := config.Config{}
	done, err := offerFirstCapture(allHarnesses, p, &cfg, []string{"claude"})
	if !done || err != nil {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if len(cfg.Archive.Projects) != 0 || p.reviewHint != "" || !setupContainsText(out.String(), "Capture sessions from Claude Code?") {
		t.Fatalf("app selection bypassed project consent: %+v\n%s", cfg, &out)
	}
}

// Anything but a first setup, such as one that already has apps, asks the
// separate questions with its saved answers as defaults.
func TestOfferFirstCaptureSkipsConfiguredSetups(t *testing.T) {
	t.Parallel()
	for _, cfg := range []config.Config{
		{Harnesses: []string{"claude"}},
		{DeclinedHarnesses: []string{"cursor"}},
	} {
		var out bytes.Buffer
		p := newPrompter(strings.NewReader(""), &out)
		done, err := offerFirstCapture(allHarnesses, p, &cfg, []string{"claude"})
		if done || err != nil || out.Len() != 0 {
			t.Fatalf("%+v: done=%v err=%v asked:\n%s", cfg, done, err, &out)
		}
	}
}

// A first setup run from the home folder keeps the separate questions, so
// one Enter never archives everything under it.
func TestSetupFirstRunFromHomeFolderAsksSeparately(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.withApps(t, "claude")
	// The home folder is itself a repository, as with a dotfiles checkout.
	must(t, os.Mkdir(filepath.Join(f.userHome, ".git"), 0o700))
	f.env.WorkingDir = func() (string, error) { return f.userHome, nil }
	project := f.project(t, "src/web-app")
	out := f.runSetup(t, strings.Join([]string{"", project, "", "s3-existing", "work", "2", ""}, "\n")+"\n")
	if setupContainsText(out, "Archive Claude Code sessions in") || !setupContainsText(out, "Capture sessions from Claude Code?") {
		t.Fatalf("home folder was offered on one Enter:\n%s", out)
	}
	cfg, _, err := config.Load(f.home)
	must(t, err)
	for _, saved := range cfg.Archive.Projects {
		if saved.Root == f.userHome {
			t.Fatalf("archived the home folder: %+v", cfg.Archive.Projects)
		}
	}
}

// setupOutput runs setup with the answers and returns what it printed,
// whatever it exited with.
func (f *screenFixture) setupOutput(answers ...string) string {
	var out bytes.Buffer
	Run([]string{"setup"}, strings.NewReader(strings.Join(answers, "\n")+"\n"), &out, &out, f.env)
	return out.String()
}

// A resumed draft that changes apps and projects, and a reconfiguration of
// an installed setup, ask the separate questions with their saved answers,
// never the combined first-run question.
func TestSetupResumeAndReconfigureSkipTheCombinedQuestion(t *testing.T) {
	t.Parallel()
	const combined = "Capture sessions from Claude Code?"
	t.Run("resumed draft", func(t *testing.T) {
		t.Parallel()
		f := newScreenFixture(t)
		f.withApps(t, "claude")
		f.inWebApp(t)
		// The first run asks the combined question, then stops at the
		// storage step, leaving a draft.
		if first := f.setupOutput("", "", ""); !setupContainsText(first, "Which projects?") {
			t.Fatalf("the first run did not ask the combined question:\n%s", first)
		}
		out := f.setupOutput("capture", "", "", "s3-existing", "work", "2", "3")
		if setupContainsText(out, combined) || !setupContainsText(out, "Change which apps are included?") {
			t.Fatalf("resuming did not ask the separate questions:\n%s", out)
		}
	})
	t.Run("reconfigure", func(t *testing.T) {
		t.Parallel()
		f := newScreenFixture(t)
		f.installed(t)
		f.withApps(t, "claude")
		f.inWebApp(t)
		out := f.setupOutput("capture", "n", "n", "")
		if setupContainsText(out, combined) || !setupContainsText(out, "Choose what to capture") || !setupContainsText(out, "Included: Codex.") {
			t.Fatalf("reconfiguring did not ask the separate questions:\n%s", out)
		}
	})
}

// The review's hint about Edit a setting is for the first pass: once the
// person has used the edit menu it would only be stale, so the second pass
// leaves it out.
func TestReviewHintIsNotRepeatedAfterAnEdit(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.withApps(t, "claude")
	f.inWebApp(t)
	out := f.setupOutput("", "", "s3-existing", "work", "2", "edit", "retention", "custom", "30", "")
	if n := strings.Count(out, "\n  Edit a setting adds projects, drops apps"); n != 0 {
		t.Fatalf("the review showed the hint %d times, want zero (actions are in the question):\n%s", n, out)
	}
	if cfg, found, err := config.Load(f.home); err != nil || !found || cfg.RetentionDays != 30 {
		t.Fatalf("the edit was not saved: %+v %v", cfg, err)
	}
}

// A new capture choice starts without the last one's hint.
func TestChooseCaptureClearsTheReviewHint(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	env := setupTestEnv(t, t.TempDir(), userHome, newFakeKeychain(), time.Now())
	env.DetectHarnesses = func(string) []string { return []string{"claude"} }
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("\n"+t.TempDir()+"\n\n"), &out)
	p.reviewHint = "stale"
	cfg := config.Config{Harnesses: []string{"claude"}}
	must(t, chooseCapture(p, &cfg, userHome, env, nil))
	if p.reviewHint != "" {
		t.Fatalf("hint kept: %q", p.reviewHint)
	}
}

// Where prompts are off (an agent's shell, or AGENT_ARCHIVE_NONINTERACTIVE),
// a first setup in a repository refuses before it asks the combined
// question or reads the apps' history, and changes nothing.
func TestSetupFirstRunAsksNothingWhereInteractionIsOff(t *testing.T) {
	t.Parallel()
	for name, vars := range map[string]map[string]string{
		"agent shell": agentShell("CODEX_THREAD_ID"),
		"switch":      {envNonInteractive: "1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newScreenFixture(t)
			f.withApps(t, "claude")
			f.inWebApp(t)
			out, errOut, code := ttyRun(t, withEnvironment(f.env, vars), "\n\n\n", "setup")
			if code != 1 || !setupContainsText(errOut, "Nothing was changed") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
			for _, text := range []string{"Archive Claude Code sessions in", "Looking for your other projects"} {
				if setupContainsText(out+errOut, text) {
					t.Fatalf("asked %q:\n%s%s", text, out, errOut)
				}
			}
			if _, found, err := config.Load(f.home); err != nil || found {
				t.Fatalf("setup saved a configuration: %v %v", found, err)
			}
		})
	}
}
