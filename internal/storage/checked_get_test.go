package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

type checkedReadTestBody struct {
	reader   io.Reader
	closeErr error
	cancel   context.CancelFunc
	read     int
	closes   int
}

func (b *checkedReadTestBody) Read(data []byte) (int, error) {
	n, err := b.reader.Read(data)
	b.read += n
	if b.cancel != nil {
		b.cancel()
	}
	return n, err
}

func (b *checkedReadTestBody) Close() error { b.closes++; return b.closeErr }

type checkedReadTestHTTP struct {
	transportErr error
	body         io.ReadCloser
	status       int
	length       *int64
	calls        int
	method       string
	path         string
}

func (h *checkedReadTestHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	h.calls++
	h.method = r.Method
	h.path = r.URL.Path
	headers := http.Header{}
	length := int64(-1)
	if h.length != nil {
		length = *h.length
		headers.Set("Content-Length", strconv.FormatInt(length, 10))
	}
	return &http.Response{StatusCode: h.status, Header: headers, Body: h.body, ContentLength: length}, h.transportErr
}

func checkedReadFixtureStore(t *testing.T, transport *checkedReadTestHTTP, readLimit int64) *S3Store {
	t.Helper()
	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("synthetic", "synthetic", ""), HTTPClient: &http.Client{Transport: transport}}
	s, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, "https://synthetic.invalid", true, 3), Bucket: "archive", Prefix: "owned", MaxGetBytes: readLimit})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestS3CheckedLimitedReadClosesActualResponseOnEveryBodyExit(t *testing.T) {
	closeErr := errors.New("synthetic close failure")
	for _, tc := range []struct {
		name     string
		length   *int64
		payload  string
		closeErr error
		limit    int64
		want     error
		readMax  int
	}{
		{"success", nil, "abc", nil, 8, nil, 3},
		{"close failure", nil, "abc", closeErr, 8, closeErr, 3},
		{"declared oversize", aws.Int64(99), "unread", closeErr, 8, ErrObjectTooLarge, 0},
		{"stream oversize", nil, "1234567890123456", closeErr, 8, ErrObjectTooLarge, 9},
		{"store cap", nil, "1234567890123456", nil, 99, ErrObjectTooLarge, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &checkedReadTestBody{reader: strings.NewReader(tc.payload), closeErr: tc.closeErr}
			transport := &checkedReadTestHTTP{body: body, status: http.StatusOK, length: tc.length}
			s := checkedReadFixtureStore(t, transport, 8)
			raw, err := s.GetLimitedChecked(t.Context(), "source", tc.limit)
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("%q %v", raw, err)
			}
			if tc.closeErr != nil && !errors.Is(err, tc.closeErr) {
				t.Fatalf("close error lost: %v", err)
			}
			if body.closes != 1 || body.read > tc.readMax || transport.calls != 1 || transport.method != http.MethodGet || transport.path != "/archive/owned/source" {
				t.Fatalf("closes=%d read=%d requests=%d %s %s", body.closes, body.read, transport.calls, transport.method, transport.path)
			}
			if len(raw) != body.read {
				t.Fatalf("partial byte evidence lost: %d/%d", len(raw), body.read)
			}
		})
	}
}

func TestS3CheckedMissingRequiresActualErrorBodyClose(t *testing.T) {
	closeErr := errors.New("synthetic error-body close failure")
	for _, closing := range []error{nil, closeErr} {
		t.Run(strconv.FormatBool(closing != nil), func(t *testing.T) {
			body := &checkedReadTestBody{reader: strings.NewReader(`<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`), closeErr: closing}
			transport := &checkedReadTestHTTP{body: body, status: http.StatusNotFound}
			s := checkedReadFixtureStore(t, transport, 64)
			raw, err := s.GetLimitedChecked(t.Context(), "missing", 8)
			if closing == nil {
				//lint:ignore errorlint Only bare ErrNotFound is canonical absence; wrapped or joined errors remain failures.
				if err != ErrNotFound { //nolint:errorlint // Only the bare sentinel is canonical absence; wrapped or joined failures must not qualify.
					t.Fatalf("canonical missing identity lost: %v", err)
				}
			} else {
				//lint:ignore errorlint Only bare ErrNotFound is canonical absence; wrapped or joined errors remain failures.
				if err == ErrNotFound || !errors.Is(err, closeErr) { //nolint:errorlint // Only the bare sentinel is canonical absence; wrapped or joined failures must not qualify.
					t.Fatalf("close failure became canonical absence: %v", err)
				}
			}
			if len(raw) != 0 || body.closes != 1 || transport.calls != 1 {
				t.Fatalf("raw=%d closes=%d requests=%d", len(raw), body.closes, transport.calls)
			}
		})
	}
}

