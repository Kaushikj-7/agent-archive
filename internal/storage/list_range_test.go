package storage

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

// pagingS3 serves ListObjectsV2 over a fixed key set with real paging:
// prefix, start-after, max-keys (capped at pageSize) and continuation tokens.
// With ignoreStartAfter it behaves like a provider that drops the parameter.
type pagingS3 struct {
	keys             []string
	pageSize         int
	ignoreStartAfter bool

	mu          sync.Mutex
	requests    []string // start-after|continuation-token of each request
	transferred int      // full-page keys, including discarded range suffixes
}

func (p *pagingS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	p.mu.Lock()
	p.requests = append(p.requests, query.Get("start-after")+"|"+query.Get("continuation-token"))
	p.mu.Unlock()
	start := query.Get("start-after")
	if p.ignoreStartAfter {
		start = ""
	}
	if token := query.Get("continuation-token"); token != "" {
		start = token
	}
	var matched []string
	for _, key := range p.keys {
		if strings.HasPrefix(key, query.Get("prefix")) && key > start {
			matched = append(matched, key)
		}
	}
	type content struct {
		Key  string `xml:"Key"`
		Size int    `xml:"Size"`
	}
	type result struct {
		XMLName               xml.Name  `xml:"ListBucketResult"`
		IsTruncated           bool      `xml:"IsTruncated"`
		NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
		Contents              []content `xml:"Contents"`
	}
	truncated, next := false, ""
	if len(matched) > p.pageSize {
		matched = matched[:p.pageSize]
		truncated, next = true, matched[len(matched)-1]
	}
	p.mu.Lock()
	p.transferred += len(matched)
	p.mu.Unlock()
	contents := make([]content, 0, len(matched))
	for _, key := range matched {
		contents = append(contents, content{Key: key, Size: 1})
	}
	out := result{IsTruncated: truncated, NextContinuationToken: next, Contents: contents}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(out)
}

func (p *pagingS3) requestLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

func newPagingStore(t *testing.T, fake *pagingS3) *S3Store {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	awsCfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
	store, err := NewS3Store(S3StoreOptions{Client: NewClient(awsCfg, server.URL, true, 1), Bucket: "archive", Prefix: "agent-archive"})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func rangeTestKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("agent-archive/sessions/claude/k%02d", i)
	}
	sort.Strings(keys)
	return keys
}

func objectKeys(objects []Object) []string {
	keys := make([]string, len(objects))
	for i, object := range objects {
		keys[i] = object.Key
	}
	return keys
}

func TestS3StoreListRangeStartsAfterAndStopsPastThrough(t *testing.T) {
	fake := &pagingS3{keys: rangeTestKeys(10), pageSize: 3}
	store := newPagingStore(t, fake)
	got, err := store.ListRange(context.Background(), "sessions", "sessions/claude/k02", "sessions/claude/k06")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sessions/claude/k03", "sessions/claude/k04", "sessions/claude/k05", "sessions/claude/k06"}
	if !slices.Equal(objectKeys(got), want) {
		t.Fatalf("ListRange = %v, want %v", objectKeys(got), want)
	}
	// The first request starts after the full bucket key; the second carries
	// only the continuation token; that page (k06, k07, k08) passes
	// through, so no third request is made.
	requests := fake.requestLog()
	if len(requests) != 2 || requests[0] != "agent-archive/sessions/claude/k02|" || requests[1] != "|agent-archive/sessions/claude/k05" {
		t.Fatalf("requests = %v, want start-after on the first only and paging to stop after the second", requests)
	}
}

func TestS3StoreListRangeUnboundedIsTheWholeListing(t *testing.T) {
	fake := &pagingS3{keys: rangeTestKeys(7), pageSize: 3}
	store := newPagingStore(t, fake)
	got, err := store.ListRange(context.Background(), "sessions", "", "")
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.List(context.Background(), "sessions")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(objectKeys(got), objectKeys(all)) || len(got) != 7 {
		t.Fatalf("ListRange(\"\", \"\") = %v, List = %v", objectKeys(got), objectKeys(all))
	}
	if requests := fake.requestLog(); requests[0] != "|" {
		t.Fatalf("an unbounded range sent start-after: %v", requests)
	}
}

