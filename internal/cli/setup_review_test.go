package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestSetupShowsVersionsBeforeActivationAndDiscoversOnce(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now().UTC())
	calls := 0
	env.DiscoverApplications = func(string) map[string]applicationDiscovery {
		calls++
		return map[string]applicationDiscovery{"codex": {Installed: true, Version: "1.2.3", VersionState: "observed"}}
	}
	input := s3SetupInput("test", "us-east-1", "profile", true, false, false, project)
	output := setupRun(t, env, strings.TrimSuffix(input, "y\n")+"d\nn\n", 0)
	if calls != 1 || !setupContainsText(output, "Codex 1.2.3") {
		t.Fatalf("calls %d output %s", calls, output)
	}
	if !setupContainsText(output, "Checking installed applications...") {
		t.Fatalf("discovery ran without notice: %s", output)
	}
	if _, found, _ := config.Load(home); found {
		t.Fatal("cancel activated configuration")
	}
	observations, err := readApplicationDiscoveries(home)
	if err != nil || len(observations) != 0 {
		t.Fatalf("cancel persisted discovery: %v %v", observations, err)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestSetupReviewDoesNotCallDetectedAppsNotFound(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now().UTC())
	env.DetectHarnesses = func(string) []string { return []string{"codex"} }
	env.DiscoverApplications = func(string) map[string]applicationDiscovery {
		return map[string]applicationDiscovery{"codex": {VersionState: "absent"}}
	}
	input := strings.Join([]string{"y", "included-projects", project, "", "s3-existing", "profile", "test", "us-east-1", "d", "y"}, "\n") + "\n"
	output := setupRun(t, env, input, 0)
	if !setupContainsText(output, "Codex (version not detected)") || setupContainsText(output, "not found") {
		t.Fatalf("detected app shown as not found:\n%s", output)
	}
	// Only the display changes; the recorded discovery keeps what was seen.
	observations, err := readApplicationDiscoveries(home)
	if err != nil || observations["codex"].Installed || observations["codex"].VersionState != "absent" {
		t.Fatalf("recorded discovery changed: %+v %v", observations, err)
	}
}

// Regression: pre-release review, carried over from agent-skills (e371b6a).
func TestReviewDiscoveriesKeepsUndetectedAbsentApps(t *testing.T) {
	t.Parallel()
	got := reviewDiscoveries(map[string]applicationDiscovery{
		"codex":  {VersionState: "absent"},
		"claude": {VersionState: "absent"},
		"cursor": {Installed: true, Version: "3.21.13", VersionState: "observed"},
	}, []string{"claude", "cursor"})
	for app, want := range map[string]string{"codex": "Codex (not found)", "claude": "Claude Code (version not detected)", "cursor": "Cursor 3.21.13"} {
		if line := appWithVersion(app, got[app]); line != want {
			t.Fatalf("%s: got %q want %q", app, line, want)
		}
	}
}

func TestRetentionReductionShowsImpactBeforeConfirmation(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project), 0)
	if err := handleTestHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "one", "cwd": project, "transcript_path": writeCodexTranscript(t, project)}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now.Add(45 * 24 * time.Hour) }
	output := setupRun(t, env, "retention\ncustom\n30\nn\n", 0)
	if !setupContainsText(output, "1 session captured on or before "+now.Add(15*24*time.Hour).Format(time.DateOnly)+" will be eligible for deletion.") {
		t.Fatal(output)
	}
	cfg, _, _ := config.Load(home)
	if cfg.RetentionDays != 90 {
		t.Fatal("decline applied shorter retention")
	}
}

// Rerunning setup from a shell with a different CLAUDE_CONFIG_DIR moves the
// hooks; the review names the files and says so before anything changes.
//
// Regression: hook ownership review, 2026-09 (1a9420b).
func TestSetupReviewWarnsWhenHookFilesMove(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	elsewhere := filepath.Join(userHome, "elsewhere")
	must(t, os.MkdirAll(elsewhere, 0700))
	env.LookupEnv = func(k string) (string, bool) { return elsewhere, k == "CLAUDE_CONFIG_DIR" }
	// Change retention (menu choice 3), keep 90 days, then cancel at review.
	var out, errOut bytes.Buffer
	Run([]string{"setup"}, strings.NewReader("3\n90\nd\nq\n"), &out, &errOut, env)
	output := out.String()
	if !setupContainsText(output, "~/elsewhere/settings.json") ||
		!setupContainsText(output, "Claude Code hooks move to ~/elsewhere/settings.json from ~/.claude/settings.json.") {
		t.Fatalf("no warning:\n%s%s", output, &errOut)
	}
	_ = home
}

