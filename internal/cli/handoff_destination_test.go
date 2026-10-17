package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/termlaunch"
)

// copyPager is Env.RunPager for tests: it records the command and copies
// its input to stdout, as `less -F` does with text that fits.
func copyPager(command *string) func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
	return func(_ context.Context, c string, _ []string, stdin io.Reader, stdout, _ io.Writer) error {
		*command = c
		_, err := io.Copy(stdout, stdin)
		return err
	}
}

// pipedHandoff is what `agent-archive handoff ARGS | cat` prints: stdout
// alone is not a terminal.
func pipedHandoff(t *testing.T, env Env, args ...string) string {
	t.Helper()
	out, errOut, code := runHandoff(t, env, args...)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	return out
}

func TestDefaultDestination(t *testing.T) {
	t.Parallel()
	all := []handoffDestination{handoffDestinationClaude, handoffDestinationCodex, handoffDestinationCursor}
	for _, tc := range []struct {
		name      string
		installed []handoffDestination
		source    string
		defaultTo map[string]string
		want      handoffDestination
	}{
		{"claude goes to codex", all, "claude", nil, handoffDestinationCodex},
		{"codex goes to claude", all, "codex", nil, handoffDestinationClaude},
		{"cursor goes to claude", all, "cursor", nil, handoffDestinationClaude},
		{"configured default", all, "claude", map[string]string{"claude": "cursor"}, handoffDestinationCursor},
		{"configured for another harness", all, "codex", map[string]string{"claude": "cursor"}, handoffDestinationClaude},
		{"configured but not installed", []handoffDestination{handoffDestinationClaude, handoffDestinationCodex}, "claude", map[string]string{"claude": "cursor"}, handoffDestinationCodex},
		{"built-in default not installed", []handoffDestination{handoffDestinationClaude, handoffDestinationCursor}, "claude", nil, handoffDestinationClaude},
		{"unknown source", []handoffDestination{handoffDestinationCursor}, "", nil, handoffDestinationCursor},
		{"nothing installed", nil, "claude", map[string]string{"claude": "codex"}, ""},
	} {
		if got := defaultDestination(tc.installed, tc.source, config.HandoffConfig{DefaultTo: tc.defaultTo}); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestInstalledDestinationsFollowLookPath(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	env.LookPath = func(name string) (string, error) {
		if slices.Contains([]string{"cursor-agent", "codex"}, name) {
			return "/opt/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	if got := installedDestinations(env); !slices.Equal(got, []handoffDestination{handoffDestinationCodex, handoffDestinationCursor}) {
		t.Fatalf("installed = %q", got)
	}
}

func TestChooseDestinationAnswers(t *testing.T) {
	t.Parallel()
	all := []handoffDestination{handoffDestinationClaude, handoffDestinationCodex, handoffDestinationCursor}
	for _, tc := range []struct {
		name      string
		installed []handoffDestination
		answer    string
		want      handoffChoice
	}{
		{"enter takes the default", all, "\n", handoffChoice{action: handoffLaunch, dest: handoffDestinationCodex}},
		{"the default is numbered first", all, "2\n", handoffChoice{action: handoffLaunch, dest: handoffDestinationClaude}},
		{"the rest keep their order", all, "3\n", handoffChoice{action: handoffLaunch, dest: handoffDestinationCursor}},
		{"an agent's name", all, "cursor\n", handoffChoice{action: handoffLaunch, dest: handoffDestinationCursor}},
		{"print", all, "p\n", handoffChoice{action: handoffPrint}},
		{"copy", all, "C\n", handoffChoice{action: handoffCopy}},
		{"write", all, "write\n", handoffChoice{action: handoffWrite}},
		{"quit", all, "q\n", handoffChoice{action: handoffQuit}},
		{"end of input quits", all, "", handoffChoice{action: handoffQuit}},
		{"asks again", all, "4\nnope\n0\nq\n", handoffChoice{action: handoffQuit}},
		{"an uninstalled agent's name asks again", []handoffDestination{handoffDestinationCodex}, "claude\n1\n", handoffChoice{action: handoffLaunch, dest: handoffDestinationCodex}},
		{"nothing installed: enter prints", nil, "\n", handoffChoice{action: handoffPrint}},
		{"nothing installed: no numbers", nil, "1\nc\n", handoffChoice{action: handoffCopy}},
		{"one agent: enter launches it", []handoffDestination{handoffDestinationClaude}, "\n", handoffChoice{action: handoffLaunch, dest: handoffDestinationClaude}},
		{"one agent: y launches it", []handoffDestination{handoffDestinationClaude}, "Y\n", handoffChoice{action: handoffLaunch, dest: handoffDestinationClaude}},
		{"one agent: yes launches it", []handoffDestination{handoffDestinationClaude}, "yes\n", handoffChoice{action: handoffLaunch, dest: handoffDestinationClaude}},
		{"one agent: its name launches it", []handoffDestination{handoffDestinationClaude}, "claude\n", handoffChoice{action: handoffLaunch, dest: handoffDestinationClaude}},
		{"one agent: n quits", []handoffDestination{handoffDestinationClaude}, "n\n", handoffChoice{action: handoffQuit}},
		{"one agent: no quits", []handoffDestination{handoffDestinationClaude}, "no\n", handoffChoice{action: handoffQuit}},
		{"one agent: q still quits", []handoffDestination{handoffDestinationClaude}, "q\n", handoffChoice{action: handoffQuit}},
		{"one agent: print", []handoffDestination{handoffDestinationClaude}, "p\n", handoffChoice{action: handoffPrint}},
		{"one agent: write", []handoffDestination{handoffDestinationClaude}, "w\n", handoffChoice{action: handoffWrite}},
		{"one agent: a word's prefix", []handoffDestination{handoffDestinationClaude}, "qu\n", handoffChoice{action: handoffQuit}},
		{"one agent: its name's prefix asks again", []handoffDestination{handoffDestinationClaude}, "cl\nn\n", handoffChoice{action: handoffQuit}},
	} {
		var out bytes.Buffer
		def := defaultDestination(tc.installed, "claude", config.HandoffConfig{})
		got, err := chooseDestination(newPrompter(strings.NewReader(tc.answer), &out), tc.installed, def, true, productionAgents.Catalog(), nil)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v, %v; want %+v\n%s", tc.name, got, err, tc.want, out.String())
		}
	}
}

func TestChooseDestinationListsTheDefaultFirst(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	installed := []handoffDestination{handoffDestinationClaude, handoffDestinationCodex, handoffDestinationCursor}
	if _, err := chooseDestination(newPrompter(strings.NewReader("q\n"), &out), installed, handoffDestinationCodex, true, productionAgents.Catalog(), nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"? Continue in", "1) Codex (default)", "2) Claude Code", "3) Cursor", "[p] print  [c] copy  [w] write  [q] quit", "Choose [1]", "Handoff quit"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in prompt:\n%s", want, out.String())
		}
	}
	out.Reset()
	if _, err := chooseDestination(newPrompter(strings.NewReader("q\n"), &out), nil, "", true, productionAgents.Catalog(), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "[q] quit") || !strings.Contains(out.String(), "[Enter] print") || strings.Contains(out.String(), "1)") {
		t.Fatalf("prompt with no agent installed:\n%s", out.String())
	}
	out.Reset()
	if _, err := chooseDestination(newPrompter(strings.NewReader("q\n"), &out), []handoffDestination{handoffDestinationClaude}, handoffDestinationClaude, true, productionAgents.Catalog(), nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"? Continue in Claude Code? [Y/n]", "or [p] print  [c] copy  [w] write", "Handoff quit"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in prompt with one agent installed:\n%s", want, out.String())
		}
	}
	for _, unwanted := range []string{"1)", "[q]", "Choose"} {
		if strings.Contains(out.String(), unwanted) {
			t.Fatalf("unexpected %q in prompt with one agent installed:\n%s", unwanted, out.String())
		}
	}
}

