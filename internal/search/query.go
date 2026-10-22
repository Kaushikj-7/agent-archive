package search

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Scope mirrors repository/name/project fallback without loading metadata during search.
type Scope struct {
	Restricted  bool     `json:"Restricted"`
	Named       bool     `json:"Named"`
	RepoKey     string   `json:"RepoKey"`
	ProjectName string   `json:"ProjectName"`
	ProjectIDs  []string `json:"ProjectIDs"`
}

// Inclusion selects all, hidden, or exclusively included replay/child sessions.
type Inclusion string

// Inclusion values preserve the existing serialized scope vocabulary.
const (
	IncludeAll Inclusion = ""
	Hide       Inclusion = "hide"
	Only       Inclusion = "only"
)

// Query applies scope before ranking/grouping. Empty Project means all projects.
// Replay and Children accept "" (all), "hide", or "only".
type Query struct {
	Scope    Scope     `json:"Scope"`
	Text     string    `json:"Text"`
	Project  string    `json:"Project"`
	Harness  string    `json:"Harness"`
	From     time.Time `json:"From"`
	To       time.Time `json:"To"`
	Replay   Inclusion `json:"Replay"`
	Children Inclusion `json:"Children"`
	Limit    int       `json:"Limit"`
	Cursor   string    `json:"Cursor"`
}

// Hit is one session's best bounded match. Title matches have nil Reference;
// MatchCount counts matching passages, not substring occurrences (overlap may
// contribute additional passages for long messages).
type Hit struct {
	SessionID  string     `json:"SessionID"`
	Title      string     `json:"Title"`
	Snippet    string     `json:"Snippet"`
	Role       string     `json:"Role"`
	Harness    string     `json:"Harness"`
	Reference  *Reference `json:"Reference"`
	MatchCount int        `json:"MatchCount"`
	CapturedAt time.Time  `json:"CapturedAt"`
	Gaps       []Gap      `json:"Gaps"`
	// Project is the session's project name, empty when it has none.
	Project string `json:"Project"`
	// ProjectID is the session's project ID, for a configured label when
	// the metadata names no project.
	ProjectID string `json:"ProjectID"`
	// Passage is the indexed text that matched: the body passage Reference
	// addresses, or the title for a title match.
	Passage string `json:"Passage"`
}

// Page is bounded output from one short read transaction. Cursors expire on an
// index write; callers restart explicitly rather than silently skip results.
type Page struct {
	Hits        []Hit       `json:"Hits"`
	Next        string      `json:"Next"`
	Generation  int64       `json:"Generation"`
	Observation Observation `json:"Observation"`
}

// ErrCursorChanged requires restarting pagination after indexing changes.
var ErrCursorChanged = errors.New("search index changed during pagination; restart search")

// ErrInvalidCursor rejects malformed cursors or a different query identity.
var ErrInvalidCursor = errors.New("invalid search cursor or query mismatch; restart search")

type pageCursor struct {
	Generation int64   `json:"Generation"`
	Query      string  `json:"Query"`
	TitleHit   int     `json:"TitleHit"`
	Score      float64 `json:"Score"`
	Role       int     `json:"Role"`
	Captured   int64   `json:"Captured"`
	Session    string  `json:"Session"`
}

// MatchExpression accepts literal whitespace-separated terms, quoted phrases,
// and a trailing * on a term/phrase. Terms are ANDed; FTS operators are always
// quoted data. Unicode61 splits punctuation (foo_bar.go is three adjacent tokens)
// and does not stem or fold accents. At most 512 query bytes bounds phrase overlap.
func MatchExpression(text string) (string, error) {
	terms, err := parseTerms(text)
	if err != nil {
		return "", err
	}
	parts := make([]string, len(terms))
	for i, t := range terms {
		parts[i] = "\"" + t.text + "\""
		if t.prefix {
			parts[i] += "*"
		}
	}
	return strings.Join(parts, " AND "), nil
}

// queryTerm is one term or quoted phrase of a query, and whether a trailing *
// made its last token a prefix.
type queryTerm struct {
	text   string
	prefix bool
}

