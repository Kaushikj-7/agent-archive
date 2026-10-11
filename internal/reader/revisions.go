package reader

import (
	"context"
	"fmt"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Revision describes one validated native revision. Empty legacy provenance is
// unknown until verified source bytes are read; it is never borrowed from active.
type Revision struct {
	RevisionID          string                  `json:"revision_id"`
	Active              bool                    `json:"active"`
	CapturedAt          time.Time               `json:"captured_at"`
	FilterVersion       string                  `json:"filter_version,omitempty"`
	SourceSchemaVersion int                     `json:"source_schema_version,omitempty"`
	Source              archive.SourceReference `json:"source"`
}

// Revisions lists the active revision first, then preserved manifest order.
// IDs are exact native rollout IDs, not prefixes, aliases or object keys.
func Revisions(metadata archive.Metadata) ([]Revision, error) {
	if _, err := metadata.SourceReferences(); err != nil {
		return nil, err
	}
	out := make([]Revision, 0)
	if metadata.History == nil {
		return out, nil
	}
	out = append(out, Revision{RevisionID: metadata.History.CurrentRevision, Active: true, CapturedAt: metadata.CapturedAt, FilterVersion: metadata.FilterVersion, Source: metadata.SourceBundle})
	for _, r := range metadata.History.Preserved {
		out = append(out, Revision{RevisionID: r.RevisionID, CapturedAt: r.CapturedAt, FilterVersion: r.FilterVersion, SourceSchemaVersion: r.SourceSchemaVersion, Source: r.Source})
	}
	return out, nil
}

// ReadRevision reads metadata and a referenced revision with bounded integrity
// verification. The returned metadata supplies selection provenance only;
// callers must rederive summaries from the selected bundle rather than reuse
// active counts, titles, lifecycle or links.
func ReadRevision(ctx context.Context, store storage.ObjectStore, key, revisionID string, limits Limits) (archive.Metadata, archive.SourceBundle, error) {
	metadata, err := ReadMetadata(ctx, store, key)
	if err != nil {
		return archive.Metadata{}, archive.SourceBundle{}, err
	}
	if actual, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID); err != nil || actual != key {
		return archive.Metadata{}, archive.SourceBundle{}, fmt.Errorf("%w: revision parent does not match requested identity", ErrInvalidMetadata)
	}
	bundle, err := LoadRevision(ctx, store, metadata, revisionID, limits)
	if err != nil {
		return archive.Metadata{}, archive.SourceBundle{}, err
	}
	selected, _, _, err := revisionSourceMetadata(metadata, revisionID)
	if err != nil {
		return archive.Metadata{}, archive.SourceBundle{}, err
	}
	selected.FilterVersion = bundle.Capture.FilterVersion
	return selected, bundle, nil
}