// Without a clipboard, c is no answer: it must not abbreviate the one agent.
func TestChooseDestinationOneAgentWithoutClipboard(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	got, err := chooseDestination(newPrompter(strings.NewReader("c\nn\n"), &out), []handoffDestination{handoffDestinationClaude}, handoffDestinationClaude, false, productionAgents.Catalog(), nil)
	if err != nil || got != (handoffChoice{action: handoffQuit}) {
		t.Fatalf("got %+v, %v\n%s", got, err, out.String())
	}
	if !strings.Contains(out.String(), "or [p] print  [w] write\n") || !strings.Contains(out.String(), "Enter one of the available choices.") {
		t.Fatalf("prompt without a clipboard:\n%s", out.String())
	}
}

func TestChooseDestinationNamesTheSessionAboveTheQuestion(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	header := []string{"Fix the scroll bug", "Codex · 2 hours ago · 14 KB"}
	if _, err := chooseDestination(newPrompter(strings.NewReader("q\n"), &out), []handoffDestination{handoffDestinationClaude}, handoffDestinationClaude, true, productionAgents.Catalog(), header); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	title, facts, question := strings.Index(text, header[0]), strings.Index(text, header[1]), strings.Index(text, "? Continue in")
	if title < 0 || facts < title || question < facts {
		t.Fatalf("header is not above the question:\n%s", text)
	}
}

