package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// ReconcileBudget bounds namespace-wide discovery, separately from source work.
// Defaults read 32 physical identities/objects, 8 exact known-session checks,
// 512 catalog nodes and 16 MiB of tree/metadata bytes within 20 seconds.
type ReconcileBudget struct {
	Entries int
	Checks  int
	Nodes   int
	Bytes   int64
}

// Discovery describes attempted discovery work, including failures. No source
// bundle is downloaded by Reconcile. CompletedWalk is never an absence proof.
type Discovery struct {
	Pages            int
	Objects          int
	Metadata         int
	Checks           int
	Removed          int
	HeadReads        int
	CoordinatorReads int
	Nodes            int
	Bytes            int64
	CompletedWalk    bool
	Unchanged        bool
}

type discoveryMode string

const (
	discoveryCatalog discoveryMode = "catalog"
	discoveryLegacy  discoveryMode = "legacy"
)

type discoveryState struct {
	mode         discoveryMode
	after        string
	token        string
	root         string
	complete     string
	validated    string
	contract     string
	stable       bool
	observed     int64
	seedWork     string
	seedSessions string
	checkRoot    string
	checkedRoot  string
	seeded       int
	turn         int
}

// Reconcile observes a bounded namespace-wide slice. Composition holds LockWork
// through this call and Run. Query scope does not alter discovery authority.
// Fresh snapshots resume logical keys, never opaque snapshot cursors. Known
// identities are checked independently, so moving sweeps cannot authorize removal.
func (s *Store) Reconcile(ctx context.Context, objects storage.ObjectStore, parsers agentapi.ParsersLookup, at time.Time, budget ReconcileBudget) (result Discovery, err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if budget.Entries <= 0 {
		budget.Entries = 32
	}
	budget.Entries = min(budget.Entries, 1000)
	if budget.Checks <= 0 {
		budget.Checks = 8
	}
	budget.Checks = min(budget.Checks, 1000)
	if budget.Nodes <= 0 {
		budget.Nodes = 512
	}
	if budget.Bytes <= 0 {
		budget.Bytes = 16 << 20
	}
	var state discoveryState
	err = s.db.QueryRowContext(ctx, "SELECT mode,after_key,token,root,stable,complete_root,observed,validated,contract,seed_work,seed_sessions,seeded,check_root,checked_root,turn FROM search_discovery WHERE id=1").Scan(&state.mode, &state.after, &state.token, &state.root, &state.stable, &state.complete, &state.observed, &state.validated, &state.contract, &state.seedWork, &state.seedSessions, &state.seeded, &state.checkRoot, &state.checkedRoot, &state.turn)
	if err != nil {
		return result, err
	}
	mode := discoveryLegacy
	if reader.CatalogAuthority(objects) {
		mode = discoveryCatalog
	}
	contract := extractionContract(parsers)
	if state.mode != mode || state.contract != contract {
		state = discoveryState{mode: mode, stable: true, contract: contract}
		if err = s.saveDiscovery(ctx, state); err != nil {
			return result, err
		}
	}
	if err = s.seedKnown(ctx, &state, budget.Checks); err != nil {
		return result, err
	}
	if mode == discoveryCatalog {
		return s.reconcileCatalog(ctx, objects, parsers, at, budget, state)
	}
	return s.reconcileLegacy(ctx, objects, parsers, at, budget, state)
}

