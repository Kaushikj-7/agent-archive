package statshtml

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/stats"
)

// hostilePayloads are names built to break out of markup, script, style or an
// attribute, to hide or rearrange text, to end a line, or to be taken for a
// template, a URL or another document.
var hostilePayloads = []string{
	`</script><script>alert(1)</script>`,
	`"><img src=x onerror=alert(1)>`,
	`'><svg onload=alert(1)>`,
	"line one\xe2\x80\xa8line two\xe2\x80\xa9end",
	"esc\x1b[31mred\x00nul\x07bell\x7f",
	"\xe2\x80\xaeevil\xe2\x80\xac",
	`</style><style>*{display:none}</style>`,
	`</title></head><body>`,
	`&lt;b&gt;&amp;#60;`,
	"bad utf8 \xff\xfe",
	`javascript:alert(1)`,
	`<!-- comment --><![CDATA[ x ]]>`,
	`</svg><script>alert(1)</script><svg>`,
	`<svg><script>alert(1)</script></svg>`,
	`{{7*7}}{{template "page"}}{{.}}`,
	`data:image/svg+xml,<svg onload=alert(1)>`,
	`url(javascript:alert(1)) expression(alert(1))`,
	`x" onmouseover="alert(1)" style="background:url(//evil.example/x)`,
	"' onmouseover='alert(1)",
	"nul\x00 and bom \xef\xbb\xbf and zero-width \xe2\x80\x8b and isolate \xe2\x81\xa6x\xe2\x81\xa9",
	"\xe2\x80\xa8\xe2\x80\xa9\xe2\x80\xa8",
	`<a href="//evil.example">click</a>`,
	`<meta http-equiv="refresh" content="0;url=//evil.example">`,
	`<iframe src="//evil.example"></iframe>`,
	`%3Cscript%3Ealert(1)%3C/script%3E`,
	`&#x3C;script&#x3E;alert(1)&#x3C;/script&#x3E;`,
	strings.Repeat("A", 100_000),
	strings.Repeat("<", 10_000),
}

// stringLeaf is a text field of the stats, at any depth, named by its path
// with every slice index written as [].
type stringLeaf struct {
	path  string
	value reflect.Value
}

func stringLeaves(s *stats.Stats) []stringLeaf {
	var out []stringLeaf
	var walk func(path string, v reflect.Value)
	walk = func(path string, v reflect.Value) {
		kind := v.Kind()
		if kind == reflect.Struct {
			for i := range v.NumField() {
				if f := v.Type().Field(i); f.IsExported() {
					walk(path+"."+f.Name, v.Field(i))
				}
			}
		}
		if kind == reflect.Pointer && !v.IsNil() {
			walk(path, v.Elem())
		}
		if kind == reflect.Slice {
			for i := range v.Len() {
				walk(path+"[]", v.Index(i))
			}
		}
		if kind == reflect.String && v.CanSet() {
			out = append(out, stringLeaf{path, v})
		}
	}
	walk("Stats", reflect.ValueOf(s).Elem())
	return out
}