func TestHandoffPromptHeaderNamesTitleAgentAgeAndSize(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	active := now.Add(-2 * time.Hour)
	h := archive.Handoff{Session: archive.HandoffSession{Harness: "codex", LastActivityAt: &active}}
	target := handoffTarget{metadata: &archive.Metadata{Title: "fix the scroll bug"}}
	got := handoffPromptHeader(target, h, 14000, now, productionAgents.Catalog(), false)
	want := []string{"Fix the scroll bug", "Codex · 2 hours ago · 14 KB"}
	if !slices.Equal(got, want) {
		t.Fatalf("header = %q, want %q", got, want)
	}
	// Without a title or activity time, the agent and size remain.
	got = handoffPromptHeader(handoffTarget{}, archive.Handoff{Session: archive.HandoffSession{Harness: "claude"}}, 10, now, productionAgents.Catalog(), false)
	if want := []string{"Claude Code · 1 KB"}; !slices.Equal(got, want) {
		t.Fatalf("header = %q, want %q", got, want)
	}
	// An ASCII terminal gets ASCII separators.
	got = handoffPromptHeader(target, h, 14000, now, productionAgents.Catalog(), true)
	if want := []string{"Fix the scroll bug", "Codex - 2 hours ago - 14 KB"}; !slices.Equal(got, want) {
		t.Fatalf("ASCII header = %q, want %q", got, want)
	}
}

