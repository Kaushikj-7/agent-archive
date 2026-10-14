package search

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Budget bounds a synchronous worker pass. Callers supply a deadline as well.
// Zero values select conservative defaults; no worker goroutine is started.
type Budget struct {
	Sources         int
	Sessions        int
	CompressedBytes int
	Limits          reader.Limits
	VerifyCurrent   bool // Production callers must recheck fresh authority before install.
}

// Progress describes actual work and the known pending/failed state after a pass.
type Progress struct {
	Loaded          int
	Reused          int
	CompressedBytes int
	Committed       int
	Writes          WriteStats
	Pending         int
	Failed          int
	Gaps            []Gap
	MetadataChecks  int
	CatalogNodes    int
	MetadataBytes   int64
}

type sourceRequest struct {
	Ownership string                  `json:"Ownership"`
	Revision  string                  `json:"Revision"`
	Reference archive.SourceReference `json:"Reference"`
	Filter    string                  `json:"Filter"`
	Schema    int                     `json:"Schema"`
	Parser    string                  `json:"Parser"`
	Extractor string                  `json:"Extractor"`
}

func digest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func requests(metadata archive.Metadata, parsers agentapi.ParsersLookup) ([]sourceRequest, error) {
	_, err := metadata.SourceReferences()
	if err != nil {
		return nil, err
	}
	version := "unavailable"
	if parsers != nil {
		if parser, ok := parsers.LookupParser(metadata.Harness.Name); ok {
			version = parser.Version()
		}
	}
	out := []sourceRequest{{Reference: metadata.SourceBundle, Filter: metadata.FilterVersion, Parser: version, Extractor: ExtractorVersion, Ownership: sourceOwnership(metadata, metadata.Harness, metadata.CapturedAt)}}
	if metadata.History != nil {
		revisions, e := reader.Revisions(metadata)
		if e != nil {
			return nil, e
		}
		out = nil
		for _, revision := range revisions {
			harness := archive.Harness{Name: metadata.Harness.Name}
			if revision.Active {
				harness = metadata.Harness
			}
			out = append(out, sourceRequest{Revision: revision.RevisionID, Reference: revision.Source, Filter: revision.FilterVersion, Schema: revision.SourceSchemaVersion, Parser: version, Extractor: ExtractorVersion, Ownership: sourceOwnership(metadata, harness, revision.CapturedAt)})
		}
	}
	return out, nil
}

// sourceOwnership mirrors reader's identity checks. Display facts such as title,
// repository labels and PR links cannot change this verification fingerprint.
func sourceOwnership(metadata archive.Metadata, harness archive.Harness, capturedAt time.Time) string {
	return digest([]any{metadata.SessionID, metadata.NativeSessionID, metadata.ProjectID, metadata.ParentSessionID, harness, capturedAt})
}

func newUnknownPrivacy(req sourceRequest, previous []sourceRequest) bool {
	// Legacy sidecars may omit provenance supplied by verified immutable bytes.
	// Only an unchanged reader-bound request can retain that projection.
	return historicalVisibilityUnavailable(req.Filter) && !slices.Contains(previous, req)
}

