package stats

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Codex's total_tokens is input plus output: its cached input and its cache
// writes are both inside input_tokens, so neither is added on top.
func TestCodexCacheWritesAreInsideItsInput(t *testing.T) {
	t.Parallel()
	// input 10000 = fresh 3000 + read 6000 + write 1000.
	m := meta("x1", "codex", day(time.September, 28, 15), modelTokens("gpt-6-luna", 10000, 500, 6000, 1000))
	got := Compute([]archive.Metadata{m}, opts())
	c := got.Composition
	if c == nil || c.FreshInput.Tokens != 3000 || c.CacheWrite.Tokens != 1000 || c.CacheRead.Tokens != 6000 || c.Total != 10500 {
		t.Fatalf("composition = %+v, want fresh 3000, read 6000, write 1000, total 10500 (input + output)", c)
	}
	// gpt-6-luna: 3000*0.1 + 6000*0.01 + 1000*0.125 + 500*0.5 = 735 micro-dollars.
	if v := got.Overview.Cost.Value; v == nil || !near(*v*1e6, 735) {
		t.Fatalf("cost = %v, want 735 micro-dollars", f64(v))
	}
	// Claude's input already leaves the cache out, so its write is added.
	c2 := Compute([]archive.Metadata{meta("c1", "claude", day(time.September, 28, 15),
		modelTokens("claude-opus-5-5", 10000, 500, 6000, 1000))}, opts()).Composition
	if c2.FreshInput.Tokens != 10000 || c2.Total != 17500 {
		t.Fatalf("claude composition = %+v", c2)
	}
	// Reads and writes that exceed the input clamp fresh at zero, without wrapping.
	huge := meta("x2", "codex", day(time.September, 28, 15), modelTokens("gpt-5", 10, 1, math.MaxInt64/2+1, math.MaxInt64/2+1))
	if c := Compute([]archive.Metadata{huge}, opts()).Composition; c.FreshInput.Tokens != 0 || c.Total < 0 {
		t.Fatalf("composition = %+v", c)
	}
}

// A price table that cannot be summed (a price beyond MaxPricePerMTok, NaN,
// +Inf or negative) must never reach the JSON as an Inf or a NaN, which
// encoding/json cannot carry: the parser rejects it, and a table built by hand
// leaves the model unpriced and flagged instead.
func TestPricesThatCannotBeSummedNeverBreakTheJSON(t *testing.T) {
	t.Parallel()
	entry := func(price string) string {
		return `{"version":"v","as_of":"2026-09-01","models":[{"id":"m","input_per_mtok":` + price + `,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1}]}`
	}
	if _, err := ParsePriceTable([]byte(entry("1e9"))); err != nil {
		t.Fatalf("a price of exactly the limit is valid: %v", err)
	}
	for _, price := range []string{"1.0000001e9", "1e300", "1.7976931348623157e308"} {
		if _, err := ParsePriceTable([]byte(entry(price))); err == nil || !strings.Contains(err.Error(), "input_per_mtok") {
			t.Errorf("price %s: err = %v, want a rejected input_per_mtok", price, err)
		}
	}
	for name, bad := range map[string]float64{"max": math.MaxFloat64, "inf": math.Inf(1), "nan": math.NaN(), "negative": -1, "just over": MaxPricePerMTok * 1.5} {
		table := PriceTable{Version: "hand", AsOf: "2026-09-01", Currency: "USD", Models: []ModelPrice{
			{ID: "m", Family: "m", InputPerMTok: bad, OutputPerMTok: 1, CacheReadPerMTok: 1, CacheWritePerMTok: 1},
		}}
		sessions := []archive.Metadata{
			meta("a", "claude", day(time.September, 28, 10), modelTokens("m", 1<<50, 5, 0, 0)),
			meta("b", "claude", day(time.August, 20, 10), modelTokens("m", 3, 5, 0, 0)),
		}
		got := Compute(sessions, Options{Now: now, Location: newYork, PriceTable: table})
		if _, err := json.Marshal(got); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if c := got.Overview.Cost; c.Value != nil || !c.Partial || c.UnpricedTokens != 1<<50+5 {
			t.Errorf("%s: the model must be unpriced and flagged: %+v", name, c)
		}
	}
}