// Picked on a terminal, a Claude Code session goes to Codex on Enter, in
// this terminal.
func TestHandoffPromptLaunchesTheDefaultHere(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	claude := f.addSession(t, "claude", "claude-native", "A Claude task", f.env.now().Add(time.Hour))
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	var got launchSpec
	f.env.LaunchHandoff = func(spec launchSpec, stdin io.Reader, _, _ io.Writer) error {
		got = spec
		// The agent reads the terminal itself, not the prompts' buffer.
		if _, buffered := stdin.(*bufio.Reader); buffered {
			t.Error("launched with the prompts' buffered reader as stdin")
		}
		return nil
	}
	out, errOut, code := runPicker(t, f.env, claude[:minShortSessionID]+"\n\n")
	if code != 0 || got.Destination != handoffDestinationCodex || got.Binary != "/opt/bin/codex" || got.Args[0] != "--cd" {
		t.Fatalf("code=%d spec=%+v stderr=%s\n%s", code, got, errOut, out)
	}
	data, err := os.ReadFile(got.HandoffFile)
	if err != nil || !strings.Contains(string(data), "A Claude task") || !strings.Contains(string(data), claude) {
		t.Fatalf("launched with %q (%v)", data, err)
	}
	if !strings.Contains(out, "1) Codex (default)") || !strings.Contains(errOut, "handoff: launching local codex in "+f.project) {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if strings.Contains(out, "## Where it left off") {
		t.Fatalf("launching also printed the handoff:\n%s", out)
	}
}

// config.json's handoff.default_to overrides the built-in default, and only
// installed agents are offered.
func TestHandoffPromptUsesConfiguredDefault(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	cfg, _, err := config.Load(f.home)
	must(t, err)
	cfg.Handoff.DefaultTo = map[string]string{"codex": "cursor"}
	must(t, config.Save(f.home, cfg))
	f.env.LookPath = func(name string) (string, error) {
		if name == "claude" {
			return "", errors.New("not found")
		}
		return "/opt/bin/" + name, nil
	}
	out, _, code := runPicker(t, f.env, "q\n", f.id)
	if code != 0 || !strings.Contains(out, "1) Cursor (default)\n  2) Codex\n  3) Muse Code\n\n  [p] print") || strings.Contains(out, "Claude Code") {
		t.Fatalf("code=%d\n%s", code, out)
	}
}

// p prints what a pipe would have, through the pager.
func TestHandoffPromptPrintsThroughThePager(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	want := pipedHandoff(t, f.env, f.id)
	var pager string
	f.env.RunPager = copyPager(&pager)
	out, errOut, code := runPicker(t, f.env, "p\n", f.id)
	if code != 0 || !strings.HasPrefix(pager, "less") || !strings.Contains(out, "Choose [1]: ") || !strings.Contains(out, "Handoff print\n\n") || !strings.HasSuffix(out, want) {
		t.Fatalf("code=%d pager=%q stderr=%s\n%s", code, pager, errOut, out)
	}
}

func TestHandoffPromptCopies(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	want := pipedHandoff(t, f.env, f.id)
	var copied []byte
	f.env.Clipboard = func(data []byte) error {
		copied = data
		return nil
	}
	out, errOut, code := runPicker(t, f.env, "c\n", f.id)
	if code != 0 || string(copied) != want || strings.Contains(out, "## Where it left off") {
		t.Fatalf("code=%d copied=%q stderr=%s\n%s", code, copied, errOut, out)
	}
	if !strings.HasSuffix(errOut, "handoff: copied "+strconv.Itoa(len(want))+" bytes\n") {
		t.Fatalf("stderr=%q", errOut)
	}
	f.env.Clipboard = func([]byte) error { return errors.New("pbcopy: not found") }
	if _, errOut, code := runPicker(t, f.env, "c\n", f.id); code != 1 || !strings.Contains(errOut, "copy: pbcopy: not found") {
		t.Fatalf("clipboard failure: code=%d stderr=%s", code, errOut)
	}
}

// w writes a private file, suggesting handoff-<short id>.md in the launch
// directory, and replaces an existing file only when told to.
func TestHandoffPromptWritesAFile(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	want := pipedHandoff(t, f.env, f.id)
	dir := t.TempDir()
	path := filepath.Join(dir, "h.md")
	out, errOut, code := runPicker(t, f.env, "w\n"+path+"\n", f.id)
	if code != 0 || !strings.Contains(out, "? Write to\n\n") || !strings.Contains(out, "Choose ["+filepath.Join(f.project, "handoff-"+shortSessionID(f.id)+".md")+"]: ") || !strings.Contains(errOut, "handoff: wrote "+path) {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	info, err := os.Stat(path)
	if data, _ := os.ReadFile(path); err != nil || info.Mode().Perm() != 0o600 || string(data) != want {
		t.Fatalf("file: %v %v %q", info, err, data)
	}
	// Declining to replace asks for another path; the file is untouched.
	must(t, os.WriteFile(path, []byte("mine"), 0o600))
	other := filepath.Join(dir, "other.md")
	out, errOut, code = runPicker(t, f.env, "w\n"+path+"\n\n"+other+"\n", f.id)
	if data, _ := os.ReadFile(path); code != 0 || string(data) != "mine" || !strings.Contains(out, "? "+path+" already exists. Replace it?") || !strings.Contains(out, "2) No (default)") {
		t.Fatalf("declined replace: code=%d file=%q stderr=%s\n%s", code, data, errOut, out)
	}
	if data, _ := os.ReadFile(other); string(data) != want {
		t.Fatalf("second path holds %q", data)
	}
	out, errOut, code = runPicker(t, f.env, "w\n"+path+"\ny\n", f.id)
	if data, _ := os.ReadFile(path); code != 0 || string(data) != want {
		t.Fatalf("confirmed replace: code=%d file=%q stderr=%s\n%s", code, data, errOut, out)
	}
}

// The default is in --project when given; Enter writes it there.
func TestHandoffPromptWritesTheDefaultInTheProject(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	elsewhere := t.TempDir()
	f.env.WorkingDir = func() (string, error) { return elsewhere, nil }
	if _, errOut, code := runPicker(t, f.env, "w\n\n", "--latest", "--project", f.project); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(f.project, "handoff-"+shortSessionID(f.id)+".md")); err != nil {
		t.Fatal(err)
	}
}

// q cancels even at the first prompt instead of writing a file named q, and
// a directory is refused before asking to replace it.
func TestHandoffPromptWriteCancelsAndRefusesADirectory(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	dir := t.TempDir()
	f.env.WorkingDir = func() (string, error) { return dir, nil }
	out, errOut, code := runPicker(t, f.env, "w\nq\n", f.id)
	if entries, _ := os.ReadDir(dir); code != 0 || len(entries) != 0 || strings.Contains(errOut, "wrote") {
		t.Fatalf("q: code=%d entries=%v stderr=%s\n%s", code, entries, errOut, out)
	}
	out, errOut, code = runPicker(t, f.env, "w\n"+dir+"\n\n", f.id)
	if code != 0 || !strings.Contains(errOut, dir+" is a directory") || strings.Contains(out, "Replace it?") {
		t.Fatalf("directory: code=%d stderr=%s\n%s", code, errOut, out)
	}
}

// ~/ is the home directory, as a shell would read it; ~user is not
// expanded, so it is a relative path that here does not exist.
func TestHandoffPromptWritesUnderHome(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	home := t.TempDir()
	f.env.UserHomeDir = func() (string, error) { return home, nil }
	if _, errOut, code := runPicker(t, f.env, "w\n~/notes/../h.md\n", f.id); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, "h.md")); err != nil {
		t.Fatal(err)
	}
	must(t, os.Mkdir(filepath.Join(home, "someone"), 0o700))
	_, errOut, code := runPicker(t, f.env, "w\n~someone/h.md\n\n", f.id)
	if _, err := os.Stat(filepath.Join(home, "someone", "h.md")); code != 0 || err == nil || !strings.Contains(errOut, "open ~someone/h.md") {
		t.Fatalf("~someone: code=%d stderr=%s", code, errOut)
	}
}