func (s *Store) reconcileCatalog(ctx context.Context, objects storage.ObjectStore, parsers agentapi.ParsersLookup, at time.Time, budget ReconcileBudget, state discoveryState) (result Discovery, err error) {
	ctx = catalog.NewReadView(ctx)
	ctx, cost, err := catalog.DiscoveryBudget(ctx, budget.Nodes, budget.Bytes)
	if err != nil {
		return result, err
	}
	defer func() {
		c := cost()
		result.Nodes = c.Nodes
		result.Bytes += c.Bytes
		result.HeadReads = c.HeadObservations
		result.CoordinatorReads = c.CoordinatorReads
	}()
	snapshot, err := catalog.OpenSnapshot(ctx, objects, nil)
	if err != nil {
		return result, err
	}
	root := digest(snapshot.Root())
	if state.complete != root {
		state.complete = ""
	}
	// Persist a finite fair turn before reads. A single failed or oversized body
	// may consume this pass, but cannot take first access from every future pass.
	turn := state.turn
	state.turn = (turn + 1) % 12
	if err = s.saveDiscovery(ctx, state); err != nil {
		return result, err
	}
	var itemErrors error
	known := func() error {
		if state.validated == "" {
			state.checkRoot = root
		} else if state.checkRoot != root {
			state.checkRoot = ""
		}
		e := s.checkKnown(ctx, objects, snapshot, parsers, at, budget.Checks, &state, &result, budget.Bytes)
		if e == nil && state.validated == "" && state.checkRoot == root && state.seeded == 3 {
			state.checkedRoot = root
		}
		return e
	}
	if state.complete == root {
		e := known()
		if discoveryMustStop(e) {
			return result, e
		}
		if err = snapshot.ValidateContinuation(ctx); err != nil {
			return result, err
		}
		result.Unchanged = true
		return result, errors.Join(e, s.saveDiscovery(ctx, state))
	}
	recent := func() error {
		page, e := snapshot.Query(ctx, catalog.Query{Index: catalog.CaptureIndex, Reverse: true}, "", 4)
		result.Pages++
		if e != nil {
			return e
		}
		var failures error
		for i := range len(page.Rows) {
			row := page.Rows[(i+turn/3)%len(page.Rows)]
			if e = s.observeCatalog(ctx, snapshot, row, parsers, at, &result); e != nil {
				state.stable = false
				failures = errors.Join(failures, e)
				if discoveryMustStop(e) {
					return failures
				}
			}
		}
		return failures
	}
	history := func() error {
		return s.reconcileHistory(ctx, snapshot, parsers, at, budget, root, &state, &result, itemErrors != nil)
	}
	stages := []func() error{known, recent, history}
	for i := range 3 {
		e := stages[(turn+i)%3]()
		if e != nil {
			state.complete = ""
			state.stable = false
			result.CompletedWalk = false
			itemErrors = errors.Join(itemErrors, e)
			if discoveryMustStop(e) {
				return result, errors.Join(itemErrors, s.saveDiscovery(ctx, state))
			}
		}
	}
	return result, errors.Join(itemErrors, s.saveDiscovery(ctx, state))
}

func (s *Store) reconcileHistory(ctx context.Context, snapshot *catalog.Snapshot, parsers agentapi.ParsersLookup, at time.Time, budget ReconcileBudget, root string, state *discoveryState, result *Discovery, priorFailure bool) error {
	var err error
	if state.after == "" {
		state.root = root
		state.stable = true
	} else if state.root != root {
		state.stable = false
	}
	rows, done, e := snapshot.ScanIdentities(ctx, state.after, budget.Entries)
	result.Pages++
	if e != nil {
		return e
	}
	var failures error
	for _, row := range rows {
		result.Objects++
		if row.Entry != nil {
			if e = s.observeCatalog(ctx, snapshot, catalog.Row{Key: row.Key, Entry: *row.Entry}, parsers, at, result); e != nil {
				if discoveryMustStop(e) && !errors.Is(e, errDiscoveryBody) {
					return e
				}
				state.stable = false
				failures = errors.Join(failures, e)
			}
		}
		state.after = row.Key
		if err = s.saveDiscovery(ctx, *state); err != nil {
			return err
		}
		if discoveryMustStop(e) {
			return failures
		}
	}
	if err = snapshot.ValidateContinuation(ctx); err != nil {
		return err
	}
	if done {
		result.CompletedWalk = true
		state.after = ""
		state.observed = at.UnixNano()
		if state.stable && failures == nil && !priorFailure {
			state.complete = root
		}
		state.root = ""
	}
	return failures
}