// parseTerms reads a query as MatchExpression documents it.
func parseTerms(text string) ([]queryTerm, error) {
	if len(text) > 512 || !utf8.ValidString(text) {
		return nil, errors.New("search query must be valid UTF-8 and at most 512 bytes")
	}
	var terms []queryTerm
	for rest := strings.TrimSpace(text); rest != ""; rest = strings.TrimSpace(rest) {
		phrase := false
		var term string
		if rest[0] == '"' {
			phrase = true
			rest = rest[1:]
			end := strings.IndexByte(rest, '"')
			if end < 0 {
				return nil, errors.New("unterminated search phrase")
			}
			term = rest[:end]
			rest = rest[end+1:]
		} else {
			end := strings.IndexFunc(rest, unicode.IsSpace)
			if end < 0 {
				end = len(rest)
			}
			term = rest[:end]
			rest = rest[end:]
		}
		prefix := false
		if phrase {
			if strings.HasPrefix(rest, "*") {
				prefix = true
				rest = rest[1:]
			}
			if rest != "" {
				r, _ := utf8.DecodeRuneInString(rest)
				if !unicode.IsSpace(r) {
					return nil, errors.New("search phrases must be separated by whitespace")
				}
			}
		} else if strings.HasSuffix(term, "*") {
			prefix = true
			term = strings.TrimSuffix(term, "*")
		}
		if strings.ContainsAny(term, "*\"") {
			return nil, errors.New("search wildcard is allowed only at the end of a term or phrase")
		}
		if !strings.ContainsFunc(term, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) {
			return nil, errors.New("search term must contain a letter or number")
		}
		terms = append(terms, queryTerm{term, prefix})
		if len(terms) > 32 {
			return nil, errors.New("search accepts at most 32 terms")
		}
	}
	if len(terms) == 0 {
		return nil, errors.New("search query is empty")
	}
	return terms, nil
}

// Span is the bytes [Start, End) of a text.
type Span struct {
	Start int
	End   int
}

// MatchSpans is where the terms of query occur in text, read as the index's
// tokenizer reads both: tokens are runs of letters, numbers and private-use
// characters, compared case-insensitively, so punctuation splits a term
// (foo_bar.go matches the adjacent tokens foo, bar and go) and a phrase's
// tokens must be adjacent. A trailing * makes a term's last token a prefix,
// and the whole token it starts is the span. Every occurrence of every term is
// returned, sorted, with overlapping spans joined; a query MatchExpression
// refuses has none.
func MatchSpans(query, text string) []Span {
	terms, err := parseTerms(query)
	if err != nil {
		return nil
	}
	words := tokenize(text)
	var spans []Span
	for _, t := range terms {
		want := tokenize(t.text)
		if len(want) == 0 {
			continue
		}
		for i := 0; i+len(want) <= len(words); i++ {
			if phraseAt(words[i:i+len(want)], want, t.prefix) {
				spans = append(spans, Span{words[i].start, words[i+len(want)-1].end})
			}
		}
	}
	slices.SortFunc(spans, func(a, b Span) int { return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(a.End, b.End)) })
	var joined []Span
	for _, s := range spans {
		if n := len(joined); n > 0 && s.Start <= joined[n-1].End {
			joined[n-1].End = max(joined[n-1].End, s.End)
			continue
		}
		joined = append(joined, s)
	}
	return joined
}

// phraseAt reports whether the tokens words are the phrase want, its last
// token a prefix when prefix is set.
func phraseAt(words, want []token, prefix bool) bool {
	last := len(want) - 1
	for j := range want {
		if j == last && prefix {
			if !strings.HasPrefix(words[j].folded, want[j].folded) {
				return false
			}
		} else if words[j].folded != want[j].folded {
			return false
		}
	}
	return true
}

// token is one token of a text as the index's unicode61 tokenizer splits it:
// its bytes [start, end) and its case-folded form.
type token struct {
	start  int
	end    int
	folded string
}