// A failed write is reported and asked again; another path then works, and
// Enter or q gives up without failing.
func TestHandoffPromptAsksAgainAfterAFailedWrite(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	dir := t.TempDir()
	missing := filepath.Join(dir, "no-such-dir", "h.md")
	good := filepath.Join(dir, "h.md")
	out, errOut, code := runPicker(t, f.env, "w\n"+missing+"\n"+good+"\n", f.id)
	if code != 0 || !strings.Contains(errOut, "agent-archive: handoff: open "+missing) || !strings.Contains(out, "? Write to (Enter to cancel)\n\n") || !strings.Contains(out, "Choose: ") {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	if _, err := os.Stat(good); err != nil {
		t.Fatal(err)
	}
	// Input ending cancels rather than asking forever.
	for _, cancel := range []string{"\n", "q\n", ""} {
		out, errOut, code := runPicker(t, f.env, "w\n"+missing+"\n"+cancel, f.id)
		if code != 0 || strings.Contains(errOut, "wrote") || strings.Contains(out, "## Where it left off") {
			t.Fatalf("cancel %q: code=%d stderr=%s\n%s", cancel, code, errOut, out)
		}
	}
	if entries, _ := os.ReadDir(f.project); len(entries) != 0 && slices.ContainsFunc(entries, func(e os.DirEntry) bool { return strings.HasPrefix(e.Name(), "handoff-") }) {
		t.Fatalf("a cancelled write wrote the default: %v", entries)
	}
}

func TestHandoffPromptQuitDoesNothing(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	out, errOut, code := runPicker(t, f.env, "q\n", f.id)
	if code != 0 || strings.Contains(out, "## Where it left off") || errOut != "" {
		t.Fatalf("code=%d stderr=%q\n%s", code, errOut, out)
	}
}

// A pipe, --output, and JSON never ask and print exactly what they did
// before the prompt existed; nothing looks for agents.
func TestHandoffWithoutATerminalPrintsAsBefore(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.LookPath = func(name string) (string, error) {
		t.Errorf("looked for %s", name)
		return "", errors.New("not found")
	}
	path := filepath.Join(t.TempDir(), "h.md")
	if _, errOut, code := runHandoff(t, f.env, "--latest", "--output", path); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	want, err := os.ReadFile(path)
	must(t, err)
	out, errOut, code := runHandoff(t, f.env, "--latest")
	if code != 0 || out != string(want) || !strings.Contains(out, "## Where it left off") || strings.Contains(errOut, "Continue in") {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
	}
	// `agent-archive handoff --latest | cat` from a terminal: stdin is one,
	// stdout is not.
	stdin := strings.NewReader("")
	var piped, pipedErr bytes.Buffer
	env := f.env
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) }
	if code := Run([]string{"handoff", "--latest"}, stdin, &piped, &pipedErr, env); code != 0 || piped.String() != string(want) {
		t.Fatalf("piped: code=%d stderr=%s\n%s", code, pipedErr.String(), piped.String())
	}
	for _, args := range [][]string{{"--latest", "--format", "json"}, {"--latest", "--output", filepath.Join(t.TempDir(), "x.md")}, {"--latest", "--no-preamble"}} {
		out, errOut, code := runPicker(t, f.env, "", args...)
		if code != 0 || strings.Contains(out, "Continue in") {
			t.Fatalf("%v on a terminal: code=%d stderr=%s\n%s", args, code, errOut, out)
		}
	}
}

