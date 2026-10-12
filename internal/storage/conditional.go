package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"github.com/wangjohn/agent-archive/internal/local"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// ErrPreconditionFailed normalizes a failed conditional write.
var ErrPreconditionFailed = errors.New("object precondition failed")

// ErrAtomicCatalogUnqualified refuses a provider without live atomic evidence.
var ErrAtomicCatalogUnqualified = errors.New("catalog-v4 requires qualified atomic provider evidence")

// PutCondition requires exactly one compare-and-swap condition.
type PutCondition struct {
	MatchETag  string
	CreateOnly bool
}

// ConditionalPutter performs one guarded object replacement or creation.
type ConditionalPutter interface {
	PutConditional(context.Context, string, []byte, PutCondition) (string, error)
}

// AtomicCatalogProvider is deliberately separate from SDK header support.
// Implementations must attest a checked-in live qualification for this endpoint.
type AtomicCatalogProvider interface{ CatalogAtomicQualification() error }

// CatalogAtomicQualification refuses activation until live evidence is reviewed.
func (s *S3Store) CatalogAtomicQualification() error {
	// No live S3 or R2 qualification has been recorded. Never infer atomicity
	// from a provider label, user configuration, or a fake HTTP server.
	return ErrAtomicCatalogUnqualified
}

// PutConditional sends SDK conditions without automatic write retries.
func (s *S3Store) PutConditional(ctx context.Context, relative string, data []byte, condition PutCondition) (string, error) {
	if condition.CreateOnly == (condition.MatchETag != "") {
		return "", errors.New("exactly one put condition is required")
	}
	key, err := s.key(relative)
	if err != nil {
		return "", err
	}
	sum := sha256Bytes(data)
	var match, create *string
	if condition.CreateOnly {
		create = aws.String("*")
	} else {
		match = aws.String(`"` + strings.Trim(condition.MatchETag, `"`) + `"`)
	}
	input := &s3.PutObjectInput{IfMatch: match, IfNoneMatch: create, Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(data), ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum[:]))}
	// The catalog owns lost-response resolution. Do not let SDK retries turn an
	// acknowledged write into a misleading precondition failure.
	output, err := s.client.PutObject(ctx, input, func(o *s3.Options) { o.RetryMaxAttempts = 1 })
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && (api.ErrorCode() == "PreconditionFailed" || api.ErrorCode() == "ConditionalRequestConflict") {
			return "", ErrPreconditionFailed
		}
		return "", err
	}
	return strings.Trim(aws.ToString(output.ETag), `"`), nil
}

// LimitedVersionedGetter bounds the body while retaining its exact validator.
type LimitedVersionedGetter interface {
	GetLimitedVersioned(context.Context, string, int64) ([]byte, string, error)
}

// GetLimitedVersioned returns a bounded body and its exact provider ETag.
func (s *S3Store) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	return s.getVersioned(ctx, key, limit)
}

// CatalogPublisher is the narrow frozen-publication adapter capability. The
// explicit authority flag lets wrappers refuse unsupported underlying stores.
type CatalogPublisher interface {
	CatalogMetadataAuthority() bool
	FreezeCatalogMutation(context.Context, string) (id, expectedRevision string, err error)
	Publication(id, key, expectedRevision string) ObjectStore
}

// CatalogObjectVersion binds timestamp precision and ETag to the same GET body.
// Unknown or zero precision cannot license catalog reclamation.
type CatalogObjectVersion struct {
	ETag         string
	LastModified time.Time
	Precision    time.Duration
}

// CatalogVersionedGetter returns a bounded body with its exact provider version.
type CatalogVersionedGetter interface {
	GetCatalogVersion(context.Context, string, int64) ([]byte, CatalogObjectVersion, error)
}

// CatalogTime bounds the provider clock observation, including uncertainty.
type CatalogTime struct {
	Earliest time.Time
	Latest   time.Time
}

// CatalogClock supplies reviewed provider clock evidence, never local wall time.
type CatalogClock interface {
	CatalogServerClock(context.Context) (CatalogTime, error)
}

// GetCatalogVersion exposes the timestamp from the same bounded SDK GET.
// Precision remains unknown until the exact provider contract is qualified.
func (s *S3Store) GetCatalogVersion(ctx context.Context, relative string, limit int64) ([]byte, CatalogObjectVersion, error) {
	if limit < 1 {
		return nil, CatalogObjectVersion{}, errors.New("object read limit must be positive")
	}
	limit = min(limit, s.maxGetBytes)
	key, err := s.key(relative)
	if err != nil {
		return nil, CatalogObjectVersion{}, err
	}
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) || s.confirmedAbsent(ctx, key, err) {
			return nil, CatalogObjectVersion{}, ErrNotFound
		}
		return nil, CatalogObjectVersion{}, err
	}
	defer func() { _ = output.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(output.Body, limit+1))
	if err != nil {
		return nil, CatalogObjectVersion{}, err
	}
	if int64(len(data)) > limit {
		return nil, CatalogObjectVersion{}, ErrObjectTooLarge
	}
	return data, CatalogObjectVersion{ETag: strings.Trim(aws.ToString(output.ETag), `"`), LastModified: aws.ToTime(output.LastModified)}, nil
}

// CatalogServerClock refuses time-based cleanup without reviewed clock evidence.
func (s *S3Store) CatalogServerClock(context.Context) (CatalogTime, error) {
	return CatalogTime{}, ErrAtomicCatalogUnqualified
}

// CatalogLifecycle holds the global admission through source, pending and
// preserved-history acknowledgement, including recovery after process restart.
type CatalogLifecycle interface {
	BeginPublication(context.Context, string, []byte) (context.Context, error)
	CompletePublication(context.Context, string, []byte) error
	EndPublicationAttempt(string)
}

// CatalogJournalLifecycle resumes only an exact durable collector journal
// under its actual originating flock. Direct unfinished owners stay fenced.
type CatalogJournalLifecycle interface {
	BeginJournalPublication(context.Context, string, []byte, local.CatalogJournal, *local.CollectorGuard) (context.Context, bool, error)
	AcknowledgeJournalRemoval(context.Context, *local.JournalRemoval, *local.CollectorGuard) error
}
