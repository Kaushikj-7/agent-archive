package listingindex_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// This store accepts every Put so rejected keys must be caught before writes,
// rather than relying on a provider failure after an attempted allocation.
type destinationKeyCountingStore struct {
	*storagetest.MemoryStore
	prefix string
	puts   int
	key    string
	bytes  int
}

func (s *destinationKeyCountingStore) ObjectKey(key string) string {
	return s.prefix + key
}

func (s *destinationKeyCountingStore) Put(ctx context.Context, key string, data []byte) error {
	s.puts++
	s.key = key
	s.bytes = len(data)
	return s.MemoryStore.Put(ctx, key, data)
}

func destinationPrefixBytes(n int) string {
	prefix := strings.Repeat("p/", n/2)
	if n%2 != 0 {
		prefix = "q" + prefix
	}
	return prefix
}

func TestRevisionDestinationKeyLimitsBeforePut(t *testing.T) {
	key, raw := revisionMetadata(t)
	var metadata archive.Metadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.NativeChild = true
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := listingindex.NewRevision(key, raw, "opaque-validator")
	if err != nil {
		t.Fatal(err)
	}
	if !revision.NativeChild || len(strings.Split(strings.TrimPrefix(revision.Key, listingindex.V3Prefix), "/")) < 5 {
		t.Fatal("fixture must preserve native-child and segmented current encoding")
	}
	for _, tc := range []struct {
		name       string
		prefix     string
		wantReject bool
	}{
		{"valid-prefix", "archive/", false},
		{"total-1024", destinationPrefixBytes(1024 - len(revision.Key)), false},
		{"total-1025", destinationPrefixBytes(1025 - len(revision.Key)), true},
		{"component-255", strings.Repeat("p", 255) + "/", false},
		{"component-256", strings.Repeat("p", 256) + "/", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &destinationKeyCountingStore{MemoryStore: storagetest.NewMemoryStore(), prefix: tc.prefix}
			err := listingindex.PutRevision(t.Context(), store, revision)
			if tc.wantReject {
				if err == nil || store.puts != 0 {
					t.Fatalf("unsafe full key reached Put: error=%v attempts=%d", err, store.puts)
				}
				return
			}
			if err != nil || store.puts != 1 || store.key != revision.Key || store.bytes != 0 {
				t.Fatalf("valid key changed: error=%v attempts=%d key-equal=%v bytes=%d", err, store.puts, store.key == revision.Key, store.bytes)
			}
		})
	}
}
