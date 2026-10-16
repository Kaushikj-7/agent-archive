package cli

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
)

// The environments an agent's shell runs commands in. Each variable alone
// must switch interaction off.
const cursorAgentEnv = "CURSOR_AGENT"

var handoffSessionEnv = launchEnvironmentKeys(Env{})

func agentShellEnv() []string { return slices.Clone(agentVariables) }

var agentVariables = []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CURSOR_AGENT", "MUSE_THREAD_ID", "MUSE_SESSION_ID"}

func TestParseSwitchAcceptsOneSetOfSpellings(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{
		"1": true, "true": true, "yes": true, "on": true, "TRUE": true, "On": true,
		"0": false, "false": false, "no": false, "off": false, "False": false,
	} {
		if on, valid := parseSwitch(value); !valid || on != want {
			t.Errorf("parseSwitch(%q) = %v, %v; want %v, true", value, on, valid, want)
		}
	}
	for _, value := range []string{"", "2", "maybe", "ture", "y", "enable"} {
		if _, valid := parseSwitch(value); valid {
			t.Errorf("parseSwitch(%q) accepted", value)
		}
	}
}

func TestNonInteractiveResolution(t *testing.T) {
	t.Parallel()
	type vars = map[string]string
	cases := []struct {
		name    string
		vars    vars
		on      bool
		invalid bool
	}{
		{name: "unset and no agent", vars: vars{}},
		{name: "explicit on", vars: vars{envNonInteractive: "1"}, on: true},
		{name: "explicit on, any spelling", vars: vars{envNonInteractive: "Yes"}, on: true},
		{name: "explicit off", vars: vars{envNonInteractive: "0"}},
		{name: "empty means automatic, none set", vars: vars{envNonInteractive: ""}},
		{name: "empty means automatic, agent set", vars: vars{envNonInteractive: "", "CODEX_THREAD_ID": "t"}, on: true},
		{name: "whitespace around a spelling is ignored", vars: vars{envNonInteractive: " On\n"}, on: true},
		{name: "whitespace only means automatic, none set", vars: vars{envNonInteractive: "  "}},
		{name: "whitespace only means automatic, agent set", vars: vars{envNonInteractive: "\t", "CURSOR_AGENT": "1"}, on: true},
		{name: "mixed case off beats an agent", vars: vars{envNonInteractive: " oFf ", "CLAUDE_CODE_SESSION_ID": "s"}},
		{name: "an empty agent variable is not an agent", vars: vars{"CLAUDE_CODE_SESSION_ID": "  ", "CODEX_THREAD_ID": "", "CURSOR_AGENT": ""}},
		{name: "invalid counts as on until reported", vars: vars{envNonInteractive: "ture"}, on: true, invalid: true},
		{name: "invalid is invalid even with an agent variable", vars: vars{envNonInteractive: "2", "CURSOR_AGENT": "1"}, on: true, invalid: true},
		{name: "invalid is invalid when it would have said off", vars: vars{envNonInteractive: "nope"}, on: true, invalid: true},
	}
	for _, key := range agentVariables {
		cases = append(cases,
			struct {
				name    string
				vars    vars
				on      bool
				invalid bool
			}{name: key + " turns it on", vars: vars{key: "1"}, on: true},
			struct {
				name    string
				vars    vars
				on      bool
				invalid bool
			}{name: "explicit 0 beats " + key, vars: vars{key: "1", envNonInteractive: "0"}},
			struct {
				name    string
				vars    vars
				on      bool
				invalid bool
			}{name: "explicit false beats " + key, vars: vars{key: "1", envNonInteractive: "false"}},
			struct {
				name    string
				vars    vars
				on      bool
				invalid bool
			}{name: "explicit 1 with " + key, vars: vars{key: "1", envNonInteractive: "1"}, on: true},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := withEnvironment(Env{}, tc.vars)
			mode, err := env.nonInteractive()
			if mode.on != tc.on || (err != nil) != tc.invalid {
				t.Fatalf("on=%v err=%v; want on=%v invalid=%v", mode.on, err, tc.on, tc.invalid)
			}
			if mode.on && mode.reason == "" {
				t.Fatal("no reason given for interaction being off")
			}
			env.IsTerminal = func(any) bool { return true }
			if got := env.interactive(struct{}{}); got != !tc.on {
				t.Fatalf("interactive on a terminal = %v, want %v", got, !tc.on)
			}
			env.IsTerminal = func(any) bool { return false }
			if env.interactive(struct{}{}) {
				t.Fatal("interactive without a terminal")
			}
		})
	}
}

