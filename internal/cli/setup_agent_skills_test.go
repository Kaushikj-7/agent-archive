package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentskills"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/scheduler"
)

func claudeSkillPath(userHome string) string {
	return filepath.Join(userHome, ".claude", "skills", "handoff", "SKILL.md")
}

func agentsSkillPath(userHome string) string {
	return filepath.Join(userHome, ".agents", "skills", "handoff", "SKILL.md")
}

// archiveSkillPaths are the agent-archive skill's files (Claude Code's, then
// the shared one), which setup installs beside /handoff's.
func archiveSkillPaths(userHome string) []string {
	return []string{
		filepath.Join(userHome, ".claude", "skills", "agent-archive", "SKILL.md"),
		filepath.Join(userHome, ".agents", "skills", "agent-archive", "SKILL.md"),
	}
}

func uninstallRun(t *testing.T, env Env) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, env); code != 0 {
		t.Fatalf("uninstall exit %d\n%s\n%s", code, &out, &errOut)
	}
	return out.String()
}

func TestSetupInstallsTheHandoffSkillUnderTheSandboxedHome(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	output := setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, project), 0)
	cfg, _, err := config.Load(home)
	must(t, err)
	files := agentskills.Files(productionAgents, userHome, filepath.Join(userHome, ".claude"), []string{"codex", "claude"}, cfg.InstalledExecutable, env.installation(home, userHome).commandDataHome())
	if len(files) != 2*len(agentskills.Registry) {
		t.Fatalf("files = %+v", files)
	}
	for _, f := range files {
		data, err := os.ReadFile(f.Path)
		if err != nil || !bytes.Equal(data, f.Content) {
			t.Fatalf("%s not installed: %v", f.Path, err)
		}
	}
	if !strings.Contains(output, "Installed /handoff, which continues a session in another agent: ~/.claude/skills/handoff/SKILL.md, ~/.agents/skills/handoff/SKILL.md\n") {
		t.Fatalf("setup did not say where /handoff went:\n%s", output)
	}
	if !strings.Contains(output, "Installed the agent-archive skill, which lets your agents look up and pull in past sessions: ~/.claude/skills/agent-archive/SKILL.md, ~/.agents/skills/agent-archive/SKILL.md\n") {
		t.Fatalf("setup did not say where the agent-archive skill went:\n%s", output)
	}

	skills := statusJSON(t, env)["agent_skills"]
	if !reflect.DeepEqual(skills, []any{claudeSkillPath(userHome), agentsSkillPath(userHome), archiveSkillPaths(userHome)[0], archiveSkillPaths(userHome)[1]}) {
		t.Fatalf("status agent_skills = %#v", skills)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"status", "--verbose"}, nil, &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
	if !strings.Contains(out.String(), "  /handoff:      ~/.claude/skills/handoff/SKILL.md\n") {
		t.Fatalf("status --verbose lacks the skill:\n%s", &out)
	}
	if !strings.Contains(out.String(), "  agent-archive: ~/.claude/skills/agent-archive/SKILL.md\n") {
		t.Fatalf("status --verbose lacks the agent-archive skill:\n%s", &out)
	}

	uninstallRun(t, env)
	for _, dir := range []string{filepath.Join(userHome, ".claude", "skills"), filepath.Join(userHome, ".agents")} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("uninstall left %s", dir)
		}
	}
}

func TestSetupLeavesAHandoffSkillItDidNotWrite(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	path := agentsSkillPath(userHome)
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	must(t, os.WriteFile(path, []byte("my own skill\n"), 0600))
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	output := setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, true, project), 0)
	if !strings.Contains(output, "Left ~/.agents/skills/handoff/SKILL.md as it is: it is not this agent-archive installation's (it lacks the marker line, or names another data directory), so /handoff is not installed there.\n") {
		t.Fatalf("setup did not report the file it left:\n%s", output)
	}
	if strings.Contains(output, "Installed /handoff") {
		t.Fatalf("setup claims an installation:\n%s", output)
	}
	if listed := statusJSON(t, env)["agent_skills"]; slices.Contains(anyStrings(listed), path) {
		t.Fatal("status lists a file setup did not write")
	}
	if output := uninstallRun(t, env); !strings.Contains(output, "Kept ~/.agents/skills/handoff/SKILL.md: it is not this agent-archive installation's (it lacks the marker line, or names another data directory).\n") {
		t.Fatalf("uninstall did not report the file it kept:\n%s", output)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "my own skill\n" {
		t.Fatalf("the person's skill changed: %q %v", data, err)
	}
}

