package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// historicalListingModel replays deterministic headers, avoiding MemoryStore's
// repeated whole-map sort for each range. GETs still use the real reader/cache.
// Pages model the historical R2 assumption, including range end-page overread.
// No transport sleeps or current provider measurements are involved.
type historicalListingModel struct {
	*storagetest.MeasuredStore
	headers      []storage.Object
	mu           sync.Mutex
	pages        int
	keys         int
	headerBytes  int
	calls        int
	returnedKeys int
	costs        map[string]float64
}

func (s *historicalListingModel) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	return s.ListRange(ctx, prefix, "", "")
}

func (s *historicalListingModel) ListRange(ctx context.Context, prefix, after, through string) ([]storage.Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	start := sort.Search(len(s.headers), func(i int) bool { return s.headers[i].Key >= prefix && s.headers[i].Key > after })
	end := start
	for end < len(s.headers) && strings.HasPrefix(s.headers[end].Key, prefix) {
		end++
	}
	selectedEnd := end
	if through != "" {
		selectedEnd = start + sort.Search(end-start, func(i int) bool { return s.headers[start+i].Key > through })
	}
	scanned := selectedEnd - start
	if selectedEnd < end {
		scanned++
	} // provider must encounter the first key beyond through
	pages := max(1, (scanned+999)/1000)
	scanned = min(pages*1000, end-start) // full final page is transferred
	bytes := 0
	for _, object := range s.headers[start : start+scanned] {
		bytes += len(object.Key) + len(object.ETag) + 16
	}
	s.mu.Lock()
	s.calls++
	s.returnedKeys += selectedEnd - start
	s.pages += pages
	s.keys += scanned
	s.headerBytes += bytes
	s.costs[prefix+"\x00"+after] += float64(pages)*100 + float64(scanned)*0.45
	s.mu.Unlock()
	return append([]storage.Object(nil), s.headers[start:selectedEnd]...), nil
}

// modelLatency schedules range services deterministically in key order into
// 16 ideal slots, then adds GET waves (8 exhaustive, serial selected on this
// baseline). It excludes local work, network setup, retry and provider limits.
func (s *historicalListingModel) modelLatency(indexed bool, iterations int) float64 {
	keys := make([]string, 0, len(s.costs))
	for key := range s.costs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	slots := make([]float64, rangeConcurrency)
	if indexed {
		slots = slots[:1]
	}
	for _, key := range keys {
		slot := 0
		for i := range slots {
			if slots[i] < slots[slot] {
				slot = i
			}
		}
		slots[slot] += s.costs[key] / float64(iterations)
	}
	duration := 0.0
	for _, slot := range slots {
		duration = max(duration, slot)
	}
	gets := s.Metrics().Gets / int64(iterations)
	workers := int64(listConcurrency)
	if indexed {
		workers = 1
	}
	return duration + float64((gets+workers-1)/workers)*100
}

// seedHistoricalListing preserves 85% child sessions and exactly 1.5 old
// source snapshots/session, in addition to the current source and sidecar.
func seedHistoricalListing(tb testing.TB, n int) (*storagetest.MemoryStore, [][]byte) {
	tb.Helper()
	mem := storagetest.NewMemoryStore()
	bodies := make([][]byte, n)
	ctx := context.Background()
	for i := range n {
		id := fmt.Sprintf("%032x", i+1)
		source := fmt.Sprintf("sessions/codex/%s/source.%064x.jsonl.gz", id, 1)
		parent := ""
		if i%20 >= 3 {
			parent = fmt.Sprintf("%032x", i-i%20+1)
		}
		m := archive.Metadata{SchemaVersion: archive.MetadataSchemaVersion, SessionID: id, NativeSessionID: id, MachineID: "model", ProjectID: "model", Harness: archive.Harness{Name: "codex"}, CapturedAt: storagetest.BenchmarkTime.Add(time.Duration(i) * time.Second), ParentSessionID: parent, SourceBundle: archive.SourceReference{Key: source, SHA256: fmt.Sprintf("%064x", 1), CompressedBytes: 1}}
		data, err := json.Marshal(m)
		if err != nil {
			tb.Fatal(err)
		}
		bodies[i] = data
		key, err := archive.MetadataObjectKey("codex", id)
		if err != nil {
			tb.Fatal(err)
		}
		if err = mem.Put(ctx, key, data); err != nil {
			tb.Fatal(err)
		}
		for generation := 1; generation <= 2+i%2; generation++ {
			key := fmt.Sprintf("sessions/codex/%s/source.%064x.jsonl.gz", id, generation)
			if err = mem.Put(ctx, key, []byte{0}); err != nil {
				tb.Fatal(err)
			}
		}
		_, etag, err := mem.GetVersioned(ctx, key)
		if err != nil {
			tb.Fatal(err)
		}
		revision, err := listingindex.NewRevision(key, data, etag)
		if err != nil {
			tb.Fatal(err)
		}
		if err = listingindex.PutRevision(ctx, mem, revision); err != nil {
			tb.Fatal(err)
		}
	}
	return mem, bodies
}