// openInFakeTmux is Env.OpenTerminal running the real termlaunch.Open as
// if inside tmux, recording the spec and the tmux command instead of
// running it. The launcher script is really written, so a ScriptDir anyone
// else could write to fails here as it would for real.
func openInFakeTmux(spec *termlaunch.Spec, tmux *[]string) func(termlaunch.Spec) (string, error) {
	return func(s termlaunch.Spec) (string, error) {
		*spec = s
		env := termlaunch.Environment{OS: platform.Darwin, LookupEnv: agentEnv(map[string]string{"TMUX": "/tmp/tmux-501/default,1,0"}),
			Run: func(_ context.Context, name string, args ...string) error {
				*tmux = append([]string{name}, args...)
				return nil
			}}
		return termlaunch.Open(context.Background(), s, env)
	}
}

// Run by an agent (no terminal), --to opens a new window, returning at once.
func TestHandoffToOffATerminalOpensANewWindow(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		t.Error("ran the agent without a terminal")
		return nil
	}
	var spec termlaunch.Spec
	var tmux []string
	f.env.OpenTerminal = openInFakeTmux(&spec, &tmux)
	out, errOut, code := runHandoff(t, f.env, f.id, "--to", "claude", "--", "--model", "opus")
	if code != 0 || out != "" || errOut != "handoff: opened claude in a new tmux window\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	handoffFile := filepath.Join(spec.ScriptDir, launchHandoffName)
	wantArgv := []string{"/opt/bin/claude", "--add-dir", spec.ScriptDir, "--model", "opus", "--", spec.Argv[len(spec.Argv)-1]}
	if spec.Dir != f.project || !slices.Equal(spec.Argv, wantArgv) || !slices.Equal(spec.Unset, handoffSessionEnv) {
		t.Fatalf("spec = %+v", spec)
	}
	if filepath.Dir(spec.ScriptDir) != filepath.Join(f.home, handoffDir) || !strings.Contains(spec.Argv[len(spec.Argv)-1], handoffFile) {
		t.Fatalf("script directory %s is not the launch copy's", spec.ScriptDir)
	}
	if data, err := os.ReadFile(handoffFile); err != nil || !strings.Contains(string(data), "Fix the flaky widget test.") {
		t.Fatalf("launch copy: %v", err)
	}
	if len(tmux) != 5 || tmux[0] != "tmux" || tmux[3] != f.project {
		t.Fatalf("tmux command %q", tmux)
	}
}

// --to runs in this terminal only when stdin and stdout both are one. Either
// alone (`codex "$(agent-archive handoff --to ...)"`, input piped in) opens a
// new window, and --here is refused.
func TestHandoffToTerminalCombinations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		stdin  bool
		stdout bool
		here   bool
	}{
		{"both terminals", true, true, true},
		{"stdout piped", true, false, false},
		{"stdin piped", false, true, false},
		{"neither", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newHandoffFixture(t, false)
			f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
			launched, opened := 0, 0
			f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
				launched++
				return nil
			}
			f.env.OpenTerminal = func(termlaunch.Spec) (string, error) {
				opened++
				return "a new tmux window", nil
			}
			stdin := strings.NewReader("")
			var out, errOut bytes.Buffer
			f.env.IsTerminal = func(stream any) bool {
				return (tc.stdin && stream == any(stdin)) || (tc.stdout && stream == any(&out))
			}
			code := Run([]string{"handoff", f.id, "--to", "codex"}, stdin, &out, &errOut, f.env)
			if code != 0 || out.Len() != 0 || strings.Contains(errOut.String(), "Continue in") {
				t.Fatalf("code=%d stdout=%q stderr=%s", code, out.String(), errOut.String())
			}
			if tc.here && (launched != 1 || opened != 0) || !tc.here && (launched != 0 || opened != 1) {
				t.Fatalf("launched here %d, opened %d windows", launched, opened)
			}
			out.Reset()
			errOut.Reset()
			code = Run([]string{"handoff", f.id, "--to", "codex", "--here"}, stdin, &out, &errOut, f.env)
			if tc.here != (code == 0) {
				t.Fatalf("--here: code=%d stderr=%s", code, errOut.String())
			}
		})
	}
}