// tokenize splits text into the index's tokens: maximal runs of token
// characters (isTokenRune); everything else separates.
func tokenize(text string) []token {
	var out []token
	start := -1
	for i, r := range text {
		// A diacritic continues a token but does not start one.
		if isTokenRune(r) && (start >= 0 || !latinDiacritic(r)) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			out = append(out, token{start, i, foldToken(text[start:i])})
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, token{start, len(text), foldToken(text[start:])})
	}
	return out
}

// isTokenRune reports whether unicode61 reads r as part of a token: a letter,
// a number or a private-use character (its default categories L*, N* and Co),
// one of the Latin combining diacritics it knows (after another token
// character only: tokenize), or a character its Unicode
// 6.1 tables do not classify, which they give no category to separate by. Go's
// tables are newer, so a character Unicode assigned after 6.1 (an emoji added
// since, say) is read as a separator here and as a token by the index.
func isTokenRune(r rune) bool {
	switch {
	case r == utf8.RuneError || r == 0xFFFE || r == 0xFFFF:
		return false
	case unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Co, r):
		return true
	case r >= 0x300 && r <= 0x331:
		return latinDiacritic(r)
	}
	return !unicode.In(r, unicode.M, unicode.P, unicode.S, unicode.Z, unicode.Cc, unicode.Cf, unicode.Cs)
}

// latinDiacritic reports whether r, in U+0300 to U+0331, is a combining mark
// unicode61 can remove from a Latin letter, which it keeps in the token even
// with remove_diacritics 0.
func latinDiacritic(r rune) bool {
	switch {
	case r < 0x300 || r > 0x331:
		return false
	case r <= 0x304, r >= 0x306 && r <= 0x30C, r == 0x30F, r == 0x311, r == 0x31B, r >= 0x323 && r <= 0x328, r == 0x32D, r == 0x32E, r == 0x330, r == 0x331:
		return true
	}
	return false
}

// foldToken compares tokens as the index does, without case: each character
// as the smallest of its simple case folds (S, s and ſ alike).
func foldToken(s string) string { return foldProjectName(s) }

const matchingQuery = `WITH matching AS MATERIALIZED (
 SELECT search_fts.rowid AS pid,p.session,p.title_hit,CASE p.role WHEN 'user' THEN 0 ELSE 1 END AS role_rank,
 bm25(search_fts,4.0,1.0) AS score,s.captured
 FROM search_fts JOIN search_passages p ON p.id=search_fts.rowid JOIN search_sessions s ON s.id=p.session
 WHERE search_fts MATCH ? AND (?='' OR s.project=?) AND (?='' OR s.harness=?)
 AND (?=0 OR s.captured>=?) AND (?=0 OR s.captured<=?)
 AND (?='' OR (?='only' AND s.replay=1) OR (?='hide' AND s.replay=0))
 AND (?='' OR (?='only' AND s.child=1) OR (?='hide' AND s.child=0))
 AND (?=0 OR CASE WHEN ?<>'' AND s.repo<>'' THEN s.repo=? WHEN ?=1 THEN (s.project_name=? OR s.project IN (SELECT value FROM json_each(?))) ELSE s.project IN (SELECT value FROM json_each(?)) END)`

const rankedQuery = matchingQuery + `
), grouped AS (
 SELECT *,row_number() OVER(PARTITION BY session ORDER BY title_hit DESC,score,role_rank,pid) AS best,
 count(*) OVER(PARTITION BY session ORDER BY title_hit DESC,score,role_rank,pid ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) AS matches FROM matching
)
 SELECT pid,session,title_hit,score,role_rank,captured,matches FROM grouped WHERE best=1`

