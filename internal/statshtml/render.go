// Package statshtml renders the numbers of `agent-archive stats` as one
// self-contained HTML page: inline styles and inline SVG, no script, no
// fonts and no request to anything else, so the file can be opened from
// disk, attached to a message or printed, and never phones anywhere.
//
// It is a pure function of the engine's stats.Stats and Options. It holds
// aggregates and names only: never prompts, transcript text, file paths or
// session IDs. Project, skill and MCP server names, and the names of models
// the built-in price table does not list, are replaced by stand-ins
// ("project A", "model A") unless Options.IncludeNames is set, and every name
// that came from a transcript (project, model, skill, MCP server) is cleaned
// of control characters and escaped for HTML by the template, which is the
// only way text reaches the page.
//
// The page has the shape of the terminal's default view: the headline trio
// (spend, sessions, tokens), the agents, daily spend, where it went, what was
// used most and what deserves a second look, with the detail under a
// divider.
package statshtml

import (
	"bytes"
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"

	_ "embed" // the page template and stylesheet are compiled in

	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statsfmt"
)

//go:embed page.html.tmpl
var pageTemplateSource string

//go:embed style.css
var styleSheet string

// pageTemplate is the page's template with the stylesheet set into its <style>
// element: the stylesheet is the template's own text, never a value passed in,
// so nothing is marked as trusted CSS.
var pageTemplate = template.Must(template.New("page").Parse(strings.Replace(pageTemplateSource, "/*STYLESHEET*/", styleSheet, 1)))

// contentSecurityPolicy is the page's own policy, in a meta tag: nothing may
// load from anywhere, no script may run, and only the page's inline styles
// apply. It is a second lock behind the escaping, so a name that somehow got
// through as markup still could not run or fetch anything.
const contentSecurityPolicy = "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'"

// Row limits keep a page a few hundred kilobytes however large the archive:
// the engine bounds the daily series (stats.MaxDays), and these bound the
// tables.
const (
	maxModelRows   = 8
	maxGroupRows   = 60
	maxProjectRows = 25
	// maxBars is the most bars the daily chart draws; a longer window
	// shows the busiest day of each run of days.
	maxBars = 120
)

// Filters are the filters the stats run applied, named in the heading so the
// page says which sessions it covers. They are the caller's own flags.
type Filters struct {
	Harness string
	Model   string
	// Origin is "imported" or "hook" when only those sessions were counted.
	Origin string
	// Replays is "include" when replay sessions were counted with the rest,
	// or "only" when nothing else was. Empty is the default: none counted.
	Replays string
}

// replaysText is how the heading names the Replays filters the command has.
var replaysText = map[string]string{
	"include": "replay sessions included",
	"only":    "replay sessions only",
}

// originText is how the heading names the Origin filters the command has.
var originText = map[string]string{
	"imported": "imported sessions",
	"hook":     "hook-captured sessions",
}

// Options say how to render.
type Options struct {
	// GeneratedAt is stamped in the footer; the zero time leaves the stamp
	// out. The renderer never reads the clock.
	GeneratedAt time.Time
	// IncludeNames shows the real names of projects, skills, MCP servers and
	// models the built-in price table does not list, which can identify a
	// client or an internal tool (a fine-tune id, a deployment name). By
	// default each is a stand-in ("project A", "skill A", "MCP server A",
	// "model A"), so the page can be shared. A model the built-in price table
	// lists ("opus", "gpt-5") is public and shown either way.
	IncludeNames bool
	// Filters are named in the heading.
	Filters Filters
	// EmptyMessage is shown when the window has no sessions; a default is
	// used when it is empty.
	EmptyMessage string
}

// Render returns the page for s.
func Render(s stats.Stats, opts Options) ([]byte, error) {
	b := builder{
		s: s, opts: opts,
		names:      newNamer(opts.IncludeNames, "project"),
		skills:     newNamer(opts.IncludeNames, "skill"),
		servers:    newNamer(opts.IncludeNames, "MCP server"),
		modelNames: newModelNamer(opts.IncludeNames, s.Models),
	}
	p := b.page()
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, p); err != nil {
		return nil, fmt.Errorf("render the stats page: %w", err)
	}
	return out.Bytes(), nil
}

