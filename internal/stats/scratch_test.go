package stats

import (
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestScratchActivityAndUsageMatchLegacyAccounting(t *testing.T) {
	orphan := meta("orphan", "codex", prepareNow, modelTokens("unknown-orphan", 7, 3, 0, 0))
	orphan.NativeChild = true
	sessions := []archive.Metadata{
		meta("root", "claude", prepareNow, modelTokens("claude-opus-5-5", 100, 50, 0, 0), modelTokens("unknown-a", 8, 2, 0, 0), skill(" first "), mcp(" first ", 2)),
		meta("child", "claude", prepareNow, parentOf("root"), modelTokens("unknown-child", 3, 1, 0, 0), skill("child"), mcp("child", 5)),
		meta("second", "codex", prepareNow, modelTokens("unknown-b", 9, 3, 1, 0)),
		meta("empty", "cursor", prepareNow),
		meta("approximate", "claude", prepareNow, tokens(12, 4, 0, 0)),
		orphan,
	}
	for range 2 {
		p := Prepare(sessions, PrepareOptions{Location: newYork})
		for _, days := range []int{1, 7, 30} {
			opts := Options{Now: prepareNow, Days: days, Location: newYork, AllRows: true, By: GroupProject}
			if got, want := statsJSON(t, p.Compute(opts)), statsJSON(t, legacyCompute(sessions, opts)); got != want {
				t.Fatalf("scratch changed usage, activity or orphan accounting for %d days", days)
			}
		}
		slices.Reverse(sessions)
	}
}

func TestMemberUsageReusesScratchAllocation(t *testing.T) {
	m := meta("root", "claude", prepareNow, modelTokens("unknown-a", 10, 5, 0, 0), modelTokens("unknown-b", 20, 10, 0, 0))
	scratch := map[string]tokenSet{}
	normalize := func(id string) string { return id }
	// Warm the map, just as later members reuse the first member's capacity.
	memberUsage(&m, normalize, scratch)
	got := testing.AllocsPerRun(100, func() { memberUsage(&m, normalize, scratch) })
	baseline := testing.AllocsPerRun(100, func() { legacyMemberUsage(&m) })
	if got != 0 || baseline < 1 {
		t.Fatalf("member scratch allocs=%g, original accounting allocs=%g; want zero scratch allocations and an allocating baseline", got, baseline)
	}
}