func TestS3CheckedForbiddenDoesNotUseUncheckedListingOrRetry(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		body := &checkedReadTestBody{reader: strings.NewReader(`<Error><Code>AccessDenied</Code></Error>`)}
		transport := &checkedReadTestHTTP{body: body, status: status}
		s := checkedReadFixtureStore(t, transport, 64)
		//lint:ignore errorlint Only bare ErrNotFound is canonical absence; wrapped or joined errors remain failures.
		if _, err := s.GetLimitedChecked(t.Context(), "source", 8); err == nil || err == ErrNotFound { //nolint:errorlint // Only the bare sentinel is canonical absence; wrapped or joined failures must not qualify.
			t.Fatalf("status=%d %v", status, err)
		}
		if transport.calls != 1 || body.closes != 1 {
			t.Fatalf("status=%d requests=%d closes=%d", status, transport.calls, body.closes)
		}
	}
}

func TestS3CheckedReadDoesNotChangeGenericCloseSemantics(t *testing.T) {
	closeErr := errors.New("synthetic close failure")
	for _, limited := range []bool{false, true} {
		body := &checkedReadTestBody{reader: strings.NewReader("abc"), closeErr: closeErr}
		transport := &checkedReadTestHTTP{body: body, status: http.StatusOK}
		s := checkedReadFixtureStore(t, transport, 64)
		var raw []byte
		var err error
		if limited {
			raw, err = s.GetLimited(t.Context(), "source", 8)
		} else {
			raw, err = s.Get(t.Context(), "source")
		}
		if err != nil || string(raw) != "abc" || body.closes != 1 {
			t.Fatalf("generic read changed: %q %v closes=%d", raw, err, body.closes)
		}
	}
}

func TestS3CheckedReadCancellationAndInvalidLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	closeErr := errors.New("synthetic close failure")
	body := &checkedReadTestBody{reader: strings.NewReader("abc"), closeErr: closeErr, cancel: cancel}
	transport := &checkedReadTestHTTP{body: body, status: http.StatusOK}
	s := checkedReadFixtureStore(t, transport, 64)
	if _, err := s.GetLimitedChecked(ctx, "source", 8); !errors.Is(err, context.Canceled) || !errors.Is(err, closeErr) {
		t.Fatalf("read/close cancellation lost: %v", err)
	}
	if body.closes != 1 || body.read > 9 {
		t.Fatalf("closes=%d read=%d", body.closes, body.read)
	}
	calls := transport.calls
	for _, limit := range []int64{0, -1} {
		if _, err := s.GetLimitedChecked(t.Context(), "source", limit); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	if _, err := s.GetLimitedChecked(ctx, "source", 8); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if transport.calls != calls {
		t.Fatal("invalid/cancelled read reached provider")
	}
	s.maxGetBytes = 1<<63 - 1
	if _, err := s.GetLimitedChecked(t.Context(), "source", 1<<63-1); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatal(err)
	}
	if transport.calls != calls {
		t.Fatal("overflow limit reached provider")
	}
}

// Private body control isolates partial-read+close joining without depending on
// SDK checksum/error wrapping. It grants no configured-provider provenance.
func TestCheckedObjectBodyPreservesPartialReadAndCloseErrors(t *testing.T) {
	readErr := errors.New("synthetic read failure")
	closeErr := errors.New("synthetic close failure")
	body := &checkedReadTestBody{reader: checkedReadFailure{err: readErr}, closeErr: closeErr}
	raw, err := readCheckedObjectBody(t.Context(), body, nil, 8)
	if string(raw) != "abc" || !errors.Is(err, readErr) || !errors.Is(err, closeErr) || body.closes != 1 {
		t.Fatalf("%q %v closes=%d", raw, err, body.closes)
	}
	if !checkedReadJoinedError(errors.Join(ErrNotFound, readErr)) || !checkedReadJoinedError(&checkedReadWrappedError{err: errors.Join(ErrNotFound, readErr)}) {
		t.Fatal("joined error branch could become absence")
	}
}

type checkedReadFailure struct{ err error }

func (r checkedReadFailure) Read(data []byte) (int, error) { return copy(data, "abc"), r.err }

type checkedReadWrappedError struct{ err error }

func (e *checkedReadWrappedError) Error() string { return "synthetic wrapper" }

func (e *checkedReadWrappedError) Unwrap() error { return e.err }

