package reader

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/trace"
)

func TestCatalogTraceSeparatesValidationFromPageDecoding(t *testing.T) {
	// Trace recording is process-wide; keep this observation sequential.
	ctx := context.Background()
	store := storagetest.NewMemoryStore()
	storagetest.SeedReadPathArchive(t, store, 3, 0)
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := OpenSessionCatalog(ctx, cache, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catalog.Close() }()
	headers, err := DiscoverCatalogHeaders(ctx, store, ListOptions{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if err = catalog.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	disable := trace.Enable()
	defer disable()
	if err = catalog.Refresh(ctx, headers); err != nil {
		t.Fatal(err)
	}
	page, err := catalog.Query(ctx, CatalogQuery{Metadata: MetadataQuery{Limit: 1}})
	if err != nil || len(page.Rows) != 1 {
		t.Fatal(page, err)
	}
	cache.keys("sessions/")
	var output bytes.Buffer
	trace.Write(&output)
	for _, pattern := range []string{`catalog refresh validation[^\n]*rows-scanned 3`, `catalog candidate decoding[^\n]*rows-scanned 1[^\n]*summaries-decoded 1`, `cache key inventory[^\n]*entries-visited [3-9]`} {
		if !regexp.MustCompile(pattern).Match(output.Bytes()) {
			t.Fatalf("missing work accounting %s:\n%s", pattern, output.String())
		}
	}
	if strings.Contains(output.String(), "native-00000001") || strings.Contains(output.String(), "synthetic project") {
		t.Fatal("trace contains fixture content")
	}
}

func TestCacheEvictionTraceEnclosesInventoryAndCountsOnlyDeletes(t *testing.T) {
	// The recorder is process-wide; this synthetic observation stays sequential.
	cache, err := OpenMetadataCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, ok := cache.keyDir("sessions/codex/synthetic/metadata.json")
	if !ok {
		t.Fatal("unsafe synthetic key")
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old.json", "current.json"} {
		if err = os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	disable := trace.Enable()
	defer disable()
	cache.readDir = func(path string) ([]os.DirEntry, error) {
		var during bytes.Buffer
		trace.Write(&during)
		if !regexp.MustCompile(`cache eviction[^\n]*unfinished`).Match(during.Bytes()) {
			t.Error("eviction span did not begin before inventory")
		}
		return entries, nil
	}
	cache.pruneVersions(dir, "current.json")
	cache.pruneVersions(dir, "current.json") // The retained inventory now names an absent old file.
	if _, err = os.Stat(filepath.Join(dir, "old.json")); !os.IsNotExist(err) {
		t.Fatal("stale validator survived", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "current.json")); err != nil {
		t.Fatal("current validator removed", err)
	}
	var after bytes.Buffer
	trace.Write(&after)
	if !strings.Contains(after.String(), "evictions 1") || strings.Contains(after.String(), "evictions 2") {
		t.Fatal("failed delete counted as eviction", after.String())
	}
}
