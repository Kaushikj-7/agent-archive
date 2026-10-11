package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentskills"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

// setupRunWith is setupRun for setup given args (flags), answered by input.
func setupRunWith(t *testing.T, env Env, args []string, input string, want int) (stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Run(append([]string{"setup"}, args...), strings.NewReader(input), &out, &errOut, env); code != want {
		t.Fatalf("setup %v: exit %d want %d\n%s\n%s", args, code, want, &out, &errOut)
	}
	return out.String(), errOut.String()
}

func mustLoadConfig(t *testing.T, home string) config.Config {
	t.Helper()
	cfg, found, err := config.Load(home)
	if err != nil || !found {
		t.Fatalf("config: %v %v", found, err)
	}
	return cfg
}

// wantSkillFiles fails unless the skill file at each of installed is there
// and each of absent is not.
func wantSkillFiles(t *testing.T, installed, absent []string) {
	t.Helper()
	for _, path := range installed {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("skill file missing: %v", err)
		}
	}
	for _, path := range absent {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("skill file %s is there (%v)", path, err)
		}
	}
}

const turnedOffLine = "Agent skills are turned off. To install them, run agent-archive setup --skills.\n"

// Setup says once that it installed the skills and how to opt out.
func TestSetupNamesTheOptOutWhenItInstallsSkills(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	output := setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, project), 0)
	if !strings.Contains(output, "Installed /handoff, which continues a session in another agent: ~/.claude/skills/handoff/SKILL.md, ~/.agents/skills/handoff/SKILL.md\nInstalled the agent-archive skill, which lets your agents look up and pull in past sessions: ~/.claude/skills/agent-archive/SKILL.md, ~/.agents/skills/agent-archive/SKILL.md\nTo remove the agent skills and keep them off, run agent-archive setup --no-skills.\n") {
		t.Fatalf("setup did not name the opt-out after the installed skills:\n%s", output)
	}
	if strings.Contains(output, "turned off") {
		t.Fatalf("setup says the skills are off:\n%s", output)
	}
	if cfg := mustLoadConfig(t, home); cfg.NoSkills {
		t.Fatal("a plain setup recorded the opt-out")
	}
	if _, ok := statusJSON(t, env)["agent_skills_disabled"]; ok {
		t.Fatal("status says skills are disabled")
	}
}

// A fresh setup with --no-skills installs none of them, on a terminal and
// with --yes, and records it.
func TestFreshSetupWithNoSkillsInstallsNone(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"prompts", "yes"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			var output string
			if mode == "yes" {
				output = setupYes(t, env, "", 0, "--yes", "--no-skills", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--project", project, "--apps", "codex,claude", "--codex-discovery", "on", "--codex-capture-scope", "included-projects")
			} else {
				out, _ := setupRunWith(t, env, []string{"--no-skills"}, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, project), 0)
				output = out
			}
			wantSkillFiles(t, nil, []string{claudeSkillPath(userHome), agentsSkillPath(userHome)})
			for _, dir := range []string{filepath.Join(userHome, ".claude", "skills"), filepath.Join(userHome, ".agents")} {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Errorf("setup created %s", dir)
				}
			}
			if !strings.Contains(output, turnedOffLine) || strings.Contains(output, "Installed /handoff") || strings.Contains(output, "Removed ") {
				t.Fatalf("setup output:\n%s", output)
			}
			if !mustLoadConfig(t, home).NoSkills {
				t.Fatal("the opt-out was not recorded")
			}
			view := statusJSON(t, env)
			if view["agent_skills_disabled"] != true {
				t.Fatalf("status agent_skills_disabled = %#v", view["agent_skills_disabled"])
			}
			if _, ok := view["agent_skills"]; ok {
				t.Fatal("status lists skills")
			}
		})
	}
}