// BenchmarkListingHistoricalModel measures the phase-3 matrix at this head.
// Cache/fixture preparation is excluded; local-ns includes decoding, cache I/O,
// header copies and reader work. The decode-only subcase isolates JSON decode.
func BenchmarkListingHistoricalModel(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		if n > 1000 && os.Getenv("AGENT_ARCHIVE_PERF") != "1" {
			continue
		}
		b.Run(fmt.Sprintf("sessions=%d", n), func(b *testing.B) {
			mem, bodies := seedHistoricalListing(b, n)
			headers, err := mem.List(context.Background(), "")
			if err != nil {
				b.Fatal(err)
			}
			b.Run("decode-only", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					for _, data := range bodies {
						var m archive.Metadata
						if err := json.Unmarshal(data, &m); err != nil {
							b.Fatal(err)
						}
					}
				}
				b.ReportMetric(float64(n), "bodies/op")
			})
			for _, mode := range []string{"sequential", "ranged", "indexed"} {
				for _, state := range []string{"cold", "warm"} {
					b.Run(mode+"/"+state, func(b *testing.B) {
						b.StopTimer()
						b.ReportAllocs()
						model := &historicalListingModel{MeasuredStore: storagetest.NewMeasuredStore(mem, 0), headers: headers, costs: map[string]float64{}}
						var opens time.Duration
						for range b.N {
							home := b.TempDir()
							if state == "warm" {
								cache, err := OpenMetadataCache(home)
								if err != nil {
									b.Fatal(err)
								}
								if mode == "indexed" {
									_, err = ListRecent(context.Background(), mem, "sessions", Filter{}, 50, ListOptions{Cache: cache, TopLevelOnly: true})
								} else {
									_, err = ListMetadataWithOptions(context.Background(), mem, "sessions", Filter{}, ListOptions{Cache: cache})
								}
								if err != nil {
									b.Fatal(err)
								}
							}
							b.StartTimer()
							start := time.Now()
							cache, err := OpenMetadataCache(home)
							opens += time.Since(start)
							if err != nil {
								b.Fatal(err)
							}
							opts := ListOptions{Cache: cache, TopLevelOnly: mode == "indexed"}
							if mode == "indexed" {
								result, err := ListRecent(context.Background(), model, "sessions", Filter{}, 50, opts)
								if err != nil || len(result.Sessions) != 50 || !result.Complete {
									b.Fatalf("indexed count/completeness: %d %v", len(result.Sessions), err)
								}
							} else {
								var store storage.ObjectStore = model
								if mode == "sequential" {
									store = struct{ storage.ObjectStore }{model}
								} // deliberately hide RangeLister
								result, err := ListMetadataWithOptions(context.Background(), store, "sessions", Filter{}, opts)
								if err != nil || len(result) != n {
									b.Fatalf("full count: %d %v", len(result), err)
								}
							}
							b.StopTimer()
						}
						metrics := model.Metrics()
						b.ReportMetric(float64(metrics.Gets)/float64(b.N), "GET/op")
						b.ReportMetric(float64(metrics.Bytes)/float64(b.N), "GET-B/op")
						b.ReportMetric(float64(metrics.PeakReads), "peak-GETs")
						b.ReportMetric(float64(model.calls)/float64(b.N), "LIST-calls/op")
						b.ReportMetric(float64(model.returnedKeys)/float64(b.N), "returned-headers/op")
						b.ReportMetric(float64(model.pages)/float64(b.N), "modeled-LIST-pages/op")
						b.ReportMetric(float64(model.keys)/float64(b.N), "modeled-header-keys/op")
						b.ReportMetric(float64(model.headerBytes)/float64(b.N), "modeled-header-B/op")
						b.ReportMetric(model.modelLatency(mode == "indexed", b.N), "modeled-ms/op")
						b.ReportMetric(float64(opens.Nanoseconds())/float64(b.N), "cacheOpen-ns/op")
					})
				}
			}
		})
	}
}

