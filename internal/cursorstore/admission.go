package cursorstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

const admissionImageLimit int64 = 128 << 20

const admissionStepPages int32 = 16

const admissionBusyRetries = 64

// AdmissionCopyStats reports bounded copy work and external WAL observations.
// External WAL bytes are not owned or capped by the provider reservation.
type AdmissionCopyStats struct {
	WorkspaceImage             bool
	Steps                      int
	BusyRetries                int
	Pages                      int64
	PageBytes                  int64
	ExternalWALBefore          int64
	ExternalWALAfter           int64
	DestinationWriteCalls      int
	DestinationWriteBytes      int64
	DestinationMaxWriteEnd     int64
	ProhibitedWrites           int
	DestinationOpens           int
	ProhibitedOpens            int
	DestinationOpenDescriptors int
}

type admissionCopyOptions struct {
	afterStep             func(int)
	beforeSourceOpen      func()
	beforeDestinationOpen func()
	afterImage            func(*os.File, string) error
}

// prepareReservedLiveImage is the preparatory bounded SQLite copy backend.
// It produces no native admission proof and has no production callers. A future
// sealed producer must bind the image, source identity and reservation lifetime
// before using its bytes for admission. A returned path alone confers no authority.
func prepareReservedLiveImage(ctx context.Context, path string, workspace admissionWorkspaceBudget) (string, AdmissionCopyStats, error) {
	return prepareAdmissionDatabase(ctx, path, workspace, admissionCopyOptions{})
}

func prepareAdmissionDatabase(ctx context.Context, path string, workspace admissionWorkspaceBudget, opts admissionCopyOptions) (out string, stats AdmissionCopyStats, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	src, err := resolve(path)
	if err != nil {
		return "", stats, err
	}
	if !src.live {
		return "", stats, NotChecked(Locked)
	}
	if workspace == nil {
		return "", stats, errors.Join(errors.New("cursor import remains unadmitted; live admission requires reserved private scratch"), NotChecked(Locked))
	}
	if err = workspace.Err(); err != nil {
		return "", stats, err
	}
	ctx, cancel := context.WithTimeout(ctx, ReadTimeout)
	defer cancel()
	allowed, err := admissionSourceFiles(src)
	if err != nil {
		return "", stats, err
	}
	stats.ExternalWALBefore = allowed[src.path+"-wal"].Size()
	err = withAdmissionSource(ctx, src, allowed, opts, func(conn *sql.Conn, tx *sql.Tx) error {
		imageBytes, e := pinAdmissionGeometry(ctx, tx, &stats)
		if e != nil {
			return e
		}
		out, e = copyAdmissionIntoWorkspace(ctx, conn, workspace, imageBytes, &stats, opts)
		return errors.Join(e, checkAdmissionNativeFiles(src, allowed, &stats))
	})
	if err != nil {
		return "", stats, err
	}
	if err = ctx.Err(); err != nil {
		return "", stats, err
	}
	stats.WorkspaceImage = true
	return out, stats, nil
}

func admissionSourceFiles(src source) (map[string]os.FileInfo, error) {
	allowed := map[string]os.FileInfo{src.path: src.before}
	for _, suffix := range []string{"-wal", "-shm"} {
		info, err := os.Lstat(src.path + suffix)
		if err != nil || !info.Mode().IsRegular() {
			return nil, NotChecked(Locked)
		}
		allowed[src.path+suffix] = info
	}
	return allowed, nil
}

func withAdmissionSource(ctx context.Context, src source, allowed map[string]os.FileInfo, opts admissionCopyOptions, copyPinned func(*sql.Conn, *sql.Tx) error) (err error) {
	root, err := os.OpenRoot(filepath.Dir(src.path))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	vfs, err := newAdmissionVFS(allowed, false, 0)
	if err != nil {
		return err
	}
	vfs.setSourceRoot(root, filepath.Dir(src.path))
	defer func() { err = errors.Join(err, vfs.Close()) }()
	if opts.beforeSourceOpen != nil {
		opts.beforeSourceOpen()
	}
	db, err := sql.Open("sqlite", admissionDSN(dsn(src.path, true), vfs.name))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	conn, err := admissionConnection(ctx, db)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, tx.Rollback()) }()
	return copyPinned(conn, tx)
}

func pinAdmissionGeometry(ctx context.Context, tx *sql.Tx, stats *AdmissionCopyStats) (int64, error) {
	// BEGIN alone is deferred. A real read pins this SAME driver connection
	// before geometry is observed or any provider scratch is allocated.
	var count int64
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&count); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA page_size").Scan(&stats.PageBytes); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA page_count").Scan(&stats.Pages); err != nil {
		return 0, err
	}
	return admissionImageBytes(stats.PageBytes, stats.Pages)
}

func copyAdmissionIntoWorkspace(ctx context.Context, conn *sql.Conn, workspace admissionWorkspaceBudget, imageBytes int64, stats *AdmissionCopyStats, opts admissionCopyOptions) (out string, err error) {
	if err = workspace.Reserve(imageBytes); err != nil {
		return "", err
	}
	root, err := workspace.OpenWorkspace()
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	f, err := root.OpenFile("native.db", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	out = filepath.Join(root.Name(), "native.db")
	dest, err := newAdmissionDestinationVFS(ctx, out, f, imageBytes)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, dest.Close()) }()
	defer dest.copyStats(stats)
	if opts.beforeDestinationOpen != nil {
		opts.beforeDestinationOpen()
	}
	dst := admissionDestinationDSN(out, dest.name, stats.PageBytes, stats.Pages)
	if err = verifyAdmissionDestination(ctx, dst, stats.PageBytes, stats.Pages); err != nil {
		return "", err
	}
	if err = runAdmissionBackup(ctx, conn, dst, stats, opts); err != nil {
		return "", err
	}
	if err = verifyAdmissionImage(root, imageBytes); err != nil {
		return "", err
	}
	if opts.afterImage != nil {
		if err = opts.afterImage(f, out); err != nil {
			return "", err
		}
	}
	return out, ctx.Err()
}