// builder turns stats into the page's view model.
type builder struct {
	s    stats.Stats
	opts Options
	// names, skills and servers show the archive's project, skill and MCP
	// server names, or stand-ins for them.
	names   *namer
	skills  *namer
	servers *namer
	// models shows the model names the built-in price table lists, or
	// stand-ins for the others.
	modelNames *modelNamer
	cost       costFlags
}

// page builds the sections in the order the page shows them, which is also the
// order a stand-in letter is handed out in: "project A" is the first project
// the page names.
func (b *builder) page() page {
	s := b.s
	window := fmt.Sprintf("Last %d days", s.Window.Days)
	if s.Window.Days == 1 {
		window = "Today"
	}
	p := page{
		CSP:   contentSecurityPolicy,
		Title: "agent-archive stats · " + strings.ToLower(window),
	}
	b.header(&p, window)
	if s.Coverage.Sessions == 0 {
		p.Empty = b.opts.EmptyMessage
		if p.Empty == "" {
			p.Empty = fmt.Sprintf("No archived sessions in the last %d days (%s to %s). Try a longer window, for example agent-archive stats --days 90 --html.",
				s.Window.Days, plain(s.Window.FirstDay), plain(s.Window.LastDay))
		}
		b.footer(&p)
		return p
	}
	p.Hero = b.hero()
	p.AgentBar = b.agentBar()
	p.Daily, p.NoSpend = b.daily()
	p.Projects = b.projects()
	p.Models = b.models()
	p.MostUsed = b.mostUsed()
	p.HeadsUp = b.headsUp()
	p.Coverage = b.coverageNotes()
	p.Agents = b.agents()
	p.Tokens = b.tokens()
	p.Facts = b.facts()
	p.Groups = b.groups()
	b.footer(&p)
	return p
}

// header fills the title, subtitle and filters.
func (b *builder) header(p *page, window string) {
	s := b.s
	agents := "1 agent"
	if s.Coverage.Agents != 1 {
		agents = fmt.Sprintf("%d agents", s.Coverage.Agents)
	}
	p.Subtitle = []string{window, plain(s.Window.FirstDay) + " to " + plain(s.Window.LastDay), agents}
	f := b.opts.Filters
	var parts []string
	if f.Harness != "" {
		parts = append(parts, "harness "+clean(f.Harness))
	}
	if f.Model != "" {
		// A stand-in already reads "model A"; it is not "model model A".
		id := b.modelNames.id(f.Model)
		if !strings.HasPrefix(id, "model ") {
			id = "model " + id
		}
		parts = append(parts, id)
	}
	if f.Origin != "" {
		text, known := originText[f.Origin]
		if !known {
			text = clean(f.Origin) + " sessions"
		}
		parts = append(parts, text)
	}
	if f.Replays != "" {
		text, known := replaysText[f.Replays]
		if !known {
			text = "replays " + clean(f.Replays)
		}
		parts = append(parts, text)
	}
	if len(parts) > 0 {
		p.Filters = "Filtered to " + strings.Join(parts, ", ") + "."
	}
}