// Two finite costs can still make a change that overflows float64: the change
// is then unknown, not +Inf.
func TestChangeThatOverflowsIsUnknown(t *testing.T) {
	t.Parallel()
	table := PriceTable{Version: "v", AsOf: "2026-09-01", Currency: "USD", Models: []ModelPrice{
		{ID: "dear", Family: "dear", InputPerMTok: MaxPricePerMTok, OutputPerMTok: MaxPricePerMTok},
		{ID: "cheap", Family: "cheap", InputPerMTok: 1e-300, OutputPerMTok: 1e-300},
	}}
	sessions := []archive.Metadata{
		meta("now", "claude", day(time.September, 28, 10), modelTokens("dear", 1<<60, 0, 0, 0)),
		meta("before", "claude", day(time.August, 20, 10), modelTokens("cheap", 1, 0, 0, 0)),
	}
	got := Compute(sessions, Options{Now: now, Location: newYork, PriceTable: table})
	cost := got.Overview.Cost
	if cost.Value == nil || cost.Previous == nil || *cost.Previous <= 0 || cost.ChangePct != nil {
		t.Fatalf("cost = value %v previous %v change %v, want finite sides and an unknown change", f64(cost.Value), f64(cost.Previous), f64(cost.ChangePct))
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatal(err)
	}
}

// A long chain of subagents is walked once, not once per session: with the
// per-session walk, 20,000 links took most of a minute.
func TestDeepSubagentChainIsWalkedOnce(t *testing.T) {
	t.Parallel()
	const n = 20_000
	// The order sessions are resolved in is by id: name the chain so the root
	// comes first, and so the deepest subagent does.
	for name, id := range map[string]func(depth int) string{
		"root first":    func(depth int) string { return fmt.Sprintf("s%06d", depth) },
		"deepest first": func(depth int) string { return fmt.Sprintf("s%06d", n-depth) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sessions := make([]archive.Metadata, 0, n)
			for depth := range n {
				opts := []option{modelTokens("claude-opus-5-5", 1, 1, 1, 1)}
				if depth > 0 {
					opts = append(opts, parentOf(id(depth-1)))
				}
				sessions = append(sessions, meta(id(depth), "claude", day(time.September, 28, 10), opts...))
			}
			start := time.Now()
			got := Compute(sessions, opts())
			if took := time.Since(start); took > 10*time.Second {
				t.Fatalf("Compute over a %d-deep chain took %v", n, took)
			}
			if got.Coverage.Sessions != 1 || got.Coverage.SubagentSessions != n-1 || got.Overview.Tokens.Value == nil || *got.Overview.Tokens.Value != 4*n {
				t.Fatalf("coverage %+v tokens %v", got.Coverage, f64(got.Overview.Tokens.Value))
			}
		})
	}
}

// The memoized resolver must agree with the plain walk it replaced, on every
// shape of graph: chains, trees, loops with tails, self-parents, missing
// parents, in any order of asking.
func TestRootResolverMatchesThePlainWalk(t *testing.T) {
	t.Parallel()
	plain := func(m *archive.Metadata, byID map[string]*archive.Metadata) *archive.Metadata {
		seen := map[*archive.Metadata]bool{m: true}
		current := m
		for current.ParentSessionID != "" {
			parent, ok := byID[current.ParentSessionID]
			if !ok {
				return current
			}
			if seen[parent] {
				return m
			}
			seen[parent] = true
			current = parent
		}
		return current
	}
	for seed := uint64(1); seed <= 200; seed++ {
		rng := rand.New(rand.NewPCG(seed, 5))
		n := 2 + rng.IntN(60)
		metas := make([]archive.Metadata, n)
		byID := map[string]*archive.Metadata{}
		for i := range metas {
			metas[i].SessionID = fmt.Sprintf("n%d", i)
			byID[metas[i].SessionID] = &metas[i]
		}
		for i := range metas {
			switch rng.IntN(5) {
			case 0: // a root
			case 1:
				metas[i].ParentSessionID = "missing"
			default:
				metas[i].ParentSessionID = fmt.Sprintf("n%d", rng.IntN(n)) // may loop or point at itself
			}
		}
		resolver := newRootResolver(byID)
		for _, i := range rng.Perm(n) {
			if got, want := resolver.resolve(&metas[i]), plain(&metas[i], byID); got != want {
				t.Fatalf("seed %d: root of %s = %s, want %s", seed, metas[i].SessionID, got.SessionID, want.SessionID)
			}
		}
	}
}

