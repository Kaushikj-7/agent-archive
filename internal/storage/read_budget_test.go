package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

type budgetClient struct {
	body     bool
	deadline time.Duration
}

func (c *budgetClient) Do(r *http.Request) (*http.Response, error) {
	deadline, ok := r.Context().Deadline()
	if !ok {
		return nil, errors.New("missing read deadline")
	}
	c.deadline = time.Until(deadline)
	if !c.body {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: &stalledReadBody{ctx: r.Context()}, Request: r}, nil
}

type stalledReadBody struct{ ctx context.Context }

func (b *stalledReadBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }

func (*stalledReadBody) Close() error { return nil }

type budgetTestOperation string

const (
	budgetGet   budgetTestOperation = "get"
	budgetHead  budgetTestOperation = "head"
	budgetPage  budgetTestOperation = "page"
	budgetList  budgetTestOperation = "list"
	budgetRange budgetTestOperation = "range"
)

func TestRemoteReadBoundsStalledHeadersAndBody(t *testing.T) {
	for _, body := range []bool{false, true} {
		for _, operation := range []budgetTestOperation{budgetGet, budgetHead, budgetPage, budgetList, budgetRange} {
			t.Run(string(operation)+map[bool]string{false: "-headers", true: "-body"}[body], func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					client := &budgetClient{body: body}
					cfg := aws.Config{Region: "test", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), HTTPClient: client}
					store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, "https://synthetic.invalid", true, 1), Bucket: "test", MaxGetBytes: 1024})
					if err != nil {
						t.Fatal(err)
					}
					start := time.Now()
					switch operation {
					case budgetGet:
						_, err = store.Get(context.Background(), "object")
					case budgetHead:
						_, err = store.Stat(context.Background(), "object")
					case budgetPage:
						_, err = store.ListPage(context.Background(), "sessions", "", 1000)
					case budgetList:
						_, err = store.List(context.Background(), "sessions")
					case budgetRange:
						_, err = store.ListRange(context.Background(), "sessions", "", "")
					}
					// HEAD has no body to consume, so a successful header is a complete read.
					if body && operation == budgetHead {
						return
					}
					if err == nil {
						t.Fatal("stalled service succeeded")
					}
					if client.deadline <= 0 || client.deadline > 2*time.Minute || time.Since(start) > 2*time.Minute {
						t.Fatalf("budget=%v elapsed=%v err=%v", client.deadline, time.Since(start), err)
					}
				})
			})
		}
	}
}

type successfulReadClient struct{}

func (successfulReadClient) Do(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("complete")), Request: r}, nil
}

func TestRemoteReadRetainsShorterCallerDeadlineAndSuccessfulBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &budgetClient{}
		cfg := aws.Config{Region: "test", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), HTTPClient: client}
		store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, "https://synthetic.invalid", true, 1), Bucket: "test", MaxGetBytes: 1024})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err = store.Get(ctx, "object"); err == nil || client.deadline > 3*time.Second {
			t.Fatalf("budget=%v err=%v", client.deadline, err)
		}
		cfg.HTTPClient = successfulReadClient{}
		store, err = NewS3Store(S3StoreOptions{Client: NewClient(cfg, "https://synthetic.invalid", true, 1), Bucket: "test", MaxGetBytes: 1024})
		if err != nil {
			t.Fatal(err)
		}
		data, err := store.Get(context.Background(), "object")
		if err != nil || string(data) != "complete" {
			t.Fatalf("data=%q err=%v", data, err)
		}
	})
}

func TestRemoteBodyReadCancelsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &budgetClient{body: true}
		cfg := aws.Config{Region: "test", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), HTTPClient: client}
		store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, "https://synthetic.invalid", true, 1), Bucket: "test", MaxGetBytes: 1024})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := store.Get(ctx, "object"); done <- err }()
		synctest.Wait()
		start := time.Now()
		cancel()
		if err := <-done; err == nil || time.Since(start) != 0 {
			t.Fatalf("elapsed=%v err=%v", time.Since(start), err)
		}
	})
}

// Each page takes most of one read budget, so the complete scan exceeds it.
type slowPagesClient struct{ pages int }

func (c *slowPagesClient) Do(r *http.Request) (*http.Response, error) {
	select {
	case <-time.After(90 * time.Second):
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	c.pages++
	body := `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`
	if c.pages == 1 {
		body = `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>next</NextContinuationToken></ListBucketResult>`
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func TestRemoteScanAllowsIndependentPageBudgets(t *testing.T) {
	for _, operation := range []string{"list", "range"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &slowPagesClient{}
				cfg := aws.Config{Region: "test", Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""), HTTPClient: client}
				store, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, "https://synthetic.invalid", true, 1), Bucket: "test", MaxGetBytes: 1024})
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				if operation == "list" {
					_, err = store.List(context.Background(), "sessions")
				} else {
					_, err = store.ListRange(context.Background(), "sessions", "", "")
				}
				if err != nil || client.pages != 2 || time.Since(start) != 3*time.Minute {
					t.Fatalf("pages=%d elapsed=%v err=%v", client.pages, time.Since(start), err)
				}
			})
		})
	}
}
