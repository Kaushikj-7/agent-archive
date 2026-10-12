package storage

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscredentials "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/wangjohn/agent-archive/internal/destination"
)

// Fake HTTP verifies SDK wiring only. It is not provider qualification.
func TestConditionalSDKHeadersAndConflicts(t *testing.T) {
	for _, c := range []PutCondition{{CreateOnly: true}, {MatchETag: "prior"}} {
		t.Run(c.MatchETag, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if c.CreateOnly && r.Header.Get("If-None-Match") != "*" {
					t.Error("missing create condition")
				}
				if !c.CreateOnly && r.Header.Get("If-Match") != `"prior"` {
					t.Error("missing match condition")
				}
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusPreconditionFailed)
				if _, err := io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>"); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			cfg := aws.Config{Region: "us-east-1", Credentials: awscredentials.NewStaticCredentialsProvider("fixture", "fixture", "")}
			s, err := NewS3Store(S3StoreOptions{Client: NewClient(cfg, server.URL, true, 3), Bucket: "fixture", Provider: "s3"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.PutConditional(t.Context(), "catalog-v4/head.json", []byte("{}"), c); !errors.Is(err, ErrPreconditionFailed) {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("conditional write retried", calls)
			}
			if !errors.Is(s.CatalogAtomicQualification(), ErrAtomicCatalogUnqualified) {
				t.Fatal("fake qualified provider")
			}
		})
	}
}

func TestCatalogConfiguredStoreRefusesBeforeCredentials(t *testing.T) {
	for _, provider := range []string{"s3", "r2", "unknown"} {
		_, err := NewConfiguredStore(t.Context(), destination.Config{Provider: provider, ArchiveFormat: destination.FormatCatalogV4}, nil)
		if !errors.Is(err, ErrAtomicCatalogUnqualified) {
			t.Fatal(err)
		}
	}
}
