package stats

import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"time"
)

// Grouping is what Options.By breaks the window down by.
type Grouping string

// The groupings `stats --by` offers.
const (
	// GroupNone asks for no grouping.
	GroupNone Grouping = ""
	// GroupDay groups by calendar day.
	GroupDay Grouping = "day"
	// GroupWeek groups by calendar week. Weeks start on Monday (ISO 8601),
	// and a group is labelled by its Monday.
	GroupWeek Grouping = "week"
	// GroupMonth groups by calendar month, labelled "2006-01".
	GroupMonth Grouping = "month"
	// GroupProject groups by project name.
	GroupProject Grouping = "project"
)

// Options say what to compute. The zero value of every field but Now is a
// usable default.
type Options struct {
	// MCPServerNames maps recorded server IDs to locally configured display names.
	MCPServerNames map[string]string

	// Now is the caller's clock: the end of the window. Compute never reads
	// the system clock. A zero Now stands for the newest captured_at in the
	// input (the Unix epoch when the input is empty).
	Now time.Time
	// Days is the window's length in calendar days, ending with Now's day.
	// 0 means DefaultDays; the value is clamped to MaxDays.
	Days int
	// Location is the time zone days, weeks, months and streaks are
	// counted in. nil means the machine's local zone (time.Local).
	Location *time.Location
	// PriceTable prices tokens. The zero value means DefaultPriceTable.
	PriceTable PriceTable
	// TopN is how many rows top projects, skills and MCP servers keep.
	// 0 means DefaultTopN. AllRows overrides it.
	TopN int
	// AllRows returns every project, skill and MCP server the window has,
	// whatever TopN says, for the views that list them all. The Total counts
	// (TotalProjects, TotalSkills, TotalDisplaySkills, MCP.TotalServers)
	// say how many there are either way. It is a separate switch rather than
	// a negative TopN so a zero or negative TopN keeps meaning the default.
	// Models are never cut, so it does not change them.
	AllRows bool
	// By, when set, fills Stats.Groups with the window broken down by day,
	// week, month or project.
	By Grouping
}

// Window defaults and limits.
const (
	// DefaultDays is the window Options.Days == 0 means.
	DefaultDays = 30
	// MaxDays bounds Options.Days, so the daily series stays a bounded size.
	MaxDays = 3660
	// DefaultTopN is how many rows a top list keeps by default.
	DefaultTopN = 5
)

// Thresholds behind the costliest session's likely drivers. They are
// heuristics over summary counts, not measurements: the archive keeps no
// per-request context size.
const (
	// LongContextTokensPerMessage is the average number of input-side tokens
	// (fresh input, cache reads and cache writes) per message from which a
	// session counts as long-context. A session with a compaction counts
	// too: Claude Code compacts only when the context is full.
	LongContextTokensPerMessage = 100_000
	// SubagentShareThreshold is the share of a session's tokens its
	// subagents must account for to be named a driver.
	SubagentShareThreshold = 0.25
	// LowCacheHitRate is the cache-hit rate below which a session's cache use
	// is named a driver, provided it read or wrote at least
	// LowCacheMinInputTokens input-side tokens (a tiny session's rate says
	// nothing). The heads-up note NoteLowCacheHit uses the same two numbers
	// for the whole window.
	LowCacheHitRate = 0.6
	// LowCacheMinInputTokens is the input-side token floor for LowCacheHitRate.
	LowCacheMinInputTokens = 50_000
)

// Thresholds behind Stats.HeadsUp. SubagentShareThreshold (above) is the
// subagent note's, applied to the window's tokens, and LowCacheHitRate with
// LowCacheMinInputTokens is the cache note's.
const (
	// MaxHeadsUp is the most notes Stats.HeadsUp holds: the highest priority
	// ones, when more apply.
	MaxHeadsUp = 3
	// CostliestNoteMinShare is the share of the window's priced spend the
	// costliest session must reach to be worth a note, in a window of more
	// than one session.
	CostliestNoteMinShare = 0.10
	// CostliestNoteMinCost is the least the costliest session must have cost,
	// in the price table's currency, to be worth a note: when everything is
	// cheap, a large share of it is not news. The cost must exceed it.
	CostliestNoteMinCost = 1.0
)

