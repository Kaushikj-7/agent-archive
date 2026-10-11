package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsretry "github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/wangjohn/agent-archive/internal/credentials"
)

// S3Store is an ObjectStore backed by Amazon S3 or a compatible endpoint such
// as Cloudflare R2. The SDK client is injected so tests can use a fake HTTP
// server without credentials or a bucket administrator account.
type S3Store struct {
	provider    string
	client      *s3.Client
	bucket      string
	prefix      string
	maxGetBytes int64
}

// S3StoreOptions configures a store. Client must be constructed with the
// desired credential provider; this package never reads credentials itself.
type S3StoreOptions struct {
	// Provider is s3 or r2 (compared case- and space-insensitively); empty is
	// unknown for custom endpoints.
	Provider string
	// Client is used as-is; endpoint, addressing style, retries, and
	// timeouts are its configuration (see NewClient).
	Client *s3.Client
	Bucket string
	Prefix string
	// MaxGetBytes bounds memory used while reading an object. Zero selects
	// the 64 MiB default appropriate for archive metadata and source bundles.
	MaxGetBytes int64
}

// NewS3Store creates a store from an AWS SDK client.
func NewS3Store(options S3StoreOptions) (*S3Store, error) {
	if options.Client == nil {
		return nil, errors.New("storage: S3 client is required")
	}
	if strings.TrimSpace(options.Bucket) == "" {
		return nil, errors.New("storage: bucket is required")
	}
	if options.Prefix != "" {
		if _, err := Prefix(options.Prefix, "archive"); err != nil {
			return nil, err
		}
	}
	maxGetBytes := options.MaxGetBytes
	if maxGetBytes <= 0 {
		maxGetBytes = 64 << 20
	}
	return &S3Store{provider: strings.ToLower(strings.TrimSpace(options.Provider)), client: options.Client, bucket: options.Bucket, prefix: strings.Trim(options.Prefix, "/"), maxGetBytes: maxGetBytes}, nil
}

// NewClient constructs an S3 client for AWS or an S3-compatible endpoint.
// For R2 callers should pass region "auto", an endpoint supplied by
// Cloudflare, and a static credentials provider from internal/credentials.
// Path style addressing is used for compatibility with both R2 and local
// fake servers.
//
// The SDK's default HTTP client waits forever for a server that accepts a
// connection and never answers (a captive portal, a stalled proxy), and a
// collector pass stuck there holds the collector lock, so every later pass
// quietly finds it busy. The client is therefore given connect, TLS, and
// response-header timeouts (see withTimeouts). A body that stalls after its
// headers arrived is bounded by a two-minute per-read/page deadline and
// any shorter caller context deadline. Uploads retain their caller budgets.
//
// The SDK's logger is silenced: config.LoadDefaultConfig points it at
// stderr, where it would print lines such as "SDK <date> DEBUG Response has
// no supported checksum" into the middle of a command's output. Failures
// reach the caller as errors, which Diagnose explains.
func NewClient(cfg aws.Config, endpoint string, pathStyle bool, maxAttempts int) *s3.Client {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	return s3.NewFromConfig(cfg, func(options *s3.Options) {
		if endpoint != "" {
			options.BaseEndpoint = aws.String(strings.TrimRight(endpoint, "/"))
		}
		options.HTTPClient = tracedClient{inner: withTimeouts(options.HTTPClient)}
		options.Logger = logging.Nop{}
		options.UsePathStyle = pathStyle
		options.Retryer = awsretry.NewStandard(func(retryOptions *awsretry.StandardOptions) {
			retryOptions.MaxAttempts = maxAttempts
		})
	})
}

// Network timeouts for the SDK's HTTP client. Variables only so a test can
// shorten them.
var (
	readOperationTimeout  = 2 * time.Minute
	dialTimeout           = 15 * time.Second
	tlsHandshakeTimeout   = 15 * time.Second
	responseHeaderTimeout = 60 * time.Second
)

