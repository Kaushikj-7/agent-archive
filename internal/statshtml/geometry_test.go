package statshtml

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
)

// numericLeaves are the addresses of every number in s, at any depth, in a
// stable order: the fields a mutation test can spoil one at a time.
func numericLeaves(s *stats.Stats) []reflect.Value {
	var out []reflect.Value
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		kind := v.Kind()
		if kind == reflect.Struct {
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		}
		if kind == reflect.Pointer && !v.IsNil() {
			walk(v.Elem())
		}
		if kind == reflect.Slice {
			for i := range v.Len() {
				walk(v.Index(i))
			}
		}
		if (kind == reflect.Float64 || kind == reflect.Int || kind == reflect.Int64) && v.CanSet() {
			out = append(out, v)
		}
	}
	walk(reflect.ValueOf(s).Elem())
	return out
}

// deepCopy is a copy of s that shares no pointer or slice with it.
func deepCopy(s stats.Stats) stats.Stats {
	var clone func(v reflect.Value) reflect.Value
	clone = func(v reflect.Value) reflect.Value {
		kind := v.Kind()
		// time.Time has unexported fields: copy it whole.
		if kind == reflect.Struct && v.Type().PkgPath() != "time" {
			out := reflect.New(v.Type()).Elem()
			for i := range v.NumField() {
				if v.Type().Field(i).IsExported() {
					out.Field(i).Set(clone(v.Field(i)))
				}
			}
			return out
		}
		if kind == reflect.Pointer && !v.IsNil() {
			out := reflect.New(v.Type().Elem())
			out.Elem().Set(clone(v.Elem()))
			return out
		}
		if kind == reflect.Slice && !v.IsNil() {
			out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
			for i := range v.Len() {
				out.Index(i).Set(clone(v.Index(i)))
			}
			return out
		}
		return v
	}
	return clone(reflect.ValueOf(s)).Interface().(stats.Stats)
}

// sizes are the attributes that are lengths, which are never negative.
var sizes = map[string]bool{"width": true, "height": true, "r": true, "rx": true, "stroke-width": true}

// geometryNumbers visits the same quoted attributes as the original regular
// expressions. Indexing the quote delimiters avoids running the regexp VM over
// every byte of the full self-contained page for every poisoned numeric leaf.
// The HTML remains untrusted input: this only extracts assertion candidates.
func geometryNumbers(page string, visit func(name, value string)) {
	space := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }
	for offset := 0; offset < len(page); {
		rel := strings.Index(page[offset:], `="`)
		if rel < 0 {
			break
		}
		eq := offset + rel
		offset = eq + 2
		start := eq
		for start > 0 && eq-start <= len("stroke-dashoffset") && !space(page[start-1]) {
			start--
		}
		if start == 0 || !space(page[start-1]) {
			continue
		}
		name := page[start:eq]
		switch name {
		case "x", "y", "x1", "x2", "y1", "y2", "cx", "cy", "r", "rx", "width", "height", "stroke-width", "stroke-dashoffset":
		default:
			continue
		}
		end := strings.IndexByte(page[offset:], '"')
		if end < 0 {
			break
		}
		visit(name, page[offset:offset+end])
		// Like FindAllStringSubmatch, consume non-overlapping matches.
		offset += end + 1
	}
}

func geometryDashArrays(page string, visit func(value string)) {
	const prefix = `stroke-dasharray="`
	for offset := 0; offset < len(page); {
		rel := strings.Index(page[offset:], prefix)
		if rel < 0 {
			break
		}
		start := offset + rel + len(prefix)
		end := strings.IndexByte(page[start:], '"')
		if end < 0 {
			break
		}
		visit(page[start : start+end])
		offset = start + end + 1
	}
}

