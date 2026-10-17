package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

var _ CheckedLimitedGetter = (*S3Store)(nil)

// GetLimitedChecked performs one bounded GET and checks actual response closes,
// including error bodies consumed by the SDK. It preserves partial bytes on
// failure. Unlike the generic reader it does not confirm HTTP403 absence by listing:
// the SDK does not expose the listing body's Close. Only an unambiguous missing
// GET with no additional error returns the exact ErrNotFound sentinel.
func (s *S3Store) GetLimitedChecked(ctx context.Context, relative string, limit int64) (data []byte, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.client == nil {
		return nil, errors.New("storage: checked object reader requires an S3 client")
	}
	if limit <= 0 {
		return nil, errors.New("object read limit must be positive")
	}
	limit = min(limit, s.maxGetBytes)
	if limit <= 0 || limit == 1<<63-1 {
		return nil, ErrObjectTooLarge
	}
	key, err := s.key(relative)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, readOperationTimeout)
	defer cancel()
	capture := &checkedResponseCapture{}
	defer func() {
		if cleanup := errors.Join(capture.close(), ctx.Err()); cleanup != nil {
			err = errors.Join(err, cleanup)
		}
	}()
	checkedHTTP, err := checkedObjectHTTPClient(s.client.Options().HTTPClient, capture)
	if err != nil {
		return nil, err
	}
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}, func(options *s3.Options) {
		options.HTTPClient = checkedHTTP
		// One response keeps capture bounded and makes every actual close observable.
		// This affects this operation only; ordinary readers retain their retry policy.
		options.Retryer = aws.NopRetryer{}
	})
	if err != nil {
		if cleanup := errors.Join(capture.close(), ctx.Err()); cleanup != nil {
			return nil, errors.Join(err, cleanup)
		}
		if capture.statusCode() == http.StatusNotFound && checkedReadMissing(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if output == nil || output.Body == nil {
		return nil, errors.New("storage: checked GET returned no body")
	}
	return readCheckedObjectBody(ctx, output.Body, output.ContentLength, limit)
}

// Only concrete missing-object SDK semantics from an actual404 qualify. Generic
// status fallback, malformed diagnostics and unknown API codes remain errors.
func checkedReadMissing(err error) bool {
	if err == nil || checkedReadJoinedError(err) || credentialService(err) != "" {
		return false
	}
	var malformed *smithy.DeserializationError
	if errors.As(err, &malformed) {
		return false
	}
	var missing *types.NoSuchKey
	var notFound *types.NotFound
	return errors.As(err, &missing) || errors.As(err, &notFound)
}

// Do not classify one branch of a joined SDK error as complete missing evidence.
func checkedReadJoinedError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(interface{ Unwrap() []error }); ok {
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return checkedReadJoinedError(wrapped.Unwrap())
	}
	return false
}

// The per-operation transport observes the actual body even when SDK error
// unmarshalling owns it. Neither the shared client nor generic calls are changed.
// Stop redirects at the HTTP client, and capture below it so a transport
// returning BOTH response and error cannot hide its actual body's Close.
func checkedObjectHTTPClient(inner aws.HTTPClient, capture *checkedResponseCapture) (aws.HTTPClient, error) {
	switch client := inner.(type) {
	case tracedClient:
		controlled, err := checkedObjectHTTPClient(client.inner, capture)
		if err != nil {
			return nil, err
		}
		return tracedClient{inner: controlled}, nil
	case *awshttp.BuildableClient:
		if client == nil {
			return nil, errors.New("storage: checked GET HTTP client is nil")
		}
		// SDK Freeze preserves dial/TLS/proxy/read-timeout configuration. This
		// isolated copy has no idle pool whose ownership could escape the read.
		frozen := client.WithTransportOptions(func(t *http.Transport) { t.DisableKeepAlives = true }).Freeze()
		return checkedObjectHTTPClient(frozen, capture)
	case *http.Client:
		if client == nil {
			return nil, errors.New("storage: checked GET HTTP client is nil")
		}
		controlled := *client
		transport := controlled.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		controlled.Transport = checkedResponseTransport{inner: transport, capture: capture}
		controlled.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return &controlled, nil
	default:
		return nil, errors.New("storage: checked GET requires an inspectable HTTP transport")
	}
}

type checkedResponseTransport struct {
	inner   http.RoundTripper
	capture *checkedResponseCapture
}