// withTimeouts gives the SDK's own HTTP client (which is what
// config.LoadDefaultConfig, or no client at all, yields) connect, TLS, and
// response-header timeouts. The response-header timer starts only once the
// request body has been sent, so it does not limit upload size. A client the
// caller supplied itself is left alone.
func withTimeouts(client aws.HTTPClient) aws.HTTPClient {
	var buildable *awshttp.BuildableClient
	switch c := client.(type) {
	case nil:
		buildable = awshttp.NewBuildableClient()
	case *awshttp.BuildableClient:
		buildable = c
	default:
		return client
	}
	return buildable.
		WithDialerOptions(func(dialer *net.Dialer) { dialer.Timeout = dialTimeout }).
		WithTransportOptions(func(transport *http.Transport) {
			transport.TLSHandshakeTimeout = tlsHandshakeTimeout
			transport.ResponseHeaderTimeout = responseHeaderTimeout
		})
}

func (s *S3Store) key(relative string) (string, error) {
	return Prefix(s.prefix, relative)
}

// ObjectKey returns the full bucket key for relative, including the
// configured prefix. Keys that Prefix rejects are returned unchanged so error
// messages still identify the object.
func (s *S3Store) ObjectKey(relative string) string {
	key, err := s.key(relative)
	if err != nil {
		return relative
	}
	return key
}

// Put uploads data to relative under the store's prefix with a SHA-256
// checksum, which S3 verifies on receipt and Stat later reports.
func (s *S3Store) Put(ctx context.Context, relative string, data []byte) error {
	key, err := s.key(relative)
	if err != nil {
		return err
	}
	sum := sha256Bytes(data)
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:         aws.String(s.bucket),
		Key:            aws.String(key),
		Body:           bytes.NewReader(data),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	})
	return err
}

// Get downloads the object at relative under the store's prefix. A missing
// object is ErrNotFound, and one larger than the store's read limit is
// ErrObjectTooLarge. A 403 is ErrNotFound only when a listing confirms the
// key is absent (see confirmedAbsent); otherwise it is returned as is.
func (s *S3Store) Get(ctx context.Context, relative string) ([]byte, error) {
	data, _, err := s.GetVersioned(ctx, relative)
	return data, err
}

// GetLimited reads an object with a caller's allocation limit, also honoring
// the store's own maximum. It never reads more than limit plus one bytes.
func (s *S3Store) GetLimited(ctx context.Context, relative string, limit int64) ([]byte, error) {
	data, _, err := s.getVersioned(ctx, relative, limit)
	return data, err
}

// GetVersioned reads bytes and their opaque ETag from one response.
func (s *S3Store) GetVersioned(ctx context.Context, relative string) ([]byte, string, error) {
	return s.getVersioned(ctx, relative, s.maxGetBytes)
}

func (s *S3Store) getVersioned(ctx context.Context, relative string, limit int64) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, readOperationTimeout)
	defer cancel()
	if limit < 1 {
		return nil, "", errors.New("object read limit must be positive")
	}
	limit = min(limit, s.maxGetBytes)
	key, err := s.key(relative)
	if err != nil {
		return nil, "", err
	}
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) || s.confirmedAbsent(ctx, key, err) {
			return nil, "", ErrNotFound
		}
		return nil, "", err
	}
	defer func() { _ = output.Body.Close() }()
	limited := io.LimitReader(output.Body, limit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > limit {
		return nil, "", fmt.Errorf("%w: %q exceeds %d bytes", ErrObjectTooLarge, relative, limit)
	}
	return data, strings.Trim(aws.ToString(output.ETag), "\""), nil
}

