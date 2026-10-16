package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
)

// deniedReadS3 behaves like S3 under the documented least-privilege policy:
// GetObject and HeadObject on a missing key answer 403 AccessDenied (the
// caller may not learn which keys exist), and ListObjectsV2 works only when
// listAllowed. unreadable names present keys whose reads are denied anyway.
type deniedReadS3 struct {
	mu          sync.Mutex
	objects     map[string][]byte
	unreadable  map[string]bool
	listAllowed bool
	lists       []string
}

func (f *deniedReadS3) serve(t *testing.T) *httptest.Server {
	t.Helper()
	deny := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		if r.Method == http.MethodHead {
			return
		}
		if _, err := io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`); err != nil {
			t.Error(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/archive/")
		query := r.URL.Query()
		if r.Method == http.MethodGet && query.Has("list-type") {
			f.lists = append(f.lists, query.Get("prefix")+"|max-keys="+query.Get("max-keys"))
			if !f.listAllowed {
				deny(w, r)
				return
			}
			// Like S3: keys in ascending order, at most max-keys of them.
			prefix := query.Get("prefix")
			var keys []string
			for k := range f.objects {
				if strings.HasPrefix(k, prefix) {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			if limit, err := strconv.Atoi(query.Get("max-keys")); err == nil && limit < len(keys) {
				keys = keys[:limit]
			}
			listed := map[string][]byte{}
			for _, k := range keys {
				listed[k] = f.objects[k]
			}
			writeListResponse(w, listed, prefix)
			return
		}
		//lint:ignore LV1001 r.Method is an arbitrary request method; the cases are net/http's own constants
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			f.objects[key] = body
			delete(f.unreadable, key)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet, http.MethodHead:
			body, ok := f.objects[key]
			if !ok || f.unreadable[key] {
				deny(w, r)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(body)
			}
		case http.MethodDelete:
			delete(f.objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func newDeniedReadStore(t *testing.T, fake *deniedReadS3, prefix string) *S3Store {
	t.Helper()
	server := fake.serve(t)
	awsCfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("test-access", "test-secret", "")}
	store, err := NewS3Store(S3StoreOptions{Client: NewClient(awsCfg, server.URL, true, 1), Bucket: "archive", Prefix: prefix, MaxGetBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// A 403 on a read is a missing object only when a listing of the key itself
// confirms it is absent. Every other 403 stays the error it is.
func TestS3DeniedReadIsNotFoundOnlyWhenAListingConfirmsAbsence(t *testing.T) {
	const key = "sessions/claude/abc/metadata.json"
	cases := []struct {
		name        string
		objects     map[string]string
		unreadable  []string
		listAllowed bool
		wantMissing bool
	}{
		{name: "missing key, listing allowed", listAllowed: true, wantMissing: true},
		{name: "missing key, only a longer key shares its prefix", objects: map[string]string{key + ".bak": "x"}, listAllowed: true, wantMissing: true},
		{name: "missing key, listing denied too", listAllowed: false, wantMissing: false},
		{name: "present key whose read is denied", objects: map[string]string{key: "{}"}, unreadable: []string{key}, listAllowed: true, wantMissing: false},
		// The one-result listing still finds the key itself: it sorts
		// before every longer key it prefixes.
		{name: "present unreadable key beside longer keys", objects: map[string]string{key: "{}", key + ".bak": "x", key + "~": "x"}, unreadable: []string{key}, listAllowed: true, wantMissing: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &deniedReadS3{objects: map[string][]byte{}, unreadable: map[string]bool{}, listAllowed: c.listAllowed}
			for k, v := range c.objects {
				fake.objects["agent-archive/"+k] = []byte(v)
			}
			for _, k := range c.unreadable {
				fake.unreadable["agent-archive/"+k] = true
			}
			store := newDeniedReadStore(t, fake, "agent-archive/")
			_, getErr := store.Get(context.Background(), key)
			_, statErr := store.Stat(context.Background(), key)
			for op, err := range map[string]error{"Get": getErr, "Stat": statErr} {
				if err == nil {
					t.Fatalf("%s succeeded against a denied read", op)
				}
				if got := errors.Is(err, ErrNotFound); got != c.wantMissing {
					t.Errorf("%s error = %v; not found = %v, want %v", op, err, got, c.wantMissing)
				}
			}
			// The confirming listing asks about the full key (with the
			// store's prefix) and for one result only.
			for _, list := range fake.lists {
				if list != "agent-archive/"+key+"|max-keys=1" {
					t.Errorf("confirming listing = %q, want the full key with max-keys=1", list)
				}
			}
			if len(fake.lists) != 2 {
				t.Errorf("listings = %d, want one per denied read", len(fake.lists))
			}
		})
	}
}

// A session's first publication checks for an existing source before
// uploading it. Under a policy that answers 403 for a missing key, that
// check must read as "not there yet", or no session could ever publish.
func TestS3FirstPublicationWorksWhenMissingKeysReadAsDenied(t *testing.T) {
	fake := &deniedReadS3{objects: map[string][]byte{}, unreadable: map[string]bool{}, listAllowed: true}
	store := newDeniedReadStore(t, fake, "agent-archive")
	source, metadata := []byte("source bytes"), []byte(`{"metadata":true}`)
	if err := PutSourceThenMetadataIndexed(context.Background(), store, "sessions/claude/abc/source.1.jsonl.gz", "sessions/claude/abc/metadata.json", source, metadata, RetryPolicy{MaxAttempts: 1}, nil); err != nil {
		t.Fatalf("first publication under a 403-for-missing policy: %v", err)
	}
	if got := string(fake.objects["agent-archive/sessions/claude/abc/metadata.json"]); got != string(metadata) {
		t.Fatalf("metadata = %q, want it published", got)
	}
}

// Only a 403 triggers the confirming listing: other failures keep their
// meaning without an extra request.
func TestS3ConfirmedAbsentIgnoresErrorsOtherThanForbidden(t *testing.T) {
	fake := &deniedReadS3{objects: map[string][]byte{}, unreadable: map[string]bool{}, listAllowed: true}
	store := newDeniedReadStore(t, fake, "")
	for _, err := range []error{
		errors.New("dial tcp: connection refused"),
		responseError(http.StatusInternalServerError, errors.New("internal")),
		responseError(http.StatusNotFound, errors.New("missing")),
	} {
		if store.confirmedAbsent(context.Background(), "sessions/x", err) {
			t.Errorf("confirmedAbsent(%v) = true, want false", err)
		}
	}
	if len(fake.lists) != 0 {
		t.Errorf("listings = %v, want none for errors other than 403", fake.lists)
	}
}

// Setup's round trip passes under a policy that answers 403 for a missing
// key, because its final read-after-delete is confirmed by a listing.
func TestVerifyAccessPassesWhenMissingKeysReadAsDenied(t *testing.T) {
	fake := &deniedReadS3{objects: map[string][]byte{}, unreadable: map[string]bool{}, listAllowed: true}
	store := newDeniedReadStore(t, fake, "agent-archive")
	if err := VerifyAccess(context.Background(), store); err != nil {
		t.Fatalf("VerifyAccess under a 403-for-missing policy: %v", err)
	}
}

func TestS3SourceSetPublicationWorksWhenMissingKeysReadAsDenied(t *testing.T) {
	fake := &deniedReadS3{objects: map[string][]byte{}, unreadable: map[string]bool{}, listAllowed: true}
	store := newDeniedReadStore(t, fake, "agent-archive")
	source, metadata := []byte("source bytes"), []byte(`{"metadata":true}`)
	if err := PutSourceSetThenMetadata(context.Background(), store, []SourcePublication{{Key: "sessions/claude/abc/source.1.jsonl.gz", SHA256: SHA256Hex(source), Size: len(source), Bytes: source}}, "sessions/claude/abc/metadata.json", metadata, MetadataPredecessor{Known: true}, RetryPolicy{MaxAttempts: 1}); err != nil {
		t.Fatalf("first publication under a 403-for-missing policy: %v", err)
	}
	if got := string(fake.objects["agent-archive/sessions/claude/abc/metadata.json"]); got != string(metadata) {
		t.Fatalf("metadata = %q, want it published", got)
	}
}
