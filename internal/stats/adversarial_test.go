package stats

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// hostileArchive is an archive with everything a corrupt or hostile bucket
// could hold: parent cycles and self-parents, repeated ids, a zero or far-off
// captured_at, sessions after Now, and token counts at the parser's 2^53 cap.
func hostileArchive(rng *rand.Rand, n int) []archive.Metadata {
	const maxCount = 1 << 53
	pick := func(vals ...int) int { return vals[rng.IntN(len(vals))] }
	count := func() int { return pick(0, 1, 7, 5000, maxCount, maxCount-1, rng.IntN(100000)) }
	models := []string{"claude-opus-5-5", "claude-sonnet-5-5", "gpt-6-luna", "other", "unknown", "", "x", "<synthetic>", "OPUS[1m]"}
	base := time.Date(2026, time.September, 15, 12, 0, 0, 0, newYork)
	out := make([]archive.Metadata, 0, n)
	for i := range n {
		var at time.Time
		switch rng.IntN(10) {
		case 0:
			// captured_at unset
		case 1:
			at = base.AddDate(0, 0, 40+rng.IntN(400)) // after Now
		case 2:
			at = base.AddDate(-5, 0, 0)
		default:
			at = base.Add(-time.Duration(rng.IntN(60*24*60)) * time.Minute)
		}
		harness := []string{"claude", "codex", "cursor", "", "Claude Code"}[rng.IntN(5)]
		m := meta(fmt.Sprintf("h%03d", rng.IntN(n)), harness, at, project([]string{"a", "b", " a ", ""}[rng.IntN(4)]))
		m.MetadataDerivedAt = base.Add(time.Duration(i) * time.Second)
		switch rng.IntN(4) {
		case 0:
			m.ParentSessionID = fmt.Sprintf("h%03d", rng.IntN(n)) // may cycle or point at itself
		case 1:
			m.ParentSessionID = "nowhere"
		}
		if rng.IntN(3) > 0 {
			m.Counts.Turns = ip(rng.IntN(20))
		}
		if rng.IntN(3) == 0 {
			m.Counts.ToolResults, m.Counts.ToolErrors = ip(count()), ip(count())
		}
		switch rng.IntN(4) {
		case 0:
			// no tokens
		case 1:
			usedModel(models[rng.IntN(len(models))], 1)(&m)
			m.Parser.Version = []string{"0.13.0", "", "garbage", "0.14.0"}[rng.IntN(4)]
			in, out, read, write := count(), count(), count(), count()
			m.Counts.InputTokens, m.Counts.OutputTokens = ip(in), ip(out)
			m.Counts.CacheReadTokens, m.Counts.CacheWriteTokens = ip(read), ip(write)
			m.Counts.ReasoningTokens = ip(count())
		default:
			for range 1 + rng.IntN(32) {
				m.ModelTokens = append(m.ModelTokens, archive.ModelTokens{
					Model: models[rng.IntN(len(models))], InputTokens: ip(count()), OutputTokens: ip(count()),
					CacheReadTokens: ip(count()), CacheWriteTokens: ip(count()), ReasoningTokens: ip(count()),
				})
			}
		}
		// Names that are not what they seem: plugin prefixes, empty halves,
		// controls, markup, invalid UTF-8, and the same skill under two names.
		for range rng.IntN(4) {
			skill([]string{"docs", "anthropic-skills:docs", ":", "a:", ":b", "x:y:z", "", " ", "\x1b[31m", "<b>&", "bad\xff:\xfe", "\u202e:evil"}[rng.IntN(12)])(&m)
		}
		if rng.IntN(3) == 0 {
			mcp([]string{"github", "linear", "", "a:b", "<i>"}[rng.IntN(5)], count())(&m)
		}
		out = append(out, m)
	}
	return out
}