// Driver codes for CostliestSession.Drivers.
const (
	DriverLongContext = "long_context"
	DriverSubagents   = "subagents"
	DriverLowCacheHit = "low_cache_hit"
)

// MonthsCompared is how many calendar months MonthRank ranks: the current
// month and the five before it.
const MonthsCompared = 6

// MCPScope says which agents MCP call counts cover. Codex's MCP calls do not
// name their server in what the archive retains, so they are not counted.
const MCPScope = "Claude Code and Cursor only; Codex MCP calls are not recorded."

// Stats is everything `agent-archive stats` shows, computed by Compute from
// session metadata. It marshals to JSON. Throughout, a null (nil pointer) is
// unknown, never zero: it means no session in scope reported the number. A
// section or field marked omitempty (Peak, PeakSpend, Composition, Subagents,
// Skills, DisplaySkills, MCP, Groups, each Highlights entry, PriceInfo's
// optional fields, and the fields of a Note that its kind does not use) is left
// out of the JSON when it has nothing to show, not sent as null; every other
// key is always present, and every other list is [] when empty, never null.
// Numbers are never NaN or infinite, which JSON cannot carry: a value that
// would be one is unknown (null) instead.
//
// Scope and sessions. Only the window's sessions count, by captured_at (what
// list and --since use), not started_at; an imported session's captured_at
// can be far from when it happened. A subagent session (one with a
// parent_session_id) is not a session of its own: its tokens, cost, tool
// results, skills and MCP calls roll up into its parent, and its parent's
// captured_at places it in time. Subagent prompts are not added to prompts.
// A subagent whose parent is not in the input (an orphan) is counted as a
// session of its own and reported in Coverage.OrphanSubagents.
type Stats struct {
	TokenObservations   *archive.TokenObservationSummary `json:"token_observations,omitempty"`
	ObservationSessions int                              `json:"observation_sessions,omitempty"`
	Window              Window                           `json:"window"`
	Prices              PriceInfo                        `json:"prices"`
	Coverage            Coverage                         `json:"coverage"`
	Daily               []Day                            `json:"daily"`
	Peak                *Peak                            `json:"peak,omitempty"`
	// PeakSpend is the day with the highest estimated cost. Nil when no day
	// had a priced cost above zero.
	PeakSpend *PeakSpend `json:"peak_spend,omitempty"`
	Overview  Overview   `json:"overview"`
	Agents    []Agent    `json:"agents"`
	Models    []ModelRow `json:"models"`
	// Projects are ranked by estimated cost, dearest first (a partly priced
	// project on the cost it has; a project with no priced cost after every
	// one that has one; ties by tokens, sessions, then name), and cut to the
	// top few after that, so a project left out never has a higher cost than
	// one kept. A partial or unpriced cost leaves tokens out: the overall
	// Overview.Cost and Models say so, whatever the cut left out.
	Projects []Project `json:"projects"`
	// TotalProjects is how many distinct projects the window has; Projects
	// keeps the top few of them.
	TotalProjects int `json:"total_projects"`
	// Composition is what the tokens were spent on. Nil when no session in
	// the window reported token counts.
	Composition *Composition `json:"composition,omitempty"`
	// Subagents is the tokens subagent sessions used. Nil when none did.
	Subagents *SubagentShare `json:"subagents,omitempty"`
	// Skills are the skills sessions used, by number of sessions, under the
	// names the sessions recorded (a plugin's skill is "plugin:skill").
	Skills []Skill `json:"skills,omitempty"`
	// DisplaySkills is the same skills for showing to a person: a plugin
	// prefix is stripped from each name (SkillDisplayName), and skills that
	// then share a name are one row, counted in the sessions that used any of
	// them (a session that used both counts once). Skills keeps the recorded
	// names; nothing else in the document changes with this.
	DisplaySkills []Skill `json:"display_skills,omitempty"`
	// TotalSkills is how many distinct recorded skill names the window has,
	// and TotalDisplaySkills how many distinct display names; Skills and
	// DisplaySkills keep the top few unless Options.AllRows is set.
	TotalSkills        int `json:"total_skills"`
	TotalDisplaySkills int `json:"total_display_skills"`
	// MCP are the MCP servers sessions called, by number of calls. Nil when
	// none were.
	MCP        *MCP       `json:"mcp,omitempty"`
	Highlights Highlights `json:"highlights"`
	// HeadsUp is what deserves the reader's attention, at most MaxHeadsUp
	// notes in priority order (see Note). [] when nothing does. Each note is
	// data only; the words are the renderer's.
	HeadsUp []Note `json:"heads_up"`
	// Groups is the window broken down as Options.By asked, nil without it.
	Groups *Groups `json:"groups,omitempty"`
}

