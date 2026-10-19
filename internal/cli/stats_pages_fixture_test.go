package cli

import (
	"math"
	"time"

	"github.com/wangjohn/agent-archive/internal/stats"
)

// The page tests draw pages from a stats.Stats built by hand, so a golden
// screen pins the layout and not the engine. realisticStats is the plan's
// fixture, from a real screen with names made up: 93 sessions from 3 agents
// (Claude Code 84, Cursor 8, Codex 1), 10B tokens of which 97% were cache
// reads, about $3,989 estimated, active on 3 of 30 days (Sep 27 the peak at
// about $2,910 with 70 sessions), ten projects, five model families, 497
// subagent runs using 77% of the tokens, skills, MCP servers, and 10 sessions
// without token data (Cursor 8, Claude Code 2). It has no previous period.

func f64(v float64) *float64 { return &v }

func i64(v int64) *int64 { return &v }

func intp(v int) *int { return &v }

// usd is a priced cost.
func usd(v float64) stats.Cost { return stats.Cost{USD: f64(v)} }

// realisticDaily is the fixture's 30 days, Aug 31 to Sep 29: three active days.
func realisticDaily() []stats.Day {
	start := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	spend := map[int]float64{26: 259, 27: 2910, 29: 820}
	sessions := map[int]int{26: 8, 27: 70, 29: 15}
	tokens := map[int]int64{26: 650_000_000, 27: 7_300_000_000, 29: 2_050_000_000}
	days := make([]stats.Day, 30)
	for i := range days {
		days[i] = stats.Day{Date: start.AddDate(0, 0, i).Format("2006-01-02"), Sessions: sessions[i], Tokens: i64(tokens[i]), Cost: usd(spend[i])}
	}
	return days
}

