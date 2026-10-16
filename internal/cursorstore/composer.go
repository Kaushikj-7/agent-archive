package cursorstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/sourceidentity"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Composer is one Cursor chat as read from state.vscdb: its composerData
// value and its messages in header order. A message whose row is missing has
// a nil Value. Its shape is archive.CursorComposer's, field for field.
type Composer struct {
	Composer json.RawMessage
	Bubbles  []Bubble
}

// Bubble is one message of a chat: its bubble ID and its bubbleId row's
// value, nil when the row is missing.
type Bubble struct {
	ID      string
	Value   json.RawMessage
	Missing bool
}

// Signature is what tells one state of a chat from the next without reading
// all its messages (spec, phase 2 decision 7): the database file changes
// constantly, so its stat can't. It is comparable with ==.
type Signature = sourceidentity.CursorSignature

// ErrComposerNotFound means the database has no composerData row for the
// chat: Cursor deleted it. It wraps fs.ErrNotExist.
var ErrComposerNotFound = fmt.Errorf("no such Cursor chat: %w", fs.ErrNotExist)

// Reader reads chats from one Cursor database, taking at most one snapshot
// of it however many chats it reads: the first chat read while Cursor runs
// copies the database, and every later one reads that copy. A collector pass
// holds one Reader and closes it when the pass ends, which removes the copy.
// A Reader is not safe for concurrent use.
type Reader struct {
	dbPath string
	// snapDir is the private directory holding the copy, "" until one is
	// taken; copyPath is the copy, "" until it is complete.
	snapDir  string
	copyPath string
	// lock holds the snapshot directory's lock while the copy is in use,
	// so no sweep removes it (see snapshotLockName).
	lock *os.File
	// snapErr is why the one snapshot attempt failed. It is not retried:
	// every later chat read through this Reader, for the rest of the pass,
	// fails with it without trying to copy the database again.
	snapErr error
	// hooks are test observation points; production Readers have none.
	hooks     readerHooks
	snapshots int
	attempts  int
	swept     bool
}

// NewReader returns a Reader for the database at dbPath. Nothing is opened
// until a chat is read.
func NewReader(dbPath string) *Reader { return &Reader{dbPath: dbPath} }

// Snapshots is how many copies of the database this Reader took: 0 or 1.
func (r *Reader) Snapshots() int { return r.snapshots }

// Close removes the Reader's snapshot, if it took one. It is safe to call
// more than once, and the Reader may be used again afterwards.
func (r *Reader) Close() error {
	dir, lock := r.snapDir, r.lock
	r.snapDir, r.copyPath, r.snapErr, r.lock = "", "", nil, nil
	if dir == "" {
		return nil
	}
	defer untrackSnapshot(dir)
	remove := os.RemoveAll
	if r.hooks.removeSnapshot != nil {
		remove = r.hooks.removeSnapshot
	}
	err := remove(dir)
	if lock != nil {
		// Released only after the copy is gone.
		err = errors.Join(err, lock.Close())
	}
	if err != nil {
		return fmt.Errorf("remove the Cursor database snapshot: %w", err)
	}
	return nil
}

// ReadComposer reads one chat consistently: its composerData row and its
// bubbleId:<composerID>:<bubbleID> rows, in header order.
//
// With Cursor running, the chat is read from the Reader's snapshot, taken
// on first use with SQLite's online backup API (spec, phase 2 decision 3):
// from a read-only connection opened as Read opens it, into a 0600 file in a
// new private directory under SnapshotRoot. The backup is one read
// transaction over the whole database, so the copy is one committed state
// even while Cursor writes; a lock held past the timeout fails the read
// rather than reading partially. With Cursor closed, the backup would
// create -wal beside the source, so the chat is read in place as Read does,
// immutable and checked afterwards, and nothing is copied.
//
// A missing database is ErrNoDatabase and a missing chat
// ErrComposerNotFound; both wrap fs.ErrNotExist. Anything else that can't be
// read safely is a *NotCheckedError.
func (r *Reader) ReadComposer(ctx context.Context, composerID string) (Composer, Signature, error) {
	return r.ReadComposerLimited(ctx, composerID, 0, 0)
}

