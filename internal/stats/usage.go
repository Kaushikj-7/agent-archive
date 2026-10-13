package stats

import (
	"math"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// tokenSet is token counts in the engine's one meaning: fresh is input that
// was not read from the prompt cache, so fresh + read + write + out is the
// total with nothing counted twice. reasoning is a part of out, and is never
// added to the total.
type tokenSet struct {
	fresh     int64
	read      int64
	write     int64
	out       int64
	reasoning int64
	// reasoningKnown is set when some record reported reasoning tokens;
	// cacheKnown when some record reported cache reads or writes, without
	// which a cache-hit rate would read a missing count as no hits.
	reasoningKnown bool
	cacheKnown     bool
}

func (t *tokenSet) total() int64 { return satAdd(t.inputSide(), t.out) }

// inputSide is every token the prompt side of a request carried.
func (t *tokenSet) inputSide() int64 { return satAdd(satAdd(t.fresh, t.read), t.write) }

func (t *tokenSet) add(o tokenSet) {
	t.fresh = satAdd(t.fresh, o.fresh)
	t.read = satAdd(t.read, o.read)
	t.write = satAdd(t.write, o.write)
	t.out = satAdd(t.out, o.out)
	t.reasoning = satAdd(t.reasoning, o.reasoning)
	t.reasoningKnown = t.reasoningKnown || o.reasoningKnown
	t.cacheKnown = t.cacheKnown || o.cacheKnown
}

// value is a stored count as a non-negative int64: nil is 0 (callers track
// whether anything was reported), and a negative count, which the parser never
// writes, is 0 too so it cannot cancel other tokens out of a total.
func value(p *int) int64 {
	if p == nil || *p < 0 {
		return 0
	}
	return int64(*p)
}

// normalizedTokens turns one harness's stored token fields into a tokenSet,
// and reports whether any of them was reported at all (a nil field is
// unknown, and a set with no reported field is not data). Storage keeps each
// harness's own meaning; the normalization happens only here: Codex's input
// includes its cached input and its cache-write input (its total_tokens is
// input plus output, so neither is added on top), so its fresh input is input
// minus cache read and cache write (never below 0). Every other harness's
// input already leaves the cache out.
func normalizedTokens(harness string, input, output, read, write, reasoning *int) (tokenSet, bool) {
	has := input != nil || output != nil || read != nil || write != nil
	set := tokenSet{
		fresh: value(input), read: value(read), write: value(write), out: value(output),
		reasoning: value(reasoning), reasoningKnown: reasoning != nil,
		cacheKnown: read != nil || write != nil,
	}
	if harness == archive.HarnessCodex {
		set.fresh = max(set.fresh-satAdd(set.read, set.write), 0)
	}
	return set, has
}

// memberUsage is the token accounting of one session (one metadata
// document), by normalized model id. With model_tokens (parser 0.14.0) each
// entry is one model, and tokens on a record that named none are under
// "unknown". Without it, the session-wide counts are all attributed to the
// session's main model and approximate is set. has is false when the session
// reported no token count.
func memberUsage(m *archive.Metadata, normalize func(string) string) (byModel map[string]tokenSet, has, approximate bool) {
	harness := archive.CanonicalHarness(m.Harness.Name)
	byModel = map[string]tokenSet{}
	if len(m.ModelTokens) > 0 {
		for _, entry := range m.ModelTokens {
			set, ok := normalizedTokens(harness, entry.InputTokens, entry.OutputTokens, entry.CacheReadTokens, entry.CacheWriteTokens, entry.ReasoningTokens)
			if !ok {
				continue
			}
			id := normalize(entry.Model)
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
	byModel[mainModel(m, normalize)] = set
	return byModel, true, true
}

// mainModel is the model a session with no per-model split is priced at:
// the one with the most turns (the lexically first on a tie), or "unknown"
// when the session names no model.
func mainModel(m *archive.Metadata, normalize func(string) string) string {
	best, bestTurns := "", -1
	for _, model := range m.Models {
		id := model.Attributes["gen_ai.response.model"]
		if strings.TrimSpace(id) == "" {
			id = model.Attributes["gen_ai.request.model"]
		}
		id = normalize(id)
		if id == "" || id == "<synthetic>" {
			continue
		}
		turns := 0
		if model.TurnCount != nil {
			turns = *model.TurnCount
		}
		if turns > bestTurns || (turns == bestTurns && id < best) {
			best, bestTurns = id, turns
		}
	}
	if best == "" {
		return archive.UnknownModel
	}
	return best
}

// costAcc accumulates estimated cost. priced is set once any token was priced
// (so a total of 0 is a real 0, not an unknown).
type costAcc struct {
	usd         float64
	priced      bool
	unpriced    int64
	approximate bool
}

func (c *costAcc) add(o costAcc) {
	c.usd += o.usd
	c.priced = c.priced || o.priced
	c.unpriced = satAdd(c.unpriced, o.unpriced)
	c.approximate = c.approximate || o.approximate
}

func (c *costAcc) cost() Cost {
	var usd *float64
	if c.priced {
		value := c.usd
		usd = &value
	}
	return Cost{USD: usd, Partial: c.unpriced > 0, UnpricedTokens: c.unpriced, Approximate: c.approximate}
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

// bucket is a running total over sessions: the overview, one agent, one
// project, one day, or one group.
type bucket struct {
	sessions     int
	dataSessions int
	promptSess   int
	prompts      int64
	tokens       tokenSet
	cost         costAcc
}

func (b *bucket) add(u *unit) {
	b.sessions++
	if u.prompts != nil {
		b.promptSess++
		b.prompts = satAdd(b.prompts, *u.prompts)
	}
	if u.hasData {
		b.dataSessions++
		b.tokens.add(u.tokens)
	}
	b.cost.add(u.cost)
}

// tokenTotal is the bucket's tokens, nil when none of its sessions reported any.
func (b *bucket) tokenTotal() *int64 {
	if b.dataSessions == 0 {
		return nil
	}
	total := b.tokens.total()
	return &total
}

func (b *bucket) promptTotal() *int64 {
	if b.promptSess == 0 {
		return nil
	}
	total := b.prompts
	return &total
}

// cacheHitRate is cache reads over the input side, nil when there is none.
func cacheHitRate(t *tokenSet) *float64 {
	side := t.inputSide()
	if !t.cacheKnown || side <= 0 {
		return nil
	}
	rate := float64(t.read) / float64(side)
	return &rate
}

// satAdd adds two non-negative counts, stopping at the largest int64 instead
// of wrapping negative. The archive caps each count it stores at 2^53, but a
// sum over many sessions (or a session's dozens of models) can pass int64.
func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
