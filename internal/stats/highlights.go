package stats

import (
	"sort"
	"time"
)

// highlights picks the facts the summary calls out. current is the window's
// units; all is every unit in the input, which the month ranking needs.
func highlights(current, all []*unit, perDay []bucket, first int, modelRows []ModelRow, now time.Time, loc *time.Location) Highlights {
	return Highlights{
		BusiestDay:       busiestDay(perDay, first),
		FavoriteModel:    favoriteModel(modelRows),
		CostliestSession: costliestSession(current),
		ToolErrors:       toolErrors(current),
		MonthRank:        monthRank(all, now, loc),
	}
}

// busiestDay is the day with the most sessions; a tie goes to the day with
// more tokens, then to the earlier day.
func busiestDay(perDay []bucket, first int) *BusiestDay {
	best := -1
	for i := range perDay {
		if perDay[i].sessions == 0 {
			continue
		}
		if best < 0 || perDay[i].sessions > perDay[best].sessions ||
			(perDay[i].sessions == perDay[best].sessions && perDay[i].tokens.total() > perDay[best].tokens.total()) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	return &BusiestDay{Date: dateString(first + best), Sessions: perDay[best].sessions}
}

// favoriteModel is the top row of cost by model. When nothing in the window
// could be priced it is the model with the most tokens instead, and says so.
func favoriteModel(rows []ModelRow) *FavoriteModel {
	if len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		if row.Priced {
			return &FavoriteModel{Label: row.Label, By: "cost"}
		}
	}
	best := rows[0]
	for _, row := range rows[1:] {
		if row.Tokens > best.Tokens {
			best = row
		}
	}
	return &FavoriteModel{Label: best.Label, By: "tokens"}
}

// costliestSession is the session with the highest estimated cost (the
// earliest on a tie), and which of the drivers it shows: long context, a
// large share of tokens in subagents, or a low cache-hit rate. Sessions with
// nothing priced cannot be costliest.
func costliestSession(current []*unit) *CostliestSession {
	var best *unit
	for _, u := range current {
		if !u.cost.priced {
			continue
		}
		if best == nil || u.cost.usd > best.cost.usd {
			best = u
		}
	}
	if best == nil {
		return nil
	}
	var perMessage *int64
	if best.rootHasData && best.messages > 0 {
		avg := best.rootTokens.inputSide() / int64(best.messages)
		perMessage = &avg
	}
	hitRate := cacheHitRate(&best.tokens)
	drivers := []string{}
	compacted := best.compacted
	if compacted || (perMessage != nil && *perMessage >= LongContextTokensPerMessage) {
		drivers = append(drivers, DriverLongContext)
	}
	if best.childTokens > 0 && share(best.childTokens, best.tokens.total()) >= SubagentShareThreshold {
		drivers = append(drivers, DriverSubagents)
	}
	if hitRate != nil && best.tokens.inputSide() >= LowCacheMinInputTokens && *hitRate < LowCacheHitRate {
		drivers = append(drivers, DriverLowCacheHit)
	}
	return &CostliestSession{
		SessionID: best.sessionID, Harness: best.harness, Project: best.project,
		Cost: best.cost.cost(), Tokens: best.tokens.total(), Subagents: best.subagentCount,
		Drivers: drivers, CacheHitRate: hitRate, AvgInputPerMessage: perMessage,
	}
}

// toolErrors is errors over results across the sessions that know both.
func toolErrors(current []*unit) *ToolErrors {
	var errors, results int64
	known, unknown := 0, 0
	for _, u := range current {
		errors = satAdd(errors, u.toolErrors)
		results = satAdd(results, u.toolResults)
		if u.toolMembersKnown > 0 {
			known++
		} else {
			unknown++
		}
	}
	// No tool results at all has no error rate (0 of 0 is not "0%").
	if known == 0 || results == 0 {
		return nil
	}
	rate := float64(errors) / float64(results)
	return &ToolErrors{Errors: errors, Results: results, Rate: rate, Sessions: known, UnknownSessions: unknown}
}

// monthRank ranks this calendar month's tokens against the five before it,
// over every unit given (not only the window). Months without token data are
// left out of the comparison rather than counted as zero.
func monthRank(all []*unit, now time.Time, loc *time.Location) *MonthRank {
	local := now.In(loc)
	today := dayNumber(civilOf(local))
	byMonth := map[int]int64{}
	for _, u := range all {
		// A session after Now (a skewed clock) is not part of the month so far.
		if u.hasData && u.day <= today {
			byMonth[u.month] = satAdd(byMonth[u.month], u.tokens.total())
		}
	}
	thisMonth := local.Year()*12 + int(local.Month()) - 1
	tokens, ok := byMonth[thisMonth]
	if !ok {
		return nil
	}
	rank, withData := 1, 0
	for back := 1; back < MonthsCompared; back++ {
		other, ok := byMonth[thisMonth-back]
		if !ok {
			continue
		}
		withData++
		if other > tokens {
			rank++
		}
	}
	if withData == 0 {
		return nil
	}
	return &MonthRank{Month: local.Format("2006-01"), Rank: rank, Of: 1 + withData, Tokens: tokens}
}

// groups breaks the window's units down by day, week, month or project.
func groups(current []*unit, by Grouping) *Groups {
	byKey := map[string]*bucket{}
	for _, u := range current {
		key := groupKey(u, by)
		if byKey[key] == nil {
			byKey[key] = &bucket{}
		}
		byKey[key].add(u)
	}
	rows := make([]Group, 0, len(byKey))
	for _, key := range sortedKeys(byKey) {
		b := byKey[key]
		rows = append(rows, Group{Key: key, Sessions: b.sessions, Prompts: b.promptTotal(), Tokens: b.tokenTotal(), Cost: b.cost.cost()})
	}
	if by == GroupProject {
		sort.SliceStable(rows, func(i, j int) bool {
			return projectBefore(rows[i].asProject(), rows[j].asProject())
		})
	}
	return &Groups{By: by, Rows: rows}
}

// asProject is a project group's row as the project it counts, to rank it as
// projects are ranked.
func (g Group) asProject() Project {
	return Project{Name: g.Key, Sessions: g.Sessions, Tokens: g.Tokens, Cost: g.Cost}
}

func groupKey(u *unit, by Grouping) string {
	switch by {
	case GroupDay:
		return dateString(u.day)
	case GroupWeek:
		return dateString(weekStart(u.day))
	case GroupMonth:
		return dateString(u.day)[:len("2006-01")]
	case GroupNone, GroupProject:
	}
	return u.project
}

// ParseGrouping reads the value of `--by`.
func ParseGrouping(s string) (Grouping, bool) {
	switch g := Grouping(s); g {
	case GroupDay, GroupWeek, GroupMonth, GroupProject:
		return g, true
	case GroupNone:
	}
	return GroupNone, false
}
