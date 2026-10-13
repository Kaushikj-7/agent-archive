package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/stats"
)

func TestStatsConfiguredMCPNamesAcrossOutputsAndWindows(t *testing.T) {
	env, mem := statsEnv(t)
	id := "1a59c906-04da-521d-bda7-0123456789ab"
	syntheticSession{id: "named-server", harness: "claude", captured: statsNow,
		models: []string{"claude-opus-5"}, turns: 1,
		perModel: []modelTokenSpec{{"claude-opus-5", 1000, 100, 0, 0}},
		mcp:      map[string]int{id: 60, "Claude_Browser": 436},
	}.publish(t, mem)
	home, err := env.readHome()
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MCPServerNames = map[string]string{id: "GitHub"}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	text := mustRunStats(t, env, 100, "--prices", goldenPrices)
	if !strings.Contains(text, "GitHub 60") || !strings.Contains(text, "Claude Browser 436") || strings.Contains(text, id) {
		t.Fatalf("MCP labels not resolved:\n%s", text)
	}
	namedHTML := mustRunStats(t, env, 0, "--html", "--include-names", "--prices", goldenPrices)
	if !strings.Contains(namedHTML, "GitHub") || strings.Contains(namedHTML, id) {
		t.Fatal("HTML labels not resolved")
	}
	privateHTML := mustRunStats(t, env, 0, "--html", "--prices", goldenPrices)
	if strings.Contains(privateHTML, "GitHub") || strings.Contains(privateHTML, id) {
		t.Fatal("shareable HTML exposes an MCP name")
	}
	var doc statsDocument
	if err := json.Unmarshal([]byte(mustRunStats(t, env, 0, "--json", "--prices", goldenPrices)), &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, srv := range doc.MCP.Servers {
		if srv.Name == id {
			found = srv.DisplayName == "GitHub" && srv.Calls == 60
		}
	}
	if !found {
		t.Fatal("JSON lost the server identity or count")
	}
	in := statsInputs{hasSessions: true, prepared: stats.Prepare([]archive.Metadata{syntheticSession{id: "window", harness: "claude", captured: statsNow, mcp: map[string]int{id: 60}}.build()}, stats.PrepareOptions{}), now: statsNow, mcpServerNames: cfg.MCPServerNames}
	for _, days := range []int{7, 30, 90} {
		if s := in.compute(days, true); s.MCP.Servers[0].Label() != "GitHub" {
			t.Fatalf("alias lost switching to %d days", days)
		}
	}
}

func TestStatsChartOmitsLeadingDaysAndSeparatesCoverage(t *testing.T) {
	s := realisticStats()
	s.Coverage.FirstRecordedDay = "2026-09-26"
	out := strings.Join(pageLines(pageOverview, s, 80, false, false), "\n")
	chart := out[strings.Index(out, "DAILY SPEND"):strings.Index(out, "WHERE IT WENT")]
	if strings.Contains(chart, "Aug 31") || !strings.Contains(chart, "Sep 26") || !strings.Contains(chart, "Earlier days omitted") {
		t.Fatalf("chart implies earlier observation:\n%s", chart)
	}
	coverageStart := strings.Index(out, "COVERAGE")
	if coverageStart < 0 {
		t.Fatal("coverage section missing")
	}
	findings := out[strings.Index(out, "FINDINGS"):coverageStart]
	if strings.Contains(findings, "no token data") || !strings.Contains(findings, "77% of tokens came from subagents") {
		t.Fatal("coverage and findings mixed")
	}
	if !strings.Contains(out[coverageStart:], "no token data") {
		t.Fatal("coverage note lost")
	}
}

func TestStatsHistoryCaptionSurvivesNarrowRendering(t *testing.T) {
	for _, long := range []bool{false, true} {
		for _, width := range []int{40, 60, 80} {
			for _, color := range []bool{false, true} {
				s := realisticStats()
				s.Coverage.FirstRecordedDay = "2026-09-01"
				if long {
					start := time.Date(2026, 3, 14, 0, 0, 0, 0, time.UTC)
					s.Window.FirstDay = start.Format("2006-01-02")
					s.Window.Days = 200
					s.Coverage.FirstRecordedDay = start.AddDate(0, 0, 1).Format("2006-01-02")
					s.Daily = make([]stats.Day, 200)
					for i := range s.Daily {
						s.Daily[i] = stats.Day{Date: start.AddDate(0, 0, i).Format("2006-01-02"), Cost: usd(0)}
					}
				}
				// Include an unknown-spend caption as well as the history note.
				s.Daily[1].Sessions = 1
				s.Daily[1].Cost = stats.Cost{}
				if long {
					// Keep the entire first bucket unknown after grouping days.
					for i := 2; i < 7; i++ {
						s.Daily[i].Sessions = 1
						s.Daily[i].Cost = stats.Cost{}
					}
				}
				out := strings.Join(pageLines(pageOverview, s, width, color, false), "\n")
				chart := out[strings.Index(out, "DAILY SPEND"):strings.Index(out, "WHERE IT WENT")]
				plain := strings.Join(strings.Fields(stripANSI(chart)), " ")
				for _, want := range []string{"spend unknown", "Earlier days omitted; empty days mean no archived sessions."} {
					if !strings.Contains(plain, want) {
						t.Errorf("width %d, color %v, long %v: caption lost %q:\n%s", width, color, long, want, chart)
					}
				}
				if long && !strings.Contains(plain, "each bar is the costliest of") {
					t.Errorf("grouped-day caption lost:\n%s", chart)
				}
			}
		}
	}
}
