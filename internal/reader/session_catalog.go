package reader

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/trace"
	"modernc.org/sqlite"
)

// SearchSummary is a private projection of published metadata. It deliberately
// excludes source references, counts, payloads, history and transcripts.
type SearchSummary struct {
	SessionID       string                    `json:"SessionID"`
	NativeSessionID string                    `json:"NativeSessionID"`
	MachineID       string                    `json:"MachineID"`
	ProjectID       string                    `json:"ProjectID"`
	ProjectName     string                    `json:"ProjectName"`
	RepoKey         string                    `json:"RepoKey"`
	Name            string                    `json:"Name"`
	Title           string                    `json:"Title"`
	Branch          string                    `json:"Branch"`
	NativeChild     bool                      `json:"NativeChild"`
	ParentSessionID string                    `json:"ParentSessionID"`
	StartedAt       time.Time                 `json:"StartedAt"`
	CapturedAt      time.Time                 `json:"CapturedAt"`
	EndedAt         *time.Time                `json:"EndedAt"`
	Harness         archive.Harness           `json:"Harness"`
	Parser          archive.ParserInfo        `json:"Parser"`
	Origin          archive.SessionOrigin     `json:"Origin"`
	Replay          *archive.Replay           `json:"Replay"`
	Models          []archive.ModelSummary    `json:"Models"`
	SkillsAvailable []archive.SkillSnapshot   `json:"SkillsAvailable"`
	SkillsUsed      []archive.SkillUse        `json:"SkillsUsed"`
	SkillDetection  archive.SkillDetection    `json:"SkillDetection"`
	CaptureGaps     []archive.CaptureGap      `json:"CaptureGaps"`
	PullRequests    []archive.PullRequestLink `json:"PullRequests"`
	GitActivity     []archive.GitEvent        `json:"GitActivity"`
}

// Metadata returns the search/display projection, never a complete body.
func (s SearchSummary) Metadata() archive.Metadata {
	return archive.Metadata{SessionID: s.SessionID, NativeSessionID: s.NativeSessionID, MachineID: s.MachineID, ProjectID: s.ProjectID, ProjectName: s.ProjectName, RepoKey: s.RepoKey, Name: s.Name, Title: s.Title, Branch: s.Branch, NativeChild: s.NativeChild, ParentSessionID: s.ParentSessionID, StartedAt: s.StartedAt, CapturedAt: s.CapturedAt, EndedAt: s.EndedAt, Harness: s.Harness, Parser: s.Parser, Origin: s.Origin, Replay: s.Replay, Models: s.Models, SkillsAvailable: s.SkillsAvailable, SkillsUsed: s.SkillsUsed, SkillDetection: s.SkillDetection, CaptureGaps: s.CaptureGaps, PullRequests: s.PullRequests, GitActivity: s.GitActivity}
}

func summarize(m archive.Metadata) SearchSummary {
	// Retain only PR numbers, never repositories, URLs or unrelated git events.
	prs := make([]archive.PullRequestLink, 0, len(m.PullRequests))
	for _, pr := range m.PullRequests {
		prs = append(prs, archive.PullRequestLink{Number: pr.Number})
	}
	events := []archive.GitEvent{}
	for _, event := range m.GitActivity {
		if event.Kind == archive.GitEventPRCreated {
			events = append(events, archive.GitEvent{Kind: event.Kind, PRNumber: event.PRNumber})
		}
	}
	gaps := make([]archive.CaptureGap, len(m.CaptureGaps))
	for i, gap := range m.CaptureGaps {
		gaps[i] = archive.CaptureGap{Code: gap.Code}
	}
	var replay *archive.Replay
	if m.Replay != nil {
		replay = &archive.Replay{}
	}
	return SearchSummary{SessionID: m.SessionID, NativeSessionID: m.NativeSessionID, MachineID: m.MachineID, ProjectID: m.ProjectID, ProjectName: m.ProjectName, RepoKey: m.RepoKey, Name: m.Name, Title: m.Title, Branch: m.Branch, NativeChild: m.NativeChild, ParentSessionID: m.ParentSessionID, StartedAt: m.StartedAt, CapturedAt: m.CapturedAt, EndedAt: m.EndedAt, Harness: m.Harness, Parser: m.Parser, Origin: m.Origin, Replay: replay, Models: m.Models, SkillsAvailable: m.SkillsAvailable, SkillsUsed: m.SkillsUsed, SkillDetection: m.SkillDetection, CaptureGaps: gaps, PullRequests: prs, GitActivity: events}
}

