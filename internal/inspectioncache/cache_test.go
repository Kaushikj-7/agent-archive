package inspectioncache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // Registers -update for go test ./... -update.
)

func cacheTestObservation() Observation {
	return Observation{Key: Key{Agent: "codex", SourceIdentity: "file\x00/synthetic/rollout.jsonl\x00thread", AdapterVersion: "1", FilterVersion: "18", InspectionVersion: 1, Proof: agentapi.SourceContentProof{Version: 1, Provider: "codex-ordinary", SHA256: [32]byte{1}, Boundary: 128}, RawBytes: 1024, RecordBytes: 512, FilteredBytes: 1024}, Summary: agentapi.ImportSummary{Conversation: true, NativeStartAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), RetainedBytes: 32, RetainedRecords: 1}}
}

func TestCacheMissingColdWarmAndCorrupt(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "inspection")
	cache := New(dir)
	observed := cacheTestObservation()
	if _, ok := cache.Lookup(ctx, observed.Key); ok {
		t.Fatal("missing cache hit")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("lookup created cache: %v", err)
	}
	if err := cache.StoreBatch(ctx, []Observation{observed}); err != nil {
		t.Fatal(err)
	}
	if got, ok := cache.Lookup(ctx, observed.Key); !ok || got != observed.Summary {
		t.Fatalf("warm summary = %+v, %v", got, ok)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("directory privacy: %v, %v", info, err)
	}
	path := cache.path(observed.Key)
	info, err = os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("entry privacy: %v, %v", info, err)
	}
	if err := os.WriteFile(path, []byte(`{"version":9000}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Lookup(ctx, observed.Key); ok {
		t.Fatal("corrupt cache hit")
	}
	if err := os.WriteFile(path, make([]byte, maxEntryBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Lookup(ctx, observed.Key); ok {
		t.Fatal("oversized cache hit")
	}
}

func TestCacheEveryContentVersionIdentityAndLimitChangesKey(t *testing.T) {
	ctx := context.Background()
	cache := New(filepath.Join(t.TempDir(), "inspection"))
	observed := cacheTestObservation()
	if err := cache.StoreBatch(ctx, []Observation{observed}); err != nil {
		t.Fatal(err)
	}
	changes := []func(*Key){
		func(k *Key) { k.Agent = "claude" },
		func(k *Key) { k.SourceIdentity += "changed" },
		func(k *Key) { k.AdapterVersion += "changed" },
		func(k *Key) { k.FilterVersion += "changed" },
		func(k *Key) { k.InspectionVersion++ },
		func(k *Key) { k.Proof.Version++ },
		func(k *Key) { k.Proof.Provider += "changed" },
		func(k *Key) { k.Proof.SHA256[0]++ },
		func(k *Key) { k.Proof.Boundary++ },
		func(k *Key) { k.RawBytes++ },
		func(k *Key) { k.RecordBytes++ },
		func(k *Key) { k.FilteredBytes++ },
		func(k *Key) { k.Records++ },
	}
	for i, change := range changes {
		k := observed.Key
		change(&k)
		if _, ok := cache.Lookup(ctx, k); ok {
			t.Fatalf("changed key %d reused summary", i)
		}
	}
}

func TestCacheRejectsInvalidAndCancelledObservations(t *testing.T) {
	cache := New(filepath.Join(t.TempDir(), "inspection"))
	observed := cacheTestObservation()
	observed.Summary.RetainedBytes = -1
	if err := cache.StoreBatch(context.Background(), []Observation{observed}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Lookup(context.Background(), observed.Key); ok {
		t.Fatal("invalid summary stored")
	}
	observed = cacheTestObservation()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cache.StoreBatch(ctx, []Observation{observed}); err == nil {
		t.Fatal("cancelled write succeeded")
	}
	if _, ok := cache.Lookup(context.Background(), observed.Key); ok {
		t.Fatal("cancelled summary stored")
	}
}

func TestCacheEvictionAndConcurrentWritersRespectCaps(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inspection")
	cache := New(dir)
	cache.maxEntries = 3
	cache.maxBytes = 2048
	observed := cacheTestObservation()
	var writers sync.WaitGroup
	for i := range 12 {
		writers.Go(func() {
			other := New(dir)
			other.maxEntries, other.maxBytes = cache.maxEntries, cache.maxBytes
			entry := observed
			entry.Key.Proof.SHA256[1] = byte(i)
			if err := other.StoreBatch(context.Background(), []Observation{entry}); err != nil && !errors.Is(err, local.ErrBusy) {
				t.Error(err)
			}
		})
	}
	writers.Wait()
	for i := range 5 {
		entry := observed
		entry.Key.Proof.SHA256[1] = byte(i + 20)
		if err := cache.StoreBatch(context.Background(), []Observation{entry}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	var bytes int64
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		count++
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		bytes += info.Size()
	}
	if count > cache.maxEntries || bytes > cache.maxBytes {
		t.Fatalf("caps exceeded: %d entries, %d bytes", count, bytes)
	}
}

func TestCommandWriterClosesAndBoundsCancelledQueue(t *testing.T) {
	cache := New(filepath.Join(t.TempDir(), "inspection"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writer := NewWriter(ctx, cache)
	for range QueueCapacity * 4 {
		writer.Observe(cacheTestObservation())
	}
	stats := writer.Close()
	if stats.Dropped != QueueCapacity*4 || stats.Queued != 0 || stats.Stored != 0 {
		t.Fatalf("cancelled queue: %+v", stats)
	}
	if got := writer.Close(); got != stats {
		t.Fatalf("repeated close: %+v", got)
	}
	if _, err := os.Stat(cache.dir); !os.IsNotExist(err) {
		t.Fatalf("cancelled queue created cache: %v", err)
	}
}

func TestValidJSONSummaryCorruptionIsAMiss(t *testing.T) {
	cache := New(filepath.Join(t.TempDir(), "inspection"))
	observed := cacheTestObservation()
	if err := cache.StoreBatch(t.Context(), []Observation{observed}); err != nil {
		t.Fatal(err)
	}
	path := cache.path(observed.Key)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entry record
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	entry.Summary.NativeStartAt = entry.Summary.NativeStartAt.Add(time.Hour)
	data, err = json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Lookup(t.Context(), observed.Key); ok {
		t.Fatal("valid JSON with altered facts accepted")
	}
}

func TestCacheHashesLocatorAndRejectsSymlinkDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inspection")
	cache := New(dir)
	observed := cacheTestObservation()
	observed.Key.SourceIdentity = "../../outside"
	if err := cache.StoreBatch(t.Context(), []Observation{observed}); err != nil {
		t.Fatal(err)
	}
	path := cache.path(observed.Key)
	if filepath.Dir(path) != dir || len(filepath.Base(path)) != 69 {
		t.Fatalf("locator escaped cache: %q", path)
	}
	alias := filepath.Join(t.TempDir(), "cache-alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	linked := New(alias)
	if _, ok := linked.Lookup(t.Context(), observed.Key); ok {
		t.Fatal("symlink cache directory accepted")
	}
	if err := linked.StoreBatch(t.Context(), []Observation{observed}); err == nil {
		t.Fatal("symlink directory written")
	}
}

func TestCacheProductionDiskCapsAndDirectoryTraversalBound(t *testing.T) {
	if MaxEntries != 10000 || MaxBytes != 16<<20 {
		t.Fatalf("production caps changed: %d/%d", MaxEntries, MaxBytes)
	}
	cache := New(filepath.Join(t.TempDir(), "inspection"))
	if err := os.Mkdir(cache.dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		path := filepath.Join(cache.dir, fmt.Sprintf("oversized-%d.json", i))
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Truncate(3 << 20); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := cache.StoreBatch(t.Context(), []Observation{cacheTestObservation()}); err != nil {
		t.Fatal(err)
	}
	entries, bytes, err := cache.inventory(t.Context())
	if err != nil || len(entries) > MaxEntries || bytes > MaxBytes {
		t.Fatalf("production disk cap: entries=%d bytes=%d error=%v", len(entries), bytes, err)
	}
	// Empty corrupt files cost no content allocations, yet metadata traversal is
	// still bounded and refuses persistence instead of growing an arbitrary tree.
	for i := range 2*MaxEntries + 2 {
		path := filepath.Join(cache.dir, fmt.Sprintf("invalid-%05d.json", i))
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := cache.StoreBatch(t.Context(), []Observation{cacheTestObservation()}); err == nil {
		t.Fatal("unbounded cache directory accepted")
	}
}
