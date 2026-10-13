package cli

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Every help names its own command on its Usage line, uses SESSION_ID (and
// IMPORT_ID) as the only ID placeholders, and has no undocumented option
// lines: each "--flag VALUE" line has a description.
func TestHelpTextIsConsistent(t *testing.T) {
	t.Parallel()
	stale := regexp.MustCompile(`\bID\b|<archive-session-id>|<id>`)
	for cmd, help := range commandHelp {
		if !strings.HasPrefix(help, "Usage: agent-archive "+cmd) {
			t.Errorf("%s: usage line %q", cmd, strings.SplitN(help, "\n", 2)[0])
		}
		if m := stale.FindString(help); m != "" {
			t.Errorf("%s: placeholder %q; use SESSION_ID or IMPORT_ID", cmd, m)
		}
		lines := strings.Split(help, "\n")
		for i, line := range lines {
			option := strings.HasPrefix(line, "  --")
			described := len(strings.Fields(strings.TrimSpace(line))) > 2 || (i+1 < len(lines) && strings.HasPrefix(lines[i+1], strings.Repeat(" ", 20)))
			if option && !described {
				t.Errorf("%s: option without a description: %q", cmd, line)
			}
		}
	}
	for _, placeholder := range []string{"agent-archive show ID", "agent-archive feedback ID"} {
		if strings.Contains(usage, placeholder) {
			t.Errorf("usage still says %q", placeholder)
		}
	}
}

// Backfill subcommands have help of their own, whichever way
// it is asked for.
func TestSubcommandHelp(t *testing.T) {
	t.Parallel()
	for _, sub := range []string{"undo", "history", "projects"} {
		for _, args := range [][]string{{"backfill", sub, "--help"}, {"help", "backfill", sub}, {"backfill", sub, "-h"}} {
			var out bytes.Buffer
			if code := Run(args, nil, &out, nil, Env{}); code != 0 || !strings.HasPrefix(out.String(), "Usage: agent-archive backfill "+sub) {
				t.Errorf("%v: exit %d\n%s", args, code, &out)
			}
		}
	}
}

func TestEveryPublicHelpIsReadOnly(t *testing.T) {
	t.Parallel()
	env := Env{Home: func() (string, error) { t.Fatal("help accessed runtime"); return "", nil }}
	for cmd := range commandHelp {
		name := strings.Fields(cmd)
		with := func(before, after string) []string {
			args := append([]string{}, before)
			args = append(args, name...)
			return append(args, after)
		}
		for _, args := range [][]string{with("", "--help")[1:], with("help", "")[:len(name)+1], with("", "-h")[1:], with("", "-help")[1:]} {
			var out, errOut bytes.Buffer
			if code := Run(args, nil, &out, &errOut, env); code != 0 || !strings.Contains(out.String(), "Usage:") {
				t.Fatalf("%v: %d %s %s", args, code, &out, &errOut)
			}
		}
	}
	for _, args := range [][]string{{"pause", "--unknown"}, {"resume", "oops"}, {"setup", "--json"}, {"status", "extra"}, {"uninstall", "--force"}, {"sync", "extra"}, {"list", "--bad"}, {"show", "id", "--bad"}} {
		if code := Run(args, nil, nil, nil, env); code != 2 {
			t.Fatalf("%v code %d", args, code)
		}
	}
}

// Each command's own flag set decides its arguments, and every command
// reports a bad one the same way: one line on stderr naming the problem
// and the command's help, exit 2, and never the flag package's usage dump.
func TestCommandsRejectUnknownArguments(t *testing.T) {
	t.Parallel()
	env := testEnv(t, t.TempDir(), time.Now())
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"status", "--jsn"}, "agent-archive: status: unknown flag --jsn; run agent-archive status --help"},
		{[]string{"sync", "--json"}, "agent-archive: sync: unknown flag --json; run agent-archive sync --help"},
		{[]string{"pause", "now"}, `agent-archive: pause: unexpected argument "now"; run agent-archive pause --help`},
		{[]string{"resume", "-x"}, "agent-archive: resume: unknown flag --x; run agent-archive resume --help"},
		{[]string{"setup", "--bukcet", "b"}, "agent-archive: setup: unknown flag --bukcet; run agent-archive setup --help"},
		{[]string{"setup", "extra"}, `agent-archive: setup: unexpected argument "extra"; run agent-archive setup --help`},
		{[]string{"uninstall", "--delete-local"}, "agent-archive: uninstall: unknown flag --delete-local; run agent-archive uninstall --help"},
		{[]string{"uninstall", "--yes", "extra"}, `agent-archive: uninstall: unexpected argument "extra"; run agent-archive uninstall --help`},
		{[]string{"list", "--bogus"}, "agent-archive: list: unknown flag --bogus; run agent-archive list --help"},
		{[]string{"list", "--since"}, "agent-archive: list: --since needs a value; run agent-archive list --help"},
		{[]string{"list", "--complete=maybe"}, `agent-archive: list: invalid value "maybe" for --complete: parse error; run agent-archive list --help`},
		{[]string{"show", "--bogus"}, "agent-archive: show: unknown flag --bogus; run agent-archive show --help"},
		{[]string{"show"}, "agent-archive: show: a SESSION_ID is required (see agent-archive list); run agent-archive show --help"},
		{[]string{"show", "a", "b"}, `agent-archive: show: unexpected argument "b"; run agent-archive show --help`},
		{[]string{"handoff", "--latest", "--bogus"}, "agent-archive: handoff: unknown flag --bogus; run agent-archive handoff --help"},
		{[]string{"handoff", "--max-bytes", "x", "--latest"}, `agent-archive: handoff: invalid value "x" for --max-bytes: parse error; run agent-archive handoff --help`},
		{[]string{"feedback", "SESSION", "--bogus"}, "agent-archive: feedback: unknown flag --bogus; run agent-archive feedback --help"},
		{[]string{"backfill", "--bogus"}, "agent-archive: backfill: unknown flag --bogus; run agent-archive backfill --help"},
		{[]string{"backfill", "--since", "yesterday"}, `agent-archive: backfill: --since: "yesterday" is not a date (2026-01-31), an RFC 3339 time, or an age (7d, 12h); run agent-archive backfill --help`},
		{[]string{"backfill", "projects", "--bogus"}, "agent-archive: backfill projects: unknown flag --bogus; run agent-archive backfill projects --help"},
		{[]string{"backfill", "undo", "--bogus"}, "agent-archive: backfill undo: unknown flag --bogus; run agent-archive backfill undo --help"},
		{[]string{"status", "extra"}, `agent-archive: status: unknown app "extra"; choose one of codex, claude, cursor; run agent-archive status --help`},
		{[]string{"status", "claude", "extra"}, `agent-archive: status: unexpected argument "extra"; run agent-archive status --help`},
		{[]string{"backfill", "history", "extra"}, `agent-archive: backfill history: unexpected argument "extra"; run agent-archive backfill history --help`},
	} {
		var out, errOut bytes.Buffer
		code := Run(tc.args, nil, &out, &errOut, env)
		if code != 2 || errOut.String() != tc.want+"\n" || out.Len() != 0 {
			t.Errorf("%v: exit %d\nstderr=%q\n  want=%q", tc.args, code, errOut.String(), tc.want+"\n")
		}
	}
}
