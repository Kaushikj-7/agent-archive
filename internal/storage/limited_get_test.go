package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

type streamingBody struct{ read int }

func (b *streamingBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	b.read += len(p)
	return len(p), nil
}

func (*streamingBody) Close() error { return nil }

type boundedHTTP struct {
	body   io.ReadCloser
	length *int64
}

func (h boundedHTTP) Do(*http.Request) (*http.Response, error) {
	headers := http.Header{}
	length := int64(-1)
	if h.length != nil {
		length = *h.length
		headers.Set("Content-Length", strconv.FormatInt(length, 10))
	}
	return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: h.body, ContentLength: length}, nil
}

func TestS3LimitedGetStopsBeforeLargeBodyAllocation(t *testing.T) {
	t.Parallel()
	body := &streamingBody{}
	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("synthetic", "synthetic", ""), HTTPClient: boundedHTTP{body: body, length: aws.Int64(1 << 30)}}
	store, e := NewS3Store(S3StoreOptions{Client: NewClient(cfg, "https://synthetic.invalid", true, 1), Bucket: "test", MaxGetBytes: 1 << 20})
	if e != nil {
		t.Fatal(e)
	}
	_, e = store.GetLimited(context.Background(), "machines/id.json", 16<<10)
	if !errors.Is(e, ErrObjectTooLarge) || body.read != 0 {
		t.Fatalf("err=%v bytes=%d", e, body.read)
	}
}

func TestLimitedGetDoesNotChangeArchiveGetLimit(t *testing.T) {
	t.Parallel()
	server := newFakeS3Server()
	defer server.Close()
	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("synthetic", "synthetic", "")}
	s, e := NewS3Store(S3StoreOptions{Client: NewClient(cfg, server.URL, true, 1), Bucket: "test", MaxGetBytes: 64})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Put(context.Background(), "body", []byte(strings.Repeat("a", 32))); e != nil {
		t.Fatal(e)
	}
	if _, e = s.GetLimited(context.Background(), "body", 8); !errors.Is(e, ErrObjectTooLarge) {
		t.Fatal(e)
	}
	b, e := s.Get(context.Background(), "body")
	if e != nil || len(b) != 32 {
		t.Fatalf("%d %v", len(b), e)
	}
	if _, e = s.GetLimited(context.Background(), "body", 0); e == nil {
		t.Fatal("zero limit accepted")
	}
}

var _ io.ReadCloser = (*streamingBody)(nil)

type partialErrorBody struct{ read int }

func (b *partialErrorBody) Read(p []byte) (int, error) {
	if b.read > 0 {
		return 0, errors.New("synthetic stream failure")
	}
	b.read = copy(p, "synthetic")
	return b.read, errors.New("synthetic stream failure")
}

func (*partialErrorBody) Close() error { return nil }

func TestS3BoundedErrorReadsRetainByteEvidence(t *testing.T) {
	t.Parallel()
	for _, catalogRead := range []bool{false, true} {
		t.Run(strconv.FormatBool(catalogRead), func(t *testing.T) {
			t.Parallel()
			for _, length := range []*int64{nil, aws.Int64(2), aws.Int64(1 << 30)} {
				body := &streamingBody{}
				cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("synthetic", "synthetic", ""), HTTPClient: boundedHTTP{body: body, length: length}}
				s, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, "https://synthetic.invalid", true, 1), Bucket: "test", MaxGetBytes: 64})
				if err != nil {
					t.Fatal(err)
				}
				var data []byte
				if catalogRead {
					data, _, err = s.GetCatalogVersion(t.Context(), "body", 8)
				} else {
					data, err = s.GetLimited(t.Context(), "body", 8)
				}
				if !errors.Is(err, ErrObjectTooLarge) || body.read > 9 || len(data) != body.read {
					t.Fatalf("length=%v: retained=%d actual=%d %v", length, len(data), body.read, err)
				}
				if length != nil && *length > 8 && body.read != 0 {
					t.Fatal("known oversized body was read")
				}
			}
			body := &partialErrorBody{}
			cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("synthetic", "synthetic", ""), HTTPClient: boundedHTTP{body: body}}
			s, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, "https://synthetic.invalid", true, 1), Bucket: "test", MaxGetBytes: 64})
			if err != nil {
				t.Fatal(err)
			}
			var data []byte
			if catalogRead {
				data, _, err = s.GetCatalogVersion(t.Context(), "body", 32)
			} else {
				data, err = s.GetLimited(t.Context(), "body", 32)
			}
			if err == nil || len(data) != body.read || len(data) == 0 {
				t.Fatalf("partial bytes discarded: %d actual=%d %v", len(data), body.read, err)
			}
		})
	}
}
