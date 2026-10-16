package cli

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statsfmt"
)

// Rows the overview shows of each list before it says how many more there are.
// A list one row past the limit is shown whole: "+ 1 more" saves no line.
const (
	overviewProjects = 4
	overviewModels   = 3
	overviewSkills   = 5
	overviewMCP      = 5
)

// overviewPage is the default screen: the headline numbers, who did the work,
// what it cost each day, where it went, what was used most and what deserves
// a look. Everything else is one key or flag away.
func (p *statsPrinter) overviewPage() [][]string {
	return [][]string{
		p.header(pageOverview),
		p.headline(),
		p.agentsBar(),
		p.dailySpend(),
		p.whereItWent(),
		p.mostUsed(),
		p.headsUp(),
		p.coverageNotes(),
		p.footer("--detail for more", "--by project", "--html"),
	}
}

// headCell is one headline number and the line of context under it.
type headCell struct {
	value string
	sub   string
}

// headlineCells are the headline trio: spend, sessions and tokens, each with
// one line of context.
func (p *statsPrinter) headlineCells() [3]headCell {
	return [3]headCell{p.spendCell(), p.sessionsCell(), p.tokensCell()}
}

func (p *statsPrinter) spendCell() headCell {
	o := p.s.Overview
	switch {
	case o.Cost.Value != nil:
		text := p.estimate(*o.Cost.Value)
		if o.Cost.Partial {
			text += "+"
		}
		sub := p.deltaText(o.Cost.Measure)
		if sub == "" {
			sub = p.dim("at list price")
		}
		return headCell{p.bold(text), sub}
	case o.Tokens.Value == nil:
		text := "needs token data"
		if p.s.TokenObservations != nil {
			text = "needs qualified accounting"
		}
		return headCell{p.bold("spend unknown"), p.dim(text)}
	}
	return headCell{p.bold("spend unknown"), p.dim("no price for its models")}
}

func (p *statsPrinter) sessionsCell() headCell {
	o := p.s.Overview
	value, sub := p.bold("sessions unknown"), ""
	if o.Sessions.Value != nil {
		value = p.bold(count(statsfmt.RoundInt(*o.Sessions.Value), "session"))
	}
	if o.Prompts.Value != nil {
		sub = p.dim(count(statsfmt.RoundInt(*o.Prompts.Value), "prompt"))
	}
	return headCell{value, sub}
}

func (p *statsPrinter) tokensCell() headCell {
	o := p.s.Overview
	if o.Tokens.Value == nil {
		return headCell{p.bold("tokens unknown"), p.dim("none recorded")}
	}
	sub := ""
	if o.CacheShare != nil {
		sub = p.dim(statsfmt.Percent(*o.CacheShare) + " served from cache")
	}
	return headCell{p.bold(statsfmt.TokenCount(statsfmt.RoundInt(*o.Tokens.Value)) + " tokens"), sub}
}

// deltaText is a measure's change against the previous period: an arrow, the
// percentage and what it is against, and nothing when there is no previous
// period to compare (a previous of zero has no percentage; it is not "new").
// An arrow up is amber and down is green: more spend is not an error.
func (p *statsPrinter) deltaText(m stats.Measure) string {
	change := p.changeText(m)
	if change == "" {
		return ""
	}
	_, prior := p.windowNames()
	return change + p.dim(" vs "+prior)
}

// changeText is the arrow and percentage alone, colored. A change of more
// than statsfmt.MaxShownChange percent reads ">999%": against next to nothing,
// the exact figure only measures how little there was.
func (p *statsPrinter) changeText(m stats.Measure) string {
	if m.Value == nil || m.Previous == nil || m.ChangePct == nil || math.IsNaN(*m.ChangePct) || math.IsInf(*m.ChangePct, 0) {
		return ""
	}
	switch dir, size := statsfmt.Change(*m.ChangePct); dir {
	case 1:
		return p.role(roleDeltaUp, p.g.up+" "+size+"%")
	case -1:
		return p.role(roleDeltaDown, p.g.down+" "+size+"%")
	}
	return p.dim("no change")
}

