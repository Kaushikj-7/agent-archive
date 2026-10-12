package discovery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
	"modernc.org/sqlite/vfs"
)

const indexHintTimeout = 100 * time.Millisecond

// HintBatch is optional bounded scheduling work, not coverage or provenance.
// SQLite supplies scheduling locators only. Its dates, IDs and index presence
// never authorize a source or establish local execution/complete coverage.
// Codex may put SQLite elsewhere; unknown locations simply use enumeration.
type HintBatch struct {
	Entries  []SourceEntry
	Queries  int
	Bytes    int64
	Locators int
	Rejected int
	Outcome  Outcome
	// refusal is transient diagnostic evidence for synthetic tests. It is never
	// persisted in health or used to authorize, schedule or retry a source.
	refusal error
}

// HintAdapter supplies optional native index hints without admission policy.
type HintAdapter interface {
	Hints(context.Context, string) HintBatch
}

func (s scan) observeIndexHints(ctx context.Context, o Options, roots []string, deadline time.Time) {
	adapter, ok := s.adapter.(HintAdapter)
	if !ok {
		return
	}
	for _, root := range roots {
		if scanStopped(ctx, o) || time.Now().After(deadline) || s.health.Probes >= 64 || s.health.Entries >= 1024 {
			return
		}
		batch := adapter.Hints(ctx, root)
		s.health.IndexQueries += batch.Queries
		s.health.IndexBytes += batch.Bytes
		s.health.IndexLocators += batch.Locators
		s.health.Outcomes["index_locator_rejected"] += batch.Rejected
		if batch.Outcome != "" {
			s.health.Outcomes[string(batch.Outcome)]++
			continue
		}
		for _, source := range batch.Entries {
			if scanStopped(ctx, o) || time.Now().After(deadline) || s.health.Probes >= 64 || s.health.Entries >= 1024 {
				return
			}
			s.visitEntry(directory{Root: root}, source)
		}
	}
}

func (a codexAdapter) Hints(ctx context.Context, root string) HintBatch {
	hints, queries, bytes, outcome, refusal := indexHintsDetailed(ctx, root)
	batch := HintBatch{Queries: queries, Bytes: bytes, Locators: len(hints), Outcome: Outcome(outcome), refusal: refusal}
	if outcome != "" {
		return batch
	}
	for _, path := range hints {
		if !filepath.IsAbs(path) || len(path) > 4096 || filepath.Clean(path) != path || (!local.PathWithin(path, filepath.Join(root, "sessions")) && !local.PathWithin(path, filepath.Join(root, "archived_sessions"))) {
			batch.Rejected++
			continue
		}
		relative, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			batch.Rejected++
			continue
		}
		batch.Entries = append(batch.Entries, a.Describe(root, relative, filepath.Base(path)))
	}
	return batch
}

