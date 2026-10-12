// Package cursorstore reads Cursor's chat database, state.vscdb, without
// writing to it or creating anything beside it. Backfill's count of
// database-only chats and the collector's cursor-sqlite source both read
// through it.
package cursorstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	// modernc.org/sqlite is a pure-Go SQLite, so builds and tests need no
	// cgo (spec, "Phase 2: Cursor database chats", decision 4).
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Reason says why Cursor's database was not checked.
type Reason string

// Reasons a read of Cursor's database was not done.
const (
	// Locked means a rollback journal shows an unfinished write (which may be
	// a hot journal only Cursor can roll back) or that Cursor held a lock
	// past the busy timeout.
	Locked Reason = "locked"
	// Unreadable means the file is not a database SQLite can open, or its
	// side files are in a state that can't be read without changing them.
	Unreadable Reason = "unreadable"
	// UnknownFormat means the table, or a value read from it, is not a shape
	// this release knows.
	UnknownFormat Reason = "unknown_format"
	// ChangedDuringRead means Cursor wrote the file while it was read in
	// place with Cursor closed.
	ChangedDuringRead Reason = "changed_during_read"
)

// NotCheckedError is a read of Cursor's database that could not be done
// safely, or whose result can't be trusted. Nothing it read is returned.
type NotCheckedError struct {
	Reason Reason
	// Err is the underlying failure, when there is one.
	Err error
}

// Error names only the reason. The underlying error, reachable through
// Unwrap, can name the database's path (a path error from resolving it, or
// SQLite's own message), which never goes into a message that may be
// printed or recorded.
func (e *NotCheckedError) Error() string {
	return fmt.Sprintf("Cursor database not checked (%s)", e.Reason)
}

// Unwrap returns the underlying failure, or nil when there is none. It may
// name the database's path, so it is for errors.Is and errors.As, not for
// display.
func (e *NotCheckedError) Unwrap() error { return e.Err }

// NotChecked returns a *NotCheckedError with reason, for a read callback to
// report a value it does not recognize.
func NotChecked(reason Reason) error { return &NotCheckedError{Reason: reason} }

// ReasonOf is err's Reason: a *NotCheckedError's own, or a SQLite error
// classified. Any other error is Unreadable.
func ReasonOf(err error) Reason {
	var nc *NotCheckedError
	if errors.As(err, &nc) {
		return nc.Reason
	}
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return Locked
		}
	}
	if msg := err.Error(); strings.Contains(msg, "no such table") || strings.Contains(msg, "no such column") {
		return UnknownFormat
	}
	return Unreadable
}

// notChecked wraps err as a *NotCheckedError, keeping one it already is and
// ErrComposerNotFound, which is a checked answer.
func notChecked(err error) error {
	var nc *NotCheckedError
	if errors.As(err, &nc) || errors.Is(err, ErrComposerNotFound) {
		return err
	}
	return &NotCheckedError{Reason: ReasonOf(err), Err: err}
}

// ErrNoDatabase means there is no database at the path: Cursor is not
// installed. It wraps fs.ErrNotExist.
var ErrNoDatabase = fmt.Errorf("no Cursor database: %w", fs.ErrNotExist)

// BusyTimeout is how long a read waits for Cursor's own lock.
const BusyTimeout = 500 * time.Millisecond

// ReadTimeout bounds one whole database read, as far as it can be
// interrupted: a query is cancelled at its deadline, but the online backup's
// single Step(-1) copies the whole database in one uninterruptible call,
// and the deadline is only checked between busy retries around it.
const ReadTimeout = 30 * time.Second

// sideFiles are the files SQLite keeps beside a database.
var sideFiles = []string{"-wal", "-shm", "-journal"}

// Options adjusts a read. The zero value is what production uses.
type Options struct {
	// AfterImmutableRead, when set (by a test), runs after a read with
	// Cursor closed and before the check that the file did not change.
	AfterImmutableRead func(path string)
}

// source is a database resolved and classified for reading.
type source struct {
	// path is the real file, symlinks resolved.
	path string
	// live is set when Cursor has the database open (or left its WAL side
	// files behind): -wal and -shm both exist.
	live bool
	// strayWAL is set when the only side file is an empty -wal, which SQLite
	// can leave when Cursor quits just as a reader opens the database. An
	// empty -wal holds no frames to replay, so the file is read as closed.
	// settled sets it for a live source whose -wal is empty too, with shm
	// recording that its -shm existed.
	strayWAL bool
	// emptyWAL is set on a live source whose -wal was empty: the database
	// file alone held every committed write when it was resolved.
	emptyWAL bool
	// shm is set when a source read as closed (settled) had a -shm file,
	// which the check afterwards then requires still to be there.
	shm bool
	// shmHeader is the -shm's wal-index header (both copies) when a settled
	// source was resolved; the check afterwards requires it unchanged.
	shmHeader []byte
	before    os.FileInfo
	header    []byte
}

