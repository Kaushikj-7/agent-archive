package cli

import (
	"fmt"
	"strings"

	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statsfmt"
)

// detailPage is everything the overview leaves out: the numbers against the
// previous period, the agents, the day-by-day spend, what the tokens were,
// the facts behind the highlights, every skill and MCP server, and the notes
// that say what the numbers rest on. A --by day, week or month table comes
// under it.
func (p *statsPrinter) detailPage() [][]string {
	return [][]string{
		p.header(pageDetail),
		p.numbersTable(),
		p.agentsTable(false),
		p.dailySpend(),
		p.composition(),
		p.highlights(),
		p.usage(),
		p.headsUp(),
		p.grouped(),
		p.detailNotes(),
		p.footer("--view overview"),
	}
}

// periodHeads are the names of the window and the period before it as the
// heads of the numbers table: "last 30d" and "prior 30d".
func (p *statsPrinter) periodHeads() (this, prior string) {
	if p.s.Window.Days == 1 {
		return "today", "day before"
	}
	return fmt.Sprintf("last %dd", p.s.Window.Days), fmt.Sprintf("prior %dd", p.s.Window.Days)
}

// numbersTable is the headline numbers with the previous period's and the
// change. The previous columns appear only when the previous period had some
// number above zero.
func (p *statsPrinter) numbersTable() []string {
	o := p.s.Overview
	type row struct {
		label string
		m     stats.Measure
		text  func(float64) string
	}
	count := func(v float64) string { return statsfmt.CommaInt(statsfmt.RoundInt(v)) }
	rows := []row{
		{"Est. spend", o.Cost.Measure, func(v float64) string { return p.estimate(v) }},
		{"Sessions", o.Sessions, count},
		{"Prompts", o.Prompts, count},
		{"Tokens", o.Tokens, func(v float64) string { return statsfmt.TokenCount(statsfmt.RoundInt(v)) }},
		{"Active days", o.ActiveDays, func(v float64) string { return fmt.Sprintf("%d of %d", statsfmt.RoundInt(v), o.DaysInWindow) }},
	}
	// The previous period's columns appear only when it had something: a
	// column of zeros and blanks says nothing more than the change's absence.
	hasPrior := false
	for _, r := range rows {
		hasPrior = hasPrior || (r.m.Previous != nil && *r.m.Previous > 0)
	}
	var labels, now, before, change []string
	for _, r := range rows {
		labels = append(labels, r.label)
		text := "unknown"
		if r.m.Value != nil {
			text = r.text(*r.m.Value)
			if r.label == "Est. spend" && o.Cost.Partial {
				text += "+"
			}
		}
		now = append(now, text)
		prev := ""
		if r.m.Previous != nil {
			prev = r.text(*r.m.Previous)
		}
		before = append(before, prev)
		change = append(change, p.changeText(r.m))
	}
	this, prior := p.periodHeads()
	cols := []tableCol{{head: this, cells: now}}
	if hasPrior {
		cols = append(cols, tableCol{head: prior, cells: before, drop: true}, tableCol{head: "change", cells: change})
	}
	lines := p.table("OVERVIEW", labels, nil, cols)
	if cache := p.s.Overview.CacheShare; cache != nil {
		lines = append(lines, p.dim(statsfmt.Percent(*cache)+" of tokens were served from cache."))
	}
	return lines
}

// agentsTable is one row per agent: sessions, share, tokens, spend and cache
// hit rate. Cursor's tokens and spend are unknown. With bars, a bar for each
// agent's share of the sessions in its color.
func (p *statsPrinter) agentsTable(withBar bool) []string {
	agents := p.s.Agents
	if len(agents) == 0 {
		return nil
	}
	var labels, sessions, share, tokens, costs, hit []string
	bar := &tableBar{width: 20}
	for _, a := range agents {
		code := agentCode(a.Harness)
		labels = append(labels, p.paint(code, p.g.bullet)+" "+nameOf(a.Label))
		bar.shares = append(bar.shares, a.SessionShare)
		bar.codes = append(bar.codes, code)
		sessions = append(sessions, statsfmt.CommaInt(int64(a.Sessions)))
		share = append(share, statsfmt.Percent(a.SessionShare))
		tokens = append(tokens, tokensText(a.Tokens))
		costs = append(costs, p.spend(a.Cost, a.Tokens))
		hit = append(hit, "n/a")
		if a.CacheHitRate != nil {
			hit[len(hit)-1] = statsfmt.Percent(*a.CacheHitRate)
		}
	}
	if !withBar {
		bar = nil
	}
	return p.table("AGENTS", labels, bar, []tableCol{
		{head: "sessions", cells: sessions},
		{head: "share", cells: share, drop: true},
		{head: "tokens", cells: tokens},
		{head: "est. cost", cells: costs},
		{head: "cache hit", cells: hit, drop: true},
	})
}