// A provider that ignored start-after would start every range from the
// beginning. The store enforces the range itself, so adjacent ranges still
// share no key and miss none.
func TestS3StoreListRangeEnforcesBoundsWhenStartAfterIsIgnored(t *testing.T) {
	fake := &pagingS3{keys: rangeTestKeys(12), pageSize: 4, ignoreStartAfter: true}
	store := newPagingStore(t, fake)
	var joined []string
	bounds := []string{"", "sessions/claude/k03", "sessions/claude/k08", ""}
	for i := 0; i+1 < len(bounds); i++ {
		got, err := store.ListRange(context.Background(), "sessions", bounds[i], bounds[i+1])
		if err != nil {
			t.Fatal(err)
		}
		joined = append(joined, objectKeys(got)...)
	}
	all, err := store.List(context.Background(), "sessions")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(joined, objectKeys(all)) {
		t.Fatalf("joined ranges = %v, want %v", joined, objectKeys(all))
	}
}

// A boundary the key rules refuse is not sent as StartAfter (it could name a
// key outside the store prefix), and the range is still enforced, so the
// listing succeeds rather than failing on every run until the cache is
// cleared.
func TestS3StoreListRangeWithAnUnusableStartStillEnforcesTheRange(t *testing.T) {
	fake := &pagingS3{keys: rangeTestKeys(6), pageSize: 4}
	store := newPagingStore(t, fake)
	got, err := store.ListRange(context.Background(), "sessions", "sessions/claude/k03/./x", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"sessions/claude/k04", "sessions/claude/k05"}; !slices.Equal(objectKeys(got), want) {
		t.Fatalf("ListRange = %v, want %v", objectKeys(got), want)
	}
	if requests := fake.requestLog(); requests[0] != "|" {
		t.Fatalf("an unusable start key was sent as start-after: %v", requests)
	}
}

func TestS3StoreListRangePropagatesErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer server.Close()
	awsCfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
	store, err := NewS3Store(S3StoreOptions{Client: NewClient(awsCfg, server.URL, true, 1), Bucket: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListRange(context.Background(), "sessions", "", ""); err == nil || !strings.Contains(err.Error(), strconv.Itoa(http.StatusForbidden)) {
		t.Fatalf("ListRange error = %v, want the 403", err)
	}
}

func TestS3StoreListRangePageBoundaryTransferCounts(t *testing.T) {
	t.Parallel()
	keys := make([]string, 3001)
	for i := range keys {
		keys[i] = fmt.Sprintf("agent-archive/sessions/custom/k%05d", i)
	}
	for _, tc := range []struct {
		name, after, through         string
		returned, pages, transferred int
	}{
		{"exact first page", "", "sessions/custom/k00999", 1000, 1, 1000},
		{"exact second page", "", "sessions/custom/k01999", 2000, 2, 2000},
		{"absent boundary", "", "sessions/custom/k00999!", 1000, 2, 2000},
		{"short range transfers full page", "", "sessions/custom/k00001", 2, 1, 1000},
		{"middle exact page", "sessions/custom/k00999", "sessions/custom/k01999", 1000, 1, 1000},
		{"final unbounded range", "sessions/custom/k01999", "", 1001, 2, 1001},
		{"empty final range", "sessions/custom/k03000", "", 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &pagingS3{keys: keys, pageSize: 1000}
			store := newPagingStore(t, fake)
			got, err := store.ListRange(context.Background(), "sessions/", tc.after, tc.through)
			if err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			transferred := fake.transferred
			fake.mu.Unlock()
			if len(got) != tc.returned || len(fake.requestLog()) != tc.pages || transferred != tc.transferred {
				t.Fatalf("returned %d pages %d transferred %d; want %d, %d, %d", len(got), len(fake.requestLog()), transferred, tc.returned, tc.pages, tc.transferred)
			}
		})
	}
}
