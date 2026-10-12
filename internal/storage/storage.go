// Package storage provides the small object-store contract used by the archive.
//
// Object keys passed to this package are relative to the configured bucket
// prefix. Implementations do not interpret JSON or archive metadata; keeping
// that boundary here makes it possible to use the collector with a fake or
// in-memory store in tests.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsretry "github.com/aws/aws-sdk-go-v2/aws/retry"
)

var (
	// ErrNotFound is returned by Get when an object does not exist.
	ErrNotFound = errors.New("object not found")
	// ErrChecksumMismatch means the bytes read from storage do not match the
	// checksum supplied by the caller.
	ErrChecksumMismatch = errors.New("object checksum mismatch")
	// ErrObjectTooLarge is returned when a remote object exceeds the bounded
	// read limit configured on S3Store.
	ErrObjectTooLarge = errors.New("object exceeds read limit")
)

// ObjectStore is the storage boundary used by the archive collector and
// reader. Put replaces an object at key atomically from the caller's point of
// view. Implementations should not mutate data after Put returns.
type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	List(ctx context.Context, prefix string) ([]Object, error)
	Delete(ctx context.Context, key string) error
}

// CatalogNamespace optionally identifies an immutable object-store destination
// across process lifetimes. The value must bind provider/endpoint/bucket/prefix
// (and any custom routing identity); revision validators are scoped to it. Empty
// declines persistent summary reuse. It must not contain credentials or tokens.
type CatalogNamespace interface {
	CatalogNamespace() string
}

// LimitedGetter reads at most limit bytes, rejecting oversized objects before
// allocating their bodies. Callers of untrusted small records require it.
type LimitedGetter interface {
	GetLimited(ctx context.Context, key string, limit int64) ([]byte, error)
}

// PageLister lists one lexicographically ordered page. Continuation is an
// opaque token supplied by the preceding page; an empty Next means done.
// Keeping this as an extension lets older ObjectStore implementations retain
// their existing all-results contract.
type PageLister interface {
	ListPage(ctx context.Context, prefix, continuation string, limit int32) (ObjectPage, error)
}

// RangeLister lists the objects under prefix whose keys are greater than
// after and at most through, in key order. An empty after starts at the
// beginning of prefix, and an empty through runs to its end, so ranges split
// at the same boundaries (after of one equal to through of the one before)
// cover every key exactly once. A cancelled context must end in an error,
// never in a shorter listing, since a reader treats a range's result as
// complete. Like PageLister it is an optional extension: a reader lists
// disjoint ranges concurrently instead of paging through one listing.
type RangeLister interface {
	ListRange(ctx context.Context, prefix, after, through string) ([]Object, error)
}

// ObjectPage contains one page of object keys and an optional continuation token.
type ObjectPage struct {
	Objects []Object
	Next    string
}

// ObjectKeyer is implemented by stores that compose a full object key from a
// relative archive key, such as S3Store applying its configured bucket
// prefix. VerifyAccess uses it to report the exact object key in errors so a
// user can find and remove a leftover setup-test object by hand.
type ObjectKeyer interface {
	ObjectKey(relative string) string
}

// Object is a listed object and intentionally contains no body. Downloading
// content is an explicit Get operation.
type Object struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

// Prefix joins a configured bucket prefix with a relative archive key.
// Prefix returns a slash-normalized key and rejects absolute keys so callers
// cannot accidentally escape their configured namespace.
func Prefix(prefix, key string) (string, error) {
	if strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "\\") || hasDotComponent(prefix) {
		return "", fmt.Errorf("invalid object prefix %q", prefix)
	}
	if strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || key == "" || hasDotComponent(key) {
		return "", fmt.Errorf("invalid object key %q", key)
	}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return key, nil
	}
	return prefix + "/" + key, nil
}

// ReadAndVerify downloads key and verifies its SHA-256 hex digest. The
// checksum covers the exact bytes returned by Get, rather than an ETag (which
// is not a SHA-256 checksum for multipart or encrypted objects).
func ReadAndVerify(ctx context.Context, store ObjectStore, key, sha256Hex string) ([]byte, error) {
	b, err := store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if !VerifySHA256(b, sha256Hex) {
		return nil, fmt.Errorf("%w for %q", ErrChecksumMismatch, key)
	}
	return b, nil
}