// Observe records an authoritative snapshot. Composition holds LockWork across
// authoritative reads and Observe/Run; arrival order cannot establish causality.
// Repeated observations do not reset eligibility; updates coalesce until due.
// Unknown/changed privacy provenance immediately removes old searchable text.
func (s *Store) Observe(ctx context.Context, metadata archive.Metadata, parsers agentapi.ParsersLookup, at time.Time, interval time.Duration) error {
	reqs, err := requests(metadata, parsers)
	if err != nil {
		return err
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	// Search metadata is independent of source extraction identity.
	metadataID := digest([]any{archive.DisplayTitle(metadata), metadata.ProjectID, metadata.Harness.Name, metadata.CapturedAt, metadata.Replay, metadata.NativeChild, metadata.ParentSessionID, metadata.RepoKey, metadata.ProjectName, metadata.State})
	sourceID := digest(reqs)
	desired := digest([]any{metadataID, sourceID})
	id := metadata.Harness.Name + "/" + metadata.SessionID
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var previous []byte
	var oldDesired, oldSourceID string
	var touched int64
	err = tx.QueryRowContext(ctx, "SELECT metadata,desired,touched,source_identity FROM search_work WHERE id=?", id).Scan(&previous, &oldDesired, &touched, &oldSourceID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err = s.observeIdentityTx(ctx, tx, metadata, id, at, oldDesired != desired && !bytes.Equal(previous, data)); err != nil {
		return err
	}
	if oldDesired == desired {
		return tx.Commit()
	}
	if len(previous) == 0 {
		if err = s.removeSessionTx(ctx, tx, id); err != nil {
			return err
		}
	}
	if len(previous) > 0 {
		if err = s.invalidateObservation(ctx, tx, id, metadata, reqs, parsers, previous, oldSourceID); err != nil {
			return err
		}
	}
	if err = s.pruneSourcesTx(ctx, tx, id, reqs); err != nil {
		return err
	}
	due := at.UnixNano()
	if touched > 0 && interval > 0 {
		due = max(due, touched+int64(interval))
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO search_work(id,metadata,desired,source_identity,generation,due,touched,priority) VALUES(?,?,?,?,1,?,?,?) ON CONFLICT(id) DO UPDATE SET metadata=excluded.metadata,desired=excluded.desired,source_identity=excluded.source_identity,priority=excluded.priority,generation=generation+1,due=CASE WHEN desired!=indexed THEN due ELSE excluded.due END,failures=0,last_error='',blocked=0`, id, data, desired, sourceID, due, at.UnixNano(), metadata.State != archive.MetadataStateClosed && metadata.State != archive.MetadataStateIdle)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) observeIdentityTx(ctx context.Context, tx *sql.Tx, metadata archive.Metadata, id string, at time.Time, changed bool) error {
	key, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO search_observed(key,session,revision,observed) VALUES(?,?,'',?) ON CONFLICT(key) DO UPDATE SET observed=excluded.observed,revision=CASE WHEN ? THEN '' ELSE revision END", key, id, at.UnixNano(), changed)
	return err
}

func (s *Store) removeSessionTx(ctx context.Context, tx *sql.Tx, id string) error {
	for _, statement := range []string{"DELETE FROM search_fts WHERE rowid IN (SELECT id FROM search_passages WHERE session=?)", "DELETE FROM search_passages WHERE session=?", "DELETE FROM search_sessions WHERE id=?"} {
		if _, err := tx.ExecContext(ctx, statement, id); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, "UPDATE search_state SET generation=generation+1 WHERE id=1")
	return err
}

type workItem struct {
	id             string
	sourceID       string
	indexedSources string
	metadata       archive.Metadata
	generation     int64
	failures       int
}

func (s *Store) nextWork(ctx context.Context, at time.Time) (workItem, error) {
	item := workItem{}
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT id,metadata,generation,failures,source_identity,indexed_sources FROM search_work INDEXED BY search_work_eligible WHERE desired!=indexed AND blocked=0 AND due<=? ORDER BY touched,priority,id LIMIT 1`, at.UnixNano()).Scan(&item.id, &data, &item.generation, &item.failures, &item.sourceID, &item.indexedSources)
	if err != nil {
		return item, err
	}
	err = json.Unmarshal(data, &item.metadata)
	return item, err
}

// Run performs bounded resumable work over observed snapshots. Downloads and
// parsing happen without a database transaction. Composition holds LockWork.
// Source staging and final
// membership replacement both reject superseded generations atomically.
func (s *Store) Run(ctx context.Context, objects storage.ObjectStore, parsers agentapi.ParsersLookup, at time.Time, budget Budget) (progress Progress, runErr error) {
	if budget.VerifyCurrent {
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		ctx = bounded
	}
	progress = Progress{}
	if budget.VerifyCurrent && reader.CatalogAuthority(objects) {
		bounded, cost, err := catalog.DiscoveryBudget(ctx, 512, 16<<20)
		if err != nil {
			return progress, err
		}
		ctx = bounded
		defer func() {
			consumed := cost()
			progress.CatalogNodes = consumed.Nodes
			progress.MetadataBytes += consumed.Bytes
		}()
	}
	if budget.Sources <= 0 {
		budget.Sources = 4
	}
	if budget.Sessions <= 0 {
		budget.Sessions = 16
	}
	if budget.CompressedBytes <= 0 {
		budget.CompressedBytes = maxSessionBytes
	}
	for range budget.Sessions {
		if err := ctx.Err(); err != nil {
			return progress, err
		}
		item, err := s.nextWork(ctx, at)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return progress, err
		}
		done, err := s.runItem(ctx, objects, parsers, at, budget, item, &progress)
		if err != nil {
			if ctx.Err() != nil {
				return progress, ctx.Err()
			}
			if errors.Is(err, ErrChanged) {
				if err = s.touchWork(ctx, item, at); err != nil {
					return progress, err
				}
				continue
			}
			if e := s.failWork(ctx, item, at, err); e != nil {
				return progress, e
			}
			progress.Failed++
			continue
		}
		if done {
			progress.Committed++
		}
		if progress.Loaded >= budget.Sources || progress.CompressedBytes >= budget.CompressedBytes {
			break
		}
	}
	err := s.db.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(failures>0),0) FROM search_work WHERE desired!=indexed").Scan(&progress.Pending, &progress.Failed)
	return progress, err
}

