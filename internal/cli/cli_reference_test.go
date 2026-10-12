package cli

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// cliReferencePath is the generated reference, from this package's directory.
var cliReferencePath = filepath.Join("..", "..", "docs", "reference", "cli.md")

// commandFlagSets returns every public command's own flag set ("backfill
// undo" included), caught as the command builds it: each command is run with
// an unknown flag, which it rejects before doing anything else.
func commandFlagSets(t *testing.T) map[string]*flag.FlagSet {
	t.Helper()
	sets := map[string]*flag.FlagSet{}
	env := testEnv(t, t.TempDir(), time.Now())
	for command := range commandHelp {
		if command == "version" {
			continue // takes no flags; it has no flag set
		}
		var built []*commandFlags
		env.observeFlags = func(f *commandFlags) { built = append(built, f) }
		args := append(strings.Fields(command), "--no-such-flag")
		if code := Run(args, nil, io.Discard, io.Discard, env); code != 2 {
			t.Errorf("%s: exit %d for an unknown flag", command, code)
		}
		if len(built) == 0 {
			t.Errorf("%s: no flag set was built", command)
			continue
		}
		sets[command] = built[0].FlagSet
	}
	return sets
}

// referenceCommandOrder is the order commands appear in the top-level help,
// each followed by its subcommands, then version.
func referenceCommandOrder(t *testing.T) []string {
	t.Helper()
	var order []string
	for _, m := range regexp.MustCompile(`(?m)^  agent-archive (\S+)`).FindAllStringSubmatch(usage, -1) {
		order = append(order, m[1])
		var subs []string
		for command := range commandHelp {
			if strings.HasPrefix(command, m[1]+" ") {
				subs = append(subs, command)
			}
		}
		slices.Sort(subs)
		order = append(order, subs...)
	}
	order = append(order, "version")
	for command := range commandHelp {
		if !slices.Contains(order, command) {
			t.Errorf("%s has help but is missing from the top-level help", command)
		}
	}
	return order
}

// commandGuides is the guide that explains each public command with
// examples, linked under its reference entry. A command without one fails
// TestCLIReferenceIsCurrent, so a new command arrives with a guide.
var commandGuides = map[string]string{
	"recover":            "[Recover a blocked transcript](../guides/transcript-recovery.md)",
	"machines revoke":    "[Multiple machines](../guides/multiple-machines.md)",
	"machines own-key":   "[Multiple machines](../guides/multiple-machines.md)",
	"machines":           "[Multiple machines](../guides/multiple-machines.md)",
	"machines add":       "[Multiple machines](../guides/multiple-machines.md)",
	"machines rename":    "[Multiple machines](../guides/multiple-machines.md)",
	"setup":              "[Set up capture](../getting-started/setup.md)",
	"status":             "[Reading status](../guides/troubleshooting.md#reading-status); `--json` fields in [JSON output](json-output.md)",
	"sync":               "[Everyday commands](../guides/troubleshooting.md#everyday-commands)",
	"pause":              "[Everyday commands](../guides/troubleshooting.md#everyday-commands)",
	"resume":             "[Everyday commands](../guides/troubleshooting.md#everyday-commands)",
	"list":               "[Inspect the archive](../guides/list-and-show.md); `--json` in [JSON output](json-output.md)",
	"show":               "[Inspect the archive](../guides/list-and-show.md)",
	"stats":              "[See your usage](../guides/stats.md); `--json` in [JSON output](json-output.md)",
	"feedback":           "[Feedback](../guides/list-and-show.md#feedback)",
	"historical-project": "[Import existing sessions](../guides/backfill.md#historical-only-projects)",
	"backfill":           "[Import existing sessions](../guides/backfill.md)",
	"backfill history":   "[Import existing sessions](../guides/backfill.md)",
	"backfill undo":      "[Undo an import](../guides/backfill.md#undo)",
	"handoff":            "[Continue a session in another agent](../guides/handoff.md)",
	"uninstall":          "[Uninstall](../getting-started/uninstall.md)",
	"eval":               "[Export sessions for evaluation](../guides/eval-export.md); records in [eval export schema](../../schemas/eval-export.schema.json)",
	"eval export":        "[Export sessions for evaluation](../guides/eval-export.md); records in [eval export schema](../../schemas/eval-export.schema.json)",
	"purge":              "[Privacy cleanup](../security/privacy.md#after-a-filter-upgrade)",
	"purge plan":         "[Privacy cleanup](../security/privacy.md#after-a-filter-upgrade)",
	"purge apply":        "[Privacy cleanup](../security/privacy.md#after-a-filter-upgrade)",
	"version":            "[Install](../getting-started/install.md)",
}