// Turning the skills off after they were installed removes the files setup
// wrote and says which; another file at those paths is left alone and named.
func TestNoSkillsRemovesOwnedSkillFilesAndReportsForeignOnes(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"prompts", "yes"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
			// The shared file is the person's own now (no marker line).
			must(t, os.WriteFile(agentsSkillPath(userHome), []byte("my own skill\n"), 0600))
			var output string
			if mode == "yes" {
				output = setupYes(t, env, "", 0, "--yes", "--no-skills")
			} else {
				output, _ = setupRunWith(t, env, []string{"--no-skills"}, "retention\ncustom\n120\ny\n", 0)
			}
			wantSkillFiles(t, []string{agentsSkillPath(userHome)}, []string{claudeSkillPath(userHome)})
			if got := readText(t, agentsSkillPath(userHome)); got != "my own skill\n" {
				t.Fatalf("the person's skill changed: %q", got)
			}
			if _, err := os.Stat(filepath.Join(userHome, ".claude", "skills")); !os.IsNotExist(err) {
				t.Errorf("the emptied skills directory stayed: %v", err)
			}
			for _, want := range []string{
				"Removed /handoff: ~/.claude/skills/handoff/SKILL.md\n",
				"Left ~/.agents/skills/handoff/SKILL.md as it is: it is not this agent-archive installation's (it lacks the marker line, or names another data directory), so setup does not remove it.\n",
				turnedOffLine,
			} {
				if !strings.Contains(output, want) {
					t.Errorf("setup output lacks %q:\n%s", want, output)
				}
			}
			if !mustLoadConfig(t, home).NoSkills {
				t.Fatal("the opt-out was not recorded")
			}
			view := statusJSON(t, env)
			if view["agent_skills_disabled"] != true {
				t.Fatalf("status agent_skills_disabled = %#v", view["agent_skills_disabled"])
			}
			if _, ok := view["agent_skills"]; ok {
				t.Fatalf("status lists a file of the person's own: %#v", view["agent_skills"])
			}
		})
	}
}

// The opt-out is sticky: a plain setup afterwards, with or without questions,
// installs nothing and does not clear it; --skills does, and installs.
func TestNoSkillsSticksUntilSkillsTurnsThemOn(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	setupYes(t, env, "", 0, "--yes", "--no-skills")
	wantSkillFiles(t, nil, []string{claudeSkillPath(userHome), agentsSkillPath(userHome)})

	output := setupYes(t, env, "", 0, "--yes")
	if !strings.Contains(output, turnedOffLine) {
		t.Fatalf("a plain setup --yes does not say the skills are off:\n%s", output)
	}
	wantSkillFiles(t, nil, []string{claudeSkillPath(userHome), agentsSkillPath(userHome)})
	if !mustLoadConfig(t, home).NoSkills {
		t.Fatal("a plain setup --yes cleared the opt-out")
	}
	output, _ = setupRunWith(t, env, nil, "retention\ncustom\n120\ny\n", 0)
	if !strings.Contains(output, turnedOffLine) {
		t.Fatalf("a plain setup does not say the skills are off:\n%s", output)
	}
	wantSkillFiles(t, nil, []string{claudeSkillPath(userHome), agentsSkillPath(userHome)})
	if cfg := mustLoadConfig(t, home); !cfg.NoSkills || cfg.RetentionDays != 120 {
		t.Fatalf("config after a plain setup: NoSkills=%v retention=%d", cfg.NoSkills, cfg.RetentionDays)
	}

	output = setupYes(t, env, "", 0, "--yes", "--skills")
	if !strings.Contains(output, "Installed /handoff") || strings.Contains(output, "turned off") {
		t.Fatalf("--skills output:\n%s", output)
	}
	wantSkillFiles(t, []string{claudeSkillPath(userHome), agentsSkillPath(userHome)}, nil)
	if mustLoadConfig(t, home).NoSkills {
		t.Fatal("--skills left the opt-out recorded")
	}
	if _, ok := statusJSON(t, env)["agent_skills_disabled"]; ok {
		t.Fatal("status still says the skills are disabled")
	}
	// Plain setup now keeps them.
	setupYes(t, env, "", 0, "--yes")
	wantSkillFiles(t, []string{claudeSkillPath(userHome), agentsSkillPath(userHome)}, nil)
}