// VerifySHA256 reports whether data's SHA-256 digest equals expectedHex.
func VerifySHA256(data []byte, expectedHex string) bool {
	got := SHA256Hex(data)
	return strings.EqualFold(got, strings.TrimSpace(expectedHex))
}

// SHA256Hex returns the lower-case SHA-256 digest of data.
func SHA256Hex(data []byte) string {
	return sha256Hex(data)
}

// probePrefix is the folder VerifyAccess writes its test object in. Probe
// lists it, so the call stays inside the one prefix a least-privilege policy
// grants s3:ListBucket for, and only leftover test objects can be in it.
const probePrefix = ".setup-test/"

// Probe makes one cheap call to the store, a listing of at most one object
// in the setup test folder, and writes nothing. Setup runs it before
// VerifyAccess so a wrong account, key, or endpoint fails at once, with the
// error Diagnose explains, instead of after the round trip's upload and
// clean-up have each failed. Passing it proves only that the store answers
// this caller; VerifyAccess is still the check that writing, reading, and
// deleting work.
//
// A store that pages (PageLister) is asked for one object. Any other store
// lists the setup test folder, which holds nothing but leftovers of failed
// checks, so neither path reads through a large archive.
func Probe(ctx context.Context, store ObjectStore) error {
	var err error
	if pager, ok := store.(PageLister); ok {
		_, err = pager.ListPage(ctx, probePrefix, "", 1)
	} else {
		_, err = store.List(ctx, probePrefix)
	}
	if err != nil {
		return fmt.Errorf("setup test probe %s: %w", setupKeyLabel(store, probePrefix), err)
	}
	return nil
}

// VerifyAccess performs the setup round trip required by the product spec.
// It creates a unique relative object key and the store applies its
// configured prefix. It reads and verifies the object, confirms it appears in
// List, and removes it. No bucket-admin operation is required. Errors name
// the full object key when the store implements ObjectKeyer.
func VerifyAccess(ctx context.Context, store ObjectStore) error {
	key := uniqueSetupKey()
	label := setupKeyLabel(store, key)
	payload := []byte(`{"agent_archive_setup_test":true}`)
	cleanup := func() error { return store.Delete(ctx, key) }
	if err := store.Put(ctx, key, payload); err != nil {
		return withCleanupError(fmt.Errorf("setup test upload %s: %w", label, err), cleanup, label)
	}
	got, err := store.Get(ctx, key)
	if err != nil {
		return withCleanupError(fmt.Errorf("setup test read %s: %w", label, err), cleanup, label)
	}
	if string(got) != string(payload) {
		return withCleanupError(fmt.Errorf("setup test read %s: %w", label, ErrChecksumMismatch), cleanup, label)
	}
	objects, err := store.List(ctx, key)
	if err != nil {
		return withCleanupError(fmt.Errorf("setup test list %s: %w", label, err), cleanup, label)
	}
	found := false
	for _, obj := range objects {
		if obj.Key == key {
			found = true
			break
		}
	}
	if !found {
		return withCleanupError(fmt.Errorf("setup test list %s: object missing", label), cleanup, label)
	}
	if err := cleanup(); err != nil {
		return fmt.Errorf("setup test cleanup %s: %w", label, err)
	}
	// Publishing a session reads its source key before the first upload and
	// needs "missing" back, not "denied". Check that now, while the reason
	// is still easy to explain, rather than at the first session.
	if _, err := store.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		if err == nil {
			return fmt.Errorf("setup test read after delete %s: the object is still there", label)
		}
		return fmt.Errorf("setup test read after delete %s: a missing object must read as not found (grant s3:ListBucket on the prefix; see docs/security/bucket-permissions.md): %w", label, err)
	}
	return nil
}

// setupKeyLabel returns the quoted object key for error messages: the full
// key as the store composes it when available, otherwise the relative key
// with a note that the configured prefix is not shown.
func setupKeyLabel(store ObjectStore, key string) string {
	if keyer, ok := store.(ObjectKeyer); ok {
		return fmt.Sprintf("%q", keyer.ObjectKey(key))
	}
	return fmt.Sprintf("%q (relative to the configured prefix)", key)
}