// Window is the period the numbers cover and the one before it, which the
// overview's changes compare against.
type Window struct {
	// Days is the window's length in calendar days.
	Days int `json:"days"`
	// Timezone is the zone days were counted in.
	Timezone string `json:"timezone"`
	// From is the start of the window's first day; To the start of the day
	// after its last (the window is From <= captured_at < To).
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// FirstDay and LastDay are the window's days as "2006-01-02".
	FirstDay string `json:"first_day"`
	LastDay  string `json:"last_day"`
	// PreviousFrom and PreviousTo bound the equally long period before it.
	PreviousFrom time.Time `json:"previous_from"`
	PreviousTo   time.Time `json:"previous_to"`
}

// PriceInfo says which prices the cost estimates used.
type PriceInfo struct {
	Version    string   `json:"version"`
	AsOf       string   `json:"as_of"`
	Currency   string   `json:"currency"`
	Sources    []string `json:"sources,omitempty"`
	Notes      string   `json:"notes,omitempty"`
	Overridden bool     `json:"overridden,omitempty"`
}

// Coverage says how much of the window the numbers rest on.
type Coverage struct {
	ObservationOnlySessions int `json:"observation_only_sessions,omitempty"`
	// FirstRecordedDay is the earliest available session day, clamped to the
	// requested window start when earlier history exists. It is an evidence
	// boundary, not proof of continuous capture.
	FirstRecordedDay string `json:"first_recorded_day,omitempty"`

	// Sessions is the window's session count (subagents rolled into their
	// parents); Agents how many distinct agents they came from.
	Sessions int `json:"sessions"`
	Agents   int `json:"agents"`
	// SessionsWithTokens are those that report any token count.
	SessionsWithTokens int `json:"sessions_with_tokens"`
	// UnknownTokensByAgent counts, per agent, the sessions that report no
	// token count (Cursor records none).
	UnknownTokensByAgent map[string]int `json:"unknown_tokens_by_agent"`
	// SessionsBeforeParser014 counts sessions whose metadata predates the
	// 0.14.0 parser, so it has no per-model tokens, tool errors or MCP
	// calls. They refresh on the next metadata pass.
	SessionsBeforeParser014 int `json:"sessions_before_parser_0_14_0"`
	// SessionsPricedAtMainModel counts sessions with token counts but no
	// per-model split, priced entirely at the session's main model (the one
	// with the most turns).
	SessionsPricedAtMainModel int `json:"sessions_priced_at_main_model"`
	// SessionsWithoutToolErrors counts sessions whose tool errors are
	// unknown: Codex writes no error flag, and older metadata has none.
	SessionsWithoutToolErrors int `json:"sessions_without_tool_errors"`
	// SubagentSessions are the subagent sessions rolled into the counts
	// above; OrphanSubagents those whose parent is not in the input, which
	// are counted as sessions of their own.
	SubagentSessions int `json:"subagent_sessions"`
	OrphanSubagents  int `json:"orphan_subagents"`
}

