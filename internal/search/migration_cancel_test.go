package search

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// A connector-local wrapper keeps this cancellation schedule isolated from
// other tests while all schema changes and rollback still use real SQLite.
type migrationRollbackConnector struct {
	dsn     string
	started chan struct{}
	release chan struct{}
}

func (c *migrationRollbackConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &migrationRollbackDriverConn{Conn: conn, connector: c}, nil
}

func (*migrationRollbackConnector) Driver() driver.Driver { return &sqlite.Driver{} }

type migrationRollbackDriverConn struct {
	driver.Conn
	connector *migrationRollbackConnector
}

func (c *migrationRollbackDriverConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &migrationRollbackTx{Tx: tx, connector: c.connector}, nil
}

type migrationRollbackTx struct {
	driver.Tx
	connector *migrationRollbackConnector
}

func (tx *migrationRollbackTx) Rollback() error {
	close(tx.connector.started)
	<-tx.connector.release
	return tx.Tx.Rollback()
}

type migrationCancelConnection struct {
	*sql.Conn
	cancel         context.CancelFunc
	rollback       <-chan struct{}
	closeEntered   chan struct{}
	transactionErr chan error
}

func (c *migrationCancelConnection) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	tx, err := c.Conn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	// Mutate the old schema inside the actual transaction before interrupting
	// it. The initializer must not return until these changes are rolled back.
	if _, err = tx.ExecContext(ctx, "CREATE TABLE canceled_migration_marker(value INTEGER); PRAGMA user_version=4"); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	c.cancel()
	select {
	case <-c.rollback:
		// database/sql has claimed rollback but the real driver rollback is
		// deliberately blocked. A second Rollback now reports ErrTxDone.
		c.transactionErr <- tx.Rollback()
		return tx, nil
	case <-time.After(10 * time.Second):
		return nil, errors.New("cancellation rollback did not start")
	}
}

func (c *migrationCancelConnection) Close() error {
	close(c.closeEntered)
	return c.Conn.Close()
}

func TestSearchMigrationCancellationWaitsForRollback(t *testing.T) {
	t.Parallel()
	dir := oldSearchIndex(t, 20000)
	connector := &migrationRollbackConnector{
		dsn:     filepath.Join(dir, "content.sqlite"),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(t.Context())
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(connector.release) }) }
	defer func() {
		release()
		_ = conn.Close()
		_ = db.Close()
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	controlled := &migrationCancelConnection{
		Conn:           conn,
		cancel:         cancel,
		rollback:       connector.started,
		closeEntered:   make(chan struct{}),
		transactionErr: make(chan error, 1),
	}
	finished := make(chan error, 1)
	go func() { finished <- (&Store{db: db, destination: "synthetic"}).initializeConnection(ctx, controlled) }()
	select {
	case err = <-controlled.transactionErr:
		if !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("cancellation did not claim transaction before cleanup: %v", err)
		}
	case err = <-finished:
		t.Fatalf("initialization returned before controlled cancellation: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("initialization did not reach controlled cancellation")
	}
	select {
	case <-controlled.closeEntered:
	case err = <-finished:
		t.Fatalf("initialization returned before waiting for rollback: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("initialization did not close its reserved connection")
	}
	// Both rollback and Close have entered. No clock-based delay is needed to
	// assert that initialization cannot finish while rollback remains blocked.
	select {
	case err = <-finished:
		t.Fatalf("initialization returned while rollback was blocked: %v", err)
	default:
	}
	release()
	select {
	case err = <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("initialization cancellation: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("initialization did not finish after rollback")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", connector.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	var version, markers int
	if err = raw.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("canceled migration changed schema: version=%d %v", version, err)
	}
	if err = raw.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_schema WHERE name='canceled_migration_marker'").Scan(&markers); err != nil || markers != 0 {
		t.Fatalf("canceled migration left transactional marker: count=%d %v", markers, err)
	}
	if err = raw.Close(); err != nil {
		t.Fatal(err)
	}
	retried, err := Open(t.Context(), dir, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = retried.Close() }()
	var count, seeded int
	if err = retried.db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_sessions").Scan(&count); err != nil || count != 20000 {
		t.Fatalf("retry lost sessions: count=%d %v", count, err)
	}
	if err = retried.db.QueryRowContext(t.Context(), "SELECT seeded FROM search_discovery").Scan(&seeded); err != nil || seeded != 0 {
		t.Fatalf("retry eagerly seeded identities: seeded=%d %v", seeded, err)
	}
}
