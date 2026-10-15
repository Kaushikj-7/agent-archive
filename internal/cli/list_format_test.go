package cli

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestUniqueShortIDsLengthensCollisions(t *testing.T) {
	t.Parallel()
	ids := []string{
		"abcdef00111111111111111111111111",
		"abcdef00222222222222222222222222",
		"zzzzzzzz333333333333333333333333",
	}
	shorts := uniqueShortIDs(ids)
	if len(shorts[0]) < 9 || shorts[0] == shorts[1] {
		t.Fatalf("colliding prefixes not lengthened: %v", shorts)
	}
	if shorts[0][:8] != "abcdef00" || shorts[2][:8] != "zzzzzzzz" {
		t.Fatalf("unexpected short ids: %v", shorts)
	}
	// Every row in one listing shares the same prefix length.
	if len(shorts[0]) != len(shorts[1]) || len(shorts[1]) != len(shorts[2]) {
		t.Fatalf("prefix lengths should match: %v", shorts)
	}
}

func TestFormatSessionRowsDefaultAndVerbose(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC)
	m := archive.Metadata{
		SessionID:   "abcdef0123456789abcdef0123456789",
		Title:       "Fix flaky OAuth callback tests",
		ProjectName: "agent-archive",
		CapturedAt:  now.Add(-2 * time.Hour),
		Harness:     archive.Harness{Name: "claude"},
		ProjectID:   "proj",
		Parser:      archive.ParserInfo{Status: archive.ParserStatusPartial},
		Models:      []archive.ModelSummary{{Attributes: map[string]string{"gen_ai.request.model": "claude-opus-5"}}},
		SkillsUsed:  []archive.SkillUse{{Name: "code-review"}},
		Origin:      archive.SessionOriginImport,
	}
	rows := formatSessionRows([]archive.Metadata{m}, listFormatOptions{
		Now: now, Projects: map[string]string{"proj": "ignored-local"},
	})
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	r := rows[0]
	if r.Title != "Fix flaky OAuth callback tests" || r.ShortID != "abcdef01" || r.When != "2 hours ago" || r.Project != "agent-archive" {
		t.Fatalf("row=%+v", r)
	}
	if r.Model != "claude-opus-5" || r.Skills != "code-review" || r.Hint != "" || r.WhenShort != "Thu" || r.Origin != "imported" {
		t.Fatalf("row=%+v", r)
	}
}

// A row shows the name the person's agent gave the session, else the first
// prompt's preview, else the short ID.
func TestFormatSessionRowsShowTheDisplayTitle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC)
	rows := formatSessionRows([]archive.Metadata{
		{SessionID: "aaaaaaaa0123456789abcdef01234567", Name: "Named in the agent", Title: "First prompt", CapturedAt: now},
		{SessionID: "bbbbbbbb0123456789abcdef01234567", Title: "First prompt only", CapturedAt: now},
		{SessionID: "cccccccc0123456789abcdef01234567", Name: "Name only", CapturedAt: now},
		{SessionID: "dddddddd0123456789abcdef01234567", CapturedAt: now},
	}, listFormatOptions{Now: now})
	var got []string
	for _, r := range rows {
		got = append(got, r.Title)
	}
	if want := []string{"Named in the agent", "First prompt only", "Name only", "dddddddd"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("titles = %q, want %q", got, want)
	}
}

func TestListTableShowsLongerStoredTitle(t *testing.T) {
	t.Parallel()
	const title = "In the agent-archive repository, investigate why sessions take so long to find and propose a practical fix"
	rows := formatSessionRows([]archive.Metadata{{
		SessionID: "aaaaaaaa0123456789abcdef01234567", Title: title,
	}}, listFormatOptions{Now: time.Now()})
	var out bytes.Buffer
	if err := printSessionTable(&out, rows, listFormatOptions{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(title)) {
		t.Fatalf("list lost the distinguishing end of the title: %s", out.String())
	}
}

func TestProjectLabelsUsesBasename(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "my-app")
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{
		{ProjectID: "a", Root: root, Included: true},
		{ProjectID: "b", Root: filepath.Join(t.TempDir(), "other"), Included: false},
	}}}
	labels := projectLabels(cfg)
	if labels["a"] != "my-app" {
		t.Fatalf("labels=%v", labels)
	}
	if _, ok := labels["b"]; ok {
		t.Fatalf("excluded project should be omitted: %v", labels)
	}
}

func TestMatchBrowseRow(t *testing.T) {
	t.Parallel()
	rows := []listRow{
		{Index: 1, SessionID: "aaaabbbbccccddddeeeeffff00001111", ShortID: "aaaabbbb", HarnessKey: "claude"},
		{Index: 2, SessionID: "1234567890abcdef1234567890abcdef", ShortID: "12345678", HarnessKey: "codex"},
	}
	if got, ok := matchBrowseRow("2", rows); !ok || got.SessionID != rows[1].SessionID {
		t.Fatalf("number match: ok=%v got=%+v", ok, got)
	}
	if got, ok := matchBrowseRow("aaaabbbb", rows); !ok || got.Index != 1 {
		t.Fatalf("short id: ok=%v got=%+v", ok, got)
	}
	// An all-decimal short id must not be treated as a row index.
	if got, ok := matchBrowseRow("12345678", rows); !ok || got.SessionID != rows[1].SessionID {
		t.Fatalf("decimal short id: ok=%v got=%+v", ok, got)
	}
	if _, ok := matchBrowseRow("9", rows); ok {
		t.Fatal("out of range should miss")
	}
}

// With color on, the colored agent dot, a dimmed hint, the live dot and the
// cursor mark take no room of their own: every row's PR and time end in the
// same column, hint or not.
func TestColoredCellsKeepTheColumnsAligned(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC)
	rows := formatSessionRows([]archive.Metadata{
		{SessionID: "aaaaaaaa0123456789abcdef01234567", Title: "With a skill", CapturedAt: now, SkillsUsed: []archive.SkillUse{{Name: "code-review"}}},
		{SessionID: "bbbbbbbb0123456789abcdef01234567", Title: "Parent of many", CapturedAt: now},
		{SessionID: "cccccccc0123456789abcdef01234567", Title: "No hint", CapturedAt: now},
	}, listFormatOptions{Now: now, Children: map[string]int{childKey("", "bbbbbbbb0123456789abcdef01234567"): 18}})
	for i := range rows {
		rows[i].PR = "#12"
	}
	rows[0].Live, rows[1].Highlight = true, true
	format, _ := listFormatOptions{Style: textStyle{color: true}, Cursor: true, Width: 100}.withColumns(rows)
	var out bytes.Buffer
	if err := printSessionTable(&out, rows, format); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], "\x1b[2m+18") {
		t.Fatalf("want three colored rows, a dim count of subagents:\n%s", out.String())
	}
	column := func(line, cell string) int {
		i := strings.Index(line, cell)
		if i < 0 {
			t.Fatalf("no %q in %q", cell, line)
		}
		return visibleWidth(line[:i])
	}
	for _, line := range lines {
		if got, want := column(line, "#12"), column(lines[0], "#12"); got != want {
			t.Errorf("PR starts at column %d, want %d, in %q", got, want, line)
		}
		if got, want := visibleWidth(line), visibleWidth(lines[0]); got != want {
			t.Errorf("row is %d columns, want %d, in %q", got, want, line)
		}
	}
}