// Inside an agent the agent's shell may be a pseudo-terminal, but
// interaction is off there: --to opens a new window instead of taking over
// that terminal, nothing asks "Continue in:", and --here is refused naming
// the switch. Each agent's variable does this, and so does the switch alone;
// a value the switch does not accept is reported and nothing is launched.
func TestHandoffInAnAgentOpensANewWindow(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		t.Error("ran in the agent's terminal")
		return nil
	}
	opened := 0
	f.env.OpenTerminal = func(termlaunch.Spec) (string, error) {
		opened++
		return "a new tmux window", nil
	}
	switches := []map[string]string{{envNonInteractive: "1"}}
	for _, key := range agentVariables {
		switches = append(switches, map[string]string{key: "not-registered"})
	}
	for _, vars := range switches {
		f.env.LookupEnv = agentEnv(vars)
		opened = 0
		out, errOut, code := runPicker(t, f.env, "1\n", f.id, "--to", "codex")
		if code != 0 || opened != 1 || out != "" || !strings.Contains(errOut, "opened codex in a new tmux window") {
			t.Fatalf("%v --to: code=%d opened=%d stdout=%q stderr=%s", vars, code, opened, out, errOut)
		}
		out, errOut, code = runPicker(t, f.env, "1\n", f.id)
		if code != 0 || opened != 1 || strings.Contains(out, "Continue in") || !strings.Contains(out, "## Where it left off") {
			t.Fatalf("%v no --to: code=%d opened=%d stderr=%s\n%s", vars, code, opened, errOut, out)
		}
		if _, errOut, code := runPicker(t, f.env, "", f.id, "--to", "codex", "--here"); code != 2 || opened != 1 || !strings.Contains(errOut, envNonInteractive) {
			t.Fatalf("%v --here: code=%d stderr=%s", vars, code, errOut)
		}
	}
	f.env.LookupEnv = agentEnv(map[string]string{envNonInteractive: "ture"})
	if out, errOut, code := runPicker(t, f.env, "1\n", f.id, "--to", "codex"); code != 2 || opened != 1 || out != "" || !strings.Contains(errOut, `AGENT_ARCHIVE_NONINTERACTIVE="ture"`) {
		t.Fatalf("invalid switch: code=%d opened=%d stdout=%q stderr=%s", code, opened, out, errOut)
	}
	// AGENT_ARCHIVE_NONINTERACTIVE=0 gives the terminal back.
	launched := 0
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		launched++
		return nil
	}
	f.env.LookupEnv = agentEnv(map[string]string{"CLAUDE_CODE_SESSION_ID": "not-registered", envNonInteractive: "0"})
	if _, errOut, code := runPicker(t, f.env, "", f.id, "--to", "codex"); code != 0 || launched != 1 || opened != 1 {
		t.Fatalf("switch off: code=%d launched=%d opened=%d stderr=%s", code, launched, opened, errOut)
	}
}

// Before setup the launch copy's private temporary directory is where the
// script goes; it too must satisfy termlaunch.
func TestHandoffToWithoutSetupOpensANewWindow(t *testing.T) {
	t.Parallel()
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	line := `{"type":"user","uuid":"u1","timestamp":"2026-01-02T00:00:00Z","message":{"role":"user","content":"Fix the build."}}` + "\n"
	must(t, os.WriteFile(transcript, []byte(line), 0o600))
	env := testEnv(t, filepath.Join(t.TempDir(), "never-set-up"), time.Now())
	env.LookPath = func(name string) (string, error) { return "/opt/bin/" + name, nil }
	env.Environ = func() []string { return nil }
	env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	env.WorkingDir = func() (string, error) { return filepath.Dir(transcript), nil }
	var spec termlaunch.Spec
	var tmux []string
	env.OpenTerminal = openInFakeTmux(&spec, &tmux)
	_, errOut, code := runHandoff(t, env, "--file", transcript, "--harness", "claude", "--to", "codex")
	if code != 0 || filepath.Dir(spec.ScriptDir) != env.tempDir() || len(tmux) == 0 {
		t.Fatalf("code=%d spec=%+v stderr=%s", code, spec, errOut)
	}
}