// A skill whose marker line the person deleted is theirs: setup and
// uninstall leave it, and status no longer lists it.
func TestSetupAndUninstallKeepAHandoffSkillMadeOwn(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", false, true, false, t.TempDir()))
	path := claudeSkillPath(userHome)
	must(t, os.WriteFile(path, []byte("tuned by hand\n"), 0600))
	if listed := statusJSON(t, env)["agent_skills"]; slices.Contains(anyStrings(listed), path) {
		t.Fatal("status lists a skill that is no longer setup's")
	}
	old, _, err := config.Load(home)
	must(t, err)
	next := old
	must(t, applySetup(home, userHome, old.InstalledExecutable, old, &next, nil, env))
	output := uninstallRun(t, env)
	if !strings.Contains(output, "Kept ~/.claude/skills/handoff/SKILL.md: it is not this agent-archive installation's (it lacks the marker line, or names another data directory).\n") {
		t.Fatalf("uninstall did not report the kept file:\n%s", output)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "tuned by hand\n" {
		t.Fatalf("edited skill changed: %q %v", data, err)
	}
}

// Setup run again writes nothing new while the files are as it left them,
// and removes the shared skill once no app that reads it is set up.
func TestSetupAgainUpdatesOnlyItsOwnHandoffSkills(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	old, _, err := config.Load(home)
	must(t, err)
	exe, err := env.executable()
	must(t, err)
	before, err := os.Stat(claudeSkillPath(userHome))
	must(t, err)

	next := old
	must(t, applySetup(home, userHome, exe, old, &next, nil, env))
	again, _, err := config.Load(home)
	must(t, err)
	if after, err := os.Stat(claudeSkillPath(userHome)); err != nil || !os.SameFile(before, after) {
		t.Fatal("an unchanged skill was rewritten")
	}

	next = again
	next.Harnesses = []string{"claude"}
	must(t, applySetup(home, userHome, exe, again, &next, nil, env))
	if _, err := os.Stat(filepath.Join(userHome, ".agents")); !os.IsNotExist(err) {
		t.Fatal("the shared skill, or directories setup created for it, stayed after Codex was removed")
	}
	if _, err := os.Stat(claudeSkillPath(userHome)); err != nil {
		t.Fatal("Claude Code's skill removed")
	}
}

func TestFailedSetupTakesBackTheHandoffSkill(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	fakeSched(env).beforeLoad = func(scheduler.Ref) error { return errors.New("bootstrap failed") }
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, project), 1)
	for _, dir := range []string{filepath.Join(userHome, ".claude", "skills"), filepath.Join(userHome, ".agents")} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("failed setup left %s", dir)
		}
	}
}

// TestAgentSkillsSpelling pins the status --json keys.
func TestAgentSkillsSpelling(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(statusView{AgentSkills: []string{"/p"}, AgentSkillsOutOfDate: []string{"/q"}})
	must(t, err)
	for _, want := range []string{`"agent_skills":["/p"]`, `"agent_skills_out_of_date":["/q"]`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("status lacks %s: %s", want, data)
		}
	}
	if strings.Contains(string(data), "agent_commands") {
		t.Errorf("status has the unreleased agent_commands key: %s", data)
	}
}

// Status says which skill files an upgrade has outdated, in its JSON, its
// warnings, and beside the file in --verbose; setup refreshes them, and
// then status does not.
func TestStatusReportsAnOutdatedSkillUntilSetupRefreshesIt(t *testing.T) {
	t.Parallel()
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, t.TempDir()))
	if _, ok := statusJSON(t, env)["agent_skills_out_of_date"]; ok {
		t.Fatal("status calls a fresh installation's skills out of date")
	}
	claude, agents := claudeSkillPath(userHome), agentsSkillPath(userHome)
	// The Claude Code file is as an earlier release worded it; the shared
	// one is the person's own (no marker line), which is not setup's to call
	// old.
	current := readText(t, claude)
	older := strings.Replace(current, "Run exactly this command", "Run this command", 1)
	if older == current {
		t.Fatal("the skill has no wording to age")
	}
	must(t, os.WriteFile(claude, []byte(older), 0600))
	must(t, os.WriteFile(agents, []byte("my own version\n"), 0600))
	view := statusJSON(t, env)
	if got := view["agent_skills_out_of_date"]; !reflect.DeepEqual(got, []any{claude}) {
		t.Fatalf("agent_skills_out_of_date = %#v", got)
	}
	// The agent-archive skill's files are current and setup's own; the
	// shared /handoff one is the person's.
	if got := view["agent_skills"]; !reflect.DeepEqual(got, []any{claude, archiveSkillPaths(userHome)[0], archiveSkillPaths(userHome)[1]}) {
		t.Fatalf("agent_skills = %#v", got)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"status"}, nil, &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
	if !strings.Contains(out.String(), "The /handoff skill at ~/.claude/skills/handoff/SKILL.md is out of date. Run agent-archive setup --refresh to refresh it.") {
		t.Fatalf("status does not warn:\n%s", &out)
	}
	out.Reset()
	if code := Run([]string{"status", "--verbose"}, nil, &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
	if !strings.Contains(out.String(), "  /handoff:      ~/.claude/skills/handoff/SKILL.md (out of date; run agent-archive setup --refresh)\n") {
		t.Fatalf("status --verbose does not mark the file:\n%s", &out)
	}
	old, _, err := config.Load(home)
	must(t, err)
	next := old
	must(t, applySetup(home, userHome, old.InstalledExecutable, old, &next, nil, env))
	view = statusJSON(t, env)
	if _, ok := view["agent_skills_out_of_date"]; ok {
		t.Fatalf("setup left the skill out of date: %#v", view["agent_skills_out_of_date"])
	}
	if readText(t, agents) != "my own version\n" {
		t.Fatal("setup replaced the person's own skill")
	}
}

