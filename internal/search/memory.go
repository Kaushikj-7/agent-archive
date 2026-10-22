package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// OpenMemory creates a search store with Open's schema that lives only in
// this process's memory: no database, journal or temporary file is written,
// and Close discards every passage. It serves a search that reads the archive
// directly instead of a local index, through the same Observe, Run, Search
// and Matches as an index on disk.
func OpenMemory(ctx context.Context, destination string) (*Store, error) {
	if destination == "" {
		return nil, errors.New("search destination context required")
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	// Each connection to :memory: is its own database, so the store keeps
	// exactly one, open for its whole life.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	s := &Store{db: db, destination: destination}
	// Sorts and FTS merges spill to temporary files unless told otherwise.
	if _, err = db.ExecContext(ctx, "PRAGMA temp_store=MEMORY"); err == nil {
		err = s.initialize(ctx)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// SessionMatches counts one session's passages, title included, that match
// q within q's scope and filters: the MatchCount Search would report for it,
// or 0 when it does not match. It reads no archive objects.
func (s *Store) SessionMatches(ctx context.Context, q Query, harness, session string) (int, error) {
	expression, err := MatchExpression(q.Text)
	if err != nil {
		return 0, err
	}
	var count int
	err = s.db.QueryRowContext(ctx, matchingQuery+") SELECT count(*) FROM matching WHERE session=?", append(queryArgs(expression, q), harness+"/"+session)...).Scan(&count)
	return count, err
}

// DropSessionText removes the conversation text a finished session no longer
// needs: its staged source projections always, and its passages too unless
// keep is set, leaving the session's metadata row so coverage still counts
// it. A store searched directly keeps only the text of matching sessions.
func (s *Store) DropSessionText(ctx context.Context, harness, session string, keep bool) error {
	id := harness + "/" + session
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	statements := []string{"DELETE FROM search_sources WHERE session=?"}
	if !keep {
		statements = append(statements, "DELETE FROM search_fts WHERE rowid IN (SELECT id FROM search_passages WHERE session=?)", "DELETE FROM search_passages WHERE session=?")
	}
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, statement, id); err != nil {
			return err
		}
	}
	if !keep {
		if _, err = tx.ExecContext(ctx, "UPDATE search_state SET generation=generation+1 WHERE id=1"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Prefetched is a session's sources loaded ahead of its turn, for Stage.
type Prefetched struct {
	id      string
	sources map[string][]byte
}

// Prefetch loads and parses every source of a session, as Run would, without
// touching the database, so several sessions can be prefetched at once while
// another is indexed. A session too large to index is not loaded.
func (s *Store) Prefetch(ctx context.Context, objects storage.ObjectStore, parsers agentapi.ParsersLookup, metadata archive.Metadata) (Prefetched, error) {
	reqs, err := requests(metadata, parsers)
	if err != nil {
		return Prefetched{}, err
	}
	compressed := 0
	for _, req := range reqs {
		if req.Reference.CompressedBytes > maxSessionBytes-compressed {
			return Prefetched{}, storage.ErrObjectTooLarge
		}
		compressed += req.Reference.CompressedBytes
	}
	out := Prefetched{id: metadata.Harness.Name + "/" + metadata.SessionID, sources: map[string][]byte{}}
	for _, req := range reqs {
		p, err := LoadProjection(ctx, objects, parsers, s.destination, metadata, req.Revision, reader.Limits{})
		if err != nil {
			return Prefetched{}, err
		}
		data, err := json.Marshal(p)
		if err != nil {
			return Prefetched{}, err
		}
		out.sources[digest(req)] = data
	}
	return out, nil
}

// Stage keeps prefetched sources where Run reuses them instead of reading
// them again. Each is kept under its request's identity, so only a Run of the
// same sources reuses it, and Run still rechecks the session against the
// bucket (VerifyCurrent) before installing it. Stage follows the session's
// Observe; Observe and ForgetSession drop what it staged as they drop what
// Run staged.
func (s *Store) Stage(ctx context.Context, p Prefetched) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for identity, data := range p.sources {
		if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO search_sources VALUES(?,?,?)", p.id, identity, data); err != nil {
			return err
		}
	}
	return tx.Commit()
}