// The property the numbers exist for, on the nastiest input: every breakdown
// adds up to the overall total, nothing is negative or non-finite, and the
// document is the same for any ordering of the input.
func TestHostileArchivesStayConsistent(t *testing.T) {
	t.Parallel()
	seeds := uint64(60)
	if testing.Short() {
		// Keep the same seeded archives and all assertions for each seed.
		seeds = 12
	}
	for seed := uint64(1); seed <= seeds; seed++ {
		rng := rand.New(rand.NewPCG(seed, 99))
		sessions := hostileArchive(rng, 30+rng.IntN(300))
		for i, by := range []Grouping{GroupNone, GroupDay, GroupWeek, GroupMonth, GroupProject} {
			// Every other run asks for the full lists, whatever TopN says.
			topN, allRows := 1000, false
			if i%2 == 1 {
				topN, allRows = 1, true
			}
			o := Options{Now: time.Date(2026, time.September, 15, 12, 0, 0, 0, newYork), Location: newYork, Days: 90, TopN: topN, AllRows: allRows, By: by}
			got := Compute(sessions, o)
			checkNewFields(t, seed, o, got)
			data, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("seed %d by %q: not serializable: %v", seed, by, err)
			}
			shuffled := append([]archive.Metadata(nil), sessions...)
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			// Repeated ids keep the latest derivation; derived_at is distinct per
			// session here, so the choice cannot depend on the order.
			again, err := json.Marshal(Compute(shuffled, o))
			if err != nil || string(again) != string(data) {
				t.Fatalf("seed %d by %q: input order changed the document (err %v)", seed, by, err)
			}

			var total int64
			if got.Composition != nil {
				total = got.Composition.Total
			}
			var agents, models, projects, daily, groups int64
			sessionsByAgent, sessionsByProject, groupSessions := 0, 0, 0
			for _, a := range got.Agents {
				agents = satAdd(agents, sumTokens(a.Tokens))
				sessionsByAgent += a.Sessions
			}
			for _, r := range got.Models {
				models = satAdd(models, r.Tokens)
			}
			for _, p := range got.Projects {
				projects = satAdd(projects, sumTokens(p.Tokens))
				sessionsByProject += p.Sessions
			}
			for _, d := range got.Daily {
				daily = satAdd(daily, sumTokens(d.Tokens))
			}
			if got.Groups != nil {
				for _, row := range got.Groups.Rows {
					groups = satAdd(groups, sumTokens(row.Tokens))
					groupSessions += row.Sessions
				}
			}
			for name, sum := range map[string]int64{"agents": agents, "models": models, "projects": projects, "daily": daily} {
				if sum != total {
					t.Errorf("seed %d by %q: %s tokens %d != overall %d", seed, by, name, sum, total)
				}
			}
			if got.Groups != nil && (groups != total || groupSessions != got.Coverage.Sessions) {
				t.Errorf("seed %d by %q: groups tokens %d sessions %d, want %d and %d", seed, by, groups, groupSessions, total, got.Coverage.Sessions)
			}
			if sessionsByAgent != got.Coverage.Sessions || sessionsByProject != got.Coverage.Sessions {
				t.Errorf("seed %d: sessions by agent %d, by project %d, want %d", seed, sessionsByAgent, sessionsByProject, got.Coverage.Sessions)
			}
			if total < 0 {
				t.Errorf("seed %d: negative total %d", seed, total)
			}
			if c := got.Composition; c != nil {
				for name, seg := range map[string]Segment{"read": c.CacheRead, "write": c.CacheWrite, "fresh": c.FreshInput, "out": c.Output} {
					if seg.Tokens < 0 || seg.Share < 0 || seg.Share > 1.0000001 || math.IsNaN(seg.Share) {
						t.Errorf("seed %d: composition %s = %+v", seed, name, seg)
					}
				}
			}
		}
	}
}