// The reason a person is told names the variable that turned interaction off.
func TestNonInteractiveReasonNamesTheVariable(t *testing.T) {
	t.Parallel()
	for _, key := range agentVariables {
		mode, _ := withEnvironment(Env{}, map[string]string{key: "x"}).nonInteractive()
		if !strings.Contains(mode.reason, key) {
			t.Errorf("%s: reason %q does not name it", key, mode.reason)
		}
	}
	mode, _ := withEnvironment(Env{}, map[string]string{envNonInteractive: "1"}).nonInteractive()
	if !strings.Contains(mode.reason, envNonInteractive) {
		t.Errorf("reason %q does not name the switch", mode.reason)
	}
}

// A bad setting is one usage error before the command runs, and never breaks
// the hooks, the collector, help, or version.
func TestInvalidNonInteractiveSettingIsOneUsageError(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	env = withEnvironment(env, map[string]string{envNonInteractive: "ture"})
	for _, args := range [][]string{nil, {"list"}, {"show"}, {"handoff"}, {"status"}, {"setup"}, {"sync"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 2 || out.Len() != 0 || strings.Count(errOut.String(), "\n") != 1 ||
			!strings.Contains(errOut.String(), `AGENT_ARCHIVE_NONINTERACTIVE="ture"`) || !strings.Contains(errOut.String(), "1 or 0") {
			t.Errorf("%v: code=%d stdout=%q stderr=%q", args, code, &out, &errOut)
		}
	}
	for _, args := range [][]string{{"version"}, {"--help"}, {"help", "list"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 0 || out.Len() == 0 || errOut.Len() != 0 {
			t.Errorf("%v: code=%d stderr=%q", args, code, &errOut)
		}
	}
	// The hidden commands are silent whatever the environment says.
	var out, errOut bytes.Buffer
	Run([]string{"_hook", "codex", "SessionStart"}, strings.NewReader("{}"), &out, &errOut, env)
	if strings.Contains(errOut.String(), envNonInteractive) {
		t.Errorf("_hook reported the setting: %q", &errOut)
	}
}

// ttyRun runs args with stdin and stdout both terminals, the way an agent's
// pseudo-terminal looks, failing the test if the command reads its input or
// starts a pager.
func ttyRun(t *testing.T, env Env, input string, args ...string) (out, errOut string, code int) {
	t.Helper()
	stdin := strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&stdout) }
	env.RunPager = func(_ context.Context, _ string, _ []string, in io.Reader, _, _ io.Writer) error {
		t.Errorf("%v started a pager", args)
		_, err := io.Copy(io.Discard, in)
		return err
	}
	code = Run(args, stdin, &stdout, &stderr, env)
	if stdin.Len() != len(input) {
		t.Errorf("%v read %d bytes of input it was not to ask for", args, len(input)-stdin.Len())
	}
	return stdout.String(), stderr.String(), code
}

// agentShell is the environment an agent's shell gives, one variable of it.
func agentShell(key string) map[string]string { return map[string]string{key: "session-1"} }