// resolve follows link to the real database and decides how it may be read.
// SQLite creates a WAL database's -wal and -shm files when they are missing,
// even for a read-only connection, so there are two ways in:
//
//   - Cursor running: the -wal and -shm files exist (live). The database is
//     opened in place read-only, with the shared-memory index opened
//     read-only too (readonly_shm), so Cursor's live writes are seen and
//     nothing is created.
//   - Cursor closed: no side file exists, so the database is complete in the
//     one file. It is opened with immutable=1, which opens no side file and
//     takes no lock, and afterwards the file must have the same size,
//     modification time, inode, and 100-byte header (which holds SQLite's
//     change counter), with still no side file; otherwise Cursor started and
//     wrote during the read, which immutable=1 could have read torn, and the
//     read is ChangedDuringRead.
//
// A symlinked database is followed first: SQLite keeps the side files beside
// the file the link points to, so they are looked for there.
//
// Anything else (a rollback journal, or one WAL side file without the other)
// is not checked. If Cursor quits between the side-file check and the open,
// SQLite may create a 0-byte -wal beside the database; that race is accepted.
// A 0-byte -wal is harmless to Cursor, and nothing next to the real database
// is ever deleted. A WAL database whose only side file is such an empty -wal
// is read as closed (it has nothing to replay), and the check afterwards
// requires the -wal to be still empty and still the only side file.
//
// A live source whose -wal is empty is marked emptyWAL. Read and the Reader
// still read it as live; ReadRecovery, which never reads a live WAL, reads
// it as closed instead (see settled).
//
// An empty link is a system with no known location for the database
// (the native provider's database location): ErrNoDatabase, as if Cursor were not
// installed.
//
// No error names the path: a path error is kept only for Unwrap.
func resolve(link string) (source, error) {
	if link == "" {
		// No location: Cursor's data is not looked for on this system.
		return source{}, ErrNoDatabase
	}
	path, err := filepath.EvalSymlinks(link)
	if errors.Is(err, os.ErrNotExist) {
		return source{}, ErrNoDatabase
	}
	if err != nil {
		return source{}, &NotCheckedError{Reason: Unreadable, Err: err}
	}
	before, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return source{}, ErrNoDatabase
	}
	if err != nil {
		return source{}, &NotCheckedError{Reason: Unreadable, Err: err}
	}
	if !before.Mode().IsRegular() {
		return source{}, NotChecked(Unreadable)
	}
	header, ok := sqliteHeader(path)
	if !ok {
		return source{}, NotChecked(Unreadable)
	}
	// File format read or write version 2 is WAL mode.
	wal := header[18] == 2 || header[19] == 2
	sides := existingSideFiles(path)
	src := source{path: path, before: before, header: header}
	switch {
	case len(sides) == 0:
	case sides["-journal"]:
		return source{}, NotChecked(Locked)
	case wal && sides["-wal"] && sides["-shm"]:
		src.live = true
		src.emptyWAL = emptyFile(path + "-wal")
	case wal && len(sides) == 1 && emptyFile(path+"-wal"):
		src.strayWAL = true
	default:
		return source{}, NotChecked(Unreadable)
	}
	return src, nil
}

// dsn is the read-only connection string for path: read in place with the
// shared-memory index read-only when live, otherwise immutable.
func dsn(path string, live bool) string {
	query := url.Values{}
	query.Set("mode", "ro")
	if live {
		query.Set("readonly_shm", "1")
	} else {
		query.Set("immutable", "1")
	}
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", BusyTimeout.Milliseconds()))
	query.Add("_pragma", "query_only(1)")
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
}

// settled is a live source whose -wal is empty, to be read as closed: the
// database file alone holds every committed write, so it is opened
// immutable (which ignores the -wal and -shm and takes no lock) and checked
// afterwards as a stray -wal is, with the -shm still required to be there.
// Cursor left such a pair when it quit without removing them (or has the
// database open but has written nothing since its last truncating
// checkpoint); a write that lands during the read fills the -wal, or, when
// checkpointed and truncated, changes the database file, and the check
// afterwards fails. Such a checkpoint can leave the file's size and header
// as they were (a WAL commit changes the header's change counter only when
// it changes page 1), so its modification time is what shows it. A file
// system whose timestamps are coarse (Linux mtime can advance only once per
// kernel tick) can leave that time as it was, so the -shm's wal-index header
// is compared too: every write transaction changes it, and a checkpoint that
// restarts the -wal changes its salts. ok is false for any other source,
// and for one whose -shm header cannot be read.
func (s source) settled() (source, bool) {
	if !s.live || !s.emptyWAL {
		return source{}, false
	}
	header, ok := shmHeader(s.path)
	if !ok {
		return source{}, false
	}
	s.live, s.emptyWAL, s.strayWAL, s.shm, s.shmHeader = false, false, true, true, header
	return s, true
}

