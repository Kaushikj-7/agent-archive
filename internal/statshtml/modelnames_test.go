package statshtml

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
)

// A model id can name a client, so the sessions below use ids no price table
// lists: a fine-tune, a deployment name, and one built to break out of markup.
// Each carries a marker ("zqcorp") that must not reach a shareable page in any
// spelling.
const (
	fineTune   = "ft:gpt-4o:zqcorp::abc"
	deployment = "zqcorp-prod-eu"
	scriptName = "zqcorp</script><script>alert(1)</script>"
)

// modelSessions is an archive that uses priced models (claude-opus-5 and
// gpt-5, both in the built-in table), public models no table prices (the
// archive's own "unknown" and "other", Codex's "codex-auto-review"), and the
// hostile ids. Among the unpriced models the fine-tune has the most tokens.
func modelSessions() []archive.Metadata {
	specs := []sessionSpec{
		{
			id: "claude", harness: "claude", project: "p", captured: day(time.September, 20, 10),
			models: []string{"claude-opus-5", fineTune}, turns: 5, messages: 20, toolResults: 10, errors: 1,
			tokens: []tokenSpec{
				{"claude-opus-5", 10_000, 20_000, 300_000, 40_000},
				{fineTune, 100_000, 200_000, 3_000_000, 0},
				{"CLAUDE-OPUS-5-20250101", 1, 1, 1, 1},
			},
		},
		{
			id: "codex", harness: "codex", project: "p", captured: day(time.September, 21, 10),
			models: []string{"gpt-5", "codex-auto-review"}, turns: 3,
			tokens: []tokenSpec{
				{"gpt-5", 90_000, 8_000, 70_000, 0},
				{"codex-auto-review", 10_000, 1_000, 5_000, 0},
				{deployment, 20_000, 2_000, 900_000, 0},
				{scriptName, 3_000, 300, 20_000, 0},
				{archive.UnknownModel, 2_000, 200, 1_000, 0},
				{archive.OtherModels, 1_000, 100, 500, 0},
			},
		},
	}
	out := make([]archive.Metadata, len(specs))
	for i, s := range specs {
		out[i] = s.build()
	}
	return out
}

// modelStats computes the stats of sessions at the fixture clock, priced by
// the golden table or, when given, by prices.
func modelStats(tb testing.TB, sessions []archive.Metadata, prices string, by stats.Grouping) stats.Stats {
	tb.Helper()
	if prices == "" {
		prices = goldenPrices
	}
	table, err := stats.ParsePriceTable([]byte(prices))
	if err != nil {
		tb.Fatal(err)
	}
	return stats.Compute(sessions, stats.Options{Now: fixtureNow, Days: 30, Location: time.UTC, PriceTable: table, By: by})
}

// customPrices is a --prices file: it prices the custom ids under a family of
// the person's own and renames the family of a built-in model.
const customPrices = `{
  "version": "mine-1",
  "as_of": "2026-09-29",
  "models": [
    {"id": "ft:gpt-4o:zqcorp::abc", "family": "zqcorp-tuned", "input_per_mtok": 1, "output_per_mtok": 2, "cache_read_per_mtok": 0.1, "cache_write_per_mtok": 0},
    {"id": "zqcorp-prod-eu",        "family": "zqcorp-tuned", "input_per_mtok": 1, "output_per_mtok": 2, "cache_read_per_mtok": 0.1, "cache_write_per_mtok": 0},
    {"id": "gpt-5",                 "family": "zqcorp-gpt",   "input_per_mtok": 1, "output_per_mtok": 2, "cache_read_per_mtok": 0.1, "cache_write_per_mtok": 0}
  ]
}`

// leakedMarkers lists the raw custom names found in a page, in any case.
func leakedMarkers(page string) []string {
	lower := strings.ToLower(page)
	var found []string
	for _, marker := range []string{"zqcorp", "ft:gpt-4o", "prod-eu", "tuned", "alert(1)"} {
		if strings.Contains(lower, marker) {
			found = append(found, marker)
		}
	}
	return found
}

// Every section of a shareable page is grepped for the custom model ids: the
// cost-by-model table and its notes, the unpriced note in the footer, the
// favorite model, the heading's filters and every breakdown. Prices from a
// person's own file cannot make a name shareable either.
func TestShareablePageNamesNoCustomModel(t *testing.T) {
	t.Parallel()
	for _, prices := range []string{"", customPrices} {
		for _, by := range []stats.Grouping{stats.GroupNone, stats.GroupDay, stats.GroupWeek, stats.GroupMonth, stats.GroupProject} {
			s := modelStats(t, modelSessions(), prices, by)
			for _, model := range []string{"", fineTune, deployment, scriptName, "claude-opus-5"} {
				page := string(render(t, s, Options{Filters: Filters{Model: model, Harness: "claude"}}))
				if found := leakedMarkers(page); len(found) > 0 {
					t.Errorf("by %q, filter %q, own prices %v: the shareable page names %q", by, model, prices != "", found)
				}
				if !strings.Contains(page, "model A") {
					t.Errorf("by %q: the page has no stand-in for an unrecognized model", by)
				}
			}
		}
	}
}

