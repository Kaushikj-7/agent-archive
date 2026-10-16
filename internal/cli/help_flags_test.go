package cli

import (
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

// D-33: every flag a command accepts is in its help, each on a line of its
// own where the help lists options; a flag with a default says it; and no
// help line is wider than a terminal's 80 columns. The flag sets are the
// commands' own, caught as each command builds its set, so a flag added
// without help fails here.
func TestHelpDescribesEveryFlag(t *testing.T) {
	t.Parallel()
	var sets []*commandFlags
	env := testEnv(t, t.TempDir(), time.Now())
	env.observeFlags = func(f *commandFlags) { sets = append(sets, f) }
	for command := range commandHelp {
		if command == "version" {
			continue
		}
		sets = nil
		args := append(strings.Fields(command), "--no-such-flag")
		if code := Run(args, nil, io.Discard, io.Discard, env); code != 2 {
			t.Errorf("%s: exit %d for an unknown flag", command, code)
		}
		if len(sets) == 0 {
			t.Errorf("%s: no flag set was built", command)
			continue
		}
		help := commandHelp[command]
		listsOptions := strings.Contains(help, "\n  --")
		sets[0].VisitAll(func(f *flag.Flag) {
			name := "--" + f.Name
			at := strings.Index(help, name)
			if at < 0 {
				t.Errorf("%s: help does not mention %s", command, name)
				return
			}
			if !listsOptions {
				return
			}
			line := strings.Index(help, "\n  "+name+" ")
			if line < 0 {
				line = strings.Index(help, "\n  "+name+"\n")
			}
			if line < 0 {
				t.Errorf("%s: %s has no option line of its own", command, name)
				return
			}
			block := help[line+1:]
			if next := strings.Index(block[1:], "\n  --"); next >= 0 {
				block = block[:next+1]
			}
			if _, isBool := f.Value.(interface{ IsBoolFlag() bool }); !isBool && f.DefValue != "" && !strings.Contains(block, "default") {
				t.Errorf("%s: %s defaults to %q but its help does not say so:\n%s", command, name, f.DefValue, block)
			}
		})
	}
	for command, help := range commandHelp {
		for line := range strings.SplitSeq(help, "\n") {
			if len([]rune(line)) > 80 {
				t.Errorf("%s: help line of %d columns: %q", command, len([]rune(line)), line)
			}
		}
	}
	for name, text := range map[string]string{"usage": usage, "advancedUsage": advancedUsage} {
		for line := range strings.SplitSeq(text, "\n") {
			if len([]rune(line)) > 82 {
				t.Errorf("%s line of %d columns: %q", name, len([]rune(line)), line)
			}
		}
	}
}

// version --help is help, not a usage error; the top-level help links the
// documentation.
func TestVersionHelpAndDocsLink(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"version", "--help"}, {"--version", "-h"}, {"help", "version"}} {
		var out strings.Builder
		if code := Run(args, nil, &out, io.Discard, Env{}); code != 0 || !strings.HasPrefix(out.String(), "Usage: agent-archive version") {
			t.Errorf("%v: exit %d\n%s", args, code, out.String())
		}
	}
	var errOut strings.Builder
	if code := Run([]string{"version", "extra"}, nil, io.Discard, &errOut, Env{}); code != 2 || !strings.Contains(errOut.String(), "version --help") {
		t.Errorf("version extra: exit %d %s", code, errOut.String())
	}
	if !strings.Contains(usage, "Docs: https://github.com/wangjohn/agent-archive/tree/main/docs") {
		t.Error("the top-level help has no link to the docs")
	}
}
