package cli

import (
	"github.com/wangjohn/agent-archive/internal/stats"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// A one-day window has no daily chart, and a window can be shorter than the
// one before it: w to it, or to a view with fewer sections, keeps the screen
// whole and the scroll position inside the new page.
func TestStatsScreenSurvivesSectionsDisappearingWithAOneDayWindow(t *testing.T) {
	t.Parallel()
	const width, height = 80, 12
	var sessions []archive.Metadata
	for _, s := range spread("claude", "claude-opus-5", statsNow, statsNow.Location(), 70, 3, 9, true) {
		sessions = append(sessions, s.build())
	}
	inputs := statsInputs{hasSessions: len(sessions) > 0, prepared: stats.Prepare(sessions, stats.PrepareOptions{Location: statsNow.Location()}), now: statsNow, location: statsNow.Location()}
	// The one-day window first, at its end, then 7 and 30 days; another view,
	// at its end, then 90 days and around to one day again.
	run := runScreen(t, screenOptions{width: width, height: height, days: 1, inputs: &inputs},
		"\x1b[F", "w", "w", "d", "\x1b[F", "w", "w", "w", "q")
	if len(run.frames) != 9 {
		t.Fatalf("%d frames", len(run.frames))
	}
	steps := []struct {
		page statsPage
		days int
		// end is whether the key before the frame scrolled to the bottom;
		// top is whether a view change put it at the top.
		end bool
		top bool
	}{
		{pageOverview, 1, false, false},
		{pageOverview, 1, true, false},
		{pageOverview, 7, false, false},
		{pageOverview, 30, false, false},
		{pageDetail, 30, false, true},
		{pageDetail, 30, true, false},
		{pageDetail, 90, false, false},
		{pageDetail, 1, false, false},
		{pageDetail, 7, false, false},
	}
	top := 0
	for i, step := range steps {
		lines := run.expectedLines(step.page, step.days, width)
		maxTop := max(len(lines)-(height-1), 0)
		switch {
		case step.end:
			top = maxTop
		case step.top:
			top = 0
		}
		top = min(top, maxTop)
		checkFrame(t, run.frames[i], lines, top, width, height, anyBar)
	}
	oneDay := strings.Join(run.expectedLines(pageOverview, 1, width), "\n")
	if strings.Contains(oneDay, "DAILY SPEND") {
		t.Errorf("a one-day window has a daily chart:\n%s", oneDay)
	}
	if !strings.Contains(oneDay, "sessions") {
		t.Errorf("the one-day window is empty; the test proves nothing:\n%s", oneDay)
	}
}
