package cli

import (
	"testing"
	"time"
)

// newYork loads a real zone with daylight saving, or skips without one.
func newYork(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no zone database: %v", err)
	}
	return loc
}

func TestAgentLabel(t *testing.T) {
	for _, tc := range []struct{ harness, name string }{
		{"claude", "Claude"},
		{"claude-code", "Claude"},
		{"Claude", "Claude"},
		{"codex", "Codex"},
		{"cursor", "Cursor"},
		{"gemini", "Gemini"},
		{"ölwechsel", "Ölwechsel"},
		{"two\nlines", "Two lines"},
		{"", "Unknown"},
	} {
		dot, name := agentLabel(tc.harness)
		if dot != agentDot || name != tc.name {
			t.Errorf("agentLabel(%q) = %q, %q; want %q, %q", tc.harness, dot, name, agentDot, tc.name)
		}
	}
}

func TestAgentColor(t *testing.T) {
	plain, color := textStyle{}, textStyle{color: true}
	if got := plain.agent("claude", agentDot); got != agentDot {
		t.Fatalf("plain style colored the dot: %q", got)
	}
	seen := map[string]string{}
	for _, h := range []string{"claude", "codex", "cursor", "gemini"} {
		got := color.agent(h, agentDot)
		if got == agentDot {
			t.Fatalf("%s dot has no color", h)
		}
		if other, ok := seen[got]; ok {
			t.Fatalf("%s and %s share a color", h, other)
		}
		seen[got] = h
	}
	if got, want := color.agent("claude-code", agentDot), color.agent("claude", agentDot); got != want {
		t.Fatalf("alias colored %q, want %q", got, want)
	}
	if got, want := color.agent("gemini", agentDot), color.dim(agentDot); got != want {
		t.Fatalf("unknown harness = %q, want dim %q", got, want)
	}
}

