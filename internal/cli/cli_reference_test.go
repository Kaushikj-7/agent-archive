package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
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

// usageTopLevelCommands is the top-level commands one help tier lists, in
// the order it lists them.
func usageTopLevelCommands(text string) []string {
	var commands []string
	for _, m := range regexp.MustCompile(`(?m)^  agent-archive (\S+)`).FindAllStringSubmatch(text, -1) {
		commands = append(commands, m[1])
	}
	return commands
}

// helpTierCommands returns the top-level commands of both help tiers in
// display order (standard tier first, then advanced), failing the test
// unless every public command — each commandHelp top level and each
// user-facing command Run dispatches, except the version pseudo-command,
// which the standard tier names only in its footer — appears in exactly
// one tier.
func helpTierCommands(t *testing.T) []string {
	t.Helper()
	standard := usageTopLevelCommands(usage)
	advanced := usageTopLevelCommands(advancedUsage)
	seen := map[string]int{}
	for _, command := range append(append([]string{}, standard...), advanced...) {
		seen[command]++
		if seen[command] > 1 {
			t.Errorf("%s appears more than once across the help tiers (standard %v, advanced %v)", command, standard, advanced)
		}
		if _, ok := commandHelp[command]; !ok {
			t.Errorf("%s is listed in a help tier but has no per-command help", command)
		}
	}
	for command := range commandHelp {
		top := strings.SplitN(command, " ", 2)[0]
		if top == "version" {
			continue
		}
		if seen[top] != 1 {
			t.Errorf("%s (top level of %s) appears in %d help tiers, want exactly 1 (standard %v, advanced %v)", top, command, seen[top], standard, advanced)
		}
	}
	// commandHelp alone does not prove coverage: a command Run dispatches
	// without a commandHelp entry would sit in neither tier unnoticed, so
	// check the dispatch switch itself (hidden _commands and the
	// help/version spellings excluded).
	for _, command := range dispatchedCommands(t) {
		if seen[command] != 1 {
			t.Errorf("dispatched command %s appears in %d help tiers, want exactly 1 (standard %v, advanced %v)", command, seen[command], standard, advanced)
		}
	}
	return append(standard, advanced...)
}

// dispatchedCommands is every user-facing command name in Run's dispatch
// switch in cli.go, parsed from the source because the switch is not
// enumerable at run time.
func dispatchedCommands(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	start := strings.Index(text, "switch args[0] {")
	if start < 0 {
		t.Fatal("cli.go: Run dispatch switch not found")
	}
	text = text[start:]
	if end := strings.Index(text, "\tdefault:"); end >= 0 {
		text = text[:end]
	}
	var commands []string
	for _, line := range regexp.MustCompile(`(?m)^\tcase (.+):$`).FindAllStringSubmatch(text, -1) {
		for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(line[1], -1) {
			name := m[1]
			if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "_") || name == "help" || name == "version" {
				continue
			}
			commands = append(commands, name)
		}
	}
	if len(commands) == 0 {
		t.Fatal("cli.go: no dispatched commands parsed from the switch")
	}
	return commands
}

// referenceTopLevelOrder is the order the per-command sections appear in
// the reference. It is the order the single top-level help listed the
// commands before it split into two tiers, kept so the split moves no
// section; a new command is added here as it lands in a tier.
var referenceTopLevelOrder = []string{
	"setup", "machines", "status", "sync", "pause", "resume", "recover",
	"list", "show", "stats", "feedback", "backfill", "migrate", "handoff",
	"eval", "uninstall", "purge",
}