var errDiscoveryBody = errors.New("search metadata body exceeds this discovery pass; coverage remains partial")

// Leave a short part of the existing pass deadline for recording a failed
// immutable key. A caller cancellation still cannot advance past that key.
func discoveryMetadataContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		deadline = deadline.Add(-500 * time.Millisecond)
		if !deadline.After(time.Now()) {
			return nil, nil, errMetadataBudget
		}
		child, cancel := context.WithDeadline(ctx, deadline)
		return child, cancel, nil
	}
	return ctx, func() {}, nil
}

func discoveryMetadataError(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return errors.Join(errDiscoveryBody, err)
	}
	return discoveryBodyError(err)
}

func readDiscoveryMetadata(ctx context.Context, objects storage.ObjectStore, key string, limit int64) (boundedMetadata, error) {
	child, cancel, err := discoveryMetadataContext(ctx)
	if err != nil {
		return boundedMetadata{}, err
	}
	defer cancel()
	body, err := readBoundedMetadata(child, objects, key, limit)
	return body, discoveryMetadataError(ctx, err)
}

func discoveryBodyError(err error) error {
	if errors.Is(err, catalog.ErrPickerBudget) || errors.Is(err, errMetadataBudget) || errors.Is(err, storage.ErrObjectTooLarge) {
		return errors.Join(errDiscoveryBody, err)
	}
	return err
}

// A failed immutable body is retried on the next sweep. Authority and pass
// failures cannot establish a safe checkpoint past the current item.
func discoveryMustStop(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, catalog.ErrStaleCursor) || errors.Is(err, catalog.ErrPickerBudget) || errors.Is(err, errMetadataBudget) || errors.Is(err, storage.ErrObjectTooLarge)
}

func (s *Store) reconcileLegacy(ctx context.Context, objects storage.ObjectStore, parsers agentapi.ParsersLookup, at time.Time, budget ReconcileBudget, state discoveryState) (result Discovery, err error) {
	pager, ok := objects.(storage.PageLister)
	if !ok {
		return result, errors.New("bounded search discovery requires paged object listing")
	}
	turn := state.turn
	state.turn = (turn + 1) % 12
	if err = s.saveDiscovery(ctx, state); err != nil {
		return result, err
	}
	var itemErrors error
	known := func() error {
		if err = s.checkKnown(ctx, objects, nil, parsers, at, budget.Checks, &state, &result, budget.Bytes); err != nil {
			return err
		}
		return nil
	}
	list := func() error {
		pageLimit := budget.Entries
		if pageLimit < 1 || pageLimit > 1000 {
			return errors.New("search page limit must be 1..1000")
		}
		page, e := pager.ListPage(ctx, "sessions/", state.token, int32(pageLimit))
		result.Pages++
		if e != nil {
			state.token = ""
			state.after = ""
			if saveErr := s.saveDiscovery(ctx, state); saveErr != nil {
				return saveErr
			}
			return e
		}
		if len(page.Objects) > budget.Entries {
			return errors.New("object store exceeded search discovery page limit")
		}
		for _, object := range page.Objects {
			result.Objects++
			// A bounded body failure resumes inside this same physical page. Listing
			// remains mutable and never supplies a snapshot completeness proof.
			if object.Key <= state.after {
				continue
			}
			if strings.HasSuffix(object.Key, "/metadata.json") {
				var prior string
				lookup := s.db.QueryRowContext(ctx, "SELECT revision FROM search_observed WHERE key=?", object.Key).Scan(&prior)
				if lookup != nil && !errors.Is(lookup, sql.ErrNoRows) {
					return lookup
				}
				if prior != "" && prior == object.ETag {
					if err = s.observeCached(ctx, object.Key, parsers, at); err != nil {
						return err
					}
					continue
				}
				m, e := readDiscoveryMetadata(ctx, objects, object.Key, budget.Bytes-result.Bytes)
				result.Metadata++
				result.Bytes += int64(len(m.raw))
				if e != nil && !errors.Is(e, storage.ErrNotFound) {
					if discoveryMustStop(e) {
						if errors.Is(discoveryBodyError(e), errDiscoveryBody) {
							state.after = object.Key
							return errors.Join(itemErrors, e, s.saveDiscovery(ctx, state))
						}
						return e
					}
					itemErrors = errors.Join(itemErrors, e)
				}
				if e == nil {
					if err = s.recordObservation(ctx, object.Key, m.revision, m.metadata, parsers, at); err != nil {
						return err
					}
				}
			}
		}
		state.after = ""
		state.token = page.Next
		if page.Next == "" {
			result.CompletedWalk = true
			state.observed = at.UnixNano()
		}
		return s.saveDiscovery(ctx, state)
	}
	stages := []func() error{known, list}
	for i := range 2 {
		if e := stages[(turn+i)%2](); e != nil {
			itemErrors = errors.Join(itemErrors, e)
			if discoveryMustStop(e) {
				return result, errors.Join(itemErrors, s.saveDiscovery(ctx, state))
			}
		}
	}
	return result, errors.Join(itemErrors, s.saveDiscovery(ctx, state))
}