// assertSaneGeometry fails when the page has a length or coordinate that is
// not a finite number, a negative size, or a percentage outside its bounds,
// or prints NaN or Inf anywhere.
func assertSaneGeometry(t *testing.T, page, what string) {
	t.Helper()
	for _, bad := range []string{"NaN", "Inf", "e+", "E+"} {
		if strings.Contains(page, bad) {
			t.Errorf("%s: the page contains %q", what, bad)
		}
	}
	geometryNumbers(page, func(name, raw string) {
		value := strings.TrimSuffix(raw, "%")
		if value == "" {
			return
		}
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			t.Errorf("%s: %s=%q is not a number", what, name, raw)
			return
		}
		if sizes[name] && n < 0 {
			t.Errorf("%s: %s=%q is negative", what, name, raw)
		}
		if strings.HasSuffix(raw, "%") && (n < 0 || n > 100) {
			t.Errorf("%s: %s=%q is outside 0 to 100 percent", what, name, raw)
		}
	})
	geometryDashArrays(page, func(raw string) {
		for f := range strings.FieldsSeq(raw) {
			if n, err := strconv.ParseFloat(f, 64); err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
				t.Errorf("%s: stroke-dasharray=%q", what, raw)
			}
		}
	})
}

// Whatever number the stats hold, however wrong (NaN, infinite, negative, a
// share over 100%, the largest integer), the page draws nothing that is not a
// number and no length that is negative: each numeric field of the stats is
// spoiled in turn, and the page checked.
func TestSpoiledNumbersNeverBreakTheGeometry(t *testing.T) {
	t.Parallel()
	base := computeFixture(t, fixtureSessions(), 30, stats.GroupProject)
	leaves := numericLeaves(&base)
	if len(leaves) < 100 {
		t.Fatalf("only %d numbers found in the stats; the walk is broken", len(leaves))
	}
	floats := []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 2, 1e300, 0}
	ints := []int64{-1, math.MaxInt64, math.MinInt64, 0}
	for i := range leaves {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			s := deepCopy(base)
			leaf := numericLeaves(&s)[i]
			var values []any
			if leaf.Kind() == reflect.Float64 {
				for _, f := range floats {
					values = append(values, f)
				}
			} else {
				for _, n := range ints {
					values = append(values, n)
				}
			}
			for _, v := range values {
				s2 := deepCopy(base)
				target := numericLeaves(&s2)[i]
				if target.Kind() == reflect.Float64 {
					target.SetFloat(v.(float64))
				} else {
					target.SetInt(v.(int64))
				}
				out, err := Render(s2, Options{IncludeNames: true})
				if err != nil {
					t.Fatalf("leaf %d = %v: %v", i, v, err)
				}
				assertSaneGeometry(t, string(out), fmt.Sprintf("leaf %d (%s) = %v", i, leaf.Type(), v))
			}
		})
	}
}

// A donut made of one type is a whole ring; two types where one has nearly
// all of it, or a slice too thin to see, still add up to the ring and leave
// no gap or overlap that shows.
func TestDonutEdgeShares(t *testing.T) {
	t.Parallel()
	circ := 2 * math.Pi * donutRadius
	for _, shares := range [][]float64{
		{1, 0, 0, 0}, {0, 0, 0, 1}, {0.9995, 0.0005, 0, 0}, {0.999, 0.001, 0, 0}, {0.5, 0.5, 0, 0},
		{0.25, 0.25, 0.25, 0.25}, {0.97, 0.01, 0.01, 0.01}, {0.4, 0.3, 0.2999, 0.0001}, {0, 0, 0, 0},
	} {
		start := 0.0
		var drawn float64
		for _, share := range shares {
			g := arcGeometry(share, start, circ)
			if share <= 0 {
				if g.dash != "" {
					t.Errorf("shares %v: a segment with no share is drawn (%q)", shares, g.dash)
				}
				continue
			}
			var visible, gap float64
			if _, err := fmt.Sscanf(g.dash, "%g %g", &visible, &gap); err != nil {
				t.Errorf("shares %v: dash %q: %v", shares, g.dash, err)
				continue
			}
			if visible <= 0 || gap < 0 || math.Abs(visible+gap-circ) > 0.02 {
				t.Errorf("shares %v: dash %q does not add up to the circumference %.2f", shares, g.dash, circ)
			}
			drawn += visible
			start += share * circ
		}
		if total := sumOf(shares); total > 0.999 && drawn < circ*total-donutGap*float64(len(shares))-0.1 {
			t.Errorf("shares %v: only %.1f of %.1f is drawn", shares, drawn, circ*total)
		}
	}
}

