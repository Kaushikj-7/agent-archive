package statsfmt

import (
	"math"
	"strings"
	"testing"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
)

// One table pins what the terminal screen and the web page both print.
func TestNumberFormats(t *testing.T) {
	t.Parallel()
	for n, want := range map[int64]string{
		0: "0", 812: "812", 999: "999", 1000: "1K", 1234: "1.2K", 9949: "9.9K", 9950: "10K", 51_400: "51K",
		999_499: "999K", 999_500: "1M", 999_999: "1M", 1_000_000: "1M", 4_900_000: "4.9M", 61_000_000: "61M",
		999_600_000: "1B", 1_200_000_000: "1.2B", 3_000_000_000_000: "3T", 999_500_000_000_000: ">999T",
		// A saturated sum is a bound, not a measurement.
		math.MaxInt64: ">999T",
	} {
		if got := TokenCount(n); got != want {
			t.Errorf("TokenCount(%d) = %q, want %q", n, got, want)
		}
	}
	for _, tc := range []struct {
		amount  float64
		precise bool
		want    string
	}{
		{0, false, "$0.00"}, {0.126, false, "$0.13"}, {5.69, false, "$5.69"}, {9.999, false, "$10.00"}, {59.4, false, "$59"},
		{612.5, false, "$613"}, {41.2, true, "$41.20"}, {1204.5, false, "$1,205"}, {1204.5, true, "$1,204.50"},
		{1e13, false, "$>999B"}, {math.MaxFloat64, true, "$>999B"},
	} {
		if got := Money("USD", tc.amount, tc.precise); got != tc.want {
			t.Errorf("Money(%v, precise=%v) = %q, want %q", tc.amount, tc.precise, got, tc.want)
		}
	}
	if got := Money("EUR", 12.4, false); got != "EUR 12" {
		t.Errorf("Money in EUR = %q", got)
	}
	for share, want := range map[float64]string{0: "0%", 0.004: "<1%", 0.005: "1%", 0.184: "18%", 0.995: "100%", 1: "100%"} {
		if got := Percent(share); got != want {
			t.Errorf("Percent(%v) = %q, want %q", share, got, want)
		}
	}
	for rate, want := range map[float64]string{0: "0%", 0.041: "4.1%", 0.0995: "10.0%", 0.25: "25%"} {
		if got := RatePercent(rate); got != want {
			t.Errorf("RatePercent(%v) = %q, want %q", rate, got, want)
		}
	}
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 3204: "3,204", 1234567: "1,234,567", -5000: "-5,000"} {
		if got := CommaInt(n); got != want {
			t.Errorf("CommaInt(%d) = %q, want %q", n, got, want)
		}
	}
	for n, want := range map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 11: "11th", 12: "12th", 13: "13th", 21: "21st", 22: "22nd"} {
		if got := Ordinal(n); got != want {
			t.Errorf("Ordinal(%d) = %q, want %q", n, got, want)
		}
	}
}

// A change is a direction and a whole-percent size, which reads ">999" once it
// is past 999: a rise from next to nothing is not measured to the digit.
func TestChangeShowsAtMost999Percent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pct  float64
		dir  int
		size string
	}{
		{18.4, 1, "18"}, {18.5, 1, "19"}, {0.4, 0, ""}, {-0.4, 0, ""}, {-40, -1, "40"}, {-100, -1, "100"},
		{999, 1, "999"}, {999.4, 1, "999"}, {999.6, 1, ">999"}, {1000, 1, ">999"}, {25_219_191, 1, ">999"},
		{1e300, 1, ">999"}, {math.MaxFloat64, 1, ">999"}, {-1e300, -1, ">999"},
		{math.NaN(), 0, ""}, {math.Inf(1), 0, ""}, {math.Inf(-1), 0, ""},
	} {
		if dir, size := Change(tc.pct); dir != tc.dir || size != tc.size {
			t.Errorf("Change(%v) = %d, %q; want %d, %q", tc.pct, dir, size, tc.dir, tc.size)
		}
	}
}

// RoundInt is the same on every platform, whatever the float.
func TestRoundIntSaturatesOnEveryPlatform(t *testing.T) {
	t.Parallel()
	for v, want := range map[float64]int64{
		0: 0, 1.4: 1, 1.5: 2, -1.5: -2, 9e18: 9_000_000_000_000_000_000, math.MaxInt64: math.MaxInt64, 1e19: math.MaxInt64,
		math.Inf(1): math.MaxInt64, math.MaxFloat64: math.MaxInt64, math.MinInt64: math.MinInt64, -1e19: math.MinInt64,
		math.Inf(-1): math.MinInt64, math.NaN(): 0,
	} {
		if got := RoundInt(v); got != want {
			t.Errorf("RoundInt(%v) = %d, want %d", v, got, want)
		}
	}
}

// A number that is not a number is never shown as one.
func TestNonFiniteNumbersReadAsUnavailable(t *testing.T) {
	t.Parallel()
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := Percent(v); got != "n/a" {
			t.Errorf("Percent(%v) = %q", v, got)
		}
		if got := RatePercent(v); got != "n/a" {
			t.Errorf("RatePercent(%v) = %q", v, got)
		}
	}
	if got := Money("USD", math.NaN(), false); got != "n/a" {
		t.Errorf("Money(NaN) = %q", got)
	}
}

// A currency code read from a price file is shown cleaned and short.
func TestMoneyCleansAndShortensTheCurrency(t *testing.T) {
	t.Parallel()
	if got := Money("E\u202eU\nR", 12.4, false); strings.ContainsAny(got, "\u202e\n") {
		t.Errorf("Money kept a control character: %q", got)
	}
	long := strings.Repeat("X", 100)
	if got := Money(long, 1, false); len([]rune(got)) > currencyLimit+len(" 1.00") {
		t.Errorf("Money kept a %d-character currency", len([]rune(got)))
	}
}

// The formatters are pure: they import only the archive's cleaner, which
// transitively reaches pure agent identity metadata.
func TestFormattersImportBoundary(t *testing.T) {
	t.Parallel()
	direct, all := importgraph.Imports(t, "github.com/wangjohn/agent-archive/internal/statsfmt")
	importgraph.Forbid(t, "internal/statsfmt", direct,
		"os", "os/exec", "io/fs", "path/filepath", "net", "net/http", "math/rand", "math/rand/v2", "time", "golang.org/x/term",
		"github.com/wangjohn/agent-archive/internal/agentmeta", "github.com/wangjohn/agent-archive/internal/jsonwire", "github.com/wangjohn/agent-archive/internal/codexmeta")
	importgraph.Forbid(t, "internal/statsfmt (transitively)", all, "net/http", "os/exec")
	for _, path := range all {
		if strings.HasPrefix(path, "github.com/wangjohn/agent-archive/internal/") &&
			path != "github.com/wangjohn/agent-archive/internal/archive" &&
			path != "github.com/wangjohn/agent-archive/internal/agentmeta" &&
			path != "github.com/wangjohn/agent-archive/internal/codexmeta" {
			t.Errorf("internal/statsfmt reaches %s; only internal/archive and its pure agentmeta/codexmeta dependencies are allowed", path)
		}
	}
}