func (s *Store) runItem(ctx context.Context, objects storage.ObjectStore, parsers agentapi.ParsersLookup, at time.Time, budget Budget, item workItem, progress *Progress) (bool, error) {
	reqs, err := requests(item.metadata, parsers)
	if err != nil {
		return false, err
	}
	if item.sourceID == item.indexedSources {
		if budget.VerifyCurrent {
			if err = s.verifyCurrent(ctx, objects, parsers, at, item, progress); err != nil {
				return false, err
			}
		}
		return s.commitWork(ctx, at, item, reqs, nil, true, progress)
	}
	compressed := 0
	for _, req := range reqs {
		if req.Reference.CompressedBytes > maxSessionBytes-compressed {
			return false, storage.ErrObjectTooLarge
		}
		compressed += req.Reference.CompressedBytes
	}
	projections := make([]Projection, 0, len(reqs))
	totalText := 0
	totalPassages := 0
	for _, req := range reqs {
		var p Projection
		var data []byte
		loadedNew := false
		err = s.db.QueryRowContext(ctx, "SELECT projection FROM search_sources WHERE session=? AND identity=?", item.id, digest(req)).Scan(&data)
		if errors.Is(err, sql.ErrNoRows) {
			if progress.Loaded >= budget.Sources || req.Reference.CompressedBytes > budget.CompressedBytes-progress.CompressedBytes {
				// A single source larger than the pass budget is explicitly blocked.
				if progress.Loaded == 0 && req.Reference.CompressedBytes > budget.CompressedBytes {
					return false, storage.ErrObjectTooLarge
				}
				return false, s.touchWork(ctx, item, at)
			}
			loaded, e := LoadProjection(ctx, objects, parsers, s.destination, item.metadata, req.Revision, budget.Limits)
			progress.Loaded++
			progress.CompressedBytes += req.Reference.CompressedBytes
			if e != nil {
				if budget.VerifyCurrent {
					if check := s.verifyCurrent(ctx, objects, parsers, at, item, progress); check != nil {
						return false, check
					}
				}
				return false, e
			}
			p = loaded
			loadedNew = true
		} else if err != nil {
			return false, err
		} else {
			progress.Reused++
			if err = json.Unmarshal(data, &p); err != nil {
				return false, err
			}
		}
		if p.Gap != nil {
			progress.Gaps = append(progress.Gaps, *p.Gap)
		}
		if len(p.Passages) > maxSessionPassages-totalPassages {
			return false, storage.ErrObjectTooLarge
		}
		totalPassages += len(p.Passages)
		for _, passage := range p.Passages {
			if len(passage.Text) > maxSessionBytes-totalText {
				return false, storage.ErrObjectTooLarge
			}
			totalText += len(passage.Text)
		}
		if loadedNew {
			data, err = json.Marshal(p)
			if err != nil {
				return false, err
			}
			if err = s.stageSource(ctx, item, digest(req), data); err != nil {
				return false, err
			}
		}
		projections = append(projections, p)
	}
	if budget.VerifyCurrent {
		if err = s.verifyCurrent(ctx, objects, parsers, at, item, progress); err != nil {
			return false, err
		}
	}
	return s.commitWork(ctx, at, item, reqs, projections, false, progress)
}

