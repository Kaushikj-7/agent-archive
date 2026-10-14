package search

import (
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
			out = append(out, sourceRequest{Revision: revision.RevisionID, Reference: revision.Source, Filter: revision.FilterVersion, Schema: revision.SourceSchemaVersion, Parser: version, Extractor: ExtractorVersion, Ownership: sourceOwnership(metadata, archive.Harness{Name: metadata.Harness.Name}, revision.CapturedAt)})
		}
	}
	return out, nil
}

// sourceOwnership mirrors reader's identity checks. Display facts such as title,
// repository labels and PR links cannot change this verification fingerprint.
func sourceOwnership(metadata archive.Metadata, harness archive.Harness, capturedAt time.Time) string {
	return digest([]any{metadata.SessionID, metadata.NativeSessionID, metadata.ProjectID, metadata.ParentSessionID, harness, capturedAt})
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
	var oldDesired string
	var touched int64
	err = tx.QueryRowContext(ctx, "SELECT metadata,desired,touched FROM search_work WHERE id=?", id).Scan(&previous, &oldDesired, &touched)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
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
		var old archive.Metadata
		if err = json.Unmarshal(previous, &old); err != nil {
			return err
		}
		oldReqs, e := requests(old, parsers)
		if e != nil {
			return e
		}
		privacyChanged := false
		membershipChanged := len(reqs) != len(oldReqs)
		for _, oldReq := range oldReqs[min(1, len(oldReqs)):] {
			if !slices.Contains(reqs, oldReq) {
				membershipChanged = true
			}
		}
		for i, req := range reqs {
			if historicalVisibilityUnavailable(req.Filter) || (i < len(oldReqs) && req.Revision == oldReqs[i].Revision && req.Filter != oldReqs[i].Filter) {
				privacyChanged = true
			}
		}
		if membershipChanged || privacyChanged {
			if _, err = tx.ExecContext(ctx, "UPDATE search_work SET indexed_sources='' WHERE id=?", id); err != nil {
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
func (s *Store) Run(ctx context.Context, objects storage.ObjectStore, parsers agentapi.ParsersLookup, at time.Time, budget Budget) (Progress, error) {
	progress := Progress{}
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
				return false, e
			}
			p = loaded
			data, e = json.Marshal(p)
			if e != nil {
				return false, e
			}
			if e = s.stageSource(ctx, item, digest(req), data); e != nil {
				return false, e
			}
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
		projections = append(projections, p)
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
	if errors.Is(cause, storage.ErrObjectTooLarge) {
		message = "source_or_projection_too_large"
		blocked = true
	}
	// Do not persist native source contents through an arbitrary error string.
	_, err := s.db.ExecContext(ctx, `UPDATE search_work SET failures=failures+1,last_error=?,due=?,touched=?,blocked=? WHERE id=? AND generation=?`, message, at.Add(delay).UnixNano(), at.UnixNano(), blocked, item.id, item.generation)
	if err != nil {
		return fmt.Errorf("record search retry: %w", err)
	}
	return nil
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
