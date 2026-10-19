package statshtml

import (
	"fmt"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// realisticPrices adds the families the realistic archive uses to the golden
// table.
const realisticPrices = `{
  "version": "golden-2",
  "as_of": "2026-09-29",
  "models": [
    {"id": "claude-opus-5",   "family": "opus-5",   "input_per_mtok": 5,    "output_per_mtok": 25, "cache_read_per_mtok": 0.5,   "cache_write_per_mtok": 6.25},
    {"id": "claude-fable-5",  "family": "fable-5",  "input_per_mtok": 10,   "output_per_mtok": 50, "cache_read_per_mtok": 1,     "cache_write_per_mtok": 12.5},
    {"id": "claude-sonnet-5", "family": "sonnet-5", "input_per_mtok": 2,    "output_per_mtok": 10, "cache_read_per_mtok": 0.2,   "cache_write_per_mtok": 2.5},
    {"id": "claude-haiku-4-5",  "family": "haiku-4.5",  "input_per_mtok": 1,    "output_per_mtok": 5,  "cache_read_per_mtok": 0.1,   "cache_write_per_mtok": 1.25},
    {"id": "gpt-5.6-sol",     "family": "gpt-5.6-sol","input_per_mtok": 1.25, "output_per_mtok": 10, "cache_read_per_mtok": 0.125, "cache_write_per_mtok": 0}
  ]
}`

// realisticProjects are the projects of the realistic archive's Claude Code
// sessions, by how many sessions each has.
var realisticProjects = []struct {
	name     string
	sessions int
}{
	{"agent-archive", 34}, {"levenshtein", 14}, {"styleprofile", 8}, {"family_books", 8}, {"benchplan", 6},
	{"dotfiles", 4}, {"site", 3}, {"notes", 2}, {"scratch", 2}, {"blog", 1}, {"infra", 1}, {"labs", 1},
}

// realisticModel is the model the n-th Claude Code session mostly used.
func realisticModel(n int) string {
	switch {
	case n%9 == 0:
		return "claude-fable-5"
	case n%13 == 0:
		return "claude-haiku-4-5"
	}
	return "claude-opus-5"
}

// realisticDay is the September day the n-th session was captured on: most on
// the 27th.
func realisticDay(n int) int {
	switch {
	case n%7 == 0:
		return 25
	case n%11 == 0:
		return 20
	}
	return 27
}

// realisticSkills are the skills the n-th session used.
func realisticSkills(n int) []string {
	var skills []string
	if n%9 == 0 || n%13 == 0 {
		skills = append(skills, "code-review")
	}
	if n%17 == 0 {
		skills = append(skills, "review-pr")
	}
	if n%29 == 0 {
		skills = append(skills, "anthropic-skills:docs")
	}
	return skills
}

// realisticMCP are the MCP calls of the n-th session.
func realisticMCP(n int) map[string]int {
	switch {
	case n%23 == 0:
		return map[string]int{"linear": 4}
	case n%3 == 0:
		return map[string]int{"github": 1 + n%5}
	}
	return nil
}

// realisticSessions is the archive behind the redesign's default view: 93
// sessions of three agents in the last 30 days (Claude Code 84, Cursor 8, Codex
// 1), most of them on one day, a dozen projects, four Claude model families
// and one OpenAI model, hundreds of subagent runs that account for most of the
// tokens, skills and MCP calls, ten sessions without token data, and nothing in
// the previous period.
func realisticSessions() []archive.Metadata {
	var specs []sessionSpec
	n := 0
	for _, p := range realisticProjects {
		for range p.sessions {
			n++
			model := realisticModel(n)
			// The last two Claude Code sessions report no token data.
			var tokens []tokenSpec
			if n <= 82 {
				tokens = []tokenSpec{{model, 300_000, 250_000, 11_000_000 + n*40_000, 900_000}}
			}
			specs = append(specs, sessionSpec{
				id: fmt.Sprintf("claude-%03d", n), harness: "claude", project: p.name,
				captured: time.Date(2026, time.September, realisticDay(n), 8+n%9, 0, 0, 0, time.UTC),
				turns:    6 + n%9, messages: 90, toolResults: 120, errors: 3 + n%4,
				models: []string{model}, tokens: tokens, skills: realisticSkills(n), mcp: realisticMCP(n),
			})
		}
	}
	// The costliest session: a long one with 38 subagents.
	for i := range specs {
		if specs[i].project == "styleprofile" && specs[i].tokens != nil {
			specs[i].tokens = []tokenSpec{{"claude-opus-5", 4_000_000, 2_000_000, 620_000_000, 40_000_000}}
			specs[i].compactions = 3
			specs[i].id = "claude-costliest"
			break
		}
	}
	// Subagent runs: 497 of them, sonnet, spread over the sessions with tokens
	// (38 under the costliest).
	runs := 0
	for i, s := range specs {
		if s.tokens == nil {
			continue
		}
		count := 6
		if s.id == "claude-costliest" {
			count = 38
		}
		for j := range count {
			if runs >= 497 {
				break
			}
			runs++
			specs = append(specs, sessionSpec{
				id: fmt.Sprintf("sub-%d-%d", i, j), harness: "claude", project: s.project, captured: s.captured, parent: s.id,
				models: []string{"claude-sonnet-5"}, turns: 3, toolResults: 40, errors: 1,
				tokens: []tokenSpec{{"claude-sonnet-5", 200_000, 120_000, 16_000_000, 600_000}},
			})
		}
	}
	for i := range 8 {
		var skills []string
		if i < 2 {
			skills = []string{"cursor-guide"}
		}
		specs = append(specs, sessionSpec{
			id: fmt.Sprintf("cursor-%d", i), harness: "cursor", project: "agent-archive",
			captured: time.Date(2026, time.September, 27, 10+i, 0, 0, 0, time.UTC),
			models:   []string{"cursor-auto"}, turns: 4, skills: skills,
		})
	}
	specs = append(specs, sessionSpec{
		id: "codex-1", harness: "codex", project: "levenshtein", captured: time.Date(2026, time.September, 27, 12, 0, 0, 0, time.UTC),
		models: []string{"gpt-5.6-sol"}, turns: 6, messages: 40, toolResults: 30,
		tokens: []tokenSpec{{"gpt-5.6-sol", 900_000, 80_000, 700_000, 0}},
	})
	out := make([]archive.Metadata, len(specs))
	for i, s := range specs {
		out[i] = s.build()
	}
	return out
}