func (s *Store) commitWork(ctx context.Context, at time.Time, item workItem, reqs []sourceRequest, projections []Projection, metadataOnly bool, progress *Progress) (bool, error) {
	var prepared *sessionProjection
	if !metadataOnly {
		projected, err := s.prepareSession(item.metadata, projections)
		if err != nil {
			return false, err
		}
		prepared = &projected
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.expectWork(ctx, tx, item); err != nil {
		return false, err
	}
	stats, err := s.replaceTx(ctx, tx, item.metadata, prepared)
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE search_work SET indexed=desired,indexed_sources=source_identity,touched=max(?,(SELECT coalesce(max(touched),0)+1 FROM search_work)),failures=0,last_error='',blocked=0 WHERE id=?", at.UnixNano(), item.id); err != nil {
		return false, err
	}
	if err = s.pruneSourcesTx(ctx, tx, item.id, reqs); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	progress.Writes.Deleted += stats.Deleted
	progress.Writes.Inserted += stats.Inserted
	progress.Writes.Unchanged = stats.Unchanged
	return true, nil
}

func (s *Store) expectWork(ctx context.Context, tx *sql.Tx, item workItem) error {
	var generation int64
	if err := tx.QueryRowContext(ctx, "SELECT generation FROM search_work WHERE id=?", item.id).Scan(&generation); err != nil {
		return err
	}
	if generation != item.generation {
		return ErrChanged
	}
	return nil
}

func (s *Store) stageSource(ctx context.Context, item workItem, identity string, data []byte) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.expectWork(ctx, tx, item); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO search_sources VALUES(?,?,?)", item.id, identity, data); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) touchWork(ctx context.Context, item workItem, at time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE search_work SET touched=max(?,(SELECT coalesce(max(touched),0)+1 FROM search_work)) WHERE id=?", at.UnixNano(), item.id)
	return err
}

