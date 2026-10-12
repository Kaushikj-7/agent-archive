package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// maxQueryPRDigits is the longest bare number that also reads as a pull
// request number: a longer one is text (a timestamp, an ID fragment).
const maxQueryPRDigits = 6

// minIDPrefixWord is the shortest word that matches the start of a session ID,
// as git's shortest abbreviation is. Session IDs are random hexadecimal, so a
// shorter word ("21", "add") would start about one session in 16^len by
// chance and make a search for it ambiguous in a large archive.
const minIDPrefixWord = 4

// sessionQuery is parsed once from what the person or agent typed. words are
// the query's words, lower-cased; prs[i] is word i read as a pull request
// number (`#212`, or a bare number of 1 to 6 digits), or 0 when it is not
// one.
type sessionQuery struct {
	words []string
	prs   []int
}

// parseSessionQuery splits s into words on white space. A word `#N`, or a
// bare number of 1 to 6 digits, is also a pull request number.
func parseSessionQuery(s string) sessionQuery {
	words := strings.Fields(strings.ToLower(s))
	q := sessionQuery{words: words, prs: make([]int, len(words))}
	for i, word := range words {
		q.prs[i] = queryPRNumber(word)
	}
	return q
}

// queryPRNumber is word as a pull request number, or 0.
func queryPRNumber(word string) int {
	digits := strings.TrimPrefix(word, "#")
	if digits == "" || len(digits) > maxQueryPRDigits {
		return 0
	}
	n := 0
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// empty reports whether the query has no words, which matches nothing.
func (q sessionQuery) empty() bool { return len(q.words) == 0 }

// sessionFields is what the matcher reads of a session: the fields a person
// remembers it by.
type sessionFields struct {
	Name      string
	Title     string
	Branch    string
	Project   string
	Harness   string
	SessionID string
	// PRs are the numbers of every pull request the session was linked to
	// or created.
	PRs []int
}

// fieldsOf is the matcher's view of a session's metadata. project is the
// name the project is shown as (sessionProjectName). PRs are the session's
// linked pull requests and those its tool calls created, every one, not only
// the latest the PR column shows.
func fieldsOf(m archive.Metadata, project string) sessionFields {
	var prs []int
	add := func(n int) {
		if n > 0 && !slices.Contains(prs, n) {
			prs = append(prs, n)
		}
	}
	for _, pr := range m.PullRequests {
		add(pr.Number)
	}
	for _, event := range m.GitActivity {
		if event.Kind == archive.GitEventPRCreated {
			add(event.PRNumber)
		}
	}
	return sessionFields{Name: m.Name, Title: m.Title, Branch: m.Branch, Project: project,
		Harness: m.Harness.Name, SessionID: m.SessionID, PRs: prs}
}

// sessionProjectName is the project a session is shown under: the name it
// was archived with, else its configured label, "" when it has neither.
func sessionProjectName(m archive.Metadata, labels map[string]string) string {
	if m.ProjectName != "" {
		return m.ProjectName
	}
	return labels[m.ProjectID]
}

// matches reports whether every word appears in some field of the row,
// ignoring case: in the name, title, branch, project name or harness, or
// starting the session ID (a word of minIDPrefixWord characters or more that
// is not a pull request number). Words may match different fields. A word that
// is a pull request number also matches a PR of the row exactly; `#N` as text
// matches only the whole reference, so `#21` does not find `#213`.
func (q sessionQuery) matches(r sessionFields) bool {
	if q.empty() {
		return false
	}
	texts := [...]string{strings.ToLower(r.Name), strings.ToLower(r.Title), strings.ToLower(r.Branch),
		strings.ToLower(r.Project), strings.ToLower(r.Harness)}
	id := strings.ToLower(r.SessionID)
	for i, word := range q.words {
		if !q.wordMatches(i, word, texts[:], id, r.PRs) {
			return false
		}
	}
	return true
}

// wordMatches reports whether word i of the query matches the row: a PR of it,
// the start of its ID, or one of its texts. A word that is a pull request
// number never matches the start of an ID (`212` is PR 212, not every session
// whose random ID starts with 212), and any other word does only from
// minIDPrefixWord characters; an exact or short ID is exactIDWins'.
func (q sessionQuery) wordMatches(i int, word string, texts []string, id string, prs []int) bool {
	if n := q.prs[i]; n > 0 && slices.Contains(prs, n) {
		return true
	}
	if q.prs[i] == 0 && len(word) >= minIDPrefixWord && strings.HasPrefix(id, word) {
		return true
	}
	if strings.HasPrefix(word, "#") && q.prs[i] > 0 {
		return slices.ContainsFunc(texts, func(text string) bool { return containsReference(text, word) })
	}
	return slices.ContainsFunc(texts, func(text string) bool { return strings.Contains(text, word) })
}

// containsReference reports whether text holds ref (`#213`) not followed by
// another digit.
func containsReference(text, ref string) bool {
	for from := 0; ; {
		at := strings.Index(text[from:], ref)
		if at < 0 {
			return false
		}
		end := from + at + len(ref)
		if end >= len(text) || text[end] < '0' || text[end] > '9' {
			return true
		}
		from += at + 1
	}
}

// exactIDWins returns the items whose session ID is the query's one word,
// whole or as the short ID the table shows, and nil when there are none: an
// exact ID wins outright, over any title that happens to contain it.
func exactIDWins[T any](items []T, q sessionQuery, fields func(T) sessionFields) []T {
	if len(q.words) != 1 {
		return nil
	}
	word := q.words[0]
	var exact []T
	for _, item := range items {
		id := strings.ToLower(fields(item).SessionID)
		if id == word || len(word) == minShortSessionID && strings.HasPrefix(id, word) {
			exact = append(exact, item)
		}
	}
	return exact
}

// matchPool returns the items q matches, in the order given (newest activity
// first, as every caller lists them), or only those with the exact session
// ID when there are any.
func matchPool[T any](items []T, q sessionQuery, fields func(T) sessionFields) []T {
	if q.empty() {
		return nil
	}
	if exact := exactIDWins(items, q, fields); len(exact) > 0 {
		return exact
	}
	var matched []T
	for _, item := range items {
		if q.matches(fields(item)) {
			matched = append(matched, item)
		}
	}
	return matched
}

// sessionSearch is the answer of the search tiers over archived sessions.
type sessionSearch struct {
	// matches are the sessions of the first tier that has any, newest first.
	matches []archive.Metadata
	// inScope is set when the answer is the scope's own: its top-level
	// sessions, or (when it has none and no top-level session anywhere
	// matches) its subagents.
	inScope bool
	// subagents is set when the answer is subagent sessions.
	subagents bool
	// outside counts the sessions of the same kind that match outside the
	// scope; 0 unless inScope.
	outside int
}

// searchSessions applies the tiers of the search, each tried only when the
// one before has no match: top-level sessions in scope, top-level sessions
// anywhere, then subagents in scope, then anywhere. An exact session ID wins
// before any of them. A scope that is off, or names no project, has no
// in-scope tiers.
func searchSessions(sessions []archive.Metadata, q sessionQuery, scope sessionScope, fields func(archive.Metadata) sessionFields) sessionSearch {
	return searchMatchedSessions(matchPool(sessions, q, fields), scope)
}

// searchShowSessions combines short identity prefixes and text matches before
// applying the existing scope tiers. Full IDs keep their direct lookup path.
func searchShowSessions(sessions []archive.Metadata, q sessionQuery, scope sessionScope, fields func(archive.Metadata) sessionFields) sessionSearch {
	var matched []archive.Metadata
	for _, m := range sessions {
		if q.matches(fields(m)) || len(exactIDWins([]archive.Metadata{m}, q, fields)) > 0 {
			matched = append(matched, m)
		}
	}
	return searchMatchedSessions(matched, scope)
}

func searchMatchedSessions(matched []archive.Metadata, scope sessionScope) sessionSearch {
	var top, subagents []archive.Metadata
	for _, m := range matched {
		if !m.IsChild() {
			top = append(top, m)
		} else {
			subagents = append(subagents, m)
		}
	}
	within := func(all []archive.Metadata) []archive.Metadata {
		return slices.DeleteFunc(slices.Clone(all), func(m archive.Metadata) bool { return !scope.contains(m, nil) })
	}
	if scope.narrowed() {
		if inScope := within(top); len(inScope) > 0 {
			return sessionSearch{matches: inScope, inScope: true, outside: len(top) - len(inScope)}
		}
	}
	if len(top) > 0 {
		return sessionSearch{matches: top}
	}
	if scope.narrowed() {
		if inScope := within(subagents); len(inScope) > 0 {
			return sessionSearch{matches: inScope, inScope: true, subagents: true, outside: len(subagents) - len(inScope)}
		}
	}
	return sessionSearch{matches: subagents, subagents: true}
}

// outsideNote is the note that says how many more sessions match outside the
// scope an answer came from: "1 match in agent-archive (3 more in other
// projects: --all-projects or a project name finds them)". It is "" unless
// the scope's top-level sessions answered and others match elsewhere.
func (s sessionSearch) outsideNote(scope sessionScope) string {
	if !s.inScope || s.subagents || s.outside == 0 {
		return ""
	}
	return outsideNote(len(s.matches), s.outside, scope.Label)
}

// outsideNote words the note for matches in the scope called label and
// outside more elsewhere.
func outsideNote(matches, outside int, label string) string {
	noun := "matches"
	if matches == 1 {
		noun = "match"
	}
	return fmt.Sprintf("%d %s in %s (%d more in other projects: --all-projects or a project name finds them)",
		matches, noun, archive.DisplayLine(label), outside)
}
