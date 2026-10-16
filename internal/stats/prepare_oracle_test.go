package stats

// This oracle preserves the pre-preparation window and member-accounting
// algorithm at d914dfb2391dd5f8b18b2d182b17bf6f4269e256, with current native-child
// orphan classification. Keep it independent of Prepare and the model lookup cache.
import (
	"github.com/wangjohn/agent-archive/internal/archive"
	"math"
	"sort"
	"strings"
	"time"
)

func legacyCompute(sessions []archive.Metadata, opts Options) Stats {
	loc := opts.Location
	if loc == nil {
		loc = time.Local
	}
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
	table := opts.PriceTable
	if table.Version == "" && len(table.Models) == 0 {
		table = DefaultPriceTable()
	}
	if table.Currency == "" {
		table.Currency = "USD" // as ParsePriceTable defaults it
	}
	prices := table.index()

	units := legacyBuildUnits(sessions, loc, prices)
	now := opts.Now
	if now.IsZero() && len(units) > 0 {
		for _, u := range units {
			if u.root.CapturedAt.After(now) {
				now = u.root.CapturedAt
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
	modelRows := legacyModels(current, prices)
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
			Version: table.Version, AsOf: table.AsOf, Currency: table.Currency, Sources: table.Sources,
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

func legacyModels(current []*unit, prices priceIndex) []ModelRow {
	byLabel := map[string]*modelAcc{}
	for _, u := range current {
		seen := map[string]bool{}
		for _, use := range u.perModel {
			label := prices.label(use.id)
			acc := byLabel[label]
			if acc == nil {
				acc = &modelAcc{models: map[string]struct{}{}}
				byLabel[label] = acc
			}
			acc.models[use.id] = struct{}{}
			acc.tokens = satAdd(acc.tokens, use.set.total())
			acc.cost.add(use.cost)
			_, priced := prices[use.id]
			acc.priced = acc.priced || priced
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

func legacyBuildUnits(sessions []archive.Metadata, loc *time.Location, prices priceIndex) []*unit {
	metas := dedupe(sessions)
	byID := make(map[string]*archive.Metadata, len(metas))
	for _, m := range metas {
		if m.SessionID != "" {
			byID[m.SessionID] = m
		}
	}
	roots := newRootResolver(byID)
	units := map[*archive.Metadata]*unit{}
	var ordered []*unit
	get := func(root *archive.Metadata) *unit {
		if u, ok := units[root]; ok {
			return u
		}
		u := &unit{root: root}
		units[root] = u
		ordered = append(ordered, u)
		return u
	}
	for _, m := range metas {
		root := roots.resolve(m)
		u := get(root)
		if m != root {
			u.children = append(u.children, m)
		}
	}
	for _, u := range ordered {
		legacyFinish(u, loc, prices)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return metadataBefore(ordered[i].root, ordered[j].root) })
	return ordered
}

func legacyFinish(u *unit, loc *time.Location, prices priceIndex) {
	root := u.root
	u.capturedAt = root.CapturedAt
	u.sessionID = root.SessionID
	if root.Counts.Messages != nil {
		u.messages = *root.Counts.Messages
	}
	u.compacted = root.Counts.Compactions != nil && *root.Counts.Compactions > 0
	u.subagentCount = len(u.children)
	u.harness = archive.CanonicalHarness(root.Harness.Name)
	u.project = strings.TrimSpace(root.ProjectName)
	captured := root.CapturedAt.In(loc)
	u.day = dayNumber(civilOf(captured))
	u.month = captured.Year()*12 + int(captured.Month()) - 1
	u.orphan = root.IsChild()
	u.lacksParser014 = !parserAtLeast(root.Parser.Version, 0, 14, 0)
	if root.Counts.Turns != nil {
		turns := value(root.Counts.Turns)
		u.prompts = &turns
	}
	u.skills = map[string]struct{}{}
	u.mcp = map[string]int64{}
	merged := map[string]tokenSet{}
	for i, m := range u.members() {
		byModel, has, approximate := legacyMemberUsage(m)
		var memberTotal tokenSet
		for id, set := range byModel {
			all := merged[id]
			all.add(set)
			merged[id] = all
			memberTotal.add(set)
		}
		if has {
			u.hasData = true
			u.approximate = u.approximate || approximate
			if i == 0 {
				u.rootHasData = true
				u.rootTokens = memberTotal
			} else {
				u.childrenWithData++
				u.childTokens = satAdd(u.childTokens, memberTotal.total())
			}
		}
		u.addToolErrors(m)
		for _, skill := range m.SkillsUsed {
			if name := strings.TrimSpace(skill.Name); name != "" {
				u.skills[name] = struct{}{}
			}
		}
		for _, call := range m.MCPCalls {
			if name := strings.TrimSpace(call.Name); name != "" && call.Count > 0 {
				u.mcp[name] = satAdd(u.mcp[name], int64(call.Count))
			}
		}
	}
	for _, id := range sortedModels(merged) {
		use := modelUse{id: id, set: merged[id]}
		if usd, ok := prices.price(id, use.set); ok {
			use.cost = costAcc{usd: usd, priced: true, approximate: u.approximate}
		} else {
			use.cost = costAcc{unpriced: use.set.total(), approximate: u.approximate}
		}
		u.perModel = append(u.perModel, use)
		u.tokens.add(use.set)
		u.cost.add(use.cost)
	}
}

func legacyMemberUsage(m *archive.Metadata) (byModel map[string]tokenSet, has, approximate bool) {
	harness := archive.CanonicalHarness(m.Harness.Name)
	byModel = map[string]tokenSet{}
	if len(m.ModelTokens) > 0 {
		for _, entry := range m.ModelTokens {
			set, ok := normalizedTokens(harness, entry.InputTokens, entry.OutputTokens, entry.CacheReadTokens, entry.CacheWriteTokens, entry.ReasoningTokens)
			if !ok {
				continue
			}
			id := NormalizeModel(entry.Model)
			if id == "" {
				id = archive.UnknownModel
			}
			merged := byModel[id]
			merged.add(set)
			byModel[id] = merged
			has = true
		}
		return byModel, has, false
	}
	c := m.Counts
	set, ok := normalizedTokens(harness, c.InputTokens, c.OutputTokens, c.CacheReadTokens, c.CacheWriteTokens, c.ReasoningTokens)
	if !ok {
		return byModel, false, false
	}
	byModel[mainModel(m, NormalizeModel)] = set
	return byModel, true, true
}

func (u *unit) members() []*archive.Metadata {
	return append([]*archive.Metadata{u.root}, u.children...)
}

// sortedModels lists a map's model ids in a fixed order, so float sums do not
// depend on map iteration.
func sortedModels(byModel map[string]tokenSet) []string {
	ids := make([]string, 0, len(byModel))
	for id := range byModel {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