// Two records of one session derived at the same instant are a tie: which one
// counts must not depend on the order they arrive in.
func TestSameSessionDerivedTwiceAtTheSameInstantIsOrderIndependent(t *testing.T) {
	t.Parallel()
	a := meta("s", "claude", day(time.September, 28, 10), modelTokens("claude-opus-5-5", 1000, 1000, 0, 0))
	b := meta("s", "claude", day(time.September, 28, 10), modelTokens("claude-opus-5-5", 5, 5, 0, 0))
	forward := mustJSON(t, Compute([]archive.Metadata{a, b}, opts()))
	backward := mustJSON(t, Compute([]archive.Metadata{b, a}, opts()))
	if forward != backward {
		t.Fatalf("the result depends on input order:\n%s\n%s", forward, backward)
	}
	// Sessions with no id at one instant are another tie, and float sums
	// depend on their order: a cost of 9e12 dwarfs the 0.001 of one token.
	table := PriceTable{Version: "v", AsOf: "2026-09-01", Currency: "USD", Models: []ModelPrice{
		{ID: "m", Family: "m", InputPerMTok: 1000, OutputPerMTok: 1000},
	}}
	var idless []archive.Metadata
	for _, n := range []int{1 << 53, 1, 2, 3, 5} {
		idless = append(idless, meta("", "claude", day(time.September, 28, 10), modelTokens("m", n, 0, 0, 0)))
	}
	want := mustJSON(t, Compute(idless, Options{Now: now, Location: newYork, PriceTable: table}))
	rng := rand.New(rand.NewPCG(9, 9))
	for range 100 {
		shuffled := slices.Clone(idless)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := mustJSON(t, Compute(shuffled, Options{Now: now, Location: newYork, PriceTable: table})); got != want {
			t.Fatalf("idless sessions: the result depends on input order:\n%s\n%s", got, want)
		}
	}
}

// The same model named through a cloud platform is that model.
func TestPartnerPlatformModelIDsArePriced(t *testing.T) {
	t.Parallel()
	ids := []string{"us.anthropic.claude-opus-5-5-v1:0", "anthropic/claude-opus-5-5", "anthropic.claude-opus-5-5-20251001-v1:0", "claude-opus-5-5@20251001"}
	var sessions []archive.Metadata
	for i, id := range ids {
		sessions = append(sessions, meta(fmt.Sprintf("s%d", i), "claude", day(time.September, 28, 10+i), modelTokens(id, 1_000_000, 0, 0, 0)))
	}
	got := Compute(sessions, opts())
	row := modelRow(t, got, "opus-5.5")
	if !row.Priced || row.Cost.Partial || !slices.Equal(row.Models, []string{"claude-opus-5-5"}) || row.Cost.USD == nil || !near(*row.Cost.USD, 16) {
		t.Fatalf("opus row = %+v, want the four ids priced as one model (4 x $4)", row)
	}
	if len(got.Models) != 1 {
		t.Fatalf("models = %+v", got.Models)
	}
	// The legacy-named Haiku 3.5 id prices as Haiku.
	legacy := Compute([]archive.Metadata{meta("h", "claude", day(time.September, 28, 10), modelTokens("claude-3-5-haiku-20241022", 1_000_000, 0, 0, 0))}, opts())
	if r := modelRow(t, legacy, "haiku-3.5"); !r.Priced || r.Cost.USD == nil || !near(*r.Cost.USD, 0.8) {
		t.Fatalf("haiku row = %+v", r)
	}
}

// A session whose subagents used the same model as it is one session of that
// model, and every exact version is a row of its own: Opus 5.5 and Opus 5 are
// not one "opus" row.
func TestModelRowCountsASessionOnce(t *testing.T) {
	t.Parallel()
	parent := meta("p", "claude", day(time.September, 28, 10), modelTokens("claude-opus-5-5", 1000, 0, 0, 0), modelTokens("claude-opus-4-8", 1000, 0, 0, 0))
	child := meta("k", "claude", day(time.September, 28, 10), parentOf("p"), modelTokens("claude-opus-5-5", 1000, 0, 0, 0), modelTokens("claude-sonnet-5-5", 1000, 0, 0, 0))
	other := meta("o", "claude", day(time.September, 28, 11), modelTokens("claude-opus-5-5", 1000, 0, 0, 0), modelTokens("claude-opus-5", 1000, 0, 0, 0))
	got := Compute([]archive.Metadata{parent, child, other}, opts())
	opus, sonnet := modelRow(t, got, "opus-5.5"), modelRow(t, got, "sonnet-5.5")
	if opus.Sessions != 2 || sonnet.Sessions != 1 || opus.Tokens != 3000 || !slices.Equal(opus.Models, []string{"claude-opus-5-5"}) {
		t.Fatalf("opus %+v sonnet %+v", opus, sonnet)
	}
	if older := modelRow(t, got, "opus-5"); older.Sessions != 1 || older.Tokens != 1000 {
		t.Fatalf("opus 5 %+v", older)
	}
	if len(got.Models) != 4 {
		t.Fatalf("models = %+v, want one row per version", got.Models)
	}
}

func TestHandBuiltTableWithoutACurrencyIsUSD(t *testing.T) {
	t.Parallel()
	table := PriceTable{Version: "hand", AsOf: "2026-09-01", Models: []ModelPrice{{ID: "m", InputPerMTok: 1, OutputPerMTok: 1}}}
	if got := Compute(nil, Options{Now: now, Location: newYork, PriceTable: table}).Prices.Currency; got != "USD" {
		t.Fatalf("currency = %q, want USD", got)
	}
}
