package stats

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

var (
	newYork = time.FixedZone("test", 0) // replaced in init with the real zone where available
	now     = time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
)

func init() {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		newYork = loc
	}
	now = time.Date(2026, time.September, 29, 12, 0, 0, 0, newYork)
}

func opts() Options { return Options{Now: now, Location: newYork} }

func TestEmptyArchive(t *testing.T) {
	t.Parallel()
	got := Compute(nil, opts())
	if got.Coverage.Sessions != 0 || got.Overview.Sessions.Value == nil || *got.Overview.Sessions.Value != 0 {
		t.Fatalf("empty archive: coverage %+v overview %+v", got.Coverage, got.Overview.Sessions)
	}
	if len(got.Daily) != DefaultDays {
		t.Fatalf("daily has %d days, want %d", len(got.Daily), DefaultDays)
	}
	for _, d := range got.Daily {
		if d.Sessions != 0 || d.Tokens == nil || *d.Tokens != 0 {
			t.Fatalf("empty day %+v", d)
		}
	}
	if got.Overview.Tokens.Value != nil || got.Overview.Cost.Value != nil || got.Overview.Prompts.Value != nil {
		t.Fatalf("an empty archive has unknown, not zero, tokens/cost/prompts: %+v", got.Overview)
	}
	if got.Overview.Sessions.ChangePct != nil {
		t.Fatalf("change against a zero previous period must be null, got %v", *got.Overview.Sessions.ChangePct)
	}
	if got.Peak != nil || got.Composition != nil || got.Subagents != nil || got.MCP != nil || got.Skills != nil {
		t.Fatalf("optional sections must be absent: %+v", got)
	}
	if got.Agents == nil || got.Models == nil || got.Projects == nil {
		t.Fatal("lists must marshal as [], not null")
	}
	if got.Highlights != (Highlights{}) {
		t.Fatalf("highlights = %+v", got.Highlights)
	}
	mustJSON(t, got)
}

func TestSingleSession(t *testing.T) {
	t.Parallel()
	m := meta("s1", "claude", day(time.September, 28, 10), turns(7),
		modelTokens("claude-opus-5-5", 1000, 500, 100000, 20000), project("alpha"))
	got := Compute([]archive.Metadata{m}, opts())
	if got.Coverage.Sessions != 1 || got.Coverage.SessionsWithTokens != 1 || got.Coverage.Agents != 1 {
		t.Fatalf("coverage = %+v", got.Coverage)
	}
	if v := got.Overview.Tokens.Value; v == nil || *v != 121500 {
		t.Fatalf("tokens = %v", f64(v))
	}
	if v := got.Overview.Cost.Value; v == nil || !near(*v, 0.134) {
		t.Fatalf("cost = %v, want 0.134", f64(v))
	}
	if got.Overview.Cost.Partial || got.Overview.Cost.Approximate {
		t.Fatalf("a fully priced session is not partial/approximate: %+v", got.Overview.Cost)
	}
	if got.Overview.ActiveDays.Value == nil || *got.Overview.ActiveDays.Value != 1 || got.Overview.CurrentStreak != 1 || got.Overview.BestStreak != 1 {
		t.Fatalf("active days/streaks = %+v", got.Overview)
	}
	if got.Peak == nil || got.Peak.Date != "2026-09-28" || got.Peak.Tokens != 121500 {
		t.Fatalf("peak = %+v", got.Peak)
	}
	if got.Highlights.BusiestDay == nil || got.Highlights.BusiestDay.Date != "2026-09-28" {
		t.Fatalf("busiest = %+v", got.Highlights.BusiestDay)
	}
	if got.Highlights.MonthRank != nil {
		t.Fatalf("no earlier month has data: %+v", got.Highlights.MonthRank)
	}
}

func multiAgentSessions() []archive.Metadata {
	return []archive.Metadata{
		meta("c1", "claude", day(time.September, 28, 10), project("alpha"), turns(10),
			modelTokens("claude-opus-5-5", 1000, 500, 100000, 20000),
			modelTokens("claude-sonnet-5-5", 2000, 1000, 50000, 0)),
		// Codex's input (10000) includes its cached input (8000).
		meta("x1", "codex", day(time.September, 28, 15), project("alpha"), turns(3),
			modelTokens("gpt-5.6-terra", 10000, 500, 8000, 0)),
		meta("u1", "cursor", day(time.September, 27, 9), project("beta"), turns(4)),
	}
}

