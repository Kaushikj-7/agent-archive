package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type resolvedRow struct {
	snapshot *Snapshot
	key      string
	hash     string
	mu       sync.Mutex
	used     bool
}

func rowDigest(row Row) (string, error) {
	raw, err := json.Marshal(struct {
		Key   string       `json:"Key"`
		Entry CatalogEntry `json:"Entry"`
	}{row.Key, row.Entry})
	if err != nil {
		return "", err
	}
	return storage.SHA256Hex(raw), nil
}

// ResolveRow expands only an explicitly tagged overflow entry through its exact
// immutable body. Raw bytes are returned for immediate verified disk caching,
// not retained by the snapshot. Inline summaries remain summary projections.
func (s *Snapshot) ResolveRow(ctx context.Context, row Row) (Row, []byte, error) {
	if err := s.check(ctx); err != nil {
		return Row{}, nil, err
	}
	if row.Entry.SummaryOverflow == "" {
		return row, nil, nil
	}
	current, err := s.findPinned(ctx, row.Key)
	if err != nil {
		return Row{}, nil, err
	}
	if current == nil {
		return Row{}, nil, errors.New("overflow row absent from snapshot")
	}
	expected, err := rowDigest(Row{Key: row.Key, Entry: *current})
	if err != nil {
		return Row{}, nil, err
	}
	actual, err := rowDigest(row)
	if err != nil || actual != expected {
		return Row{}, nil, errors.New("overflow row differs from snapshot")
	}
	raw, err := s.ReadMetadata(ctx, row.Entry)
	if err != nil {
		return Row{}, nil, err
	}
	body, err := decodeEntryBody(row.Entry, raw)
	if err != nil {
		return Row{}, nil, err
	}
	key, err := archive.MetadataObjectKey(body.Harness.Name, body.SessionID)
	if err != nil || key != row.Key {
		return Row{}, nil, errors.New("resolved catalog row identity mismatch")
	}
	row.Entry.Summary = body
	row.Entry.SummaryOverflow = ""
	hash, err := rowDigest(row)
	if err != nil {
		return Row{}, nil, err
	}
	row.resolved = &resolvedRow{snapshot: s, key: row.Key, hash: hash}
	return row, raw, s.check(ctx)
}

// ResolvedMetadata consumes verified full-body provenance once. It refuses
// caller-mutated rows and returns independently owned decoded metadata. A normal
// summary never gains body authority through this method.
func (s *Snapshot) ResolvedMetadata(ctx context.Context, row Row) (archive.Metadata, bool, error) {
	if err := s.check(ctx); err != nil {
		return archive.Metadata{}, false, err
	}
	proof := row.resolved
	if proof == nil {
		return archive.Metadata{}, false, nil
	}
	if proof.snapshot != s || proof.key != row.Key {
		return archive.Metadata{}, false, errors.New("foreign resolved catalog row")
	}
	hash, err := rowDigest(row)
	if err != nil || hash != proof.hash {
		return archive.Metadata{}, false, errors.New("resolved catalog row changed")
	}
	proof.mu.Lock()
	defer proof.mu.Unlock()
	if proof.used {
		return archive.Metadata{}, false, nil
	}
	raw, err := json.Marshal(row.Entry.Summary)
	if err != nil {
		return archive.Metadata{}, false, err
	}
	var body archive.Metadata
	if err = json.Unmarshal(raw, &body); err != nil {
		return archive.Metadata{}, false, err
	}
	if err = s.check(ctx); err != nil {
		return archive.Metadata{}, false, err
	}
	proof.used = true
	return body, true, nil
}