func sumOf(v []float64) float64 {
	var t float64
	for _, x := range v {
		t += x
	}
	return t
}

// Every bar of the daily chart lies inside the plot and the chart's width:
// a window of any length, days without sessions, days without token counts,
// a peak at either edge, and a chart of one bar.
func TestDailyBarsStayInsideTheChart(t *testing.T) {
	t.Parallel()
	for _, days := range []int{1, 2, 7, 30, 31, 119, 120, 121, 240, 365, 1000, stats.MaxDays} {
		// Each shape is what day i's tokens are, and how many days have sessions.
		shapes := map[string]struct {
			tokens func(i int, base tokenSpec) []tokenSpec
			days   int
		}{
			"peak-first": {func(i int, b tokenSpec) []tokenSpec { b.input = 5_000_000 - i*100_000; return []tokenSpec{b} }, 20},
			"peak-last":  {func(i int, b tokenSpec) []tokenSpec { b.input = 100_000 + i*100_000; return []tokenSpec{b} }, 20},
			"flat":       {func(i int, b tokenSpec) []tokenSpec { b.input = 1; return []tokenSpec{b} }, 20},
			"one-day":    {func(i int, b tokenSpec) []tokenSpec { return []tokenSpec{b} }, 1},
			"unknown-days": {func(i int, b tokenSpec) []tokenSpec {
				if i%2 == 0 {
					return nil
				}
				return []tokenSpec{b}
			}, 20},
		}
		for shape, spec := range shapes {
			t.Run(fmt.Sprintf("%d-%s", days, shape), func(t *testing.T) {
				t.Parallel()
				var sessions []archive.Metadata
				for i := range min(days, spec.days) {
					at := fixtureNow.AddDate(0, 0, -i*max(days/20, 1))
					tokens := spec.tokens(i, tokenSpec{"claude-opus-5", 10_000 * (i + 1), 5_000, 100_000, 10_000})
					sessions = append(sessions, sessionSpec{
						id: fmt.Sprintf("s%d", i), harness: "claude", project: "p", captured: at,
						models: []string{"claude-opus-5"}, turns: 3, tokens: tokens,
					}.build())
				}
				s := computeFixture(t, sessions, days, stats.GroupNone)
				page := string(render(t, s, Options{}))
				assertSaneGeometry(t, page, "chart")
				bars := regexp.MustCompile(`<rect class="bar ([a-z ]+)" x="([0-9.]+)%" width="([0-9.]+)%" y="([0-9.]+)" height="([0-9.]+)"`).FindAllStringSubmatch(page, -1)
				if s.Coverage.SessionsWithTokens > 0 && len(bars) == 0 {
					t.Fatal("no bars")
				}
				if len(bars) > maxBars {
					t.Errorf("%d bars, over %d", len(bars), maxBars)
				}
				for _, m := range bars {
					x, _ := strconv.ParseFloat(m[2], 64)
					w, _ := strconv.ParseFloat(m[3], 64)
					y, _ := strconv.ParseFloat(m[4], 64)
					h, _ := strconv.ParseFloat(m[5], 64)
					if x < 0 || x+w > 100.001 {
						t.Errorf("bar x=%.2f w=%.2f leaves the chart", x, w)
					}
					if y < chartPlotTop-0.01 || y+h > chartBaseline+0.01 {
						t.Errorf("bar y=%.2f h=%.2f leaves the plot (%d to %d)", y, h, chartPlotTop, chartBaseline)
					}
				}
				// The peak's label, wherever it is, is inside the chart.
				if m := regexp.MustCompile(`class="peak-label" x="([0-9.]+)%" y="([0-9.]+)" text-anchor="([a-z]+)"`).FindStringSubmatch(page); m != nil {
					x, _ := strconv.ParseFloat(m[1], 64)
					if x < 0 || x > 100 {
						t.Errorf("the peak label is at %s%%", m[1])
					}
					if runsOff := map[string]bool{"start": x > 25, "end": x < 75}; runsOff[m[3]] {
						t.Errorf("the peak label is anchored %s at %.1f%%, so it would run off the chart", m[3], x)
					}
				}
			})
		}
	}
}