// Stat describes an object with a HEAD request in checksum mode. Put stores
// every object with a SHA-256 checksum, and S3 reports it back as base64;
// a store that keeps none (or a multipart upload's composite checksum,
// "<digest>-<parts>") yields an empty SHA256, so callers read the object
// instead.
func (s *S3Store) Stat(ctx context.Context, relative string) (ObjectInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, readOperationTimeout)
	defer cancel()
	key, err := s.key(relative)
	if err != nil {
		return ObjectInfo{}, err
	}
	output, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		// A HEAD response has no body, so it carries no error code: a 404
		// for a missing bucket looks exactly like one for a missing object,
		// and both read as ErrNotFound here (Get, whose body names
		// NoSuchBucket, can tell them apart). For the collector the cost is
		// bounded: a metadata refresh over a bucket that is gone is recorded
		// as impossible (a refresh-skip) and not retried until the session
		// publishes again or the parser changes, and a missing bucket fails
		// every other storage call loudly anyway.
		if isNotFound(err) || s.confirmedAbsent(ctx, key, err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, err
	}
	var size int64
	if output.ContentLength != nil {
		size = *output.ContentLength
	}
	var sum string
	if output.ChecksumSHA256 != nil && output.ChecksumType != types.ChecksumTypeComposite {
		if digest, err := base64.StdEncoding.DecodeString(*output.ChecksumSHA256); err == nil && len(digest) == sha256.Size {
			sum = hex.EncodeToString(digest)
		}
	}
	return ObjectInfo{
		ETag:   strings.Trim(aws.ToString(output.ETag), "\""),
		Size:   size,
		SHA256: sum,
	}, nil
}

// List returns every object whose key starts with relativePrefix under the
// store's prefix, following all result pages, with keys relative to the
// store's prefix and no bodies. An empty relativePrefix lists the whole
// prefix.
func (s *S3Store) List(ctx context.Context, relativePrefix string) ([]Object, error) {
	prefix, err := s.keyForList(relativePrefix)
	if err != nil {
		return nil, err
	}
	pager := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix)})
	var objects []Object
	for pager.HasMorePages() {
		pageCtx, cancel := context.WithTimeout(ctx, readOperationTimeout)
		page, pageErr := pager.NextPage(pageCtx)
		cancel()
		if pageErr != nil {
			return nil, pageErr
		}
		for _, item := range page.Contents {
			if item.Key == nil {
				continue
			}
			objects = append(objects, Object{
				Key:          trimStorePrefix(*item.Key, s.prefix),
				Size:         aws.ToInt64(item.Size),
				ETag:         strings.Trim(aws.ToString(item.ETag), "\""),
				LastModified: aws.ToTime(item.LastModified),
			})
		}
	}
	return objects, nil
}

// ListPage returns one provider page without fetching later pages.
func (s *S3Store) ListPage(ctx context.Context, relativePrefix, continuation string, limit int32) (ObjectPage, error) {
	ctx, cancel := context.WithTimeout(ctx, readOperationTimeout)
	defer cancel()
	prefix, err := s.keyForList(relativePrefix)
	if err != nil {
		return ObjectPage{}, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var token *string
	if continuation != "" {
		token = aws.String(continuation)
	}
	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket), Prefix: aws.String(prefix),
		MaxKeys: aws.Int32(limit), ContinuationToken: token,
	}
	output, err := s.client.ListObjectsV2(ctx, input)
	if err != nil {
		return ObjectPage{}, err
	}
	page := ObjectPage{Objects: make([]Object, 0, len(output.Contents))}
	for _, item := range output.Contents {
		if item.Key == nil {
			continue
		}
		page.Objects = append(page.Objects, Object{Key: trimStorePrefix(*item.Key, s.prefix), Size: aws.ToInt64(item.Size), ETag: strings.Trim(aws.ToString(item.ETag), "\""), LastModified: aws.ToTime(item.LastModified)})
	}
	if output.IsTruncated != nil && *output.IsTruncated {
		page.Next = aws.ToString(output.NextContinuationToken)
		if page.Next == "" {
			return ObjectPage{}, fmt.Errorf("list page is truncated without continuation token")
		}
	}
	return page, nil
}