// --skills is also accepted by a prompt-driven setup.
func TestSkillsFlagTurnsThemOnInPromptDrivenSetup(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", false, true, false, t.TempDir()))
	setupYes(t, env, "", 0, "--yes", "--no-skills")
	output, _ := setupRunWith(t, env, []string{"--skills"}, "retention\ncustom\n120\ny\n", 0)
	if !strings.Contains(output, "Installed /handoff") {
		t.Fatalf("setup --skills output:\n%s", output)
	}
	wantSkillFiles(t, []string{claudeSkillPath(userHome)}, nil)
	if mustLoadConfig(t, home).NoSkills {
		t.Fatal("--skills left the opt-out recorded")
	}
}

// Both flags together are a usage error before anything is read or changed.
func TestNoSkillsAndSkillsTogetherAreAUsageError(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--no-skills", "--skills"}, {"--yes", "--skills", "--no-skills"}} {
		home, userHome := t.TempDir(), t.TempDir()
		env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
		out, errOut := setupRunWith(t, env, args, "", 2)
		if out != "" || strings.Count(errOut, "\n") != 1 || !strings.Contains(errOut, "--no-skills and --skills contradict each other") {
			t.Errorf("setup %v: stdout %q stderr %q", args, out, errOut)
		}
		if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
			t.Errorf("setup %v touched the data directory: %v %v", args, entries, err)
		}
	}
}

// A failed setup puts the skills it removed back, and the opt-out is not
// recorded.
func TestFailedSetupTakesBackTheRemovalOfSkills(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	before := map[string]string{claudeSkillPath(userHome): readText(t, claudeSkillPath(userHome)), agentsSkillPath(userHome): readText(t, agentsSkillPath(userHome))}
	fakeSched(env).beforeLoad = func(scheduler.Ref) error { return errors.New("bootstrap failed") }
	setupRunWith(t, env, []string{"--no-skills"}, "retention\ncustom\n120\ny\n", 1)
	for path, want := range before {
		if got := readText(t, path); got != want {
			t.Errorf("failed setup did not restore %s:\n%s", path, got)
		}
	}
	if mustLoadConfig(t, home).NoSkills {
		t.Fatal("a failed setup recorded the opt-out")
	}
}

// A skill file of another installation (another data directory) sharing the
// HOME is never removed, and setup reports it.
func TestNoSkillsLeavesAnotherInstallationsSkill(t *testing.T) {
	t.Parallel()
	primary, secondary, userHome := twoInstallations(t)
	setupRun(t, primary, s3SetupInput("b", "us-east-1", "p", true, false, false, t.TempDir()), 0)
	first := readText(t, agentsSkillPath(userHome))
	claudeDir := filepath.Join(userHome, "b", "claude")
	vars := map[string]string{"CLAUDE_CONFIG_DIR": claudeDir, "CODEX_HOME": filepath.Join(userHome, "b", "codex")}
	secondary.LookupEnv = func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	setupRun(t, secondary, s3SetupInput("b", "us-east-1", "p", true, true, false, t.TempDir()), 0)
	own := filepath.Join(claudeDir, "skills", "handoff", "SKILL.md")
	wantSkillFiles(t, []string{own}, nil)

	output := setupYes(t, secondary, "", 0, "--yes", "--no-skills")
	wantSkillFiles(t, nil, []string{own})
	if readText(t, agentsSkillPath(userHome)) != first {
		t.Fatal("the second installation removed or changed the first one's skill")
	}
	for _, want := range []string{"Removed /handoff: ", "Left ~/.agents/skills/handoff/SKILL.md as it is", turnedOffLine} {
		if !strings.Contains(output, want) {
			t.Errorf("setup output lacks %q:\n%s", want, output)
		}
	}
	// The first installation's setting is its own.
	if _, ok := statusJSON(t, primary)["agent_skills_disabled"]; ok {
		t.Fatal("the first installation's skills are turned off")
	}
}

// With a flag and nothing to change (the person leaves the menu), setup says
// the skills were not changed rather than silently ignoring the flag.
func TestNoSkillsFlagWhenSetupMakesNoChange(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	output, _ := setupRunWith(t, env, []string{"--no-skills"}, "exit\n", 0)
	if !strings.Contains(output, "The agent skills were not changed: setup made no change this run. To change only the skills, run agent-archive setup --yes --no-skills.\n") {
		t.Fatalf("output:\n%s", output)
	}
	wantSkillFiles(t, []string{claudeSkillPath(userHome), agentsSkillPath(userHome)}, nil)
	if mustLoadConfig(t, home).NoSkills {
		t.Fatal("the opt-out was recorded without a setup")
	}
}

