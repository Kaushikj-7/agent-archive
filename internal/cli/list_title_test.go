package cli

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// TestListTitle covers listTitle on titles seen in real session lists and on
// the edge cases its rules must leave alone.
func TestListTitle(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "question with quoted phrase",
			in:   "help me understand what \"Waiting for native history or own-task evidence\" means and whether or not it's possible to import this …",
			want: "Help me understand what \"Waiting for native history or own-task evidence\" means and whether or not it's possible to import this …",
		},
		{
			name: "first sentence only",
			in:   "when i run backfill, I should have a better loading indicator to see how far along I am with things. Right now, it just shows a …",
			want: "When i run backfill, I should have a better loading indicator to see how far along I am with things",
		},
		{
			name: "pasted files with a quoted name",
			in:   "# Files pasted by the user: ## \"agent-archive · 72 sessions · a all projects # TITLE PR WHEN HARNESS ID 1 <exte…\": /Users/wangjo…",
			want: "Pasted files: agent-archive",
		},
		{name: "pasted files with a path", in: "Files pasted by the user: /Users/me/src/list_table.go: …", want: "Pasted files: list_table.go"},
		{name: "pasted files with no name", in: "# Files pasted by the user:", want: "Pasted files"},
		{
			name: "code review of a hash range",
			in:   "/code-review d3e525605855086e5f0f2acf1115dd6df0d6fbf6...351890220f3581bb8478807c22c7a54f855afa7b",
			want: "Code review d3e5256…",
		},
		{name: "code review of one hash", in: "/code-review d3e525605855086e5f0f2acf1115dd6df0d6fbf6", want: "Code review d3e5256…"},
		{name: "plugin command with a hash", in: "/review-kit:review_pr 351890220f3581bb8478807c22c7a54f855afa7b and more", want: "Review pr 3518902… and more"},
		{name: "slash command without a hash", in: "/code-review 533", want: "/code-review 533"},
		{name: "slash command with a short hash", in: "/handoff fix d3e5256", want: "/handoff fix d3e5256"},
		{name: "plain request", in: "yes please go ahead and publish the PR", want: "Yes please go ahead and publish the PR"},
		{name: "already capitalized", in: "Review historical project command", want: "Review historical project command"},
		{
			name: "curly apostrophe",
			in:   "Let’s do a performance review of the cli as a senior staff engineer, especially the key things…",
			want: "Let’s do a performance review of the cli as a senior staff engineer, especially the key things…",
		},
		{name: "short request", in: "please create a PR", want: "Please create a PR"},
		{name: "empty", in: "", want: ""},
		{name: "spaces", in: "   ", want: "   "},
		{name: "only punctuation", in: "...", want: "..."},
		{name: "only quotes", in: `""`, want: `""`},
		{name: "version inside the first sentence", in: "v0.2.1 release notes. Next", want: "v0.2.1 release notes"},
		{name: "long version sentence", in: "ship the v0.2.1 release notes. Next we tag it", want: "Ship the v0.2.1 release notes"},
		{name: "replay mark kept", in: "[replay] foo", want: "[replay] Foo"},
		{name: "replay mark with markup", in: "[replay] # \"fix the build\"", want: "[replay] Fix the build"},
		{name: "replay mark alone", in: "[replay] ", want: "[replay]"},
		{name: "non-ASCII first letter", in: "élan vital for the picker", want: "Élan vital for the picker"},
		{name: "cyrillic first letter", in: "привет мир", want: "Привет мир"},
		{name: "uncased script", in: "修复 列表", want: "修复 列表"},
		{name: "short ID", in: "8f3a2c1d", want: "8f3a2c1d"},
		{name: "short ID of letters", in: "abcdefab", want: "abcdefab"},
		{name: "hex word", in: "add a test", want: "Add a test"},
		{name: "markdown heading", in: "## fix the scroll bug", want: "Fix the scroll bug"},
		{name: "list bullet", in: "- update the docs", want: "Update the docs"},
		{name: "wrapping quotes", in: "\"publish the PR\"", want: "Publish the PR"},
		{name: "wrapping backticks", in: "`go test ./...`", want: "Go test ./..."},
		{name: "unbalanced opening quote", in: "\"publish the PR …", want: "Publish the PR …"},
		{name: "quote inside kept", in: "\"a\" and \"b\" differ", want: "\"a\" and \"b\" differ"},
		{name: "abbreviation", in: "add flags, e.g. --json and --all to it", want: "Add flags, e.g. --json and --all to it"},
		{name: "ellipsis", in: "so the list... it wraps badly", want: "So the list... it wraps badly"},
		{name: "file name and URL", in: "see list_table.go and https://example.com/a.b for it", want: "See list_table.go and https://example.com/a.b for it"},
		{name: "question kept", in: "can you fix the picker? it wraps", want: "Can you fix the picker?"},
		{name: "short first sentence kept whole", in: "ok. please create a PR", want: "Ok. please create a PR"},
		{name: "camel case left alone", in: "iPhone build fails", want: "iPhone build fails"},
		{name: "path left alone", in: "/Users/me/notes.md says what", want: "/Users/me/notes.md says what"},
		{name: "first line only", in: "fix it\nwith more", want: "Fix it"},
		{name: "abbreviation after a bracket", in: "add flags (e.g. --json) to the list command", want: "Add flags (e.g. --json) to the list command"},
		{name: "dotted initialism", in: "fix the bug in the U.S. version please", want: "Fix the bug in the U.S. version please"},
		{name: "final period dropped", in: "Fix the bug in the picker.", want: "Fix the bug in the picker"},
		{name: "final period after a short first sentence", in: "ok. please create a PR.", want: "Ok. please create a PR"},
		{name: "final ellipsis kept", in: "so the list wraps...", want: "So the list wraps..."},
		{name: "final abbreviation kept", in: "add flags to it, e.g.", want: "Add flags to it, e.g."},
		{name: "lone period", in: ".", want: "."},
		{name: "ordered list number", in: "1. fix the bug in the picker", want: "Fix the bug in the picker"},
		{name: "ordered list parenthesis", in: "2) update the docs", want: "Update the docs"},
		{name: "year is not a list number", in: "2024. was a year of releases", want: "2024. was a year of releases"},
		{name: "slash command with a long number", in: "/code-review 1728912345678", want: "/code-review 1728912345678"},
		{name: "slash command with a letters-only hex run", in: "/code-review deadbeefcafebabe", want: "/code-review deadbeefcafebabe"},
		{name: "longer short ID", in: "abcdefab-", want: "abcdefab-"},
		{name: "abbreviation from the list", in: "compare a vs. b in the list output", want: "Compare a vs. b in the list output"},
		{name: "quotes at both ends that are not a pair", in: "\"foo\" or \"bar\"", want: "\"foo\" or \"bar\""},
		{name: "nested markup", in: "> - \"quoted bullet item\"", want: "Quoted bullet item"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := listTitle(tc.in)
			if got != tc.want {
				t.Errorf("listTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := listTitle(got); again != got {
				t.Errorf("listTitle(%q) = %q, not a fixed point of %q", got, again, tc.in)
			}
		})
	}
}