func TestShortWhen(t *testing.T) {
	ny := newYork(t)
	// Wednesday, Oct 14 2026, 15:30 in New York.
	now := time.Date(2026, 10, 14, 15, 30, 0, 0, ny)
	for _, tc := range []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero", time.Time{}, "-"},
		{"same instant", now, "now"},
		{"seconds ago", now.Add(-59 * time.Second), "now"},
		{"future", now.Add(3 * time.Hour), "now"},
		{"one minute", now.Add(-time.Minute), "1m"},
		{"minutes", now.Add(-59 * time.Minute), "59m"},
		{"one hour", now.Add(-time.Hour), "1h"},
		{"this morning", time.Date(2026, 10, 14, 0, 0, 0, 0, ny), "15h"},
		{"before midnight", time.Date(2026, 10, 13, 23, 59, 0, 0, ny), "Tue"},
		{"another zone, same local day", time.Date(2026, 10, 14, 2, 0, 0, 0, time.UTC), "Tue"},
		{"six days back", time.Date(2026, 10, 8, 0, 0, 0, 0, ny), "Thu"},
		{"seven days back", time.Date(2026, 10, 7, 23, 0, 0, 0, ny), "Oct 7"},
		{"start of year", time.Date(2026, 1, 1, 0, 0, 0, 0, ny), "Jan 1"},
		{"last year", time.Date(2025, 12, 31, 23, 59, 0, 0, ny), "2025-12-31"},
		{"long ago", time.Date(2025, 3, 1, 9, 0, 0, 0, ny), "2025-03-01"},
	} {
		if got := shortWhen(now, tc.t); got != tc.want {
			t.Errorf("%s: shortWhen = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestShortWhenMinutesCrossMidnight(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 10, 0, 0, time.UTC)
	if got := shortWhen(now, now.Add(-20*time.Minute)); got != "20m" {
		t.Fatalf("20 minutes before, across midnight = %q, want 20m", got)
	}
	if got := shortWhen(now, now.Add(-70*time.Minute)); got != "Tue" {
		t.Fatalf("70 minutes before, across midnight = %q, want Tue", got)
	}
}

func TestShortWhenDST(t *testing.T) {
	ny := newYork(t)
	// Clocks went back at 2:00 on Sunday Nov 1 2026, so that day has 25
	// hours: 23:30 is 24h30m after midnight and still the same day.
	now := time.Date(2026, 11, 1, 23, 30, 0, 0, ny)
	if got := shortWhen(now, time.Date(2026, 11, 1, 0, 0, 0, 0, ny)); got != "24h" {
		t.Fatalf("25-hour day = %q, want 24h", got)
	}
	if got := shortWhen(now, time.Date(2026, 10, 31, 23, 59, 0, 0, ny)); got != "Sat" {
		t.Fatalf("day before the fall-back day = %q, want Sat", got)
	}
	// Clocks went forward at 2:00 on Sunday Mar 8 2026: a 23-hour day.
	spring := time.Date(2026, 3, 9, 0, 30, 0, 0, ny)
	if got := shortWhen(spring, time.Date(2026, 3, 8, 0, 0, 0, 0, ny)); got != "Sun" {
		t.Fatalf("23-hour day = %q, want Sun", got)
	}
	if got := shortWhen(spring, time.Date(2026, 3, 3, 0, 0, 0, 0, ny)); got != "Tue" {
		t.Fatalf("six days back across spring forward = %q, want Tue", got)
	}
}

func TestLongWhen(t *testing.T) {
	now := time.Date(2026, 10, 14, 15, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		t    time.Time
		want string
	}{
		{time.Time{}, "at an unknown time"},
		{now.Add(time.Hour), "just now"},
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-time.Minute), "1 minute ago"},
		{now.Add(-2 * time.Minute), "2 minutes ago"},
		{now.Add(-time.Hour), "1 hour ago"},
		{now.Add(-47 * time.Hour), "47 hours ago"},
		{now.Add(-48 * time.Hour), "2 days ago"},
		{now.Add(-24 * 400 * time.Hour), "400 days ago"},
	} {
		if got := longWhen(now, tc.t); got != tc.want {
			t.Errorf("longWhen(%v) = %q, want %q", tc.t, got, tc.want)
		}
		if got := relativeAge(now, tc.t); got != tc.want {
			t.Errorf("relativeAge(%v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestCompactAge(t *testing.T) {
	now := time.Date(2026, 10, 14, 15, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		t    time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-2 * time.Hour), "2h ago"},
		{time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), "Mon"},
		{time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC), "Sep 1"},
		{time.Date(2024, 9, 1, 9, 0, 0, 0, time.UTC), "2024-09-01"},
	} {
		if got := compactAge(now, tc.t); got != tc.want {
			t.Errorf("compactAge(%v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestDayBucket(t *testing.T) {
	ny := newYork(t)
	at := func(m time.Month, d, h, min int) time.Time { return time.Date(2026, m, d, h, min, 0, 0, ny) }
	// Wednesday Oct 14 2026: this week began Monday Oct 12, last week ran
	// Oct 5–11.
	wed := at(10, 14, 15, 30)
	// Monday Nov 2 2026, the day after clocks fell back: this week is only
	// today, last week ran Oct 26–Nov 1 across the month change.
	mon := at(11, 2, 9, 0)
	// Tuesday Sep 1 2026: yesterday is in August.
	tue := at(9, 1, 9, 0)
	for _, tc := range []struct {
		name   string
		now, t time.Time
		want   dayBucketKind
	}{
		{"now", wed, wed, bucketToday},
		{"midnight today", wed, at(10, 14, 0, 0), bucketToday},
		{"future", wed, wed.Add(48 * time.Hour), bucketToday},
		{"before midnight", wed, at(10, 13, 23, 59), bucketYesterday},
		{"other zone", wed, time.Date(2026, 10, 14, 3, 59, 0, 0, time.UTC), bucketYesterday},
		{"monday", wed, at(10, 12, 0, 0), bucketEarlierThisWeek},
		{"sunday", wed, at(10, 11, 23, 59), bucketLastWeek},
		{"last monday", wed, at(10, 5, 0, 0), bucketLastWeek},
		{"before last week", wed, at(10, 4, 23, 59), bucketEarlierThisMonth},
		{"first of month", wed, at(10, 1, 0, 0), bucketEarlierThisMonth},
		{"last month", wed, at(9, 30, 23, 59), bucketOlder},
		{"zero", wed, time.Time{}, bucketOlder},
		{"monday yesterday", mon, at(11, 1, 23, 0), bucketYesterday},
		{"monday dst day", mon, at(11, 1, 0, 30), bucketYesterday},
		{"monday saturday", mon, at(10, 31, 12, 0), bucketLastWeek},
		{"monday last monday", mon, at(10, 26, 0, 0), bucketLastWeek},
		{"monday before last week", mon, at(10, 25, 23, 0), bucketOlder},
		{"month change yesterday", tue, at(8, 31, 23, 0), bucketYesterday},
		{"month change this week", tue, at(8, 31, 0, 0), bucketYesterday},
		{"month change last week", tue, at(8, 30, 12, 0), bucketLastWeek},
		{"month change before", tue, at(8, 23, 12, 0), bucketOlder},
		{"year change", time.Date(2027, 1, 1, 9, 0, 0, 0, ny), at(12, 31, 22, 0), bucketYesterday},
		{"year change last week", time.Date(2027, 1, 4, 9, 0, 0, 0, ny), at(12, 28, 9, 0), bucketLastWeek},
	} {
		if got := dayBucket(tc.now, tc.t); got != tc.want {
			t.Errorf("%s: dayBucket = %s, want %s", tc.name, got.label(), tc.want.label())
		}
	}
}

func TestDayBucketLabels(t *testing.T) {
	want := []string{"TODAY", "YESTERDAY", "EARLIER THIS WEEK", "LAST WEEK", "EARLIER THIS MONTH", "OLDER"}
	for i, label := range want {
		if got := dayBucketKind(i).label(); got != label {
			t.Errorf("bucket %d label = %q, want %q", i, got, label)
		}
	}
}