// A saved draft never carries the opt-out: this run's flag and the committed
// setting decide.
func TestSavedDraftDoesNotKeepTheOptOut(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	setupRunWith(t, env, []string{"--no-skills"}, "retention\ncustom\n120\nno\n", 0)
	wantSkillFiles(t, []string{claudeSkillPath(userHome)}, nil)
	output, _ := setupRunWith(t, env, nil, "continue\nyes\n", 0)
	if !strings.Contains(output, "Installed /handoff") {
		t.Fatalf("output:\n%s", output)
	}
	if cfg := mustLoadConfig(t, home); cfg.NoSkills || cfg.RetentionDays != 120 {
		t.Fatalf("config: NoSkills=%v retention=%d", cfg.NoSkills, cfg.RetentionDays)
	}
	wantSkillFiles(t, []string{claudeSkillPath(userHome), agentsSkillPath(userHome)}, nil)
}

func TestSkillsChoice(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		opts  setupOptions
		saved bool
		want  bool
	}{
		{setupOptions{}, false, false},
		{setupOptions{}, true, true},
		{setupOptions{noSkills: true}, false, true},
		{setupOptions{noSkills: true}, true, true},
		{setupOptions{skills: true}, true, false},
		{setupOptions{skills: true}, false, false},
	} {
		if got := tc.opts.skillsChoice().noSkills(tc.saved); got != tc.want {
			t.Errorf("%+v with saved %v: NoSkills = %v, want %v", tc.opts, tc.saved, got, tc.want)
		}
	}
}

// Status says the skills are off in its text, only when they are, and pins
// the JSON key.
func TestStatusSaysSkillsAreTurnedOff(t *testing.T) {
	t.Parallel()
	_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	statusText := func(args ...string) string {
		var out, errOut bytes.Buffer
		if code := Run(append([]string{"status"}, args...), nil, &out, &errOut, env); code != 0 {
			t.Fatal(errOut.String())
		}
		return out.String()
	}
	if strings.Contains(statusText(), "Agent skills") {
		t.Fatal("status mentions agent skills while they are on")
	}
	setupYes(t, env, "", 0, "--yes", "--no-skills")
	// The short status keeps to problems; the setting is with the others
	// (projects, skill evidence) under --verbose, and in its details.
	if got := statusText(); strings.Contains(got, "Agent skills") {
		t.Errorf("the short status mentions a chosen setting:\n%s", got)
	}
	got := statusText("--verbose")
	for _, want := range []string{"Agent skills: turned off; agent-archive setup --skills turns them on", "Agent skills:  turned off"} {
		if !strings.Contains(got, want) {
			t.Errorf("status --verbose lacks %q:\n%s", want, got)
		}
	}
}

func TestAgentSkillsDisabledSpelling(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(statusView{AgentSkillsDisabled: true})
	must(t, err)
	if !strings.Contains(string(data), `"agent_skills_disabled":true`) {
		t.Errorf("status lacks agent_skills_disabled: %s", data)
	}
	data, err = json.Marshal(statusView{})
	must(t, err)
	if strings.Contains(string(data), "agent_skills_disabled") {
		t.Errorf("status has agent_skills_disabled while skills are on: %s", data)
	}
}

// The help lists both flags, and the CLI reference is checked separately.
func TestSetupHelpNamesBothSkillFlags(t *testing.T) {
	t.Parallel()
	help := commandHelp["setup"]
	for _, want := range []string{"[--no-skills | --skills]", "\n  --no-skills ", "\n  --skills "} {
		if !strings.Contains(help, want) {
			t.Errorf("setup help lacks %q", want)
		}
	}
}