// A second installation in the same HOME, with hook files of its own, runs
// its skills with its data directory, and never replaces or removes the
// first one's shared ~/.agents skill.
func TestSecondInstallationKeepsTheFirstsHandoffSkill(t *testing.T) {
	t.Parallel()
	primary, secondary, userHome := twoInstallations(t)
	setupRun(t, primary, s3SetupInput("b", "us-east-1", "p", true, false, false, t.TempDir()), 0)
	first := readText(t, agentsSkillPath(userHome))
	if strings.Contains(first, "AGENT_ARCHIVE_HOME") {
		t.Fatalf("the default installation's skill names a data directory:\n%s", first)
	}
	claudeDir, codexDir := filepath.Join(userHome, "b", "claude"), filepath.Join(userHome, "b", "codex")
	vars := map[string]string{"CLAUDE_CONFIG_DIR": claudeDir, "CODEX_HOME": codexDir}
	secondary.LookupEnv = func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	output := setupRun(t, secondary, s3SetupInput("b", "us-east-1", "p", true, true, false, t.TempDir()), 0)
	if !strings.Contains(output, "Left ~/.agents/skills/handoff/SKILL.md as it is") {
		t.Fatalf("setup did not report the first installation's skill:\n%s", output)
	}
	own := readText(t, filepath.Join(claudeDir, "skills", "handoff", "SKILL.md"))
	if !strings.Contains(own, "AGENT_ARCHIVE_HOME=") {
		t.Fatalf("the relocated installation's skill lacks its data directory:\n%s", own)
	}
	uninstallRun(t, secondary)
	if readText(t, agentsSkillPath(userHome)) != first {
		t.Fatal("the second installation changed the first one's skill")
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "skills")); !os.IsNotExist(err) {
		t.Fatalf("uninstall left the second installation's skill: %v", err)
	}
}

// A failed setup puts back the skill it replaced, not only one it created,
// with its permissions.
func TestFailedSetupRestoresTheHandoffSkillItReplaced(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", false, true, false, project), 0)
	path := claudeSkillPath(userHome)
	older := strings.Replace(readText(t, path), "Run exactly this command", "Run this command", 1)
	must(t, os.WriteFile(path, []byte(older), 0644))
	must(t, os.Chmod(path, 0644))
	var written string
	fakeSched(env).beforeLoad = func(scheduler.Ref) error {
		if written == "" {
			written = readText(t, path)
			return errors.New("bootstrap failed")
		}
		return nil
	}
	setupRun(t, env, "retention\ncustom\n120\ny\n", 1)
	if written == older || written == "" {
		t.Fatal("setup did not replace the skill before it failed")
	}
	if readText(t, path) != older {
		t.Fatal("failed setup did not restore the skill it replaced")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("failed setup did not restore the skill's permissions: %v %v", info.Mode(), err)
	}
}

// With several skills registered, the review reports each skill's files
// under its own name: what was installed, and what was left alone.
func TestSetupReportsEachSkillItsOwnFiles(t *testing.T) {
	t.Parallel()
	userHome := t.TempDir()
	skills := []agentskills.Skill{{Name: "handoff", Slash: true, Summary: "continues a session in another agent"}, {Name: "second", Slash: true}, {Name: "third", Summary: "reads things"}}
	file := func(skill, name, content string) agentskills.File {
		path := filepath.Join(userHome, ".agents", "skills", skill, name)
		must(t, os.MkdirAll(filepath.Dir(path), 0700))
		must(t, os.WriteFile(path, []byte("on disk\n"), 0600))
		return agentskills.File{Skill: skill, Path: path, Content: []byte(content)}
	}
	files := []agentskills.File{
		file("handoff", "SKILL.md", "on disk\n"),
		file("second", "SKILL.md", "on disk\n"),
		file("second", "other.md", "not what is on disk\n"),
		file("third", "SKILL.md", "on disk\n"),
		file("third", "other.md", "not what is on disk\n"),
	}
	var out bytes.Buffer
	printSkillFiles(newPrompter(strings.NewReader(""), &out), skills, files, userHome)
	want := "Installed /handoff, which continues a session in another agent: ~/.agents/skills/handoff/SKILL.md\n" +
		"Left ~/.agents/skills/second/other.md as it is: it is not this agent-archive installation's (it lacks the marker line, or names another data directory), so /second is not installed there.\n" +
		"Installed /second: ~/.agents/skills/second/SKILL.md\n" +
		"Left ~/.agents/skills/third/other.md as it is: it is not this agent-archive installation's (it lacks the marker line, or names another data directory), so the third skill is not installed there.\n" +
		"Installed the third skill, which reads things: ~/.agents/skills/third/SKILL.md\n"
	if out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out.String(), want)
	}
}

// anyStrings is a decoded JSON array of strings.
func anyStrings(v any) []string {
	var out []string
	items, _ := v.([]any)
	for _, item := range items {
		out = append(out, item.(string))
	}
	return out
}

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	return string(data)
}