func runAdmissionBackup(ctx context.Context, conn *sql.Conn, dst string, stats *AdmissionCopyStats, opts admissionCopyOptions) error {
	return conn.Raw(func(driverConn any) (err error) {
		b, ok := driverConn.(backuper)
		if !ok {
			return errors.New("bounded cursor admission backup capability unavailable")
		}
		unlock := lockAdmissionRegistry()
		bk, err := b.NewBackup(dst)
		unlock()
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, bk.Finish()) }()
		maxSteps := (stats.Pages + int64(admissionStepPages) - 1) / int64(admissionStepPages)
		for int64(stats.Steps) < maxSteps {
			if err = ctx.Err(); err != nil {
				return err
			}
			more, e := bk.Step(admissionStepPages)
			if e != nil {
				if !busy(e) || stats.BusyRetries >= admissionBusyRetries {
					return e
				}
				stats.BusyRetries++
				timer := time.NewTimer(backupRetry)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
				continue
			}
			stats.Steps++
			if opts.afterStep != nil {
				opts.afterStep(stats.Steps)
			}
			if int64(bk.PageCount()) != stats.Pages {
				return errors.New("cursor admission pinned geometry changed")
			}
			if !more {
				return ctx.Err()
			}
		}
		return errBackupIncomplete
	})
}

func checkAdmissionNativeFiles(src source, allowed map[string]os.FileInfo, stats *AdmissionCopyStats) (err error) {
	sides := existingSideFiles(src.path)
	if len(sides) != 2 || !sides["-wal"] || !sides["-shm"] {
		return NotChecked(Locked)
	}
	if after, e := os.Lstat(src.path + "-wal"); e == nil {
		stats.ExternalWALAfter = after.Size()
	} else {
		err = e
	}
	for path, before := range allowed {
		after, e := os.Lstat(path)
		if e != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
			err = errors.Join(err, NotChecked(Locked))
		}
	}
	return err
}

func verifyAdmissionImage(root *os.Root, imageBytes int64) error {
	copied, err := root.Stat("native.db")
	if err != nil || copied.Size() != imageBytes {
		return errors.New("cursor admission image geometry is incomplete")
	}
	entries, err := root.Open(".")
	if err != nil {
		return err
	}
	names, err := entries.Readdirnames(2)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	err = errors.Join(err, entries.Close())
	if err != nil || len(names) != 1 || names[0] != "native.db" {
		return errors.New("cursor admission unexpected scratch sidefile")
	}
	return nil
}

func admissionDSN(dsn, name string) string {
	u, _ := url.Parse(dsn)
	q := u.Query()
	q.Set("vfs", name)
	q.Add("_pragma", "cache_size(-2048)")
	q.Add("_pragma", "mmap_size(0)")
	u.RawQuery = q.Encode()
	return u.String()
}

func admissionDestinationDSN(path, name string, pageSize, pages int64) string {
	q := url.Values{"mode": {"rw"}, "vfs": {name}}
	for _, pragma := range []string{"journal_mode(MEMORY)", "temp_store(MEMORY)", "mmap_size(0)", "cache_size(-2048)", "page_size(" + strconv.FormatInt(pageSize, 10) + ")", "max_page_count(" + strconv.FormatInt(pages, 10) + ")"} {
		q.Add("_pragma", pragma)
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
}

func verifyAdmissionDestination(ctx context.Context, dsn string, pageSize, pages int64) (err error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	conn, err := admissionConnection(ctx, db)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	for _, check := range []struct {
		pragma   string
		expected string
	}{{"journal_mode", "memory"}, {"temp_store", "2"}, {"page_size", strconv.FormatInt(pageSize, 10)}, {"max_page_count", strconv.FormatInt(pages, 10)}} {
		var got string
		if err = conn.QueryRowContext(ctx, "PRAGMA "+check.pragma).Scan(&got); err != nil {
			return err
		}
		if got != check.expected {
			return fmt.Errorf("cursor admission destination %s unsupported", check.pragma)
		}
	}
	return nil
}

func admissionImageBytes(pageSize, pages int64) (int64, error) {
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 || pages <= 0 || pages > admissionImageLimit/pageSize {
		return 0, agentapi.Wrap(agentapi.Limit, agentapi.ErrRawLimit)
	}
	return pages * pageSize, nil
}

func admissionConnection(ctx context.Context, db *sql.DB) (*sql.Conn, error) {
	unlock := lockAdmissionRegistry()
	defer unlock()
	return db.Conn(ctx)
}

// admissionWorkspaceBudget owns private scratch and its durable quota receipt.
// Reserve must persist capacity before returning success; failures and partial
// images retain that charge until owned cleanup succeeds. The owner must also
// charge the pass memory ledger and keep its captured reservation through reader
// closure. Root/OpenWorkspace must denote the same private rooted directory.
// No implementation is wired into production until those contracts are sealed.
type admissionWorkspaceBudget = LiveAdmissionWorkspace

// LiveAdmissionWorkspace supplies bounded scratch IO, never capture authority.
// Only the sealed live producer can turn an actual fixed read into a proof.
type LiveAdmissionWorkspace interface {
	OpenWorkspace() (*os.Root, error)
	Reserve(int64) error
	Err() error
}
