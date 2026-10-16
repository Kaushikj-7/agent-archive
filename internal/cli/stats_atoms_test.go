package cli

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/statsfmt"
)

// cutUsageStats is realisticStats with more skills and MCP servers than the
// overview and the detail screen list: 45 of each, the first four the
// realistic ones, so both say how many more there are.
func cutUsageStats() stats.Stats {
	s := realisticStats()
	skills := []string{"code-review", "review-pr", "docs", "cursor-guide"}
	servers := []string{"github", "linear"}
	for i := len(skills); i < 45; i++ {
		skills = append(skills, fmt.Sprintf("skill-%02d", i))
	}
	for i := len(servers); i < 45; i++ {
		servers = append(servers, fmt.Sprintf("server-%02d", i))
	}
	s.Skills, s.DisplaySkills, s.MCP.Servers = nil, nil, nil
	for i, name := range skills {
		sk := stats.Skill{Name: name, Sessions: 100 - i}
		s.Skills = append(s.Skills, sk)
		s.DisplaySkills = append(s.DisplaySkills, sk)
	}
	for i, name := range servers {
		s.MCP.Servers = append(s.MCP.Servers, stats.MCPServer{Name: name, Calls: int64(5000 - 7*i), Sessions: 1})
	}
	s.TotalSkills, s.TotalDisplaySkills, s.MCP.TotalServers = len(skills), len(skills), len(servers)
	return s
}

// A command or a name with its count is wrapped whole: the atoms are what a
// line may break between.
func TestCommandAtoms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		lead   string
		flags  string
		suffix string
		room   int
		want   []string
	}{
		{"flags alone", "", "--json --by project", ")", 40, []string{"--json --by project)"}},
		{"the whole command fits", "agent-archive stats --days 30", "--json --all", ")", 60, []string{"agent-archive stats --days 30 --json --all)"}},
		{"the whole command just fits", "agent-archive stats --days 30", "--json --all", ")", 43, []string{"agent-archive stats --days 30 --json --all)"}},
		{"the whole command does not fit", "agent-archive stats --days 30", "--json --all", ")", 42, []string{"agent-archive stats --days 30", "--json --all)"}},
		{"a comma stays with the last atom", "agent-archive stats --days 30", "--json --all", ",", 20, []string{"agent-archive stats --days 30", "--json --all,"}},
	} {
		if got := commandAtoms(tc.lead, tc.flags, tc.suffix, tc.room); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Atoms stay whole on a line while they fit, and fall back to their words when
// one is wider than the room; no line is wider than the screen.
func TestPackAtoms(t *testing.T) {
	t.Parallel()
	p := &statsPrinter{width: 20}
	for _, tc := range []struct {
		name   string
		first  int
		indent int
		atoms  []string
		want   []string
	}{
		{"fits on one line", 0, 0, []string{"a b", "c d"}, []string{"a b c d"}},
		{"breaks between atoms", 0, 0, []string{"github 41 ·", "linear 12 ·", "docs 3"}, []string{"github 41 ·", "linear 12 · docs 3"}},
		{"indents the following lines", 8, 8, []string{"aaaa bbbb", "cccc dddd"}, []string{"aaaa bbbb", "        cccc dddd"}},
		{"an atom wider than the room falls back to its words", 0, 4, []string{"x", "--json --by project --all", "y"}, []string{"x --json --by", "    project --all y"}},
	} {
		if got := p.packAtoms(tc.first, tc.indent, tc.atoms); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// At every width the screens adapt to, in color or not, in Unicode or ASCII,
// the skills and MCP rows never break a name from its count, and the hint
// under them never breaks the command it names: the flags (and the whole
// command, when a line is wide enough for it) are on one line. No line is
// wider than the terminal.
func TestStatsUsageRowsKeepNamesCountsAndCommandsTogether(t *testing.T) {
	t.Parallel()
	s := cutUsageStats()
	type mode struct {
		color       bool
		ascii       bool
		interactive bool
		filters     statsFilters
	}
	modes := []mode{
		{false, false, false, statsFilters{}},
		{true, false, false, statsFilters{}},
		{false, true, false, statsFilters{}},
		{false, false, true, statsFilters{}},
		{true, true, true, statsFilters{Harness: "claude", Model: "opus"}},
	}
	for _, page := range []statsPage{pageOverview, pageDetail} {
		shownSkills, shownServers := overviewSkills, overviewMCP
		if page == pageDetail {
			shownSkills, shownServers = statsMaxUseRows, statsMaxUseRows
		}
		for _, width := range statsMatrixWidths() {
			for _, m := range modes {
				glyphs := unicodeGlyphs
				if m.ascii {
					glyphs = asciiGlyphs
				}
				view := statsView{style: textStyle{color: m.color}, width: width, glyphs: glyphs, interactive: m.interactive, filters: m.filters}
				out := stripANSI(strings.Join(renderPage(page, s, view), "\n"))
				where := fmt.Sprintf("%s at %d columns (%+v)", page, width, m)
				checkTerminalSafe(t, where, out, min(width, statsMaxWidth))
				lines := strings.Split(out, "\n")
				onOneLine := func(text string) bool {
					return slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, text) })
				}
				for i, sk := range s.DisplaySkills[:shownSkills] {
					if pair := fmt.Sprintf("%s %s", sk.Name, statsfmt.CommaInt(int64(sk.Sessions))); !onOneLine(pair) && visibleWidth(pair) < statsMinWidth-useLabelWidth-2 {
						t.Fatalf("%s: skill %d, %q, is split over lines:\n%s", where, i, pair, out)
					}
				}
				for i, srv := range s.MCP.Servers[:shownServers] {
					if pair := fmt.Sprintf("%s %s", srv.Name, statsfmt.CommaInt(srv.Calls)); !onOneLine(pair) && visibleWidth(pair) < statsMinWidth-useLabelWidth-2 {
						t.Fatalf("%s: server %d, %q, is split over lines:\n%s", where, i, pair, out)
					}
				}
				flags := "--json --all"
				if !onOneLine(flags+")") && !onOneLine(flags+",") {
					t.Fatalf("%s: the command %q is split over lines:\n%s", where, flags, out)
				}
				if m.interactive {
					command := "agent-archive stats --days 30 " + flags
					if room := width - useLabelWidth - 2; visibleWidth(command+",") <= room && !onOneLine(command+")") && !onOneLine(command+",") {
						t.Fatalf("%s: the command %q fits a line and is split over lines:\n%s", where, command, out)
					}
				}
			}
		}
	}
}

// statsMatrixWidths keeps the full width sweep by default. Short mode keeps
// the narrow command-wrapping widths and each layout threshold's neighbors.
func statsMatrixWidths() []int {
	if testing.Short() {
		var widths []int
		for w := statsMinWidth; w <= statsBarWidth+5; w++ {
			widths = append(widths, w)
		}
		return append(widths, statsFullWidth-1, statsFullWidth, statsFullWidth+1,
			statsUnknownWidth-1, statsUnknownWidth, statsUnknownWidth+1,
			statsMaxWidth-1, statsMaxWidth, statsMaxWidth+1, 250)
	}
	var widths []int
	for w := statsMinWidth; w <= 250; w++ {
		widths = append(widths, w)
	}
	return widths
}