// composition is what the tokens were: cache reads, cache writes, fresh input
// and output, as a stacked bar and a legend that names each part and its
// share, so the colors are never the only thing that tells them apart.
func (p *statsPrinter) composition() []string {
	c := p.s.Composition
	if c == nil || c.Total == 0 {
		return nil
	}
	type part struct {
		label string
		role  statsRole
		stats.Segment
	}
	parts := []part{
		{"Cache read", roleCacheRead, c.CacheRead},
		{"Cache write", roleCacheWrite, c.CacheWrite},
		{"Fresh input", roleFreshInput, c.FreshInput},
		{"Output", roleOutput, c.Output},
	}
	segments := make([]segment, len(parts))
	for i, pt := range parts {
		segments[i] = segment{pt.Share, statsRoleCodes[pt.role]}
	}
	lines := []string{p.bold("WHAT USED YOUR TOKENS"), p.stackedBar(segments, min(max(p.width-30, 20), 60))}
	for _, pt := range parts {
		lines = append(lines, fmt.Sprintf("%s %s  %4s  %s", p.role(pt.role, p.g.bullet), padRight(pt.label, len("Cache write")),
			statsfmt.Percent(pt.Share), statsfmt.TokenCount(pt.Tokens)))
	}
	if c.ReasoningOfOutput != nil && *c.ReasoningOfOutput > 0 {
		lines = append(lines, p.dim("Output includes "+statsfmt.TokenCount(*c.ReasoningOfOutput)+" reasoning tokens."))
	}
	if sub := p.s.Subagents; sub != nil {
		lines = append(lines, fmt.Sprintf("Subagents  %s of tokens (%s) in %s", statsfmt.Percent(sub.Share), statsfmt.TokenCount(sub.Tokens),
			plural(p.s.Coverage.SubagentSessions, "run")))
	}
	return lines
}

// highlights are single facts about the window: streaks, the busiest day, the
// favorite model, the month's rank, the tool error rate and the costliest
// session.
func (p *statsPrinter) highlights() []string {
	h := p.s.Highlights
	o := p.s.Overview
	type item struct {
		label string
		text  string
	}
	var items []item
	if text := p.streakText(o); text != "" {
		items = append(items, item{"Streak", text})
	}
	if h.BusiestDay != nil {
		items = append(items, item{"Busiest day", fmt.Sprintf("%s (%s)", p.dayLabel(h.BusiestDay.Date), plural(h.BusiestDay.Sessions, "session"))})
	}
	if h.FavoriteModel != nil {
		fav := truncateVisible(clean(h.FavoriteModel.Label), statsNameLimit)
		if h.FavoriteModel.By == "tokens" {
			fav += " (by tokens)"
		}
		items = append(items, item{"Favorite model", fav})
	}
	if m := h.MonthRank; m != nil && m.Of > 1 {
		items = append(items, item{"This month", "the " + ordinalHeaviest(m.Rank) + p.monthsOf(m) + ", by tokens so far"})
	}
	if t := h.ToolErrors; t != nil {
		text := fmt.Sprintf("%s of %s tool results flagged as errors, %s measured", statsfmt.RatePercent(t.Rate), statsfmt.CommaInt(t.Results), plural(t.Sessions, "session"))
		if t.UnknownSessions > 0 {
			text += fmt.Sprintf(" (%d do not record them)", t.UnknownSessions)
		}
		items = append(items, item{"Tool errors", text})
	}
	if c := h.CostliestSession; c != nil && c.Cost.USD != nil {
		text := p.estimate(*c.Cost.USD)
		if c.Project != "" {
			text += " " + p.g.sep + " " + projectLabel(c.Project)
		}
		if drivers := driversText(c.Drivers, c.Subagents, c.CacheHitRate); drivers != "" {
			text += " " + p.g.sep + " " + drivers
		}
		items = append(items, item{"Costliest", text})
	}
	if len(items) == 0 {
		return nil
	}
	labelW := 0
	for _, it := range items {
		labelW = max(labelW, visibleWidth(it.label))
	}
	lines := []string{p.bold("HIGHLIGHTS")}
	for _, it := range items {
		lines = append(lines, p.hang(it.label, labelW, it.text, p.dim)...)
	}
	return lines
}