func TestHistoricalListingModelPagesAndFixture(t *testing.T) {
	mem, bodies := seedHistoricalListing(t, 20)
	children := 0
	for _, data := range bodies {
		var m archive.Metadata
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		if m.ParentSessionID != "" {
			children++
		}
	}
	if children != 17 {
		t.Fatalf("children=%d", children)
	}
	headers, err := mem.List(context.Background(), "sessions/")
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 70 {
		t.Fatalf("session headers=%d", len(headers))
	}
	model := &historicalListingModel{MeasuredStore: storagetest.NewMeasuredStore(mem, 0), headers: headers, costs: map[string]float64{}}
	result, err := model.ListRange(context.Background(), "sessions/", "", headers[9].Key)
	if err != nil || len(result) != 10 || model.pages != 1 || model.keys != 70 {
		t.Fatalf("range=%d pages=%d keys=%d err=%v", len(result), model.pages, model.keys, err)
	}
	result, err = model.List(context.Background(), "missing/")
	if err != nil || len(result) != 0 || model.pages != 2 || model.keys != 70 {
		t.Fatalf("empty=%d pages=%d keys=%d err=%v", len(result), model.pages, model.keys, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = model.List(ctx, "sessions/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

// BenchmarkReadPathListingPolicies retains the historical benchmark above and
// compares current page policies over all three prefixes in one shared budget.
// Page/key assumptions are reported as sensitivity cases, not calibrated facts.
func BenchmarkReadPathListingPolicies(b *testing.B) {
	for _, n := range storagetest.ReadPathSizes() {
		mem := storagetest.NewMemoryStore()
		storagetest.SeedReadPathArchive(b, mem, n, 0)
		for _, ranges := range []int{1, 8, 16, 0} {
			name := fmt.Sprintf("sessions=%d/ranges=%d", n, ranges)
			if ranges == 0 {
				name = fmt.Sprintf("sessions=%d/page-sized", n)
			}
			b.Run(name, func(b *testing.B) {
				groups := make([][]storage.Object, 3)
				for i, prefix := range []string{"sessions/", listingindex.V2Prefix, listingindex.V3Prefix} {
					var err error
					groups[i], err = mem.List(context.Background(), prefix)
					if err != nil {
						b.Fatal(err)
					}
				}
				model := storagetest.ListingLatencyModel{StopAtBoundary: true}
				b.ReportAllocs()
				b.ResetTimer()
				var costs []storagetest.ListingPageCost
				for range b.N {
					costs = readPathPolicyCosts(groups, ranges, model)
				}
				b.StopTimer()
				var pages, keys, bytes, discarded int64
				waves := make([]int64, 0, len(costs))
				for _, cost := range costs {
					pages += cost.Pages
					keys += cost.Keys
					bytes += cost.Bytes
					discarded += cost.DiscardedBytes
					waves = append(waves, cost.Pages)
				}
				b.ReportMetric(float64(pages), "provider-pages/op")
				b.ReportMetric(float64(keys), "transferred-keys/op")
				b.ReportMetric(float64(bytes), "transferred-header-B/op")
				b.ReportMetric(float64(discarded), "discarded-header-B/op")
				b.ReportMetric(float64(storagetest.ListingSchedule(waves, 16)), "critical-page-waves/op")
				// Both decompositions give a600ms full page, yet differ on short pages.
				for _, overhead := range []time.Duration{100 * time.Millisecond, 400 * time.Millisecond} {
					services := make([]int64, 0, len(costs))
					perKey := (600*time.Millisecond - overhead) / 1000
					for _, cost := range costs {
						services = append(services, int64(time.Duration(cost.Pages)*overhead+time.Duration(cost.Keys)*perKey))
					}
					b.ReportMetric(float64(storagetest.ListingSchedule(services, 16))/float64(time.Millisecond), fmt.Sprintf("modeled-%dms-overhead-ms/op", overhead.Milliseconds()))
				}
			})
		}
	}
}

func readPathPolicyCosts(groups [][]storage.Object, ranges int, model storagetest.ListingLatencyModel) []storagetest.ListingPageCost {
	var costs []storagetest.ListingPageCost
	for _, headers := range groups {
		count := ranges
		if count == 0 {
			count = max(1, (len(headers)+999)/1000)
		}
		if len(headers) <= 1000 {
			count = 1
		}
		after := ""
		for i := range count {
			through := ""
			if i+1 < count {
				if ranges == 0 {
					through = headers[(i+1)*1000-1].Key
				} else {
					through = headers[(i+1)*len(headers)/count-1].Key
				}
			}
			_, cost := storagetest.ModelListingRange(headers, after, through, model)
			costs = append(costs, cost)
			after = through
		}
	}
	return costs
}

func TestReadPathListingPolicySharedBudget(t *testing.T) {
	t.Parallel()
	groups := make([][]storage.Object, 3)
	for group, n := range []int{5040, 0, 1800} {
		for i := range n {
			groups[group] = append(groups[group], storage.Object{Key: fmt.Sprintf("prefix-%d/%05d", group, i)})
		}
	}
	for _, tc := range []struct {
		ranges int
		pages  int64
		keys   int64
		waves  int64
	}{{0, 9, 6840, 1}, {16, 33, 26942, 3}} {
		costs := readPathPolicyCosts(groups, tc.ranges, storagetest.ListingLatencyModel{StopAtBoundary: true})
		var pages, keys int64
		var counts []int64
		for _, cost := range costs {
			pages += cost.Pages
			keys += cost.Keys
			counts = append(counts, cost.Pages)
		}
		waves := storagetest.ListingSchedule(counts, 16)
		if pages != tc.pages || keys != tc.keys || waves != tc.waves {
			t.Fatalf("ranges=%d pages=%d keys=%d waves=%d", tc.ranges, pages, keys, waves)
		}
	}
}
