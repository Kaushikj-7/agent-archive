package listingindex_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/testutil/providertest"
)

func TestDisposableProviderPortableListingKeepsCanonicalCoverage(t *testing.T) {
	store := providertest.NewDisposableS3(t)
	key, raw := revisionMetadata(t)
	if err := store.Put(t.Context(), key, raw); err != nil {
		t.Fatal(err)
	}
	if err := listingindex.PublishRevision(t.Context(), store, key, raw); err != nil {
		t.Fatal(err)
	}
	hints, err := store.List(t.Context(), listingindex.V3Prefix)
	if err != nil || len(hints) != 1 {
		t.Fatal("missing portable hint", len(hints), err)
	}
	if len(strings.Split(strings.TrimPrefix(hints[0].Key, listingindex.V3Prefix), "/")) < 5 {
		t.Fatal("fixture did not exercise segmented summary")
	}
	for _, component := range strings.Split(store.ObjectKey(hints[0].Key), "/") {
		if len(component) > 255 {
			t.Fatal("oversized provider component")
		}
	}
	parsed, err := listingindex.ParseRevision(hints[0].Key)
	if err != nil || parsed.ValidateMetadata(raw) != nil {
		t.Fatal("portable summary differs from canonical bytes", err)
	}
	peer := store.IndependentOwner(t)
	scan := false
	got, err := reader.ListRecent(t.Context(), peer, "sessions", reader.Filter{}, 1, reader.ListOptions{CompatibilityScan: func(string) { scan = true }})
	if err != nil || scan || !got.Complete || got.TotalMatched != 1 || len(got.Sessions) != 1 {
		t.Fatal("indexed coverage failed", got, err)
	}
	// A malformed auxiliary hint must force complete canonical fallback, rather
	// than discard the real session or assert unsupported indexed coverage.
	if err = store.Put(t.Context(), hints[0].Key+"/", nil); err != nil {
		t.Fatal(err)
	}
	scan = false
	got, err = reader.ListRecent(t.Context(), peer, "sessions", reader.Filter{}, 1, reader.ListOptions{CompatibilityScan: func(string) { scan = true }})
	if err != nil || !scan || !got.Complete || got.TotalMatched != 1 || len(got.Sessions) != 1 {
		t.Fatal("canonical fallback failed", got, err)
	}
	var metadata map[string]any
	if err = json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	if got.Sessions[0].SessionID != metadata["session_id"] {
		t.Fatal("canonical identity changed")
	}
}