// Day is one calendar day of the window.
type Day struct {
	// Date is "2006-01-02" in the window's time zone.
	Date     string `json:"date"`
	Sessions int    `json:"sessions"`
	// Tokens is the day's token total: 0 on a day without sessions, null on
	// a day whose sessions report no token counts.
	Tokens *int64 `json:"tokens"`
	// Cost is the day's estimated cost, priced as the overall cost is and
	// placed in the day the same way tokens are (a subagent's cost is on its
	// parent's day). usd is 0 on a day without sessions and null on a day
	// with nothing priced (sessions that report no tokens, or only tokens of
	// models the price table lacks, which partial and unpriced_tokens then
	// say). The days' costs add up to the overview's.
	Cost Cost `json:"cost"`
}

// Peak is the day with the most tokens (the earliest, on a tie).
type Peak struct {
	Date   string `json:"date"`
	Tokens int64  `json:"tokens"`
}

// PeakSpend is the day with the highest estimated cost (the earliest, on a
// tie).
type PeakSpend struct {
	Date string `json:"date"`
	// USD is the cost, in the price table's currency (the field keeps its
	// historical name, as Cost.USD does).
	USD float64 `json:"usd"`
}

// Measure is one overview number, the previous period's, and the change.
type Measure struct {
	Value    *float64 `json:"value"`
	Previous *float64 `json:"previous"`
	// ChangePct is (Value - Previous) / Previous * 100. Null when either
	// side is unknown or the previous is zero, where a percentage is
	// meaningless (say "new" instead).
	ChangePct *float64 `json:"change_pct"`
}

// CostMeasure is a Measure of estimated cost with what limits it.
type CostMeasure struct {
	ObservationOnlySessions int `json:"observation_only_sessions,omitempty"`
	Measure
	Partial        bool  `json:"partial"`
	Approximate    bool  `json:"approximate"`
	UnpricedTokens int64 `json:"unpriced_tokens"`
}

// Overview is the headline numbers, each against the previous period.
type Overview struct {
	Sessions Measure `json:"sessions"`
	// Prompts are human prompts (counts.turns) of sessions that report them.
	Prompts    Measure     `json:"prompts"`
	Tokens     Measure     `json:"tokens"`
	Cost       CostMeasure `json:"cost"`
	ActiveDays Measure     `json:"active_days"`
	// CacheShare is the part of all the window's tokens that were cache reads
	// (0 to 1), which is most of them in a long session. Null when no session
	// reports token counts, or none reports cache counts (a missing count is
	// not a zero share).
	CacheShare *float64 `json:"cache_share"`
	// DaysInWindow is Window.Days, for "24/30".
	DaysInWindow int `json:"days_in_window"`
	// CurrentStreak is the run of consecutive active days ending today, or
	// yesterday when today has no session yet (a streak is not broken until
	// a whole day passes). BestStreak is the longest run inside the window.
	CurrentStreak int `json:"current_streak"`
	BestStreak    int `json:"best_streak"`
}

// Cost is an estimate in the price table's currency, at list price.
type Cost struct {
	// ObservationOnlySessions counts members excluded from billing, including observed zero.
	ObservationOnlySessions int `json:"observation_only_sessions,omitempty"`
	// USD is null when no token in scope could be priced (the field keeps
	// its historical name whatever the currency).
	USD *float64 `json:"usd"`
	// Partial is set when some tokens were left out because their model is
	// not in the price table; UnpricedTokens is how many.
	Partial        bool  `json:"partial"`
	UnpricedTokens int64 `json:"unpriced_tokens"`
	// Approximate is set when some tokens belong to a session with no
	// per-model split and were priced at the session's main model.
	Approximate bool `json:"approximate"`
}