// ListRange lists the objects under relativePrefix in the key range (after,
// through] (see RangeLister). The provider starts at after through
// ListObjectsV2's StartAfter, sent on the first request only (later pages
// carry just the continuation token, since not every S3-compatible provider
// is known to accept both), and paging stops at the first page that passes
// through. The range is also enforced here, key by key, so a provider that
// ignored StartAfter (or a start key it could not be given) would cost extra
// pages but never return a key twice across ranges.
func (s *S3Store) ListRange(ctx context.Context, relativePrefix, after, through string) ([]Object, error) {
	prefix, err := s.keyForList(relativePrefix)
	if err != nil {
		return nil, err
	}
	// A start key the prefix rules refuse (a boundary a newer release would
	// no longer write, say) only costs the provider's head start: the range
	// is still enforced below, key by key.
	var start *string
	if key, err := s.key(after); after != "" && err == nil {
		start = aws.String(key)
	}
	input := &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), StartAfter: start}
	var objects []Object
	for {
		pageCtx, cancel := context.WithTimeout(ctx, readOperationTimeout)
		page, pageErr := s.client.ListObjectsV2(pageCtx, input)
		cancel()
		if pageErr != nil {
			return nil, pageErr
		}
		for _, item := range page.Contents {
			if item.Key == nil {
				continue
			}
			key := trimStorePrefix(*item.Key, s.prefix)
			if key <= after {
				continue
			}
			if through != "" && key > through {
				return objects, nil
			}
			objects = append(objects, Object{
				Key:          key,
				Size:         aws.ToInt64(item.Size),
				ETag:         strings.Trim(aws.ToString(item.ETag), "\""),
				LastModified: aws.ToTime(item.LastModified),
			})
		}
		if !aws.ToBool(page.IsTruncated) {
			return objects, nil
		}
		token := aws.ToString(page.NextContinuationToken)
		if token == "" {
			return nil, errors.New("list page is truncated without continuation token")
		}
		input = &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), ContinuationToken: aws.String(token)}
	}
}

func (s *S3Store) keyForList(relativePrefix string) (string, error) {
	if strings.TrimSpace(relativePrefix) == "" {
		if s.prefix == "" {
			return "", nil
		}
		return s.prefix + "/", nil
	}
	return s.key(relativePrefix)
}

// Delete removes the object at relative under the store's prefix.
func (s *S3Store) Delete(ctx context.Context, relative string) error {
	key, err := s.key(relative)
	if err != nil {
		return err
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return err
}

func trimStorePrefix(key, prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return key
	}
	return strings.TrimPrefix(strings.TrimPrefix(key, prefix), "/")
}

// isNotFound reports whether err means the object does not exist. It trusts
// only typed evidence: the SDK's NoSuchKey or NotFound error, or an HTTP
// response whose status is 404. Error text is never matched, so a failure
// whose message merely mentions "not found" (a 403, a DNS failure, a
// misconfigured endpoint) stays an error instead of reading as a missing
// object. A 404 whose code is NoSuchBucket names a missing bucket, which is a
// configuration problem, not an absent object. A 403 is never not-found here;
// Get and Stat ask confirmedAbsent about it separately. A 404 from a
// service the SDK asks for credentials (the EC2 metadata service, STS, SSO,
// a container credentials endpoint) is about the credentials and never
// means a missing object.
func isNotFound(err error) bool {
	if err == nil || credentialService(err) != "" {
		return false
	}
	var noSuchKey *types.NoSuchKey
	var notFound *types.NotFound
	if errors.As(err, &noSuchKey) || errors.As(err, &notFound) {
		return true
	}
	var noSuchBucket *types.NoSuchBucket
	if errors.As(err, &noSuchBucket) {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucket" {
		return false
	}
	var response *smithyhttp.ResponseError
	return errors.As(err, &response) && response.HTTPStatusCode() == http.StatusNotFound
}