func withCleanupError(primary error, cleanup func() error, label string) error {
	if cleanupErr := cleanup(); cleanupErr != nil {
		return fmt.Errorf("%w; setup test cleanup %s also failed: %w", primary, label, cleanupErr)
	}
	return primary
}

func hasDotComponent(value string) bool {
	for component := range strings.SplitSeq(value, "/") {
		//lint:ignore LV1001 path components are arbitrary text; only the two dot names are special
		if component == "." || component == ".." {
			return true
		}
	}
	return false
}

// PutSourceThenMetadataIndexed implements source-first publication. The
// source is uploaded and verified in storage before metadata is published. Retry
// attempts reuse the exact input bytes, so a retry cannot produce another
// source hash or timestamp.
//
// Verification goes through verifyStoredObject, before the upload (a source
// already stored with these exact bytes is not uploaded again) and after it.
// A store that reports an object's SHA-256 (ObjectStatter) is checked that
// way, without a download: S3Store.Put sends the source's SHA-256 with the
// upload, the service refuses a body that does not match it, and a HEAD then
// reports the checksum and size of the object now stored at the key. A
// matching checksum and size therefore prove that key holds exactly these
// bytes, as a download would, and a HEAD needs the same read permission as a
// GET. A store that reports no checksum is read back and hashed instead.
// beforeMetadata runs after source verification and before the authoritative
// sidecar. A failed index write leaves no new sidecar; a failed sidecar write
// leaves only a harmless stale index entry.
func PutSourceThenMetadataIndexed(ctx context.Context, store ObjectStore, sourceKey, metadataKey string, source, metadata []byte, retry RetryPolicy, beforeMetadata func() error) error {
	if sourceKey == "" || metadataKey == "" {
		return errors.New("source and metadata keys are required")
	}
	if sourceKey == metadataKey {
		return errors.New("source and metadata keys must differ")
	}
	sum := SHA256Hex(source)
	if err := retry.run(ctx, func() error {
		switch err := verifyStoredObject(ctx, store, sourceKey, sum, len(source)); {
		case err == nil:
			return nil
		case errors.Is(err, ErrChecksumMismatch):
			return fmt.Errorf("existing source: %w", err)
		case !errors.Is(err, ErrNotFound):
			return err
		}
		if err := store.Put(ctx, sourceKey, source); err != nil {
			return err
		}
		return verifyStoredObject(ctx, store, sourceKey, sum, len(source))
	}); err != nil {
		return fmt.Errorf("publish source %q: %w", sourceKey, err)
	}
	if beforeMetadata != nil {
		if err := beforeMetadata(); err != nil {
			return fmt.Errorf("publish listing index: %w", err)
		}
	}
	if err := retry.run(ctx, func() error { return store.Put(ctx, metadataKey, metadata) }); err != nil {
		return fmt.Errorf("publish metadata %q: %w", metadataKey, err)
	}
	return nil
}

// PutMetadataForSourceIndexed publishes metadata that points at a source object
// already in storage, for a caller that no longer has the source's bytes. The
// source is read back and checked against sourceSHA256 first, so metadata
// never points at a missing or different object: a missing source is
// ErrNotFound and a different one ErrChecksumMismatch, and neither publishes.
//
// A store that can describe an object (ObjectStatter) and reports its SHA-256
// is checked that way, without a download; otherwise the source is read back.
// sourceSize, when positive, must match too.
// beforeMetadata can write an immutable index entry immediately before the
// sidecar replacement.
func PutMetadataForSourceIndexed(ctx context.Context, store ObjectStore, sourceKey, sourceSHA256 string, sourceSize int, metadataKey string, metadata []byte, retry RetryPolicy, beforeMetadata func() error) error {
	if sourceKey == "" || metadataKey == "" || sourceSHA256 == "" {
		return errors.New("source key, source checksum, and metadata key are required")
	}
	if sourceKey == metadataKey {
		return errors.New("source and metadata keys must differ")
	}
	if err := retry.run(ctx, func() error {
		return verifyStoredObject(ctx, store, sourceKey, sourceSHA256, sourceSize)
	}); err != nil {
		return fmt.Errorf("verify source %q: %w", sourceKey, err)
	}
	if beforeMetadata != nil {
		if err := beforeMetadata(); err != nil {
			return fmt.Errorf("publish listing index: %w", err)
		}
	}
	if err := retry.run(ctx, func() error { return store.Put(ctx, metadataKey, metadata) }); err != nil {
		return fmt.Errorf("publish metadata %q: %w", metadataKey, err)
	}
	return nil
}

