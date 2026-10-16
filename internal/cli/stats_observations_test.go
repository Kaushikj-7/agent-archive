package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statshtml"
)

func TestObservationStatsAgreeAcrossTerminalJSONAndHTML(t *testing.T) {
	t.Parallel()
	var a archive.ObservationAccumulator
	if a.Add("<img src=x onerror=alert(1)>", 202, 46) != nil {
		t.Fatal("synthetic")
	}
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	m := archive.Metadata{SessionID: "synthetic", CapturedAt: now, Harness: archive.Harness{Name: "muse"}, TokenObservations: a.Summary()}
	s := stats.Compute([]archive.Metadata{m}, stats.Options{Now: now, Days: 1, Location: time.UTC})
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var doc stats.Stats
	if json.Unmarshal(raw, &doc) != nil || *doc.TokenObservations.Input != 202 || doc.Overview.Cost.Value != nil || doc.Composition != nil {
		t.Fatal("JSON observations")
	}
	html, err := statshtml.Render(s, statshtml.Options{IncludeNames: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []statsPage{pageOverview, pageDetail, pageProjects, pageModels, pageAgents} {
		text := strings.Join(renderPage(page, s, statsView{width: 120, glyphs: unicodeGlyphs}), "\n")
		if strings.Contains(text, "no token data") || strings.Contains(string(html), "no token data") {
			t.Fatal("observations described as absent token data")
		}
		for _, expected := range []string{"Observed input 202, output 46", "full usage, composition and cost unavailable", "excludes 1 observation-only sessions"} {
			if !strings.Contains(text, expected) || !strings.Contains(string(html), expected) {
				t.Fatalf("missing %q on %v", expected, page)
			}
		}
	}
	if strings.Contains(string(html), "<img") || strings.Contains(string(html), "onerror") {
		t.Fatal("observation labels escaped privacy boundary")
	}
}

func TestStatsModelFilterSelectsObservationsWithoutPricing(t *testing.T) {
	const futureAvailability archive.AvailabilityState = "future"
	t.Parallel()
	env, mem := statsEnv(t)
	var a archive.ObservationAccumulator
	if a.Add("claude-sonnet-4", 0, 0) != nil {
		t.Fatal("synthetic")
	}
	m := syntheticSession{id: "observed-model", harness: "muse", project: "synthetic", captured: statsNow}.build()
	m.TokenObservations = a.Summary()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey("muse", m.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err = mem.Put(t.Context(), key, raw); err != nil {
		t.Fatal(err)
	}
	// The actual CLI reader filter selects a measured zero with a known priced model.
	out := mustRunStats(t, env, 0, "--json", "--model", "claude-sonnet-4", "--no-cache")
	var doc statsDocument
	if err = json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Coverage.Sessions != 1 || doc.TokenObservations == nil || *doc.TokenObservations.Input != 0 || doc.Overview.Cost.Value != nil || doc.Composition != nil || doc.Coverage.ObservationOnlySessions != 1 {
		t.Fatal("filtered observation was omitted or priced")
	}
	for _, state := range []archive.AvailabilityState{"", archive.AvailabilityUnknown, archive.AvailabilityUnavailable, futureAvailability} {
		m.TokenObservations.Observed.State = state
		raw, _ = json.Marshal(m)
		if err = mem.Put(t.Context(), key, raw); err != nil {
			t.Fatal(err)
		}
		out = mustRunStats(t, env, 0, "--json", "--model", "claude-sonnet-4", "--no-cache")
		doc = statsDocument{}
		if err = json.Unmarshal([]byte(out), &doc); err != nil || doc.Coverage.Sessions != 0 || doc.TokenObservations != nil {
			t.Fatal("unavailable observation selected", err)
		}
	}
}

func TestMixedObservationsDiscloseSubsetWithoutCatalogGap(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	input, output := 1000, 100
	priced := archive.Metadata{SessionID: "priced", CapturedAt: now, Harness: archive.Harness{Name: "claude"}, ModelTokens: []archive.ModelTokens{{Model: "claude-sonnet-4", InputTokens: &input, OutputTokens: &output}}}
	var a archive.ObservationAccumulator
	if a.Add("claude-sonnet-4", 101, 23) != nil {
		t.Fatal("synthetic")
	}
	observed := archive.Metadata{SessionID: "observed", CapturedAt: now, Harness: archive.Harness{Name: "muse"}, TokenObservations: a.Summary()}
	opts := stats.Options{Now: now, Days: 1, Location: time.UTC}
	legacy := stats.Compute([]archive.Metadata{priced}, opts)
	mixed := stats.Compute([]archive.Metadata{priced, observed}, opts)
	if legacy.Overview.Cost.Value == nil || mixed.Overview.Cost.Value == nil || *legacy.Overview.Cost.Value != *mixed.Overview.Cost.Value || !mixed.Overview.Cost.Partial || mixed.Overview.Cost.UnpricedTokens != 0 {
		t.Fatal("qualified subset changed")
	}
	html, err := statshtml.Render(mixed, statshtml.Options{})
	if err != nil {
		t.Fatal(err)
	}
	outputs := []string{string(html)}
	for _, page := range []statsPage{pageOverview, pageDetail, pageProjects, pageModels, pageAgents} {
		outputs = append(outputs, strings.Join(renderPage(page, mixed, statsView{width: 120, glyphs: unicodeGlyphs}), "\n"))
	}
	for i, text := range outputs {
		if !strings.Contains(text, "excludes 1 observation-only sessions") || strings.Contains(text, "models with no price") || strings.Contains(text, "price table does not list") {
			t.Fatalf("output %d: known priced observations described as absent catalog models", i)
		}
	}
}
