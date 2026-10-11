package nativecodec

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

const codexNullOpenPage = `<external_codex_apps_open_page>{"page_id":null}</external_codex_apps_open_page>`

// legacyCodexBundle is a Codex source as filter 15 retained it: a user
// record's text still holds the leading open-page wrapper that filter 16
// drops. A metadata refresh re-derives from these retained bytes.
func legacyCodexBundle(t *testing.T, userContents ...any) SourceBundle {
	t.Helper()
	now := time.Date(2026, 10, 5, 19, 15, 7, 0, time.UTC)
	records := []map[string]any{{"type": "session_meta", "timestamp": now.Format(time.RFC3339), "payload": map[string]any{"id": "n", "cwd": "/p"}}}
	for i, content := range userContents {
		records = append(records, map[string]any{"type": "response_item", "timestamp": now.Add(time.Duration(i+1) * time.Second).Format(time.RFC3339), "payload": map[string]any{"type": "message", "role": "user", "id": "u" + string(rune('a'+i)), "content": content}})
	}
	records = append(records, map[string]any{"type": "response_item", "timestamp": now.Add(time.Minute).Format(time.RFC3339), "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Done."}}}})
	return SourceBundle{
		SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture: SourceCapture{
			Harness: Harness{Name: "codex"}, AdapterName: "codex", AdapterVersion: "0.15.0",
			SourceFormat: "codex-jsonl", FilterVersion: "15", CapturedAt: now,
		},
		NativeRecords: records,
	}
}

func inputText(text string) []any {
	return []any{map[string]any{"type": "input_text", "text": text}}
}

func humanPrompts(t *testing.T, bundle SourceBundle) []string {
	t.Helper()
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var prompts []string
	for _, turn := range view.Turns {
		if turn.Kind == TurnKindHumanPrompt {
			prompts = append(prompts, turn.Text)
		}
	}
	return prompts
}

// The Codex desktop app writes the open-page context as its own user record
// before the typed prompt. A source retained before filter 16 still holds it,
// and it must neither title the session nor count as a prompt.
func TestLegacyCodexOpenPageOnlyRecordFallsThroughToNextPrompt(t *testing.T) {
	t.Parallel()
	bundle := legacyCodexBundle(t, inputText(codexNullOpenPage), inputText("Fix the flaky upload test"), inputText("Then open a PR"))
	if got := parserTestMetadata(t, bundle).Title; got != "Fix the flaky upload test" {
		t.Fatalf("metadata title = %q", got)
	}
	if got := labelsOf(t, bundle).Title; got != "Fix the flaky upload test" {
		t.Fatalf("labels title = %q", got)
	}
	if got := humanPrompts(t, bundle); len(got) != 2 || got[0] != "Fix the flaky upload test" {
		t.Fatalf("prompts = %q", got)
	}
}

func TestLegacyCodexOpenPageStringContentFallsThrough(t *testing.T) {
	t.Parallel()
	bundle := legacyCodexBundle(t, codexNullOpenPage, "Review the diff")
	if got := parserTestMetadata(t, bundle).Title; got != "Review the diff" {
		t.Fatalf("metadata title = %q", got)
	}
}

func TestLegacyCodexOpenPageBesidePromptKeepsPrompt(t *testing.T) {
	t.Parallel()
	for _, content := range []any{
		inputText(codexNullOpenPage + "\nReview this page."),
		[]any{map[string]any{"type": "input_text", "text": codexNullOpenPage}, map[string]any{"type": "input_text", "text": "Review this page."}},
	} {
		bundle := legacyCodexBundle(t, content)
		if got := parserTestMetadata(t, bundle).Title; got != "Review this page." {
			t.Fatalf("metadata title = %q for %v", got, content)
		}
	}
}

// Nothing but context: the title is empty, never the raw wrapper, so a
// native thread name (or the session ID) is what a row shows instead.
func TestLegacyCodexOpenPageOnlySessionHasNoPromptTitle(t *testing.T) {
	t.Parallel()
	bundle := legacyCodexBundle(t, inputText(codexNullOpenPage), inputText(`<external_codex_apps_open_page>Selected page</external_codex_apps_open_page>`))
	m := parserTestMetadata(t, bundle)
	if m.Title != "" {
		t.Fatalf("metadata title = %q", m.Title)
	}
	if got := humanPrompts(t, bundle); len(got) != 0 {
		t.Fatalf("prompts = %q", got)
	}
}