// hero is the headline trio: spend, sessions and tokens, each with what makes
// the number readable under it. The change against the previous period is
// spend's alone, and only when there was a previous period.
func (b *builder) hero() []heroStat {
	o := b.s.Overview
	value := "n/a"
	if o.Tokens.Value != nil {
		value = b.cost.costText(b.s.Prices.Currency, o.Cost.Value, o.Cost.Approximate, o.Cost.Partial, false)
	}
	glyph, spoken, dir := deltaText(o.Cost.Measure)
	vs := ""
	switch {
	case glyph == "":
	case b.s.Window.Days == 1:
		vs = "vs the day before"
	default:
		vs = fmt.Sprintf("vs prior %d days", b.s.Window.Days)
	}
	spend := heroStat{
		Label: "Spend", Value: value, Delta: glyph, DeltaSpoken: spoken, DeltaDir: dir, DeltaVs: vs,
		Lines: []string{"estimated at list price"},
	}
	var prompts []string
	if o.Prompts.Value != nil && finite(*o.Prompts.Value) {
		prompts = []string{plural(int(statsfmt.RoundInt(*o.Prompts.Value)), "prompt")}
	}
	var cache []string
	switch {
	case o.Tokens.Value == nil:
		cache = []string{"no session reports token counts"}
	case o.CacheShare != nil && finite(*o.CacheShare):
		cache = []string{statsfmt.Percent(*o.CacheShare) + " served from cache"}
	}
	return []heroStat{
		spend,
		{Label: "Sessions", Value: measureCount(o.Sessions), Lines: prompts},
		{Label: "Tokens", Value: measureTokens(o.Tokens), Lines: cache},
	}
}

func (b *builder) agents() *barTable {
	t := &barTable{ID: "agents", Title: "Agents", Heading: "Agent",
		Cols: []string{"Sessions", "Tokens", "Spend", "Cache hit"}}
	for _, a := range b.s.Agents {
		tokens, cost, cache := "unknown", "n/a", "unknown"
		if a.Tokens != nil {
			tokens = statsfmt.TokenCount(*a.Tokens)
			cost = b.cost.costText(b.s.Prices.Currency, a.Cost.USD, a.Cost.Approximate, a.Cost.Partial, false)
			cache = "n/a"
			if a.CacheHitRate != nil {
				cache = statsfmt.Percent(*a.CacheHitRate)
			}
		}
		t.Rows = append(t.Rows, barRow{
			Label: clean(a.Label),
			Cells: []string{statsfmt.CommaInt(int64(a.Sessions)) + " (" + statsfmt.Percent(a.SessionShare) + ")", tokens, cost, cache},
		})
	}
	return t
}

// spendShares are the bar lengths of rows by their estimated cost, each as a
// share of the dearest row; a row with no cost gets none.
func spendShares(costs []stats.Cost) []float64 {
	top := 0.0
	for _, c := range costs {
		if v, ok := spendOf(c); ok {
			top = math.Max(top, v)
		}
	}
	out := make([]float64, len(costs))
	for i, c := range costs {
		if v, ok := spendOf(c); ok && top > 0 {
			out[i] = v / top
		}
	}
	return out
}

func (b *builder) models() *barTable {
	rows := b.s.Models
	if len(rows) == 0 {
		return nil
	}
	t := &barTable{ID: "models", Title: "By model", Heading: "Model", HasBars: true,
		Cols: []string{"Spend", "Share"}}
	more := 0
	if len(rows) > maxModelRows {
		more = len(rows) - maxModelRows
		rows = rows[:maxModelRows]
	}
	costs := make([]stats.Cost, len(rows))
	for i, r := range rows {
		if r.Priced {
			costs[i] = r.Cost
		}
	}
	shares := spendShares(costs)
	for i, r := range rows {
		t.Rows = append(t.Rows, b.modelRow(r, shares[i]))
	}
	t.BarNote = "Bars show estimated cost, each against the dearest model."
	if more > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("and %d more models", more))
	}
	if b.modelNames.hidden {
		t.Notes = append(t.Notes, "Models the built-in price table does not list are replaced by letters in this file.")
	}
	return t
}

// modelRow is a model's row: its cost and share of the cost, or, for a model
// the price table does not list, its tokens and no bar (it has no cost to
// draw). bar is the row's bar length, 0 to 1.
func (b *builder) modelRow(r stats.ModelRow, bar float64) barRow {
	label := b.modelNames.label(r.Label)
	class := b.modelNames.class(r.Label)
	if !r.Priced || r.Cost.USD == nil {
		return barRow{Label: label, Class: class, Pct: "0%", Cells: []string{"unpriced", statsfmt.TokenCount(r.Tokens) + " tokens"}}
	}
	share := 0.0
	if r.CostShare != nil {
		share = *r.CostShare
	}
	cost := b.cost.costText(b.s.Prices.Currency, r.Cost.USD, r.Cost.Approximate, r.Cost.Partial, false)
	return barRow{Label: label, Class: class, Pct: pct(bar), Cells: []string{cost, statsfmt.Percent(share)}}
}

