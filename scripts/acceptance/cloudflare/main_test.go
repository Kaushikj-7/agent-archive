package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/wangjohn/agent-archive/internal/cloudflare"
	"github.com/wangjohn/agent-archive/internal/storage"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReportRefusesSecretsAndUsesPrivateMode(t *testing.T) {
	r := &runner{path: filepath.Join(t.TempDir(), "report.json"), secrets: []string{"canary-secret-should-not-be-saved"}, r: report{RunID: "synthetic"}}
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(r.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("report must be private", err)
	}
	r.r.Checks = []check{{Detail: "canary-secret-should-not-be-saved"}}
	if err := r.save(); err == nil {
		t.Fatal("secret-bearing report accepted")
	}
	data, err := os.ReadFile(r.path)
	if err != nil || strings.Contains(string(data), r.secrets[0]) {
		t.Fatal("secret escaped into report", err)
	}
}

func TestCleanupDeletesOnlyConfirmedNewBucketsAndChecksEnvelope(t *testing.T) {
	for _, success := range []bool{true, false} {
		calls := 0
		r := &runner{path: filepath.Join(t.TempDir(), "report.json"), token: "synthetic-token", r: report{Account: strings.Repeat("a", 32), Buckets: []bucket{{Name: "aa-accept-confirmed", Created: true}, {Name: "aa-accept-uncertain", Created: false}}}}
		r.bucketCleanupClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Method != http.MethodDelete || req.URL.Host != "api.cloudflare.com" || !strings.HasSuffix(req.URL.Path, "/aa-accept-confirmed") || req.Header.Get("Authorization") != "Bearer synthetic-token" {
				t.Fatal("wrong cleanup target")
			}
			body := `{"success":false}`
			if success {
				body = `{"success":true}`
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}
		r.cleanup()
		if calls != 1 || r.r.Buckets[0].Deleted != success || r.r.Buckets[1].Deleted {
			t.Fatal("cleanup inferred unconfirmed deletion")
		}
	}
}

func TestErrorsNeverExposeProviderMessages(t *testing.T) {
	err := &cloudflare.Error{Status: 403, Codes: []int{1000}, Messages: []string{"sensitive-provider-canary"}}
	if strings.Contains(safeError(err), "canary") {
		t.Fatal("raw provider message escaped")
	}
}

func cleanupFixture(t *testing.T) (*runner, *[]string) {
	t.Helper()
	var requests []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodDelete || req.Context().Err() != nil {
			t.Fatal("cleanup must use DELETE and an uncancelled context")
		}
		requests = append(requests, req.URL.Path)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true,"result":{}}`)), Header: make(http.Header)}, nil
	})}
	r := &runner{
		path: filepath.Join(t.TempDir(), "report.json"), ctx: context.Background(),
		api:                 cloudflare.New("synthetic-token", cloudflare.Options{HTTPClient: client}),
		bucketCleanupClient: client,
		r: report{Account: strings.Repeat("a", 32),
			Tokens:  []issued{{ID: "key-one"}, {ID: "key-two"}, {ID: "key-three"}},
			Buckets: []bucket{{Name: "aa-accept-one", Created: true}, {Name: "aa-accept-two", Created: true}},
		},
	}
	return r, &requests
}

func assertCompleteCleanup(t *testing.T, r *runner, requests []string) {
	t.Helper()
	if len(requests) != 5 {
		t.Fatalf("expected all three token and two bucket deletions, got %v", requests)
	}
	for _, token := range r.r.Tokens {
		if !token.Deleted {
			t.Fatal("token cleanup skipped")
		}
	}
	for _, bucket := range r.r.Buckets {
		if !bucket.Deleted {
			t.Fatal("bucket cleanup skipped")
		}
	}
}

func TestInterruptedAcceptanceFailsAfterSuccessfulCleanup(t *testing.T) {
	r, requests := cleanupFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.ctx = ctx
	r.execute(func() {
		r.record("previous check", "pass", "", 0)
		cancel() // Simulate returning from an interrupted retry loop.
	})
	assertCompleteCleanup(t, r, *requests)
	if !r.failed() {
		t.Fatal("interrupted acceptance reported success")
	}
	data, err := os.ReadFile(r.path)
	if err != nil || !strings.Contains(string(data), "acceptance interrupted") {
		t.Fatal("interruption was not recorded", err)
	}
}

func TestPersistenceFailureDoesNotStopCleanup(t *testing.T) {
	for _, duringRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "during-cleanup", true: "during-run"}[duringRun], func(t *testing.T) {
			r, requests := cleanupFixture(t)
			if err := r.save(); err != nil {
				t.Fatal(err)
			}
			r.execute(func() {
				if err := os.RemoveAll(filepath.Dir(r.path)); err != nil {
					t.Fatal(err)
				}
				if duringRun {
					r.record("previous check", "pass", "", 0)
				}
			})
			assertCompleteCleanup(t, r, *requests)
			if r.persistenceErr == nil || !r.failed() {
				t.Fatal("report persistence failure was not retained as a run failure")
			}
		})
	}
}

// stalledCleanupBody models headers arriving while the DELETE body never finishes.
// release allows the before-fix variant to unwind without leaking a goroutine.
type stalledCleanupBody struct {
	ctx     context.Context
	reading chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *stalledCleanupBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.reading) })
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.release:
		return 0, io.EOF
	}
}

func (*stalledCleanupBody) Close() error { return nil }

func TestDeferredObjectCleanupBoundsBodyAndReachesResourceCleanup(t *testing.T) {
	r, requests := cleanupFixture(t)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.ctx = parent
	cancel() // Cleanup must still issue its DELETE after interruption.
	reading, release := make(chan struct{}), make(chan struct{})
	requestContext := make(chan context.Context, 1)
	transport := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requestContext <- req.Context()
		return &http.Response{StatusCode: http.StatusForbidden, Request: req,
			Header: http.Header{"Content-Type": []string{"application/xml"}},
			Body:   &stalledCleanupBody{ctx: req.Context(), reading: reading, release: release}}, nil
	})}
	cfg := aws.Config{Region: "auto", HTTPClient: transport,
		Credentials: awscreds.NewStaticCredentialsProvider("synthetic-id", "synthetic-secret", "")}
	store, err := storage.NewS3Store(storage.S3StoreOptions{Provider: "r2",
		Client: storage.NewClient(cfg, "https://synthetic.invalid", true, 1), Bucket: "aa-accept-one"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var deleteErr error
		r.execute(func() {
			defer func() {
				deleteErr = deleteScratchObject(parent, store, "outside-archive/synthetic", 100*time.Millisecond)
			}()
		})
		done <- deleteErr
	}()
	// Release the synthetic response and join even if a broken implementation hangs.
	defer func() {
		close(release)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("cleanup worker did not exit after releasing synthetic body")
		}
	}()
	select {
	case <-reading:
	case <-time.After(time.Second):
		t.Fatal("DELETE response body was not read")
	}
	ctx := <-requestContext
	select {
	case err := <-done:
		// Put the result back so the deferred join has a completed worker to observe.
		done <- err
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("interruption-independent DELETE has no deadline")
		}
		if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("stalled body did not terminate at cleanup deadline: err=%v context=%v", err, ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("stalled DELETE prevented final resource cleanup")
	}
	assertCompleteCleanup(t, r, *requests)
	if !r.failed() {
		t.Fatal("interrupted acceptance must remain a failure after cleanup")
	}
}