// unchanged is the check after an immutable read: the file is the same one,
// with the same size, modification time, and header, and still no side file
// (or, for a stray -wal, still that -wal, still empty, with no journal and a
// -shm only if one was there before, with the same wal-index header when a
// settled source recorded it).
func (s source) unchanged() bool {
	after, err := os.Stat(s.path)
	headerAfter, ok := sqliteHeader(s.path)
	if err != nil || !ok || after.Size() != s.before.Size() || !after.ModTime().Equal(s.before.ModTime()) ||
		!os.SameFile(s.before, after) || !bytes.Equal(s.header, headerAfter) {
		return false
	}
	if s.shmHeader != nil {
		if after, ok := shmHeader(s.path); !ok || !bytes.Equal(s.shmHeader, after) {
			return false
		}
	}
	sides := existingSideFiles(s.path)
	if s.strayWAL {
		return sides["-wal"] && sides["-shm"] == s.shm && !sides["-journal"] && emptyFile(s.path+"-wal")
	}
	return len(sides) == 0
}

// emptyFile reports whether path is a regular file of zero bytes.
func emptyFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() == 0
}

// Read opens the database at path in place, as resolve describes, without
// writing it or creating anything beside it, and calls read with it. A
// missing database is ErrNoDatabase. Every other failure, including one
// read returns, is a *NotCheckedError; with Cursor closed, a file that
// changed during the read is ChangedDuringRead whatever read returned.
//
// Each statement read runs sees a consistent database, but with Cursor
// running two statements may see different states of it; ReadComposer reads
// a snapshot instead.
func Read(ctx context.Context, path string, opts Options, read func(context.Context, *sql.DB) error) error {
	src, err := resolve(path)
	if err != nil {
		return err
	}
	return readInPlace(ctx, src, opts, read)
}

// readInPlace is Read after resolve.
func readInPlace(ctx context.Context, src source, opts Options, read func(context.Context, *sql.DB) error) error {
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	err := query(ctx, dsn(src.path, src.live), read)
	if !src.live {
		if opts.AfterImmutableRead != nil {
			opts.AfterImmutableRead(src.path)
		}
		if !src.unchanged() {
			return NotChecked(ChangedDuringRead)
		}
	}
	if err != nil {
		return notChecked(err)
	}
	return nil
}

// query opens dsn with one connection and calls read with it.
func query(ctx context.Context, dsn string, read func(context.Context, *sql.DB) error) error {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	return read(ctx, db)
}

// sqliteHeader reads the 100-byte database header; ok is false for a file
// that is not a SQLite database.
//
// It opens the file read-only and takes no lock, but closing any descriptor
// of a file releases every POSIX advisory (fcntl) lock this process holds on
// it. It must not run while this process holds a SQLite connection that locks
// the same database; readInPlace checks only after its connection is closed,
// and an immutable connection takes no lock. shmHeader has the same caveat
// for the -shm.
func sqliteHeader(path string) (header []byte, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	header = make([]byte, 100)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, false
	}
	if !bytes.Equal(header[:16], []byte("SQLite format 3\x00")) {
		return nil, false
	}
	return header, true
}

// shmHeaderSize is the -shm's wal-index header: two 48-byte copies of the
// WAL index header, which SQLite updates on every write transaction and
// every restart of the -wal.
const shmHeaderSize = 96

// shmHeader reads the first shmHeaderSize bytes of path's -shm without
// changing it: a regular file (not a link), opened O_RDONLY, with no lock
// taken, no mapping and nothing written. ok is false for a missing, shorter
// or replaced -shm. The same-process lock caveat of sqliteHeader applies.
func shmHeader(path string) (header []byte, ok bool) {
	name := path + "-shm"
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, false
	}
	header = make([]byte, shmHeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, false
	}
	return header, true
}

// existingSideFiles reports which of path's side files exist.
func existingSideFiles(path string) map[string]bool {
	out := map[string]bool{}
	for _, suffix := range sideFiles {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			out[suffix] = true
		}
	}
	return out
}
