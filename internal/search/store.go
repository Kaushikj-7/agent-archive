package search

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	_ "modernc.org/sqlite" // Register the same pure-Go driver used by the metadata catalog.
)

// Store is a private, destination-bound, disposable content index. The caller
// owns scheduling and supplies a directory outside the metadata-only catalog.
type Store struct {
	db          *sql.DB
	destination string
}

// Open creates or opens a private content database with short bounded writes.
// A different destination/privacy context requires a different directory.
func Open(ctx context.Context, dir, destination string) (*Store, error) {
	if destination == "" {
		return nil, errors.New("search destination context required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return nil, errors.New("search requires a real directory")
	}
	if err = os.Chmod(dir, 0700); err != nil { //nolint:gosec // Private directory needs owner traversal.
		return nil, err
	}
	path := filepath.Join(dir, "content.sqlite")
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		info, e := os.Lstat(name)
		if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
		if e == nil {
			if !info.Mode().IsRegular() {
				return nil, errors.New("search requires regular files")
			}
			if e = os.Chmod(name, 0600); e != nil {
				return nil, e
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_pragma=busy_timeout(100)&_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(256)&_pragma=secure_delete(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, destination: destination}
	if err = s.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) initialize(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 0 && version != 1 {
		return errors.New("unsupported search schema; rebuild index")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS search_state (id INTEGER PRIMARY KEY CHECK(id=1),destination TEXT NOT NULL,generation INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS search_sessions (id TEXT PRIMARY KEY,project TEXT NOT NULL,harness TEXT NOT NULL,captured INTEGER NOT NULL,replay INTEGER NOT NULL,child INTEGER NOT NULL,title TEXT NOT NULL,projection_hash TEXT NOT NULL,gaps BLOB NOT NULL,repo TEXT NOT NULL,project_name TEXT NOT NULL,session_id TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS search_scope ON search_sessions(project,harness,captured);
 CREATE TABLE IF NOT EXISTS search_passages (id INTEGER PRIMARY KEY,session TEXT NOT NULL,reference BLOB,role TEXT NOT NULL,title_hit INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS search_membership ON search_passages(session);
 CREATE VIRTUAL TABLE IF NOT EXISTS search_fts USING fts5(title,body,tokenize='unicode61 remove_diacritics 0');
 PRAGMA user_version=1;`)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO search_state VALUES(1,?,0)", s.destination); err != nil {
		return err
	}
	var destination string
	if err = tx.QueryRowContext(ctx, "SELECT destination FROM search_state WHERE id=1").Scan(&destination); err != nil {
		return err
	}
	if destination != s.destination {
		return errors.New("search destination/privacy context mismatch; use a separate index")
	}
	return tx.Commit()
}

// Close releases the database. No worker or goroutine is started by Open.
func (s *Store) Close() error { return s.db.Close() }

// WriteStats separates metadata-only updates from passage/FTS mutations.
type WriteStats struct {
	Deleted   int  `json:"Deleted"`
	Inserted  int  `json:"Inserted"`
	Unchanged bool `json:"Unchanged"`
}

// Replace atomically installs a complete observed session projection. This is
// the S1 engine boundary; S2 adds expected-generation work commits around it.
// Exact metadata-only changes leave FTS untouched. Active sources precede
// preserved sources so proven shared native history prefers the active reference.
func (s *Store) Replace(ctx context.Context, metadata archive.Metadata, projections []Projection) (WriteStats, error) {
	stats := WriteStats{}
	passages, gaps, err := s.project(metadata, projections)
	if err != nil {
		return stats, err
	}
	encoded, err := json.Marshal(passages)
	if err != nil {
		return stats, err
	}
	sum := sha256.Sum256(encoded)
	hash := hex.EncodeToString(sum[:])
	gapData, err := json.Marshal(gaps)
	if err != nil {
		return stats, err
	}
	title := archive.DisplayTitle(metadata)
	sessionKey := metadata.Harness.Name + "/" + metadata.SessionID
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return stats, err
	}
	defer func() { _ = tx.Rollback() }()
	var oldHash, oldTitle string
	err = tx.QueryRowContext(ctx, "SELECT projection_hash,title FROM search_sessions WHERE id=?", sessionKey).Scan(&oldHash, &oldTitle)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return stats, err
	}
	rebuild := oldHash != hash || errors.Is(err, sql.ErrNoRows)
	titleChanged := oldTitle != title || errors.Is(err, sql.ErrNoRows)
	deleteRows := func(titleHit int) error {
		var count int
		if e := tx.QueryRowContext(ctx, "SELECT count(*) FROM search_passages WHERE session=? AND title_hit=?", sessionKey, titleHit).Scan(&count); e != nil {
			return e
		}
		stats.Deleted += count
		if _, e := tx.ExecContext(ctx, "DELETE FROM search_fts WHERE rowid IN (SELECT id FROM search_passages WHERE session=? AND title_hit=?)", sessionKey, titleHit); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "DELETE FROM search_passages WHERE session=? AND title_hit=?", sessionKey, titleHit)
		return e
	}
	if rebuild || titleChanged {
		insert, e := tx.PrepareContext(ctx, "INSERT INTO search_passages(session,reference,role,title_hit) VALUES(?,?,?,?)")
		if e != nil {
			return stats, e
		}
		defer func() { _ = insert.Close() }()
		fts, e := tx.PrepareContext(ctx, "INSERT INTO search_fts(rowid,title,body) VALUES(?,?,?)")
		if e != nil {
			return stats, e
		}
		defer func() { _ = fts.Close() }()
		add := func(ref any, role string, titleHit int, title, body string) error {
			result, e := insert.ExecContext(ctx, sessionKey, ref, role, titleHit)
			if e != nil {
				return e
			}
			id, e := result.LastInsertId()
			if e != nil {
				return e
			}
			_, e = fts.ExecContext(ctx, id, title, body)
			if e == nil {
				stats.Inserted++
			}
			return e
		}
		if rebuild {
			if err = deleteRows(0); err != nil {
				return stats, err
			}
			for _, p := range passages {
				ref, e := json.Marshal(p.Reference)
				if e != nil {
					return stats, e
				}
				if err = add(ref, p.Role, 0, "", p.Text); err != nil {
					return stats, err
				}
			}
		}
		if titleChanged {
			if err = deleteRows(1); err != nil {
				return stats, err
			}
			if title != "" {
				if err = add(nil, "", 1, title, ""); err != nil {
					return stats, err
				}
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO search_sessions VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET project=excluded.project,harness=excluded.harness,captured=excluded.captured,replay=excluded.replay,child=excluded.child,title=excluded.title,projection_hash=excluded.projection_hash,gaps=excluded.gaps,repo=excluded.repo,project_name=excluded.project_name,session_id=excluded.session_id`, sessionKey, metadata.ProjectID, metadata.Harness.Name, metadata.CapturedAt.UnixNano(), metadata.Replay != nil, metadata.NativeChild || metadata.ParentSessionID != "", title, hash, gapData, metadata.RepoKey, foldProjectName(metadata.ProjectName), metadata.SessionID)
	if err != nil {
		return stats, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE search_state SET generation=generation+1 WHERE id=1"); err != nil {
		return stats, err
	}
	stats.Unchanged = !rebuild
	return stats, tx.Commit()
}

func (s *Store) project(metadata archive.Metadata, projections []Projection) ([]Passage, []Gap, error) {
	if metadata.SessionID == "" {
		return nil, nil, errors.New("search session identity required")
	}
	var passages []Passage
	totalText := 0
	var gaps []Gap
	seen := map[string]bool{}
	sources := map[string]bool{}
	for _, p := range projections {
		if p.Source.Destination != s.destination || p.Source.SessionID != metadata.SessionID || p.Source.ExtractorVersion != ExtractorVersion {
			return nil, nil, errors.New("search projection belongs to another context")
		}
		if sources[p.Source.Key()] {
			continue
		}
		sources[p.Source.Key()] = true
		if p.Gap != nil {
			gaps = append(gaps, *p.Gap)
			continue
		}
		for _, v := range p.Passages {
			if v.Reference.Source != p.Source || len(v.Text) > passageBytes || v.Reference.End-v.Reference.Start != len(v.Text) {
				return nil, nil, errors.New("invalid search passage")
			}
			if v.Physical != "" {
				digest := sha256.Sum256([]byte(v.Text))
				key := fmt.Sprintf("%s/%d/%d/%s/%x", v.Physical, v.Reference.Start, v.Reference.End, v.Role, digest)
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			if len(passages) >= maxSessionPassages || len(v.Text) > maxSessionBytes-totalText {
				return nil, nil, errors.New("search session projection exceeds passage or 32 MiB limit")
			}
			totalText += len(v.Text)
			passages = append(passages, v)
		}
	}
	return passages, gaps, nil
}

// Observation is query coverage at a known local observation, not a promise of
// immediately current remote archive contents. S3 supplies reconciliation facts.
type Observation struct {
	LastReconciled time.Time `json:"LastReconciled"`
	Pending        int       `json:"Pending"`
	Failed         int       `json:"Failed"`
	Complete       bool      `json:"Complete"`
	Limitations    []string  `json:"Limitations"`
}