func (s *Store) failWork(ctx context.Context, item workItem, at time.Time, cause error) error {
	delay := time.Second << min(item.failures, 8)
	message := "source_unavailable"
	blocked := false
	if errors.Is(cause, storage.ErrObjectTooLarge) || errors.Is(cause, archive.ErrSourceTooLarge) {
		message = "source_or_projection_too_large"
		blocked = true
	}
	// Do not persist native source contents through an arbitrary error string.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record search retry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE search_work SET failures=failures+1,last_error=?,due=?,touched=?,blocked=? WHERE id=? AND generation=?`, message, at.Add(delay).UnixNano(), at.UnixNano(), blocked, item.id, item.generation)
	if err != nil {
		return fmt.Errorf("record search retry: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if blocked && changed > 0 {
		// Permanently impossible work must not retain private staged text.
		// The generation-guarded update and cache deletion share this write lock.
		if _, err = tx.ExecContext(ctx, "DELETE FROM search_sources WHERE session=?", item.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) pruneSourcesTx(ctx context.Context, tx *sql.Tx, id string, reqs []sourceRequest) error {
	identities := make([]string, 0, len(reqs))
	for _, req := range reqs {
		identities = append(identities, digest(req))
	}
	membership, err := json.Marshal(identities)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM search_sources WHERE session=? AND identity NOT IN (SELECT value FROM json_each(?))", id, membership)
	return err
}

func (s *Store) verifyCurrent(ctx context.Context, objects storage.ObjectStore, parsers agentapi.ParsersLookup, at time.Time, item workItem, progress *Progress) error {
	key, err := archive.MetadataObjectKey(item.metadata.Harness.Name, item.metadata.SessionID)
	if err != nil {
		return err
	}
	progress.MetadataChecks++
	var m archive.Metadata
	if reader.CatalogAuthority(objects) {
		ctx = catalog.NewReadView(ctx)
		snapshot, e := catalog.OpenSnapshot(ctx, objects, nil)
		if e != nil {
			return e
		}
		entry, e := snapshot.Find(ctx, key)
		if e != nil {
			return e
		}
		if entry == nil {
			if e = s.forget(ctx, key, item.id); e != nil {
				return e
			}
			return ErrChanged
		}
		m, e = reader.ReadCatalogSelection(ctx, snapshot, catalog.Row{Key: key, Entry: *entry})
		if e != nil {
			return e
		}
		if e = snapshot.ValidateContinuation(ctx); e != nil {
			return e
		}
	} else {
		body, e := readBoundedMetadata(ctx, objects, key, (16<<20)-progress.MetadataBytes)
		progress.MetadataBytes += int64(len(body.raw))
		if errors.Is(e, storage.ErrNotFound) {
			if e = s.forget(ctx, key, item.id); e != nil {
				return e
			}
			return ErrChanged
		}
		if e != nil {
			return e
		}
		m = body.metadata
	}
	canonical, e := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
	if e != nil || canonical != key {
		return errors.New("search metadata disagrees with canonical session identity")
	}
	if err = s.Observe(ctx, m, parsers, at, 0); err != nil {
		return err
	}
	var generation int64
	if err = s.db.QueryRowContext(ctx, "SELECT generation FROM search_work WHERE id=?", item.id).Scan(&generation); err != nil {
		return err
	}
	if generation != item.generation {
		return ErrChanged
	}
	return nil
}

func (s *Store) invalidateObservation(ctx context.Context, tx *sql.Tx, id string, metadata archive.Metadata, reqs []sourceRequest, parsers agentapi.ParsersLookup, previous []byte, oldSourceID string) error {
	var old archive.Metadata
	var err error
	if err = json.Unmarshal(previous, &old); err != nil {
		return err
	}
	oldReqs, e := requests(old, parsers)
	if e != nil {
		return e
	}
	// Recompute old metadata using the running interpretation. A mismatch with
	// its stored source identity proves a local parser/extractor contract change.
	privacyChanged := metadata.FilterVersion != old.FilterVersion || digest(oldReqs) != oldSourceID
	membershipChanged := len(reqs) != len(oldReqs)
	for _, oldReq := range oldReqs[min(1, len(oldReqs)):] {
		if !slices.Contains(reqs, oldReq) {
			membershipChanged = true
		}
	}
	for i, req := range reqs {
		if newUnknownPrivacy(req, oldReqs) || (i < len(oldReqs) && req.Revision == oldReqs[i].Revision && (req.Filter != oldReqs[i].Filter || req.Schema != oldReqs[i].Schema)) {
			privacyChanged = true
		}
	}
	if membershipChanged || privacyChanged {
		// Removed rows cannot count as indexed if a later observation restores
		// the previous desired snapshot before replacement work runs.
		if _, err = tx.ExecContext(ctx, "UPDATE search_work SET indexed='',indexed_sources='' WHERE id=?", id); err != nil {
			return err
		}
		if err = s.removeSessionTx(ctx, tx, id); err != nil {
			return err
		}
	}
	if privacyChanged {
		if _, err = tx.ExecContext(ctx, "DELETE FROM search_sources WHERE session=?", id); err != nil {
			return err
		}
	}
	return nil
}