// Status never calls a file "out of date" while the skills are off: setup
// removes it rather than refreshing it, so status says it is left over, and
// a plain setup removes it.
func TestStatusWarnsOfALeftoverSkillFileWhileSkillsAreOff(t *testing.T) {
	t.Parallel()
	_, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	claude := claudeSkillPath(userHome)
	current := readText(t, claude)
	older := strings.Replace(current, "Run exactly this command", "Run this command", 1)
	if older == current {
		t.Fatal("the skill has no wording to age")
	}
	setupYes(t, env, "", 0, "--yes", "--no-skills")
	// A restored backup puts an earlier release's file back.
	must(t, os.MkdirAll(filepath.Dir(claude), 0700))
	must(t, os.WriteFile(claude, []byte(older), 0600))
	view := statusJSON(t, env)
	if _, ok := view["agent_skills_out_of_date"]; ok {
		t.Fatalf("status calls a file out of date while the skills are off: %#v", view["agent_skills_out_of_date"])
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"status"}, nil, &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
	if strings.Contains(out.String(), "out of date") || !strings.Contains(out.String(), "The agent skills are turned off, but the /handoff skill file at ~/.claude/skills/handoff/SKILL.md is still there. Run agent-archive setup to remove it.") {
		t.Fatalf("status:\n%s", &out)
	}
	if got := readText(t, claude); got != older {
		t.Fatal("status changed the file")
	}
	setupYes(t, env, "", 0, "--yes")
	wantSkillFiles(t, nil, []string{claude})
	out.Reset()
	if code := Run([]string{"status"}, nil, &out, &errOut, env); code != 0 || strings.Contains(out.String(), "still there") {
		t.Fatalf("status after setup (exit %d):\n%s", code, &out)
	}
}

// An opt-out interrupted part way is recovered like any setup: the journal
// puts the skill file and the configuration back, so the opt-out was never
// made; abandoning recovery keeps the files as they are, and setup then
// converges on what the saved configuration says.
func TestInterruptedNoSkillsSetupRecoversAndConverges(t *testing.T) {
	t.Parallel()
	interrupted := func(t *testing.T) (home, userHome string, env Env, journal setupjournal.Journal) {
		t.Helper()
		home, userHome, env = installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
		old := mustLoadConfig(t, home)
		next := old
		next.NoSkills = true
		mergeCommittedSetupState(old, &next, nil)
		must(t, prepareSetupConfig(home, old.InstalledExecutable, old, &next, env))
		var err error
		journal, err = planSetupTransaction(home, userHome, old.InstalledExecutable, old, &next, env)
		must(t, err)
		removals := 0
		for _, c := range journal.Changes {
			if c.Delete {
				removals++
			}
		}
		if removals != 2*len(agentskills.Registry) {
			t.Fatalf("the opt-out plans %d removals, want %d: %+v", removals, 2*len(agentskills.Registry), journal.Changes)
		}
		must(t, local.Write(setupjournal.JournalPath(home), journal))
		// The crash came after the Claude Code file was removed.
		must(t, os.Remove(claudeSkillPath(userHome)))
		return home, userHome, env, journal
	}
	t.Run("recovery", func(t *testing.T) {
		t.Parallel()
		home, userHome, env, _ := interrupted(t)
		setupYes(t, env, "", 0, "--yes")
		wantSkillFiles(t, []string{claudeSkillPath(userHome), agentsSkillPath(userHome)}, nil)
		if mustLoadConfig(t, home).NoSkills {
			t.Fatal("a recovered setup recorded the opt-out")
		}
		if setupjournal.TransactionPending(home) {
			t.Fatal("the journal is still there")
		}
	})
	t.Run("abandoned before the configuration was written", func(t *testing.T) {
		t.Parallel()
		home, userHome, env, _ := interrupted(t)
		out, _ := setupRunWith(t, env, []string{"--abandon-recovery"}, "", 0)
		if !strings.Contains(out, "Discarded") {
			t.Fatalf("abandon:\n%s", out)
		}
		wantSkillFiles(t, []string{agentsSkillPath(userHome)}, []string{claudeSkillPath(userHome)})
		if mustLoadConfig(t, home).NoSkills {
			t.Fatal("abandoning recovery recorded the opt-out")
		}
		setupYes(t, env, "", 0, "--yes")
		wantSkillFiles(t, []string{claudeSkillPath(userHome), agentsSkillPath(userHome)}, nil)
	})
	t.Run("abandoned after the configuration was written", func(t *testing.T) {
		t.Parallel()
		home, userHome, env, journal := interrupted(t)
		for _, c := range journal.Changes {
			if filepath.Base(c.Path) == "config.json" {
				must(t, os.WriteFile(c.Path, c.After, 0600))
			}
		}
		if !mustLoadConfig(t, home).NoSkills {
			t.Fatal("the fixture did not write the opt-out")
		}
		setupRunWith(t, env, []string{"--abandon-recovery"}, "", 0)
		output := setupYes(t, env, "", 0, "--yes")
		wantSkillFiles(t, nil, []string{claudeSkillPath(userHome), agentsSkillPath(userHome)})
		if !strings.Contains(output, turnedOffLine) || !mustLoadConfig(t, home).NoSkills {
			t.Fatalf("setup after abandoning:\n%s", output)
		}
	})
}