// For a configuration that never recorded its hook files, the move warning
// says an earlier release used the fixed path; it does not claim the
// variable changed since setup last ran.
//
// Regression: hook ownership second review, 2026-09 (ffee0e3).
func TestSetupReviewMoveWarningForUnrecordedHookFiles(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	cfg, _, _ := config.Load(home)
	cfg.HookFiles = nil
	must(t, config.Save(home, cfg))
	elsewhere := filepath.Join(userHome, "elsewhere")
	must(t, os.MkdirAll(elsewhere, 0700))
	env.LookupEnv = func(k string) (string, bool) { return elsewhere, k == "CLAUDE_CONFIG_DIR" }
	var out, errOut bytes.Buffer
	Run([]string{"setup"}, strings.NewReader("3\n90\nd\nq\n"), &out, &errOut, env)
	if !setupContainsText(out.String(), "An earlier release installed them at the fixed path") || setupContainsText(out.String(), "differs from when setup last ran") {
		t.Fatalf("review:\n%s%s", &out, &errOut)
	}
}

func TestShortSetupAndReviewEdits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		edits  string
		probes int
		days   int
		prefix string
	}{
		{"accept", "y\n", 1, 90, defaultPrefix},
		{"retention", "edit\nretention\ncustom\n30\ny\n", 1, 30, defaultPrefix},
		{"folder", "edit\nprefix\n../invalid\narchive/\ny\n", 2, 90, "archive/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, project := t.TempDir(), t.TempDir()
			project, _ = filepath.EvalSymlinks(project)
			if err := os.Mkdir(filepath.Join(project, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
			env.DetectHarnesses = func(string) []string { return []string{"codex", "claude"} }
			env.WorkingDir = func() (string, error) { return project, nil }
			env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{Name: "personal", Region: "us-west-2"}}, nil }
			probes, publications := 0, 0
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
				if _, committed, err := config.Load(home); err != nil {
					t.Fatal(err)
				} else if committed {
					publications++
				} else {
					probes++
				}
				return storagetest.NewMemoryStore(), nil
			}
			out := setupRun(t, env, "\nincluded-projects\n\ns3-existing\n\ntest-bucket\n"+tc.edits, 0)
			cfg, found, err := config.Load(home)
			if err != nil || !found {
				t.Fatalf("load: %v", err)
			}
			if probes != tc.probes || publications != 1 || cfg.RetentionDays != tc.days || cfg.Storage.Prefix != tc.prefix {
				t.Fatalf("verification probes=%d postcommit publications=%d config=%+v", probes, publications, cfg)
			}
			if cfg.Storage.Region != "us-west-2" || cfg.Storage.AWSProfile != "personal" || len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != project {
				t.Fatalf("unexpected config: %+v", cfg)
			}
			for _, unwanted := range []string{"Change these settings?", "Bucket region (", "Project path"} {
				if setupContainsText(out, unwanted) {
					t.Fatalf("unexpected %q: %s", unwanted, out)
				}
			}
		})
	}
}

func TestReviewEditCancellationDoesNotInstall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		ending string
		code   int
	}{
		{"cancel", "n\n", 0}, {"EOF", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
			input := s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir())
			input = strings.TrimSuffix(input, "y\n") + "edit\nretention\ncustom\n30\n" + tc.ending
			setupRun(t, env, input, tc.code)
			if _, found, err := config.Load(home); err != nil || found {
				t.Fatalf("installed without confirmation: found=%v err=%v", found, err)
			}
		})
	}
}

