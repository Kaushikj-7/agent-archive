// Package stats computes the numbers behind `agent-archive stats` from
// session metadata: tokens, sessions, estimated cost, agents, models,
// projects, and highlights, over a window of calendar days with the previous
// period beside it.
//
// It is pure. Compute reads no clock, file, environment or network: the
// caller passes the metadata, the time, the time zone and the price table,
// and gets a JSON-serializable Stats back, the same one for the same input
// however the input is ordered. TestStatsImportBoundary keeps it that way.
//
// Unknown is a state, never zero. A metadata field the archive could not
// record (Cursor's tokens, tool errors for Codex, every field a sidecar
// written before parser 0.14.0 lacks) stays nil all the way out, and the
// numbers built from it say how many sessions they rest on.
package stats

import (
	"math"
	"slices"
	"sort"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Compute derives Stats from session metadata. See Stats for what counts as a
// session and how the window is bounded, and Options for the knobs.
func Compute(sessions []archive.Metadata, opts Options) Stats {
	return Prepare(sessions, PrepareOptions{Location: opts.Location, PriceTable: opts.PriceTable}).Compute(opts)
}

// Compute derives a window from the prepared snapshot. Location and PriceTable
// in opts are ignored; prepare again to change either. Concurrent calls are safe.
func (p *Prepared) Compute(opts Options) Stats {
	loc, table, units := p.location, p.table, p.units
	days := opts.Days
	if days <= 0 {
		days = DefaultDays
	}
	days = min(days, MaxDays)
	topN := opts.TopN
	if topN <= 0 {
		topN = DefaultTopN
	}
	if opts.AllRows {
		topN = math.MaxInt
	}
	now := opts.Now
	if now.IsZero() && len(units) > 0 {
		for _, u := range units {
			if u.capturedAt.After(now) {
				now = u.capturedAt
			}
		}
	}
	if now.IsZero() {
		now = time.Unix(0, 0)
	}
	today := dayNumber(civilOf(now.In(loc)))
	first := today - days + 1

	var current, previous []*unit
	for _, u := range units {
		switch {
		case u.day >= first && u.day <= today:
			current = append(current, u)
		case u.day >= first-days && u.day < first:
			previous = append(previous, u)
		}
	}

	total := &bucket{}
	perDay := make([]bucket, days)
	active := make([]bool, days)
	for _, u := range current {
		total.add(u)
		perDay[u.day-first].add(u)
		active[u.day-first] = true
	}
	prev := &bucket{}
	prevDays := map[int]bool{}
	for _, u := range previous {
		prev.add(u)
		prevDays[u.day] = true
	}

	dailySeries, peak, peakSpend := daily(perDay, first)
	topProjects, projectCount := projects(current, topN)
	modelRows := models(current)
	var grouped *Groups
	if opts.By != GroupNone {
		grouped = groups(current, opts.By)
	}
	subagents := subagentShare(current, total)
	highlighted := highlights(current, units, perDay, first, modelRows, now, loc)
	cov := coverage(current)
	for _, u := range units {
		if u.day <= today && (cov.FirstRecordedDay == "" || dateString(max(first, u.day)) < cov.FirstRecordedDay) {
			cov.FirstRecordedDay = dateString(max(first, u.day))
		}
	}
	skillLists := skills(current, topN)
	out := Stats{
		Window: Window{
			Days: days, Timezone: loc.String(),
			From: startOfDay(first, loc), To: startOfDay(today+1, loc),
			FirstDay: dateString(first), LastDay: dateString(today),
			PreviousFrom: startOfDay(first-days, loc), PreviousTo: startOfDay(first, loc),
		},
		Prices: PriceInfo{
			Version: table.Version, AsOf: table.AsOf, Currency: table.Currency, Sources: slices.Clone(table.Sources),
			Notes: table.Notes, Overridden: table.Overridden,
		},
		Coverage:      cov,
		Daily:         dailySeries,
		Peak:          peak,
		PeakSpend:     peakSpend,
		Overview:      overview(total, prev, active, len(prevDays), days),
		Agents:        agents(current, total),
		Models:        modelRows,
		Projects:      topProjects,
		TotalProjects: projectCount,
		Composition:   composition(total),
		Subagents:     subagents,
		Skills:        skillLists.recorded,
		DisplaySkills: skillLists.display,
		MCP:           mcpServers(current, topN, opts.MCPServerNames),
		Highlights:    highlighted,
		HeadsUp:       headsUp(cov, total, subagents, highlighted.CostliestSession),
		Groups:        grouped,

		TotalSkills:        skillLists.recordedTotal,
		TotalDisplaySkills: skillLists.displayTotal,
	}
	return out
}

func coverage(current []*unit) Coverage {
	cov := Coverage{Sessions: len(current), UnknownTokensByAgent: map[string]int{}}
	agentsSeen := map[string]bool{}
	for _, u := range current {
		agentsSeen[u.harness] = true
		if u.hasData {
			cov.SessionsWithTokens++
		} else {
			cov.UnknownTokensByAgent[u.harness]++
		}
		if u.lacksParser014 {
			cov.SessionsBeforeParser014++
		}
		if u.approximate {
			cov.SessionsPricedAtMainModel++
		}
		if u.toolMembersKnown == 0 {
			cov.SessionsWithoutToolErrors++
		}
		cov.SubagentSessions += u.subagentCount
		if u.orphan {
			cov.OrphanSubagents++
		}
	}
	cov.Agents = len(agentsSeen)
	return cov
}

func daily(perDay []bucket, first int) ([]Day, *Peak, *PeakSpend) {
	out := make([]Day, len(perDay))
	var peak *Peak
	var peakSpend *PeakSpend
	for i := range perDay {
		b := &perDay[i]
		date := dateString(first + i)
		tokens := b.tokenTotal()
		cost := b.cost.cost()
		if b.sessions == 0 {
			// A day without sessions cost nothing: a known zero, as its tokens.
			zeroTokens, zeroCost := int64(0), 0.0
			tokens, cost.USD = &zeroTokens, &zeroCost
		}
		out[i] = Day{Date: date, Sessions: b.sessions, Tokens: tokens, Cost: cost}
		if tokens != nil && *tokens > 0 && (peak == nil || *tokens > peak.Tokens) {
			peak = &Peak{Date: date, Tokens: *tokens}
		}
		if cost.USD != nil && *cost.USD > 0 && (peakSpend == nil || *cost.USD > peakSpend.USD) {
			peakSpend = &PeakSpend{Date: date, USD: *cost.USD}
		}
	}
	return out, peak, peakSpend
}

func floatPtr[T int | int64](p *T) *float64 {
	if p == nil {
		return nil
	}
	v := float64(*p)
	return &v
}

func measure(value, previous *float64) Measure {
	var change *float64
	if value != nil && previous != nil && *previous != 0 {
		// A price table can make both sides tiny or huge; a change that
		// overflows is unknown, since JSON cannot carry Inf or NaN.
		if pct := (*value - *previous) / *previous * 100; !math.IsInf(pct, 0) && !math.IsNaN(pct) {
			change = &pct
		}
	}
	return Measure{Value: value, Previous: previous, ChangePct: change}
}

// cacheShare is cache reads over every token of the bucket, nil when no
// session reported tokens or none reported cache counts.
func cacheShare(b *bucket) *float64 {
	all := b.tokens.total()
	if b.dataSessions == 0 || !b.tokens.cacheKnown || all <= 0 {
		return nil
	}
	s := float64(b.tokens.read) / float64(all)
	return &s
}

func overview(cur, prev *bucket, active []bool, prevActive, days int) Overview {
	countF := func(n int) *float64 { v := float64(n); return &v }
	activeCount := 0
	for _, on := range active {
		if on {
			activeCount++
		}
	}
	current, best := streaks(active)
	costCur, costPrev := cur.cost.cost(), prev.cost.cost()
	return Overview{
		Sessions:   measure(countF(cur.sessions), countF(prev.sessions)),
		Prompts:    measure(floatPtr(cur.promptTotal()), floatPtr(prev.promptTotal())),
		Tokens:     measure(floatPtr(cur.tokenTotal()), floatPtr(prev.tokenTotal())),
		ActiveDays: measure(countF(activeCount), countF(prevActive)),
		CacheShare: cacheShare(cur),
		Cost: CostMeasure{
			Measure: measure(costCur.USD, costPrev.USD),
			Partial: costCur.Partial, Approximate: costCur.Approximate, UnpricedTokens: costCur.UnpricedTokens,
		},
		DaysInWindow: days, CurrentStreak: current, BestStreak: best,
	}
}

// harnessLabels are the names the terminal shows for the agents the archive
// captures. Any other harness shows under its own name.
var harnessLabels = map[string]string{
	archive.HarnessClaude: "Claude Code",
	archive.HarnessCodex:  "Codex",
	archive.HarnessCursor: "Cursor",
}

func harnessLabel(harness string) string {
	if label, ok := harnessLabels[harness]; ok {
		return label
	}
	if harness == "" {
		return "unknown"
	}
	return harness
}

func share(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

func agents(current []*unit, total *bucket) []Agent {
	byAgent := map[string]*bucket{}
	for _, u := range current {
		if byAgent[u.harness] == nil {
			byAgent[u.harness] = &bucket{}
		}
		byAgent[u.harness].add(u)
	}
	out := make([]Agent, 0, len(byAgent))
	for harness, b := range byAgent {
		var tokenShare, hitRate *float64
		if tokens := b.tokenTotal(); tokens != nil && total.dataSessions > 0 {
			s := share(*tokens, total.tokens.total())
			tokenShare = &s
		}
		if b.dataSessions > 0 {
			hitRate = cacheHitRate(&b.tokens)
		}
		out = append(out, Agent{
			Harness: harness, Label: harnessLabel(harness), Sessions: b.sessions,
			SessionsWithTokens: b.dataSessions, UnknownTokenSessions: b.sessions - b.dataSessions,
			Tokens: b.tokenTotal(), Cost: b.cost.cost(), SessionShare: share(int64(b.sessions), int64(total.sessions)),
			TokenShare: tokenShare, CacheHitRate: hitRate,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		return out[i].Harness < out[j].Harness
	})
	return out
}

type modelAcc struct {
	models   map[string]struct{}
	sessions int
	tokens   int64
	cost     costAcc
	priced   bool
}

func models(current []*unit) []ModelRow {
	byLabel := map[string]*modelAcc{}
	for _, u := range current {
		seen := map[string]bool{}
		for _, use := range u.perModel {
			label := use.label
			acc := byLabel[label]
			if acc == nil {
				acc = &modelAcc{models: map[string]struct{}{}}
				byLabel[label] = acc
			}
			acc.models[use.id] = struct{}{}
			acc.tokens = satAdd(acc.tokens, use.set.total())
			acc.cost.add(use.cost)
			acc.priced = acc.priced || use.priced
			if !seen[label] {
				seen[label] = true
				acc.sessions++
			}
		}
	}
	var pricedCost float64
	for _, label := range sortedKeys(byLabel) {
		if acc := byLabel[label]; acc.priced {
			pricedCost += acc.cost.usd
		}
	}
	out := make([]ModelRow, 0, len(byLabel))
	for _, label := range sortedKeys(byLabel) {
		acc := byLabel[label]
		ids := make([]string, 0, len(acc.models))
		for id := range acc.models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		var costShare *float64
		if acc.priced && pricedCost > 0 {
			s := acc.cost.usd / pricedCost
			costShare = &s
		}
		out = append(out, ModelRow{
			Label: label, Models: ids, Sessions: acc.sessions, Tokens: acc.tokens, Priced: acc.priced,
			Cost: acc.cost.cost(), CostShare: costShare,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Priced != b.Priced {
			return a.Priced
		}
		if a.Priced && a.Cost.USD != nil && b.Cost.USD != nil && *a.Cost.USD != *b.Cost.USD {
			return *a.Cost.USD > *b.Cost.USD
		}
		if a.Tokens != b.Tokens {
			return a.Tokens > b.Tokens
		}
		return a.Label < b.Label
	})
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func projects(current []*unit, topN int) ([]Project, int) {
	byName := map[string]*bucket{}
	for _, u := range current {
		if byName[u.project] == nil {
			byName[u.project] = &bucket{}
		}
		byName[u.project].add(u)
	}
	out := make([]Project, 0, len(byName))
	for _, name := range sortedKeys(byName) {
		b := byName[name]
		out = append(out, Project{Name: name, Sessions: b.sessions, Tokens: b.tokenTotal(), Cost: b.cost.cost()})
	}
	sort.SliceStable(out, func(i, j int) bool { return projectBefore(out[i], out[j]) })
	count := len(out)
	if len(out) > topN {
		out = out[:topN]
	}
	return out, count
}

// projectBefore ranks projects by spend, the order every list of them keeps
// and the one a top-N cut is made in: the dearest first, a partly priced
// project on the spend it does have, and a project with nothing priced after
// every one that has a price. Projects of the same spend (or with none) go by
// tokens (unknown last), then sessions, then name, so the order does not
// depend on the order the sessions arrive in.
func projectBefore(a, b Project) bool {
	av, aok := spendOf(a.Cost)
	bv, bok := spendOf(b.Cost)
	switch {
	case aok && !bok:
		return true
	case !aok && bok:
		return false
	case aok && av != bv:
		return av > bv
	}
	switch {
	case a.Tokens != nil && b.Tokens == nil:
		return true
	case a.Tokens == nil && b.Tokens != nil:
		return false
	case a.Tokens != nil && *a.Tokens != *b.Tokens:
		return *a.Tokens > *b.Tokens
	}
	if a.Sessions != b.Sessions {
		return a.Sessions > b.Sessions
	}
	return a.Name < b.Name
}

// spendOf is a cost's dollars, and whether it has any: an unpriced cost, or a
// number that is not one (which nothing produces, but which would break the
// order), has none.
func spendOf(c Cost) (float64, bool) {
	if c.USD == nil || math.IsNaN(*c.USD) {
		return 0, false
	}
	return *c.USD, true
}

func composition(total *bucket) *Composition {
	if total.dataSessions == 0 {
		return nil
	}
	t := total.tokens
	all := t.total()
	var reasoning *int64
	if t.reasoningKnown {
		known := t.reasoning
		reasoning = &known
	}
	return &Composition{
		Total:             all,
		CacheRead:         Segment{t.read, share(t.read, all)},
		CacheWrite:        Segment{t.write, share(t.write, all)},
		FreshInput:        Segment{t.fresh, share(t.fresh, all)},
		Output:            Segment{t.out, share(t.out, all)},
		ReasoningOfOutput: reasoning,
	}
}

func subagentShare(current []*unit, total *bucket) *SubagentShare {
	var tokens int64
	sessions := 0
	for _, u := range current {
		tokens = satAdd(tokens, u.childTokens)
		sessions += u.childrenWithData
	}
	if tokens == 0 || total.dataSessions == 0 {
		return nil
	}
	return &SubagentShare{Sessions: sessions, Tokens: tokens, Share: share(tokens, total.tokens.total())}
}

// skillLists is the skills sessions used, under the names they recorded and
// under their display names, each cut to the top few with the full count.
type skillLists struct {
	recorded      []Skill
	recordedTotal int
	display       []Skill
	displayTotal  int
}

// skills counts, per skill name, the sessions that used it. The display list
// merges names that are the same skill once a plugin prefix is stripped, and
// counts a session that used several of them once, so it is counted from the
// sessions rather than added up from the recorded counts.
func skills(current []*unit, topN int) skillLists {
	recorded := map[string]int{}
	display := map[string]int{}
	for _, u := range current {
		shown := map[string]struct{}{}
		for name := range u.skills {
			recorded[name]++
			shown[SkillDisplayName(name)] = struct{}{}
		}
		for name := range shown {
			display[name]++
		}
	}
	return skillLists{
		recorded: topSkills(recorded, topN), recordedTotal: len(recorded),
		display: topSkills(display, topN), displayTotal: len(display),
	}
}

// topSkills is the topN skills with the most sessions, the first by name on a
// tie; nil when there are none.
func topSkills(counts map[string]int, topN int) []Skill {
	out := make([]Skill, 0, len(counts))
	for _, name := range sortedKeys(counts) {
		out = append(out, Skill{Name: name, Sessions: counts[name]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sessions > out[j].Sessions })
	if len(out) > topN {
		out = out[:topN]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mcpServers(current []*unit, topN int, names map[string]string) *MCP {
	calls := map[string]int64{}
	sessions := map[string]int{}
	for _, u := range current {
		for server, n := range u.mcp {
			calls[server] = satAdd(calls[server], n)
			sessions[server]++
		}
	}
	if len(calls) == 0 {
		return nil
	}
	servers := make([]MCPServer, 0, len(calls))
	for _, name := range sortedKeys(calls) {
		servers = append(servers, MCPServer{Name: name, DisplayName: mcpDisplayName(name, names, len(servers)+1), Calls: calls[name], Sessions: sessions[name]})
	}
	sort.SliceStable(servers, func(i, j int) bool { return servers[i].Calls > servers[j].Calls })
	if len(servers) > topN {
		servers = servers[:topN]
	}
	return &MCP{Scope: MCPScope, TotalServers: len(calls), Servers: servers}
}