func TestS3CheckedMissingBucketAndInvalidKeyRemainFailures(t *testing.T) {
	body := &checkedReadTestBody{reader: strings.NewReader(`<Error><Code>NoSuchBucket</Code></Error>`)}
	transport := &checkedReadTestHTTP{body: body, status: http.StatusNotFound}
	s := checkedReadFixtureStore(t, transport, 64)
	//lint:ignore errorlint Only bare ErrNotFound is canonical absence; wrapped or joined errors remain failures.
	if _, err := s.GetLimitedChecked(t.Context(), "source", 8); err == nil || err == ErrNotFound { //nolint:errorlint // Only the bare sentinel is canonical absence; wrapped or joined failures must not qualify.
		t.Fatalf("missing bucket became missing object: %v", err)
	}
	if body.closes != 1 {
		t.Fatal("missing-bucket response not closed")
	}
	calls := transport.calls
	for _, key := range []string{"", "/absolute", "../outside", "safe/../outside"} {
		if _, err := s.GetLimitedChecked(t.Context(), key, 8); err == nil {
			t.Fatalf("invalid key accepted: %q", key)
		}
	}
	if transport.calls != calls {
		t.Fatal("invalid key reached provider")
	}
}

func TestS3CheckedReadBoundsErrorBodiesBeforeSDKDecoding(t *testing.T) {
	for _, declared := range []bool{false, true} {
		t.Run(strconv.FormatBool(declared), func(t *testing.T) {
			stream := &streamingBody{}
			body := &checkedReadTestBody{reader: stream}
			var length *int64
			if declared {
				length = aws.Int64(CheckedReadErrorBodyBytes + 100)
			}
			transport := &checkedReadTestHTTP{body: body, status: http.StatusNotFound, length: length}
			s := checkedReadFixtureStore(t, transport, 64)
			_, err := s.GetLimitedChecked(t.Context(), "missing", 1)
			//lint:ignore errorlint Only bare ErrNotFound is canonical absence; wrapped or joined errors remain failures.
			if err == ErrNotFound || !errors.Is(err, ErrObjectTooLarge) { //nolint:errorlint // Only the bare sentinel is canonical absence; wrapped or joined failures must not qualify.
				t.Fatalf("oversized SDK diagnostics became absence: %v", err)
			}
			if int64(body.read) > CheckedReadErrorBodyBytes+1 || body.closes != 1 {
				t.Fatalf("error-body bytes=%d closes=%d", body.read, body.closes)
			}
			if declared && body.read != 0 {
				t.Fatal("declared oversized SDK body was read")
			}
		})
	}
}

func TestS3CheckedMissingRefusesCorruptContradictoryAndTransportResponses(t *testing.T) {
	transportErr := errors.New("synthetic response transport failure")
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		transport error
	}{
		{"malformed404", 404, `<Error><Code>NoSuchKey</Code>`, nil},
		{"HTML404", 404, `<html><body>missing route</body></html>`, nil},
		{"empty404", 404, ``, nil},
		{"denied404", 404, `<Error><Code>AccessDenied</Code></Error>`, nil},
		{"unknown404", 404, `<Error><Code>OtherFailure</Code></Error>`, nil},
		{"bucket404", 404, `<Error><Code>NoSuchBucket</Code></Error>`, nil},
		{"missing403", 403, `<Error><Code>NoSuchKey</Code></Error>`, nil},
		{"missing503", 503, `<Error><Code>NoSuchKey</Code></Error>`, nil},
		{"transport404", 404, `<Error><Code>NoSuchKey</Code></Error>`, transportErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &checkedReadTestBody{reader: strings.NewReader(tc.body)}
			transport := &checkedReadTestHTTP{body: body, status: tc.status, transportErr: tc.transport}
			store := checkedReadFixtureStore(t, transport, 64)
			_, err := store.GetLimitedChecked(t.Context(), "source", 8)
			//lint:ignore errorlint Only bare ErrNotFound is canonical absence; wrapped or joined errors remain failures.
			if err == nil || err == ErrNotFound { //nolint:errorlint // Only the bare sentinel is canonical absence; wrapped or joined failures must not qualify.
				t.Fatalf("invalid missing evidence became absence: %v", err)
			}
			if tc.transport != nil && !errors.Is(err, tc.transport) {
				t.Fatalf("actual transport error lost: %v", err)
			}
			if body.closes != 1 || transport.calls != 1 {
				t.Fatalf("closes=%d requests=%d", body.closes, transport.calls)
			}
		})
	}
}