func queryIdentity(q Query) string {
	q.Cursor = ""
	q.Limit = 0
	b, _ := json.Marshal(q)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Search performs no archive reads, refreshes or parser work. Candidate ranking
// is exhaustive within scope: limits bound output, not common-word FTS work.
func (s *Store) Search(ctx context.Context, q Query) (Page, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	page := Page{Hits: []Hit{}, Observation: Observation{Limitations: []string{"archive coverage has not been reconciled"}}}
	expression, err := MatchExpression(q.Text)
	if err != nil {
		return page, err
	}
	for _, v := range []Inclusion{q.Replay, q.Children} {
		if v != IncludeAll && v != Hide && v != Only {
			return page, errors.New("invalid search scope")
		}
	}
	if q.Limit == 0 {
		q.Limit = 20
	}
	if q.Limit < 1 || q.Limit > 100 {
		return page, errors.New("search limit must be between 1 and 100")
	}
	identity := queryIdentity(q)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.readObservation(ctx, tx, &page.Observation); err != nil {
		return page, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT generation FROM search_state WHERE id=1").Scan(&page.Generation); err != nil {
		return page, err
	}
	args := queryArgs(expression, q)
	sqlText := rankedQuery
	if q.Cursor != "" {
		var c pageCursor
		b, e := base64.RawURLEncoding.DecodeString(q.Cursor)
		if e != nil || len(b) > 4096 {
			return page, ErrInvalidCursor
		}
		if e = json.Unmarshal(b, &c); e != nil || c.Query != identity {
			return page, ErrInvalidCursor
		}
		if c.Generation != page.Generation {
			return page, ErrCursorChanged
		}
		sqlText += ` AND (-title_hit,score,role_rank,-captured,session) > (?,?,?,?,?)`
		args = append(args, -c.TitleHit, c.Score, c.Role, -c.Captured, c.Session)
	}
	sqlText += " ORDER BY title_hit DESC,score,role_rank,captured DESC,session LIMIT ?"
	args = append(args, q.Limit+1)
	rows, err := tx.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return page, err
	}
	defer func() { _ = rows.Close() }()
	type selected struct {
		pid      int64
		session  string
		titleHit int
		score    float64
		role     int
		captured int64
		matches  int
	}
	var chosen []selected
	for rows.Next() {
		var v selected
		if err = rows.Scan(&v.pid, &v.session, &v.titleHit, &v.score, &v.role, &v.captured, &v.matches); err != nil {
			return page, err
		}
		chosen = append(chosen, v)
	}
	err = rows.Err()
	if err != nil {
		return page, err
	}
	more := len(chosen) > q.Limit
	if more {
		chosen = chosen[:q.Limit]
	}
	for _, v := range chosen {
		hit := Hit{MatchCount: v.matches, CapturedAt: time.Unix(0, v.captured).UTC()}
		var ref, gaps []byte
		// Only selected session hits invoke snippet(), never all FTS candidates.
		// search_sessions keeps the project's name folded for matching; the
		// name shown is the observed metadata's.
		var observed []byte
		err = tx.QueryRowContext(ctx, `SELECT s.session_id,s.harness,s.title,p.reference,p.role,s.gaps,snippet(search_fts,?,'','',' … ',32),CASE p.title_hit WHEN 1 THEN search_fts.title ELSE search_fts.body END,(SELECT w.metadata FROM search_work w WHERE w.id=p.session) FROM search_fts JOIN search_passages p ON p.id=search_fts.rowid JOIN search_sessions s ON s.id=p.session WHERE search_fts MATCH ? AND search_fts.rowid=?`, 1-v.titleHit, expression, v.pid).Scan(&hit.SessionID, &hit.Harness, &hit.Title, &ref, &hit.Role, &gaps, &hit.Snippet, &hit.Passage, &observed)
		if err != nil {
			return page, err
		}
		hit.Project, hit.ProjectID = projectOf(observed)
		if len(ref) > 0 {
			hit.Reference = new(Reference)
			if err = json.Unmarshal(ref, hit.Reference); err != nil {
				return page, err
			}
		}
		if err = json.Unmarshal(gaps, &hit.Gaps); err != nil {
			return page, err
		}
		hit.Title = archive.DisplayLine(hit.Title)
		hit.Snippet = boundSnippet(archive.DisplayLine(hit.Snippet))
		page.Hits = append(page.Hits, hit)
	}
	if more {
		v := chosen[len(chosen)-1]
		b, e := json.Marshal(pageCursor{page.Generation, identity, v.titleHit, v.score, v.role, v.captured, v.session})
		if e != nil {
			return page, e
		}
		page.Next = base64.RawURLEncoding.EncodeToString(b)
	}
	return page, tx.Commit()
}