func (p *statsPrinter) streakText(o stats.Overview) string {
	switch {
	case o.CurrentStreak > 0:
		return fmt.Sprintf("%s in a row now (best %d)", plural(o.CurrentStreak, "day"), o.BestStreak)
	case o.BestStreak > 0:
		return fmt.Sprintf("none now (best %s)", plural(o.BestStreak, "day"))
	}
	return ""
}

func (p *statsPrinter) monthsOf(m *stats.MonthRank) string {
	if m.Of >= stats.MonthsCompared {
		return fmt.Sprintf(" of the last %d months", m.Of)
	}
	return fmt.Sprintf(" of the %d months with token data", m.Of)
}

// ordinalHeaviest is a month's rank by tokens in words: "heaviest",
// "2nd-heaviest".
func ordinalHeaviest(rank int) string {
	if rank <= 1 {
		return "heaviest"
	}
	return statsfmt.Ordinal(rank) + "-heaviest"
}

// usage lists every skill and MCP server the window has (up to a bound), with
// what their counts mean.
func (p *statsPrinter) usage() []string {
	skills := p.skillsRow(statsMaxUseRows)
	mcp := p.mcpRow(statsMaxUseRows)
	if len(skills) == 0 && len(mcp) == 0 {
		return nil
	}
	lines := []string{p.bold("SKILLS AND MCP")}
	lines = append(lines, skills...)
	lines = append(lines, mcp...)
	var notes []string
	switch {
	case len(skills) > 0 && len(mcp) > 0:
		notes = append(notes, "Skills count the sessions that used each one; MCP counts calls.")
	case len(skills) > 0:
		notes = append(notes, "Skills count the sessions that used each one.")
	default:
		notes = append(notes, "MCP counts calls.")
	}
	for _, note := range notes {
		lines = append(lines, p.dimAll(p.wrap(note))...)
	}
	return lines
}

// grouped is the --by breakdown by day, week or month.
func (p *statsPrinter) grouped() []string {
	g := p.s.Groups
	if g == nil || len(g.Rows) == 0 {
		return nil
	}
	title := "BY " + strings.ToUpper(clean(string(g.By)))
	rows := g.Rows
	more := 0
	if len(rows) > statsMaxGroupRows {
		more = len(rows) - statsMaxGroupRows
		if g.By == stats.GroupProject {
			rows = rows[:statsMaxGroupRows]
		} else {
			// Chronological rows: keep the newest.
			rows = rows[more:]
		}
	}
	var labels, sessions, prompts, tokens, costs []string
	for _, r := range rows {
		if g.By == stats.GroupProject {
			labels = append(labels, projectLabel(r.Key))
		} else {
			labels = append(labels, r.Key)
		}
		sessions = append(sessions, statsfmt.CommaInt(int64(r.Sessions)))
		prompts = append(prompts, "unknown")
		if r.Prompts != nil {
			prompts[len(prompts)-1] = statsfmt.CommaInt(*r.Prompts)
		}
		tokens = append(tokens, tokensText(r.Tokens))
		costs = append(costs, p.spend(r.Cost, r.Tokens))
	}
	lines := p.table(title, labels, nil, []tableCol{
		{head: "sessions", cells: sessions},
		{head: "prompts", cells: prompts, drop: true},
		{head: "tokens", cells: tokens},
		{head: "est. cost", cells: costs},
	})
	if g.By == stats.GroupWeek {
		lines = append(lines, p.dim("Weeks start on Monday."))
	}
	switch {
	case more > 0 && g.By == stats.GroupProject:
		lines = append(lines, p.dimAll(p.packAtoms(0, 0, allInAtoms(fmt.Sprintf("+ %d more", more), allProjectGroupsHint, p.width)))...)
	case more > 0:
		head := fmt.Sprintf("%d earlier rows not shown", more)
		lines = append(lines, p.dimAll(p.packAtoms(0, 0, allInAtoms(head, "--json --by "+string(g.By), p.width)))...)
	}
	return lines
}