func TestMultiAgentMultiModelCursorUnknown(t *testing.T) {
	t.Parallel()
	got := Compute(multiAgentSessions(), opts())

	if got.Coverage.Sessions != 3 || got.Coverage.SessionsWithTokens != 2 || got.Coverage.Agents != 3 {
		t.Fatalf("coverage = %+v", got.Coverage)
	}
	if got.Coverage.UnknownTokensByAgent["cursor"] != 1 || len(got.Coverage.UnknownTokensByAgent) != 1 {
		t.Fatalf("unknown by agent = %v", got.Coverage.UnknownTokensByAgent)
	}
	cursor := agentByName(t, got, "cursor")
	if cursor.Tokens != nil || cursor.Cost.USD != nil || cursor.TokenShare != nil || cursor.CacheHitRate != nil || cursor.UnknownTokenSessions != 1 {
		t.Fatalf("Cursor's tokens and cost are unknown, not zero: %+v", cursor)
	}
	if !strings.Contains(mustJSON(t, cursor), `"tokens":null`) {
		t.Fatalf("unknown tokens must marshal as null: %s", mustJSON(t, cursor))
	}
	claude, codex := agentByName(t, got, "claude"), agentByName(t, got, "codex")
	if i64(claude.Tokens) != "174500" || i64(codex.Tokens) != "10500" {
		t.Fatalf("agent tokens claude=%s codex=%s", i64(claude.Tokens), i64(codex.Tokens))
	}
	if claude.Label != "Claude Code" || got.Agents[0].Harness != "claude" {
		t.Fatalf("agents = %+v", got.Agents)
	}
	if v := got.Overview.Tokens.Value; *v != 185000 {
		t.Fatalf("total tokens = %v, want 185000", *v)
	}
	if v := got.Overview.Cost.Value; !near(*v, 0.134+0.024+0.0116) {
		t.Fatalf("total cost = %v", *v)
	}
	if !near(*claude.TokenShare+*codex.TokenShare, 1) {
		t.Fatalf("token shares = %v %v", *claude.TokenShare, *codex.TokenShare)
	}
	if !near(claude.SessionShare, 1.0/3) {
		t.Fatalf("session share = %v", claude.SessionShare)
	}
	// By model, grouped under price families, costliest first.
	labels := []string{}
	for _, r := range got.Models {
		labels = append(labels, r.Label)
	}
	if len(labels) != 3 || labels[0] != "opus" || labels[1] != "sonnet" || labels[2] != "gpt-5.6" {
		t.Fatalf("model rows = %v", labels)
	}
	if v := modelRow(t, got, "opus").Cost.USD; !near(*v, 0.134) {
		t.Fatalf("opus cost = %v", *v)
	}
	if got.Highlights.FavoriteModel == nil || got.Highlights.FavoriteModel.Label != "opus" || got.Highlights.FavoriteModel.By != "cost" {
		t.Fatalf("favorite = %+v", got.Highlights.FavoriteModel)
	}
	// Top projects by tokens: alpha has them, beta's are unknown.
	if got.TotalProjects != 2 || got.Projects[0].Name != "alpha" || got.Projects[1].Name != "beta" || got.Projects[1].Tokens != nil {
		t.Fatalf("projects = %+v", got.Projects)
	}
	if got.Projects[0].Sessions != 2 {
		t.Fatalf("alpha sessions = %d", got.Projects[0].Sessions)
	}
	if got.Overview.Prompts.Value == nil || *got.Overview.Prompts.Value != 17 {
		t.Fatalf("prompts = %v", f64(got.Overview.Prompts.Value))
	}
}

func TestCodexFreshInputIsInputMinusCachedInput(t *testing.T) {
	t.Parallel()
	codex := meta("x1", "codex", day(time.September, 28, 15),
		modelTokens("gpt-5.6-terra", 10000, 500, 8000, 0))
	claude := meta("c1", "claude", day(time.September, 28, 16),
		modelTokens("claude-opus-5-5", 10000, 500, 8000, 0))
	cases := []struct {
		name       string
		session    archive.Metadata
		fresh      int64
		total      int64
		hitRate    float64
		costMicros float64
	}{
		// Codex: fresh 2000 + read 8000 + out 500; cost 2000*2+8000*.2+500*12 = 11600 micro-dollars.
		{"codex", codex, 2000, 10500, 8000.0 / 10000, 11600},
		// Claude's input excludes the cache: fresh 10000 + read 8000 + out 500; 10000*4+8000*.2+500*20 = 51600.
		{"claude", claude, 10000, 18500, 8000.0 / 18000, 51600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Compute([]archive.Metadata{tc.session}, opts())
			c := got.Composition
			if c == nil || c.FreshInput.Tokens != tc.fresh || c.Total != tc.total {
				t.Fatalf("composition = %+v, want fresh %d total %d", c, tc.fresh, tc.total)
			}
			if c.CacheRead.Tokens != 8000 || c.Output.Tokens != 500 {
				t.Fatalf("composition = %+v", c)
			}
			if r := got.Agents[0].CacheHitRate; r == nil || !near(*r, tc.hitRate) {
				t.Fatalf("cache hit = %v want %v", f64(r), tc.hitRate)
			}
			if v := got.Overview.Cost.Value; v == nil || !near(*v*1e6, tc.costMicros) {
				t.Fatalf("cost = %v micro-dollars, want %v", f64(v), tc.costMicros)
			}
		})
	}
}

func TestCodexCachedInputLargerThanInputClampsFreshAtZero(t *testing.T) {
	t.Parallel()
	m := meta("x1", "codex", day(time.September, 28, 15), modelTokens("gpt-5", 100, 10, 500, 0))
	c := Compute([]archive.Metadata{m}, opts()).Composition
	if c.FreshInput.Tokens != 0 || c.Total != 510 {
		t.Fatalf("composition = %+v", c)
	}
}

