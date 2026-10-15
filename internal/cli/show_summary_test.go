package cli

import (
	"bytes"
	"context"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

var summaryNow = time.Date(2026, 9, 29, 13, 2, 0, 0, time.UTC)

func intPtr(n int) *int { return &n }

// summaryFixture is a Claude Code session with every row the summary shows.
func summaryFixture() sessionView {
	importedAt := time.Date(2026, 9, 29, 12, 30, 0, 0, time.UTC)
	endedAt := time.Date(2026, 9, 29, 10, 58, 0, 0, time.UTC)
	return sessionView{
		Metadata: archive.Metadata{
			SessionID: "03e60c25f1a04b7c9d2e8f6a1b3c5d7e",
			Name:      "Stabilize the OAuth tests",
			Title:     "Fix flaky OAuth callback tests",
			Branch:    "fix-oauth",
			PullRequests: []archive.PullRequestLink{
				{Repository: "wangjohn/agent-archive", Number: 155, URL: "https://github.com/wangjohn/agent-archive/pull/155"},
				{Repository: "wangjohn/agent-archive", Number: 160},
			},
			ProjectName:   "agent-archive",
			StartedAt:     time.Date(2026, 9, 29, 10, 14, 0, 0, time.UTC),
			CapturedAt:    time.Date(2026, 9, 29, 11, 2, 0, 0, time.UTC),
			EndedAt:       &endedAt,
			Harness:       archive.Harness{Name: "claude", Version: "2.4.1"},
			Parser:        archive.ParserInfo{Name: "claude", Version: "0.13.0", Status: archive.ParserStatusPartial},
			FilterVersion: "12",
			State:         archive.MetadataStateIdle,
			TurnOutcome:   archive.TurnOutcomeCompleted,
			Models: []archive.ModelSummary{
				{Attributes: map[string]string{"gen_ai.request.model": "claude-haiku-4-5"}, TurnCount: intPtr(4)},
				{Attributes: map[string]string{"gen_ai.request.model": "claude-opus-5-5", "agent_archive.request.reasoning_level": "high"}, TurnCount: intPtr(31)},
				{Attributes: map[string]string{"gen_ai.request.model": "claude-opus-5-5", "agent_archive.request.reasoning_level": "high"}, Source: archive.ModelSummarySourceHook},
				{Attributes: map[string]string{"gen_ai.request.model": "<synthetic>"}, TurnCount: intPtr(1)},
			},
			SkillsUsed: []archive.SkillUse{{Name: "simplify"}, {Name: "code-review"}},
			Counts: archive.Counts{
				Turns: intPtr(35), Messages: intPtr(212), ToolCalls: intPtr(148),
				UserShellCommands: intPtr(3), Compactions: intPtr(1), FilesTouched: intPtr(14),
				Commits: intPtr(2), Pushes: intPtr(1), PRsCreated: intPtr(1), PRsMerged: intPtr(1),
			},
			GitActivity: []archive.GitEvent{
				{Kind: archive.GitEventCommit, Source: archive.GitEventSourceShell, SHA: "3f9c2ab"},
				{Kind: archive.GitEventCommit, Source: archive.GitEventSourceShell, SHA: "4a0d3bc"},
				{Kind: archive.GitEventPush, Source: archive.GitEventSourceShell, Branch: "fix-oauth"},
				{Kind: archive.GitEventPRCreated, Source: archive.GitEventSourceMCP, Repository: "wangjohn/agent-archive", PRNumber: 155},
				{Kind: archive.GitEventPRMerged, Source: archive.GitEventSourceMCP, Repository: "wangjohn/agent-archive", PRNumber: 155},
			},
			ToolsUsed: []archive.ToolUsage{
				{Name: "Bash", Count: 42}, {Name: "Edit", Count: 18}, {Name: "Read", Count: 12},
				{Name: "Grep", Count: 9}, {Name: "mcp__github__create_pull_request", Count: 1},
			},
			CaptureGaps: []archive.CaptureGap{
				{Code: archive.CaptureGapImportedWithoutHookEvidence, Detail: "No hook observed this session before it was imported (imported_at): activity before then has no hook lifecycle events, final-response text, or skill inventory."},
				{Code: "hidden_instruction_omitted", Record: 3, Detail: "injected instruction block omitted"},
				{Code: "hidden_instruction_omitted", Record: 9, Detail: "injected instruction block omitted"},
				{Code: "sensitive_content_redacted", Record: 12, Detail: "content redacted"},
				{Code: "unknown_field_omitted", Record: 20, Detail: "omitted keys: advisorModel"},
				{Code: "incomplete_or_invalid_record", Record: 40, Detail: "jsonl record omitted"},
				{Code: "incomplete_or_invalid_record", Record: 41, Detail: "jsonl record omitted"},
			},
			LinkedSessions: []archive.LinkedSessionReference{{SessionID: "child-1"}, {SessionID: "child-2"}},
			Origin:         archive.SessionOriginImport,
			ImportedAt:     &importedAt,
		},
		LinkedAvailability: []reader.LinkedAvailability{
			{SessionID: "child-1", State: reader.LinkedStateMetadataAvailable},
			{SessionID: "child-2", State: reader.LinkedStateUnavailableOrExpired},
		},
	}
}

func renderSummaryText(view sessionView, opts summaryOptions) string {
	var b bytes.Buffer
	renderSessionSummary(&b, view, opts)
	return b.String()
}

// Regenerate with `go test ./internal/cli -run TestSessionSummaryGolden
// -update` and review the diff.
func TestSessionSummaryGolden(t *testing.T) {
	t.Parallel()
	opts := summaryOptions{Now: summaryNow, Location: time.UTC, Hints: true}
	golden.Check(t, filepath.Join("testdata", "show", "summary.txt"), []byte(renderSummaryText(summaryFixture(), opts)))
	opts.Style = textStyle{color: true}
	golden.Check(t, filepath.Join("testdata", "show", "summary-color.txt"), []byte(renderSummaryText(summaryFixture(), opts)))
}

// The line under the title says the agent, project and time in the session
// list's words: the agent's name, colored as the list's dot when there is
// color, and the detail strip's "last active" phrase.
func TestSummaryContextUsesTheListWords(t *testing.T) {
	t.Parallel()
	meta := func(harness string, active time.Time) archive.Metadata {
		return archive.Metadata{Harness: archive.Harness{Name: harness}, ProjectName: "agent-archive", CapturedAt: active, StartedAt: active}
	}
	plain := summaryOptions{Now: summaryNow}
	for _, tc := range []struct {
		name string
		m    archive.Metadata
		want string
	}{
		{"claude", meta("claude", summaryNow.Add(-2*time.Minute)), "Claude · agent-archive · last active 2 minutes ago"},
		{"codex hours", meta("codex", summaryNow.Add(-5*time.Hour)), "Codex · agent-archive · last active 5 hours ago"},
		{"days", meta("cursor", summaryNow.Add(-72*time.Hour)), "Cursor · agent-archive · last active 3 days ago"},
		{"other harness", meta("aider", summaryNow), "Aider · agent-archive · last active just now"},
		{"no harness or time", archive.Metadata{ProjectName: "agent-archive"}, "agent-archive"},
	} {
		if got := summaryContext(tc.m, plain, 80); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	color := summaryOptions{Now: summaryNow, Style: textStyle{color: true}}
	s := color.Style
	m := meta("claude", summaryNow.Add(-2*time.Minute))
	if got, want := summaryContext(m, color, 80), s.agent("claude", "Claude")+s.dim(" · agent-archive · last active 2 minutes ago"); got != want {
		t.Errorf("color: got %q, want %q", got, want)
	}
	// Cut inside the agent's name, the line is dim, and fits.
	if got := summaryContext(m, color, 4); got != s.dim(ellipsize("Claude · agent-archive", 4)) || visibleWidth(got) > 4 {
		t.Errorf("cut: got %q", got)
	}
}

// Unknown counts are left out, not shown as zero, and a session with no
// gaps, links, models, or skills has no rows for them.
func TestSessionSummaryOmitsAbsentData(t *testing.T) {
	t.Parallel()
	view := sessionView{Metadata: archive.Metadata{
		SessionID: "abcdef0123456789abcdef0123456789",
		Harness:   archive.Harness{Name: "codex"},
		Counts:    archive.Counts{Turns: intPtr(2), ToolCalls: intPtr(0), FilesTouched: intPtr(0)},
	}}
	text := renderSummaryText(view, summaryOptions{Now: summaryNow, Location: time.UTC})
	if !strings.HasPrefix(text, "abcdef01\n") {
		t.Fatalf("title does not fall back to the short ID:\n%s", text)
	}
	if !strings.Contains(text, "Activity  2 turns · 0 tool calls\n") {
		t.Fatalf("activity row:\n%s", text)
	}
	for _, absent := range []string{"message", "compaction", "edited", "Tools", "When", "Model", "Skills", "Subagents", "Parent", "Imported", "Omitted", "Incomplete", "Transcript:", "completed"} {
		if strings.Contains(text, absent) {
			t.Fatalf("%q shown for absent data:\n%s", absent, text)
		}
	}
	if !strings.Contains(text, "Agent     Codex\n") || !strings.Contains(text, "origin hook") {
		t.Fatalf("agent or provenance missing:\n%s", text)
	}
}

// Gaps the archive records by design (filtering, redaction, fields the
// parser does not recognize) are named in the quiet Omitted row and never
// warn; any other code, including one this version does not know, is
// listed under the warning.
func TestSessionSummarySeparatesRoutineGaps(t *testing.T) {
	t.Parallel()
	view := sessionView{Metadata: archive.Metadata{
		SessionID: "abcdef0123456789abcdef0123456789",
		Harness:   archive.Harness{Name: "claude"},
		CaptureGaps: []archive.CaptureGap{
			{Code: "unknown_record_type", Detail: "record omitted"},
			{Code: "hidden_instruction_omitted"},
			{Code: "hidden_instruction_omitted"},
			{Code: "sensitive_content_redacted"},
			{Code: "unknown_field_omitted", Detail: "omitted keys: advisorModel, apiBlockIndex"},
		},
	}}
	text := renderSummaryText(view, summaryOptions{Now: summaryNow, Location: time.UTC})
	want := "  Omitted   injected instructions, redacted secrets, unrecognized fields,\n            unrecognized records\n"
	if !strings.Contains(text, want) {
		t.Fatalf("missing %q:\n%s", want, text)
	}
	for _, absent := range []string{"!", "Incomplete", "hidden_instruction_omitted", "omitted keys"} {
		if strings.Contains(text, absent) {
			t.Fatalf("%q shown for routine gaps:\n%s", absent, text)
		}
	}

	view.CaptureGaps = append(view.CaptureGaps, archive.CaptureGap{Code: "some_future_gap", Detail: "new"})
	text = renderSummaryText(view, summaryOptions{Now: summaryNow, Location: time.UTC})
	if !strings.Contains(text, "  ! Incomplete capture\n    some_future_gap — new\n") {
		t.Fatalf("an unknown code is not warned about:\n%s", text)
	}
	if strings.Contains(text, "unknown_record_type") {
		t.Fatalf("a routine code is listed under the warning:\n%s", text)
	}
}

func TestSessionSummaryParentAndSpanAcrossDays(t *testing.T) {
	t.Parallel()
	view := sessionView{Metadata: archive.Metadata{
		SessionID:       "abcdef0123456789abcdef0123456789",
		ParentSessionID: "0123456789abcdef0123456789abcdef",
		Harness:         archive.Harness{Name: "claude"},
		StartedAt:       time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC),
		CapturedAt:      time.Date(2026, 1, 1, 1, 30, 0, 0, time.UTC),
		State:           archive.MetadataStateActive,
	}}
	text := renderSummaryText(view, summaryOptions{Now: summaryNow, Location: time.UTC})
	for _, want := range []string{"Parent    01234567\n", "When      Dec 31 2025, 23:00 → Jan 1, 01:30 (2h 30m span)\n", "! active\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
}

// Every metadata string is one display line: escape sequences, control
// characters, and bidi overrides from the bucket never reach the terminal.
func TestSessionSummaryNeutralizesEscapes(t *testing.T) {
	t.Parallel()
	view := summaryFixture()
	view.Title = "evil\x1b[2J\x1b]52;c;cGFzdGU=\x07title\u202e\nsecond line"
	view.ProjectName = "proj\x1b[31m"
	view.Harness.Version = "1.0\u009b2J"
	view.SkillsUsed = []archive.SkillUse{{Name: "skill\x07"}}
	view.ToolsUsed = []archive.ToolUsage{{Name: "tool\x1b[2J", Count: 3}}
	view.CaptureGaps = []archive.CaptureGap{{Code: "gap\x1b[1m", Detail: "detail\r\nmore"}}
	view.Models = []archive.ModelSummary{{Attributes: map[string]string{"gen_ai.request.model": "model\x1b[0m", "agent_archive.request.reasoning_level": "hi\x9b"}}}
	text := renderSummaryText(view, summaryOptions{Now: summaryNow, Location: time.UTC, Hints: true})
	for _, r := range text {
		if (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) {
			t.Fatalf("control %U in summary:\n%q", r, text)
		}
	}
	if !strings.Contains(text, "evil[2J]52;c;cGFzdGU=title second line") {
		t.Fatalf("title not one display line:\n%s", text)
	}
}

func TestSessionSummaryModelLines(t *testing.T) {
	t.Parallel()
	got := summaryModels([]archive.ModelSummary{
		{Attributes: map[string]string{"gen_ai.request.model": "gpt-5", "gen_ai.response.model": "gpt-5-2026-08"}, TurnCount: intPtr(1)},
		{Attributes: map[string]string{"agent_archive.request.model_label": "Auto"}},
		{Attributes: map[string]string{"gen_ai.request.model": "o4", "agent_archive.request.reasoning_level": "low"}, TurnCount: intPtr(3)},
		{Attributes: map[string]string{"gen_ai.request.model": "o4", "agent_archive.request.reasoning_level": "low"}, TurnCount: intPtr(2)},
		{Attributes: map[string]string{}},
	})
	want := []string{"o4 (low reasoning) · 5 responses", "gpt-5-2026-08 · 1 response", "Auto"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("models = %q, want %q", got, want)
	}
}

func TestSessionSummaryTruncatesLongLines(t *testing.T) {
	t.Parallel()
	view := summaryFixture()
	view.Title = strings.Repeat("long title ", 20)
	view.SkillsUsed = nil
	for i := range 30 {
		view.SkillsUsed = append(view.SkillsUsed, archive.SkillUse{Name: "skill-" + strings.Repeat("x", i)})
	}
	text := renderSummaryText(view, summaryOptions{Now: summaryNow, Location: time.UTC, Style: textStyle{width: 40}, Hints: true})
	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		if w := visibleWidth(line); w > 40 {
			t.Fatalf("line of %d columns: %q", w, line)
		}
	}
	if !strings.Contains(text, "…") {
		t.Fatalf("no ellipsis:\n%s", text)
	}
}

// transcriptFixture filters the Claude handoff fixture into a bundle, the
// same path a capture takes.
func transcriptFixture(t *testing.T) archive.Transcript {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "archive", "testdata", "handoff", "claude.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	_, adapter, ok := productionAgents.LookupSources("claude")
	if !ok {
		t.Fatal("Claude filter unavailable")
	}
	filtered, err := (claude.Filter{}).FilterJSONL(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{
		ArchiveSessionID: summaryFixture().SessionID, NativeSessionID: "native-claude", ProjectID: "project", ProjectRoot: "/Users/someone/widgets",
		Harness: archive.Harness{Name: adapter.Name()}, SessionStartedAt: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
	}
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := buildTranscript(context.Background(), nil, bundle)
	if err != nil {
		t.Fatal(err)
	}
	return transcript
}

// Regenerate with `go test ./internal/cli -run TestTranscriptGolden -update`
// and review the diff.
func TestTranscriptGolden(t *testing.T) {
	t.Parallel()
	transcript := transcriptFixture(t)
	for name, full := range map[string]bool{"transcript.txt": false, "transcript-full.txt": true} {
		var b bytes.Buffer
		renderTranscript(&b, summaryFixture(), transcript, transcriptOptions{summaryOptions: summaryOptions{Now: summaryNow, Location: time.UTC}, Full: full})
		golden.Check(t, filepath.Join("testdata", "show", name), b.Bytes())
	}
}

// show --transcript pages the readable transcript; --full adds results;
// --transcript --json is the former --normalized output.
func TestShowTranscriptFlags(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	run := func(args ...string) (string, string, int) {
		var out, errOut bytes.Buffer
		code := Run(args, nil, &out, &errOut, env)
		return out.String(), errOut.String(), code
	}
	out, errOut, code := run("show", id, "--transcript")
	if code != 0 || !strings.Contains(out, "visible") || strings.Contains(out, `"turns"`) {
		t.Fatalf("--transcript: code=%d stderr=%s\n%s", code, errOut, out)
	}
	if _, errOut, code := run("show", id, "--full"); code != 2 || !strings.Contains(errOut, "--full needs --transcript") {
		t.Fatalf("--full alone: code=%d stderr=%s", code, errOut)
	}
	if out, errOut, code := run("show", id, "--transcript", "--full"); code != 0 || !strings.Contains(out, "visible") {
		t.Fatalf("--transcript --full: code=%d stderr=%s\n%s", code, errOut, out)
	}
	transcriptJSON, errOut, code := run("show", id, "--transcript", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("--transcript --json: code=%d stderr=%s", code, errOut)
	}
	normalized, errOut, code := run("show", id, "--normalized")
	if code != 0 || normalized != transcriptJSON {
		t.Fatalf("--normalized differs from --transcript --json: code=%d\n%s\n---\n%s", code, normalized, transcriptJSON)
	}
	if strings.TrimSpace(errOut) != "agent-archive: show: --normalized is deprecated; use --transcript --json" {
		t.Fatalf("deprecation note = %q", errOut)
	}
}

// On a terminal, show --transcript goes through the pager unless
// --no-pager.
func TestShowTranscriptPages(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(&out) }
	var paged []string
	env.RunPager = func(_ context.Context, command string, _ []string, in io.Reader, _, _ io.Writer) error {
		text, err := io.ReadAll(in)
		paged = append(paged, command+"\n"+string(text))
		return err
	}
	if code := Run([]string{"show", id, "--transcript"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if len(paged) != 1 || !strings.HasPrefix(paged[0], "less -FRX --mouse --wheel-lines=3 ") || !strings.Contains(paged[0], "visible") || out.Len() != 0 {
		t.Fatalf("paged=%q out=%q", paged, out.String())
	}
	if code := Run([]string{"show", id, "--transcript", "--no-pager"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if len(paged) != 1 || !strings.Contains(out.String(), "visible") {
		t.Fatalf("--no-pager paged: paged=%d out=%q", len(paged), out.String())
	}
}

// Transcript content needs an explicit request: bare show with
// --transcript is a usage error, since the browser's t key asks for it.
func TestShowTranscriptNeedsSessionID(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	stdin := strings.NewReader("1\n")
	var out, errOut bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(&out) }
	if code := Run([]string{"show", "--transcript"}, stdin, &out, &errOut, env); code != 2 || out.Len() != 0 {
		t.Fatalf("code=%d out=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "--transcript needs a SESSION_ID") {
		t.Fatalf("stderr=%s", errOut.String())
	}
}

// Shell and local commands are shown where they ran; a shell command's
// output only with --full, a local command's always. A notice from the app
// heads its own exchange.
func TestTranscriptRendersCommandsAndNotices(t *testing.T) {
	t.Parallel()
	transcript := archive.Transcript{Exchanges: []archive.TranscriptExchange{
		{Kind: archive.TranscriptExchangeLeading, Steps: []archive.TranscriptStep{
			{Kind: archive.TranscriptStepShell, Text: "git status\ngit log", Output: "On branch main"},
			{Kind: archive.TranscriptStepCommand, Text: "/model", Output: "Set model to claude-opus-5"},
		}},
		{Kind: archive.TranscriptExchangeNotification, Text: "Background task completed", Steps: []archive.TranscriptStep{
			{Kind: archive.TranscriptStepText, Text: "The reviewer finished."},
		}},
	}}
	render := func(full bool) string {
		var b bytes.Buffer
		renderTranscript(&b, summaryFixture(), transcript, transcriptOptions{summaryOptions: summaryOptions{Now: summaryNow, Location: time.UTC}, Full: full})
		return b.String()
	}
	plain, full := render(false), render(true)
	for _, want := range []string{"  $ git status …\n", "  » /model\n      │ Set model to claude-opus-5\n", "── Background task completed ─", "The reviewer finished."} {
		if !strings.Contains(plain, want) {
			t.Fatalf("missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "On branch main") || !strings.Contains(full, "  $ git status …\n      │ On branch main\n") {
		t.Fatalf("shell output shown without --full, or not with it:\n%s\n---\n%s", plain, full)
	}
}

// The prompt is quoted with a gutter that survives wrapping and blank
// lines, and each run of the agent's steps starts with its name: after the
// prompt and after the person's own command, never before that command.
func TestTranscriptMarksWhoIsSpeaking(t *testing.T) {
	t.Parallel()
	transcript := archive.Transcript{Exchanges: []archive.TranscriptExchange{
		{Kind: archive.TranscriptExchangePrompt, Text: "please review the levenshtein repo for gaps\n\nthen write a packet", Steps: []archive.TranscriptStep{
			{Kind: archive.TranscriptStepText, Text: "I'll start."},
			{Kind: archive.TranscriptStepCommand, Text: "/model", Output: "Set model to claude-opus-5"},
			{Kind: archive.TranscriptStepTool, Tool: &archive.HandoffToolCall{Name: "Bash", Summary: "go test ./..."}},
		}},
	}}
	var b bytes.Buffer
	renderTranscript(&b, summaryFixture(), transcript, transcriptOptions{summaryOptions: summaryOptions{Now: summaryNow, Location: time.UTC, Style: textStyle{width: 24}}})
	_, got, _ := strings.Cut(b.String(), "─\n")
	want := `┃ please review the
┃ levenshtein repo for
┃ gaps
┃
┃ then write a packet

Claude Code ›
I'll start.

  » /model
      │ Set model to claude-opus-5

Claude Code ›
  ▸ Bash go test ./...
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A prompt word wider than the line, such as a link or text with no spaces,
// is split so every row keeps the gutter, and a wide character is never cut.
func TestTranscriptPromptSplitsLongWords(t *testing.T) {
	t.Parallel()
	transcript := archive.Transcript{Exchanges: []archive.TranscriptExchange{
		{Kind: archive.TranscriptExchangePrompt, Text: "see https://example.com/a/very/long/path ok\n日本語のテキストです"},
	}}
	var b bytes.Buffer
	renderTranscript(&b, summaryFixture(), transcript, transcriptOptions{summaryOptions: summaryOptions{Now: summaryNow, Location: time.UTC, Style: textStyle{width: 13}}})
	_, got, _ := strings.Cut(b.String(), "─\n")
	want := `┃ see
┃ https://exa
┃ mple.com/a/
┃ very/long/p
┃ ath
┃ ok
┃ 日本語のテ
┃ キストです
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A session ID from the bucket is display text too, even as a fallback
// title or in a hint.
func TestSessionSummaryNeutralizesSessionID(t *testing.T) {
	t.Parallel()
	view := sessionView{Metadata: archive.Metadata{SessionID: "\x1b[2J\u202eé0123456789", Harness: archive.Harness{Name: "codex"}}}
	for _, text := range []string{
		renderSummaryText(view, summaryOptions{Now: summaryNow, Hints: true}),
		func() string {
			var b bytes.Buffer
			renderTranscript(&b, view, archive.Transcript{}, transcriptOptions{})
			return b.String()
		}(),
	} {
		for _, r := range text {
			if (r < 0x20 && r != '\n') || (r >= 0x80 && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) {
				t.Fatalf("control %U in output:\n%q", r, text)
			}
		}
	}
	if got := shortSessionID("abcdefgé12"); got != "abcdefg" {
		t.Fatalf("short ID cut a character: %q", got)
	}
}

// The hints name the full ID, and the harness when show was given one.
func TestShowHintsNameAnUnambiguousID(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"show", id[:minShortSessionID], "--harness", "codex"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "agent-archive show "+id+" --harness codex --transcript") {
		t.Fatalf("hint:\n%s", out.String())
	}
}

func TestShowFullNeedsReadableTranscript(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	for _, args := range [][]string{{"show", id, "--transcript", "--full", "--json"}, {"show", id, "--normalized", "--full"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 2 || out.Len() != 0 || !strings.Contains(errOut.String(), "--full is for the readable transcript") {
			t.Fatalf("%v: code=%d out=%s stderr=%s", args, code, out.String(), errOut.String())
		}
	}
}

// Every code the Omitted row names is one the archive writes, so a typo
// cannot turn a routine gap into a warning.
func TestRoutineGapsAreKnownCodes(t *testing.T) {
	t.Parallel()
	known := map[string]bool{}
	for _, code := range archive.CaptureGapCodes {
		known[code] = true
	}
	for _, gap := range routineGaps {
		if !known[gap.code] {
			t.Errorf("routineGaps names %q, which is not in archive.CaptureGapCodes", gap.code)
		}
	}
}

// The heading is the name the session's agent gave it, else the first prompt.
// With a name, the prompt is a row of its own, and the branch and the linked
// pull requests are rows; a session with none of them has no such rows.
func TestSessionSummaryShowsNameBranchAndLinkedPullRequests(t *testing.T) {
	t.Parallel()
	opts := summaryOptions{Now: summaryNow, Location: time.UTC}
	lines := strings.Split(renderSummaryText(summaryFixture(), opts), "\n")
	if lines[0] != "Stabilize the OAuth tests" {
		t.Fatalf("heading = %q, want the name", lines[0])
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{
		"  Prompt    Fix flaky OAuth callback tests\n",
		"  Branch    fix-oauth\n",
		"  PRs       wangjohn/agent-archive#155 · wangjohn/agent-archive#160\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary lacks %q:\n%s", want, text)
		}
	}
	m := summaryFixture().Metadata
	m.Name, m.Branch, m.PullRequests = "", "", nil
	text = renderSummaryText(sessionView{Metadata: m}, opts)
	if first, _, _ := strings.Cut(text, "\n"); first != "Fix flaky OAuth callback tests" {
		t.Errorf("heading without a name = %q, want the first prompt", first)
	}
	for _, label := range []string{"Prompt ", "Branch ", "PRs "} {
		if strings.Contains(text, "  "+label) {
			t.Errorf("a session with no %s has that row:\n%s", label, text)
		}
	}
}

// The Git row counts commits and pushes and names each pull request, or
// counts pull requests the capped event list does not hold.
func TestSummaryGit(t *testing.T) {
	t.Parallel()
	m := summaryFixture().Metadata
	if got := strings.Join(summaryGit(m), " · "); got != "2 commits · 1 push · PR #155 opened · PR #155 merged" {
		t.Fatalf("git = %q", got)
	}
	m.Counts.Pushes, m.Counts.PRsCreated = intPtr(3), intPtr(140)
	if got := strings.Join(summaryGit(m), " · "); got != "2 commits · 3 pushes · 140 PRs opened · PR #155 merged" {
		t.Fatalf("capped git = %q", got)
	}
	if got := summaryGit(archive.Metadata{Counts: archive.Counts{Commits: intPtr(0), Pushes: intPtr(0)}}); got != nil {
		t.Fatalf("a session with no git work shows %q", got)
	}
}

// The Commit row names the commit the session started on, says when its tree
// was dirty, and adds the last commit a stop saw when it moved.
func TestSummaryCommit(t *testing.T) {
	t.Parallel()
	start, last := strings.Repeat("3f", 20), strings.Repeat("9e", 20)
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		head *archive.SessionGitHead
		want string
	}{
		{nil, ""},
		{&archive.SessionGitHead{Start: &archive.GitHead{SHA: start, Dirty: new(false), ObservedAt: at}}, "started on 3f3f3f3f3f3f"},
		{&archive.SessionGitHead{Start: &archive.GitHead{SHA: start, ObservedAt: at}}, "started on 3f3f3f3f3f3f"},
		{&archive.SessionGitHead{Start: &archive.GitHead{SHA: start, Dirty: new(true), ObservedAt: at}}, "started on 3f3f3f3f3f3f with uncommitted changes"},
		{&archive.SessionGitHead{Start: &archive.GitHead{SHA: start, ObservedAt: at}, Last: &archive.GitHead{SHA: start, ObservedAt: at}}, "started on 3f3f3f3f3f3f"},
		{&archive.SessionGitHead{Start: &archive.GitHead{SHA: start, ObservedAt: at}, Last: &archive.GitHead{SHA: last, ObservedAt: at}}, "started on 3f3f3f3f3f3f · last seen on 9e9e9e9e9e9e"},
		{&archive.SessionGitHead{Last: &archive.GitHead{SHA: last, ObservedAt: at}}, "last seen on 9e9e9e9e9e9e"},
	} {
		if got := strings.Join(summaryCommit(tc.head), " · "); got != tc.want {
			t.Errorf("summaryCommit(%+v) = %q, want %q", tc.head, got, tc.want)
		}
	}
	view := sessionView{Metadata: archive.Metadata{SessionID: "s", GitHead: &archive.SessionGitHead{Start: &archive.GitHead{SHA: start, Dirty: new(true), ObservedAt: at}}}}
	if out := renderSummaryText(view, summaryOptions{Now: at, Location: time.UTC}); !strings.Contains(out, "Commit    started on 3f3f3f3f3f3f with uncommitted changes") {
		t.Errorf("show has no Commit row:\n%s", out)
	}
}

// The Tools row wraps between tools: no line is cut with an ellipsis and then
// carries on, at the default width or a narrow one. Only a tool too wide for
// a line by itself is cut.
func TestSessionSummaryToolsWrapBetweenTools(t *testing.T) {
	t.Parallel()
	view := summaryFixture()
	view.ToolsUsed = []archive.ToolUsage{
		{Name: "Bash", Count: 251}, {Name: "Agent", Count: 65}, {Name: "SendMessage", Count: 33}, {Name: "ToolSearch", Count: 6},
		{Name: "Write", Count: 5}, {Name: "mcp__ccd_session__spawn_task", Count: 4}, {Name: "mcp__ccd_pr__get_status", Count: 3},
		{Name: "Read", Count: 2}, {Name: "TaskStop", Count: 2}, {Name: "mcp__ccd_pr__bind_pr", Count: 2},
	}
	want := summaryTools(view.ToolsUsed)
	// At 52 columns the widest tool, 30, still fits a line with its " ·".
	for _, width := range []int{summaryWidth, 52} {
		text := renderSummaryText(view, summaryOptions{Now: summaryNow, Location: time.UTC, Style: textStyle{width: width}})
		var got []string
		inTools := false
		for line := range strings.SplitSeq(text, "\n") {
			switch {
			case strings.HasPrefix(line, "  Tools "):
				inTools = true
			case inTools && !strings.HasPrefix(line, strings.Repeat(" ", 12)):
				inTools = false
			}
			if !inTools {
				continue
			}
			if w := visibleWidth(line); w > width {
				t.Errorf("width %d: Tools line of %d columns: %q", width, w, line)
			}
			value := strings.TrimSuffix(strings.TrimSpace(line[12:]), " ·")
			if strings.Contains(value, "…") {
				t.Errorf("width %d: a line is cut with an ellipsis: %q", width, line)
			}
			got = append(got, strings.Split(value, " · ")...)
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("width %d: Tools lists %q, want %q\n%s", width, got, want, text)
		}
	}
	// A tool too wide for a line is cut on a line of its own.
	wide := "mcp__" + strings.Repeat("x", 60)
	view.ToolsUsed = []archive.ToolUsage{{Name: "Bash", Count: 3}, {Name: wide, Count: 1}, {Name: "Read", Count: 1}}
	text := renderSummaryText(view, summaryOptions{Now: summaryNow, Location: time.UTC, Style: textStyle{width: 40}})
	// 40 columns less the 12 the label takes, and 2 for the " ·" after it.
	cut := wide[:25] + "…"
	for _, wantLine := range []string{"  Tools     Bash 3 ·\n", "            " + cut + " ·\n", "            Read 1\n"} {
		if !strings.Contains(text, wantLine) {
			t.Errorf("no line %q in:\n%s", wantLine, text)
		}
	}
}

// An imported session with no end time is not given a span to the import:
// its capture time is when backfill ran. The When row and the header both
// date it by its start.
func TestSessionSummaryImportWithoutAnEndIsNotSpannedToTheImport(t *testing.T) {
	t.Parallel()
	view := summaryFixture()
	view.EndedAt = nil
	view.Origin = archive.SessionOriginImport
	view.StartedAt = summaryNow.Add(-14 * 24 * time.Hour)
	view.CapturedAt = summaryNow.Add(-time.Hour)
	text := renderSummaryText(view, summaryOptions{Now: summaryNow, Location: time.UTC, Style: textStyle{width: summaryWidth}})
	if want := "  When      " + formatSummaryTime(view.StartedAt, summaryOptions{Now: summaryNow, Location: time.UTC}) + "\n"; !strings.Contains(text, want) {
		t.Errorf("no When row %q:\n%s", want, text)
	}
	if strings.Contains(text, "span") || strings.Contains(text, "1 hour ago") {
		t.Errorf("the summary dates an import by the import:\n%s", text)
	}
}