func extractionContract(parsers agentapi.ParsersLookup) string {
	versions := []string{archive.FilterVersion, ExtractorVersion}
	for _, name := range []string{"claude", "codex", "cursor"} {
		if parsers != nil {
			if p, ok := parsers.LookupParser(name); ok {
				versions = append(versions, name, p.Version())
			}
		}
	}
	return digest(versions)
}

func (s *Store) saveDiscovery(ctx context.Context, state discoveryState) error {
	_, err := s.db.ExecContext(ctx, "UPDATE search_discovery SET mode=?,after_key=?,token=?,root=?,stable=?,complete_root=?,observed=?,validated=?,contract=?,seed_work=?,seed_sessions=?,seeded=?,check_root=?,checked_root=?,turn=? WHERE id=1", state.mode, state.after, state.token, state.root, state.stable, state.complete, state.observed, state.validated, state.contract, state.seedWork, state.seedSessions, state.seeded, state.checkRoot, state.checkedRoot, state.turn)
	return err
}

func (s *Store) observeCatalog(ctx context.Context, snapshot *catalog.Snapshot, row catalog.Row, parsers agentapi.ParsersLookup, at time.Time, result *Discovery) error {
	var prior string
	err := s.db.QueryRowContext(ctx, "SELECT revision FROM search_observed WHERE key=?", row.Key).Scan(&prior)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if prior == row.Entry.Revision {
		return s.observeCached(ctx, row.Key, parsers, at)
	}
	metadataCtx, cancel, err := discoveryMetadataContext(ctx)
	if err != nil {
		return err
	}
	m, err := reader.ReadCatalogSelection(metadataCtx, snapshot, row)
	cancel()
	result.Metadata++
	if err != nil {
		return discoveryMetadataError(ctx, err)
	}
	if err = snapshot.ValidateContinuation(ctx); err != nil {
		return err
	}
	return s.recordObservation(ctx, row.Key, row.Entry.Revision, m, parsers, at)
}

func (s *Store) recordObservation(ctx context.Context, key, revision string, m archive.Metadata, parsers agentapi.ParsersLookup, at time.Time) error {
	canonical, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if err != nil || canonical != key {
		return errors.New("search metadata disagrees with canonical session identity")
	}
	if err = s.Observe(ctx, m, parsers, at, 0); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO search_observed(key,session,revision,observed) VALUES(?,?,?,?) ON CONFLICT(key) DO UPDATE SET session=excluded.session,revision=excluded.revision,observed=excluded.observed", key, m.Harness.Name+"/"+m.SessionID, revision, at.UnixNano())
	return err
}

