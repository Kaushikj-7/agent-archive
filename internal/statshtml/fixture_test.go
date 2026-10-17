package statshtml

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// fixtureNow is the clock the tests run at: noon UTC, so the window's days
// are UTC days.
var fixtureNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func day(month time.Month, d, hour int) time.Time {
	return time.Date(2026, month, d, hour, 0, 0, 0, time.UTC)
}

func ip(n int) *int { return &n }

// tokenSpec is one model's tokens of a session.
type tokenSpec struct {
	model     string
	input     int
	output    int
	cacheRead int
	write     int
}

// sessionSpec is an invented session: only metadata, never content.
type sessionSpec struct {
	id          string
	harness     string
	project     string
	captured    time.Time
	parent      string
	models      []string
	turns       int
	messages    int
	toolResults int
	errors      int
	compactions int
	tokens      []tokenSpec
	skills      []string
	mcp         map[string]int
}

// counts are the session's counts; the token totals are the sums over its
// models. Codex writes no tool-error flag, so its errors are unknown.
func (s sessionSpec) counts(input, output, read, write int) archive.Counts {
	var toolErrors, inputTokens, outputTokens, cacheRead, cacheWrite *int
	if s.toolResults > 0 && s.harness != "codex" {
		toolErrors = ip(s.errors)
	}
	if len(s.tokens) > 0 {
		inputTokens, outputTokens, cacheRead, cacheWrite = ip(input), ip(output), ip(read), ip(write)
	}
	return archive.Counts{
		Turns: positive(s.turns), Messages: positive(s.messages), Compactions: positive(s.compactions),
		ToolResults: positive(s.toolResults), ToolErrors: toolErrors,
		InputTokens: inputTokens, OutputTokens: outputTokens, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
	}
}

// positive is a count that is reported only when it is above zero.
func positive(n int) *int {
	if n > 0 {
		return ip(n)
	}
	return nil
}

func (s sessionSpec) build() archive.Metadata {
	var models []archive.ModelSummary
	for _, model := range s.models {
		models = append(models, archive.ModelSummary{
			Attributes: map[string]string{"gen_ai.request.model": model}, Source: archive.ModelSummarySourceNativeTranscript,
			ResponseModelStatus: archive.ResponseModelStatusNotExposed, TurnCount: ip(s.turns),
		})
	}
	var input, output, read, write int
	var perModel []archive.ModelTokens
	for _, t := range s.tokens {
		input, output, read, write = input+t.input, output+t.output, read+t.cacheRead, write+t.write
		perModel = append(perModel, archive.ModelTokens{
			Model: t.model, InputTokens: ip(t.input), OutputTokens: ip(t.output),
			CacheReadTokens: ip(t.cacheRead), CacheWriteTokens: ip(t.write),
		})
	}
	counts := s.counts(input, output, read, write)
	var skills []archive.SkillUse
	for _, name := range s.skills {
		skills = append(skills, archive.SkillUse{Name: name, Evidence: archive.SkillUseEvidenceNativeInvocation})
	}
	var mcp []archive.ToolUsage
	for _, server := range slices.Sorted(maps.Keys(s.mcp)) {
		mcp = append(mcp, archive.ToolUsage{Name: server, Count: s.mcp[server]})
	}
	return archive.Metadata{
		SchemaVersion: archive.MetadataSchemaVersion, SessionID: s.id, NativeSessionID: s.id,
		ProjectName: s.project, StartedAt: s.captured, CapturedAt: s.captured, MetadataDerivedAt: s.captured,
		Harness:         archive.Harness{Name: s.harness},
		Parser:          archive.ParserInfo{Name: s.harness, Version: "0.14.0", Status: archive.ParserStatusPartial},
		ParentSessionID: s.parent,
		SourceBundle:    archive.SourceReference{Key: "sessions/" + s.harness + "/" + s.id + "/source.jsonl.gz", SHA256: strings.Repeat("a", 64)},
		Counts:          counts, Models: models, ModelTokens: perModel, SkillsUsed: skills, MCPCalls: mcp,
	}
}