func TestListOnATerminalInAnAgentIsAPlainTable(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	for _, key := range agentVariables {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			out, errOut, code := ttyRun(t, withEnvironment(env, agentShell(key)), "1\nq\n", "list")
			if code != 0 || errOut != "" {
				t.Fatalf("code=%d stderr=%s", code, errOut)
			}
			if strings.Contains(out, "Enter number") || strings.Contains(out, enterAltScreenSequence) || strings.Contains(out, "\x1b[?1000h") {
				t.Fatalf("picker or alternate screen in an agent:\n%q", out)
			}
			if !strings.Contains(out, id[:minShortSessionID]) {
				t.Fatalf("listing missing the session:\n%s", out)
			}
		})
	}
	t.Run("explicit switch", func(t *testing.T) {
		t.Parallel()
		out, _, code := ttyRun(t, withEnvironment(env, map[string]string{envNonInteractive: "1"}), "1\nq\n", "list")
		if code != 0 || strings.Contains(out, "Enter number") {
			t.Fatalf("code=%d:\n%s", code, out)
		}
	})
	t.Run("explicit 0 gives the picker back inside an agent", func(t *testing.T) {
		t.Parallel()
		vars := agentShell("CLAUDE_CODE_SESSION_ID")
		vars[envNonInteractive] = "0"
		stdin := strings.NewReader("1\nq\n")
		var stdout, stderr bytes.Buffer
		e := withEnvironment(env, vars)
		e.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&stdout) }
		if code := Run([]string{"list"}, stdin, &stdout, &stderr, e); code != 0 || !strings.Contains(stdout.String(), "Enter number") {
			t.Fatalf("code=%d stderr=%s out=%s", code, &stderr, &stdout)
		}
	})
	t.Run("unset and no agent variable", func(t *testing.T) {
		t.Parallel()
		stdin := strings.NewReader("1\nq\n")
		var stdout, stderr bytes.Buffer
		e := env
		e.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&stdout) }
		if code := Run([]string{"list"}, stdin, &stdout, &stderr, e); code != 0 || !strings.Contains(stdout.String(), "Enter number") {
			t.Fatalf("code=%d stderr=%s out=%s", code, &stderr, &stdout)
		}
	})
}

// Bare show and show --json on a terminal never open the one-shot picker
// in an agent; they ask for an ID like they do when piped.
func TestBareShowOnATerminalInAnAgentNeedsAnID(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	for _, key := range agentVariables {
		for _, args := range [][]string{{"show"}, {"show", "--json"}} {
			out, errOut, code := ttyRun(t, withEnvironment(env, agentShell(key)), "1\nq\n", args...)
			if code != 2 || !strings.Contains(errOut, "SESSION_ID is required") || strings.Contains(out, "Enter number") {
				t.Errorf("%s %v: code=%d stderr=%q stdout=%q", key, args, code, errOut, out)
			}
		}
	}
}

// Several matches print the candidates and exit 1 instead of picking.
func TestAmbiguousShowInAnAgentListsCandidates(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	other := id[:len(id)-1] + "0"
	if other == id {
		other = id[:len(id)-1] + "1"
	}
	keys, err := mem.List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range keys {
		if !strings.Contains(object.Key, id) {
			continue
		}
		data, err := mem.Get(t.Context(), object.Key)
		if err != nil {
			t.Fatal(err)
		}
		copied := strings.ReplaceAll(string(data), id, other)
		if err := mem.Put(t.Context(), strings.ReplaceAll(object.Key, id, other), []byte(copied)); err != nil {
			t.Fatal(err)
		}
	}
	query := id[:len(id)-1]

	// On a terminal, outside an agent, the picker opens.
	out, code := ttyRunAllowingInput(t, env, "q\n", "show", query)
	if code != 0 || !strings.Contains(out, "Enter number") {
		t.Fatalf("no picker outside an agent: code=%d\n%s", code, out)
	}
	for _, key := range agentVariables {
		out, errOut, code := ttyRun(t, withEnvironment(env, agentShell(key)), "1\n", "show", query)
		if code != 1 || strings.Contains(out, "Enter number") || !strings.Contains(errOut, "matches 2 sessions") ||
			!strings.Contains(errOut, id[:minShortSessionID]) || !strings.Contains(errOut, other[:minShortSessionID]) {
			t.Errorf("%s: code=%d stdout=%q stderr=%q", key, code, out, errOut)
		}
	}
}

// ttyRunAllowingInput is ttyRun for a run that is meant to read its input.
func ttyRunAllowingInput(t *testing.T, env Env, input string, args ...string) (out string, code int) {
	t.Helper()
	stdin := strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&stdout) }
	code = Run(args, stdin, &stdout, &stderr, env)
	return stdout.String(), code
}