func TestReasoningIsASubsetOfOutput(t *testing.T) {
	t.Parallel()
	m := meta("x1", "codex", day(time.September, 28, 15), modelTokens("gpt-5", 1000, 400, 0, 0))
	m.ModelTokens[0].ReasoningTokens = ip(300)
	m.Counts.ReasoningTokens = ip(300)
	c := Compute([]archive.Metadata{m}, opts()).Composition
	if c.Total != 1400 || c.Output.Tokens != 400 || c.ReasoningOfOutput == nil || *c.ReasoningOfOutput != 300 {
		t.Fatalf("reasoning was added to the total: %+v", c)
	}
	// A session that reports none leaves it unknown, not zero.
	m2 := meta("x2", "codex", day(time.September, 28, 15), modelTokens("gpt-5", 1000, 400, 0, 0))
	if c := Compute([]archive.Metadata{m2}, opts()).Composition; c.ReasoningOfOutput != nil {
		t.Fatalf("unreported reasoning = %d, want unknown", *c.ReasoningOfOutput)
	}
}

func TestSubagentSessionsRollUpIntoTheirParent(t *testing.T) {
	t.Parallel()
	parent := meta("p", "claude", day(time.September, 28, 10), turns(5), project("alpha"),
		modelTokens("claude-opus-5-5", 1000, 1000, 6000, 0), skill("review-pr"), mcp("github", 3), toolResults(10, 1))
	// The child was captured days later, outside the parent's day, yet is placed with it.
	child1 := meta("k1", "claude", day(time.September, 30, 10), turns(40), parentOf("p"),
		modelTokens("claude-sonnet-5-5", 500, 500, 2000, 0), skill("review-pr", "create-skill"), mcp("github", 2), mcp("linear", 1), toolResults(20, 2))
	child2 := meta("k2", "claude", day(time.September, 28, 11), turns(9), parentOf("k1"), // nested
		modelTokens("claude-sonnet-5-5", 100, 100, 0, 0))
	got := Compute([]archive.Metadata{child2, child1, parent}, opts())

	if got.Coverage.Sessions != 1 || got.Coverage.SubagentSessions != 2 || got.Coverage.OrphanSubagents != 0 {
		t.Fatalf("coverage = %+v", got.Coverage)
	}
	if v := got.Overview.Sessions.Value; *v != 1 {
		t.Fatalf("subagents counted as sessions: %v", *v)
	}
	if v := got.Overview.Prompts.Value; *v != 5 {
		t.Fatalf("subagent prompts were added to prompts: %v", *v)
	}
	const parentTokens, childTokens = 8000, 3000 + 200
	if v := got.Overview.Tokens.Value; *v != parentTokens+childTokens {
		t.Fatalf("tokens = %v, want %d", *v, parentTokens+childTokens)
	}
	if s := got.Subagents; s == nil || s.Tokens != childTokens || s.Sessions != 2 || !near(s.Share, float64(childTokens)/(parentTokens+childTokens)) {
		t.Fatalf("subagents = %+v", got.Subagents)
	}
	if i64(got.Agents[0].Tokens) != "11200" {
		t.Fatalf("agent tokens = %s", i64(got.Agents[0].Tokens))
	}
	if len(got.Skills) != 2 || got.Skills[0] != (Skill{"create-skill", 1}) || got.Skills[1] != (Skill{"review-pr", 1}) {
		t.Fatalf("skills counted per subagent, not per session: %+v", got.Skills)
	}
	if got.MCP == nil || got.MCP.Servers[0].Name != "github" || got.MCP.Servers[0].Calls != 5 || got.MCP.Servers[1].Name != "linear" {
		t.Fatalf("mcp = %+v", got.MCP)
	}
	if te := got.Highlights.ToolErrors; te == nil || te.Errors != 3 || te.Results != 30 {
		t.Fatalf("tool errors = %+v", te)
	}
	if got.Highlights.CostliestSession == nil || got.Highlights.CostliestSession.Subagents != 2 {
		t.Fatalf("costliest = %+v", got.Highlights.CostliestSession)
	}
	// Both subagent model rows are in cost by model.
	if modelRow(t, got, "sonnet").Tokens != 3200 {
		t.Fatalf("sonnet tokens = %d", modelRow(t, got, "sonnet").Tokens)
	}
}

func TestSubagentsWithoutTokensLeaveNoShare(t *testing.T) {
	t.Parallel()
	parent := meta("p", "claude", day(time.September, 28, 10), modelTokens("claude-opus-5-5", 1, 1, 0, 0))
	child := meta("k", "claude", day(time.September, 28, 11), parentOf("p"))
	got := Compute([]archive.Metadata{parent, child}, opts())
	if got.Subagents != nil {
		t.Fatalf("a share of zero must be omitted: %+v", got.Subagents)
	}
	if got.Coverage.SubagentSessions != 1 {
		t.Fatalf("coverage = %+v", got.Coverage)
	}
}