// fixtureSessions is a multi-agent archive: Claude Code sessions in two
// projects with two models, subagents, a long-context session, skills and MCP
// calls; Codex sessions with a priced and an unpriced model; Cursor sessions
// without tokens; a previous period; and earlier months for the month rank.
func fixtureSessions() []archive.Metadata {
	opus, sonnet := "claude-opus-5", "claude-sonnet-5"
	var specs []sessionSpec
	for i := range 12 {
		project := "agent-archive"
		if i%3 == 0 {
			project = "proj-api"
		}
		specs = append(specs, sessionSpec{
			id: fmt.Sprintf("claude-%02d", i), harness: "claude", project: project, captured: day(time.September, 2+i*2, 9+i%6),
			models: []string{opus, sonnet}, turns: 8 + i, messages: 60 + 5*i, toolResults: 100, errors: 4 + i%3,
			tokens: []tokenSpec{
				{opus, 40_000 + i*2_000, 120_000, 2_500_000 + i*100_000, 300_000},
				{sonnet, 20_000, 60_000, 900_000, 100_000},
			},
			skills: []string{"review-pr"}, mcp: map[string]int{"github": 3 + i},
		})
	}
	specs = append(specs, sessionSpec{
		id: "claude-big", harness: "claude", project: "proj-api", captured: day(time.September, 17, 14),
		models: []string{opus}, turns: 40, messages: 200, toolResults: 400, errors: 30, compactions: 2,
		tokens: []tokenSpec{{opus, 3_000_000, 900_000, 12_000_000, 2_000_000}},
		skills: []string{"create-skill", "review-pr"}, mcp: map[string]int{"linear": 12},
	})
	for i := range 2 {
		specs = append(specs, sessionSpec{
			id: fmt.Sprintf("claude-sub-%d", i), harness: "claude", project: "proj-api", captured: day(time.September, 17, 15),
			parent: "claude-big", models: []string{sonnet}, turns: 5, toolResults: 50, errors: 2,
			tokens: []tokenSpec{{sonnet, 500_000, 200_000, 4_000_000, 300_000}},
		})
	}
	for i := range 5 {
		specs = append(specs, sessionSpec{
			id: fmt.Sprintf("codex-%02d", i), harness: "codex", project: "dotfiles", captured: day(time.September, 5+i*4, 11),
			models: []string{"gpt-5"}, turns: 6, messages: 40, toolResults: 30,
			tokens: []tokenSpec{{"gpt-5", 900_000, 80_000, 700_000, 0}},
		})
	}
	specs = append(specs, sessionSpec{
		id: "codex-review", harness: "codex", project: "dotfiles", captured: day(time.September, 24, 11),
		models: []string{"codex-auto-review"}, turns: 2, messages: 10, toolResults: 5,
		tokens: []tokenSpec{{"codex-auto-review", 100_000, 10_000, 50_000, 0}},
	})
	for i := range 3 {
		specs = append(specs, sessionSpec{
			id: fmt.Sprintf("cursor-%02d", i), harness: "cursor", project: "agent-archive", captured: day(time.September, 10+i*5, 16),
			models: []string{"cursor-auto"}, turns: 4,
		})
	}
	for i := range 6 {
		specs = append(specs, sessionSpec{
			id: fmt.Sprintf("claude-prev-%02d", i), harness: "claude", project: "agent-archive", captured: day(time.August, 3+i*4, 10),
			models: []string{opus}, turns: 7, messages: 50, toolResults: 80, errors: 3,
			tokens: []tokenSpec{{opus, 30_000, 90_000, 2_000_000, 200_000}},
		})
	}
	for i, month := range []time.Month{time.April, time.May, time.June, time.July} {
		specs = append(specs, sessionSpec{
			id: fmt.Sprintf("claude-old-%d", i), harness: "claude", project: "agent-archive", captured: day(month, 10, 10),
			models: []string{opus}, turns: 5, messages: 30, toolResults: 40, errors: 1,
			tokens: []tokenSpec{{opus, 20_000 + i*1_000_000, 60_000, 1_000_000, 100_000}},
		})
	}
	out := make([]archive.Metadata, len(specs))
	for i, s := range specs {
		out[i] = s.build()
	}
	return out
}

// computeFixture is the engine's stats for sessions at the fixture's clock and
// its own pinned prices, so the goldens do not move when the built-in table
// does.
func computeFixture(tb testing.TB, sessions []archive.Metadata, days int, by stats.Grouping) stats.Stats {
	tb.Helper()
	table, err := stats.ParsePriceTable([]byte(goldenPrices))
	if err != nil {
		tb.Fatal(err)
	}
	return stats.Compute(sessions, stats.Options{
		Now: fixtureNow, Days: days, Location: time.UTC, PriceTable: table, By: by,
	})
}

// goldenPrices is a small price table for the tests, in the built-in table's
// format.
const goldenPrices = `{
  "version": "golden-1",
  "as_of": "2026-09-29",
  "models": [
    {"id": "claude-opus-5",   "family": "opus-5",   "input_per_mtok": 5,    "output_per_mtok": 25, "cache_read_per_mtok": 0.5,   "cache_write_per_mtok": 6.25},
    {"id": "claude-sonnet-5", "family": "sonnet-5", "input_per_mtok": 2,    "output_per_mtok": 10, "cache_read_per_mtok": 0.2,   "cache_write_per_mtok": 2.5},
    {"id": "gpt-5",           "family": "gpt-5",  "input_per_mtok": 1.25, "output_per_mtok": 10, "cache_read_per_mtok": 0.125, "cache_write_per_mtok": 0}
  ]
}`