// renderCLIReference renders docs/reference/cli.md from the help each
// command prints and the flags each command's flag set defines.
func renderCLIReference(t *testing.T) []byte {
	t.Helper()
	sets := commandFlagSets(t)
	var b strings.Builder
	b.WriteString(`# CLI reference

<!-- Generated from the commands' own help and flag sets. Do not edit by
     hand; after changing a command's help or flags, regenerate it with
     go test ./internal/cli -run TestCLIReferenceIsCurrent -update -->

Every public command of ` + "`agent-archive`" + `: its help, exactly as
` + "`agent-archive help COMMAND`" + ` (or ` + "`agent-archive COMMAND --help`" + `) prints it,
the flags its parser accepts, and a link to the guide that walks through it.
New to agent-archive? Start with [install](../getting-started/install.md) and
[setup](../getting-started/setup.md); the [glossary](glossary.md) explains
terms such as capture, collector, and retention.

Every command also accepts ` + "`--help`" + ` and ` + "`-h`" + `. Help never activates
hooks, reads credentials, or changes state. The hidden commands ` + "`_hook`" + ` and
` + "`_collect`" + ` (what app hooks and the LaunchAgent run) are not part of the
interface and are left out.

This page is generated from the CLI itself, and ` + "`go test ./...`" + ` fails when it
is stale. After changing a command's help or flags, regenerate it with
` + "`go test ./internal/cli -run TestCLIReferenceIsCurrent -update`" + ` (see
[fixtures and goldens](../../dev/contributing/testing.md#fixtures-and-goldens)).

## Commands

` + "```text\n" + usage + "```\n")
	b.WriteString(`
## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success, and help. |
| 1 | An operational failure. What a command was asked for goes to stdout; why it did not do it, or not all of it, goes to stderr. A ` + "`sync`" + ` that is paused, finds another command running, or fails for some sessions exits 1; its summary line, if it ran, stays on stdout. |
| 2 | A usage error: an unknown command or flag, a bad flag value, or an unexpected argument. It is reported in one line on stderr before the command does anything. |
| 128 + signal | A command stopped by a signal exits with the shell's status for it: Ctrl-C (130), SIGHUP (129), SIGQUIT (131), or SIGTERM (143). ` + "`backfill`" + ` stops at once on a second Ctrl-C; ` + "`list`, `show` and `stats`" + ` restore the terminal first. |
`)
	for _, command := range referenceCommandOrder(t) {
		guide, ok := commandGuides[command]
		if !ok {
			t.Errorf("%s has no guide in commandGuides", command)
		}
		fmt.Fprintf(&b, "\n## agent-archive %s\n\nGuide: %s.\n\n```text\n%s```\n", command, guide, commandHelp[command])
		set, ok := sets[command]
		if !ok {
			continue
		}
		var rows []string
		set.VisitAll(func(f *flag.Flag) {
			value := "a value"
			if _, repeatable := f.Value.(*stringList); repeatable {
				value = "a value; repeatable"
			}
			if bf, isBool := f.Value.(interface{ IsBoolFlag() bool }); isBool && bf.IsBoolFlag() {
				value = "no value"
			}
			def := "—"
			if value != "no value" && f.DefValue != "" {
				def = "`" + f.DefValue + "`"
			}
			rows = append(rows, fmt.Sprintf("| `--%s` | %s | %s |", f.Name, value, def))
		})
		if len(rows) == 0 {
			b.WriteString("\nNo flags.\n")
			continue
		}
		b.WriteString("\n| Flag | Takes | Default |\n| --- | --- | --- |\n")
		b.WriteString(strings.Join(rows, "\n") + "\n")
	}
	return []byte(b.String())
}

// D-32: docs/reference/cli.md is generated from the commands' help and flag
// sets, and this fails when it is stale. Regenerate with
// `go test ./internal/cli -run TestCLIReferenceIsCurrent -update`.
func TestCLIReferenceIsCurrent(t *testing.T) {
	t.Parallel()
	golden.Check(t, cliReferencePath, renderCLIReference(t))
}
