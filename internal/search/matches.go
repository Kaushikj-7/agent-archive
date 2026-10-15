package search

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// MatchPage contains at most twenty body references in the original query's
// relevance order. Cursors are tied to its generation and qualified session.
type MatchPage struct {
	References []Reference
	Next       string
	Previous   string
}

type matchCursor struct {
	Generation int64
	Query      string
	Session    string
	Score      float64
	Role       int
	Passage    int64
	Reverse    bool
}

// Matches reads one bounded page of matching body passages without reading any
// archive objects. Title matches are deliberately excluded. A generation change
// requires an explicit search restart, including for the first match page.
func (s *Store) Matches(ctx context.Context, q Query, harness, session string, generation int64, cursor string) (MatchPage, error) {
	page := MatchPage{References: []Reference{}}
	expression, err := MatchExpression(q.Text)
	if err != nil {
		return page, err
	}
	for _, v := range []Inclusion{q.Replay, q.Children} {
		if v != IncludeAll && v != Hide && v != Only {
			return page, errors.New("invalid search scope")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return page, err
	}
	defer func() { _ = tx.Rollback() }()
	var current int64
	if err = tx.QueryRowContext(ctx, "SELECT generation FROM search_state WHERE id=1").Scan(&current); err != nil {
		return page, err
	}
	if current != generation {
		return page, ErrCursorChanged
	}
	key, identity := harness+"/"+session, queryIdentity(q)
	c, err := decodeMatchCursor(cursor, generation, identity, key)
	if err != nil {
		return page, err
	}
	text, args := matchPageSQL(q, expression, harness, session, c, cursor != "")
	rows, err := tx.QueryContext(ctx, text, args...)
	if err != nil {
		return page, err
	}
	type selected struct {
		pid   int64
		score float64
		role  int
	}
	var selectedRows []selected
	for rows.Next() {
		var v selected
		if err = rows.Scan(&v.pid, &v.score, &v.role); err != nil {
			_ = rows.Close()
			return page, err
		}
		selectedRows = append(selectedRows, v)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return page, err
	}
	more := len(selectedRows) > 20
	if more {
		selectedRows = selectedRows[:20]
	}
	if c.Reverse {
		for i, j := 0, len(selectedRows)-1; i < j; i, j = i+1, j-1 {
			selectedRows[i], selectedRows[j] = selectedRows[j], selectedRows[i]
		}
	}
	for _, v := range selectedRows {
		var data []byte
		if err = tx.QueryRowContext(ctx, "SELECT reference FROM search_passages WHERE id=?", v.pid).Scan(&data); err != nil {
			return page, err
		}
		var ref Reference
		if err = json.Unmarshal(data, &ref); err != nil {
			return page, err
		}
		page.References = append(page.References, ref)
	}
	encode := func(v selected, reverse bool) string {
		b, _ := json.Marshal(matchCursor{generation, identity, key, v.score, v.role, v.pid, reverse})
		return base64.RawURLEncoding.EncodeToString(b)
	}
	if len(selectedRows) > 0 {
		if !c.Reverse && more || c.Reverse && cursor != "" {
			page.Next = encode(selectedRows[len(selectedRows)-1], false)
		}
		if c.Reverse && more || !c.Reverse && cursor != "" {
			page.Previous = encode(selectedRows[0], true)
		}
	}
	return page, tx.Commit()
}

func decodeMatchCursor(token string, generation int64, identity, key string) (matchCursor, error) {
	c := matchCursor{Generation: generation, Query: identity, Session: key}
	if token == "" {
		return c, nil
	}
	if len(token) > 5500 {
		return c, ErrInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(data) > 4096 {
		return c, ErrInvalidCursor
	}
	if err = json.Unmarshal(data, &c); err != nil || c.Query != identity || c.Session != key {
		return c, ErrInvalidCursor
	}
	if c.Generation != generation {
		return c, ErrCursorChanged
	}
	return c, nil
}

// matchPageSQL is the concrete query used by Matches, shared with its actual
// EXPLAIN witness so performance evidence cannot drift to a different query.
func matchPageSQL(q Query, expression, harness, session string, c matchCursor, continued bool) (string, []any) {
	// One range-constrained MATCH cursor avoids reopening FTS for each member.
	// Exact session predicates retain correctness even when body IDs are sparse.
	text := matchingQuery + ` AND s.harness=? AND s.session_id=? AND p.title_hit=0
 AND search_fts.rowid >= (SELECT min(id) FROM search_passages WHERE session=? AND title_hit=0)
 AND search_fts.rowid <= (SELECT max(id) FROM search_passages WHERE session=? AND title_hit=0)
) SELECT pid,score,role_rank FROM matching WHERE title_hit=0 AND session=?`
	args := append(queryArgs(expression, q), harness, session, harness+"/"+session, harness+"/"+session, harness+"/"+session)
	if continued {
		comparison := ">"
		if c.Reverse {
			comparison = "<"
		}
		text += " AND (score,role_rank,pid) " + comparison + " (?,?,?)"
		args = append(args, c.Score, c.Role, c.Passage)
	}
	order := "ASC"
	if c.Reverse {
		order = "DESC"
	}
	text += " ORDER BY score " + order + ",role_rank " + order + ",pid " + order + " LIMIT 21"
	return text, args
}