// Which text of the stats reaches the page is a decision, and this pins it:
// each text field is set, in turn, to a marker no other text contains, and the
// fields whose marker shows up in the page are listed. A new text field in
// the stats does not reach the page (and so cannot leak) until this list
// says it may. Session IDs, harness IDs, model IDs, price sources and notes
// are not on it. With names hidden, no project, skill or MCP server name is
// either, and no model label: the fixture's marker is not a model the
// built-in price table lists.
func TestOnlyTheseStatsTextsReachThePage(t *testing.T) {
	t.Parallel()
	base := computeFixture(t, fixtureSessions(), 30, stats.GroupProject)
	reached := func(reveal bool) []string {
		seen := map[string]bool{}
		for i, leaf := range stringLeaves(&base) {
			marker := fmt.Sprintf("ZQ%dQZ", i)
			s := deepCopy(base)
			stringLeaves(&s)[i].value.SetString(marker)
			out, err := Render(s, Options{IncludeNames: reveal})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(out), marker) {
				seen[leaf.path] = true
			}
		}
		var paths []string
		for p := range seen {
			paths = append(paths, p)
		}
		slices.Sort(paths)
		return paths
	}
	common := []string{
		"Stats.Coverage.FirstRecordedDay", "Stats.Agents[].Label", "Stats.Daily[].Date", "Stats.Groups.By", "Stats.Highlights.BusiestDay.Date",
		"Stats.HeadsUp[].ByAgent[].Label", "Stats.PeakSpend.Date",
		"Stats.MCP.Scope",
		"Stats.Prices.AsOf", "Stats.Prices.Currency", "Stats.Prices.Version",
		"Stats.Window.FirstDay", "Stats.Window.LastDay", "Stats.Window.Timezone",
	}
	named := []string{
		"Stats.DisplaySkills[].Name", "Stats.Groups.Rows[].Key", "Stats.HeadsUp[].Project",
		"Stats.Highlights.CostliestSession.Project", "Stats.Highlights.FavoriteModel.Label",
		"Stats.MCP.Servers[].DisplayName", "Stats.MCP.Servers[].Name", "Stats.Models[].Label", "Stats.Projects[].Name",
	}
	if got, want := reached(true), slices.Sorted(slices.Values(slices.Concat(common, named))); !slices.Equal(got, want) {
		t.Errorf("with names shown, the stats texts on the page are\n  %q\nwant\n  %q", got, want)
	}
	if got, want := reached(false), slices.Sorted(slices.Values(common)); !slices.Equal(got, want) {
		t.Errorf("with names hidden, the stats texts on the page are\n  %q\nwant\n  %q", got, want)
	}
}

// Hostile text is inert wherever it lands: every text field of the stats is
// set, in turn, to each payload, and the page it makes must still be
// well-formed, allow only the elements it uses, hold nothing that loads, and
// carry none of what the payload would do if it were markup or contained a
// character that rearranges text.
func TestHostileTextIsInertInEveryField(t *testing.T) {
	t.Parallel()
	base := computeFixture(t, fixtureSessions(), 30, stats.GroupProject)
	leaves := stringLeaves(&base)
	for p, payload := range hostilePayloads {
		if testing.Short() && len(payload) > 4096 {
			// Scale only repeated long strings; every attack, every field and
			// both privacy modes still render and pass the same safety checks.
			payload = payload[:4096]
		}
		t.Run(strconv.Itoa(p), func(t *testing.T) {
			t.Parallel()
			where := ""
			defer func() {
				if t.Failed() {
					t.Logf("while %q was in %s", payload[:min(len(payload), 40)], where)
				}
			}()
			for i, leaf := range leaves {
				where = leaf.path
				s := deepCopy(base)
				stringLeaves(&s)[i].value.SetString(payload)
				for _, reveal := range []bool{true, false} {
					out := render(t, s, Options{
						IncludeNames: reveal, Filters: Filters{Harness: payload, Model: payload, Origin: payload},
						EmptyMessage: payload,
					})
					page := string(out)
					for _, raw := range []string{
						"<script", "<img", "<iframe", "<meta http-equiv=\"refresh", "<a href", "<!--", "<![CDATA[",
						"</style><style>", "</title></head>", "\xe2\x80\xa8", "\xe2\x80\xa9", "\xe2\x80\xae", "\xe2\x80\xac",
						"\xe2\x81\xa6", "\xe2\x81\xa9", "\x1b", "\x00", "\x07", "\x7f",
					} {
						if strings.Contains(page, raw) {
							t.Fatalf("payload %d in %s (names shown %v): the page contains %q", p, leaf.path, reveal, raw)
						}
					}
					if len(page) > 400_000 {
						t.Fatalf("payload %d in %s: the page is %d bytes", p, leaf.path, len(page))
					}
				}
			}
		})
	}
}