// headline is the trio of big numbers. In three columns when they fit the
// terminal (spread across it when there is room), else one line each.
func (p *statsPrinter) headline() []string {
	cells := p.headlineCells()
	const indent = 2
	third := (p.width - indent) / 3
	natural := indent
	spread := true
	for i, c := range cells {
		w := max(visibleWidth(c.value), visibleWidth(c.sub))
		natural += w
		if i < 2 {
			natural += 3
		}
		if w+1 > third {
			spread = false
		}
	}
	switch {
	case spread:
		return p.headlineColumns(cells, indent, []int{third, third, 0})
	case natural <= p.width:
		widths := make([]int, 3)
		for i, c := range cells[:2] {
			widths[i] = max(visibleWidth(c.value), visibleWidth(c.sub)) + 3
		}
		return p.headlineColumns(cells, indent, widths)
	}
	valueW := 0
	for _, c := range cells {
		valueW = max(valueW, visibleWidth(c.value))
	}
	var lines []string
	for _, c := range cells {
		lines = append(lines, strings.TrimRight(strings.Repeat(" ", indent)+padRight(c.value, valueW)+"  "+c.sub, " "))
	}
	return lines
}

// headlineColumns lays the cells out in columns of the given widths (the last
// is as wide as it needs), two lines each.
func (p *statsPrinter) headlineColumns(cells [3]headCell, indent int, widths []int) []string {
	var top, sub strings.Builder
	top.WriteString(strings.Repeat(" ", indent))
	sub.WriteString(strings.Repeat(" ", indent))
	for i, c := range cells {
		top.WriteString(padRight(c.value, widths[i]))
		sub.WriteString(padRight(c.sub, widths[i]))
	}
	lines := []string{strings.TrimRight(top.String(), " ")}
	if line := strings.TrimRight(sub.String(), " "); strings.TrimSpace(line) != "" {
		lines = append(lines, line)
	}
	return lines
}

// agentsLabelWidth is the width of "AGENTS" and its gap: the bar's column.
const agentsLabelWidth = len("AGENTS") + 2

// agentsBar is one bar split among the agents by their share of the sessions,
// with a legend of names and shares under it.
func (p *statsPrinter) agentsBar() []string {
	var segments []segment
	var items []string
	for _, a := range p.s.Agents {
		if a.Sessions == 0 {
			continue
		}
		code := agentCode(a.Harness)
		segments = append(segments, segment{a.SessionShare, code})
		items = append(items, p.paint(code, p.g.bullet)+" "+nameOf(a.Label)+" "+statsfmt.Percent(a.SessionShare))
	}
	if len(segments) == 0 {
		return nil
	}
	items = append(items, p.dim("of sessions"))
	barW := min(max(p.width-30, 20), 60)
	lines := []string{p.bold("AGENTS") + "  " + p.stackedBar(segments, barW)}
	for i, line := range packItems(items, agentsLabelWidth, agentsLabelWidth, 3, p.width) {
		if i == 0 {
			line = strings.Repeat(" ", agentsLabelWidth) + line
		}
		lines = append(lines, line)
	}
	return lines
}

// rankRow is one row of a ranked list: a name, its number and a bar.
type rankRow struct {
	label string
	value string
	// share is the row's part of the largest (0 to 1); a negative share draws
	// no bar.
	share float64
	// code is the SGR code of the bar.
	code string
}

// rankRows draws a ranked list in colW columns: a name, a bar scaled to the
// largest row, and the number, right-aligned at the column's edge; the names
// take at least minLabelW columns and the numbers minValueW, so two lists can
// line up. Without
// room for a bar (or without bars, on a narrow terminal), it is a name and
// its number.
func (p *statsPrinter) rankRows(rows []rankRow, colW int, bars bool, minLabelW, minValueW int) []string {
	labelW, valueW := minLabelW, minValueW
	for _, r := range rows {
		labelW = max(labelW, visibleWidth(r.label))
		valueW = max(valueW, visibleWidth(r.value))
	}
	barW := 0
	if bars {
		labelW = min(labelW, 18)
		barW = min(colW-labelW-valueW-4, statsMaxBar)
		if barW < 4 {
			labelW = max(minLabelWidth, colW-valueW-4-4)
			barW = min(colW-labelW-valueW-4, statsMaxBar)
		}
		if barW < 4 {
			barW = 0
		}
	}
	if barW == 0 {
		labelW = min(labelW, max(colW-valueW-2, 1))
	}
	lines := make([]string, len(rows))
	for i, r := range rows {
		line := padRight(p.cut(r.label, labelW), labelW)
		if barW > 0 {
			line += "  " + p.bar(r.share, barW, r.code)
			line += "  " + padLeft(r.value, valueW)
		} else {
			line = padRight(line, colW-valueW) + padLeft(r.value, valueW)
		}
		lines[i] = strings.TrimRight(line, " ")
	}
	return lines
}

