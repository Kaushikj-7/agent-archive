package agentapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"io"
	"strings"
)

// SourceReadLimits bounds how much of a source bundle LoadSource reads: the compressed
// object (default 32 MiB) and the decompressed stream (default 128 MiB). A
// zero or negative value means the default.
type SourceReadLimits struct {
	MaxCompressedBytes   int
	MaxUncompressedBytes int
}

// CompressedLimit returns the positive compressed-byte limit or the format default.
func (l SourceReadLimits) CompressedLimit() int {
	if l.MaxCompressedBytes > 0 {
		return l.MaxCompressedBytes
	}
	return 32 << 20
}

// UncompressedLimit returns the positive wire-byte limit or the format default.
func (l SourceReadLimits) UncompressedLimit() int {
	if l.MaxUncompressedBytes > 0 {
		return l.MaxUncompressedBytes
	}
	return 128 << 20
}

// SelectRevisionSourceMetadata validates the complete source set and selects
// one revision's immutable reference before its bounded bytes are read.
func SelectRevisionSourceMetadata(metadata archive.Metadata, revisionID string) (archive.Metadata, error) {
	selected, _, _, err := revisionSourceMetadata(metadata, revisionID)
	return selected, err
}

func sourceChecksumMatches(data []byte, expected string) bool {
	sum := sha256.Sum256(data)
	return strings.EqualFold(hex.EncodeToString(sum[:]), strings.TrimSpace(expected))
}

// DecodeReferencedSource verifies bounded immutable compressed bytes against a
// selected metadata pointer before decoding. Private publication stages reuse
// this reader without pretending to be a second object-store implementation.
func DecodeReferencedSource(ctx context.Context, metadata archive.Metadata, data []byte, limits SourceReadLimits) (archive.SourceBundle, error) {
	return decodeReferencedSource(ctx, metadata, data, limits, &metadata.FilterVersion, 0, false)
}

// DecodeRevisionSource verifies a retained revision using its own provenance.
// Absent legacy fields are derived from bounded decoded bytes, never the active filter.
func DecodeRevisionSource(ctx context.Context, metadata archive.Metadata, revisionID string, data []byte, limits SourceReadLimits) (archive.SourceBundle, error) {
	selected, filter, schema, err := revisionSourceMetadata(metadata, revisionID)
	if err != nil {
		return archive.SourceBundle{}, err
	}
	return decodeReferencedSource(ctx, selected, data, limits, filter, schema, revisionID != metadata.History.CurrentRevision)
}

func revisionSourceMetadata(metadata archive.Metadata, revisionID string) (archive.Metadata, *string, int, error) {
	if _, err := metadata.SourceReferences(); err != nil {
		return archive.Metadata{}, nil, 0, err
	}
	if metadata.History == nil {
		return archive.Metadata{}, nil, 0, errors.New("session has no native revision history")
	}
	if metadata.History.CurrentRevision == revisionID {
		return metadata, &metadata.FilterVersion, 0, nil
	}
	for _, revision := range metadata.History.Preserved {
		if revision.RevisionID != revisionID {
			continue
		}
		selected := metadata
		selected.SourceBundle = revision.Source
		selected.CapturedAt = revision.CapturedAt
		selected.History = &archive.RevisionHistory{CurrentRevision: revision.RevisionID}
		if revision.FilterVersion == "" {
			return selected, nil, revision.SourceSchemaVersion, nil
		}
		return selected, &revision.FilterVersion, revision.SourceSchemaVersion, nil
	}
	return archive.Metadata{}, nil, 0, errors.New("revision is not referenced by this session")
}

func decodeReferencedSource(ctx context.Context, metadata archive.Metadata, data []byte, limits SourceReadLimits, filter *string, schema int, preserved bool) (archive.SourceBundle, error) {
	if err := ctx.Err(); err != nil {
		return archive.SourceBundle{}, err
	}
	if err := metadata.ValidateSourceReference(); err != nil {
		return archive.SourceBundle{}, err
	}
	if len(data) > limits.CompressedLimit() {
		return archive.SourceBundle{}, errors.New("source exceeds compressed read limit")
	}
	if len(data) != metadata.SourceBundle.CompressedBytes {
		return archive.SourceBundle{}, errors.New("source compressed size does not match metadata")
	}
	if !sourceChecksumMatches(data, metadata.SourceBundle.SHA256) {
		return archive.SourceBundle{}, errors.New("source checksum mismatch")
	}
	// The verified compressed bytes are decoded as a stream, one JSONL line at
	// a time: the decompressed document is never held whole, only the
	// assembled records the caller asked for.
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(data), archive.DecodeOptions{MaxUncompressedBytes: int64(limits.UncompressedLimit())})
	if err != nil {
		var unsupported *archive.UnsupportedSourceSchemaError
		if errors.As(err, &unsupported) {
			// An unsupported header may precede a corrupt gzip trailer. Only
			// fully verified encodings may become a stable format gap.
			if verifyErr := verifyUnsupportedEncoding(data, limits.UncompressedLimit()); verifyErr != nil {
				return archive.SourceBundle{}, fmt.Errorf("decode source: %w", verifyErr)
			}
			err = &unsupportedSourceError{cause: err}
		}
		return archive.SourceBundle{}, fmt.Errorf("decode source: %w", err)
	}
	if bundle.History != nil && (metadata.History == nil || metadata.History.CurrentRevision != bundle.History.ActiveRolloutID) {
		return archive.SourceBundle{}, errors.New("source revision disagrees with metadata")
	}
	if (bundle.SchemaVersion != archive.SourceSchemaVersion && bundle.SchemaVersion != archive.HistorySourceSchemaVersion) || bundle.ArchiveSessionID != metadata.SessionID || bundle.NativeSessionID != metadata.NativeSessionID || bundle.ProjectID != metadata.ProjectID || bundle.ParentSessionID != metadata.ParentSessionID || !sourceHarnessMatches(bundle.Capture.Harness, metadata.Harness, preserved) || !bundle.Capture.CapturedAt.Equal(metadata.CapturedAt) || (filter != nil && bundle.Capture.FilterVersion != *filter) || (schema != 0 && bundle.SchemaVersion != schema) {
		return archive.SourceBundle{}, errors.New("source identity does not match metadata")
	}
	key, err := archive.SourceObjectKey(bundle, metadata.SourceBundle.SHA256)
	if err != nil || key != metadata.SourceBundle.Key {
		return archive.SourceBundle{}, errors.New("source key does not match metadata identity")
	}
	return bundle, nil
}

// A preserved physical revision has its own producer version and mode, bound
// by the immutable source checksum. The active sidecar cannot supply those
// observations, while the harness name remains part of stable source identity.
func sourceHarnessMatches(source, active archive.Harness, preserved bool) bool {
	if preserved {
		return source.Name == active.Name
	}
	return source == active
}

// IsUnsupportedSource reports a rejected format only after its referenced
// compressed bytes passed size, checksum and complete gzip verification.
func IsUnsupportedSource(err error) bool {
	var unsupported *unsupportedSourceError
	return errors.As(err, &unsupported)
}

type unsupportedSourceError struct{ cause error }

func (e *unsupportedSourceError) Error() string { return e.cause.Error() }
func (e *unsupportedSourceError) Unwrap() error { return e.cause }

func verifyUnsupportedEncoding(data []byte, limit int) error {
	compressed, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer func() { _ = compressed.Close() }()
	n, err := io.CopyN(io.Discard, compressed, int64(limit)+1)
	if n > int64(limit) {
		return archive.ErrSourceTooLarge
	}
	if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