// referenceCommandOrder is referenceTopLevelOrder, each command followed
// by its subcommands, then version.
func referenceCommandOrder(t *testing.T) []string {
	t.Helper()
	tiers := helpTierCommands(t)
	for _, name := range referenceTopLevelOrder {
		if !slices.Contains(tiers, name) {
			t.Errorf("%s is in referenceTopLevelOrder but in no help tier", name)
		}
	}
	if len(tiers) != len(referenceTopLevelOrder) {
		t.Errorf("help tiers list %v; referenceTopLevelOrder must name the same commands", tiers)
	}
	var order []string
	for _, name := range referenceTopLevelOrder {
		order = append(order, name)
		var subs []string
		for command := range commandHelp {
			if strings.HasPrefix(command, name+" ") {
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
	"recover":           "[Recover a blocked transcript](../guides/transcript-recovery.md)",
	"machines revoke":   "[Multiple machines](../guides/multiple-machines.md)",
	"machines own-key":  "[Multiple machines](../guides/multiple-machines.md)",
	"machines":          "[Multiple machines](../guides/multiple-machines.md)",
	"machines add":      "[Multiple machines](../guides/multiple-machines.md)",
	"machines rename":   "[Multiple machines](../guides/multiple-machines.md)",
	"setup":             "[Set up capture](../getting-started/setup.md)",
	"status":            "[Reading status](../guides/troubleshooting.md#reading-status); `--json` fields in [JSON output](json-output.md)",
	"sync":              "[Everyday commands](../guides/troubleshooting.md#everyday-commands)",
	"pause":             "[Everyday commands](../guides/troubleshooting.md#everyday-commands)",
	"resume":            "[Everyday commands](../guides/troubleshooting.md#everyday-commands)",
	"list":              "[Inspect the archive](../guides/list-and-show.md); `--json` in [JSON output](json-output.md)",
	"show":              "[Inspect the archive](../guides/list-and-show.md)",
	"stats":             "[See your usage](../guides/stats.md); `--json` in [JSON output](json-output.md)",
	"feedback":          "[Feedback](../guides/list-and-show.md#feedback)",
	"backfill projects": "[Import existing sessions](../guides/backfill.md#historical-only-projects)",
	"backfill":          "[Import existing sessions](../guides/backfill.md)",
	"backfill history":  "[Import existing sessions](../guides/backfill.md)",
	"backfill undo":     "[Undo an import](../guides/backfill.md#undo)",
	"handoff":           "[Continue a session in another agent](../guides/handoff.md)",
	"uninstall":         "[Uninstall](../getting-started/uninstall.md)",
	"eval":              "[Export sessions for evaluation](../guides/eval-export.md); records in [eval export schema](../../schemas/eval-export.schema.json)",
	"eval export":       "[Export sessions for evaluation](../guides/eval-export.md); records in [eval export schema](../../schemas/eval-export.schema.json)",
	"purge":             "[Privacy cleanup](../security/privacy.md#after-a-filter-upgrade)",
	"purge plan":        "[Privacy cleanup](../security/privacy.md#after-a-filter-upgrade)",
	"purge apply":       "[Privacy cleanup](../security/privacy.md#after-a-filter-upgrade)",
	"version":           "[Install](../getting-started/install.md)",
	"migrate":           "[Migrate the archive](../guides/catalog-migration.md)",
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

The standard tier (` + "`agent-archive --help`" + `):

` + "```text\n" + usage + "```\n" + `
The advanced tier (` + "`agent-archive help advanced`" + `):

` + "```text\n" + advancedUsage + "```\n")
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

// The two help tiers are display only: `help advanced` (and the --help/-h
// spellings) prints the advanced tier without touching the environment,
// the standard tier points at it, per-command help for an advanced command
// still answers, and an unknown command prints only the standard tier.
func TestHelpAdvancedTier(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"help", "advanced"}, {"--help", "advanced"}, {"-h", "advanced"}} {
		var out strings.Builder
		if code := Run(args, nil, &out, io.Discard, Env{}); code != 0 || out.String() != advancedUsage {
			t.Errorf("%v: exit %d, output is not exactly advancedUsage:\n%s", args, code, out.String())
		}
	}
	if !strings.Contains(usage, "agent-archive help advanced") {
		t.Error("standard usage does not point at help advanced")
	}
	var out strings.Builder
	if code := Run([]string{"help", "eval"}, nil, &out, io.Discard, Env{}); code != 0 || out.String() != commandHelp["eval"] {
		t.Errorf("help eval: exit %d\n%s", code, out.String())
	}
	var errOut strings.Builder
	if code := Run([]string{"bogus"}, nil, io.Discard, &errOut, Env{}); code != 2 {
		t.Fatalf("bogus: exit %d", code)
	}
	if !strings.Contains(errOut.String(), usage) || strings.Contains(errOut.String(), "advanced commands") {
		t.Errorf("unknown command must print the standard usage only:\n%s", errOut.String())
	}
}