// moreLine says how many rows a list leaves out.
func (p *statsPrinter) moreLine(n int) string {
	return p.dim(fmt.Sprintf("+ %d more", n))
}

// projectRows are the projects as ranked rows, by spend; the bar is scaled to
// the largest spend among them.
func (p *statsPrinter) projectRows(projects []stats.Project) []rankRow {
	top := 0.0
	for _, pr := range projects {
		if pr.Cost.USD != nil {
			top = math.Max(top, *pr.Cost.USD)
		}
	}
	rows := make([]rankRow, len(projects))
	for i, pr := range projects {
		share := -1.0
		if pr.Cost.USD != nil && top > 0 {
			share = *pr.Cost.USD / top
		}
		rows[i] = rankRow{label: projectLabel(pr.Name), value: p.spend(pr.Cost, pr.Tokens), share: share, code: statsRoleCodes[roleProject]}
	}
	return rows
}

// modelRows are the model families as ranked rows, by spend, with the bar in
// the family's color.
func (p *statsPrinter) modelRows(models []stats.ModelRow) []rankRow {
	top := 0.0
	for _, m := range models {
		if m.Priced && m.Cost.USD != nil {
			top = math.Max(top, *m.Cost.USD)
		}
	}
	rows := make([]rankRow, len(models))
	for i, m := range models {
		value, share := "unpriced", -1.0
		if m.Priced && m.Cost.USD != nil {
			value = p.money(*m.Cost.USD)
			if m.Cost.Partial {
				value += "+"
			}
			if top > 0 {
				share = *m.Cost.USD / top
			}
		}
		rows[i] = rankRow{label: clean(m.Label), value: value, share: share, code: modelCode(m.Label)}
	}
	return rows
}

// projectLabel is a project's name as shown: sanitized and cut short, with a
// stand-in for sessions that carry none.
func projectLabel(name string) string {
	if name == "" {
		return "(no project)"
	}
	return truncateVisible(clean(name), statsNameLimit)
}

// limitRows is how many of n rows to show: all when at most limit+1, else
// limit.
func limitRows(n, limit int) int {
	if n <= limit+1 {
		return n
	}
	return limit
}

// whereItWent is the projects and the models by spend: two columns from 80
// terminal columns, stacked from 60, and plain rows below.
func (p *statsPrinter) whereItWent() []string {
	projects := p.s.Projects
	totalProjects := max(p.s.TotalProjects, len(projects))
	models := p.s.Models
	if len(projects) == 0 && len(models) == 0 {
		return nil
	}
	shownProjects := limitRows(len(projects), overviewProjects)
	shownModels := limitRows(len(models), overviewModels)
	bars := p.bars()
	var project, model []string
	if len(projects) > 0 {
		project = []string{p.dim("By project")}
	}
	if len(models) > 0 {
		model = []string{p.dim("By model")}
	}
	colW := p.width
	twoColumns := len(projects) > 0 && len(models) > 0 && p.width >= statsFullWidth
	if twoColumns {
		colW = (p.width - 4) / 2
	}
	projectRows := p.projectRows(projects[:shownProjects])
	modelRows := p.modelRows(models[:shownModels])
	// Stacked lists line their bars and their numbers up.
	labelW, valueW := 0, 0
	if !twoColumns {
		for _, r := range append(slices.Clone(projectRows), modelRows...) {
			labelW = max(labelW, min(visibleWidth(r.label), 18))
			valueW = max(valueW, visibleWidth(r.value))
		}
	}
	project = append(project, p.rankRows(projectRows, colW, bars, labelW, valueW)...)
	if more := totalProjects - shownProjects; more > 0 {
		project = append(project, p.moreLine(more))
	}
	model = append(model, p.rankRows(modelRows, colW, bars, labelW, valueW)...)
	if more := len(models) - shownModels; more > 0 {
		model = append(model, p.moreLine(more))
	}
	lines := []string{p.bold("WHERE IT WENT")}
	if !twoColumns {
		lines = append(lines, project...)
		if len(project) > 0 && len(model) > 0 {
			lines = append(lines, "")
		}
		return append(lines, model...)
	}
	for i := range max(len(project), len(model)) {
		left, right := "", ""
		if i < len(project) {
			left = project[i]
		}
		if i < len(model) {
			right = model[i]
		}
		lines = append(lines, strings.TrimRight(padRight(left, colW)+"    "+right, " "))
	}
	return lines
}