// SessionCatalog indexes summaries after a fresh complete canonical discovery.
type SessionCatalog interface {
	Refresh(context.Context, HeaderSnapshot) error
	Query(context.Context, CatalogQuery) (CatalogPage, error)
}

// CatalogQuery selects a safe summary candidate set. Words also bind the
// cursor; callers apply their text matcher before limiting final matches.
type CatalogQuery struct {
	Metadata MetadataQuery
	// Words select a safe candidate superset. CLI Unicode, exact ID, PR and
	// configured project label rules remain the final authority.
	Words  []string
	Cursor string
}

// CatalogRow identifies one exact canonical metadata revision.
type CatalogRow struct {
	Key     string
	ETag    string
	Hash    string
	Summary SearchSummary
}

// CatalogPage reports candidate rows and an exact candidate total. Complete
// describes discovery and typed filtering, not caller-specific text matching.
type CatalogPage struct {
	Rows     []CatalogRow
	Next     string
	Total    int
	Complete bool
}

// ErrStaleCatalogCursor asks callers to restart their view after refresh.
var ErrStaleCatalogCursor = errors.New("sessions changed; refresh the view")

// SQLiteSessionCatalog retains summaries only; bodies stay in MetadataCache.
type SQLiteSessionCatalog struct {
	db             *sql.DB
	store          storage.ObjectStore
	opts           ListOptions
	viewMu         sync.RWMutex
	viewEpoch      string
	viewGeneration int64
	viewReady      bool
	remoteSnapshot *catalog.Snapshot
	remoteBinding  string
	namespace      string
}

// OpenSessionCatalog opens a disposable private SQLite index. Each connection
// has a busy timeout; a transaction serializes reconciliation across processes.
func OpenSessionCatalog(ctx context.Context, cache *MetadataCache, store storage.ObjectStore, opts ListOptions) (*SQLiteSessionCatalog, error) {
	return openSessionCatalog(ctx, cache, store, opts, false)
}

func boundCatalogCache(cache *MetadataCache, store storage.ObjectStore, opts ListOptions) (ListOptions, error) {
	if cache == nil {
		return opts, errors.New("session catalog requires a metadata cache")
	}
	if opts.Cache == nil {
		opts.Cache = cache
	}
	if CatalogAuthority(store) && opts.Cache.dir != cache.dir {
		return opts, errors.New("remote session catalog requires its bound metadata cache")
	}
	return opts, nil
}

