// Package providertest exercises production storage against disposable CI S3.
package providertest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/smithy-go"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// DisposableS3 records actual provider requests in an isolated test prefix.
// It cannot resolve ambient credentials or contact a non-loopback endpoint.
type DisposableS3 struct {
	*storage.S3Store
	http     *disposableHTTP
	endpoint string
	bucket   string
	prefix   string
}

type disposableHTTP struct {
	client   *http.Client
	origin   string
	mu       sync.Mutex
	requests int
	statuses map[int]int
}

func (c *disposableHTTP) Do(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	if r.URL.Scheme+"://"+r.URL.Host != c.origin {
		return nil, errors.New("acceptance request left disposable provider origin")
	}
	c.mu.Lock()
	if c.requests >= 4096 {
		c.mu.Unlock()
		return nil, errors.New("acceptance provider request budget exhausted")
	}
	c.requests++
	c.mu.Unlock()
	response, err := c.client.Do(r)
	if response != nil {
		c.mu.Lock()
		c.statuses[response.StatusCode]++
		c.mu.Unlock()
	}
	return response, err
}

// Requests returns the number of real HTTP attempts, including provider errors.
func (s *DisposableS3) Requests() int {
	s.http.mu.Lock()
	defer s.http.mu.Unlock()
	return s.http.requests
}

// NewDisposableS3 requires explicit opt-in and a scratch bucket created by CI.
// Cleanup lists and deletes only this constructor's random prefix.
func NewDisposableS3(t *testing.T) *DisposableS3 {
	t.Helper()
	if os.Getenv("AGENT_ARCHIVE_PROVIDER_ACCEPTANCE") != "1" {
		t.Skip("disposable real-provider acceptance not requested")
	}
	endpoint := os.Getenv("AA_PROVIDER_ENDPOINT")
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		t.Fatal("acceptance requires an explicit loopback disposable provider endpoint")
	}
	bucket := os.Getenv("AA_PROVIDER_BUCKET")
	access, secret := os.Getenv("AA_PROVIDER_ACCESS"), os.Getenv("AA_PROVIDER_SECRET")
	if bucket != "aa-disposable-acceptance" || access == "" || secret == "" {
		t.Fatal("acceptance scratch bucket and static credentials are required")
	}
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Ephemeral credentials never pass through an ambient proxy.
	counter := &disposableHTTP{origin: endpoint, statuses: map[int]int{}, client: &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider(access, secret, ""), HTTPClient: counter}
	runPrefix := os.Getenv("AA_PROVIDER_RUN_PREFIX")
	decoded, err := hex.DecodeString(strings.TrimPrefix(runPrefix, "aa-provider-"))
	if err != nil || !strings.HasPrefix(runPrefix, "aa-provider-") || len(decoded) != 8 || hex.EncodeToString(decoded) != strings.TrimPrefix(runPrefix, "aa-provider-") {
		t.Fatal("acceptance requires an exact disposable run prefix")
	}
	prefix := runPrefix + "/run-" + hex.EncodeToString(suffix[:])
	remote, err := storage.NewS3Store(storage.S3StoreOptions{Provider: "s3", Client: storage.NewClient(cfg, endpoint, true, 1), Bucket: bucket, Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	verifyCredentialScope(t, cfg, endpoint, bucket, runPrefix)
	store := &DisposableS3{S3Store: remote, http: counter, endpoint: endpoint, bucket: bucket, prefix: prefix}
	t.Cleanup(func() { cleanupDisposableS3(t, store) })
	return store
}

// IndependentOwner creates a distinct credential/client identity in the same
// disposable destination. The original owner's cleanup still owns the prefix.
func (s *DisposableS3) IndependentOwner(t *testing.T) *DisposableS3 {
	t.Helper()
	access, secret := os.Getenv("AA_PROVIDER_PEER_ACCESS"), os.Getenv("AA_PROVIDER_PEER_SECRET")
	if access == "" || secret == "" || access == os.Getenv("AA_PROVIDER_ACCESS") {
		t.Fatal("a separately issued disposable provider identity is required")
	}
	cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider(access, secret, ""), HTTPClient: s.http}
	remote, err := storage.NewS3Store(storage.S3StoreOptions{Provider: "s3", Client: storage.NewClient(cfg, s.endpoint, true, 1), Bucket: s.bucket, Prefix: s.prefix})
	if err != nil {
		t.Fatal(err)
	}
	verifyCredentialScope(t, cfg, s.endpoint, s.bucket, os.Getenv("AA_PROVIDER_RUN_PREFIX"))
	return &DisposableS3{S3Store: remote, http: s.http, endpoint: s.endpoint, bucket: s.bucket, prefix: s.prefix}
}

// Successful out-of-prefix work is a failed gate, even in a disposable service.
// Require the actual provider's authorization error, not a transport failure.
func verifyCredentialScope(t *testing.T, cfg aws.Config, endpoint, bucket, runPrefix string) {
	t.Helper()
	outside, err := storage.NewS3Store(storage.S3StoreOptions{Provider: "s3", Client: storage.NewClient(cfg, endpoint, true, 1), Bucket: bucket, Prefix: runPrefix + "-outside"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	putErr := outside.Put(ctx, "scope-probe", []byte("synthetic scope probe"))
	_, listErr := outside.List(ctx, "")
	for _, denial := range []error{putErr, listErr} {
		var apiError smithy.APIError
		if !errors.As(denial, &apiError) || apiError.ErrorCode() != "AccessDenied" {
			t.Fatal("provider credential did not prove out-of-prefix AccessDenied", denial)
		}
	}
	t.Log("real provider refused out-of-prefix PUT and LIST with AccessDenied")
}

func cleanupDisposableS3(t *testing.T, s *DisposableS3) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	page, err := s.ListPage(ctx, "", "", 513)
	if err != nil || len(page.Objects) > 512 || page.Next != "" {
		t.Error("disposable prefix cleanup inventory failed or exceeded bounds", err)
		return
	}
	for _, object := range page.Objects {
		if err := s.Delete(ctx, object.Key); err != nil {
			t.Error("disposable prefix cleanup failed", err)
		}
	}
	remaining, err := s.ListPage(ctx, "", "", 1)
	if err != nil || len(remaining.Objects) != 0 || remaining.Next != "" {
		t.Error("disposable prefix cleanup incomplete", err)
	}
	s.http.mu.Lock()
	defer s.http.mu.Unlock()
	t.Logf("real S3 HTTP requests=%d statuses=%v; prefix cleanup verified", s.http.requests, s.http.statuses)
}