func TestS3CheckedReadRefusesRedirectBeforeFollowingAndChecksItsClose(t *testing.T) {
	closeErr := errors.New("synthetic redirect-body close failure")
	body := &checkedReadTestBody{reader: strings.NewReader("redirect"), closeErr: closeErr}
	calls := 0
	transport := checkedRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == "/archive/owned/source" {
			return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": []string{"/target"}}, Body: body}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("followed"))}, nil
	})
	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("synthetic", "synthetic", ""), HTTPClient: &http.Client{Transport: transport}}
	store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, "https://synthetic.invalid", true, 3), Bucket: "archive", Prefix: "owned"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetLimitedChecked(t.Context(), "source", 64); err == nil || !errors.Is(err, closeErr) {
		t.Fatalf("redirect/close failure disappeared: %v", err)
	}
	if calls != 1 || body.closes != 1 {
		t.Fatalf("redirect followed/close lost: calls=%d closes=%d", calls, body.closes)
	}
}

func TestS3CheckedBuildableClientDoesNotFollowOwnedEndpointRedirect(t *testing.T) {
	var targets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			targets.Add(1)
			_, _ = io.WriteString(w, "followed")
			return
		}
		w.Header().Set("Location", "/target")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("synthetic", "synthetic", "")}
	store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, server.URL, true, 3), Bucket: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	//lint:ignore errorlint Only bare ErrNotFound is canonical absence; wrapped or joined errors remain failures.
	if _, err := store.GetLimitedChecked(t.Context(), "source", 64); err == nil || err == ErrNotFound { //nolint:errorlint // Only the bare sentinel is canonical absence; wrapped or joined failures must not qualify.
		t.Fatalf("redirect became accepted read: %v", err)
	}
	if targets.Load() != 0 {
		t.Fatal("actual SDK BuildableClient followed redirect")
	}
}

type checkedRoundTripFunc func(*http.Request) (*http.Response, error)

func (f checkedRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCheckedObjectBodyUsesOneExactBoundedPayloadBuffer(t *testing.T) {
	for _, limit := range []int64{8, 32 << 10, 1 << 20} {
		body := &checkedReadTestBody{reader: strings.NewReader("abc")}
		raw, err := readCheckedObjectBody(t.Context(), body, nil, limit)
		if err != nil || string(raw) != "abc" || int64(cap(raw)) != limit+1 || body.closes != 1 {
			t.Fatalf("limit=%d payload=%q cap=%d closes=%d err=%v", limit, raw, cap(raw), body.closes, err)
		}
	}
	body := &checkedReadTestBody{reader: strings.NewReader("not read")}
	raw, err := readCheckedObjectBody(t.Context(), body, aws.Int64(99), 8)
	if !errors.Is(err, ErrObjectTooLarge) || raw != nil || body.read != 0 || body.closes != 1 {
		t.Fatalf("known oversize allocated/read payload: %v bytes=%d closes=%d", err, body.read, body.closes)
	}
}

func TestCheckedObjectBodyPreservesWrappedAndJoinedEOFFailures(t *testing.T) {
	readFailure := errors.New("synthetic read failure")
	closeFailure := errors.New("synthetic checked close failure")
	for _, reading := range []error{fmt.Errorf("synthetic wrapper: %w", io.EOF), errors.Join(io.EOF, readFailure)} {
		body := &checkedReadTestBody{reader: checkedReadFailure{err: reading}, closeErr: closeFailure}
		raw, err := readCheckedObjectBody(t.Context(), body, nil, 8)
		if err == nil || string(raw) != "abc" || !errors.Is(err, reading) || !errors.Is(err, io.EOF) || !errors.Is(err, closeFailure) || body.closes != 1 {
			t.Fatalf("wrapped/joined EOF became completion: data=%q err=%v closes=%d", raw, err, body.closes)
		}
	}
}

// Exercise the actual response capture separately: even if SDK code consumes a
// wrapped/joined EOF, the remembered failure must survive until checked close.
func TestCheckedResponseCaptureRemembersWrappedAndJoinedEOF(t *testing.T) {
	readFailure := errors.New("synthetic read failure")
	closeFailure := errors.New("synthetic checked close failure")
	for _, reading := range []error{fmt.Errorf("synthetic wrapper: %w", io.EOF), errors.Join(io.EOF, readFailure)} {
		inner := &checkedReadTestBody{reader: checkedReadFailure{err: reading}, closeErr: closeFailure}
		body := &checkedResponseBody{inner: inner, ctx: t.Context()}
		capture := &checkedResponseCapture{body: body}
		var data [8]byte
		n, err := body.Read(data[:])
		if n != 3 || string(data[:n]) != "abc" || !errors.Is(err, reading) {
			t.Fatalf("partial read changed: n=%d err=%v", n, err)
		}
		closing := capture.close()
		if closing == nil || !errors.Is(closing, reading) || !errors.Is(closing, closeFailure) || inner.closes != 1 {
			t.Fatalf("captured EOF failure lost: %v closes=%d", closing, inner.closes)
		}
	}
}