func openSessionCatalog(ctx context.Context, cache *MetadataCache, store storage.ObjectStore, opts ListOptions, repair bool) (*SQLiteSessionCatalog, error) {
	var err error
	opts, err = boundCatalogCache(cache, store, opts)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(filepath.Dir(cache.dir), "catalog")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return nil, errors.New("catalog requires a real directory")
	}
	if err = os.Chmod(dir, 0700); err != nil { //nolint:gosec // A private directory requires owner traversal; files use 0600.
		return nil, err
	}
	if !repair {
		unlock, e := lockCatalogOpen(dir)
		if e != nil {
			return nil, e
		}
		defer unlock()
	}
	path := filepath.Join(dir, "sessions.sqlite")
	for _, candidate := range []string{path, path + "-wal", path + "-shm", filepath.Join(dir, "open.lock")} {
		if info, e := os.Lstat(candidate); e == nil && !info.Mode().IsRegular() {
			return nil, errors.New("catalog requires regular files")
		} else if e != nil && !os.IsNotExist(e) {
			return nil, e
		} else if e == nil {
			if err := os.Chmod(candidate, 0600); err != nil {
				return nil, err
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
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("catalog requires a regular file")
	}
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS catalog_state (id INTEGER PRIMARY KEY CHECK(id=1), generation INTEGER NOT NULL, epoch TEXT NOT NULL, complete INTEGER NOT NULL); CREATE TABLE IF NOT EXISTS sessions (key TEXT PRIMARY KEY, etag TEXT NOT NULL, hash TEXT NOT NULL, capture TEXT NOT NULL, activity TEXT NOT NULL, summary BLOB NOT NULL, search TEXT NOT NULL, lowerid TEXT NOT NULL, unlabeled INTEGER NOT NULL, summary_hash TEXT NOT NULL); CREATE INDEX IF NOT EXISTS sessions_capture ON sessions(capture DESC,key); CREATE INDEX IF NOT EXISTS sessions_activity ON sessions(activity DESC,capture DESC,key)`)
	if err == nil {
		err = initializeCatalogState(ctx, db)
	}
	if err != nil {
		_ = db.Close()
		var damaged *sqlite.Error
		if !repair && errors.As(err, &damaged) && (damaged.Code() == 11 || damaged.Code() == 26) {
			// SQLite rejected this disposable file before any transaction. Retire
			// the damaged bytes and create a fresh summary index on this request.
			for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
				if e := os.Remove(candidate); e != nil && !os.IsNotExist(e) {
					return nil, e
				}
			}
			return openSessionCatalog(ctx, cache, store, opts, true)
		}
		return nil, err
	}
	opts.Cache = cache
	return &SQLiteSessionCatalog{db: db, store: store, opts: opts, namespace: catalogStoreNamespace(store)}, nil
}

func lockCatalogOpen(dir string) (func(), error) {
	// NamedLock creates its file, so reject a preexisting symlink before
	// opening it rather than letting a dangling link create another path.
	info, err := os.Lstat(filepath.Join(dir, "open.lock"))
	if err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("catalog requires regular files")
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return local.NamedLock(dir, "open.lock")
}

// Unknown stores are reusable only within this process and for the exact same
// immutable store instance. Keeping bounded strong references prevents address
// reuse from inheriting another destination's summaries. Registry eviction only
// makes a later open cold; namespaces are never recycled.
var catalogStoreIDs = struct {
	sync.Mutex
	stores map[storage.ObjectStore]string
}{stores: make(map[storage.ObjectStore]string)}

func catalogStoreNamespace(store storage.ObjectStore) string {
	if identified, ok := store.(storage.CatalogNamespace); ok {
		if namespace := identified.CatalogNamespace(); namespace != "" {
			return "destination:" + storage.SHA256Hex([]byte(namespace))
		}
	}
	if store == nil || reflect.TypeOf(store).Kind() != reflect.Pointer {
		return "handle:" + rand.Text()
	}
	catalogStoreIDs.Lock()
	defer catalogStoreIDs.Unlock()
	if namespace, ok := catalogStoreIDs.stores[store]; ok {
		return namespace
	}
	if len(catalogStoreIDs.stores) >= 64 {
		clear(catalogStoreIDs.stores)
	}
	namespace := "instance:" + rand.Text()
	catalogStoreIDs.stores[store] = namespace
	return namespace
}

func initializeCatalogState(ctx context.Context, db *sql.DB) error {
	if err := migrateCatalogSummaryHash(ctx, db); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(catalog_state)")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			return err
		}
		found = found || name == "namespace"
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if !found {
		if _, err = db.ExecContext(ctx, "ALTER TABLE catalog_state ADD COLUMN namespace TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	_, err = db.ExecContext(ctx, "INSERT OR IGNORE INTO catalog_state(id,generation,epoch,complete,namespace) VALUES(1,0,?,0,'')", rand.Text())
	return err
}

// migrateCatalogSummaryHash leaves legacy rows untrusted until a proven complete
// refresh reloads them. The checksum covers the complete persisted row tuple,
// separately from the full metadata revision hash.
func migrateCatalogSummaryHash(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(sessions)")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			return err
		}
		found = found || name == "summary_hash"
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil || found {
		return err
	}
	_, err = db.ExecContext(ctx, "ALTER TABLE sessions ADD COLUMN summary_hash TEXT NOT NULL DEFAULT ''")
	return err
}

// Close releases the catalog database handle.
func (c *SQLiteSessionCatalog) Close() error { return c.db.Close() }

// DiscoverCatalogHeaders always discovers the full canonical scope so a
// narrowed query cannot evict other projects or harnesses from the local index.
func DiscoverCatalogHeaders(ctx context.Context, store storage.ObjectStore, opts ListOptions) (HeaderSnapshot, error) {
	span := trace.Start("read metadata headers")
	defer span.End()
	opts.Cache.maintain(ctx)
	scan := span.Child("scan cache")
	known := opts.Cache.keys("sessions/")
	scan.Count("cached keys", len(known))
	scan.End()
	objects, err := listObjects(ctx, store, "sessions/", known)
	if err != nil {
		return HeaderSnapshot{}, err
	}
	if span != nil {
		span.Count("keys", len(objects))
		sidecars := 0
		for _, object := range objects {
			if isMetadataKey(object.Key) {
				sidecars++
			}
		}
		span.Count("sidecars", sidecars)
	}
	opts.Cache.evictUnlisted(known, objects)
	return HeaderSnapshot{CanonicalComplete: true, Canonical: objects, knownCanonical: known}, nil
}

// ListMetadataFromSnapshot reuses a catalog discovery when an unverifiable or
// unsupported summary requires the established exhaustive metadata fallback.
func ListMetadataFromSnapshot(ctx context.Context, store storage.ObjectStore, snapshot HeaderSnapshot, filter Filter, opts ListOptions) ([]archive.Metadata, error) {
	if !snapshot.CanonicalComplete {
		return nil, errors.New("metadata fallback requires complete canonical discovery")
	}
	return listMetadataFromHeaders(ctx, store, filter, opts, snapshot.Canonical, snapshot.knownCanonical)
}

// Refresh reconciles current canonical revisions. Proven absence is retired
// even when an unrelated changed body fails, without publishing a ready view.
func (c *SQLiteSessionCatalog) Refresh(ctx context.Context, snapshot HeaderSnapshot) error {
	span := trace.Start("refresh session catalog")
	reused := 0
	defer func() {
		span.Count("from catalog", reused)
		span.End()
	}()
	if !snapshot.CanonicalComplete {
		return errors.New("session catalog requires complete canonical discovery")
	}
	c.viewMu.Lock()
	defer c.viewMu.Unlock()
	c.viewReady = false
	// A canonical refresh owns a legacy row universe, not the retained remote view.
	c.remoteSnapshot = nil
	c.remoteBinding = ""
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	destinationChanged, err := prepareCanonicalCatalog(ctx, tx, c.namespace)
	if err != nil {
		return err
	}
	prior, validSummary, err := canonicalCatalogRows(ctx, tx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var pending []storage.Object
	for _, obj := range snapshot.Canonical {
		if err = ctx.Err(); err != nil {
			return err
		}
		if !isMetadataKey(obj.Key) {
			continue
		}
		seen[obj.Key] = true
		if obj.ETag == "" || prior[obj.Key] != obj.ETag || !validSummary[obj.Key] {
			pending = append(pending, obj)
		} else {
			reused++
		}
	}
	// Complete canonical discovery proves absence independently of decoding a
	// different current sidecar. Retire those logical rows even if that decode
	// fails, while withholding a complete view and invalidating every old cursor.
	retired := destinationChanged
	for key := range prior {
		if !seen[key] {
			if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE key=?", key); err != nil {
				return err
			}
			retired = true
		}
	}
	loaded, err := c.readChanges(ctx, pending, snapshot.Revisions)
	if err != nil {
		readErr := err
		if retired && ctx.Err() == nil {
			if _, err = tx.ExecContext(ctx, "UPDATE catalog_state SET generation=generation+1,complete=0 WHERE id=1"); err != nil {
				return errors.Join(readErr, err)
			}
			if err = tx.Commit(); err != nil {
				return errors.Join(readErr, err)
			}
		}
		return readErr
	}
	changed := len(loaded) > 0 || retired
	for _, row := range loaded {
		data, e := json.Marshal(row.Summary)
		if e != nil {
			return e
		}
		m := row.Summary.Metadata()
		record := catalogRecord{key: row.Key, etag: row.ETag, hash: row.Hash, capture: catalogTime(m.CapturedAt), activity: catalogTime(listingindex.ActivityTime(m)), summary: data, search: catalogSearch(row.Summary), lowerID: strings.ToLower(m.SessionID), unlabeled: m.ProjectName == ""}
		if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO sessions VALUES(?,?,?,?,?,?,?,?,?,?)", record.key, record.etag, record.hash, record.capture, record.activity, record.summary, record.search, record.lowerID, record.unlabeled, catalogRecordChecksum(record)); err != nil {
			return err
		}
	}
	if changed {
		if _, err = tx.ExecContext(ctx, "UPDATE catalog_state SET generation=generation+1,complete=1 WHERE id=1"); err != nil {
			return err
		}
	}
	if !changed {
		if _, err = tx.ExecContext(ctx, "UPDATE catalog_state SET complete=1 WHERE id=1"); err != nil {
			return err
		}
	}
	var epoch string
	var generation int64
	if err = tx.QueryRowContext(ctx, "SELECT epoch,generation FROM catalog_state WHERE id=1").Scan(&epoch, &generation); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	c.viewEpoch, c.viewGeneration, c.viewReady = epoch, generation, true
	return nil
}

// prepareCanonicalCatalog keeps provenance retirement inside the caller's writer transaction.
func prepareCanonicalCatalog(ctx context.Context, tx *sql.Tx, canonicalNamespace string) (bool, error) {
	// Acquire the SQLite writer before reading validators, preventing a stale
	// comparison from overwriting another connection's completed refresh.
	if _, err := tx.ExecContext(ctx, "UPDATE catalog_state SET generation=generation WHERE id=1"); err != nil {
		return false, err
	}
	// Canonical reconciliation may replace remote rows even at equal cardinality.
	// Invalidate their root in the SAME transaction, including retirement-only
	// commits on unrelated decode failure. Rollback retains the prior provenance.
	var hasRemoteRoot bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type='table' AND name='remote_root')").Scan(&hasRemoteRoot); err != nil {
		return false, err
	}
	if hasRemoteRoot {
		if _, err := tx.ExecContext(ctx, "DELETE FROM remote_root WHERE id=1"); err != nil {
			return false, err
		}
	}
	var namespace string
	if err := tx.QueryRowContext(ctx, "SELECT namespace FROM catalog_state WHERE id=1").Scan(&namespace); err != nil {
		return false, err
	}
	destinationChanged := namespace != canonicalNamespace
	if destinationChanged {
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions"); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE catalog_state SET namespace=?,generation=generation+1,complete=0 WHERE id=1", canonicalNamespace); err != nil {
			return false, err
		}
	}
	return destinationChanged, nil
}

// canonicalCatalogRows closes its cursor before the caller uses the single connection again.
func canonicalCatalogRows(ctx context.Context, tx *sql.Tx) (map[string]string, map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT key,etag,hash,capture,activity,summary,search,lowerid,unlabeled,summary_hash FROM sessions")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	prior := map[string]string{}
	validSummary := map[string]bool{}
	for rows.Next() {
		var record catalogRecord
		if err = record.scan(rows); err != nil {
			return nil, nil, err
		}
		prior[record.key] = record.etag
		validSummary[record.key] = record.valid()
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, nil, err
	}
	return prior, validSummary, nil
}

// readChanges overlaps independent cold/changed reads while keeping observers
// and errors in canonical selection order. Every worker joins before rollback.
func (c *SQLiteSessionCatalog) readChanges(ctx context.Context, objects []storage.Object, revisions map[RevisionID]listingindex.Revision) ([]CatalogRow, error) {
	span := trace.Start("read sidecars")
	span.Count("sidecars", len(objects))
	results := make([]CatalogRow, len(objects))
	errs := make([]error, len(objects))
	cached := make([]bool, len(objects))
	// Workers have joined before this accounting runs. Count only successfully
	// verified bodies, separately from summaries reused without a body decode.
	defer func() {
		if span == nil {
			return
		}
		cacheHits, downloaded := 0, 0
		for i, row := range results {
			if row.Key == "" {
				continue
			}
			if cached[i] {
				cacheHits++
			} else {
				downloaded++
			}
		}
		span.Count("from cache", cacheHits)
		span.Count("downloaded", downloaded)
		span.End()
	}()
	next := 0
	completed := make(chan struct{}, len(objects))
	failed := false
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range min(listConcurrency, len(objects)) {
		wg.Go(func() {
			for {
				mu.Lock()
				if failed || ctx.Err() != nil || next == len(objects) {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				var expected *listingindex.Revision
				if revision, ok := revisions[RevisionID{Key: objects[i].Key, ETag: objects[i].ETag}]; ok {
					expected = &revision
				}
				m, hash, hit, err := c.readRevision(ctx, objects[i], expected)
				cached[i] = hit
				errs[i] = err
				completed <- struct{}{}
				if err != nil {
					mu.Lock()
					failed = true
					mu.Unlock()
					return
				}
				results[i] = CatalogRow{Key: objects[i].Key, ETag: objects[i].ETag, Hash: hash, Summary: summarize(m)}
			}
		})
	}
	go func() { wg.Wait(); close(completed) }()
	finished := 0
	for range completed {
		finished++
		if c.opts.Progress != nil {
			c.opts.Progress(finished, len(objects))
		}
	}
	for i := range next {
		if errs[i] != nil {
			return nil, errs[i]
		}
		if c.opts.BodyRead != nil {
			c.opts.BodyRead(objects[i].Key, cached[i])
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// Fixed precision preserves time ordering under SQLite text comparison.
func catalogTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func (c *SQLiteSessionCatalog) readRevision(ctx context.Context, obj storage.Object, expected *listingindex.Revision) (archive.Metadata, string, bool, error) {
	if obj.ETag == "" {
		return archive.Metadata{}, "", false, errors.New("catalog requires metadata revision validators")
	}
	data, cached := c.opts.Cache.get(obj.Key, obj.ETag)
	verifiable, valid := etagMatchesBytes(obj.ETag, data)
	if cached && (!verifiable || !valid) {
		cached = false
	}
	if !cached {
		var err error
		if getter, ok := c.store.(storage.VersionedGetter); ok {
			var etag string
			data, etag, err = getter.GetVersioned(ctx, obj.Key)
			if err == nil && etag != obj.ETag {
				return archive.Metadata{}, "", false, ErrRefreshRequired
			}
		} else {
			if !verifiable {
				return archive.Metadata{}, "", false, errors.New("store cannot verify catalog metadata revisions")
			}
			data, err = c.store.Get(ctx, obj.Key)
		}
		if err != nil {
			return archive.Metadata{}, "", false, err
		}
	}
	if verifiable, valid := etagMatchesBytes(obj.ETag, data); verifiable && !valid {
		return archive.Metadata{}, "", cached, ErrRefreshRequired
	}
	if expected != nil {
		if storage.SHA256Hex(data) != expected.Hash {
			return archive.Metadata{}, "", cached, ErrRefreshRequired
		}
		if err := expected.ValidateMetadata(data); err != nil {
			return archive.Metadata{}, "", cached, err
		}
	}
	m, err := decodeMetadata(obj.Key, data)
	if err != nil {
		return archive.Metadata{}, "", cached, err
	}
	if !cached {
		c.opts.Cache.putVerified(obj.Key, obj.ETag, data)
	}
	return m, storage.SHA256Hex(data), cached, nil
}

// Query returns candidates from this handle's last successful refresh.
// Another handle replacing that generation requires refreshing this view.
func (c *SQLiteSessionCatalog) Query(ctx context.Context, q CatalogQuery) (CatalogPage, error) {
	span := trace.Start("query session catalog")
	defer span.End()
	c.viewMu.RLock()
	defer c.viewMu.RUnlock()
	if !c.viewReady {
		return CatalogPage{}, ctx.Err()
	}
	if err := c.validateRemoteQuery(ctx, q.Cursor != ""); err != nil {
		return CatalogPage{}, err
	}
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CatalogPage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var generation int64
	var epoch string
	var complete bool
	if err = tx.QueryRowContext(ctx, "SELECT generation,epoch,complete FROM catalog_state WHERE id=1").Scan(&generation, &epoch, &complete); err != nil {
		return CatalogPage{}, err
	}
	if epoch != c.viewEpoch || generation != c.viewGeneration {
		return CatalogPage{}, ErrStaleCatalogCursor
	}
	offset, token, err := catalogCursor(q, epoch, generation, c.remoteBinding)
	if err != nil {
		return CatalogPage{}, err
	}
	order, where, args := catalogQueryPredicates(q)
	needsSummaryFilter := q.Metadata.IncludeRootChildren || catalogRequiresSummaryFilter(q.Metadata.Filter)
	page := CatalogPage{Complete: complete}
	statement := "SELECT key,etag,hash,capture,activity,summary,search,lowerid,unlabeled,summary_hash FROM sessions" + where + " ORDER BY " + order //nolint:gosec // SQL fragments are fixed predicates/orders; every input is bound.
	if !needsSummaryFilter {
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM sessions"+where, args...).Scan(&page.Total); err != nil {
			return CatalogPage{}, err
		}
		if offset > page.Total {
			return CatalogPage{}, ErrStaleCatalogCursor
		}
		limit := catalogSQLLimit(q.Metadata.Limit)
		statement += " LIMIT ? OFFSET ?"
		args = append(args, limit, offset)
	}
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return CatalogPage{}, err
	}
	defer func() { _ = rows.Close() }()
	if err = fillCatalogPage(rows, q, offset, needsSummaryFilter, &page); err != nil {
		return CatalogPage{}, err
	}
	if offset > page.Total {
		return CatalogPage{}, ErrStaleCatalogCursor
	}
	if offset+len(page.Rows) < page.Total {
		page.Next = base64.RawURLEncoding.EncodeToString([]byte(token + strconv.Itoa(offset+len(page.Rows))))
	}
	if err = rows.Close(); err != nil {
		return CatalogPage{}, err
	}
	if err = tx.Commit(); err != nil {
		return CatalogPage{}, err
	}
	if err = c.validateRemoteQuery(ctx, false); err != nil {
		return CatalogPage{}, err
	}
	span.Count("summaries", len(page.Rows))
	return page, nil
}

// fillCatalogPage validates candidates before applying final typed filtering.
// Root-child queries require the complete non-date inventory before trimming.
func fillCatalogPage(rows *sql.Rows, q CatalogQuery, offset int, filterSummaries bool, page *CatalogPage) error {
	filter := q.Metadata.Filter
	if q.Metadata.IncludeRootChildren {
		filter.From, filter.To = time.Time{}, time.Time{}
	}
	var candidates []CatalogRow
	limit := q.Metadata.Limit
	for rows.Next() {
		var record catalogRecord
		if err := record.scan(rows); err != nil {
			return err
		}
		if !record.valid() {
			return errors.New("invalid session catalog summary checksum")
		}
		row := CatalogRow{Key: record.key, ETag: record.etag, Hash: record.hash}
		if err := json.Unmarshal(record.summary, &row.Summary); err != nil {
			return fmt.Errorf("invalid session catalog: %w", err)
		}
		m := row.Summary.Metadata()
		if !matches(m, filter) || q.Metadata.TopLevelOnly && m.IsChild() {
			continue
		}
		if q.Metadata.IncludeRootChildren {
			candidates = append(candidates, row)
			continue
		}
		if filterSummaries {
			page.Total++
		}
		if !filterSummaries || page.Total > offset && (limit <= 0 || len(page.Rows) < limit) {
			page.Rows = append(page.Rows, row)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if q.Metadata.IncludeRootChildren {
		candidates = SelectRootChildren(candidates, q.Metadata.Filter, func(row CatalogRow) (string, string, time.Time) {
			return row.Summary.SessionID, row.Summary.ParentSessionID, row.Summary.CapturedAt
		})
		page.Total = len(candidates)
		if offset <= page.Total {
			end := page.Total
			if limit > 0 {
				end = offset + min(limit, page.Total-offset)
			}
			page.Rows = candidates[offset:end]
		}
	}
	return nil
}

func catalogSQLLimit(limit int) int {
	if limit <= 0 {
		return -1
	}
	return limit
}

func catalogQueryPredicates(q CatalogQuery) (string, string, []any) {
	order := "capture DESC,key"
	if q.Metadata.Order == ActivityOrder {
		order = "activity DESC,capture DESC,key"
	}
	// SQL predicates are exact for these typed fields. Text stays a candidate
	// superset; Go's Unicode matcher and query-time labels decide final matches.
	where, args := catalogWhere(q.Metadata)
	var clauses strings.Builder
	clauses.WriteString(where)
	for _, word := range q.Words {
		word = strings.ToLower(word)
		clauses.WriteString(" AND (instr(search,?) > 0 OR instr(lowerid,?) = 1 OR instr(search,?) > 0 OR unlabeled = 1)")
		// Leading zeros still denote the same PR in the CLI matcher. Keep
		// original text/ID candidates and OR a canonical numeric candidate.
		args = append(args, word, word, catalogPRCandidate(word))
	}
	where = clauses.String()
	return order, where, args
}

func (c *SQLiteSessionCatalog) validateRemoteQuery(ctx context.Context, continuation bool) error {
	if c.remoteSnapshot == nil {
		return ctx.Err()
	}
	if continuation {
		return c.remoteSnapshot.ValidateContinuation(ctx)
	}
	return c.remoteSnapshot.ValidateRead(ctx)
}

func catalogCursor(q CatalogQuery, epoch string, generation int64, remoteBinding string) (int, string, error) {
	binding, err := json.Marshal(struct {
		Metadata MetadataQuery `json:"metadata"`
		Words    []string      `json:"words"`
		Remote   string        `json:"remote,omitempty"`
	}{q.Metadata, q.Words, remoteBinding})
	if err != nil {
		return 0, "", err
	}
	digest := sha256.Sum256(binding)
	token := epoch + ":" + strconv.FormatInt(generation, 10) + ":" + hex.EncodeToString(digest[:]) + ":"
	if q.Cursor == "" {
		return 0, token, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
	if err != nil || !strings.HasPrefix(string(raw), token) {
		return 0, "", ErrStaleCatalogCursor
	}
	offset, err := strconv.Atoi(strings.TrimPrefix(string(raw), token))
	if err != nil || offset < 0 {
		return 0, "", ErrStaleCatalogCursor
	}
	return offset, token, nil
}

func catalogWhere(q MetadataQuery) (string, []any) {
	clauses := []string{"1=1"}
	var args []any
	add := func(clause string, value any) { clauses = append(clauses, clause); args = append(args, value) }
	f := q.Filter
	if f.Harness != "" {
		add("json_extract(summary,'$.Harness.name') = ?", f.Harness)
	}
	if !q.IncludeRootChildren && !f.From.IsZero() {
		add("capture >= ?", catalogTime(f.From))
	}
	if !q.IncludeRootChildren && !f.To.IsZero() {
		add("capture <= ?", catalogTime(f.To))
	}
	if q.TopLevelOnly {
		clauses = append(clauses, "json_extract(summary,'$.ParentSessionID') = '' AND coalesce(json_extract(summary,'$.NativeChild'),0) = 0")
	}
	if f.Replays == ReplaysHidden {
		clauses = append(clauses, "json_type(summary,'$.Replay') = 'null'")
	}
	if f.Replays == ReplaysOnly {
		clauses = append(clauses, "json_type(summary,'$.Replay') = 'object'")
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// catalogSearch folds with Go, matching the CLI's Unicode semantics rather
// than SQLite's ASCII-only lower(). Published-name gaps are never excluded by
// word predicates because their configured project label exists only at query.
func catalogSearch(s SearchSummary) string {
	texts := []string{s.Name, s.Title, s.Branch, s.ProjectName, s.Harness.Name}
	for _, pr := range s.PullRequests {
		texts = append(texts, strconv.Itoa(pr.Number), "#"+strconv.Itoa(pr.Number))
	}
	for _, event := range s.GitActivity {
		if event.Kind == archive.GitEventPRCreated {
			texts = append(texts, strconv.Itoa(event.PRNumber), "#"+strconv.Itoa(event.PRNumber))
		}
	}
	return strings.ToLower(strings.Join(texts, "\x00"))
}

// catalogRequiresSummaryFilter identifies predicates that must be checked on
// every candidate before counting and paging the final typed result.
func catalogRequiresSummaryFilter(f Filter) bool {
	return f.Model != "" || f.Skill != "" || f.SkillSHA256 != "" || f.RequireCompleteCoverage
}

// catalogPRCandidate mirrors the CLI's positive, at-most-six-digit PR shape.
// Non-PR words reuse the original text; the final matcher removes false hits.
func catalogPRCandidate(word string) string {
	digits := strings.TrimPrefix(word, "#")
	if digits == "" || len(digits) > 6 {
		return word
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return word
		}
	}
	number, err := strconv.Atoi(digits)
	if err != nil || number <= 0 {
		return word
	}
	return strconv.Itoa(number)
}

// catalogRecord is the fixed private SQLite row layout. Every field affecting
// identity, selection and ordering participates in the integrity checksum.
type catalogRecord struct {
	key       string
	etag      string
	hash      string
	capture   string
	activity  string
	summary   []byte
	search    string
	lowerID   string
	unlabeled bool
	checksum  string
}

func (r *catalogRecord) valid() bool {
	return r.checksum != "" && catalogRecordChecksum(*r) == r.checksum
}

func (r *catalogRecord) scan(row interface{ Scan(...any) error }) error {
	return row.Scan(&r.key, &r.etag, &r.hash, &r.capture, &r.activity, &r.summary, &r.search, &r.lowerID, &r.unlabeled, &r.checksum)
}

// catalogRecordFormat versions the disposable projection. Earlier checksummed
// rows omitted native child ownership and must reload from complete headers.
const catalogRecordFormat = "session-catalog-native-child-v1"

// catalogRecordChecksum uses length-prefix framing to keep tuple boundaries
// unambiguous. It hashes raw summary bytes without decoding every warm row.
func catalogRecordChecksum(r catalogRecord) string {
	buffer := make([]byte, 0, len(r.key)+len(r.etag)+len(r.hash)+len(r.capture)+len(r.activity)+len(r.summary)+len(r.search)+len(r.lowerID)+len(catalogRecordFormat)+82)
	for _, field := range []string{catalogRecordFormat, r.key, r.etag, r.hash, r.capture, r.activity, r.search, r.lowerID} {
		buffer = binary.AppendUvarint(buffer, uint64(len(field)))
		buffer = append(buffer, field...)
	}
	buffer = binary.AppendUvarint(buffer, uint64(len(r.summary)))
	buffer = append(buffer, r.summary...)
	if r.unlabeled {
		buffer = append(buffer, 1)
	} else {
		buffer = append(buffer, 0)
	}
	return storage.SHA256Hex(buffer)
}