// Agent is one coding agent's share of the window.
type Agent struct {
	// Harness is the archive's name ("claude", "codex", "cursor"); Label the
	// name to show ("Claude Code").
	Harness  string `json:"harness"`
	Label    string `json:"label"`
	Sessions int    `json:"sessions"`
	// SessionsWithTokens are the sessions that report token counts;
	// UnknownTokenSessions those that do not.
	SessionsWithTokens   int `json:"sessions_with_tokens"`
	UnknownTokenSessions int `json:"unknown_token_sessions"`
	// Tokens is null when none of the agent's sessions report tokens.
	Tokens *int64 `json:"tokens"`
	Cost   Cost   `json:"cost"`
	// SessionShare is the agent's part of all sessions; TokenShare its part
	// of all known tokens (null when it has none).
	SessionShare float64  `json:"session_share"`
	TokenShare   *float64 `json:"token_share"`
	// CacheHitRate is cache reads over all input-side tokens (fresh input,
	// cache reads, cache writes); null when the agent reports none. It is in
	// the JSON only; the terminal view leaves it out.
	CacheHitRate *float64 `json:"cache_hit_rate"`
}

// ModelRow is the cost and tokens of one model family.
type ModelRow struct {
	// Label is the price entry's family ("opus"), or the model id itself
	// when the model is unpriced.
	Label string `json:"label"`
	// Models are the distinct model ids grouped under the label.
	Models   []string `json:"models"`
	Sessions int      `json:"sessions"`
	Tokens   int64    `json:"tokens"`
	Priced   bool     `json:"priced"`
	// Cost is null for an unpriced model.
	Cost Cost `json:"cost"`
	// CostShare is this row's part of all priced cost; null when unpriced.
	CostShare *float64 `json:"cost_share"`
}

// Project is one project's share of the window, by project name.
type Project struct {
	// Name is the project's name; empty when sessions carry none.
	Name     string `json:"name"`
	Sessions int    `json:"sessions"`
	Tokens   *int64 `json:"tokens"`
	Cost     Cost   `json:"cost"`
}

// Segment is one part of the token composition.
type Segment struct {
	Tokens int64   `json:"tokens"`
	Share  float64 `json:"share"`
}

// Composition splits the window's tokens four ways. Fresh input is input
// that was not read from or written to the cache (Codex's input minus its
// cached input and cache-write input: OpenAI reports both as parts of
// input_tokens, and a Codex record's total_tokens is input plus output), so
// the four add up to Total without counting anything twice. A Codex record
// whose cache counts exceed its input contradicts that; it has no fresh input
// (never a negative one) and keeps both cache counts, so nothing is dropped.
// ReasoningOfOutput is the part of Output spent reasoning: a subset, never
// an addition.
type Composition struct {
	Total             int64   `json:"total"`
	CacheRead         Segment `json:"cache_read"`
	CacheWrite        Segment `json:"cache_write"`
	FreshInput        Segment `json:"fresh_input"`
	Output            Segment `json:"output"`
	ReasoningOfOutput *int64  `json:"reasoning_of_output"`
}

// SubagentShare is the tokens subagents used, as part of all tokens.
type SubagentShare struct {
	Sessions int     `json:"sessions"`
	Tokens   int64   `json:"tokens"`
	Share    float64 `json:"share"`
}

// Skill is a skill and the number of sessions that used it (by native
// invocation or read inference). The archive does not record how often a
// session called a skill, so this is sessions, not calls.
type Skill struct {
	Name     string `json:"name"`
	Sessions int    `json:"sessions"`
}

// MCP lists the MCP servers called, by number of calls.
type MCP struct {
	// Scope says which agents the counts cover (MCPScope).
	Scope string `json:"scope"`
	// TotalServers is how many servers were called; Servers keeps the top few
	// unless Options.AllRows is set.
	TotalServers int         `json:"total_servers"`
	Servers      []MCPServer `json:"servers"`
}