// projects is where the spend went by project, in the engine's order: dearest
// first, a project it could not price last. The engine keeps its top few
// projects by that order, so what the page leaves out is what it says: how many
// more projects there are (it does not say what they cost).
func (b *builder) projects() *barTable {
	rows := b.s.Projects
	if len(rows) == 0 {
		return nil
	}
	t := &barTable{ID: "projects", Title: "By project", Heading: "Project", HasBars: true,
		Cols: []string{"Spend", "Sessions"}}
	costs := make([]stats.Cost, len(rows))
	for i, r := range rows {
		costs[i] = r.Cost
	}
	shares := spendShares(costs)
	for i, r := range rows {
		cost := "n/a"
		if r.Tokens != nil {
			cost = b.cost.costText(b.s.Prices.Currency, r.Cost.USD, r.Cost.Approximate, r.Cost.Partial, false)
		}
		t.Rows = append(t.Rows, barRow{
			Label: b.names.project(r.Name), Class: "project", Pct: pct(shares[i]),
			Cells: []string{cost, statsfmt.CommaInt(int64(r.Sessions))},
		})
	}
	t.BarNote = "Bars show estimated cost, each against the dearest project listed."
	if more := b.s.TotalProjects - len(rows); more > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("and %d more projects, not shown", more))
	}
	if !b.opts.IncludeNames {
		t.Notes = append(t.Notes, "Project names are replaced by letters in this file.")
	}
	return t
}

// mostUsed is the skills used in the most sessions and the MCP servers called
// most. A plugin's skill is listed under its bare name (the engine merges the
// spellings). It is nil when neither was used.
func (b *builder) mostUsed() *mostUsed {
	m := &mostUsed{}
	for _, sk := range b.s.DisplaySkills {
		m.Skills = append(m.Skills, nameCount{Name: b.skills.name(sk.Name), Count: plural(sk.Sessions, "session")})
	}
	if c := b.s.MCP; c != nil {
		for _, srv := range c.Servers {
			m.MCP = append(m.MCP, nameCount{Name: b.serverLabel(srv), Count: callCount(srv.Calls)})
		}
	}
	if len(m.Skills)+len(m.MCP) == 0 {
		return nil
	}
	switch {
	case len(m.Skills) > 0 && len(m.MCP) > 0:
		m.Notes = append(m.Notes, "Skills count the sessions that used each one; MCP counts calls.")
	case len(m.Skills) > 0:
		m.Notes = append(m.Notes, "Skills count the sessions that used each one.")
	default:
		m.Notes = append(m.Notes, "MCP counts calls.")
	}
	if more := b.s.TotalDisplaySkills - len(m.Skills); more > 0 && len(m.Skills) > 0 {
		m.Notes = append(m.Notes, fmt.Sprintf("and %d more skills", more))
	}
	if c := b.s.MCP; c != nil && len(m.MCP) > 0 {
		if more := c.TotalServers - len(m.MCP); more > 0 {
			m.Notes = append(m.Notes, fmt.Sprintf("and %d more MCP servers", more))
		}
		m.Notes = append(m.Notes, "MCP: "+plain(c.Scope))
	}
	if !b.opts.IncludeNames {
		m.Notes = append(m.Notes, "Skill and MCP server names are replaced by letters in this file.")
	}
	return m
}

// headsUp is the engine's notes as sentences, in its order. The engine sends
// data only and the words are here; a note with a number missing is left out
// rather than worded around it. Subagent runs are runs, never sessions.
func (b *builder) headsUp() []string {
	var out []string
	for _, n := range b.s.HeadsUp {
		if n.Kind == stats.NoteUnmeteredSessions {
			continue
		}
		var text string
		switch n.Kind {
		case stats.NoteSubagentShare:
			text = b.subagentNote(n)
		case stats.NoteCostliestSession:
			text = b.costliestNote(n)
		case stats.NoteUnmeteredSessions:
			text = b.observationAccountingContext(unmeteredNote(n))
		case stats.NoteLowCacheHit:
			text = lowCacheNote(n)
		}
		if text != "" {
			out = append(out, text)
		}
	}
	return out
}