// useLabelWidth is the width of the "Skills" and "MCP" labels' column.
const useLabelWidth = len("Skills")

// mostUsed is the skills sessions used and the MCP servers they called, each
// row only when there is data for it.
func (p *statsPrinter) mostUsed() []string {
	skills := p.skillsRow(overviewSkills)
	mcp := p.mcpRow(overviewMCP)
	if len(skills) == 0 && len(mcp) == 0 {
		return nil
	}
	lines := []string{p.bold("MOST USED")}
	lines = append(lines, skills...)
	return append(lines, mcp...)
}

// skillsRow lists up to limit skills by the sessions that used them, by their
// display names.
func (p *statsPrinter) skillsRow(limit int) []string {
	skills := p.s.DisplaySkills
	if len(skills) == 0 {
		return nil
	}
	shown := skills[:min(len(skills), limit)]
	items := make([]string, len(shown))
	for i, sk := range shown {
		items[i] = fmt.Sprintf("%s %s", truncateVisible(clean(sk.Name), statsNameLimit), statsfmt.CommaInt(int64(sk.Sessions)))
	}
	unit := "sessions"
	if len(shown) == 1 && shown[0].Sessions == 1 {
		unit = "session"
	}
	atoms := p.usageAtoms(items, unit)
	if more := max(p.s.TotalDisplaySkills, len(skills)) - len(shown); more > 0 {
		atoms = append(endWith(atoms, ","), p.moreAtoms(more, allUsageHint, p.width-useLabelWidth-2)...)
	}
	return p.hangAtoms("Skills", useLabelWidth, atoms, p.dim)
}

// mcpRow lists up to limit MCP servers by their calls, with which agents the
// counts cover.
func (p *statsPrinter) mcpRow(limit int) []string {
	m := p.s.MCP
	if m == nil || len(m.Servers) == 0 {
		return nil
	}
	shown := m.Servers[:min(len(m.Servers), limit)]
	items := make([]string, len(shown))
	for i, srv := range shown {
		items[i] = fmt.Sprintf("%s %s", truncateVisible(clean(srv.Label()), statsNameLimit), statsfmt.CommaInt(srv.Calls))
	}
	unit := "calls"
	if len(shown) == 1 && shown[0].Calls == 1 {
		unit = "call"
	}
	atoms := p.usageAtoms(items, unit)
	if scope := mcpScopeText(m.Scope); scope != "" {
		atoms = append(atoms, strings.Fields("("+scope+")")...)
	}
	if more := max(m.TotalServers, len(m.Servers)) - len(shown); more > 0 {
		atoms = append(endWith(atoms, ","), p.moreAtoms(more, allUsageHint, p.width-useLabelWidth-2)...)
	}
	return p.hangAtoms("MCP", useLabelWidth, atoms, p.dim)
}

// mcpScopeText is which agents an MCP scope names: its first clause, as
// "Claude Code and Cursor only".
func mcpScopeText(scope string) string {
	first, _, _ := strings.Cut(clean(scope), ";")
	return strings.TrimSpace(first)
}