// A skill directory that is a link (the person's own skill, linked in) is
// never removed through, and setup names it.
func TestNoSkillsLeavesALinkedSkillDirectory(t *testing.T) {
	t.Parallel()
	_, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	own := filepath.Join(t.TempDir(), "handoff")
	must(t, os.MkdirAll(own, 0700))
	// The link's target holds a file that carries setup's marker line.
	marked := readText(t, claudeSkillPath(userHome))
	must(t, os.WriteFile(filepath.Join(own, "SKILL.md"), []byte(marked), 0600))
	must(t, os.RemoveAll(filepath.Dir(claudeSkillPath(userHome))))
	must(t, os.Symlink(own, filepath.Dir(claudeSkillPath(userHome))))
	output := setupYes(t, env, "", 0, "--yes", "--no-skills")
	if got := readText(t, filepath.Join(own, "SKILL.md")); got != marked {
		t.Fatalf("setup changed a file behind a linked skill directory: %q", got)
	}
	if info, err := os.Lstat(filepath.Dir(claudeSkillPath(userHome))); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("setup removed the link: %v", err)
	}
	if !strings.Contains(output, "Left ~/.claude/skills/handoff/SKILL.md as it is") {
		t.Fatalf("setup does not name the linked skill:\n%s", output)
	}
	wantSkillFiles(t, nil, []string{agentsSkillPath(userHome)})
}

// planAgentSkills, which a refresh will call with the saved configuration,
// decides by the configuration alone: with NoSkills it plans only removals
// (never a write, whatever the executable), without it, installs.
func TestPlanAgentSkillsHonorsNoSkillsWithoutAsking(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	cfg := mustLoadConfig(t, home)
	claudeDir := filepath.Join(userHome, ".claude")
	dataHome := env.installation(home, userHome).commandDataHome()
	changes, kept, err := planAgentSkills(userHome, claudeDir, claudeDir, cfg, "/moved/agent-archive", dataHome)
	must(t, err)
	if len(changes) != 2*len(agentskills.Registry) || len(kept) != 0 {
		t.Fatalf("with skills on and a moved executable: changes %+v kept %v", changes, kept)
	}
	for _, c := range changes {
		if c.Delete {
			t.Fatalf("with skills on, a change deletes %s", c.Path)
		}
	}
	cfg.NoSkills = true
	must(t, os.WriteFile(agentsSkillPath(userHome), []byte("mine\n"), 0600))
	changes, kept, err = planAgentSkills(userHome, claudeDir, claudeDir, cfg, "/moved/agent-archive", dataHome)
	must(t, err)
	// Every file of setup's goes, whichever skill it is; the person's own stays.
	var removed []string
	for _, c := range changes {
		if !c.Delete {
			t.Fatalf("with NoSkills, a change writes %s", c.Path)
		}
		removed = append(removed, c.Path)
	}
	if want := append([]string{claudeSkillPath(userHome)}, archiveSkillPaths(userHome)...); !slices.Equal(removed, want) {
		t.Fatalf("with NoSkills: removes %v, want %v", removed, want)
	}
	if len(kept) != 1 || kept[0] != agentsSkillPath(userHome) {
		t.Fatalf("with NoSkills: kept %v", kept)
	}
	for _, path := range removed {
		must(t, os.Remove(path))
	}
	if changes, _, err = planAgentSkills(userHome, claudeDir, claudeDir, cfg, "/moved/agent-archive", dataHome); err != nil || len(changes) != 0 {
		t.Fatalf("with NoSkills and no owned file: %+v %v", changes, err)
	}
}
