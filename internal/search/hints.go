package search

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// SessionIdentity is a content-free candidate for authoritative metadata recheck.
type SessionIdentity struct {
	Harness   string
	SessionID string
}

const hintBatchLimit = 256

const unobservedHintLimit = 2048

// RecordSessionHints admits one bounded identity-only batch to an existing,
// current-schema namespace. It never creates, upgrades or repairs an index.
// Known identities retain their real revision and become pending rechecks. Hints
// do not rewind the fair sweep cursor or claim a new metadata revision. Missing/old/busy caches may
// drop the entire batch; periodic reconciliation supplies recovery.
// The caller holds the home lifecycle lock and supplies a short deadline.
func RecordSessionHints(ctx context.Context, dir, destination string, identities []SessionIdentity) (err error) {
	if len(identities) == 0 {
		return nil
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("search hints require a real index directory")
	}
	path := filepath.Join(dir, "content.sqlite")
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("search hints require a regular index file")
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=rw&_pragma=busy_timeout(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != schemaVersion {
		return ErrUpgradeRequired
	}
	var actual string
	if err = db.QueryRowContext(ctx, "SELECT destination FROM search_state WHERE id=1").Scan(&actual); err != nil {
		return err
	}
	if actual != destination {
		return errors.New("search destination/privacy context mismatch")
	}
	return insertSessionHints(ctx, db, identities)
}

func insertSessionHints(ctx context.Context, db *sql.DB, identities []SessionIdentity) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM (SELECT key FROM search_observed WHERE observed=0 AND revision='' LIMIT ?)", unobservedHintLimit).Scan(&count); err != nil {
		return err
	}
	remaining := unobservedHintLimit - count
	seen := map[SessionIdentity]bool{}
	for _, id := range identities[:min(len(identities), hintBatchLimit)] {
		if seen[id] {
			continue
		}
		seen[id] = true
		key, e := archive.MetadataObjectKey(id.Harness, id.SessionID)
		if e != nil {
			continue
		}
		updated, e := tx.ExecContext(ctx, "UPDATE search_observed SET observed=0 WHERE key=?", key)
		if e != nil {
			return e
		}
		exists, e := updated.RowsAffected()
		if e != nil {
			return e
		}
		if exists > 0 || remaining <= 0 {
			continue
		}
		result, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO search_observed(key,session,revision,observed) VALUES(?,?,'',0)", key, id.Harness+"/"+id.SessionID)
		if e != nil {
			return e
		}
		inserted, e := result.RowsAffected()
		if e != nil {
			return e
		}
		remaining -= int(inserted)
	}
	return tx.Commit()
}