// projectNameOf is the project name in a session's observed metadata (JSON),
// made safe for one line; empty when there is none.
// projectOf is the observed metadata's project name and ID.
func projectOf(metadata []byte) (name, id string) {
	var project struct {
		ProjectName string `json:"project_name"`
		ProjectID   string `json:"project_id"`
	}
	if len(metadata) == 0 || json.Unmarshal(metadata, &project) != nil {
		return "", ""
	}
	return archive.DisplayLine(project.ProjectName), project.ProjectID
}

func queryArgs(expression string, q Query) []any {
	var from, to int64
	if !q.From.IsZero() {
		from = q.From.UnixNano()
	}
	if !q.To.IsZero() {
		to = q.To.UnixNano()
	}
	ids, _ := json.Marshal(q.Scope.ProjectIDs)
	return []any{expression, q.Project, q.Project, q.Harness, q.Harness, from, from, to, to, q.Replay, q.Replay, q.Replay, q.Children, q.Children, q.Children, q.Scope.Restricted, q.Scope.RepoKey, q.Scope.RepoKey, q.Scope.Named, foldProjectName(q.Scope.ProjectName), string(ids), string(ids)}
}

func boundSnippet(text string) string {
	if utf8.RuneCountInString(text) <= 512 {
		return text
	}
	return string([]rune(text)[:511]) + "…"
}

func foldProjectName(s string) string {
	return strings.Map(func(r rune) rune {
		smallest := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < smallest {
				smallest = next
			}
		}
		return smallest
	}, s)
}

func (s *Store) readObservation(ctx context.Context, tx *sql.Tx, observation *Observation) error {
	var err error
	if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(failures>0),0) FROM search_work WHERE desired!=indexed").Scan(&observation.Pending, &observation.Failed); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM search_work").Scan(&observation.Known); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(coalesce(json_array_length(gaps),0)>0),0) FROM search_sessions").Scan(&observation.Indexed, &observation.Unsupported); err != nil {
		return err
	}

	var reconciled, observed int64
	var mode, complete, checked string
	var seeded int
	if err = tx.QueryRowContext(ctx, "SELECT observed,mode,complete_root,seeded,checked_root FROM search_discovery WHERE id=1").Scan(&reconciled, &mode, &complete, &seeded, &checked); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(max(observed),0),(SELECT count(*) FROM search_observed WHERE observed=0) FROM search_observed").Scan(&observed, &observation.Rechecks); err != nil {
		return err
	}
	if reconciled > 0 {
		observation.LastReconciled = time.Unix(0, reconciled).UTC()
	}
	if observed = max(observed, reconciled); observed > 0 {
		observation.LastObserved = time.Unix(0, observed).UTC()
	}
	observation.DiscoveryComplete = mode == "catalog" && complete != ""
	var gaps int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM search_sessions WHERE coalesce(json_array_length(gaps),0)>0").Scan(&gaps); err != nil {
		return err
	}
	observation.IdentityChecksComplete = observation.Rechecks == 0 && seeded == 3 && (mode != "catalog" || (checked != "" && checked == complete))
	observation.Complete = observation.DiscoveryComplete && observation.IdentityChecksComplete && observation.Pending == 0 && gaps == 0
	if reconciled > 0 || observed > 0 {
		observation.Limitations = []string{"remote changes after last observation are unknown"}
		if !observation.DiscoveryComplete {
			observation.Limitations = append(observation.Limitations, "discovery is partial; a mutable legacy listing cannot prove snapshot completeness")
		}
		if !observation.IdentityChecksComplete {
			observation.Limitations = append(observation.Limitations, "local identities still require bounded authoritative validation")
		}
		if gaps > 0 {
			observation.Limitations = append(observation.Limitations, "retained sources have extraction coverage gaps")
		}
	}
	return nil
}

// ValidateCursor bounds and validates the opaque cursor before command IO.
func ValidateCursor(token string) error {
	if len(token) > 5500 {
		return ErrInvalidCursor
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) > 4096 {
		return ErrInvalidCursor
	}
	var value pageCursor
	if err = json.Unmarshal(b, &value); err != nil || value.Query == "" {
		return ErrInvalidCursor
	}
	return nil
}
