package storagetest

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// A synthetic S3 endpoint verifies pages and transferred headers independently
// of the model, including exact and absent boundaries and StartAfter/token use.
func TestListingModelMatchesS3ProviderPages(t *testing.T) {
	t.Parallel()
	headers := make([]storage.Object, 2500)
	for i := range headers {
		headers[i] = storage.Object{Key: fmt.Sprintf("sessions/%04d", i), ETag: "etag", Size: 1}
	}
	for _, tc := range []struct{ name, after, through string }{
		{"exact-page", "", headers[999].Key},
		{"absent-boundary", "", headers[999].Key + "x"},
		{"short-range", "", headers[9].Key},
		{"final-range", headers[1999].Key, ""},
		{"empty-range", headers[2499].Key, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var pages, keys atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				start := sort.Search(len(headers), func(i int) bool { return headers[i].Key > q.Get("start-after") })
				if token := q.Get("continuation-token"); token != "" {
					if q.Get("start-after") != "" {
						t.Error("continuation repeated StartAfter")
					}
					var err error
					start, err = strconv.Atoi(token)
					if err != nil {
						t.Error(err)
						http.Error(w, "invalid token", 400)
						return
					}
				}
				prefix := q.Get("prefix")
				end := min(start+1000, len(headers))
				type item struct {
					Key  string
					ETag string
					Size int64
				}
				result := struct {
					XMLName               xml.Name `xml:"ListBucketResult"`
					IsTruncated           bool
					NextContinuationToken string `xml:",omitempty"`
					Contents              []item
				}{}
				for _, h := range headers[start:end] {
					if strings.HasPrefix(h.Key, prefix) {
						result.Contents = append(result.Contents, item{h.Key, h.ETag, h.Size})
					}
				}
				result.IsTruncated = end < len(headers)
				if result.IsTruncated {
					result.NextContinuationToken = strconv.Itoa(end)
				}
				pages.Add(1)
				keys.Add(int64(len(result.Contents)))
				w.Header().Set("Content-Type", "application/xml")
				if err := xml.NewEncoder(w).Encode(result); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("synthetic", "synthetic", "")}
			store, err := storage.NewS3Store(storage.S3StoreOptions{Client: storage.NewClient(cfg, server.URL, true, 1), Bucket: "archive"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.ListRange(t.Context(), "sessions/", tc.after, tc.through)
			if err != nil {
				t.Fatal(err)
			}
			want, cost := ModelListingRange(headers, tc.after, tc.through, ListingLatencyModel{StopAtBoundary: true})
			if !reflect.DeepEqual(got, want) || pages.Load() != cost.Pages || keys.Load() != cost.Keys {
				t.Fatalf("got=%d want=%d pages=%d/%d transferred=%d/%d", len(got), len(want), pages.Load(), cost.Pages, keys.Load(), cost.Keys)
			}
		})
	}
}