// FuzzListTitle checks that listTitle, on any display line, returns valid
// UTF-8 with no control or invisible characters, is empty only for empty
// input, and is a fixed point: cleaning a cleaned title changes nothing.
func FuzzListTitle(f *testing.F) {
	for _, seed := range []string{
		"", "...", "please create a PR", "[replay] foo", "v0.2.1 release notes. Next",
		"# Files pasted by the user: ## \"agent-archive · 72 sessions\": /Users/wangjo…",
		"/code-review d3e525605855086e5f0f2acf1115dd6df0d6fbf6...351890220f3581bb8478807c22c7a54f855afa7b",
		"\"`'“‘", "élan", "a\u200bb", "\x1b]52;c;x\x07", "\xff\xfe", "ǆungla", "e.g. x. y z",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		in := archive.DisplayLine(raw)
		got := listTitle(in)
		if in != "" && got == "" {
			t.Fatalf("listTitle(%q) is empty", in)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("listTitle(%q) = %q, invalid UTF-8", in, got)
		}
		if utf8.ValidString(in) && !strings.ContainsRune(in, utf8.RuneError) && strings.ContainsRune(got, utf8.RuneError) {
			t.Fatalf("listTitle(%q) = %q, a replacement character appeared", in, got)
		}
		for _, r := range got {
			if invisibleOrControl(r) || r == '\n' {
				t.Fatalf("listTitle(%q) = %q, %U survived", in, got, r)
			}
		}
		if again := listTitle(got); again != got {
			t.Fatalf("listTitle(%q) = %q, then %q", in, got, again)
		}
	})
}