func realisticStats() stats.Stats {
	sessions, prompts, tokens, cost := 93.0, 673.0, 10_000_000_000.0, 3989.0
	cacheShare := 0.97
	return stats.Stats{
		Window: stats.Window{
			Days: 30, Timezone: "UTC", FirstDay: "2026-08-31", LastDay: "2026-09-29",
			From: time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		},
		Prices: stats.PriceInfo{Version: "2026-09-29", AsOf: "2026-09-29", Currency: "USD"},
		Coverage: stats.Coverage{
			Sessions: 93, Agents: 3, SessionsWithTokens: 83, SubagentSessions: 497,
			UnknownTokensByAgent: map[string]int{"cursor": 8, "claude": 2},
		},
		Daily:     realisticDaily(),
		Peak:      &stats.Peak{Date: "2026-09-27", Tokens: 7_300_000_000},
		PeakSpend: &stats.PeakSpend{Date: "2026-09-27", USD: 2910},
		Overview: stats.Overview{
			Sessions: stats.Measure{Value: &sessions}, Prompts: stats.Measure{Value: &prompts}, Tokens: stats.Measure{Value: &tokens},
			Cost:       stats.CostMeasure{Measure: stats.Measure{Value: &cost}},
			ActiveDays: stats.Measure{Value: f64(3)}, CacheShare: &cacheShare, DaysInWindow: 30, BestStreak: 1,
		},
		Agents: []stats.Agent{
			{
				Harness: "claude", Label: "Claude Code", Sessions: 84, SessionsWithTokens: 82, UnknownTokenSessions: 2,
				Tokens: i64(9_900_000_000), Cost: usd(3940), SessionShare: 84.0 / 93, TokenShare: f64(0.99), CacheHitRate: f64(0.97),
			},
			{
				Harness: "cursor", Label: "Cursor", Sessions: 8, UnknownTokenSessions: 8, SessionShare: 8.0 / 93,
			},
			{
				Harness: "codex", Label: "Codex", Sessions: 1, SessionsWithTokens: 1, Tokens: i64(100_000_000), Cost: usd(49),
				SessionShare: 1.0 / 93, TokenShare: f64(0.01), CacheHitRate: f64(0.9),
			},
		},
		Models: []stats.ModelRow{
			{Label: "opus-5", Models: []string{"claude-opus-5"}, Sessions: 60, Tokens: 7_000_000_000, Priced: true, Cost: usd(3270), CostShare: f64(3270.0 / 3989)},
			{Label: "fable-5", Models: []string{"claude-fable-5"}, Sessions: 20, Tokens: 1_500_000_000, Priced: true, Cost: usd(386), CostShare: f64(386.0 / 3989)},
			{Label: "sonnet-5", Models: []string{"claude-sonnet-5"}, Sessions: 30, Tokens: 1_300_000_000, Priced: true, Cost: usd(320), CostShare: f64(320.0 / 3989)},
			{Label: "gpt-5.6-sol", Models: []string{"gpt-5.6"}, Sessions: 1, Tokens: 100_000_000, Priced: true, Cost: usd(7), CostShare: f64(7.0 / 3989)},
			{Label: "haiku-5", Models: []string{"claude-haiku-5"}, Sessions: 10, Tokens: 100_000_000, Priced: true, Cost: usd(6), CostShare: f64(6.0 / 3989)},
		},
		Projects: []stats.Project{
			{Name: "agent-archive", Sessions: 31, Tokens: i64(4_600_000_000), Cost: usd(1862)},
			{Name: "levenshtein", Sessions: 18, Tokens: i64(1_900_000_000), Cost: usd(751)},
			{Name: "styleprofile", Sessions: 9, Tokens: i64(1_500_000_000), Cost: usd(586)},
			{Name: "family_books", Sessions: 12, Tokens: i64(1_100_000_000), Cost: usd(427)},
			{Name: "benchplan", Sessions: 6, Tokens: i64(500_000_000), Cost: usd(200)},
			{Name: "notes", Sessions: 5, Tokens: i64(170_000_000), Cost: usd(70)},
			{Name: "blog", Sessions: 4, Tokens: i64(110_000_000), Cost: usd(45)},
			{Name: "scripts", Sessions: 3, Tokens: i64(70_000_000), Cost: usd(28)},
			{Name: "infra", Sessions: 3, Tokens: i64(50_000_000), Cost: usd(20)},
			{Name: "dotfiles", Sessions: 2},
		},
		TotalProjects: 10,
		Composition: &stats.Composition{
			Total:      10_000_000_000,
			CacheRead:  stats.Segment{Tokens: 9_700_000_000, Share: 0.97},
			CacheWrite: stats.Segment{Tokens: 200_000_000, Share: 0.02},
			FreshInput: stats.Segment{Tokens: 30_000_000, Share: 0.003},
			Output:     stats.Segment{Tokens: 70_000_000, Share: 0.007},
		},
		Subagents: &stats.SubagentShare{Sessions: 497, Tokens: 7_700_000_000, Share: 0.77},
		Skills: []stats.Skill{
			{Name: "code-review", Sessions: 10}, {Name: "review-pr", Sessions: 4},
			{Name: "anthropic-skills:docs", Sessions: 3}, {Name: "cursor-guide", Sessions: 2},
		},
		DisplaySkills: []stats.Skill{
			{Name: "code-review", Sessions: 10}, {Name: "review-pr", Sessions: 4}, {Name: "docs", Sessions: 3}, {Name: "cursor-guide", Sessions: 2},
		},
		TotalSkills: 4, TotalDisplaySkills: 4,
		MCP: &stats.MCP{Scope: stats.MCPScope, TotalServers: 2, Servers: []stats.MCPServer{
			{Name: "github", Calls: 41, Sessions: 12}, {Name: "linear", Calls: 12, Sessions: 4},
		}},
		Highlights: stats.Highlights{
			BusiestDay:       &stats.BusiestDay{Date: "2026-09-27", Sessions: 70},
			FavoriteModel:    &stats.FavoriteModel{Label: "opus-5", By: "cost"},
			CostliestSession: &stats.CostliestSession{SessionID: "abc", Harness: "claude", Project: "styleprofile", Cost: usd(564), Subagents: 38, Drivers: []string{stats.DriverLongContext, stats.DriverSubagents}},
			ToolErrors:       &stats.ToolErrors{Errors: 210, Results: 4200, Rate: 0.05, Sessions: 70, UnknownSessions: 13},
			MonthRank:        &stats.MonthRank{Month: "2026-09", Rank: 1, Of: 6, Tokens: 10_000_000_000},
		},
		HeadsUp: []stats.Note{
			{Kind: stats.NoteSubagentShare, Share: f64(0.77), Tokens: i64(7_700_000_000), Runs: intp(497)},
			{Kind: stats.NoteCostliestSession, Cost: &stats.Cost{USD: f64(564)}, CostShare: f64(0.14), Project: "styleprofile", Subagents: intp(38), Drivers: []string{stats.DriverLongContext, stats.DriverSubagents}},
			{Kind: stats.NoteUnmeteredSessions, Sessions: intp(10), ByAgent: []stats.AgentSessions{{Harness: "cursor", Label: "Cursor", Sessions: 8}, {Harness: "claude", Label: "Claude Code", Sessions: 2}}},
		},
	}
}