// ReadComposerLimited checks native value lengths before allocating transcript values.
func (r *Reader) ReadComposerLimited(ctx context.Context, composerID string, rawLimit, recordLimit int64) (Composer, Signature, error) {
	if composerID == "" {
		return Composer{}, Signature{}, errors.New("a Cursor composer ID is required")
	}
	if !r.swept {
		r.swept = true
		RemoveStaleSnapshots()
	}
	var c Composer
	var sig Signature
	read := func(ctx context.Context, db *sql.DB) error {
		var err error
		if rawLimit > 0 || recordLimit > 0 {
			c, sig, err = queryComposerLimited(ctx, db, composerID, rawLimit, recordLimit)
		} else {
			c, sig, err = queryComposer(ctx, db, composerID)
		}
		return err
	}
	var err error
	switch {
	case r.copyPath != "":
		err = r.readCopy(ctx, read)
	case r.snapErr != nil:
		err = r.snapErr
	default:
		var src source
		if src, err = resolve(r.dbPath); err != nil {
			break
		}
		if !src.live {
			err = readInPlace(ctx, src, Options{}, read)
			break
		}
		r.attempts++
		if err = r.snapshot(ctx, src); err != nil {
			r.snapErr = err
			break
		}
		err = r.readCopy(ctx, read)
	}
	if err != nil {
		return Composer{}, Signature{}, err
	}
	return c, sig, nil
}

// ReadSignature reads the chat's Signature in place, as Read does, inside
// one read transaction: the composerData row, which of its messages have
// rows, and the last message's row. It costs a few indexed rows, never a
// copy of the database, and agrees with what ReadComposer returns for the
// same state.
func ReadSignature(ctx context.Context, dbPath, composerID string) (Signature, error) {
	if composerID == "" {
		return Signature{}, errors.New("a Cursor composer ID is required")
	}
	var sig Signature
	err := Read(ctx, dbPath, Options{}, func(ctx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }() // a read-only transaction; nothing to undo
		value, oversized, err := signatureComposerRow(ctx, tx, composerID, archive.MaxRecordBytes)
		if err != nil {
			return err
		}
		if oversized {
			sig, err = signatureOnly(ctx, tx, composerID)
			return err
		}
		var ids []string
		if sig, ids, err = decodeHeaders(value); err != nil || len(ids) == 0 {
			return err
		}
		sig, err = signatureRows(ctx, tx, composerID, sig, ids)
		return err
	})
	if err != nil {
		return Signature{}, err
	}
	return sig, nil
}

