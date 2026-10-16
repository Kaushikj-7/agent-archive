package archive

import "testing"

func TestOpenPageContextStrippingPreservesHumanText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"<external_codex_apps_open_page>Page data</external_codex_apps_open_page>", ""},
		{"<external_codex_apps_open_page>Page data</external_codex_apps_open_page>Summarize this page.", "Summarize this page."},
		{"Keep this <external_codex_apps_open_page>outer<external_codex_apps_open_page>inner</external_codex_apps_open_page>tail</external_codex_apps_open_page> request", "Keep this <external_codex_apps_open_page>outer<external_codex_apps_open_page>inner</external_codex_apps_open_page>tail</external_codex_apps_open_page> request"},
		{"<external_codex_apps_open_page>unfinished", ""},
		{"Why does `<external_codex_apps_open_page>...</external_codex_apps_open_page>` appear?", "Why does `<external_codex_apps_open_page>...</external_codex_apps_open_page>` appear?"},
		{"```xml\n<external_codex_apps_open_page>example</external_codex_apps_open_page>\n```", "```xml\n<external_codex_apps_open_page>example</external_codex_apps_open_page>\n```"},
		{"<external_codex_apps_open_page-example>Keep this</external_codex_apps_open_page-example>", "<external_codex_apps_open_page-example>Keep this</external_codex_apps_open_page-example>"},
		{"    <external_codex_apps_open_page>example</external_codex_apps_open_page>", "    <external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"<external_codex_apps_open_page>Context</external_codex_apps_open_page>\n    <external_codex_apps_open_page>example</external_codex_apps_open_page>", "\n    <external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"\t<external_codex_apps_open_page>example</external_codex_apps_open_page>", "\t<external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"    <external_codex_apps_open_page>example</external_codex_apps_open_page>\n<system-reminder>Injected</system-reminder>", "    <external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"Explain external_codex_apps_open_page in an example.", "Explain external_codex_apps_open_page in an example."},
	} {
		_, got := stripInjectedInstructions(tc.input)
		if got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}

func TestOpenPageContextPreservesIndentedTextAfterWhitespaceBlankLines(t *testing.T) {
	t.Parallel()
	for _, blank := range []string{" \n", "\t\n", " \t\r\n \n", " \r", "\r"} {
		for _, indent := range []string{"    ", "\t"} {
			quoted := blank + indent + "<external_codex_apps_open_page>example</external_codex_apps_open_page>"
			for _, prefix := range []string{"", "<system-reminder>Injected</system-reminder>", "<external_codex_apps_open_page>Context</external_codex_apps_open_page>", "<system-reminder>Injected</system-reminder><external_codex_apps_open_page>Context</external_codex_apps_open_page>"} {
				state := PrivacyState{AddGap: func(string, int, string) {}}
				got, keep := SanitizeValue(prefix+quoted, &state)
				if !keep || got != quoted {
					t.Fatalf("sanitize %q: got %q, keep %v; want %q", prefix+quoted, got, keep, quoted)
				}
				again, keep := SanitizeValue(got, &state)
				if !keep || again != quoted {
					t.Fatalf("refilter: got %q, keep %v; want %q", again, keep, quoted)
				}
			}
		}
	}
}

func TestCodexProjectInstructionsAreStrippedOnlyAsALeadingWrapper(t *testing.T) {
	t.Parallel()
	wrapper := "# AGENTS.md instructions for /work/widget\n\n<INSTRUCTIONS>\n# Widget\n\nKeep functions small.\n</INSTRUCTIONS>"
	for _, tc := range []struct {
		input string
		want  string
	}{
		{wrapper, ""},
		{"\n" + wrapper + "\n\nReview the parser.", "Review the parser."},
		{"# AGENTS.md instructions for /work/widget\r\n\r\n<INSTRUCTIONS>\r\nrules\r\n</INSTRUCTIONS>", ""},
		{"# AGENTS.md instructions for /w\n\n<INSTRUCTIONS>\nouter <INSTRUCTIONS>inner</INSTRUCTIONS> tail\n</INSTRUCTIONS>after", "after"},
		{"# AGENTS.md instructions for /w\n\n<INSTRUCTIONS>\nunfinished", ""},
		{wrapper + "<environment_context>\n<cwd>/w</cwd>\n</environment_context>", ""},
		{"Why does Codex send " + wrapper + "?", "Why does Codex send " + wrapper + "?"},
		{"# AGENTS.md instructions for /w\nKeep this heading as prose.", "# AGENTS.md instructions for /w\nKeep this heading as prose."},
		{"    " + wrapper, "    " + wrapper},
		{"<INSTRUCTIONS>Typed by a person.</INSTRUCTIONS>", "<INSTRUCTIONS>Typed by a person.</INSTRUCTIONS>"},
	} {
		_, got := stripInjectedInstructions(tc.input)
		if got != tc.want {
			t.Fatalf("strip %q: got %q want %q", tc.input, got, tc.want)
		}
		state := PrivacyState{AddGap: func(string, int, string) {}}
		if again, _ := SanitizeValue(got, &state); got != "" && again != got {
			t.Fatalf("refilter %q: got %q", got, again)
		}
	}
}