// The review checks each chosen app's hook file as it is at the review:
// one that became invalid after the checks before the first question, or
// that those checks did not cover, shows as ✗, each such file on a line of
// its own.
func TestReviewChecklistChecksHookFilesAsTheyAreNow(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	settings := filepath.Join(userHome, ".claude", "settings.json")
	cursor := filepath.Join(userHome, ".cursor", "hooks.json")
	codex := filepath.Join(userHome, ".codex", "hooks.json")
	for _, path := range []string{settings, cursor} {
		must(t, os.MkdirAll(filepath.Dir(path), 0o700))
		must(t, os.WriteFile(path, []byte("{\n  // a comment\n}\n"), 0o600))
	}
	files := hooks.Files{"codex": codex, "claude": settings, "cursor": cursor}
	got := hookFilesChecks([]string{"codex"}, files, userHome)
	if len(got) != 1 || got[0].mark != symbolOK || got[0].label != "Hook file is valid" || got[0].detail != "~/.codex/hooks.json" {
		t.Fatalf("valid file not reported: %+v", got)
	}
	got = hookFilesChecks([]string{"codex", "claude", "cursor"}, files, userHome)
	if len(got) != 3 ||
		got[0].mark != symbolFail || got[0].label != "Claude Code hook file is invalid" || !strings.HasPrefix(got[0].detail, "~/.claude/settings.json:2:") ||
		got[1].mark != symbolFail || got[1].label != "Cursor hook file is invalid" || !strings.HasPrefix(got[1].detail, "~/.cursor/hooks.json:2:") ||
		got[2].mark != symbolOK || got[2].detail != "~/.codex/hooks.json" {
		t.Fatalf("invalid files not each reported: %+v", got)
	}
	if got = hookFilesChecks(nil, files, userHome); len(got) != 0 {
		t.Fatalf("a check without apps: %+v", got)
	}
}

// Codex's /hooks step shows for a first setup, and on a reconfiguration
// when Codex is newly included or its hooks move to a file it has not
// approved.
func TestReviewChecklistShowsCodexStepOnlyWhenNew(t *testing.T) {
	t.Parallel()
	cfg := config.Config{Harnesses: []string{"codex", "claude"}}
	step := func(review setupReview) bool {
		for _, check := range reviewChecklist(cfg, review, time.Now()) {
			if check.label == "Codex needs one step" {
				return check.mark == symbolWarn && setupContainsText(check.detail, "/hooks")
			}
		}
		return false
	}
	installed := config.Config{Harnesses: []string{"codex"}}
	here := hooks.Files{"codex": "/u/.codex/hooks.json"}
	moved := hooks.Files{"codex": "/u/alt/hooks.json"}
	if !step(setupReview{}) || !step(setupReview{existing: installed}) ||
		step(setupReview{existing: installed, reconfiguring: true, hookFiles: here, installedHookFiles: here}) ||
		!step(setupReview{existing: installed, reconfiguring: true, hookFiles: moved, installedHookFiles: here}) ||
		!step(setupReview{existing: config.Config{Harnesses: []string{"claude"}}, reconfiguring: true}) {
		t.Fatal("Codex step shown wrongly")
	}
}

// A hook file that breaks while setup asks its questions shows ✗ at the
// review, which then neither offers nor accepts starting. Once the file is
// fixed, Check again clears the ✗ and setup can start.
func TestSetupReviewBlocksStartOnHookFileBrokenAfterPreflight(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now().UTC())
	hooksFile := filepath.Join(userHome, ".codex", "hooks.json")
	must(t, os.MkdirAll(filepath.Dir(hooksFile), 0o700))
	env.IsTerminal = func(stream any) bool { _, ok := stream.(*hookFixingAnswers); return ok }
	broken := false
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		// The storage check comes after the checks before the first
		// question, so this is the file breaking in between.
		if !broken {
			broken = true
			must(t, os.WriteFile(hooksFile, []byte("{\n  // a comment\n}\n"), 0o600))
		}
		return storagetest.NewMemoryStore(), nil
	}
	answers := []string{"y", "n", "n", "included-projects", project, "", "s3-existing", "profile", "bucket", "us-east-1",
		"y",     // refused: starting is not a choice
		"check", // the file is fixed just before this answer
		"y"}
	in := &hookFixingAnswers{answers: answers, fixAt: 11, fix: func() {
		if _, found, err := config.Load(home); err != nil || found {
			t.Fatalf("setup committed before the invalid hook file was fixed: found=%v err=%v", found, err)
		}
		must(t, os.WriteFile(hooksFile, []byte("{}\n"), 0o600))
	}}
	var out bytes.Buffer
	if code := Run([]string{"setup"}, in, &out, &out, env); code != 0 {
		t.Fatalf("setup exit %d\n%s", code, &out)
	}
	output := out.String()
	blocked := strings.Index(output, "✗ Codex hook file is invalid")
	refused := strings.Index(output, "Enter one of the available choices.")
	if blocked < 0 || refused < blocked || !setupContainsText(output, "Fix the blocking checks first.") {
		t.Fatalf("✗ did not block starting:\n%s", output)
	}
	checked := setupReceiptIndex(output, "Check again")
	if checked < 0 {
		t.Fatalf("missing check receipt: %s", output)
	}
	if setupContainsText(output[checked:], "✗ Codex hook file is invalid") || !setupContainsText(output[refused:], "1) Start archiving (default)") {
		t.Fatalf("check again did not clear the ✗:\n%s", output)
	}
	if _, found, err := config.Load(home); err != nil || !found {
		t.Fatalf("setup did not start after the fix: %v", err)
	}
}