// confirmedAbsent reports whether a read of key that was refused with HTTP 403
// was refused only because the object does not exist.
//
// S3 answers a GET or HEAD for a missing key with 404 only when the caller
// may list the bucket; otherwise it answers 403, so the caller cannot learn
// which keys exist. The least-privilege policy in
// docs/security/bucket-permissions.md grants s3:ListBucket only under an
// s3:prefix condition, and a GetObject request carries no s3:prefix, so a
// missing key there can read as 403. A session's first upload and show both
// need to tell "missing" from "denied". This asks the one question that
// policy does allow: a ListObjectsV2 whose Prefix is the key itself, for at
// most one result. A key sorts before every longer key it prefixes, so the
// first result is the key exactly when it exists.
//
// Only a successful listing that lacks the key counts as absence. Any other
// 403 (a present object the caller may not read, bad or expired credentials,
// a skewed clock) makes the listing fail or find the key, and the caller
// keeps the read's own error, so a real permission problem is never taken
// for a missing object.
func (s *S3Store) confirmedAbsent(ctx context.Context, key string, readErr error) bool {
	var response *smithyhttp.ResponseError
	if !errors.As(readErr, &response) || response.HTTPStatusCode() != http.StatusForbidden {
		return false
	}
	output, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(key), MaxKeys: aws.Int32(1)})
	if err != nil {
		return false
	}
	for _, item := range output.Contents {
		if aws.ToString(item.Key) == key {
			return false
		}
	}
	return true
}

func sha256Bytes(data []byte) [32]byte {
	// Kept separate from the hex helper so Put can pass the standard S3
	// base64 checksum header without a second encoding round trip.
	return sha256Sum(data)
}

var (
	_ ObjectStore   = (*S3Store)(nil)
	_ ObjectStatter = (*S3Store)(nil)
)

// NewConfiguredStore resolves the selected profile or credential store reference and
// builds the common S3 client used for both providers. It is deliberately
// small so CLI setup can perform its synthetic round trip without knowing SDK
// credential details.
func NewConfiguredStore(ctx context.Context, cfg credentials.Config, keychain credentials.CredentialStore) (*S3Store, error) {
	var awsCfg aws.Config
	var endpoint string
	var err error
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "s3":
		region := cfg.Region
		if strings.TrimSpace(region) == "" {
			return nil, errors.New("storage: AWS region is required")
		}
		awsCfg, err = credentials.LoadAWSConfig(ctx, cfg.AWSProfile, region)
	case "r2":
		awsCfg, endpoint, err = credentials.LoadR2Config(ctx, cfg, keychain)
	default:
		return nil, fmt.Errorf("storage: unsupported provider %q", cfg.Provider)
	}
	if err != nil {
		return nil, err
	}
	client := NewClient(awsCfg, endpoint, true, 3)
	return NewS3Store(S3StoreOptions{Provider: cfg.Provider, Client: client, Bucket: cfg.Bucket, Prefix: cfg.Prefix})
}

// ListBucketNames returns the names of every bucket the client's
// credentials can list, following all result pages, in the order the
// provider returns them. It needs s3:ListAllMyBuckets, which a
// least-privilege profile often lacks; callers fall back to asking for the
// name.
func ListBucketNames(ctx context.Context, client *s3.Client) ([]string, error) {
	pager := s3.NewListBucketsPaginator(client, &s3.ListBucketsInput{})
	var names []string
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, bucket := range page.Buckets {
			if name := aws.ToString(bucket.Name); name != "" {
				names = append(names, name)
			}
		}
	}
	return names, nil
}

// BucketRegion returns the AWS region bucket lives in, from
// GetBucketLocation. S3 reports us-east-1 as an empty location and the
// legacy eu-west-1 as "EU"; both are translated to region names.
func BucketRegion(ctx context.Context, client *s3.Client, bucket string) (string, error) {
	output, err := client.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(bucket)})
	if err != nil {
		return "", err
	}
	switch location := string(output.LocationConstraint); location {
	case "":
		return "us-east-1", nil
	case "EU":
		return "eu-west-1", nil
	default:
		return location, nil
	}
}