// No pager starts in an agent, on a terminal whose stdin is not the one
// interaction depends on.
func TestPagerNotStartedInAnAgent(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	run := func(vars map[string]string) int {
		var stdout, stderr bytes.Buffer
		pages := 0
		e := withEnvironment(env, vars)
		e.IsTerminal = func(stream any) bool { return stream == any(&stdout) }
		e.RunPager = func(_ context.Context, _ string, _ []string, in io.Reader, _, _ io.Writer) error {
			pages++
			_, err := io.Copy(io.Discard, in)
			return err
		}
		if code := Run([]string{"list"}, strings.NewReader(""), &stdout, &stderr, e); code != 0 {
			t.Fatalf("code=%d stderr=%s", code, &stderr)
		}
		return pages
	}
	if pages := run(nil); pages != 1 {
		t.Fatalf("pager started %d times outside an agent, want 1", pages)
	}
	for _, key := range agentVariables {
		if pages := run(agentShell(key)); pages != 0 {
			t.Errorf("%s: pager started %d times", key, pages)
		}
	}
	if pages := run(map[string]string{envNonInteractive: "1"}); pages != 0 {
		t.Errorf("explicit switch: pager started %d times", pages)
	}
	vars := agentShell("CODEX_THREAD_ID")
	vars[envNonInteractive] = "0"
	if pages := run(vars); pages != 1 {
		t.Errorf("explicit 0: pager started %d times, want 1", pages)
	}
}

// handoff never opens its picker in an agent, with or without --to, and a
// session named on the command line, including the caller's own, still works.
// Inside Cursor, --to takes the newest Cursor session instead of asking, and
// the fixture has none.
func TestHandoffInAnAgentNeverPicks(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, true)
	for _, key := range agentVariables {
		for _, args := range [][]string{{"handoff"}, {"handoff", "--harness", "codex"}, {"handoff", "--to", "codex"}, {"handoff", "--to", "claude", "--harness", "cursor"}} {
			f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
				t.Errorf("%s %v: launched without a session", key, args)
				return nil
			}
			out, errOut, code := ttyRun(t, withEnvironment(f.env, agentShell(key)), "1\n", args...)
			wantCode, want := 2, "name a session ID or title, --latest, or --file PATH"
			if key == cursorAgentEnv && slices.Contains(args, "--to") {
				wantCode, want = 1, "no session for "
			}
			if code != wantCode || !strings.Contains(errOut, want) || out != "" {
				t.Errorf("%s %v: code=%d stdout=%q stderr=%q", key, args, code, out, errOut)
			}
		}
	}
	// The same words on a terminal outside an agent still open the picker.
	out, code := ttyRunAllowingInput(t, f.env, "q\n", "handoff", "--harness", "codex")
	if code != 0 || !strings.Contains(out, "to hand off") {
		t.Fatalf("no picker outside an agent: code=%d\n%s", code, out)
	}
}

// Inside an agent, even on a terminal, the launched agent gets a new window:
// the agent's shell is not a terminal to take over.
func TestHandoffToFromInsideAnAgentUsesTheCallingSession(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		t.Error("ran in the agent's terminal")
		return nil
	}
	launched := 0
	f.env.OpenTerminal = func(termlaunch.Spec) (string, error) {
		launched++
		return "a new tmux window", nil
	}
	env := withEnvironment(f.env, map[string]string{"CODEX_THREAD_ID": "native-1"})
	_, errOut, code := ttyRun(t, env, "1\n", "handoff", "--latest", "--harness", "codex", "--to", "claude")
	if code != 0 || launched != 1 {
		t.Fatalf("code=%d launched=%d stderr=%s", code, launched, errOut)
	}
	_, errOut, code = ttyRun(t, env, "1\n", "handoff", f.id, "--to", "claude")
	if code != 0 || launched != 2 {
		t.Fatalf("by ID: code=%d launched=%d stderr=%s", code, launched, errOut)
	}
}

// Commands that must have a terminal to ask keep refusing, and when the
// switch is what made them refuse they say how to override it.
func TestConfirmationsRefuseWithTheOverrideHint(t *testing.T) {
	t.Parallel()
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	for _, key := range agentVariables {
		agentEnv := withEnvironment(env, agentShell(key))
		for _, cmd := range []string{"setup", "uninstall"} {
			out, errOut, code := ttyRun(t, agentEnv, "y\n", cmd)
			if code != 1 || out != "" || strings.Count(errOut, "\n") != 1 || !strings.Contains(errOut, "Nothing was changed") ||
				!strings.Contains(errOut, key) || !strings.Contains(errOut, "AGENT_ARCHIVE_NONINTERACTIVE=0") {
				t.Errorf("%s %s: code=%d stdout=%q stderr=%q", key, cmd, code, out, errOut)
			}
		}
	}
	if cfg, _, _ := config.Load(home); !cfg.Archive.Enabled {
		t.Fatal("refusing changed the configuration")
	}

	// The explicit switch says so too.
	_, errOut, code := ttyRun(t, withEnvironment(env, map[string]string{envNonInteractive: "true"}), "y\n", "uninstall")
	if code != 1 || !strings.Contains(errOut, envNonInteractive+" is set") || !strings.Contains(errOut, envNonInteractive+"=0") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}

	// Without a terminal the switch is not the reason, so it is not mentioned.
	var out, errBuf bytes.Buffer
	noTerminal := withEnvironment(env, agentShell("CODEX_THREAD_ID"))
	noTerminal.IsTerminal = func(any) bool { return false }
	if code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errBuf, noTerminal); code != 1 || strings.Contains(errBuf.String(), envNonInteractive) {
		t.Fatalf("piped: code=%d stderr=%q", code, &errBuf)
	}

	// --yes still works in an agent shell: nothing is asked.
	if code := Run([]string{"uninstall", "--yes"}, nil, &out, &errBuf, noTerminal); code != 0 {
		t.Fatalf("uninstall --yes: code=%d stderr=%q", code, &errBuf)
	}
}