func TestOrphanSubagentIsASessionOfItsOwn(t *testing.T) {
	t.Parallel()
	orphan := meta("k", "claude", day(time.September, 28, 11), parentOf("gone"), modelTokens("claude-opus-5-5", 10, 10, 0, 0))
	cycleA := meta("a", "claude", day(time.September, 28, 12), parentOf("b"), modelTokens("claude-opus-5-5", 1, 1, 0, 0))
	cycleB := meta("b", "claude", day(time.September, 28, 13), parentOf("a"), modelTokens("claude-opus-5-5", 1, 1, 0, 0))
	got := Compute([]archive.Metadata{orphan, cycleA, cycleB}, opts())
	if got.Coverage.Sessions != 3 || got.Coverage.OrphanSubagents != 3 || got.Coverage.SubagentSessions != 0 {
		t.Fatalf("coverage = %+v", got.Coverage)
	}
	if *got.Overview.Tokens.Value != 24 {
		t.Fatalf("tokens = %v", *got.Overview.Tokens.Value)
	}
}

func TestParentOutsideTheWindowKeepsItsSubagentsOut(t *testing.T) {
	t.Parallel()
	parent := meta("p", "claude", day(time.July, 1, 10), modelTokens("claude-opus-5-5", 100, 100, 0, 0))
	child := meta("k", "claude", day(time.September, 28, 11), parentOf("p"), modelTokens("claude-opus-5-5", 100, 100, 0, 0))
	got := Compute([]archive.Metadata{parent, child}, opts())
	if got.Coverage.Sessions != 0 || got.Overview.Tokens.Value != nil {
		t.Fatalf("the subagent follows its parent out of the window: %+v", got.Coverage)
	}
}

func TestSessionsBeforeParser014ArePricedAtTheMainModelAndFlagged(t *testing.T) {
	t.Parallel()
	old := meta("o1", "claude", day(time.September, 28, 10), parserVersion("0.13.0"),
		usedModel("claude-sonnet-5-5", 2), usedModel("claude-opus-5-5", 9),
		tokens(1000, 500, 100000, 20000))
	got := Compute([]archive.Metadata{old}, opts())
	// Everything at opus 5.5, the session's main model.
	if v := got.Overview.Cost.Value; v == nil || !near(*v, 0.134) {
		t.Fatalf("cost = %v, want 0.134", f64(v))
	}
	if !got.Overview.Cost.Approximate || got.Overview.Cost.Partial {
		t.Fatalf("cost flags = %+v", got.Overview.Cost)
	}
	if got.Coverage.SessionsBeforeParser014 != 1 || got.Coverage.SessionsPricedAtMainModel != 1 || got.Coverage.SessionsWithoutToolErrors != 1 {
		t.Fatalf("coverage = %+v", got.Coverage)
	}
	if len(got.Models) != 1 || got.Models[0].Label != "opus" || got.Models[0].Tokens != 121500 {
		t.Fatalf("models = %+v", got.Models)
	}
	if !got.Agents[0].Cost.Approximate {
		t.Fatalf("agent cost = %+v", got.Agents[0].Cost)
	}
	// A tie on turns takes the model id that sorts first; no model at all is unpriced.
	tied := meta("o2", "claude", day(time.September, 28, 10), parserVersion("0.13.0"),
		usedModel("claude-sonnet-5-5", 3), usedModel("claude-opus-5-5", 3), tokens(1000, 0, 0, 0))
	if id := mainModel(&tied, NormalizeModel); id != "claude-opus-5-5" {
		t.Fatalf("tie went to %q", id)
	}
	bare := meta("o3", "claude", day(time.September, 28, 10), parserVersion("0.13.0"), tokens(1000, 0, 0, 0))
	g := Compute([]archive.Metadata{bare}, opts())
	if g.Overview.Cost.Value != nil || !g.Overview.Cost.Partial || g.Overview.Cost.UnpricedTokens != 1000 {
		t.Fatalf("a model-less old session is unpriced: %+v", g.Overview.Cost)
	}
}

func TestUnpricedModelsMakeCostPartialNeverGuessed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		session      archive.Metadata
		wantUSD      *float64
		wantUnpriced int64
	}{
		{"auto review model only", meta("a", "codex", day(time.September, 28, 10), modelTokens("codex-auto-review", 1000, 100, 200, 0)), nil, 1100},
		{"unknown model only", meta("b", "claude", day(time.September, 28, 10), modelTokens("unknown", 100, 100, 0, 0)), nil, 200},
		{"bare alias is ambiguous", meta("c", "claude", day(time.September, 28, 10), modelTokens("opus", 100, 100, 0, 0)), nil, 200},
		{"priced plus unpriced", meta("d", "claude", day(time.September, 28, 10),
			modelTokens("claude-opus-5-5", 1000, 0, 0, 0), modelTokens("unknown", 50, 50, 0, 0)), fp(0.004), 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Compute([]archive.Metadata{tc.session}, opts())
			cost := got.Overview.Cost
			if !cost.Partial || cost.UnpricedTokens != tc.wantUnpriced {
				t.Fatalf("cost = %+v, want %d unpriced tokens", cost, tc.wantUnpriced)
			}
			if (tc.wantUSD == nil) != (cost.Value == nil) || (tc.wantUSD != nil && !near(*cost.Value, *tc.wantUSD)) {
				t.Fatalf("usd = %v, want %v", f64(cost.Value), f64(tc.wantUSD))
			}
			// Tokens still count in full: unpriced is a cost gap, not a token gap.
			if *got.Overview.Tokens.Value == 0 {
				t.Fatal("unpriced tokens were dropped from the token total")
			}
			for _, row := range got.Models {
				if !row.Priced && (row.Cost.USD != nil || row.CostShare != nil) {
					t.Fatalf("unpriced row has a price: %+v", row)
				}
			}
		})
	}
}

