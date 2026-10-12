package reader

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

// A listing killed between writing a cache file's temporary copy and
// renaming it left the temporary file for good. Bounded maintenance now
// removes stale ones, and leaves one that may belong to a write in progress.
func TestCacheMaintenanceRemovesStaleTemporaryFiles(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "cache", "metadata")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyDir := filepath.Join(dir, hex.EncodeToString([]byte("sessions/codex/temp/metadata.json")))
	if err := os.Mkdir(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale, fresh := filepath.Join(keyDir, ".pending-stale"), filepath.Join(keyDir, ".pending-fresh")
	for _, path := range []string{stale, fresh} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleCacheTempAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	cache, err := OpenMetadataCache(home)
	if err != nil {
		t.Fatal(err)
	}
	cache.maintain(context.Background())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temporary file left behind: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a temporary file that may be in use was removed: %v", err)
	}
}