// ReadLastUpdated reads each chat's lastUpdatedAt (Unix milliseconds, the
// Signature's LastUpdatedAt) in one read of the database, in place as Read
// does, so ordering many chats by activity opens it once. Only composerData
// rows are read. A chat that is missing, unreadable, or has no positive
// lastUpdatedAt is left out.
func ReadLastUpdated(ctx context.Context, dbPath string, composerIDs []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(composerIDs) == 0 {
		return out, nil
	}
	err := Read(ctx, dbPath, Options{}, func(ctx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }() // a read-only transaction; nothing to undo
		for _, id := range composerIDs {
			if id == "" {
				continue
			}
			value, oversized, err := signatureComposerRow(ctx, tx, id, archive.MaxRecordBytes)
			if errors.Is(err, ErrComposerNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if oversized {
				sig, err := signatureOnly(ctx, tx, id)
				if err == nil && sig.LastUpdatedAt > 0 {
					out[id] = sig.LastUpdatedAt
				}
				continue
			}
			if sig, _, err := decodeHeaders(value); err == nil && sig.LastUpdatedAt > 0 {
				out[id] = sig.LastUpdatedAt
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ComposerRow is one chat's composerData value as ReadComposerRows found
// it. Value is nil when the row is larger than the caller's limit, which
// Oversized then reports.
type ComposerRow struct {
	Value     []byte
	Oversized bool
}

// ReadComposerRows reads each chat's composerData row, and no message row,
// in one read transaction in place, as Read does, so every row comes from
// the same state of the database. A chat with no row is left out; a row
// larger than limit bytes is reported Oversized without being loaded.
func ReadComposerRows(ctx context.Context, dbPath string, composerIDs []string, limit int64) (map[string]ComposerRow, error) {
	out := map[string]ComposerRow{}
	if len(composerIDs) == 0 {
		return out, nil
	}
	err := Read(ctx, dbPath, Options{}, func(ctx context.Context, db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }() // a read-only transaction; nothing to undo
		for _, id := range composerIDs {
			if id == "" {
				continue
			}
			value, oversized, err := signatureComposerRow(ctx, tx, id, limit)
			if errors.Is(err, ErrComposerNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if oversized {
				value = nil
			}
			out[id] = ComposerRow{Value: value, Oversized: oversized}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// signatureComposerRow keeps oversized inline content in SQLite. The ordinary
// branch returns the original value and preserves the legacy decoder semantics.
func signatureComposerRow(ctx context.Context, q querier, id string, limit int64) ([]byte, bool, error) {
	var value []byte
	var size sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(value AS BLOB)) <= ? THEN value END,length(CAST(value AS BLOB)) FROM cursorDiskKV WHERE key = ?`, limit, "composerData:"+id).Scan(&value, &size)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !size.Valid {
		return nil, false, ErrComposerNotFound
	}
	return value, size.Valid && size.Int64 > limit, err
}

// readCopy reads the Reader's snapshot. The copy is this process's own and
// nothing writes it, so immutable is exact, and it opens no side file even
// though the copy's header still says WAL.
func (r *Reader) readCopy(ctx context.Context, read func(context.Context, *sql.DB) error) error {
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	if err := query(ctx, dsn(r.copyPath, false), read); err != nil {
		return notChecked(err)
	}
	return nil
}

// readerHooks let a test watch a Reader at work.
type readerHooks struct {
	removeSnapshot func(string) error
	// afterSnapshot runs once the copy is written and before it is read.
	afterSnapshot func(copyPath string)
	// backupRetried runs before each busy retry of the backup.
	backupRetried func()
	// lockPlaced runs once the snapshot's lock file is in place, before the
	// copy is written.
	lockPlaced func(lockPath string)
}

// snapshot copies the live database src into a new private directory under
// SnapshotRoot. The directory is recorded before anything is written into
// it, so Close removes whatever a failure, or a panic, left.
func (r *Reader) snapshot(ctx context.Context, src source) error {
	root, err := SnapshotRoot()
	if err != nil {
		return err
	}
	// MkdirTemp creates the directory 0700.
	dir, err := os.MkdirTemp(root, snapshotPrefix)
	if err != nil {
		return errors.New("create a Cursor database snapshot directory")
	}
	r.snapDir = dir
	trackSnapshot(dir)
	if r.lock, err = lockSnapshot(dir, r.hooks.lockPlaced); err != nil {
		return errors.New("lock a Cursor database snapshot directory")
	}
	copyPath := filepath.Join(dir, "state.vscdb")
	// Created here, empty and 0600, so SQLite opens it rather than creating
	// it with the default mode.
	f, err := os.OpenFile(copyPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create a Cursor database snapshot file")
	}
	if err := f.Close(); err != nil {
		return errors.New("create a Cursor database snapshot file")
	}
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	r.snapshots++
	if err := backup(ctx, dsn(src.path, true), copyPath, r.hooks.backupRetried); err != nil {
		return errors.Join(notChecked(err), agentapi.Wrap(agentapi.Cleanup, r.Close()))
	}
	r.copyPath = copyPath
	if r.hooks.afterSnapshot != nil {
		r.hooks.afterSnapshot(copyPath)
	}
	return nil
}

// backuper is the modernc.org/sqlite connection's online backup API.
type backuper interface {
	NewBackup(dstURI string) (*sqlite.Backup, error)
}

// backupRetry is how long backup waits before retrying a busy source.
const backupRetry = 25 * time.Millisecond

// errBackupIncomplete is a Step(-1) that reports pages left to copy, which
// it should never do.
var errBackupIncomplete = errors.New("the Cursor database backup stopped before the last page")

// backup copies the database srcDSN opens into the file at dst with SQLite's
// online backup API, in one Step(-1) so the copy is one read transaction's
// state. That step can't be interrupted; SQLITE_BUSY and SQLITE_LOCKED are
// retried until ctx ends; retried, when set, runs before each retry.
func backup(ctx context.Context, srcDSN, dst string, retried func()) error {
	db, err := sql.Open("sqlite", srcDSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	// Opened read-write: the copy's own rollback journal, if SQLite makes
	// one, lands in the private directory beside it.
	dstURI := (&url.URL{Scheme: "file", Path: dst}).String()
	return conn.Raw(func(driverConn any) error {
		b, ok := driverConn.(backuper)
		if !ok {
			return errors.New("the SQLite driver has no online backup API")
		}
		for {
			bk, err := b.NewBackup(dstURI)
			if err != nil {
				return err
			}
			more, stepErr := bk.Step(-1)
			finishErr := bk.Finish()
			switch {
			case stepErr == nil && more:
				return errBackupIncomplete
			case stepErr == nil:
				return finishErr
			case !busy(stepErr):
				return stepErr
			}
			if retried != nil {
				retried()
			}
			select {
			case <-ctx.Done():
				return &NotCheckedError{Reason: Locked, Err: stepErr}
			case <-time.After(backupRetry):
			}
		}
	})
}

func busy(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		switch serr.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		}
	}
	return false
}

// querier is a database or a transaction.
type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// composerRow reads the chat's composerData value.
func composerRow(ctx context.Context, db querier, composerID string) ([]byte, error) {
	var value []byte
	err := db.QueryRowContext(ctx, `SELECT value FROM cursorDiskKV WHERE key = ?`, "composerData:"+composerID).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && value == nil) {
		return nil, ErrComposerNotFound
	}
	return value, err
}

// bubbleQuery reads one chat's message rows through the key index, between
// the prefix bubbleId:<composerID>: and bubbleUpper of it, which bracket
// exactly the keys with the prefix. bubbleKeyQuery reads only their keys,
// from the key index alone (a covering index), so a signature costs no read
// of the rows themselves however long the chat.
const (
	bubbleQuery    = `SELECT key, value FROM cursorDiskKV WHERE key >= ? AND key < ?`
	bubbleKeyQuery = `SELECT key FROM cursorDiskKV WHERE key >= ? AND key < ?`
)

func bubblePrefix(composerID string) string { return "bubbleId:" + composerID + ":" }

// bubbleUpper is prefix with its final ':' raised to ';'.
func bubbleUpper(prefix string) string { return prefix[:len(prefix)-1] + ";" }

// messageHash identifies a message row's value; "" for a missing row.
func messageHash(value []byte) string {
	if value == nil {
		return ""
	}
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:16])
}

// queryComposer reads the chat's composerData row, then its message rows.
// It runs on a snapshot or an unchanged immutable file, so the two
// statements see the same state.
func queryComposer(ctx context.Context, db *sql.DB, composerID string) (Composer, Signature, error) {
	value, err := composerRow(ctx, db, composerID)
	if err != nil {
		return Composer{}, Signature{}, err
	}
	sig, ids, err := decodeHeaders(value)
	if err != nil {
		return Composer{}, Signature{}, err
	}
	c := Composer{Composer: json.RawMessage(value), Bubbles: make([]Bubble, len(ids))}
	if len(ids) == 0 {
		return c, sig, nil
	}
	prefix := bubblePrefix(composerID)
	rows, err := db.QueryContext(ctx, bubbleQuery, prefix, bubbleUpper(prefix))
	if err != nil {
		return Composer{}, Signature{}, err
	}
	defer func() { _ = rows.Close() }()
	found := map[string]json.RawMessage{}
	keys := map[string]bool{}
	for rows.Next() {
		var key string
		var v []byte
		if err := rows.Scan(&key, &v); err != nil {
			return Composer{}, Signature{}, err
		}
		id := strings.TrimPrefix(key, prefix)
		keys[id] = true
		if v != nil {
			found[id] = v
		}
	}
	if err := rows.Err(); err != nil {
		return Composer{}, Signature{}, err
	}
	for i, id := range ids {
		c.Bubbles[i] = Bubble{ID: id, Value: found[id]}
		if keys[id] {
			sig.MessageRows++
		}
	}
	sig.LastMessageHash = messageHash(c.Bubbles[len(ids)-1].Value)
	return c, sig, nil
}

// decodeHeaders reads the chat's Signature, apart from its message rows, and
// its message IDs in order from its composerData value:
// fullConversationHeadersOnly, or in older chats the inline conversation. An
// older chat's messages are inline, so it lists no message rows to read. A
// value that is not an object, or a message list or ID of another shape, is
// UnknownFormat.
func decodeHeaders(value []byte) (Signature, []string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(value, &fields); err != nil || fields == nil {
		return Signature{}, nil, NotChecked(UnknownFormat)
	}
	present := func(name string) (json.RawMessage, bool) {
		raw, ok := fields[name]
		return raw, ok && string(raw) != "null"
	}
	var lastUpdatedAt int64
	if raw, ok := present("lastUpdatedAt"); ok {
		var ms float64
		if json.Unmarshal(raw, &ms) != nil {
			return Signature{}, nil, NotChecked(UnknownFormat)
		}
		lastUpdatedAt = int64(ms)
	}
	type header struct {
		BubbleID *string `json:"bubbleId"`
	}
	var ids []string
	if raw, ok := present("fullConversationHeadersOnly"); ok {
		var headers []header
		if json.Unmarshal(raw, &headers) != nil {
			return Signature{}, nil, NotChecked(UnknownFormat)
		}
		for _, h := range headers {
			if h.BubbleID == nil || *h.BubbleID == "" {
				return Signature{}, nil, NotChecked(UnknownFormat)
			}
			ids = append(ids, *h.BubbleID)
		}
		var lastBubbleID string
		if len(ids) > 0 {
			lastBubbleID = ids[len(ids)-1]
		}
		return Signature{LastUpdatedAt: lastUpdatedAt, HeaderCount: len(ids), LastBubbleID: lastBubbleID}, ids, nil
	}
	if raw, ok := present("conversation"); ok {
		var inline []header
		if json.Unmarshal(raw, &inline) != nil {
			return Signature{}, nil, NotChecked(UnknownFormat)
		}
		var lastBubbleID string
		if n := len(inline); n > 0 && inline[n-1].BubbleID != nil {
			lastBubbleID = *inline[n-1].BubbleID
		}
		return Signature{LastUpdatedAt: lastUpdatedAt, HeaderCount: len(inline), LastBubbleID: lastBubbleID}, nil, nil
	}
	return Signature{LastUpdatedAt: lastUpdatedAt}, nil, nil
}

// queryComposerLimited preserves header order without first loading every bubble value.
func queryComposerLimited(ctx context.Context, db *sql.DB, id string, rawLimit, recordLimit int64) (Composer, Signature, error) {
	check := func(n int64) error {
		if recordLimit > 0 && n > recordLimit {
			return agentapi.Wrap(agentapi.Limit, ErrRecordLimit)
		}
		if rawLimit > 0 && n > rawLimit {
			return agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
		}
		return nil
	}
	var observedSize sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT length(CAST(value AS BLOB)) FROM cursorDiskKV WHERE key = ?`, "composerData:"+id).Scan(&observedSize)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !observedSize.Valid {
		return Composer{}, Signature{}, ErrComposerNotFound
	}
	if err != nil {
		return Composer{}, Signature{}, err
	}
	if err = check(observedSize.Int64); err != nil {
		sig, sigErr := signatureOnly(ctx, db, id)
		if sigErr != nil {
			return Composer{}, Signature{}, sigErr
		}
		return limitedFailure(sig, err)
	}
	value, err := composerRow(ctx, db, id)
	if err != nil {
		return Composer{}, Signature{}, err
	}
	sig, ids, err := decodeHeaders(value)
	if err != nil {
		return Composer{}, Signature{}, err
	}
	lengths, err := bubbleLengths(ctx, db, id)
	if err != nil {
		return Composer{}, Signature{}, err
	}
	total := observedSize.Int64
	for _, key := range ids {
		n, present := lengths[key]
		if !present {
			continue
		}
		sig.MessageRows++
		if err = check(n); err != nil {
			sig, err2 := signatureRows(ctx, db, id, Signature{LastUpdatedAt: sig.LastUpdatedAt, HeaderCount: sig.HeaderCount, LastBubbleID: sig.LastBubbleID}, ids)
			if err2 != nil {
				return Composer{}, Signature{}, err2
			}
			return limitedFailure(sig, err)
		}
		if rawLimit > 0 && n > rawLimit-total {
			sig, err2 := signatureRows(ctx, db, id, Signature{LastUpdatedAt: sig.LastUpdatedAt, HeaderCount: sig.HeaderCount, LastBubbleID: sig.LastBubbleID}, ids)
			if err2 != nil {
				return Composer{}, Signature{}, err2
			}
			return limitedFailure(sig, agentapi.ErrRawLimit)
		}
		total += n
	}
	found, err := listedBubbles(ctx, db, id, ids, lengths)
	if err != nil {
		return Composer{}, Signature{}, err
	}
	c := Composer{Composer: value, Bubbles: make([]Bubble, len(ids))}
	for i, key := range ids {
		_, present := lengths[key]
		c.Bubbles[i] = Bubble{ID: key, Value: found[key], Missing: !present}
	}
	if len(ids) > 0 {
		sig.LastMessageHash = messageHash(c.Bubbles[len(ids)-1].Value)
	}
	return c, sig, nil
}

func limitedFailure(sig Signature, err error) (Composer, Signature, error) {
	o := agentapi.SourceObservation{Signature: sig.SourceSignature(), Present: true, Empty: sig.HeaderCount == 0, Activity: time.UnixMilli(sig.LastUpdatedAt)}
	return Composer{}, sig, &agentapi.SourceError{Kind: agentapi.Limit, Err: err, Observed: &o}
}

func bubbleLengths(ctx context.Context, q querier, id string) (out map[string]int64, err error) {
	prefix := bubblePrefix(id)
	rows, err := q.QueryContext(ctx, `SELECT key,length(CAST(value AS BLOB)) FROM cursorDiskKV WHERE key >= ? AND key < ?`, prefix, bubbleUpper(prefix))
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	out = map[string]int64{}
	for rows.Next() {
		var key string
		var n sql.NullInt64
		if err = rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		out[strings.TrimPrefix(key, prefix)] = n.Int64
	}
	return out, rows.Err()
}

func listedBubbles(ctx context.Context, q querier, id string, ids []string, present map[string]int64) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	seen := map[string]bool{}
	var keys []string
	prefix := bubblePrefix(id)
	for _, key := range ids {
		if _, exists := present[key]; exists && !seen[key] {
			seen[key] = true
			keys = append(keys, prefix+key)
		}
	}
	const batchSize = 128
	for start := 0; start < len(keys); start += batchSize {
		end := min(start+batchSize, len(keys))
		args := make([]any, end-start)
		for i, key := range keys[start:end] {
			args[i] = key
		}
		query := `SELECT key,value FROM cursorDiskKV WHERE key IN (` + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + `)`
		if err := readListedBubbles(ctx, q, query, args, prefix, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func readListedBubbles(ctx context.Context, q querier, query string, args []any, prefix string, out map[string]json.RawMessage) (err error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var key string
		var value []byte
		if err = rows.Scan(&key, &value); err != nil {
			return err
		}
		out[strings.TrimPrefix(key, prefix)] = value
	}
	return rows.Err()
}

// ErrRecordLimit marks a native value larger than the read policy permits.
var ErrRecordLimit = errors.New("cursor record exceeds source record limit")

// Attempts counts snapshot preparation attempts, including failures before backup.
func (r *Reader) Attempts() int { return r.attempts }

func signatureRows(ctx context.Context, q querier, id string, sig Signature, ids []string) (out Signature, err error) {
	if len(ids) == 0 {
		return sig, nil
	}
	prefix := bubblePrefix(id)
	rows, err := q.QueryContext(ctx, bubbleKeyQuery, prefix, bubbleUpper(prefix))
	if err != nil {
		return Signature{}, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	present := map[string]bool{}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return Signature{}, err
		}
		present[strings.TrimPrefix(key, prefix)] = true
	}
	err = rows.Err()
	if err != nil {
		return Signature{}, err
	}
	for _, id := range ids {
		if present[id] {
			sig.MessageRows++
		}
	}
	sig.LastMessageHash, err = messageHashAt(ctx, q, prefix+ids[len(ids)-1])
	return sig, err
}

func messageHashAt(ctx context.Context, q querier, key string) (string, error) {
	const chunkSize = 64 * 1024
	var chunk []byte
	var size sql.NullInt64
	var null bool
	var kind sqliteValueKind
	err := q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(value) IN ('blob','text') THEN substr(CAST(value AS BLOB),1,?) ELSE value END,length(CAST(value AS BLOB)),value IS NULL,typeof(value) FROM cursorDiskKV WHERE key = ?`, chunkSize, key).Scan(&chunk, &size, &null, &kind)
	if errors.Is(err, sql.ErrNoRows) || err == nil && null {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if kind != sqliteBlob && kind != sqliteText {
		return messageHash(chunk), nil
	}
	h := sha256.New()
	_, _ = h.Write(chunk)
	for offset := int64(len(chunk)); offset < size.Int64; offset += int64(len(chunk)) {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		if err = q.QueryRowContext(ctx, `SELECT substr(CAST(value AS BLOB),?,?) FROM cursorDiskKV WHERE key = ?`, offset+1, chunkSize, key).Scan(&chunk); err != nil {
			return "", err
		}
		if len(chunk) == 0 {
			return "", NotChecked(ChangedDuringRead)
		}
		_, _ = h.Write(chunk)
	}
	return hex.EncodeToString(h.Sum(nil)[:16]), nil
}

// signatureOnly visits the composer once without returning unknown native content.
// A statement-local JSON tree retains only typed metadata and direct identities;
// separate json_each statements would parse a huge omitted value repeatedly.
func signatureOnly(ctx context.Context, q querier, id string) (Signature, error) {
	key := "composerData:" + id
	rows, err := q.QueryContext(ctx, signatureNodeQuery, key)
	if err != nil {
		return Signature{}, err
	}
	defer func() { _ = rows.Close() }()
	var found, valid bool
	var root sqliteValueKind
	var timestamp int64
	var timestampErr error
	var headers signatureTreeHeaders
	inline := signatureTreeHeaders{inline: true}
	for rows.Next() {
		var present bool
		var index, parent sql.NullInt64
		var key any
		var kind sql.NullString
		var scalar any
		if err := rows.Scan(&present, &index, &parent, &key, &kind, &scalar); err != nil {
			return Signature{}, err
		}
		found = present
		if !parent.Valid {
			valid = kind.Valid
			root = sqliteValueKind(kind.String)
			continue
		}
		typ := sqliteValueKind(kind.String)
		if parent.Int64 == 0 {
			switch key {
			case "lastUpdatedAt":
				timestamp, timestampErr = signatureTimestamp(typ, scalar)
			case "fullConversationHeadersOnly":
				headers = newSignatureTreeHeaders(index.Int64, typ, false)
			case "conversation":
				inline = newSignatureTreeHeaders(index.Int64, typ, true)
			}
			continue
		}
		headers.visit(index.Int64, parent.Int64, typ, scalar)
		inline.visit(index.Int64, parent.Int64, typ, scalar)
	}
	if err := rows.Err(); err != nil {
		return Signature{}, err
	}
	// Release the SQL row owner before fallback or bubble-row queries on a
	// serial connection. Unknown JSON must still be fully validated.
	if err := rows.Close(); err != nil {
		return Signature{}, err
	}
	if !found {
		return Signature{}, ErrComposerNotFound
	}
	if !valid {
		return signatureJSONFallback(ctx, q, key, id)
	}
	if root != sqliteObject || timestampErr != nil {
		return Signature{}, NotChecked(UnknownFormat)
	}
	selected, name := &headers, "fullConversationHeadersOnly"
	if !headers.present {
		selected, name = &inline, "conversation"
	}
	selected.finish()
	if selected.err != nil {
		return Signature{}, NotChecked(UnknownFormat)
	}
	sig := Signature{LastUpdatedAt: timestamp, HeaderCount: selected.count, LastBubbleID: selected.last}
	// SQL's invalid UTF-8/surrogate decoding is repaired only for affected
	// identities, using bounded raw reads and the selected array ordinal.
	for _, ordinal := range selected.repairs {
		identity, err := rawHeaderIdentity(ctx, q, key, name, ordinal)
		if err != nil {
			return Signature{}, err
		}
		if !selected.inline {
			selected.ids[ordinal] = identity
		}
		if ordinal == int64(selected.count-1) {
			sig.LastBubbleID = identity
		}
	}
	if selected.inline {
		return sig, nil
	}
	return signatureRows(ctx, q, id, sig, selected.ids)
}

// JSON validation and the selected tree traversal stay within one statement.
// The CASE never exports large wrong-type timestamps/identities. The path
// predicate restricts identity rows to direct array objects, excluding unknown
// nested fields and their unrestricted contents. No whole-value CTE is built.
// jsonb reuses json_valid's parsed cache and avoids a second text translation
// in json_tree. Equal text-character and blob-byte lengths prove there is no
// raw NUL (which terminates SQLite text length); other inputs retain the scan.
const signatureNodeQuery = `SELECT c.value IS NOT NULL, j.id, j.parent, j.key, j.type,
 CASE WHEN j.parent = 0 AND j.key = 'lastUpdatedAt' AND j.type IN ('integer','real') THEN j.atom
      WHEN j.parent != 0 AND lower(j.key) = 'bubbleid' AND j.type = 'text'
           AND j.path NOT IN ('$.fullConversationHeadersOnly','$.conversation') THEN j.atom END
 FROM cursorDiskKV AS c LEFT JOIN json_tree(
 CASE WHEN json_valid(c.value)
      AND (length(CAST(c.value AS TEXT)) = length(CAST(c.value AS BLOB))
           OR instr(CAST(c.value AS BLOB),x'00') = 0) THEN jsonb(c.value) END) AS j
 WHERE c.key = ? AND (j.parent IS NULL
 OR j.parent = 0 AND j.key IN ('lastUpdatedAt','fullConversationHeadersOnly','conversation')
 OR j.path IN ('$.fullConversationHeadersOnly','$.conversation')
 OR lower(j.key) = 'bubbleid'
    AND (j.path GLOB '$.fullConversationHeadersOnly[[]*]' OR j.path GLOB '$.conversation[[]*]')
    AND instr(substr(j.path,instr(j.path,'[')+1),']') = length(j.path)-instr(j.path,'['))
 ORDER BY j.id`

func signatureTimestamp(kind sqliteValueKind, value any) (int64, error) {
	switch kind {
	case "", sqliteNull:
		return 0, nil
	case sqliteInteger:
		if n, ok := value.(int64); ok {
			return int64(float64(n)), nil
		}
		// JSON integral numbers outside SQLite's int64 range retain their
		// integer JSON type but have a floating-point atom. Match the legacy
		// decoder's float64 conversion, including its int64 overflow behavior.
		fallthrough
	case sqliteReal:
		if n, ok := value.(float64); ok && !math.IsInf(n, 0) && !math.IsNaN(n) {
			return int64(n), nil
		}
	case sqliteBlob, sqliteText, sqliteArray, sqliteObject:
		return 0, NotChecked(UnknownFormat)
	}
	return 0, NotChecked(UnknownFormat)
}

type signatureTreeHeaders struct {
	signatureHeaders
	inline      bool
	node        int64
	entry       int64
	live        bool
	identity    string
	identitySet bool
	invalid     bool
	repairs     []int64
}

func newSignatureTreeHeaders(node int64, kind sqliteValueKind, inline bool) signatureTreeHeaders {
	var err error
	if kind != sqliteArray && kind != sqliteNull {
		err = NotChecked(UnknownFormat)
	}
	return signatureTreeHeaders{
		signatureHeaders: signatureHeaders{present: kind != sqliteNull, err: err},
		inline:           inline,
		node:             node,
	}
}

func (h *signatureTreeHeaders) visit(node, parent int64, kind sqliteValueKind, scalar any) {
	if parent == h.node && h.present && h.err == nil {
		h.finish()
		h.entry, h.live = node, true
		h.invalid = kind != sqliteObject && kind != sqliteNull
		return
	}
	if !h.live || parent != h.entry {
		return
	}
	switch kind {
	case sqliteText:
		h.identity, h.identitySet = scalar.(string), true
	case sqliteNull:
		h.identity, h.identitySet = "", false
	case sqliteBlob, sqliteInteger, sqliteReal, sqliteArray, sqliteObject:
		// encoding/json rejects any wrong-type duplicate, even if a later
		// bubbleId has a valid string. Top-level duplicates instead select last.
		h.invalid = true
	default:
		h.invalid = true
	}
}

func (h *signatureTreeHeaders) finish() {
	if !h.live {
		return
	}
	h.count++
	h.last = h.identity
	if h.invalid || !h.inline && (!h.identitySet || h.identity == "") {
		h.err = NotChecked(UnknownFormat)
	}
	if !utf8.ValidString(h.identity) {
		h.repairs = append(h.repairs, int64(h.count-1))
	}
	if !h.inline {
		h.ids = append(h.ids, h.identity)
	}
	h.live, h.identitySet, h.invalid = false, false, false
	h.identity = ""
}

// sqliteValueKind preserves SQLite's actual storage and JSON type spellings.
type sqliteValueKind string

const (
	sqliteBlob    sqliteValueKind = "blob"
	sqliteText    sqliteValueKind = "text"
	sqliteNull    sqliteValueKind = "null"
	sqliteInteger sqliteValueKind = "integer"
	sqliteReal    sqliteValueKind = "real"
	sqliteArray   sqliteValueKind = "array"
	sqliteObject  sqliteValueKind = "object"
)