func fp(v float64) *float64 { return &v }

func TestFavoriteModelFallsBackToTokensWhenNothingIsPriced(t *testing.T) {
	t.Parallel()
	got := Compute([]archive.Metadata{
		meta("a", "codex", day(time.September, 28, 10), modelTokens("codex-auto-review", 1000, 100, 0, 0)),
		meta("b", "codex", day(time.September, 28, 11), modelTokens("unknown", 10, 10, 0, 0)),
	}, opts())
	f := got.Highlights.FavoriteModel
	if f == nil || f.Label != "codex-auto-review" || f.By != "tokens" {
		t.Fatalf("favorite = %+v", f)
	}
	if got.Highlights.CostliestSession != nil {
		t.Fatalf("costliest without a price = %+v", got.Highlights.CostliestSession)
	}
}

func TestNilCountsStayUnknownNotZero(t *testing.T) {
	t.Parallel()
	// No token, turn, or tool count at all: everything derived stays unknown.
	bare := meta("b", "cursor", day(time.September, 28, 10))
	got := Compute([]archive.Metadata{bare}, opts())
	if got.Coverage.SessionsWithTokens != 0 {
		t.Fatalf("a session with no counts has token data: %+v", got.Coverage)
	}
	if got.Overview.Tokens.Value != nil || got.Overview.Prompts.Value != nil || got.Overview.Cost.Value != nil {
		t.Fatalf("unknown became a value: %+v", got.Overview)
	}
	if d := got.Daily[len(got.Daily)-2]; d.Sessions != 1 || d.Tokens != nil {
		t.Fatalf("a day whose sessions report no tokens has unknown tokens: %+v", d)
	}
	if got.Composition != nil || got.Peak != nil || got.Highlights.ToolErrors != nil {
		t.Fatalf("sections built from nothing: %+v", got)
	}
	// A reported zero is a value.
	zero := meta("z", "claude", day(time.September, 28, 10), tokens(0, 0, 0, 0), turns(0))
	g := Compute([]archive.Metadata{zero}, opts())
	if g.Overview.Tokens.Value == nil || *g.Overview.Tokens.Value != 0 || g.Overview.Prompts.Value == nil {
		t.Fatalf("a reported zero is not unknown: %+v", g.Overview)
	}
}

func TestPartlyKnownSessionKeepsUnreportedFieldsOutOfCacheHitRate(t *testing.T) {
	t.Parallel()
	m := meta("a", "claude", day(time.September, 28, 10))
	m.Counts.InputTokens, m.Counts.OutputTokens = ip(1000), ip(10)
	got := Compute([]archive.Metadata{m}, opts())
	if got.Agents[0].CacheHitRate != nil {
		t.Fatalf("cache hit rate from unreported cache counts: %v", *got.Agents[0].CacheHitRate)
	}
	if i64(got.Agents[0].Tokens) != "1010" {
		t.Fatalf("tokens = %s", i64(got.Agents[0].Tokens))
	}
}

func TestToolErrorRateCoversOnlySessionsThatKnowIt(t *testing.T) {
	t.Parallel()
	got := Compute([]archive.Metadata{
		meta("a", "claude", day(time.September, 28, 10), toolResults(100, 4)),
		meta("b", "cursor", day(time.September, 28, 11), toolResults(100, 0)),
		// Codex counts results but cannot say which failed.
		meta("c", "codex", day(time.September, 28, 12), toolResultsOnly(1000)),
		meta("d", "claude", day(time.September, 28, 13)),
	}, opts())
	te := got.Highlights.ToolErrors
	if te == nil || te.Errors != 4 || te.Results != 200 || !near(te.Rate, 0.02) || te.Sessions != 2 || te.UnknownSessions != 2 {
		t.Fatalf("tool errors = %+v", te)
	}
	// Sessions that know their errors but ran no tool: 0 of 0 is no rate, not 0%.
	none := Compute([]archive.Metadata{meta("z", "claude", day(time.September, 28, 10), toolResults(0, 0))}, opts())
	if none.Highlights.ToolErrors != nil {
		t.Fatalf("0 tool results gave tool errors %+v", none.Highlights.ToolErrors)
	}
	// Only Codex: unknown, not 0%.
	only := Compute([]archive.Metadata{meta("c", "codex", day(time.September, 28, 12), toolResultsOnly(1000))}, opts())
	if only.Highlights.ToolErrors != nil {
		t.Fatalf("Codex-only tool errors = %+v", only.Highlights.ToolErrors)
	}
}