// headsUp is what deserves a look: at most three notes, each a bullet and a
// sentence. A note is words about numbers the engine found; none is shown
// when nothing applies.
func (p *statsPrinter) headsUp() []string {
	var lines []string
	for _, n := range p.s.HeadsUp {
		if n.Kind == stats.NoteUnmeteredSessions {
			continue
		}
		text := p.noteText(n)
		if text == "" {
			continue
		}
		wrapped := strings.Split(hangingIndent("  ", text, p.width), "\n")
		wrapped[0] = p.role(roleHeadsUp, p.g.bullet) + wrapped[0][1:]
		lines = append(lines, wrapped...)
	}
	if len(lines) == 0 {
		return nil
	}
	return append([]string{p.bold("FINDINGS")}, lines...)
}

// noteText words a heads-up note. Subagent work is "runs", never "sessions".
func (p *statsPrinter) noteText(n stats.Note) string {
	switch n.Kind {
	case stats.NoteSubagentShare:
		if n.Share == nil {
			return ""
		}
		text := statsfmt.Percent(*n.Share) + " of tokens came from subagents"
		if n.Runs != nil {
			text += " (" + plural(*n.Runs, "run") + ")"
		}
		return text
	case stats.NoteCostliestSession:
		return p.costliestText(n)
	case stats.NoteUnmeteredSessions:
		return p.unmeteredText(n)
	case stats.NoteLowCacheHit:
		if n.HitRate == nil {
			return ""
		}
		text := fmt.Sprintf("Cache hit rate is %s, below %s", statsfmt.Percent(*n.HitRate), statsfmt.Percent(stats.LowCacheHitRate))
		if n.InputTokens != nil {
			text += " across " + statsfmt.TokenCount(*n.InputTokens) + " input tokens"
		}
		return text
	}
	return ""
}

func (p *statsPrinter) costliestText(n stats.Note) string {
	if n.Cost == nil || n.Cost.USD == nil {
		return ""
	}
	parts := []string{"Costliest session " + p.estimate(*n.Cost.USD)}
	if n.Project != "" {
		parts = append(parts, projectLabel(n.Project))
	}
	subagents := 0
	if n.Subagents != nil {
		subagents = *n.Subagents
	}
	var cacheHit *float64
	if c := p.s.Highlights.CostliestSession; c != nil {
		cacheHit = c.CacheHitRate
	}
	if drivers := driversText(n.Drivers, subagents, cacheHit); drivers != "" {
		parts = append(parts, drivers)
	}
	return strings.Join(parts, " "+p.g.sep+" ")
}

// driversText says what likely made a session costly.
func driversText(drivers []string, subagents int, cacheHit *float64) string {
	var parts []string
	if slices.Contains(drivers, stats.DriverLongContext) {
		parts = append(parts, "long context")
	}
	if slices.Contains(drivers, stats.DriverSubagents) {
		parts = append(parts, plural(subagents, "subagent"))
	}
	if slices.Contains(drivers, stats.DriverLowCacheHit) && cacheHit != nil {
		parts = append(parts, statsfmt.Percent(*cacheHit)+" cache hit")
	}
	return strings.Join(parts, ", ")
}

func (p *statsPrinter) unmeteredText(n stats.Note) string {
	if n.Sessions == nil {
		return ""
	}
	text := plural(*n.Sessions, "session") + " have no token data"
	if *n.Sessions == 1 {
		text = "1 session has no token data"
	}
	text = p.observationAccountingContext(text)
	var by []string
	for _, a := range n.ByAgent {
		by = append(by, fmt.Sprintf("%s %d", nameOf(a.Label), a.Sessions))
	}
	if len(by) > 0 {
		text += " (" + strings.Join(by, ", ") + ")"
	}
	return text
}

// coverageNotes separates missing data from findings about how agents were used.
func (p *statsPrinter) coverageNotes() []string {
	var notes []string
	if text := p.unknownTokenText(); text != "" {
		notes = append(notes, text)
	}
	if len(notes) == 0 {
		return nil
	}
	lines := []string{p.bold("COVERAGE")}
	for _, note := range notes {
		lines = append(lines, p.dimAll(p.wrap(note))...)
	}
	return lines
}