var errMetadataBudget = errors.New("search metadata read budget exhausted; retry a fresh pass")

type boundedMetadata struct {
	metadata archive.Metadata
	raw      []byte
	revision string
}

func readBoundedMetadata(ctx context.Context, objects storage.ObjectStore, key string, limit int64) (boundedMetadata, error) {
	getter, ok := objects.(storage.LimitedGetter)
	if !ok {
		return boundedMetadata{}, errors.New("search metadata requires bounded reads")
	}
	// Reserve the bounded getter's one-byte overflow probe in the pass budget.
	limit--
	if limit <= 0 {
		return boundedMetadata{}, errMetadataBudget
	}
	var raw []byte
	var revision string
	var err error
	if versioned, ok := objects.(storage.LimitedVersionedGetter); ok {
		raw, revision, err = versioned.GetLimitedVersioned(ctx, key, min(limit, 32<<20))
	} else {
		raw, err = getter.GetLimited(ctx, key, min(limit, 32<<20))
	}
	if errors.Is(err, storage.ErrObjectTooLarge) && limit < 32<<20 {
		return boundedMetadata{raw: raw}, errMetadataBudget
	}
	if err != nil {
		return boundedMetadata{raw: raw}, err
	}
	m, err := reader.ReadMetadata(ctx, metadataBody{ObjectStore: objects, key: key, raw: raw}, key)
	return boundedMetadata{metadata: m, raw: raw, revision: revision}, err
}

type metadataBody struct {
	storage.ObjectStore
	key string
	raw []byte
}

func (b metadataBody) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key != b.key {
		return nil, errors.New("unexpected search metadata key")
	}
	return b.raw, nil
}