func TestSkillsAndMCP(t *testing.T) {
	t.Parallel()
	var sessions []archive.Metadata
	for i, spec := range []struct {
		skills []string
		mcp    []option
	}{
		{[]string{"review-pr", "create-skill"}, []option{mcp("github", 41)}},
		{[]string{"review-pr"}, []option{mcp("linear", 12), mcp("github", 1)}},
		{[]string{"review-pr", "review-pr"}, nil},
	} {
		built := append([]option{skill(spec.skills...)}, spec.mcp...)
		sessions = append(sessions, meta(string(rune('a'+i)), "claude", day(time.September, 28, 10+i), built...))
	}
	got := Compute(sessions, Options{Now: now, Location: newYork, TopN: 1})
	if len(got.Skills) != 1 || got.Skills[0] != (Skill{"review-pr", 3}) {
		t.Fatalf("skills = %+v (a repeated skill in one session counts once)", got.Skills)
	}
	if got.MCP == nil || len(got.MCP.Servers) != 1 || got.MCP.Servers[0] != (MCPServer{Name: "github", Calls: 42, Sessions: 2}) || got.MCP.Scope != MCPScope {
		t.Fatalf("mcp = %+v", got.MCP)
	}
}

func TestCostliestSessionDrivers(t *testing.T) {
	t.Parallel()
	at := day(time.September, 28, 10)
	cases := []struct {
		name    string
		session []archive.Metadata
		drivers []string
	}{
		{"long context by average per message", []archive.Metadata{
			meta("s", "claude", at, messages(10), modelTokens("claude-opus-5-5", 100, 100, 2_000_000, 0))}, // about 200k per message
			[]string{DriverLongContext}},
		{"long context by compaction", []archive.Metadata{
			meta("s", "claude", at, compactions(1), modelTokens("claude-opus-5-5", 100, 100, 100000, 0))},
			[]string{DriverLongContext}},
		{"low cache hit", []archive.Metadata{
			meta("s", "claude", at, modelTokens("claude-opus-5-5", 100000, 100, 20000, 0))},
			[]string{DriverLowCacheHit}},
		{"tiny sessions do not report a low hit rate", []archive.Metadata{
			meta("s", "claude", at, modelTokens("claude-opus-5-5", 1000, 100, 0, 0))},
			[]string{}},
		{"subagents", []archive.Metadata{
			meta("s", "claude", at, modelTokens("claude-opus-5-5", 100, 100, 100000, 0)),
			meta("k", "claude", at, parentOf("s"), modelTokens("claude-sonnet-5-5", 100, 100, 100000, 0))},
			[]string{DriverSubagents}},
		{"a small subagent share is not a driver", []archive.Metadata{
			meta("s", "claude", at, modelTokens("claude-opus-5-5", 100, 100, 1000000, 0)),
			meta("k", "claude", at, parentOf("s"), modelTokens("claude-sonnet-5-5", 100, 100, 1000, 0))},
			[]string{}},
		{"all three", []archive.Metadata{
			meta("s", "claude", at, messages(2), compactions(2), modelTokens("claude-opus-5-5", 300000, 100, 10000, 0)),
			meta("k1", "claude", at, parentOf("s"), modelTokens("claude-opus-5-5", 200000, 100, 0, 0)),
			meta("k2", "claude", at, parentOf("s"), modelTokens("claude-opus-5-5", 200000, 100, 0, 0))},
			[]string{DriverLongContext, DriverSubagents, DriverLowCacheHit}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := Compute(tc.session, opts()).Highlights.CostliestSession
			if c == nil {
				t.Fatal("no costliest session")
			}
			if len(c.Drivers) != len(tc.drivers) {
				t.Fatalf("drivers = %v, want %v (%+v)", c.Drivers, tc.drivers, c)
			}
			for i := range tc.drivers {
				if c.Drivers[i] != tc.drivers[i] {
					t.Fatalf("drivers = %v, want %v", c.Drivers, tc.drivers)
				}
			}
		})
	}
	// The costliest is the highest cost, not the most tokens.
	pricey := meta("pricey", "claude", at, project("proj-api"), modelTokens("claude-fable-5-1", 0, 100000, 0, 0))
	big := meta("big", "claude", at, modelTokens("claude-haiku-4-5", 3_000_000, 0, 0, 0))
	got := Compute([]archive.Metadata{big, pricey}, opts()).Highlights.CostliestSession
	if got.SessionID != "pricey" || got.Project != "proj-api" || !near(*got.Cost.USD, 5.0) || got.Tokens != 100000 {
		t.Fatalf("costliest = %+v", got)
	}
}