// hookFixingAnswers hands setup one answer per read, running fix just
// before the answer at index fixAt.
type hookFixingAnswers struct {
	answers []string
	next    int
	fixAt   int
	fix     func()
}

func (a *hookFixingAnswers) Read(p []byte) (int, error) {
	if a.next == len(a.answers) {
		return 0, io.EOF
	}
	if a.next == a.fixAt {
		a.fix()
	}
	line := a.answers[a.next] + "\n"
	a.next++
	return copy(p, line), nil
}

// Details renders through a buffer, which must retain the invoking terminal's
// dimensions and show every selected app's native capture roots.
// Regression: 2026-10 setup review P2-R1-03/P2-R1-04.
func TestSetupDetailsRetainsNarrowLayoutAndAllAppSourceRoots(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.withApps(t, "claude", "cursor")
	f.inWebApp(t)
	out := &promptScreen{caps: promptCapabilities{Width: 36, Height: 20}}
	input := strings.NewReader("\n\ns3-existing\nwork\n2\ndetails\nq\n")
	if code := Run([]string{"setup"}, input, out, out, f.env); code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	text := f.normalize(out.String())
	detailsAt := strings.Index(text, "\nFull settings and privacy\n")
	if detailsAt < 0 {
		t.Fatalf("missing Details heading: %s", text)
	}
	details := text[detailsAt+1:]
	if !strings.Contains(details, "  Apps\n    Claude Code") || !setupContainsText(details, "Claude Code: ~/.claude") || !setupContainsText(details, "Cursor: ~/.cursor") {
		t.Fatalf("details lost width or source roots:\n%s", details)
	}
}

// Removing the final imported-only app is a meaningful publishing change, so
// both review views must retain its old/new row before Save.
// Regression: 2026-10 setup review P2-R1-05.
func TestSetupReviewShowsRemovedImportedApps(t *testing.T) {
	t.Parallel()
	old := config.Config{Harnesses: []string{"claude"}, ImportedHarnesses: []string{"cursor"}, RetentionDays: 90}
	next := old
	next.ImportedHarnesses = nil
	model := buildSetupReviewModel(next, setupReview{existing: old, reconfiguring: true}, time.Now())
	for _, details := range []bool{false, true} {
		var out bytes.Buffer
		p := newPrompter(strings.NewReader(""), &out)
		renderSetupReview(p, model, details)
		p.close()
		if !setupContainsText(out.String(), "* Imported none") || !setupContainsText(out.String(), "was Cursor (sessions imported") {
			t.Fatalf("removed publishing scope missing:\n%s", &out)
		}
	}
}

// A receiver reviews consent before credentials are staged or storage is probed.
// Regression: 2026-10 setup review P2-R1-08.
func TestSetupReviewBeforeProbeDoesNotClaimStorageConnected(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out)
	defer p.close()
	showSetupReview(p, config.Config{}, setupReview{storageUnchecked: true})
	if strings.Contains(out.String(), "✓ Storage connected") || !setupContainsText(out.String(), "Storage not checked yet · Connection will be checked after you confirm these settings") {
		t.Fatal(out.String())
	}
}

// Configuration establishes hook capture, but cannot establish app approval.
// Regression: 2026-10 setup review P2-R1-09.
func TestSetupHookOnlyCompletionDoesNotClaimAutomaticCaptureOn(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.withApps(t, "codex")
	f.inWebApp(t)
	input := strings.NewReader("\nall-projects\ndone\ns3-existing\nwork\n2\nedit\ndiscovery\nno\nstart\ndone\n")
	var out bytes.Buffer
	if code := Run([]string{"setup"}, input, &out, &out, f.env); code != 0 {
		t.Fatalf("exit %d\n%s", code, &out)
	}
	if strings.Contains(out.String(), "Automatic capture is on") || !setupContainsText(out.String(), "Setup complete · Codex hook capture is configured") || !strings.Contains(out.String(), "/hooks") {
		t.Fatal(out.String())
	}
}