// The archive caps each stored count at 2^53. Summed over enough sessions
// (or the 32 models of one), that passes int64, and a wrapped total would be
// negative: the sum stops at the largest int64 instead.
func TestHugeTokenCountsAddUpWithoutWrapping(t *testing.T) {
	t.Parallel()
	const maxCount = 1 << 53
	build := func(n int) []archive.Metadata {
		var sessions []archive.Metadata
		for i := range n {
			sessions = append(sessions, meta(fmt.Sprintf("s%04d", i), "claude", day(time.September, 10, 9),
				tokens(maxCount, maxCount, maxCount, maxCount), usedModel("claude-opus-5-5", 1)))
		}
		return sessions
	}
	o := Options{Now: day(time.September, 20, 12), Location: newYork}

	// 100 sessions: 400 * 2^53 fits, and every figure is exact.
	got := Compute(build(100), o)
	if want := int64(400 * maxCount); got.Composition.Total != want || *got.Agents[0].Tokens != want || got.Models[0].Tokens != want {
		t.Errorf("100 sessions: total %d, agent %s, model %d, want %d", got.Composition.Total, i64(got.Agents[0].Tokens), got.Models[0].Tokens, want)
	}

	// 300 sessions: 1200 * 2^53 does not fit; every figure saturates together.
	got = Compute(build(300), o)
	if got.Composition.Total != math.MaxInt64 || *got.Agents[0].Tokens != math.MaxInt64 || got.Models[0].Tokens != math.MaxInt64 {
		t.Errorf("300 sessions: total %d, agent %s, model %d, want %d", got.Composition.Total, i64(got.Agents[0].Tokens), got.Models[0].Tokens, int64(math.MaxInt64))
	}
	segments := map[string]Segment{
		"read": got.Composition.CacheRead, "write": got.Composition.CacheWrite,
		"fresh": got.Composition.FreshInput, "output": got.Composition.Output,
	}
	for name, seg := range segments {
		if seg.Tokens < 0 || seg.Share < 0 || seg.Share > 1 {
			t.Errorf("300 sessions: %s segment %+v", name, seg)
		}
	}
	if _, err := json.Marshal(got); err != nil {
		t.Errorf("saturated stats do not serialize: %v", err)
	}
}

// A negative count (the parser never writes one) is not a count: it must not
// cancel real tokens out of a total.
func TestNegativeCountsAreNotTokens(t *testing.T) {
	t.Parallel()
	m := meta("neg", "claude", day(time.September, 10, 9), usedModel("claude-opus-5-5", 1), tokens(-5, 10, 0, 0))
	m.Counts.Turns = ip(-3)
	got := Compute([]archive.Metadata{m}, Options{Now: day(time.September, 20, 12), Location: newYork})
	if got.Composition == nil || got.Composition.Total != 10 {
		t.Errorf("composition %+v, want a total of 10", got.Composition)
	}
	if p := got.Overview.Prompts.Value; p == nil || *p != 0 {
		t.Errorf("prompts %v, want 0", f64(p))
	}
}

// A subagent chain that loops (a session its own ancestor) still counts every
// session once: the loop's sessions, and whatever hangs off it, stand alone.
func TestParentLoopsCountEachSessionOnce(t *testing.T) {
	t.Parallel()
	at := day(time.September, 28, 10)
	sessions := []archive.Metadata{
		meta("self", "claude", at, parentOf("self"), modelTokens("claude-opus-5-5", 1, 1, 0, 0)),
		meta("a", "claude", at, parentOf("b"), modelTokens("claude-opus-5-5", 2, 2, 0, 0)),
		meta("b", "claude", at, parentOf("a"), modelTokens("claude-opus-5-5", 4, 4, 0, 0)),
		meta("under-loop", "claude", at, parentOf("a"), modelTokens("claude-opus-5-5", 8, 8, 0, 0)),
		meta("root", "claude", at, modelTokens("claude-opus-5-5", 16, 16, 0, 0)),
		meta("kid", "claude", at, parentOf("root"), modelTokens("claude-opus-5-5", 32, 32, 0, 0)),
		meta("grandkid", "claude", at, parentOf("kid"), modelTokens("claude-opus-5-5", 64, 64, 0, 0)),
	}
	got := Compute(sessions, opts())
	if got.Overview.Tokens.Value == nil || *got.Overview.Tokens.Value != 254 {
		t.Fatalf("tokens = %v, want every session's tokens once (254)", f64(got.Overview.Tokens.Value))
	}
	if got.Coverage.Sessions != 5 || got.Coverage.SubagentSessions != 2 {
		t.Fatalf("coverage = %+v, want 5 sessions with 2 subagents rolled in", got.Coverage)
	}
}