func (b *builder) subagentNote(n stats.Note) string {
	if n.Share == nil || n.Runs == nil {
		return ""
	}
	return statsfmt.Percent(*n.Share) + " of tokens came from subagents (" + plural(*n.Runs, "run") + ")"
}

func (b *builder) costliestNote(n stats.Note) string {
	if n.Cost == nil || n.Cost.USD == nil {
		return ""
	}
	text := "Costliest session " + b.cost.costText(b.s.Prices.Currency, n.Cost.USD, n.Cost.Approximate, n.Cost.Partial, true)
	if n.Project != "" {
		text += " · " + b.names.project(n.Project)
	}
	subagents := 0
	if n.Subagents != nil {
		subagents = *n.Subagents
	}
	var hit *float64
	if c := b.s.Highlights.CostliestSession; c != nil {
		hit = c.CacheHitRate
	}
	if drivers := driversText(n.Drivers, subagents, hit); drivers != "" {
		text += " · " + drivers
	}
	return text
}

func unmeteredNote(n stats.Note) string {
	if n.Sessions == nil {
		return ""
	}
	text := plural(*n.Sessions, "session") + " have no token data"
	if *n.Sessions == 1 {
		text = "1 session has no token data"
	}
	var by []string
	for _, a := range n.ByAgent {
		by = append(by, clean(a.Label)+" "+statsfmt.CommaInt(int64(a.Sessions)))
	}
	if len(by) > 0 {
		text += " (" + strings.Join(by, ", ") + ")"
	}
	return text
}

func lowCacheNote(n stats.Note) string {
	if n.HitRate == nil || n.InputTokens == nil {
		return ""
	}
	return "Cache hit rate is " + statsfmt.Percent(*n.HitRate) + " over " + statsfmt.TokenCount(*n.InputTokens) + " input-side tokens, which is low"
}

func (b *builder) groups() *barTable {
	g := b.s.Groups
	if g == nil || len(g.Rows) == 0 {
		return nil
	}
	// By is one of the engine's few groupings; it is cleaned like any text.
	by := clean(string(g.By))
	title := "By " + by
	heading := "Group"
	if runes := []rune(by); len(runes) > 0 {
		heading = strings.ToUpper(string(runes[:1])) + string(runes[1:])
	}
	if g.By == stats.GroupWeek {
		title += " (weeks start Monday)"
	}
	rows, limit := g.Rows, maxGroupRows
	if g.By == stats.GroupProject {
		limit = maxProjectRows
	}
	t := &barTable{ID: "groups", Title: title, Heading: heading,
		Cols: []string{"Sessions", "Prompts", "Tokens", "Spend"}}
	if len(rows) > limit {
		if g.By == stats.GroupProject {
			t.Notes = append(t.Notes, fmt.Sprintf("and %d more projects", len(rows)-limit))
			rows = rows[:limit]
		} else {
			// Chronological rows: keep the newest.
			t.Notes = append(t.Notes, fmt.Sprintf("%d earlier rows are not shown", len(rows)-limit))
			rows = rows[len(rows)-limit:]
		}
	}
	for _, r := range rows {
		label := clean(r.Key)
		if g.By == stats.GroupProject {
			label = b.names.project(r.Key)
		}
		prompts, tokens, cost := "unknown", "unknown", "n/a"
		if r.Prompts != nil {
			prompts = statsfmt.CommaInt(*r.Prompts)
		}
		if r.Tokens != nil {
			tokens = statsfmt.TokenCount(*r.Tokens)
			cost = b.cost.costText(b.s.Prices.Currency, r.Cost.USD, r.Cost.Approximate, r.Cost.Partial, false)
		}
		t.Rows = append(t.Rows, barRow{Label: label, Cells: []string{statsfmt.CommaInt(int64(r.Sessions)), prompts, tokens, cost}})
	}
	return t
}