func (c checkedResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	c.capture.mu.Lock()
	if c.capture.called {
		c.capture.mu.Unlock()
		return nil, errors.New("storage: checked GET attempted an additional response")
	}
	c.capture.called = true
	c.capture.mu.Unlock()
	if c.inner == nil {
		return nil, errors.New("storage: checked GET requires an HTTP client")
	}
	response, err := c.inner.RoundTrip(request)
	c.capture.mu.Lock()
	if response != nil {
		c.capture.status = response.StatusCode
	}
	c.capture.transportErr = err
	c.capture.mu.Unlock()
	if response != nil && response.Body != nil {
		body := &checkedResponseBody{inner: response.Body, ctx: request.Context()}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			body.errorRemaining = CheckedReadErrorBodyBytes + 1
			body.rejectErrorBody = response.ContentLength > CheckedReadErrorBodyBytes
		}
		c.capture.mu.Lock()
		c.capture.body = body
		c.capture.mu.Unlock()
		response.Body = body
		if err != nil {
			err = errors.Join(err, body.Close())
		}
	}
	if err != nil {
		return nil, err
	} // Body already captured/closed above.
	return response, nil
}

type checkedResponseCapture struct {
	mu           sync.Mutex
	called       bool
	body         *checkedResponseBody
	status       int
	transportErr error
}

func (c *checkedResponseCapture) close() error {
	c.mu.Lock()
	body := c.body
	transport := c.transportErr
	c.mu.Unlock()
	if body == nil {
		return transport
	}
	closing := body.Close()
	body.mu.Lock()
	reading := body.readErr
	body.mu.Unlock()
	return errors.Join(transport, reading, closing)
}

func (c *checkedResponseCapture) statusCode() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

type checkedResponseBody struct {
	inner           io.ReadCloser
	ctx             context.Context
	once            sync.Once
	err             error
	mu              sync.Mutex
	readErr         error
	errorRemaining  int64
	errorRead       int64
	rejectErrorBody bool
}

func (b *checkedResponseBody) Read(data []byte) (int, error) {
	remember := func(err error) {
		//lint:ignore errorlint Only bare EOF is completion; wrapped or joined EOF remains a failure.
		if err != nil && err != io.EOF { //nolint:errorlint // Only bare EOF is completion; wrapped or joined EOF remains a read failure.
			b.mu.Lock()
			b.readErr = err
			b.mu.Unlock()
		}
	}
	if err := b.ctx.Err(); err != nil {
		remember(err)
		return 0, err
	}
	if b.rejectErrorBody {
		remember(ErrObjectTooLarge)
		return 0, ErrObjectTooLarge
	}
	if b.errorRemaining > 0 {
		data = data[:min(int64(len(data)), b.errorRemaining)]
	} else if b.errorRead > 0 {
		remember(ErrObjectTooLarge)
		return 0, ErrObjectTooLarge
	}
	n, err := b.inner.Read(data)
	if b.errorRemaining > 0 {
		b.errorRemaining -= int64(n)
		b.errorRead += int64(n)
		if b.errorRead > CheckedReadErrorBodyBytes {
			err = errors.Join(err, ErrObjectTooLarge)
		}
	}
	if cancelled := b.ctx.Err(); cancelled != nil {
		err = errors.Join(err, cancelled)
	}
	remember(err)
	return n, err
}
func (b *checkedResponseBody) Close() error {
	b.once.Do(func() { b.err = b.inner.Close() })
	return b.err
}

func readCheckedObjectBody(ctx context.Context, body io.ReadCloser, length *int64, limit int64) (data []byte, err error) {
	if body == nil {
		return nil, errors.New("storage: checked object body is missing")
	}
	defer func() {
		if cleanup := errors.Join(body.Close(), ctx.Err()); cleanup != nil {
			err = errors.Join(err, cleanup)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit == 1<<63-1 {
		return nil, ErrObjectTooLarge
	}
	if length != nil && *length > limit {
		return nil, ErrObjectTooLarge
	}
	// One exact buffer avoids io.ReadAll's retained-chunk/final-copy peak.
	// The caller reserves the full limit+1 BEFORE this method, not just bytes
	// eventually returned. No capacity growth or final copy is performed.
	size := limit + 1
	if size > int64(int(^uint(0)>>1)) {
		return nil, ErrObjectTooLarge
	}
	buffer := make([]byte, int(size))
	reader := checkedObjectContextReader{ctx: ctx, reader: body}
	used, empty := 0, 0
	for used < len(buffer) {
		n, reading := reader.Read(buffer[used:])
		if n < 0 || n > len(buffer)-used {
			return buffer[:used], errors.New("storage: invalid checked body read count")
		}
		used += n
		if reading != nil {
			//lint:ignore errorlint Only bare EOF is completion; wrapped or joined EOF remains a failure.
			if reading != io.EOF { //nolint:errorlint // Only bare EOF is completion; wrapped or joined EOF must survive.
				err = reading
			}
			break
		}
		if n == 0 {
			empty++
			if empty == 100 {
				err = io.ErrNoProgress
				break
			}
		} else {
			empty = 0
		}
	}
	data = buffer[:used]
	if int64(used) > limit {
		err = errors.Join(err, ErrObjectTooLarge)
	}
	return data, err
}

type checkedObjectContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r checkedObjectContextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(data)
	if cancelled := r.ctx.Err(); cancelled != nil {
		err = errors.Join(err, cancelled)
	}
	return n, err
}