// indexHintsDetailed returns the original attempt's refusal for synthetic
// diagnostics. The bounded read, metrics, cleanup and public outcome stay the
// same; diagnostic consumers must not issue a second read to infer its cause.
func indexHintsDetailed(ctx context.Context, root string) ([]string, int, int64, string, error) {
	path := filepath.Join(root, "state_5.sqlite")
	// Only settled DBs are useful through the read-only Go VFS. Live WAL
	// requires shared-memory operations that can race into native side writes;
	// skip it rather than dropping WAL updates or changing Codex's files.
	before, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, 0, "index_hints_missing", err
	}
	if err != nil || !before.Mode().IsRegular() || indexHasSides(path) {
		if err == nil {
			err = errors.New("index is not a settled regular file")
		}
		return nil, 0, 0, "index_hints_unavailable", err
	}
	ctx, cancel := context.WithTimeout(ctx, indexHintTimeout)
	defer cancel()
	var hints []string
	queries := 0
	fsys := &indexFS{root: root, ctx: ctx}
	name, registered, err := vfs.New(fsys)
	if err != nil {
		return nil, 0, 0, "index_hints_unavailable", err
	}
	defer func() { _ = registered.Close() }()
	parameters := url.Values{"vfs": {name}, "mode": {"ro"}, "immutable": {"1"}}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Opaque: "state_5.sqlite", RawQuery: parameters.Encode()}).String())
	if err != nil {
		return nil, 0, 0, "index_hints_unavailable", err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	err = func() error {
		conn, err := db.Conn(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		// Bound malformed database/schema values before preparing any statement.
		for _, limit := range []struct {
			id    int
			value int
		}{{sqlite3.SQLITE_LIMIT_LENGTH, 1 << 20}, {sqlite3.SQLITE_LIMIT_SQL_LENGTH, 16384}, {sqlite3.SQLITE_LIMIT_ATTACHED, 0}, {sqlite3.SQLITE_LIMIT_VDBE_OP, 20000}} {
			if _, err := sqlite.Limit(conn, limit.id, limit.value); err != nil {
				return err
			}
		}
		for _, ordered := range []struct {
			index string
			query string
		}{
			{"idx_threads_created_at_ms", "SELECT substr(rollout_path,1,4097) FROM threads INDEXED BY idx_threads_created_at_ms ORDER BY created_at_ms DESC,id DESC LIMIT 32"},
			{"idx_threads_updated_at_ms", "SELECT substr(rollout_path,1,4097) FROM threads INDEXED BY idx_threads_updated_at_ms ORDER BY updated_at_ms DESC,id DESC LIMIT 32"},
		} {
			// Constants only: INDEXED BY fails closed on a missing index. EXPLAIN
			// additionally refuses a same-named incompatible index requiring sorting.
			query := ordered.query
			queries++
			if err := hintPlan(ctx, conn, query, ordered.index); err != nil {
				return fmt.Errorf("%s EXPLAIN: %w", ordered.index, err)
			}
			queries++
			hints, err = readIndexLocators(ctx, conn, query, hints)
			if err != nil {
				return fmt.Errorf("%s locators: %w", ordered.index, err)
			}
		}
		return nil
	}()
	after, statErr := os.Lstat(path)
	if err != nil || statErr != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || indexHasSides(path) {
		if err == nil {
			err = statErr
		}
		if err == nil {
			err = errors.New("index changed during hint read")
		}
		return nil, queries, fsys.bytes, "index_hints_unavailable", err
	}
	return hints, queries, fsys.bytes, "", nil
}

func readIndexLocators(ctx context.Context, conn *sql.Conn, query string, hints []string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		if len(path) <= 4096 {
			hints = appendUnique(hints, path)
		}
	}
	return hints, rows.Err()
}

func hintPlan(ctx context.Context, conn *sql.Conn, query, index string) error {
	rows, err := conn.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	ordered := false
	count := 0
	for rows.Next() {
		count++
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			return err
		}
		if count > 16 || len(detail) > 2048 || strings.Contains(detail, "TEMP B-TREE") {
			return errors.New("unordered scheduling index")
		}
		if strings.Contains(detail, "USING INDEX "+index) {
			ordered = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !ordered {
		return errors.New("unsupported scheduling index")
	}
	return nil
}

// No VFS write or side-file operation reaches the native filesystem. The Go
// filesystem opens only the expected confined regular DB, never arbitrary
// SQLite filenames. Reads are context-aware and have a separate page-I/O cap.
type indexFS struct {
	root  string
	ctx   context.Context
	bytes int64
}

func (f *indexFS) Open(name string) (fs.File, error) {
	if name != "state_5.sqlite" {
		return nil, fs.ErrNotExist
	}
	opened, err := sourcefacts.OpenRegular(f.root, filepath.Join(f.root, name))
	if err != nil {
		return nil, err
	}
	return &indexFile{File: opened, owner: f}, nil
}

type indexFile struct {
	*os.File
	owner *indexFS
}

func (f *indexFile) Read(p []byte) (int, error) {
	if err := f.owner.ctx.Err(); err != nil {
		return 0, err
	}
	const maxIndexBytes = 4 << 20
	if f.owner.bytes >= maxIndexBytes {
		return 0, errors.New("scheduling read budget exceeded")
	}
	if int64(len(p)) > maxIndexBytes-f.owner.bytes {
		p = p[:maxIndexBytes-f.owner.bytes]
	}
	n, err := f.File.Read(p)
	f.owner.bytes += int64(n)
	return n, err
}

func indexHasSides(path string) bool {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}
