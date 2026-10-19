package stats

import (
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestDefaultPriceTableIsValid(t *testing.T) {
	t.Parallel()
	table := DefaultPriceTable()
	if table.Version == "" || table.Currency != "USD" || len(table.Sources) == 0 || table.Notes == "" {
		t.Fatalf("table header = %+v", table)
	}
	asOf, err := time.Parse("2006-01-02", table.AsOf)
	if err != nil {
		t.Fatalf("as_of = %q", table.AsOf)
	}
	if asOf.After(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) || asOf.Before(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("as_of %s is not a plausible date", table.AsOf)
	}
	seen := map[string]bool{}
	for _, m := range table.Models {
		if seen[m.ID] || m.ID != NormalizeModel(m.ID) {
			t.Errorf("model id %q repeated or not normalized", m.ID)
		}
		seen[m.ID] = true
		if m.Family == "" || m.InputPerMTok <= 0 || m.OutputPerMTok <= 0 || m.CacheReadPerMTok <= 0 {
			t.Errorf("%s has a missing price: %+v", m.ID, m)
		}
		if m.CacheReadPerMTok > m.InputPerMTok || m.OutputPerMTok < m.InputPerMTok {
			t.Errorf("%s prices are not ordered as list prices are: %+v", m.ID, m)
		}
		if strings.HasPrefix(m.ID, "claude-") && m.CacheWritePerMTok < m.InputPerMTok {
			t.Errorf("%s: a cache write costs at least the input price: %+v", m.ID, m)
		}
		if strings.HasPrefix(m.ID, "claude-") {
			// Anthropic's published multipliers: a 5-minute write is 1.25x input,
			// a read 0.1x (0.025x on Fable 5.1 and Mythos 5.1, 0.05x on Opus 5.5). A typo in one
			// column shows up here.
			readMultiplier := map[string]float64{"claude-fable-5-1": 0.025, "claude-mythos-5-1": 0.025, "claude-opus-5-5": 0.05}[m.ID]
			if readMultiplier == 0 {
				readMultiplier = 0.1
			}
			if !near(m.CacheWritePerMTok, 1.25*m.InputPerMTok) || !near(m.CacheReadPerMTok, readMultiplier*m.InputPerMTok) {
				t.Errorf("%s cache prices do not follow Anthropic's multipliers: %+v", m.ID, m)
			}
		} else if strings.HasPrefix(m.ID, "gpt-6") || strings.HasPrefix(m.ID, "gpt-5.6") {
			// The GPT-6 and GPT-5.6 families list a cache-write price of 1.25x input.
			if !near(m.CacheWritePerMTok, 1.25*m.InputPerMTok) {
				t.Errorf("%s: cache write should be 1.25x input: %+v", m.ID, m)
			}
		} else if m.CacheWritePerMTok != 0 {
			t.Errorf("%s: older OpenAI models have no cache-write charge: %+v", m.ID, m)
		}
	}
	for _, source := range table.Sources {
		if !strings.HasPrefix(source, "https://") {
			t.Errorf("source %q is not a URL", source)
		}
	}
}

func TestRealModelIDsThePriceTableKnows(t *testing.T) {
	t.Parallel()
	table := DefaultPriceTable()
	for _, tc := range []struct {
		id     string
		priced bool
		family string
	}{
		// Model ids seen in real Claude Code and Codex archives.
		{"claude-opus-5-5", true, "opus-5.5"},
		{"claude-opus-5-5[1m]", true, "opus-5.5"},
		{"claude-opus-5", true, "opus-5"},
		{"claude-opus-4-8", true, "opus-4.8"},
		{"claude-fable-5-1", true, "fable-5.1"},
		{"claude-sonnet-5-5", true, "sonnet-5.5"},
		{"claude-sonnet-5", true, "sonnet-5"},
		{"claude-haiku-4-5-20251001", true, "haiku-4.5"},
		{"Claude-Haiku-4-5@20251001", true, "haiku-4.5"},
		{"claude-opus-4-20250514", true, "opus-4"},
		{"gpt-5.6-terra", true, "gpt-5.6-terra"},
		{"gpt-6-luna", true, "gpt-6-luna"},
		{"gpt-5", true, "gpt-5"},
		{"gpt-5-2025-08-07", true, "gpt-5"},
		{"gpt-5.5", true, "gpt-5.5"},
		{"gpt-5.5-2026-04-23", true, "gpt-5.5"},
		{"gpt-5.6", true, "gpt-5.6-sol"}, // OpenAI's alias for gpt-5.6-sol
		{"gpt-6.1-sol", true, "gpt-6.1-sol"},
		{"gpt-5.3-codex", true, "gpt-5.3-codex"},
		{"gpt-5.2-codex", true, "gpt-5.2-codex"},
		{"gpt-5.1-codex-mini", true, "gpt-5.1-codex-mini"},
		{"claude-mythos-5-1", true, "mythos-5.1"},
		// Ids a cloud platform gives the same models (Bedrock, Vertex).
		{"anthropic.claude-opus-4-6-v1", true, "opus-4.6"},
		{"global.anthropic.claude-opus-4-6-v1", true, "opus-4.6"},
		{"us.anthropic.claude-sonnet-4-5-20250929-v1:0", true, "sonnet-4.5"},
		{"anthropic.claude-sonnet-4-6", true, "sonnet-4.6"},
		{"claude-opus-4@20250514", true, "opus-4"},
		// Not priced, never guessed.
		{"codex-auto-review", false, ""},
		{"unknown", false, ""},
		{"opus", false, ""},
		{"fable", false, ""},
		{"<synthetic>", false, ""},
		{"jev-latest", false, ""},
		{"claude-opus-5-6", false, ""}, // a later version is not the earlier one
		{"claude-opus-5-5-fast", false, ""},
		{"gpt-5.5-pro", false, ""},   // no cached-input price is published
		{"gpt-5.6-cyber", false, ""}, // access-restricted, not a coding-agent model
		{"claude-opus-4-6-v2", false, ""},
		// Models the vendors price but the table leaves out on purpose.
		{"gpt-5.4-pro", false, ""}, {"gpt-5-pro", false, ""}, {"o3", false, ""}, {"o4-mini", false, ""},
		{"gpt-4.1", false, ""}, {"gpt-4o", false, ""}, {"chat-latest", false, ""},
		{"claude-3-7-sonnet-20250219", false, ""}, {"claude-3-haiku-20240307", false, ""}, {"claude-mythos-preview", false, ""},
		{"", false, ""},
	} {
		entry, ok := table.Lookup(tc.id)
		if ok != tc.priced || (ok && entry.Family != tc.family) {
			t.Errorf("Lookup(%q) = %+v, %v; want priced=%v family=%q", tc.id, entry, ok, tc.priced, tc.family)
		}
	}
}

func TestNormalizeModel(t *testing.T) {
	t.Parallel()
	if got := NormalizeModel("  claude-opus-5-5  "); got != "claude-opus-5-5" {
		t.Fatalf("NormalizeModel did not trim: %q", got)
	}
	for in, want := range map[string]string{
		"Claude-Opus-5-5[1m]":          "claude-opus-5-5",
		"claude-haiku-4-5-20251001":    "claude-haiku-4-5",
		"claude-haiku-4-5@20251001":    "claude-haiku-4-5",
		"claude-opus-4-1":              "claude-opus-4-1", // a version, not a date
		"claude-3-5-sonnet-2024102":    "claude-3-5-sonnet-2024102",
		"gpt-5.6-terra":                "gpt-5.6-terra",
		"gpt-5-2025-08-07":             "gpt-5",
		"gpt-5.3-codex":                "gpt-5.3-codex",
		"":                             "",
		"claude-sonnet-4-20250514[1m]": "claude-sonnet-4",
		// The same model named by a cloud platform.
		"anthropic/claude-opus-5-5":                            "claude-opus-5-5",
		"openai/gpt-5":                                         "gpt-5",
		"anthropic.claude-opus-4-1-20250805-v1:0":              "claude-opus-4-1",
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0":         "claude-sonnet-4-5",
		"global.anthropic.claude-haiku-4-5-20251001-v1:0":      "claude-haiku-4-5",
		"us-gov.anthropic.claude-sonnet-4-5-v1:0":              "claude-sonnet-4-5",
		"publishers/anthropic/models/claude-opus-4-5@20251101": "claude-opus-4-5",
		"deepseek-v3":               "deepseek-v3", // a version in a name, not Bedrock's
		"anthropic.":                "",
		"/":                         "",
		"claude-3-5-haiku-20241022": "claude-3-5-haiku",
		// Bedrock's one colon-less version suffix, only behind its vendor prefix.
		"anthropic.claude-opus-4-6-v1":        "claude-opus-4-6",
		"global.anthropic.claude-opus-4-6-v1": "claude-opus-4-6",
		"claude-opus-4-6-v1":                  "claude-opus-4-6-v1",
		"anthropic.claude-opus-4-6-v2":        "claude-opus-4-6-v2",
		"gpt-5-v1":                            "gpt-5-v1",
	} {
		if got := NormalizeModel(in); got != want {
			t.Errorf("NormalizeModel(%q) = %q, want %q", in, got, want)
		}
	}
}

const validTable = `{"version":"custom-1","as_of":"2026-09-01","models":[
 {"id":"claude-opus-5-5","family":"opus","input_per_mtok":1,"output_per_mtok":2,"cache_read_per_mtok":0.1,"cache_write_per_mtok":1.25},
 {"id":"my-model","input_per_mtok":3,"output_per_mtok":4,"cache_read_per_mtok":0,"cache_write_per_mtok":0}]}`

func TestParsePriceTable(t *testing.T) {
	t.Parallel()
	got, err := ParsePriceTable([]byte(validTable))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "custom-1" || got.Currency != "USD" || len(got.Models) != 2 || got.Models[1].Family != "my-model" || got.Models[1].CacheReadPerMTok != 0 {
		t.Fatalf("table = %+v", got)
	}
	for name, doc := range map[string]string{
		"not json":         `{`,
		"no version":       `{"as_of":"2026-09-01","models":[{"id":"a","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1}]}`,
		"bad date":         `{"version":"v","as_of":"Sept 2026","models":[{"id":"a","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1}]}`,
		"no models":        `{"version":"v","as_of":"2026-09-01","models":[]}`,
		"missing price":    `{"version":"v","as_of":"2026-09-01","models":[{"id":"a","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1}]}`,
		"negative":         `{"version":"v","as_of":"2026-09-01","models":[{"id":"a","input_per_mtok":-1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1}]}`,
		"unknown field":    `{"version":"v","as_of":"2026-09-01","models":[{"id":"a","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1,"cache_per_mtok":1}]}`,
		"duplicate id":     `{"version":"v","as_of":"2026-09-01","models":[{"id":"a","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1},{"id":"A[1m]","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1}]}`,
		"empty id":         `{"version":"v","as_of":"2026-09-01","models":[{"id":" ","input_per_mtok":1,"output_per_mtok":1,"cache_read_per_mtok":1,"cache_write_per_mtok":1}]}`,
		"trailing garbage": validTable + `{}`,
		// A currency and a version are printed on the screen: a code, and a label.
		"long currency":     strings.Replace(validTable, `"as_of"`, `"currency":"ABCDEFGHIJ","as_of"`, 1),
		"currency symbol":   strings.Replace(validTable, `"as_of"`, `"currency":"$","as_of"`, 1),
		"digits in code":    strings.Replace(validTable, `"as_of"`, `"currency":"U5D","as_of"`, 1),
		"control in code":   strings.Replace(validTable, `"as_of"`, `"currency":"U\u001bD","as_of"`, 1),
		"very long version": strings.Replace(validTable, `custom-1`, strings.Repeat("v", 65), 1),
	} {
		if _, err := ParsePriceTable([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParsePriceTable([]byte(strings.Replace(validTable, `"cache_read_per_mtok":0.1,`, "", 1))); err == nil || !strings.Contains(err.Error(), "cache_read_per_mtok") {
		t.Errorf("a missing price must name the field, got %v", err)
	}
}

func TestOverridesReplaceAndAddEntries(t *testing.T) {
	t.Parallel()
	custom, err := ParsePriceTable([]byte(validTable))
	if err != nil {
		t.Fatal(err)
	}
	merged := DefaultPriceTable().WithOverrides(custom)
	if !merged.Overridden || merged.Version != "custom-1" || merged.AsOf != "2026-09-01" {
		t.Fatalf("merged header = %+v", merged)
	}
	if e, _ := merged.Lookup("claude-opus-5-5"); e.InputPerMTok != 1 || e.OutputPerMTok != 2 {
		t.Fatalf("the override did not replace: %+v", e)
	}
	if _, ok := merged.Lookup("my-model"); !ok {
		t.Fatal("the new model was not added")
	}
	if e, ok := merged.Lookup("claude-sonnet-5-5"); !ok || e.InputPerMTok != 2 {
		t.Fatalf("an untouched default was lost: %+v", e)
	}
	if len(merged.Models) != len(DefaultPriceTable().Models)+1 {
		t.Fatalf("merged has %d models", len(merged.Models))
	}
	if DefaultPriceTable().Overridden {
		t.Fatal("the default table is marked overridden")
	}

	m := meta("a", "claude", day(time.September, 28, 10), modelTokens("claude-opus-5-5", 1_000_000, 0, 0, 0))
	got := Compute([]archive.Metadata{m}, Options{Now: now, Location: newYork, PriceTable: merged})
	if v := got.Overview.Cost.Value; v == nil || !near(*v, 1) || got.Prices.Version != "custom-1" || !got.Prices.Overridden {
		t.Fatalf("cost %v prices %+v", f64(v), got.Prices)
	}
}

// Prices in another currency replace the built-in ones: relabelling dollar
// prices as euros would be a silent, wrong estimate.
func TestOverridesInAnotherCurrencyDoNotKeepDollarPrices(t *testing.T) {
	t.Parallel()
	custom, err := ParsePriceTable([]byte(strings.Replace(validTable, `"as_of"`, `"currency":"eur","as_of"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	merged := DefaultPriceTable().WithOverrides(custom)
	if merged.Currency != "EUR" || len(merged.Models) != 2 {
		t.Fatalf("merged = %s with %d models, want EUR with only the custom 2", merged.Currency, len(merged.Models))
	}
	if _, ok := merged.Lookup("claude-sonnet-5-5"); ok {
		t.Fatal("a dollar price survived into a euro table")
	}
	same, err := ParsePriceTable([]byte(strings.Replace(validTable, `"as_of"`, `"currency":"usd","as_of"`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if merged := DefaultPriceTable().WithOverrides(same); merged.Currency != "USD" || len(merged.Models) != len(DefaultPriceTable().Models)+1 {
		t.Fatalf("same-currency override lost the defaults: %s, %d models", merged.Currency, len(merged.Models))
	}
}

func TestAnExplicitEmptyTableLeavesEverythingUnpriced(t *testing.T) {
	t.Parallel()
	m := meta("a", "claude", day(time.September, 28, 10), modelTokens("claude-opus-5-5", 1000, 0, 0, 0))
	got := Compute([]archive.Metadata{m}, Options{Now: now, Location: newYork, PriceTable: PriceTable{Version: "none", AsOf: "2026-09-01"}})
	if got.Overview.Cost.Value != nil || !got.Overview.Cost.Partial || got.Prices.Version != "none" {
		t.Fatalf("cost = %+v prices %+v", got.Overview.Cost, got.Prices)
	}
	dflt := Compute([]archive.Metadata{m}, Options{Now: now, Location: newYork})
	if dflt.Prices.Version != DefaultPriceTable().Version || dflt.Prices.AsOf != DefaultPriceTable().AsOf {
		t.Fatalf("default prices = %+v", dflt.Prices)
	}
}

func TestEachTokenTypeIsPricedSeparately(t *testing.T) {
	t.Parallel()
	table := PriceTable{Version: "t", AsOf: "2026-09-01", Currency: "USD", Models: []ModelPrice{
		{ID: "m", Family: "m", InputPerMTok: 1, OutputPerMTok: 10, CacheReadPerMTok: 100, CacheWritePerMTok: 1000},
	}}
	for _, tc := range []struct {
		name  string
		input int
		out   int
		read  int
		write int
		want  float64
	}{
		{"fresh input only", 1_000_000, 0, 0, 0, 1},
		{"output only", 0, 1_000_000, 0, 0, 10},
		{"cache read only", 0, 0, 1_000_000, 0, 100},
		{"cache write only", 0, 0, 0, 1_000_000, 1000},
	} {
		m := meta("a", "claude", day(time.September, 28, 10), modelTokens("m", tc.input, tc.out, tc.read, tc.write))
		got := Compute([]archive.Metadata{m}, Options{Now: now, Location: newYork, PriceTable: table})
		if v := got.Overview.Cost.Value; v == nil || !near(*v, tc.want) {
			t.Errorf("%s: cost = %v, want %v", tc.name, f64(v), tc.want)
		}
	}
	// Reasoning tokens are part of output and add nothing.
	m := meta("a", "claude", day(time.September, 28, 10), modelTokens("m", 0, 1_000_000, 0, 0))
	m.ModelTokens[0].ReasoningTokens = ip(900_000)
	if v := Compute([]archive.Metadata{m}, Options{Now: now, Location: newYork, PriceTable: table}).Overview.Cost.Value; !near(*v, 10) {
		t.Fatalf("reasoning was priced on top of output: %v", *v)
	}
}

// Family answers for the models a table lists, as the archive records them,
// and for nothing else: a fine-tune id, a family name and a model no table
// prices are not listed.
func TestFamilyIsOnlyForListedModels(t *testing.T) {
	t.Parallel()
	table := DefaultPriceTable()
	cases := []struct {
		model  string
		family string
		listed bool
	}{
		{"claude-opus-5", "opus-5", true},
		{"  CLAUDE-OPUS-5-20250101 ", "opus-5", true},
		{"claude-opus-5[1m]", "opus-5", true},
		{"gpt-5", "gpt-5", true},
		{"opus", "", false},
		{"ft:gpt-4o:acme::abc", "", false},
		{"codex-auto-review", "", false},
		{"unknown", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		family, listed := table.Family(c.model)
		if family != c.family || listed != c.listed {
			t.Errorf("Family(%q) = %q, %v; want %q, %v", c.model, family, listed, c.family, c.listed)
		}
	}
	// It reads the table it is asked of: a table's own family is its answer,
	// a family left empty falls back to the id, and an entry with an invalid
	// price is not listed.
	custom := PriceTable{Models: []ModelPrice{
		{ID: "acme-model", Family: "acme"},
		{ID: "no-family"},
		{ID: "bad", InputPerMTok: -1},
	}}
	for model, want := range map[string]string{"acme-model": "acme", "NO-FAMILY": "no-family", "bad": ""} {
		if family, listed := custom.Family(model); family != want || listed != (want != "") {
			t.Errorf("custom Family(%q) = %q, %v; want %q", model, family, listed, want)
		}
	}
}

// Each built-in model is its own row in cost by model, named for its exact
// version: GPT-6.1 Sol is not lumped in with GPT-6 Luna, nor Opus 5.5 with Opus
// 5. Only two names of one model share a label.
func TestBuiltInModelsAreLabelledByExactVersion(t *testing.T) {
	t.Parallel()
	aliases := map[string]string{"gpt-5.6": "gpt-5.6-sol", "claude-3-5-haiku": "claude-haiku-3-5"}
	owner := map[string]string{}
	for _, m := range DefaultPriceTable().Models {
		id := m.ID
		if target, ok := aliases[id]; ok {
			id = target
		}
		if prev, ok := owner[m.Family]; ok && prev != id {
			t.Errorf("%s and %s share the label %q", prev, id, m.Family)
		}
		owner[m.Family] = id
	}
}

// An alias row is the model it routes to, price for price: OpenAI's docs say
// the gpt-5.6 alias routes to gpt-5.6-sol, so a table that priced them apart
// would price the same tokens two ways.
func TestAliasRowsMatchTheirModel(t *testing.T) {
	t.Parallel()
	table := DefaultPriceTable()
	alias, ok := table.Lookup("gpt-5.6")
	if !ok {
		t.Fatal("gpt-5.6 is not priced")
	}
	sol, ok := table.Lookup("gpt-5.6-sol")
	if !ok {
		t.Fatal("gpt-5.6-sol is not priced")
	}
	alias.ID, sol.ID = "", ""
	if alias != sol {
		t.Fatalf("gpt-5.6 = %+v, gpt-5.6-sol = %+v", alias, sol)
	}
}

// The notes say what the estimate leaves out, so a reader can tell which way it
// is biased. A rewrite that drops one of these should be a decision, not an
// accident.
func TestPriceNotesNameWhatIsNotModelled(t *testing.T) {
	t.Parallel()
	notes := DefaultPriceTable().Notes
	for _, want := range []string{
		"5-minute", "1-hour", "272K", "fast mode", "Batch", "data-residency", "2026-11-21", "unpriced", "codex-auto-review",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes do not mention %q", want)
		}
	}
}
