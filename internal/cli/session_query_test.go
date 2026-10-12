package cli

import (
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func fieldsOfTest() sessionFields {
	return sessionFields{
		Name: "Implement Linux support", Title: "In the agent-archive repo, add a systemd unit",
		Branch: "feature/linux-port", Project: "agent-archive", Harness: "claude",
		SessionID: "d7a77938aaaaaaaaaaaaaaaaaaaaaaaa", PRs: []int{212, 7},
	}
}

// Every word must appear in some field, in any case, and words may match
// different fields.
func TestSessionQueryRequiresEveryWordInSomeField(t *testing.T) {
	t.Parallel()
	r := fieldsOfTest()
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"linux", true},
		{"LINUX Support", true},
		{"linux systemd", true},        // name and title
		{"linux port", true},           // name and branch
		{"agent-archive claude", true}, // project and harness
		{"d7a779", true},               // a short ID prefix
		{"d7a77938aaaaaaaaaaaaaaaaaaaaaaaa", true},
		{"linux 212", true},      // name and a PR
		{"linux windows", false}, // one word nothing has
		{"windows", false},
		{"a77938", false}, // an ID is matched from its start
		{"   ", false},
		{"", false},
	} {
		if got := parseSessionQuery(tc.query).matches(r); got != tc.want {
			t.Errorf("%q matches = %v, want %v", tc.query, got, tc.want)
		}
	}
}

// `#N` and a bare number of 1 to 6 digits match a PR number exactly; `#N` as
// text matches only the whole reference.
func TestSessionQueryNumbersMatchPullRequests(t *testing.T) {
	t.Parallel()
	r := fieldsOfTest()
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"#212", true},
		{"212", true},
		{"#7", true},
		{"7", true},
		{"#21", false}, // not a PR of the session, and not a prefix of one
		{"21", false},
		{"#2121", false},
		{"1234567", false}, // seven digits is text
		{"#0", false},
		{"linux #212", true},
		{"linux #213", false},
	} {
		if got := parseSessionQuery(tc.query).matches(r); got != tc.want {
			t.Errorf("%q matches = %v, want %v", tc.query, got, tc.want)
		}
	}
	// A PR written in the name or title is found by its reference too, and
	// the reference does not match a longer number.
	review := sessionFields{Title: "Review and fix PR #208 (5b-1b)"}
	if !parseSessionQuery("#208").matches(review) || !parseSessionQuery("208").matches(review) {
		t.Error("a reference in the title is not found")
	}
	if parseSessionQuery("#20").matches(review) {
		t.Error("#20 found #208")
	}
	if parseSessionQuery("#208").matches(sessionFields{Title: "Review PR #2081"}) {
		t.Error("#208 found #2081")
	}
}

// Words are parsed once: numbers are PRs, other words are not.
func TestParseSessionQuery(t *testing.T) {
	t.Parallel()
	q := parseSessionQuery("  Linux #212 7 1234567 #x ")
	if want := []string{"linux", "#212", "7", "1234567", "#x"}; !slices.Equal(q.words, want) {
		t.Fatalf("words %q, want %q", q.words, want)
	}
	if want := []int{0, 212, 7, 0, 0}; !slices.Equal(q.prs, want) {
		t.Fatalf("prs %v, want %v", q.prs, want)
	}
}

// fieldsOf offers every PR the session linked or created, not only the latest
// the PR column shows.
func TestFieldsOfCollectsEveryPullRequest(t *testing.T) {
	t.Parallel()
	m := archive.Metadata{
		SessionID: "abc", Name: "n", Title: "t", Branch: "b", Harness: archive.Harness{Name: "codex"},
		PullRequests: []archive.PullRequestLink{{Repository: "o/r", Number: 5}, {Repository: "o/r", Number: 9}},
		GitActivity: []archive.GitEvent{
			{Kind: archive.GitEventPRCreated, PRNumber: 9},
			{Kind: archive.GitEventPRCreated, PRNumber: 11},
			{Kind: archive.GitEventPRMerged, PRNumber: 13},
		},
	}
	got := fieldsOf(m, "proj")
	if !slices.Equal(got.PRs, []int{5, 9, 11}) {
		t.Fatalf("PRs %v, want [5 9 11] (linked, then created, once each, merged ones not)", got.PRs)
	}
	if got.Name != "n" || got.Title != "t" || got.Branch != "b" || got.Project != "proj" || got.Harness != "codex" || got.SessionID != "abc" {
		t.Fatalf("fields %+v", got)
	}
}