// AGENT_ARCHIVE_NONINTERACTIVE=0 is the override the refusal names, and it
// works: the confirmation is asked.
func TestExplicitZeroLetsAConfirmationBeAskedInAnAgent(t *testing.T) {
	t.Parallel()
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("b", "us-east-1", "p", false, true, false, t.TempDir()))
	vars := agentShell("CLAUDE_CODE_SESSION_ID")
	vars[envNonInteractive] = "0"
	stdin := strings.NewReader("y\n")
	var out, errOut bytes.Buffer
	env = withEnvironment(env, vars)
	env.IsTerminal = func(stream any) bool { _, ok := stream.(*strings.Reader); return ok }
	if code := Run([]string{"uninstall"}, stdin, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s stdout=%s", code, &errOut, &out)
	}
	if cfg, _, _ := config.Load(home); cfg.Archive.Enabled {
		t.Fatal("uninstall did not run after the override")
	}
}

func TestBackfillConfirmationsRefuseWithTheOverrideHint(t *testing.T) {
	t.Parallel()
	f, _ := newUndoFixture(t)
	f.env = withEnvironment(f.env, agentShell("CODEX_THREAD_ID"))
	for _, args := range [][]string{{}, {"undo"}} {
		out, errOut, code := f.importRun(t, strings.NewReader("y\n"), true, args...)
		if code != 1 || out != "" || !strings.Contains(errOut, "needs a terminal") || !strings.Contains(errOut, "Nothing was changed") ||
			!strings.Contains(errOut, "CODEX_THREAD_ID") || !strings.Contains(errOut, "AGENT_ARCHIVE_NONINTERACTIVE=0") {
			t.Errorf("backfill %v: code=%d stdout=%q stderr=%q", args, code, out, errOut)
		}
	}
	// --dry-run needs no terminal and still works.
	if _, errOut, code := f.importRun(t, strings.NewReader(""), true, "--dry-run"); code != 0 {
		t.Fatalf("dry run: code=%d stderr=%s", code, errOut)
	}
}

// setup --yes never reads a secret from a terminal it may not ask on.
func TestSetupYesDoesNotReadTheR2SecretFromATerminalInAnAgent(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := withEnvironment(setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now()), agentShell("CODEX_THREAD_ID"))
	stdin := strings.NewReader("private-secret\n")
	var out, errOut bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) }
	code := Run([]string{"setup", "--yes", "--provider", "r2", "--r2-account", testR2Account, "--r2-access-key-id", "KEY", "--bucket", "b", "--project", project, "--apps", "codex", "--codex-discovery", "on", "--codex-capture-scope", "included-projects"}, stdin, &out, &errOut, env)
	if code == 0 || stdin.Len() != len("private-secret\n") || !strings.Contains(errOut.String()+out.String(), envR2SecretAccessKey) {
		t.Fatalf("code=%d unread=%d\n%s\n%s", code, stdin.Len(), &out, &errOut)
	}
	if _, found, _ := config.Load(home); found {
		t.Fatal("saved a configuration")
	}
	// The refusal comes before the Keychain or launchd is asked anything, as
	// for a script whose secret is missing.
	if strings.Contains(out.String(), "Keychain") || strings.Contains(out.String(), "Background job") {
		t.Fatalf("checks ran before the refusal:\n%s", &out)
	}
}