// detailNotes say what the numbers rest on: their scope, how much of the
// window has token data, how sessions were priced, and what the marks mean.
func (p *statsPrinter) detailNotes() []string {
	s := p.s
	cov := s.Coverage
	notes := []string{"Scope: this archive only."}
	notes = append(notes, fmt.Sprintf("%s of %s report token counts.", statsfmt.CommaInt(int64(cov.SessionsWithTokens)), plural(cov.Sessions, "session")))
	if unknown := p.unknownTokenText(); unknown != "" {
		notes = append(notes, unknown)
	}
	if cov.SessionsPricedAtMainModel > 0 {
		notes = append(notes, plural(cov.SessionsPricedAtMainModel, "session")+" priced at the session's main model, with no per-model split (metadata from before parser 0.14.0).")
	}
	if s.Prices.Version != "" {
		text := "Prices " + clean(s.Prices.Version) + ", as of " + clean(s.Prices.AsOf)
		if s.Prices.Overridden {
			text += ", with your --prices file applied"
		}
		notes = append(notes, text+".")
	}
	notes = append(notes, "~ marks an estimate at list price.")
	if p.partialSpend() || p.anyPartial() {
		text := "+ leaves out tokens of models the price table does not list" + p.unpricedModels() + "."
		if p.s.Coverage.ObservationOnlySessions > 0 && p.s.Overview.Cost.UnpricedTokens == 0 {
			text = "+ marks partial priced coverage."
		}
		if cov.ObservationOnlySessions > 0 {
			text += fmt.Sprintf(" Priced subset also excludes %d observation-only sessions.", cov.ObservationOnlySessions)
		}
		notes = append(notes, text)
	}
	if m := s.MCP; m != nil {
		notes = append(notes, "MCP: "+clean(m.Scope))
	}
	var lines []string
	for _, note := range notes {
		lines = append(lines, p.wrap(note)...)
	}
	return append([]string{p.bold("NOTES")}, p.dimAll(lines)...)
}

// anyPartial is whether any cost the page shows leaves out unpriced tokens.
func (p *statsPrinter) anyPartial() bool {
	for _, m := range p.s.Models {
		if !m.Priced || m.Cost.Partial {
			return true
		}
	}
	return false
}

// unknownTokenText names the agents whose sessions report no tokens, which
// the totals leave out.
func (p *statsPrinter) unknownTokenText() string {
	var parts []string
	for _, a := range p.s.Agents {
		if a.UnknownTokenSessions > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", nameOf(a.Label), a.UnknownTokenSessions))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return p.observationAccountingContext("Sessions with no token data are left out of token and cost totals (" + strings.Join(parts, ", ") + ").")
}

// unpricedNames lists the models the window used that the price table does not
// list, the first few of them, and how many of those names there are.
func (p *statsPrinter) unpricedNames() (string, int) {
	var names []string
	for _, r := range p.s.Models {
		if !r.Priced || r.Cost.USD == nil {
			names = append(names, truncateVisible(clean(r.Label), statsNameLimit))
		}
	}
	total := len(names)
	if total > 3 {
		names = append(names[:3], p.g.ellipsis)
	}
	return strings.Join(names, ", "), total
}

// unpricedModels is unpricedNames in parentheses, or nothing when every model
// is priced.
func (p *statsPrinter) unpricedModels() string {
	if names, total := p.unpricedNames(); total > 0 {
		return " (" + names + ")"
	}
	return ""
}