// MCPServer is one MCP server's call count.
type MCPServer struct {
	// DisplayName is a friendly label; Name remains the recorded identity.
	DisplayName string `json:"display_name,omitempty"`

	Name     string `json:"name"`
	Calls    int64  `json:"calls"`
	Sessions int    `json:"sessions"`
}

// Highlights are the single facts the summary calls out. Each is nil when
// the data to state it is missing.
type Highlights struct {
	BusiestDay       *BusiestDay       `json:"busiest_day,omitempty"`
	FavoriteModel    *FavoriteModel    `json:"favorite_model,omitempty"`
	CostliestSession *CostliestSession `json:"costliest_session,omitempty"`
	ToolErrors       *ToolErrors       `json:"tool_errors,omitempty"`
	MonthRank        *MonthRank        `json:"month_rank,omitempty"`
}

// BusiestDay is the day with the most sessions (most tokens, then earliest,
// on a tie).
type BusiestDay struct {
	Date     string `json:"date"`
	Sessions int    `json:"sessions"`
}

// FavoriteModel is the model family at the top of cost by model, or, when no
// model in the window could be priced, of tokens.
type FavoriteModel struct {
	Label string `json:"label"`
	// By is "cost" or "tokens".
	By string `json:"by"`
}

// CostliestSession is the session with the highest estimated cost, subagents
// included, and what likely made it so. It names the session by its id so
// `show` can open it; nothing else identifying leaves the archive.
type CostliestSession struct {
	SessionID string   `json:"session_id"`
	Harness   string   `json:"harness"`
	Project   string   `json:"project"`
	Cost      Cost     `json:"cost"`
	Tokens    int64    `json:"tokens"`
	Subagents int      `json:"subagents"`
	Drivers   []string `json:"drivers"`
	// CacheHitRate is null when the session has no input-side tokens.
	CacheHitRate *float64 `json:"cache_hit_rate"`
	// AvgInputPerMessage is null when the message count is unknown.
	AvgInputPerMessage *int64 `json:"avg_input_tokens_per_message"`
}

// ToolErrors is the share of tool results the app flagged as errors, over
// the sessions that know it. Per-tool rates are not derivable: an error is
// counted per session, not per tool.
type ToolErrors struct {
	Errors  int64   `json:"errors"`
	Results int64   `json:"results"`
	Rate    float64 `json:"rate"`
	// Sessions are those it covers; UnknownSessions those left out because
	// they do not record errors.
	Sessions        int `json:"sessions"`
	UnknownSessions int `json:"unknown_sessions"`
}

// MonthRank ranks this month's tokens against the five months before it,
// over the whole input, not just the window. This month is month to date, so
// early in a month its rank is pessimistic.
type MonthRank struct {
	Month string `json:"month"`
	// Rank is 1 for the heaviest. Of counts this month and the previous
	// months that have any token data (at most MonthsCompared).
	Rank   int   `json:"rank"`
	Of     int   `json:"of"`
	Tokens int64 `json:"tokens"`
}

// Groups is the window broken down by Options.By.
type Groups struct {
	By Grouping `json:"by"`
	// Rows are chronological for day, week and month, and by estimated cost
	// for project (ranked as Stats.Projects is). Only groups with sessions
	// appear. A week or month cut by the window's edge covers only part of it.
	Rows []Group `json:"rows"`
}

// Group is one row of a grouping.
type Group struct {
	// Key is "2006-01-02" (a day, or a week's Monday), "2006-01" (a month),
	// or a project name.
	Key      string `json:"key"`
	Sessions int    `json:"sessions"`
	Prompts  *int64 `json:"prompts"`
	Tokens   *int64 `json:"tokens"`
	Cost     Cost   `json:"cost"`
}