// withPrior gives every measure a previous period, spend and tokens lower
// (so their arrows point up) and sessions higher (down).
func withPrior(s stats.Stats) stats.Stats {
	set := func(m stats.Measure, previous float64) stats.Measure {
		m.Previous = f64(previous)
		if previous > 0 && m.Value != nil {
			m.ChangePct = f64((*m.Value - previous) / previous * 100)
		}
		return m
	}
	o := &s.Overview
	o.Sessions = set(o.Sessions, 121)
	o.Prompts = set(o.Prompts, 673)
	o.Tokens = set(o.Tokens, 8_500_000_000)
	o.Cost.Measure = set(o.Cost.Measure, 3380)
	o.ActiveDays = set(o.ActiveDays, 5)
	return s
}

// cursorOnlyStats is a window of Cursor sessions alone: no tokens, so no
// spend, models, composition or chart.
func cursorOnlyStats() stats.Stats {
	sessions, prompts := 8.0, 61.0
	daily := realisticDaily()
	for i := range daily {
		daily[i].Tokens, daily[i].Cost = nil, stats.Cost{}
		daily[i].Sessions = 0
	}
	daily[27].Sessions, daily[26].Sessions = 5, 3
	daily[27].Cost, daily[26].Cost = stats.Cost{}, stats.Cost{}
	return stats.Stats{
		Window: stats.Window{Days: 30, Timezone: "UTC", FirstDay: "2026-08-31", LastDay: "2026-09-29"},
		Prices: stats.PriceInfo{Version: "2026-09-29", AsOf: "2026-09-29", Currency: "USD"},
		Coverage: stats.Coverage{
			Sessions: 8, Agents: 1, UnknownTokensByAgent: map[string]int{"cursor": 8},
		},
		Daily: daily,
		Overview: stats.Overview{
			Sessions: stats.Measure{Value: &sessions}, Prompts: stats.Measure{Value: &prompts},
			ActiveDays: stats.Measure{Value: f64(2)}, DaysInWindow: 30, BestStreak: 2,
		},
		Agents:        []stats.Agent{{Harness: "cursor", Label: "Cursor", Sessions: 8, UnknownTokenSessions: 8, SessionShare: 1}},
		Projects:      []stats.Project{{Name: "agent-archive", Sessions: 5}, {Name: "dotfiles", Sessions: 3}},
		TotalProjects: 2,
		Highlights:    stats.Highlights{BusiestDay: &stats.BusiestDay{Date: "2026-09-27", Sessions: 5}},
		HeadsUp: []stats.Note{
			{Kind: stats.NoteUnmeteredSessions, Sessions: intp(8), ByAgent: []stats.AgentSessions{{Harness: "cursor", Label: "Cursor", Sessions: 8}}},
		},
	}
}

// saturatedStats has totals at the top of every number format: sums that
// saturated at the largest int64, and money past what is shown.
func saturatedStats() stats.Stats {
	s := realisticStats()
	huge := float64(math.MaxInt64)
	s.Overview.Tokens.Value = &huge
	s.Overview.Sessions.Value = f64(1e9)
	s.Overview.Prompts.Value = f64(1e12)
	s.Overview.Cost.Value = f64(2e12)
	s.PeakSpend.USD = 1e12
	s.Daily[27].Cost = usd(1e12)
	for i := range s.Agents {
		s.Agents[i].Tokens = i64(math.MaxInt64)
	}
	s.Composition.Total = math.MaxInt64
	s.Composition.CacheRead.Tokens = math.MaxInt64
	s.Subagents.Tokens = math.MaxInt64
	s.Models[0].Tokens = math.MaxInt64
	s.Models[0].Cost = usd(2e12)
	s.Projects[0].Tokens = i64(math.MaxInt64)
	s.Projects[0].Cost = usd(1e12)
	return s
}

// hostileStats has names that would break a screen if it printed them raw:
// escape sequences, bidirectional controls, wide and combining characters.
func hostileStats() stats.Stats {
	s := realisticStats()
	for i, name := range hostileNames {
		if i < len(s.Projects) {
			s.Projects[i].Name = name
		}
		if i < len(s.Models) {
			s.Models[i].Label = name
		}
	}
	s.DisplaySkills[0].Name = hostileNames[0]
	s.DisplaySkills[1].Name = hostileNames[6]
	s.MCP.Servers[0].Name = hostileNames[1]
	s.MCP.Scope = hostileNames[2]
	s.Agents[0].Label = hostileNames[6]
	s.Agents[1].Label = hostileNames[0]
	s.Agents[2].Label = hostileNames[1]
	s.Highlights.CostliestSession.Project = hostileNames[8]
	s.HeadsUp[1].Project = hostileNames[8]
	s.HeadsUp[2].ByAgent[0].Label = hostileNames[9]
	s.HeadsUp[2].ByAgent[1].Label = hostileNames[2]
	s.Prices.Version = hostileNames[0]
	return s
}