// The built-in table's models are shown by name, and so are the archive's own
// placeholders; a model nothing lists is a letter in this run's ranking, and
// one model has one label throughout the file.
func TestModelStandInsFollowTheRanking(t *testing.T) {
	t.Parallel()
	s := modelStats(t, modelSessions(), "", stats.GroupNone)
	page := string(render(t, s, Options{}))
	for _, shown := range []string{"opus-5", "gpt-5", archive.UnknownModel, archive.OtherModels} {
		if !strings.Contains(page, ">"+shown+"</th>") {
			t.Errorf("the table does not show %q", shown)
		}
	}
	if strings.Contains(page, "codex-auto-review") {
		t.Error("a model the built-in table does not list is named on the shareable page")
	}
	// The unpriced rows are ranked by tokens: the fine-tune, the deployment,
	// the script name, then codex-auto-review.
	names := newModelNamer(false, s.Models)
	want := []struct {
		id    string
		label string
	}{
		{fineTune, "model A"}, {deployment, "model B"}, {scriptName, "model C"}, {"codex-auto-review", "model D"},
	}
	for _, w := range want {
		if got := names.id(w.id); got != w.label {
			t.Errorf("model %q is shown as %q, want %q", w.id, got, w.label)
		}
		// The label is in the table's row and in the footer's unpriced note
		// (which lists at most three).
		if strings.Count(page, ">"+w.label+"</th>") != 1 {
			t.Errorf("%s is not one row of the table", w.label)
		}
	}
	if !strings.Contains(page, "(model A, model B, model C, …)") {
		t.Error("the footer does not list the unpriced models by their stand-ins")
	}
	if !strings.Contains(page, "replaced by letters in this file") {
		t.Error("the model table does not say its names were replaced")
	}
}

// A model filter is echoed as the table names that model: a public id as it
// is, an unlisted one as its stand-in.
func TestModelFilterIsRedactedLikeTheTable(t *testing.T) {
	t.Parallel()
	s := modelStats(t, modelSessions(), "", stats.GroupNone)
	standIn := regexp.MustCompile(`^Filtered to model [A-Z]\.$`)
	cases := []struct {
		filter string
		want   string
	}{
		{"claude-opus-5", "Filtered to model claude-opus-5."},
		{"Claude-Opus-5-20250101", "Filtered to model Claude-Opus-5-20250101."},
		{"claude-opus-5[1m]", "Filtered to model claude-opus-5[1m]."},
		{fineTune, "Filtered to model A."},
		{"FT:GPT-4O:ZQCORP::ABC", "Filtered to model A."},
		{deployment, "Filtered to model B."},
		{"never-seen-zqcorp", "Filtered to model E."},
	}
	for _, c := range cases {
		page := string(render(t, s, Options{Filters: Filters{Model: c.filter}}))
		if !strings.Contains(page, c.want) {
			t.Errorf("filter %q: the heading is not %q", c.filter, c.want)
		}
		if standIn.MatchString(c.want) && strings.Contains(strings.ToLower(page), strings.ToLower(c.filter)) {
			t.Errorf("filter %q is on the shareable page", c.filter)
		}
	}
}

// The engine finds a listed model behind a path or a cloud vendor's prefix, and
// what it drops can name a client: the heading names the model, never the
// text as typed.
func TestModelFilterWithAPrefixIsNotEchoed(t *testing.T) {
	t.Parallel()
	s := modelStats(t, modelSessions(), "", stats.GroupNone)
	for _, filter := range []string{"zqcorp-client/claude-opus-5", "zqcorp-prod.anthropic.claude-opus-5-v1:0", "ZQCORP/GPT-5",
		"claude-opus-5[zqcorp-client]", "claude-opus-5 [zqcorp]", "claude-opus-5-20251001[zqcorp]", "gpt-5.4[zqcorp]"} {
		page := string(render(t, s, Options{Filters: Filters{Model: filter}}))
		if leaked := leakedMarkers(page); len(leaked) > 0 {
			t.Errorf("filter %q: the page names %q", filter, leaked)
		}
		if !strings.Contains(page, "Filtered to model opus-5.") && !strings.Contains(page, "Filtered to model gpt-5.") {
			t.Errorf("filter %q: the heading does not name the model", filter)
		}
	}
}

// A price file of the person's own names itself: its version can name a client
// or a contract, so a shareable page says that the file was used, not what it
// is called; the built-in table's version is public.
func TestOwnPriceFileVersionIsNotOnAShareablePage(t *testing.T) {
	t.Parallel()
	s := modelStats(t, modelSessions(), "", stats.GroupNone)
	s.Prices.Version = "zqcorp-contract-7"
	s.Prices.Overridden = true
	if page := string(render(t, s, Options{})); leakedMarkers(page) != nil || !strings.Contains(page, "Prices are from your own price file") {
		t.Errorf("the shareable footer names the price file: %q", leakedMarkers(page))
	}
	if page := string(render(t, s, Options{IncludeNames: true})); !strings.Contains(page, "Prices zqcorp-contract-7, as of 2026-09-29, with your own price file applied.") {
		t.Error("--include-names does not show the price file's version")
	}
	s.Prices.Overridden = false
	s.Prices.Version = "2026-09.3"
	if page := string(render(t, s, Options{})); !strings.Contains(page, "Prices 2026-09.3, as of 2026-09-29.") {
		t.Error("the built-in table's version is not shown")
	}
}