func TestMonthRank(t *testing.T) {
	t.Parallel()
	month := func(m time.Month, tokens int) archive.Metadata {
		return meta("m"+m.String(), "claude", day(m, 15, 10), modelTokens("claude-opus-5-5", tokens, 0, 0, 0))
	}
	cases := []struct {
		name     string
		sessions []archive.Metadata
		want     *MonthRank
	}{
		{"second heaviest of six", []archive.Metadata{
			month(time.September, 500), month(time.August, 900), month(time.July, 100), month(time.June, 200), month(time.May, 300), month(time.April, 400),
		}, &MonthRank{Month: "2026-09", Rank: 2, Of: 6, Tokens: 500}},
		{"heaviest", []archive.Metadata{month(time.September, 500), month(time.August, 100)}, &MonthRank{Month: "2026-09", Rank: 1, Of: 2, Tokens: 500}},
		{"the sixth month back is out of range", []archive.Metadata{month(time.September, 500), month(time.March, 900)}, nil},
		{"no earlier data", []archive.Metadata{month(time.September, 500)}, nil},
		{"a session after Now is not this month so far", []archive.Metadata{
			month(time.September, 500), month(time.August, 100),
			meta("later", "claude", day(time.September, 30, 10), modelTokens("claude-opus-5-5", 9999, 0, 0, 0)),
		}, &MonthRank{Month: "2026-09", Rank: 1, Of: 2, Tokens: 500}},
		{"this month has no data", []archive.Metadata{month(time.August, 100)}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Compute(tc.sessions, opts()).Highlights.MonthRank
			if (tc.want == nil) != (got == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("month rank = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBusiestDayTieGoesToMoreTokensThenEarlier(t *testing.T) {
	t.Parallel()
	got := Compute([]archive.Metadata{
		meta("a", "claude", day(time.September, 20, 10), modelTokens("claude-opus-5-5", 10, 0, 0, 0)),
		meta("b", "claude", day(time.September, 21, 10), modelTokens("claude-opus-5-5", 50, 0, 0, 0)),
		meta("c", "claude", day(time.September, 22, 10), modelTokens("claude-opus-5-5", 50, 0, 0, 0)),
	}, opts())
	if b := got.Highlights.BusiestDay; b.Date != "2026-09-21" || b.Sessions != 1 {
		t.Fatalf("busiest = %+v", b)
	}
	if got.Peak.Date != "2026-09-21" {
		t.Fatalf("peak tie must go to the earlier day: %+v", got.Peak)
	}
}

func TestPreviousPeriodDelta(t *testing.T) {
	t.Parallel()
	cur := func(id string, d int, tok int, opts ...option) archive.Metadata {
		return meta(id, "claude", day(time.September, d, 10), append(opts, modelTokens("claude-opus-5-5", tok, 0, 0, 0), turns(2))...)
	}
	prev := func(id string, d int, tok int) archive.Metadata {
		return meta(id, "claude", day(time.August, d, 10), modelTokens("claude-opus-5-5", tok, 0, 0, 0), turns(1))
	}
	t.Run("both periods", func(t *testing.T) {
		t.Parallel()
		got := Compute([]archive.Metadata{cur("a", 5, 1200), cur("b", 6, 1200), prev("c", 5, 1000), prev("d", 6, 1000), prev("e", 7, 1000), prev("f", 8, 1000)}, opts())
		o := got.Overview
		if *o.Sessions.Previous != 4 || !near(*o.Sessions.ChangePct, -50) {
			t.Fatalf("sessions = %+v", o.Sessions)
		}
		if !near(*o.Tokens.ChangePct, (2400.0-4000)/4000*100) || !near(*o.Prompts.ChangePct, 0) {
			t.Fatalf("tokens %+v prompts %+v", o.Tokens, o.Prompts)
		}
		if *o.ActiveDays.Value != 2 || *o.ActiveDays.Previous != 4 {
			t.Fatalf("active days = %+v", o.ActiveDays)
		}
		if o.Cost.ChangePct == nil || !near(*o.Cost.ChangePct, (2400.0-4000)/4000*100) {
			t.Fatalf("cost = %+v", o.Cost)
		}
	})
	t.Run("zero previous", func(t *testing.T) {
		t.Parallel()
		got := Compute([]archive.Metadata{cur("a", 5, 1200)}, opts())
		o := got.Overview
		if o.Sessions.Previous == nil || *o.Sessions.Previous != 0 || o.Sessions.ChangePct != nil {
			t.Fatalf("sessions = %+v", o.Sessions)
		}
		if o.Tokens.Previous != nil || o.Tokens.ChangePct != nil || o.Cost.Previous != nil || o.Cost.ChangePct != nil {
			t.Fatalf("unknown previous tokens/cost are null, not 0: %+v %+v", o.Tokens, o.Cost)
		}
	})
	t.Run("previous period edges", func(t *testing.T) {
		t.Parallel()
		// Window: Aug 31 - Sep 29. Previous: Aug 1 - Aug 30.
		edge := func(id string, at time.Time) archive.Metadata {
			return meta(id, "claude", at, modelTokens("claude-opus-5-5", 1, 0, 0, 0))
		}
		got := Compute([]archive.Metadata{
			edge("before", time.Date(2026, time.July, 31, 23, 59, 59, 0, newYork)),
			edge("prevFirst", time.Date(2026, time.August, 1, 0, 0, 0, 0, newYork)),
			edge("prevLast", time.Date(2026, time.August, 30, 23, 59, 59, 999, newYork)),
			edge("curFirst", time.Date(2026, time.August, 31, 0, 0, 0, 0, newYork)),
			edge("curLast", time.Date(2026, time.September, 29, 23, 59, 59, 999, newYork)),
			edge("after", time.Date(2026, time.September, 30, 0, 0, 0, 0, newYork)),
		}, opts())
		if *got.Overview.Sessions.Value != 2 || *got.Overview.Sessions.Previous != 2 {
			t.Fatalf("sessions = %+v", got.Overview.Sessions)
		}
		if got.Window.FirstDay != "2026-08-31" || got.Window.LastDay != "2026-09-29" || got.Daily[0].Sessions != 1 || got.Daily[29].Sessions != 1 {
			t.Fatalf("window = %+v daily first/last %+v %+v", got.Window, got.Daily[0], got.Daily[29])
		}
		if !got.Window.From.Equal(time.Date(2026, time.August, 31, 0, 0, 0, 0, newYork)) || !got.Window.To.Equal(time.Date(2026, time.September, 30, 0, 0, 0, 0, newYork)) ||
			!got.Window.PreviousFrom.Equal(time.Date(2026, time.August, 1, 0, 0, 0, 0, newYork)) || !got.Window.PreviousTo.Equal(got.Window.From) {
			t.Fatalf("window = %+v", got.Window)
		}
	})
}

func TestWindowOptions(t *testing.T) {
	t.Parallel()
	m := meta("a", "claude", day(time.September, 20, 10), modelTokens("claude-opus-5-5", 1, 0, 0, 0))
	for _, tc := range []struct {
		name string
		opts Options
		days int
		in   bool
	}{
		{"default is 30 days", Options{Now: now, Location: newYork}, 30, true},
		{"7 days includes Sep 20", Options{Now: now, Location: newYork, Days: 7}, 7, false},
		{"10 days includes Sep 20", Options{Now: now, Location: newYork, Days: 10}, 10, true},
		{"negative means default", Options{Now: now, Location: newYork, Days: -3}, 30, true},
		{"clamped", Options{Now: now, Location: newYork, Days: MaxDays * 2}, MaxDays, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Compute([]archive.Metadata{m}, tc.opts)
			if got.Window.Days != tc.days || len(got.Daily) != tc.days || (got.Coverage.Sessions == 1) != tc.in {
				t.Fatalf("days %d/%d sessions %d", got.Window.Days, len(got.Daily), got.Coverage.Sessions)
			}
		})
	}
	// A zero Now means the newest captured_at in the input.
	got := Compute([]archive.Metadata{m}, Options{Location: newYork})
	if got.Window.LastDay != "2026-09-20" || got.Coverage.Sessions != 1 {
		t.Fatalf("zero Now: %+v", got.Window)
	}
	if empty := Compute(nil, Options{Location: time.UTC}); empty.Window.LastDay != "1970-01-01" {
		t.Fatalf("zero Now, empty input: %+v", empty.Window)
	}
	// A nil Location is the local zone.
	if got := Compute(nil, Options{Now: now}); got.Window.Timezone != time.Local.String() {
		t.Fatalf("timezone = %q", got.Window.Timezone)
	}
}

func TestDuplicateSessionIDsKeepTheLatestDerivation(t *testing.T) {
	t.Parallel()
	at := day(time.September, 28, 10)
	older := meta("s", "claude", at, modelTokens("claude-opus-5-5", 100, 0, 0, 0))
	newer := meta("s", "claude", at, modelTokens("claude-opus-5-5", 999, 0, 0, 0))
	newer.MetadataDerivedAt = at.Add(time.Hour)
	for _, in := range [][]archive.Metadata{{older, newer}, {newer, older}} {
		got := Compute(in, opts())
		if got.Coverage.Sessions != 1 || *got.Overview.Tokens.Value != 999 {
			t.Fatalf("got %d sessions, %v tokens", got.Coverage.Sessions, *got.Overview.Tokens.Value)
		}
	}
}

func TestJSONShape(t *testing.T) {
	t.Parallel()
	got := Compute(multiAgentSessions(), Options{Now: now, Location: newYork, By: GroupDay})
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"window", "prices", "coverage", "daily", "overview", "agents", "models", "projects", "highlights", "groups", "composition"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("JSON lacks %q", key)
		}
	}
	prices := doc["prices"].(map[string]any)
	if prices["as_of"] == "" || prices["version"] == "" {
		t.Fatalf("prices = %v", prices)
	}
	// Privacy: no prompt text or paths; project names and session ids only where documented.
	for _, s := range []string{"/Users", "title"} {
		if strings.Contains(string(data), s) {
			t.Errorf("JSON contains %q", s)
		}
	}
}