// facts are the single facts of the detail: the days active with their
// streaks, and the highlights the engine found.
func (b *builder) facts() []highlight {
	o := b.s.Overview
	h := b.s.Highlights
	var out []highlight
	if o.ActiveDays.Value != nil && finite(*o.ActiveDays.Value) {
		text := fmt.Sprintf("%d of %d days", int(*o.ActiveDays.Value+0.5), o.DaysInWindow)
		switch {
		case o.CurrentStreak > 0:
			text += fmt.Sprintf(" · streak %s (best %d)", plural(o.CurrentStreak, "day"), o.BestStreak)
		case o.BestStreak > 0:
			text += fmt.Sprintf(" · no current streak (best %d)", o.BestStreak)
		}
		out = append(out, highlight{"Days active", text})
	}
	if h.BusiestDay != nil {
		out = append(out, highlight{"Busiest day", fmt.Sprintf("%s (%s)", dayLabel(h.BusiestDay.Date, b.s.Window.Days), plural(h.BusiestDay.Sessions, "session"))})
	}
	if h.FavoriteModel != nil {
		text := b.modelNames.label(h.FavoriteModel.Label)
		if h.FavoriteModel.By == "tokens" {
			text += " (by tokens)"
		}
		out = append(out, highlight{"Favorite model", text})
	}
	if c := h.CostliestSession; c != nil && c.Cost.USD != nil {
		text := b.cost.costText(b.s.Prices.Currency, c.Cost.USD, c.Cost.Approximate, c.Cost.Partial, true)
		if c.Project != "" {
			text += " · " + b.names.project(c.Project)
		}
		if drivers := driversText(c.Drivers, c.Subagents, c.CacheHitRate); drivers != "" {
			text += " (" + drivers + ")"
		}
		out = append(out, highlight{"Costliest session", text})
	}
	if t := h.ToolErrors; t != nil {
		text := fmt.Sprintf("%s of %s tool results flagged as errors, %s measured", statsfmt.RatePercent(t.Rate), statsfmt.CommaInt(t.Results), plural(t.Sessions, "session"))
		if t.UnknownSessions > 0 {
			text += fmt.Sprintf(" (%d do not record them)", t.UnknownSessions)
		}
		out = append(out, highlight{"Tool errors", text})
	}
	if m := h.MonthRank; m != nil && m.Of > 1 {
		of := fmt.Sprintf(" of the %d months with token data", m.Of)
		if m.Of >= stats.MonthsCompared {
			of = fmt.Sprintf(" of the last %d months", m.Of)
		}
		out = append(out, highlight{"Month rank", "This month so far is your " + ordinalHeaviest(m.Rank) + of})
	}
	return out
}

