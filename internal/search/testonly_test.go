package search

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// LoadSession eagerly loads verified source fixtures for focused engine tests.
// Production callers use the bounded Observe/Run worker.
func LoadSession(ctx context.Context, objects storage.ObjectStore, parsers agentapi.ParsersLookup, destination string, metadata archive.Metadata, limits reader.Limits) ([]Projection, error) {
	ids := []string{""}
	if metadata.History != nil {
		revisions, err := reader.Revisions(metadata)
		if err != nil {
			return nil, err
		}
		ids = nil
		for _, r := range revisions {
			ids = append(ids, r.RevisionID)
		}
	}
	var out []Projection
	totalCompressed := 0
	refs, err := metadata.SourceReferences()
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		if ref.CompressedBytes > maxSessionBytes-totalCompressed {
			return nil, storage.ErrObjectTooLarge
		}
		totalCompressed += ref.CompressedBytes
	}
	totalText := 0
	totalPassages := 0
	for _, id := range ids {
		p, err := LoadProjection(ctx, objects, parsers, destination, metadata, id, limits)
		if err != nil {
			return nil, err
		}
		if len(p.Passages) > maxSessionPassages-totalPassages {
			return nil, storage.ErrObjectTooLarge
		}
		totalPassages += len(p.Passages)
		for _, passage := range p.Passages {
			if len(passage.Text) > maxSessionBytes-totalText {
				return nil, storage.ErrObjectTooLarge
			}
			totalText += len(passage.Text)
		}
		out = append(out, p)
	}
	return out, nil
}

// Replace installs fixture projections for focused engine query tests.
// Production callers use expected-generation worker commits.
func (s *Store) Replace(ctx context.Context, metadata archive.Metadata, projections []Projection) (WriteStats, error) {
	prepared, err := s.prepareSession(metadata, projections)
	if err != nil {
		return WriteStats{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WriteStats{}, err
	}
	defer func() { _ = tx.Rollback() }()
	stats, err := s.replaceTx(ctx, tx, metadata, &prepared)
	if err != nil {
		return stats, err
	}
	return stats, tx.Commit()
}

// ReadPassage rechecks authoritative metadata and verified bytes, then resolves
// the exact extraction version. It never silently substitutes newer content.
func ReadPassage(ctx context.Context, objects storage.ObjectStore, parsers agentapi.ParsersLookup, ref Reference, offset, maxBytes int, limits reader.Limits) (Context, error) {
	out, err := ReadContext(ctx, objects, parsers, ref, offset, maxBytes, 0, 0, limits)
	return out.Selected, err
}