// checkNewFields holds what the fields the spend view and the heads-up notes
// read must satisfy on any archive, however hostile: each day's cost adds up
// to the overall cost, the peak is the dearest day, the cache share is a
// share, the notes are few and in priority order, and the lists agree with
// their totals.
func checkNewFields(tb testing.TB, seed uint64, o Options, got Stats) {
	tb.Helper()
	var sum, dearest float64
	var unpriced int64
	for _, d := range got.Daily {
		unpriced = satAdd(unpriced, d.Cost.UnpricedTokens)
		if d.Cost.USD == nil {
			continue
		}
		if math.IsNaN(*d.Cost.USD) || math.IsInf(*d.Cost.USD, 0) || *d.Cost.USD < 0 {
			tb.Errorf("seed %d: day %s cost %v", seed, d.Date, *d.Cost.USD)
		}
		sum += *d.Cost.USD
		dearest = max(dearest, *d.Cost.USD)
	}
	overall := 0.0
	if got.Overview.Cost.Value != nil {
		overall = *got.Overview.Cost.Value
	}
	if !near2(sum, overall) {
		tb.Errorf("seed %d: the days' costs add to %v, the overall cost is %v", seed, sum, overall)
	}
	if unpriced != got.Overview.Cost.UnpricedTokens {
		tb.Errorf("seed %d: the days' unpriced tokens add to %d, overall %d", seed, unpriced, got.Overview.Cost.UnpricedTokens)
	}
	if (got.PeakSpend == nil) != (dearest <= 0) || (got.PeakSpend != nil && got.PeakSpend.USD != dearest) {
		tb.Errorf("seed %d: peak spend %+v, dearest day %v", seed, got.PeakSpend, dearest)
	}
	if c := got.Overview.CacheShare; c != nil && (math.IsNaN(*c) || *c < 0 || *c > 1) {
		tb.Errorf("seed %d: cache share %v", seed, *c)
	}
	if len(got.HeadsUp) > MaxHeadsUp || got.HeadsUp == nil {
		tb.Errorf("seed %d: %d heads-up notes (nil %v)", seed, len(got.HeadsUp), got.HeadsUp == nil)
	}
	last := -1
	order := []NoteKind{NoteSubagentShare, NoteCostliestSession, NoteUnmeteredSessions, NoteLowCacheHit}
	for _, n := range got.HeadsUp {
		at := slices.Index(order, n.Kind)
		if at <= last {
			tb.Errorf("seed %d: heads-up kinds %v are not each once, in priority order", seed, noteKinds(got.HeadsUp))
		}
		last = at
	}
	if o.AllRows {
		if len(got.Projects) != got.TotalProjects || len(got.Skills) != got.TotalSkills || len(got.DisplaySkills) != got.TotalDisplaySkills ||
			(got.MCP != nil && len(got.MCP.Servers) != got.MCP.TotalServers) {
			tb.Errorf("seed %d: AllRows lists %d/%d projects, %d/%d skills, %d/%d display skills",
				seed, len(got.Projects), got.TotalProjects, len(got.Skills), got.TotalSkills, len(got.DisplaySkills), got.TotalDisplaySkills)
		}
	}
	if got.TotalDisplaySkills > got.TotalSkills || len(got.Skills) > got.TotalSkills {
		tb.Errorf("seed %d: %d display skills of %d recorded", seed, got.TotalDisplaySkills, got.TotalSkills)
	}
	for _, s := range got.DisplaySkills {
		if s.Sessions > got.Coverage.Sessions || s.Sessions < 1 {
			tb.Errorf("seed %d: display skill %+v of %d sessions", seed, s, got.Coverage.Sessions)
		}
	}
}
