package statshtml

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/stats"
)

// Keep the previous assertion extractors as independent oracles. This pins
// malformed input handling as well as all attributes used by real rendered pages.
func TestGeometryScannerMatchesPreviousExpressions(t *testing.T) {
	numbers := regexp.MustCompile(`\s(x|y|x1|x2|y1|y2|cx|cy|r|rx|width|height|stroke-width|stroke-dashoffset)="([^"]*)"`)
	dashes := regexp.MustCompile(`stroke-dasharray="([^"]*)"`)
	check := func(label, page string) {
		t.Helper()
		var got, want [][2]string
		geometryNumbers(page, func(name, value string) { got = append(got, [2]string{name, value}) })
		for _, match := range numbers.FindAllStringSubmatch(page, -1) {
			want = append(want, [2]string{match[1], match[2]})
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s numeric attributes: got %q want %q in %q", label, got, want, page)
		}
		var gotDash, wantDash []string
		geometryDashArrays(page, func(value string) { gotDash = append(gotDash, value) })
		for _, match := range dashes.FindAllStringSubmatch(page, -1) {
			wantDash = append(wantDash, match[1])
		}
		if !reflect.DeepEqual(gotDash, wantDash) {
			t.Fatalf("%s dash arrays: got %q want %q", label, gotDash, wantDash)
		}
	}
	for _, names := range []bool{false, true} {
		page, err := Render(computeFixture(t, fixtureSessions(), 30, stats.GroupProject), Options{IncludeNames: names})
		if err != nil {
			t.Fatal(err)
		}
		check(fmt.Sprint("rendered names=", names), string(page))
	}
	for _, name := range []string{"x", "y", "x1", "x2", "y1", "y2", "cx", "cy", "r", "rx", "width", "height", "stroke-width", "stroke-dashoffset"} {
		for _, prefix := range []string{" ", "\t", "\n", "\r", "\f", "\v", "\u00a0", "", " data-"} {
			check("attribute boundary", prefix+name+`="-1"`)
		}
	}
	atoms := []string{" ", "\t", "\n", "\r", "\f", "\v", "\u00a0", "x", "y", "x1", "x2", "y1", "y2", "cx", "cy", "r", "rx", "width", "height", "stroke-width", "stroke-dashoffset", "stroke-dasharray", "data-x", "=\"", "'", "\"", "0", "-1", "NaN", "Inf", "101%", "", "<svg", ">", "1 2 -3"}
	rng := rand.New(rand.NewPCG(0x150, 2))
	for seed := range 1000 {
		var page strings.Builder
		for range 30 {
			page.WriteString(atoms[rng.IntN(len(atoms))])
		}
		check(fmt.Sprint("seed ", seed), page.String())
	}
	for _, page := range []string{`x="1" y="2"`, ` x="unterminated`, ` x=" y="3" width="4"`, ` width="" height="-1"`, "\tstroke-dasharray=\"1 2 -3\"\nrx=\"101%\""} {
		check("malformed", page)
	}
}