// When only unlisted models were used, the favorite model is the one with the
// most tokens, and it is a stand-in too.
func TestFavoriteModelByTokensIsRedacted(t *testing.T) {
	t.Parallel()
	spec := sessionSpec{
		id: "only", harness: "claude", project: "p", captured: day(time.September, 20, 10), models: []string{fineTune}, turns: 2,
		tokens: []tokenSpec{{fineTune, 1_000, 2_000, 3_000, 0}},
	}
	s := modelStats(t, []archive.Metadata{spec.build()}, "", stats.GroupNone)
	if s.Highlights.FavoriteModel == nil || s.Highlights.FavoriteModel.By != "tokens" {
		t.Fatalf("the favorite model is %+v, want one by tokens", s.Highlights.FavoriteModel)
	}
	page := string(render(t, s, Options{}))
	if !strings.Contains(page, "model A (by tokens)") {
		t.Error("the favorite model is not the stand-in")
	}
	if found := leakedMarkers(page); len(found) > 0 {
		t.Errorf("the page names %q", found)
	}
}

// With real names asked for, every model is shown as the engine names it, and
// the page no longer talks about stand-ins for models.
func TestIncludeNamesShowsEveryModel(t *testing.T) {
	t.Parallel()
	s := modelStats(t, modelSessions(), "", stats.GroupNone)
	page := string(render(t, s, Options{IncludeNames: true, Filters: Filters{Model: fineTune}}))
	for _, want := range []string{"codex-auto-review", "Filtered to model " + fineTune + ".", "zqcorp-prod-eu"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page with real names does not show %q", want)
		}
	}
	if strings.Contains(page, "model A") || strings.Contains(page, "replaced by letters") {
		t.Error("the page with real names still talks about model stand-ins")
	}
	if strings.Contains(page, "</script><script>") {
		t.Error("a model name reached the page as markup")
	}
}

// A price file can give a model any family label, and rows are labelled from
// it. A row is shown by name only when the built-in table agrees with its
// label, so a person's own family, even for a built-in model, is not public.
func TestFamilyFromAPersonsOwnPricesIsNotShown(t *testing.T) {
	t.Parallel()
	s := modelStats(t, modelSessions(), customPrices, stats.GroupNone)
	labels := map[string]bool{}
	for _, row := range s.Models {
		labels[row.Label] = true
	}
	if !labels["zqcorp-tuned"] || !labels["zqcorp-gpt"] {
		t.Fatalf("the engine's rows %v do not carry the person's own families, so this test proves nothing", labels)
	}
	page := string(render(t, s, Options{}))
	if found := leakedMarkers(page); len(found) > 0 {
		t.Errorf("the page names %q", found)
	}
	// One family covering two ids is one row and one label.
	names := newModelNamer(false, s.Models)
	if a, b := names.id(fineTune), names.id(deployment); a != b {
		t.Errorf("the two ids of one family are shown as %q and %q", a, b)
	}
}

// Which rows are public is decided by the built-in table alone.
func TestPublicRow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		row  stats.ModelRow
		want bool
	}{
		{"built-in model", stats.ModelRow{Label: "opus-5", Models: []string{"claude-opus-5"}}, true},
		{"built-in alias", stats.ModelRow{Label: "gpt-5.6-sol", Models: []string{"gpt-5.6", "gpt-5.6-sol"}}, true},
		{"two versions under one label", stats.ModelRow{Label: "opus-5", Models: []string{"claude-opus-5", "claude-opus-4-8"}}, false},
		{"unlisted id", stats.ModelRow{Label: "codex-auto-review", Models: []string{"codex-auto-review"}}, false},
		{"one unlisted id in a row", stats.ModelRow{Label: "opus-5", Models: []string{"claude-opus-5", fineTune}}, false},
		{"label of another model", stats.ModelRow{Label: "sonnet-5", Models: []string{"claude-opus-5"}}, false},
		{"own label of a built-in id", stats.ModelRow{Label: "zqcorp-gpt", Models: []string{"gpt-5"}}, false},
		{"unknown", stats.ModelRow{Label: archive.UnknownModel, Models: []string{archive.UnknownModel}}, true},
		{"other", stats.ModelRow{Label: archive.OtherModels, Models: []string{archive.OtherModels}}, true},
		{"placeholder label over a custom id", stats.ModelRow{Label: archive.UnknownModel, Models: []string{fineTune}}, false},
		{"no ids", stats.ModelRow{Label: "opus-5"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := publicRow(c.row); got != c.want {
				t.Errorf("publicRow = %v, want %v", got, c.want)
			}
		})
	}
}
