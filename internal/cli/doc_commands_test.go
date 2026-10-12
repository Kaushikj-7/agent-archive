package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var (
	// docFence is a fenced code block; docCodeSpan an inline code span.
	docFence    = regexp.MustCompile("(?ms)^[ \t]*(```|~~~)([^\n]*)\n(.*?)\n[ \t]*(```|~~~)")
	docCodeSpan = regexp.MustCompile("`([^`\n]+)`")
	// docInvocation is `agent-archive` and what follows it on the line, up
	// to the end of a shell command.
	docInvocation = regexp.MustCompile(`(?:^|[\s("'$])agent-archive((?:[ \t]+[^\s|;&)<>"'` + "`" + `]+)*)`)
	// shellContinuation is a backslash that continues a shell command on
	// the next line.
	shellContinuation = regexp.MustCompile(`[ \t]*\\\n[ \t]*`)
	// shellComment is a shell comment: a # at the start of a line or after
	// a space, to the end of the line.
	shellComment = regexp.MustCompile(`(^|\s)#.*$`)
	// docCommandWord is what a command (or a placeholder for one) looks
	// like, as opposed to the next word of a sentence.
	docCommandWord = regexp.MustCompile(`^(-{0,2}[a-z_][a-z-]*|[A-Z_]+)$`)
	docFlagName    = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// docCommandSources are the Markdown and issue-template files whose quoted
// commands must exist: README.md, docs/, dev/ except the unimplemented
// proposals, and .github/ISSUE_TEMPLATE.
func docCommandSources(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	files := []string{filepath.Join(root, "README.md")}
	for _, dir := range []string{"docs", "dev", filepath.Join(".github", "ISSUE_TEMPLATE")} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if slash := filepath.ToSlash(path); strings.HasSuffix(slash, "dev/proposals") {
					return filepath.SkipDir
				}
				return nil
			}
			if slices.Contains([]string{".md", ".yml", ".yaml"}, filepath.Ext(path)) {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// quotedCode returns the code a file quotes: every fenced block (but a
// diagram's) and inline code span. An issue template's YAML strings are
// Markdown, so the same applies to them.
func quotedCode(text string) []string {
	var code []string
	for _, m := range docFence.FindAllStringSubmatch(text, -1) {
		if !strings.Contains(m[2], "mermaid") {
			code = append(code, m[3])
		}
	}
	text = docFence.ReplaceAllString(text, "")
	for _, m := range docCodeSpan.FindAllStringSubmatch(text, -1) {
		code = append(code, m[1])
	}
	return code
}

// commandWords splits what follows agent-archive into words, ending at the
// end of a sentence or an aside ("run agent-archive sync." or "agent-archive
// (as recorded)") and dropping a word's trailing punctuation. It returns
// nothing when the first word does not look like a command, as in prose.
func commandWords(s string) []string {
	var words []string
	for word := range strings.FieldsSeq(s) {
		if strings.HasPrefix(word, "(") {
			break
		}
		trimmed := strings.TrimRight(word, ".,:;")
		if trimmed != "" {
			words = append(words, trimmed)
		}
		if trimmed != word {
			break
		}
	}
	if len(words) == 0 || !docCommandWord.MatchString(words[0]) {
		return nil
	}
	return words
}

// flagNameOf returns the flag a quoted word names ("--json", "[--yes]",
// "--since=7d"), or "" when it names none.
func flagNameOf(word string) string {
	word = strings.Trim(word, "[]")
	if !strings.HasPrefix(word, "-") {
		return ""
	}
	name, _, _ := strings.Cut(strings.TrimLeft(word, "-"), "=")
	if !docFlagName.MatchString(name) {
		return ""
	}
	return name
}

// D-39: every `agent-archive COMMAND [SUBCOMMAND] --flag` quoted in the docs,
// the README, and the issue templates names a real command and flags its
// parser accepts, so a renamed or removed flag cannot linger in the docs.
func TestDocsQuoteOnlyRealCommandsAndFlags(t *testing.T) {
	t.Parallel()
	sets := commandFlagSets(t)
	checked := 0
	for _, path := range docCommandSources(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(filepath.Join("..", ".."), path)
		// Specs propose flags that do not exist yet, so only an explicit
		// `agent-archive ...` is checked there; elsewhere a code span such
		// as `list --json` names the command too.
		design := strings.HasPrefix(filepath.ToSlash(rel), "dev/specs/")
		for _, code := range quotedCode(string(data)) {
			problems, n := quotedInvocationProblems(sets, code, !design)
			checked += n
			for _, problem := range problems {
				t.Errorf("%s: %s", rel, problem)
			}
		}
	}
	if checked < 20 {
		t.Fatalf("checked only %d quoted flags; the extraction is broken", checked)
	}
}

// quotedInvocationProblems checks every `agent-archive COMMAND [SUBCOMMAND]
// --flag` in a piece of quoted code (see quotedCode) against the commands and
// flag sets the CLI has, returning what is wrong with each and how many flags
// it checked. With bareCommands, a line that starts with a command and a
// flag, as a code span such as `list --json` does, names the command too.
func quotedInvocationProblems(sets map[string]*flag.FlagSet, code string, bareCommands bool) (problems []string, checked int) {
	// What may follow agent-archive besides a public command: help, the
	// version flags, and the hidden entry points apps and
	// launchd run (_hook takes --harness).
	others := map[string][]string{
		"help": nil, "--help": nil, "-h": nil, "--version": nil, "-v": nil,
		"_collect": nil, "_hook": {"harness"},
		"_catalog": {"bucket", "prefix", "isolated", "owner"},
	}
	code = shellContinuation.ReplaceAllString(code, " ")
	for line := range strings.SplitSeq(code, "\n") {
		line = shellComment.ReplaceAllString(line, "")
		invocations := docInvocation.FindAllStringSubmatch(line, -1)
		if fields := strings.Fields(line); bareCommands && len(invocations) == 0 && len(fields) > 1 && commandHelp[fields[0]] != "" && slices.ContainsFunc(fields[1:], func(w string) bool { return flagNameOf(w) != "" }) {
			invocations = [][]string{{line, " " + line}}
		}
		for _, m := range invocations {
			words := commandWords(m[1])
			if len(words) == 0 {
				continue // agent-archive alone, or in a sentence
			}
			command, rest := words[0], words[1:]
			if command == "COMMAND" {
				continue // a placeholder: any command, any of its flags
			}
			accepted, other := others[command]
			if _, public := commandHelp[command]; !public && !other {
				problems = append(problems, fmt.Sprintf("`agent-archive %s`: no command %q", strings.Join(words, " "), command))
				continue
			}
			if len(rest) > 0 {
				if _, ok := commandHelp[command+" "+rest[0]]; ok {
					command, rest = command+" "+rest[0], rest[1:]
				} else if near := nearSubcommand(command, rest[0]); near != "" {
					problems = append(problems, fmt.Sprintf("`agent-archive %s`: %s has no subcommand %q (%q?)", strings.Join(words, " "), command, rest[0], near))
					continue
				}
			}
			if set, ok := sets[command]; ok {
				accepted = nil
				set.VisitAll(func(f *flag.Flag) { accepted = append(accepted, f.Name) })
			}
			for _, word := range rest {
				if word == "--" {
					break // the rest is for another program (handoff --to)
				}
				name := flagNameOf(word)
				if name == "" {
					continue
				}
				checked++
				if !slices.Contains(append([]string{"help", "h"}, accepted...), name) {
					spelled, _, _ := strings.Cut(strings.Trim(word, "[]"), "=")
					problems = append(problems, fmt.Sprintf("`agent-archive %s`: %s has no flag %s", strings.Join(words, " "), command, spelled))
				}
			}
		}
	}
	return problems, checked
}

// nearSubcommand returns the subcommand of command that word misspells
// (`backfill histroy`, `backfill undoo`), or "" when word is not within two
// edits of one. A word further from every subcommand, or too short to tell
// ("do"), is taken as prose, as in "sessions agent-archive backfill
// imported".
func nearSubcommand(command, word string) string {
	if len(word) < 4 || !docFlagName.MatchString(word) {
		return ""
	}
	for other := range commandHelp {
		sub, ok := strings.CutPrefix(other, command+" ")
		if ok && editDistance(sub, word) <= 2 {
			return sub
		}
	}
	return ""
}

// editDistance is the Levenshtein distance between a and b.
func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current := make([]int, len(b)+1)
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous = current
	}
	return previous[len(b)]
}
