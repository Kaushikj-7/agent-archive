package cli

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// This file holds the pure pieces a session line is built from: the agent
// label, the short and long times, and the day group. Nothing here does
// I/O, so every list, picker and summary formats a session the same way.

// agentDot is the mark before an agent's name, colored by the agent.
const agentDot = "●"

// agentLabel is the dot and short name that stand for a harness in a
// session line: "Claude", "Codex" or "Cursor" for the agents
// agent-archive captures, aliases included. Any other harness keeps its
// own name, made safe for one line with its first letter upper-cased, and
// an empty one is "Unknown". Color it with textStyle.agent.
//
//lint:ignore unparam every agent shares one dot today; the session list rows take it from here so a later mark changes in one place.
func agentLabel(harness string) (dot, name string) {
	switch knownAgent(harness) {
	case agentmeta.Claude:
		return agentDot, "Claude"
	case agentmeta.Codex:
		return agentDot, "Codex"
	case agentmeta.Cursor:
		return agentDot, "Cursor"
	case agentmeta.Muse:
		// Runtime-only identities retain the ordinary safe display fallback.
	}
	name = archive.DisplayLine(harness)
	if name == "" {
		return agentDot, "Unknown"
	}
	r, size := utf8.DecodeRuneInString(name)
	return agentDot, string(unicode.ToUpper(r)) + name[size:]
}

// knownAgent is the built-in agent harness names, or "" when it names none.
func knownAgent(harness string) agentmeta.ID {
	id, known := archive.KnownHarness(strings.ToLower(strings.TrimSpace(harness)))
	if !known {
		return ""
	}
	return agentmeta.ID(id)
}

// agent paints text, usually an agentLabel dot, in harness's color: one
// color each for Claude, Codex and Cursor, and dim for any other harness.
// Without color it returns text as it is.
func (s textStyle) agent(harness, text string) string {
	switch knownAgent(harness) {
	case agentmeta.Claude:
		return s.paint("38;5;208", text) // orange
	case agentmeta.Codex:
		return s.paint("38;5;39", text) // blue
	case agentmeta.Cursor:
		return s.paint("38;5;141", text) // violet
	case agentmeta.Muse:
		return s.dim(text)
	}
	return s.dim(text)
}

// shortWhen is when t was, relative to now, for a list's WHEN column: at
// most six characters ("Oct 10") within now's year, and ten for an older
// date ("2025-03-01"). Days are local days in now's location.
//
//   - The zero time is "-".
//   - Under a minute ago, or in the future (clock skew), is "now".
//   - Under an hour ago is minutes: "2m".
//   - Earlier the same local day is hours: "13h".
//   - Up to 6 local days back is the weekday: "Mon". A week back would
//     repeat today's weekday, so it is a date instead.
//   - Earlier in now's year is the month and day: "Oct 9".
//   - Anything older is the date: "2025-03-01".
func shortWhen(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	loc := now.Location()
	local := t.In(loc)
	days := localDaysBetween(local, now)
	switch {
	case days == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case days <= 6:
		return local.Format("Mon")
	case local.Year() == now.Year():
		return local.Format("Jan 2")
	default:
		return local.Format("2006-01-02")
	}
}

// longWhen is how long before now t was, as a phrase: "just now",
// "2 minutes ago", "5 hours ago" up to 48 hours, then "3 days ago". A time
// in the future is "just now", and the zero time is "at an unknown time".
func longWhen(now, t time.Time) string {
	if t.IsZero() {
		return "at an unknown time"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	default:
		return plural(int(d/(24*time.Hour)), "day") + " ago"
	}
}

// dayBucketKind is a day group of a session list, newest first.
type dayBucketKind int

const (
	bucketToday dayBucketKind = iota
	bucketYesterday
	bucketEarlierThisWeek
	bucketLastWeek
	bucketEarlierThisMonth
	bucketOlder
)

// dayBucket is the day group t falls in, as seen at now, by local days in
// now's location. Weeks start on Monday.
//
//   - Today: now's local day, and any time after now (clock skew).
//   - Yesterday: the local day before.
//   - Earlier this week: earlier in now's week, from Monday.
//   - Last week: the Monday-to-Sunday week before now's.
//   - Earlier this month: earlier in now's month.
//   - Older: anything before, the zero time included.
//
// The first group that fits wins, so on a Monday yesterday is Yesterday,
// not Last week, and last week can reach into the month before.
func dayBucket(now, t time.Time) dayBucketKind {
	loc := now.Location()
	local := t.In(loc)
	days := localDaysBetween(local, now)
	sinceMonday := (int(now.Weekday()) + 6) % 7
	switch {
	case days <= 0:
		return bucketToday
	case days == 1:
		return bucketYesterday
	case days <= sinceMonday:
		return bucketEarlierThisWeek
	case days <= sinceMonday+7:
		return bucketLastWeek
	case local.Year() == now.Year() && local.Month() == now.Month():
		return bucketEarlierThisMonth
	default:
		return bucketOlder
	}
}

// label is the bucket's heading in a session list, in capitals: "TODAY".
func (b dayBucketKind) label() string {
	switch b {
	case bucketToday:
		return "TODAY"
	case bucketYesterday:
		return "YESTERDAY"
	case bucketEarlierThisWeek:
		return "EARLIER THIS WEEK"
	case bucketLastWeek:
		return "LAST WEEK"
	case bucketEarlierThisMonth:
		return "EARLIER THIS MONTH"
	case bucketOlder:
	}
	// Older, and any kind past the last, is the oldest group.
	return "OLDER"
}

// localDaysBetween is how many calendar days from's date is before to's,
// each read in its own location: 0 on the same day, 1 for the day before,
// negative when from is a later day. Counting dates rather than hours keeps
// a 23- or 25-hour daylight-saving day one day long.
func localDaysBetween(from, to time.Time) int {
	return civilDay(to) - civilDay(from)
}

// civilDay numbers t's calendar date in its own location, one per day.
func civilDay(t time.Time) int {
	y, m, d := t.Date()
	return int(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}