func (b *builder) footer(p *page) {
	s := b.s
	var lines []string
	scope := "Scope: this archive only."
	var unknown []string
	for _, a := range s.Agents {
		if a.UnknownTokenSessions > 0 {
			unknown = append(unknown, fmt.Sprintf("%s: %d", clean(a.Label), a.UnknownTokenSessions))
		}
	}
	if len(unknown) > 0 {
		scope += " Sessions with no token data are left out of token and cost totals (" + strings.Join(unknown, ", ") + ")."
	}
	lines = append(lines, b.observationAccountingContext(scope))
	if s.Coverage.Sessions > 0 {
		lines = append(lines, joinSentences(
			fmt.Sprintf("Token data: %s of %s.", statsfmt.CommaInt(int64(s.Coverage.SessionsWithTokens)), plural(s.Coverage.Sessions, "session")),
			subagentNote(s.Coverage),
			fmt.Sprintf("Days are counted in %s; sessions are placed by when they were captured.", plain(s.Window.Timezone)),
		))
	}
	cost := "Cost is an estimate at list price, not a bill."
	switch {
	case s.Prices.Overridden && !b.opts.IncludeNames:
		// The version and date are text from the person's own price file,
		// which can name a client or a contract; the built-in table's are
		// public. A shareable page says only that the file was used.
		cost += " Prices are from your own price file (its version is left out; --include-names shows it)."
	case s.Prices.Version != "":
		cost += fmt.Sprintf(" Prices %s, as of %s", plain(s.Prices.Version), plain(s.Prices.AsOf))
		if s.Prices.Overridden {
			cost += ", with your own price file applied"
		}
		cost += "."
	}
	lines = append(lines, cost)
	if b.cost.approx {
		lines = append(lines, fmt.Sprintf("~ priced at the session's main model for %s with no per-model split (metadata from before parser 0.14.0).",
			plural(s.Coverage.SessionsPricedAtMainModel, "session")))
	}
	if b.cost.partial {
		text := "+ marks partial priced coverage."
		if s.Coverage.ObservationOnlySessions == 0 || s.Overview.Cost.UnpricedTokens > 0 {
			text = "+ leaves out tokens of models the price table does not list" + b.unpricedModels() + "."
		}
		if s.Coverage.ObservationOnlySessions > 0 {
			text += fmt.Sprintf(" Priced subset also excludes %d observation-only sessions.", s.Coverage.ObservationOnlySessions)
		}
		lines = append(lines, text)
	}
	p.Footer.Lines = lines
	p.Footer.Privacy = "This page holds counts and names only: no prompts, transcript text, file paths or session IDs."
	if !b.opts.IncludeNames {
		p.Footer.Privacy += " Project, skill, MCP server and unrecognized model names are replaced by letters; run with --include-names to show them."
	}
	if !b.opts.GeneratedAt.IsZero() {
		p.Footer.Generated = "Generated " + b.opts.GeneratedAt.Format("2006-01-02 15:04 MST") + " by agent-archive stats --html."
	}
}

func subagentNote(c stats.Coverage) string {
	if c.SubagentSessions == 0 {
		return ""
	}
	note := plural(c.SubagentSessions, "subagent run") + " counted with their parent sessions."
	if c.OrphanSubagents > 0 {
		note += fmt.Sprintf(" %d of them have no parent in the archive and count as sessions of their own.", c.OrphanSubagents)
	}
	return note
}

// unpricedModels lists, in parentheses, the models the window used that the
// price table does not list.
func (b *builder) unpricedModels() string {
	var names []string
	for _, r := range b.s.Models {
		if !r.Priced || r.Cost.USD == nil {
			names = append(names, b.modelNames.label(r.Label))
		}
	}
	if len(names) == 0 {
		return ""
	}
	if len(names) > 3 {
		names = append(names[:3], "…")
	}
	return " (" + strings.Join(names, ", ") + ")"
}

func (b *builder) serverLabel(srv stats.MCPServer) string {
	if b.opts.IncludeNames {
		return clean(srv.Label())
	}
	return b.servers.name(srv.Name)
}

func (b *builder) coverageNotes() []string {
	var out []string
	if s := b.s.TokenObservations; s != nil {
		if s.Observed.Available() && s.Input != nil && s.Output != nil {
			out = append(out, fmt.Sprintf("Observed input %d, output %d across %d sessions; full usage, composition and cost unavailable.", *s.Input, *s.Output, b.s.ObservationSessions))
		} else {
			out = append(out, "Token observations unavailable: exact aggregate limit.")
		}
	}
	if n := b.s.Coverage.ObservationOnlySessions; n > 0 {
		out = append(out, fmt.Sprintf("Priced subset excludes %d observation-only sessions.", n))
	}
	for _, n := range b.s.HeadsUp {
		if n.Kind == stats.NoteUnmeteredSessions {
			if text := b.observationAccountingContext(unmeteredNote(n)); text != "" {
				out = append(out, text)
			}
		}
	}
	if text := b.s.ChartCoverageNote(); text != "" {
		out = append(out, plain(text))
	}
	return out
}

func (b *builder) observationAccountingContext(text string) string {
	if b.s.TokenObservations != nil {
		return strings.ReplaceAll(text, "no token data", "no qualified token accounting")
	}
	return text
}