// Mentions, quoted markup and indented examples are what the person wrote,
// so they stay in the title as filter 16 keeps them.
func TestLegacyCodexOpenPageExamplesStayInTitle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"Why does `<external_codex_apps_open_page>...</external_codex_apps_open_page>` appear?", "Why does `<external_codex_apps_open_page>...</external_codex_apps_open_page>` appear?"},
		{"    <external_codex_apps_open_page>example</external_codex_apps_open_page>", "<external_codex_apps_open_page>example</external_codex_apps_open_page>"},
		{"Explain external_codex_apps_open_page with examples.", "Explain external_codex_apps_open_page with examples."},
	} {
		bundle := legacyCodexBundle(t, inputText(tc.input))
		if got := parserTestMetadata(t, bundle).Title; got != tc.want {
			t.Fatalf("title for %q = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// A refresh of a filter-15 source derives what a current capture of the
// same transcript derives.
func TestLegacyCodexOpenPageMatchesCurrentFilter(t *testing.T) {
	t.Parallel()
	contents := []any{inputText(codexNullOpenPage), inputText("Fix the flaky upload test")}
	legacy := legacyCodexBundle(t, contents...)
	var lines []string
	for _, record := range legacy.NativeRecords {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(line))
	}
	filtered, err := CodexAdapter{}.FilterJSONL(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	current := parserTestBundle(t, "codex", CodexAdapter{}, filtered)
	legacyMeta, currentMeta := parserTestMetadata(t, legacy), parserTestMetadata(t, current)
	if legacyMeta.Title != currentMeta.Title || *legacyMeta.Counts.Turns != *currentMeta.Counts.Turns || *legacyMeta.Counts.Messages != *currentMeta.Counts.Messages {
		t.Fatalf("legacy %q %d/%d, current %q %d/%d", legacyMeta.Title, *legacyMeta.Counts.Turns, *legacyMeta.Counts.Messages, currentMeta.Title, *currentMeta.Counts.Turns, *currentMeta.Counts.Messages)
	}
	if got := humanPrompts(t, legacy); len(got) != 1 {
		t.Fatalf("legacy prompts = %q", got)
	}
}

// The rule is Codex's: Claude text that starts with the same tag is kept.
func TestClaudePromptKeepsOpenPageMarkup(t *testing.T) {
	t.Parallel()
	bundle := SourceBundle{
		SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture:       SourceCapture{Harness: Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: "1", SourceFormat: "claude-jsonl", FilterVersion: "15", CapturedAt: time.Unix(1, 0)},
		NativeRecords: []map[string]any{{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": codexNullOpenPage}}},
	}
	if got := parserTestMetadata(t, bundle).Title; got != codexNullOpenPage {
		t.Fatalf("title = %q", got)
	}
}

// The parser strip is Codex's alone, even for a record shaped like a Codex
// one: Claude and Cursor derivation never applies the filter's rule again.
func TestInjectedContextStripIsCodexOnly(t *testing.T) {
	t.Parallel()
	record := map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": inputText(codexNullOpenPage + "\nReview this page.")}}
	before, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []nativeProfile{profileClaude, profileCursor} {
		if got, _ := json.Marshal(withoutCodexInjectedContext(agent, record)); string(got) != string(before) {
			t.Fatalf("%s record changed: %s", agent, got)
		}
	}
	got, _ := json.Marshal(withoutCodexInjectedContext(profileCodex, record))
	if !strings.Contains(string(got), `"text":"Review this page."`) || strings.Contains(string(got), "external_codex_apps_open_page") {
		t.Fatalf("codex record = %s", got)
	}
	if after, _ := json.Marshal(record); string(after) != string(before) {
		t.Fatalf("input record mutated: %s", after)
	}
}

// Text the current filter keeps is a fixed point of the parser strip, so a
// Codex session captured under filter 16 or later derives what it did.
func TestCurrentFilterTextIsInjectedContextFixedPoint(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		codexNullOpenPage,
		codexNullOpenPage + "Summarize this page.",
		"Keep this <external_codex_apps_open_page>outer<external_codex_apps_open_page>inner</external_codex_apps_open_page>tail</external_codex_apps_open_page> request",
		"<external_codex_apps_open_page>unfinished",
		"```xml\n<external_codex_apps_open_page>example</external_codex_apps_open_page>\n```",
		"<external_codex_apps_open_page>Context</external_codex_apps_open_page>\n    <external_codex_apps_open_page>example</external_codex_apps_open_page>",
		"<system-reminder>Injected</system-reminder><external_codex_apps_open_page>Context</external_codex_apps_open_page> \t\r\n    <external_codex_apps_open_page>example</external_codex_apps_open_page>",
		"\r\n" + codexNullOpenPage + "\r\nFix it",
		"<sys<system-reminder>x</system-reminder>tem-reminder>y</system-reminder>z",
		" \n    <external_codex_apps_open_page>example</external_codex_apps_open_page>",
	} {
		state := sanitizeState{AddGap: func(string, int, string) {}}
		kept, keep := sanitizeValue(in, &state)
		if !keep {
			continue
		}
		if injected, out := archive.StripInjectedContext(kept.(string)); injected {
			t.Fatalf("filtered %q from %q strips again to %q", kept, in, out)
		}
	}
}
