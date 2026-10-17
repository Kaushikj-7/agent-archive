package reader

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/trace"
)

// CacheMaintenance records progress through disposable cache directories.
// Cursor is a validated encoded key; LastSweep records the last completed pass.
type CacheMaintenance struct {
	Cursor    string    `json:"cursor"`
	LastSweep time.Time `json:"last_sweep"`
}

const maintenanceFile = ".maintenance"

const cacheMaintenanceDirectoryBudget = 64

func (c *MetadataCache) directoryEntries(dir string) ([]os.DirEntry, error) {
	span := trace.Start("cache directory inventory")
	defer span.End()
	var entries []os.DirEntry
	var err error
	if c.readDir != nil {
		entries, err = c.readDir(dir)
	} else {
		entries, err = os.ReadDir(dir)
	}
	span.Count("entries-visited", len(entries))
	return entries, err
}

// maintain visits at most 64 key directories once per opened command cache.
// Root-name inventory is still O(N); it does not stat or open live child
// directories. Concurrent processes can repeat work, but atomic checkpoints
// never leave partial state, and cleanup failures only cause later misses.
func (c *MetadataCache) maintain(ctx context.Context) {
	if c == nil {
		return
	}
	c.maintenanceOnce.Do(func() { c.sweep(ctx) })
}

func (c *MetadataCache) sweep(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	state := c.maintenanceState()
	entries, err := c.directoryEntries(c.dir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && cacheKey(entry.Name()) != "" {
			names = append(names, entry.Name())
		} else {
			removeStaleCacheTemp(c.dir, entry)
		}
	}
	start := sort.SearchStrings(names, state.Cursor)
	if start < len(names) && names[start] == state.Cursor {
		start++
	}
	if start == len(names) {
		start = 0
	}
	end := min(start+cacheMaintenanceDirectoryBudget, len(names))
	for _, name := range names[start:end] {
		if ctx.Err() != nil {
			break
		}
		c.maintainDirectory(filepath.Join(c.dir, name))
		state.Cursor = name
	}
	if ctx.Err() == nil && end == len(names) {
		state.Cursor = ""
		state.LastSweep = time.Now()
	}
	data, err := json.Marshal(state)
	if err == nil {
		_ = writeCacheFile(filepath.Join(c.dir, maintenanceFile), data)
	}
}

func (c *MetadataCache) maintenanceState() CacheMaintenance {
	path := filepath.Join(c.dir, maintenanceFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return CacheMaintenance{}
	}
	data, err := os.ReadFile(path)
	var state CacheMaintenance
	if err != nil || json.Unmarshal(data, &state) != nil || (state.Cursor != "" && cacheKey(state.Cursor) == "") {
		return CacheMaintenance{}
	}
	return state
}

func (c *MetadataCache) maintainDirectory(dir string) {
	span := trace.Start("cache eviction")
	defer span.End()
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return
	}
	entries, err := c.directoryEntries(dir)
	if err != nil {
		return
	}
	// Retain the most recently written validator. If concurrent writers make
	// this choice stale, the next fresh-header read simply downloads again.
	latest := ""
	var modified time.Time
	for _, entry := range entries {
		removeStaleCacheTemp(dir, entry)
		if !cacheValidatorFile(entry) {
			continue
		}
		info, err := entry.Info()
		if err == nil && (latest == "" || info.ModTime().After(modified)) {
			latest, modified = entry.Name(), info.ModTime()
		}
	}
	for _, entry := range entries {
		if cacheValidatorFile(entry) && entry.Name() != latest {
			if os.Remove(filepath.Join(dir, entry.Name())) == nil {
				span.Count("evictions", 1)
			}
		}
	}
}

func cacheValidatorFile(entry os.DirEntry) bool {
	return entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json")
}

func removeStaleCacheTemp(dir string, entry os.DirEntry) {
	if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), ".pending-") {
		return
	}
	span := trace.Start("cache eviction")
	defer span.End()
	info, err := entry.Info()
	if err == nil && info.ModTime().Before(time.Now().Add(-staleCacheTempAge)) {
		if os.Remove(filepath.Join(dir, entry.Name())) == nil {
			span.Count("evictions", 1)
		}
	}
}

// pruneVersions only visits a key when new validated bytes have been written.
func (c *MetadataCache) pruneVersions(dir, current string) {
	span := trace.Start("cache eviction")
	defer span.End()
	entries, err := c.directoryEntries(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.Name() != current && cacheValidatorFile(entry) {
			if os.Remove(filepath.Join(dir, entry.Name())) == nil {
				span.Count("evictions", 1)
			}
		}
	}
}