func (s *Store) checkKnown(ctx context.Context, objects storage.ObjectStore, snapshot *catalog.Snapshot, parsers agentapi.ParsersLookup, at time.Time, limit int, state *discoveryState, result *Discovery, bytes int64) (checkErr error) {
	var active string
	defer func() {
		if checkErr == nil || (discoveryMustStop(checkErr) && !errors.Is(checkErr, errDiscoveryBody)) || active == "" {
			return
		}
		state.validated = active
		state.checkRoot = ""
		state.checkedRoot = ""
		checkErr = errors.Join(checkErr, s.saveDiscovery(ctx, *state))
	}()
	rows, err := s.db.QueryContext(ctx, "SELECT key,session FROM search_observed WHERE key>? ORDER BY key LIMIT ?", state.validated, limit)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type known struct {
		key string
		id  string
	}
	keys := make([]known, 0, limit)
	for rows.Next() {
		var k known
		if err = rows.Scan(&k.key, &k.id); err != nil {
			_ = rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, k := range keys {
		active = k.key
		result.Checks++
		var m archive.Metadata
		var revision string
		var absent bool
		if snapshot != nil {
			entry, e := snapshot.Find(ctx, k.key)
			if e != nil {
				return e
			}
			absent = entry == nil
			if !absent {
				revision = entry.Revision
				var prior string
				if e = s.db.QueryRowContext(ctx, "SELECT revision FROM search_observed WHERE key=?", k.key).Scan(&prior); e != nil {
					return e
				}
				if prior == revision {
					if e = s.observeCached(ctx, k.key, parsers, at); e != nil {
						return e
					}
					state.validated = k.key
					if e = s.saveDiscovery(ctx, *state); e != nil {
						return e
					}
					continue
				}
				metadataCtx, cancel, e := discoveryMetadataContext(ctx)
				if e != nil {
					return e
				}
				m, e = reader.ReadCatalogSelection(metadataCtx, snapshot, catalog.Row{Key: k.key, Entry: *entry})
				cancel()
				result.Metadata++
				if e != nil {
					return discoveryMetadataError(ctx, e)
				}
			}
		} else {
			body, e := readDiscoveryMetadata(ctx, objects, k.key, bytes-result.Bytes)
			result.Metadata++
			result.Bytes += int64(len(body.raw))
			absent = errors.Is(e, storage.ErrNotFound)
			if e != nil && !absent {
				return discoveryBodyError(e)
			}
			m = body.metadata
			revision = body.revision
		}
		if snapshot != nil {
			if err = snapshot.ValidateContinuation(ctx); err != nil {
				return err
			}
		}
		if absent {
			if err = s.forget(ctx, k.key, k.id); err != nil {
				return err
			}
			result.Removed++
		} else if err = s.recordObservation(ctx, k.key, revision, m, parsers, at); err != nil {
			return err
		}
		state.validated = k.key
		if err = s.saveDiscovery(ctx, *state); err != nil {
			return err
		}
	}
	if len(keys) < limit {
		state.validated = ""
	}
	return nil
}

func (s *Store) forget(ctx context.Context, key, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.removeSessionTx(ctx, tx, id); err != nil {
		return err
	}
	for _, statement := range []string{"DELETE FROM search_sources WHERE session=?", "DELETE FROM search_work WHERE id=?"} {
		if _, err = tx.ExecContext(ctx, statement, id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM search_observed WHERE key=?", key); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) observeCached(ctx context.Context, key string, parsers agentapi.ParsersLookup, at time.Time) error {
	var raw []byte
	if err := s.db.QueryRowContext(ctx, "SELECT w.metadata FROM search_work w JOIN search_observed o ON o.session=w.id WHERE o.key=?", key).Scan(&raw); err != nil {
		return err
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return err
	}
	if err := s.Observe(ctx, metadata, parsers, at, 0); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "UPDATE search_observed SET observed=? WHERE key=?", at.UnixNano(), key)
	return err
}

func (s *Store) seedKnown(ctx context.Context, state *discoveryState, limit int) error {
	// Bare session ids and canonical metadata keys have different prefix ordering.
	// Until seeding finishes, new identities may land behind the check cursor.
	if state.seeded != 3 {
		state.validated = ""
		state.checkRoot = ""
		state.checkedRoot = ""
	}
	if state.seeded&1 == 0 {
		after, done, err := s.seedRows(ctx, "SELECT id,json_extract(metadata,'$.harness.name'),json_extract(metadata,'$.session_id') FROM search_work WHERE id>? ORDER BY id LIMIT ?", state.seedWork, limit)
		if err != nil {
			return err
		}
		state.seedWork = after
		if done {
			state.seeded |= 1
		}
	}
	if state.seeded&2 == 0 {
		after, done, err := s.seedRows(ctx, "SELECT id,harness,session_id FROM search_sessions WHERE id>? ORDER BY id LIMIT ?", state.seedSessions, limit)
		if err != nil {
			return err
		}
		state.seedSessions = after
		if done {
			state.seeded |= 2
		}
	}
	return s.saveDiscovery(ctx, *state)
}

func (s *Store) seedRows(ctx context.Context, query, after string, limit int) (string, bool, error) {
	rows, err := s.db.QueryContext(ctx, query, after, limit)
	if err != nil {
		return after, false, err
	}
	defer func() { _ = rows.Close() }()
	type identity struct {
		id      string
		harness string
		session string
	}
	identities := make([]identity, 0, limit)
	for rows.Next() {
		var row identity
		if err = rows.Scan(&row.id, &row.harness, &row.session); err != nil {
			_ = rows.Close()
			return after, false, err
		}
		identities = append(identities, row)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return after, false, err
	}
	for _, row := range identities {
		key, e := archive.MetadataObjectKey(row.harness, row.session)
		if e != nil {
			return after, false, e
		}
		if _, err = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO search_observed(key,session,revision,observed) VALUES(?,?,'',0)", key, row.id); err != nil {
			return after, false, err
		}
		after = row.id
	}
	return after, len(identities) < limit, nil
}