// ObjectInfo describes a stored object without its content.
type ObjectInfo struct {
	// ETag is the provider's opaque revision validator, without quotes.
	ETag string
	Size int64
	// SHA256 is the object's whole-content SHA-256 in lower-case hex as the
	// store records it, or "" when it records none for this object (one
	// uploaded without a checksum, or a multipart upload's composite one).
	SHA256 string
}

// VersionedGetter returns the provider validator from the same response as
// the bytes. An empty validator cannot establish indexed freshness.
type VersionedGetter interface {
	GetVersioned(context.Context, string) ([]byte, string, error)
}

// ObjectStatter is implemented by stores that can describe an object without
// downloading it. Stat returns ErrNotFound for a missing object.
type ObjectStatter interface {
	Stat(ctx context.Context, key string) (ObjectInfo, error)
}

// verifyStoredObject checks that key holds an object with the given SHA-256
// hex digest (and size, when positive): through Stat when the store reports
// a digest, otherwise by reading the object back.
func verifyStoredObject(ctx context.Context, store ObjectStore, key, sha256Hex string, size int) error {
	if statter, ok := store.(ObjectStatter); ok {
		info, err := statter.Stat(ctx, key)
		if err != nil {
			return err
		}
		if info.SHA256 != "" {
			if !strings.EqualFold(info.SHA256, strings.TrimSpace(sha256Hex)) || (size > 0 && info.Size != int64(size)) {
				return fmt.Errorf("%w for %q", ErrChecksumMismatch, key)
			}
			return nil
		}
	}
	data, err := ReadAndVerify(ctx, store, key, sha256Hex)
	if err != nil {
		return err
	}
	if size > 0 && len(data) != size {
		return fmt.Errorf("%w for %q", ErrChecksumMismatch, key)
	}
	return nil
}

// RetryPolicy controls bounded retries for transient storage operations.
// MaxAttempts includes the first attempt. Zero uses the default of three.
// Only an error isTransient accepts is retried: a denied request, a missing
// bucket, or a checksum mismatch fails the same way every time.
type RetryPolicy struct {
	MaxAttempts int
	InitialWait time.Duration
	MaxWait     time.Duration
}

// sdkRetryables is the AWS SDK's own classification of retryable errors:
// connection failures, throttling, and 5xx responses.
var sdkRetryables = awsretry.IsErrorRetryables(awsretry.DefaultRetryables)

// isTransient reports whether a storage error may succeed if the operation is
// simply tried again. It follows the AWS SDK's classification, with three
// refinements: an error that already exhausted the SDK's own retries is not
// retried again on top of them; a cancelled or expired context never is; and
// a response body cut short (io.ErrUnexpectedEOF), which the SDK does not see
// because the body is read after it returns, is.
func isTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrChecksumMismatch) || errors.Is(err, ErrObjectTooLarge) {
		return false
	}
	var exhausted *awsretry.MaxAttemptsError
	if errors.As(err, &exhausted) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return sdkRetryables.IsErrorRetryable(err) == aws.TrueTernary
}

func (p RetryPolicy) run(ctx context.Context, operation func() error) error {
	attempts := p.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	wait := p.InitialWait
	if wait <= 0 {
		wait = 25 * time.Millisecond
	}
	maxWait := p.MaxWait
	if maxWait <= 0 {
		maxWait = time.Second
	}
	var err error
	for attempt := range attempts {
		if err = operation(); err == nil {
			return nil
		}
		if attempt+1 == attempts || !isTransient(err) || ctx.Err() != nil {
			break
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
		wait *= 2
		if wait > maxWait {
			wait = maxWait
		}
	}
	return err
}
