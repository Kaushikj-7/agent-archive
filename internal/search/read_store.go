package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// ErrUpgradeRequired requires explicit refresh to migrate an existing index.
var ErrUpgradeRequired = errors.New("search index needs upgrade; run search --refresh")

// ErrUninitialized identifies a schema-zero file with no application objects.
// Read callers still require explicit refresh; an opted-in worker may retry initialization.
var ErrUninitialized = fmt.Errorf("%w: initial index creation was interrupted", ErrUpgradeRequired)

// ErrWorkBusy means another refresh or lifecycle change owns search state.
var ErrWorkBusy = errors.New("another search refresh is running or lifecycle change owns search state")

// OpenRead opens only an existing current-schema index without writer admission.
// Missing files are reported with os.ErrNotExist and are never recreated.
func OpenRead(ctx context.Context, dir, destination string) (*Store, error) {
	path := filepath.Join(dir, "content.sqlite")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("search requires a regular index file")
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro&_pragma=busy_timeout(100)&_pragma=query_only(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, destination: destination}
	var version int
	err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	if err == nil && version != schemaVersion {
		err = ErrUpgradeRequired
		if version == 0 {
			var objects int
			probe := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&objects)
			if probe != nil {
				err = probe
			} else if objects == 0 {
				err = ErrUninitialized
			}
		}
	}
	var actual string
	if err == nil {
		err = db.QueryRowContext(ctx, "SELECT destination FROM search_state WHERE id=1").Scan(&actual)
	}
	if err == nil && actual != destination {
		err = errors.New("search destination/privacy context mismatch")
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Rebuild clears derived content atomically while preserving cursor generations.
// The caller holds the home search lifecycle lock.
func (s *Store) Rebuild(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{"DELETE FROM search_fts", "DELETE FROM search_passages", "DELETE FROM search_sessions", "DELETE FROM search_sources", "DELETE FROM search_work", "DELETE FROM search_observed", "DELETE FROM search_discovery"} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO search_discovery(id) VALUES(1); UPDATE search_state SET generation=generation+1 WHERE id=1"); err != nil {
		return err
	}
	return tx.Commit()
}

// Observation reads namespace-wide local coverage without archive IO.
func (s *Store) Observation(ctx context.Context) (Observation, error) {
	var out Observation
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	err = s.readObservation(ctx, tx, &out)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// TargetComplete checks only the explicitly named session's installed work.
func (s *Store) TargetComplete(ctx context.Context, harness, session string) (bool, error) {
	var complete bool
	var failures int
	err := s.db.QueryRowContext(ctx, `SELECT s.id IS NOT NULL AND w.desired=w.indexed AND w.failures=0 AND coalesce(json_array_length(s.gaps),0)=0,w.failures FROM search_work w LEFT JOIN search_sessions s ON s.id=w.id WHERE w.id=?`, harness+"/"+session).Scan(&complete, &failures)
	if err == nil && failures > 0 {
		err = ErrFailedWork
	}
	return complete, err
}

// ErrFailedWork identifies observed indexing failures for an explicit target.
var ErrFailedWork = errors.New("search target has failed work; retry after backoff or inspect coverage")