// An exact session ID, whole or as the short ID the table shows, wins over a
// title that happens to contain it; a prefix is only a match.
func TestExactSessionIDWinsOutright(t *testing.T) {
	t.Parallel()
	rows := []sessionFields{
		{SessionID: "ffff000011112222", Title: "Notes on abcd1234 and friends"},
		{SessionID: "abcd1234deadbeef00000000", Title: "Something else"},
		{SessionID: "abcd1234deadbeef11111111", Title: "Sibling"},
	}
	fields := func(r sessionFields) sessionFields { return r }
	ids := func(got []sessionFields) []string {
		var out []string
		for _, r := range got {
			out = append(out, r.SessionID)
		}
		return out
	}
	// The short ID is exact for both sessions that share it, and for no title.
	if got := ids(matchPool(rows, parseSessionQuery("abcd1234"), fields)); !slices.Equal(got, []string{"abcd1234deadbeef00000000", "abcd1234deadbeef11111111"}) {
		t.Fatalf("short ID: %v", got)
	}
	if got := ids(matchPool(rows, parseSessionQuery("ABCD1234DEADBEEF11111111"), fields)); !slices.Equal(got, []string{"abcd1234deadbeef11111111"}) {
		t.Fatalf("full ID: %v", got)
	}
	// Not exact: a shorter prefix matches by prefix, and the title too.
	if got := ids(matchPool(rows, parseSessionQuery("abcd12"), fields)); len(got) != 3 {
		t.Fatalf("prefix: %v", got)
	}
	// Two words are never an ID.
	if got := matchPool(rows, parseSessionQuery("abcd1234 notes"), fields); len(got) != 1 || got[0].Title != rows[0].Title {
		t.Fatalf("two words: %v", got)
	}
}

// A pull request number never matches the start of a session ID, and any
// other word does only from minIDPrefixWord characters, so a random hex ID
// that happens to start with "212" or "add" does not answer a search for PR
// 212 or the word "add".
func TestSessionQueryShortWordsDoNotMatchAnIDPrefix(t *testing.T) {
	t.Parallel()
	byID := sessionFields{SessionID: "212add9f00000000000000000000beef", Title: "Unrelated work"}
	withPR := sessionFields{SessionID: "0000000000000000000000000000cafe", Title: "Linux support", PRs: []int{212}}
	for _, tc := range []struct {
		query  string
		byID   bool
		withPR bool
	}{
		{"212", false, true},
		{"#212", false, true},
		{"2", false, false},
		{"212a", true, false}, // 4 characters, not a number: an ID prefix
		{"212add9f", true, false},
		{"212add9f00000000000000000000beef", true, false},
	} {
		q := parseSessionQuery(tc.query)
		if got := q.matches(byID); got != tc.byID {
			t.Errorf("%q matches the session by its ID = %v, want %v", tc.query, got, tc.byID)
		}
		if got := q.matches(withPR); got != tc.withPR {
			t.Errorf("%q matches the session with PR 212 = %v, want %v", tc.query, got, tc.withPR)
		}
	}
	fields := func(r sessionFields) sessionFields { return r }
	for _, query := range []string{"212", "#212"} {
		if got := matchPool([]sessionFields{byID, withPR}, parseSessionQuery(query), fields); len(got) != 1 || got[0].SessionID != withPR.SessionID {
			t.Errorf("%q: %v, want only the session with PR 212", query, got)
		}
	}
	// A three-character hex word is text only; four characters start an ID.
	hexID := sessionFields{SessionID: "addbed0000000000000000000000cafe", Title: "Unrelated work"}
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"add", false},
		{"ADD", false},
		{"addb", true},
		{"ADDBED", true},
		{"cafe", false}, // the end of an ID is not its start
	} {
		if got := parseSessionQuery(tc.query).matches(hexID); got != tc.want {
			t.Errorf("%q matches = %v, want %v", tc.query, got, tc.want)
		}
	}
	if !parseSessionQuery("add").matches(sessionFields{SessionID: "addbed0000000000000000000000cafe", Title: "Add a systemd unit"}) {
		t.Error("a three-character word no longer matches a title")
	}
}

// Show prefixes remain ambiguous when another title mentions the same prefix.
func TestShowPrefixIncludesTextCandidates(t *testing.T) {
	sessions := []archive.Metadata{
		{SessionID: "abcdef01000000000000000000000001", Harness: archive.Harness{Name: "codex"}},
		{SessionID: "other", Title: "Compare abcdef01 results", Harness: archive.Harness{Name: "codex"}},
	}
	fields := func(m archive.Metadata) sessionFields { return fieldsOf(m, "") }
	got := searchShowSessions(sessions, parseSessionQuery("abcdef01"), sessionScope{}, fields)
	if len(got.matches) != 2 {
		t.Fatalf("show matched %d sessions; want prefix and title", len(got.matches))
	}
	if legacy := searchSessions(sessions, parseSessionQuery("abcdef01"), sessionScope{}, fields); len(legacy.matches) != 1 {
		t.Fatal("handoff prefix precedence changed")
	}
}