// --new-window opens a window even from a terminal.
func TestHandoffNewWindowOnATerminal(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		t.Error("ran in this terminal")
		return nil
	}
	opened := 0
	f.env.OpenTerminal = func(termlaunch.Spec) (string, error) {
		opened++
		return "a new iTerm2 tab", nil
	}
	if _, errOut, code := runPicker(t, f.env, "", f.id, "--to", "codex", "--new-window"); code != 0 || opened != 1 || !strings.Contains(errOut, "opened codex in a new iTerm2 tab") {
		t.Fatalf("--to: code=%d opened=%d stderr=%s", code, opened, errOut)
	}
	// An agent chosen at the prompt too.
	if _, errOut, code := runPicker(t, f.env, "1\n", f.id, "--new-window"); code != 0 || opened != 2 {
		t.Fatalf("prompt: code=%d opened=%d stderr=%s", code, opened, errOut)
	}
	// Print needs no window.
	var pager string
	f.env.RunPager = copyPager(&pager)
	if _, _, code := runPicker(t, f.env, "p\n", f.id, "--new-window"); code != 0 || opened != 2 {
		t.Fatalf("print opened a window: code=%d opened=%d", code, opened)
	}
}

// --here runs in this terminal, which there must be.
func TestHandoffHereAndNewWindowFlags(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	launched := 0
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		launched++
		return nil
	}
	if _, errOut, code := runPicker(t, f.env, "", f.id, "--to", "codex", "--here"); code != 0 || launched != 1 {
		t.Fatalf("--here on a terminal: code=%d stderr=%s", code, errOut)
	}
	for _, tc := range []struct {
		terminal bool
		args     []string
		message  string
	}{
		{false, []string{f.id, "--to", "codex", "--here"}, "--here needs a terminal"},
		{true, []string{f.id, "--to", "codex", "--here", "--new-window"}, "mutually exclusive"},
		{false, []string{f.id, "--new-window"}, "apply to a launched agent"},
		{true, []string{f.id, "--here", "--output", filepath.Join(t.TempDir(), "x.md")}, "apply to a launched agent"},
		{true, []string{f.id, "--new-window", "--format", "json"}, "apply to a launched agent"},
		{true, []string{f.id, "--here", "--no-preamble"}, "apply to a launched agent"},
		{true, []string{f.id, "--new-window", "--no-preamble"}, "apply to a launched agent"},
	} {
		run := runHandoff
		if tc.terminal {
			run = func(t *testing.T, env Env, args ...string) (string, string, int) {
				t.Helper()
				return runPicker(t, env, "", args...)
			}
		}
		if _, errOut, code := run(t, f.env, tc.args...); code != 2 || !strings.Contains(errOut, tc.message) {
			t.Errorf("%v: code=%d stderr=%s", tc.args, code, errOut)
		}
	}
	if launched != 1 {
		t.Fatalf("a rejected command launched")
	}
}

// With no terminal to open, the command fails naming what to run by hand,
// and never runs the agent here.
func TestHandoffWithNoTerminalToOpenNamesTheCommand(t *testing.T) {
	t.Parallel()
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	f.env.LaunchHandoff = func(launchSpec, io.Reader, io.Writer, io.Writer) error {
		t.Error("ran the agent without a terminal")
		return nil
	}
	var spec termlaunch.Spec
	f.env.OpenTerminal = func(s termlaunch.Spec) (string, error) {
		spec = s
		return termlaunch.Open(context.Background(), s, termlaunch.Environment{OS: platform.Linux, LookupEnv: agentEnv(nil),
			Run: func(context.Context, string, ...string) error { return errors.New("ran a command") }})
	}
	_, errOut, code := runHandoff(t, f.env, f.id, "--to", "codex")
	if code != 1 || !strings.Contains(errOut, "no terminal to open a new window in; run it by hand:\n  cd ") ||
		!strings.Contains(errOut, "'/opt/bin/codex' '--cd'") || !strings.Contains(errOut, filepath.Join(spec.ScriptDir, launchHandoffName)) {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
}

func TestHandoffDestinationFlagSpellings(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--here", "--new-window"} {
		if !strings.Contains(commandHelp["handoff"], "  "+flag) {
			t.Errorf("help does not document %s", flag)
		}
	}
}
