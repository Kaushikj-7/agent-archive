package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// BenchmarkCommandScale measures Run: config/startup, cache, discovery, selected
// metadata and rendering. It validates captured JSON after timing; benchmark reports contain no data.
func BenchmarkCommandScale(b *testing.B) {
	for _, c := range storagetest.ScaleCases() {
		b.Run(c.Name(), func(b *testing.B) { benchmarkListCase(b, c) })
	}
}

func BenchmarkCommandLatency(b *testing.B) {
	for _, state := range []string{"cold", "warm", "changed"} {
		c := storagetest.BenchCase{Sessions: 80, Limit: 50, CacheState: state, Delay: time.Millisecond}
		b.Run(c.Name(), func(b *testing.B) { benchmarkListCase(b, c) })
	}
}

func benchmarkEnv(tb testing.TB, home string, store storage.ObjectStore) Env {
	tb.Helper()
	cfg := config.Config{MachineID: "bench", Storage: credentialsTestConfig(), Archive: archive.Config{SchemaVersion: 1, MachineID: "bench", Enabled: true}}
	if err := config.Save(home, cfg); err != nil {
		tb.Fatal(err)
	}
	// Read-only commands receive private homes and no host environment, repo,
	// scheduler or credentials. TestMain additionally fails closed for host calls.
	return Env{Home: func() (string, error) { return home, nil }, UserHomeDir: func() (string, error) { return home, nil }, AccountHome: func() (string, error) { return home, nil }, TempDir: func() string { return home }, Now: func() time.Time { return storagetest.BenchmarkTime.Add(24 * time.Hour) }, LookupEnv: noEnv, OS: platform.Darwin, OpenStore: func(config.Config) (storage.ObjectStore, error) { return store, nil }, WorkingDir: func() (string, error) { return "", errors.New("no benchmark working directory") }, Interrupts: noInterrupts, repoKey: func(string) string { return "" }, IsTerminal: func(any) bool { return false }}
}

func benchmarkListCase(b *testing.B, c storagetest.BenchCase) {
	b.Helper()
	b.StopTimer()
	mem := storagetest.NewMemoryStore()
	storagetest.SeedArchive(b, mem, c.Sessions, 0)
	store := storagetest.NewMeasuredStore(mem, c.Delay)
	home := b.TempDir()
	env := benchmarkEnv(b, home, store)
	args := []string{"list", "--all-projects", "--json", "--limit", strconv.Itoa(c.Limit)}
	warmEnv := env
	warmEnv.OpenStore = func(config.Config) (storage.ObjectStore, error) { return mem, nil }
	b.ReportAllocs()
	for range b.N {
		if c.CacheState == "changed" {
			mem = storagetest.NewMemoryStore()
			storagetest.SeedArchive(b, mem, c.Sessions, 0)
			store.MemoryStore = mem
		}
		if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
			b.Fatal(err)
		}
		if c.CacheState != "cold" {
			if code := Run(args, nil, io.Discard, io.Discard, warmEnv); code != 0 {
				b.Fatal("warmup failed")
			}
		}
		if c.CacheState == "changed" {
			storagetest.SeedArchive(b, mem, c.Sessions, 1)
		}
		var output bytes.Buffer
		b.StartTimer()
		code := Run(args, nil, &output, io.Discard, env)
		b.StopTimer()
		if err := validateBenchmarkList(code, output.Bytes(), c); err != nil {
			b.Fatal(err)
		}
	}
	storagetest.ReportReadMetrics(b, store.Metrics())
}

// validateBenchmarkList checks the same invocation whose work was measured.
// Decode and validation run after StopTimer, without another cache-warming read.
func validateBenchmarkList(code int, output []byte, c storagetest.BenchCase) error {
	if code != 0 {
		return fmt.Errorf("synthetic list exit %d", code)
	}
	var doc listDocument
	if err := json.Unmarshal(output, &doc); err != nil {
		return fmt.Errorf("synthetic list JSON: %w", err)
	}
	want := c.Sessions
	if c.Limit > 0 {
		want = min(want, c.Limit)
	}
	if doc.Version != listSchemaVersion || doc.Limit != c.Limit || doc.Returned != want || len(doc.Sessions) != want || !doc.TotalMatchedKnown || doc.TotalMatched == nil || *doc.TotalMatched != c.Sessions || doc.Truncated != (want < c.Sessions) {
		return errors.New("incomplete synthetic command listing")
	}
	generation := 0
	if c.CacheState == "changed" {
		generation = 1
	}
	for i, m := range doc.Sessions {
		identity := c.Sessions - i
		id := fmt.Sprintf("%08x%024x", identity, identity)
		if m.SessionID != id || m.NativeSessionID != id || m.Harness.Name != "codex" || m.Title != fmt.Sprintf("synthetic-%d", generation) || !m.CapturedAt.Equal(storagetest.BenchmarkTime.Add(time.Duration(identity-1)*time.Second)) {
			return errors.New("incorrect synthetic command listing order or revision")
		}
	}
	return nil
}

// BenchmarkUsableTerminalScreen measures the first complete interactive stats
// frame. TerminalSize is requested after data loading/computation and before
// rendering. The writer marks the complete draw containing the clear-screen sequence, so a
// spinner or partial frame cannot count as usable. This is an injected 80x24
// terminal; it excludes process exec and a real terminal emulator's paint time.
func BenchmarkUsableTerminalScreen(b *testing.B) {
	b.StopTimer()
	mem := storagetest.NewMemoryStore()
	storagetest.SeedArchive(b, mem, 800, 0)
	store := storagetest.NewMeasuredStore(mem, 0)
	home := b.TempDir()
	env := benchmarkEnv(b, home, store)
	var elapsed time.Duration
	b.ReportAllocs()
	for range b.N {
		if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
			b.Fatal(err)
		}
		in := strings.NewReader("")
		out := &benchmarkScreen{}
		env.IsTerminal = func(stream any) bool { return stream == any(in) || stream == any(out) }
		env.TerminalSize = func(io.Writer) (int, int, bool) { return 80, 24, true }
		env.openKeys = func(io.Reader) (keyTerminal, bool) { return newFakeKeys("q"), true }
		b.StartTimer()
		out.start = time.Now()
		code := Run([]string{"stats", "--prices", goldenPrices}, in, out, io.Discard, env)
		b.StopTimer()
		if code != 0 || out.first == 0 {
			b.Fatal("no usable synthetic stats frame")
		}
		elapsed += out.first
	}
	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N), "usable-screen-ns/op")
	storagetest.ReportReadMetrics(b, store.Metrics())
}

type benchmarkScreen struct {
	start time.Time
	first time.Duration
}

func (s *benchmarkScreen) Write(p []byte) (int, error) {
	if s.first == 0 && bytes.Contains(p, []byte(clearScreenSequence)) {
		s.first = time.Since(s.start)
	}

	return len(p), nil
}

func (*benchmarkScreen) colorTerminal() bool { return false }

// BenchmarkCommandReads pins common read-only command paths beyond list. These
// small cold cases expose request duplication without timing a real provider.
func BenchmarkCommandReads(b *testing.B) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"show-large-parent", []string{"show", "00000001000000000000000000000001"}},
		{"show-exact", []string{"show", "00000001000000000000000000000001"}},
		{"show-short-ID", []string{"show", "00000001"}},
		{"stats-narrow", []string{"stats", "--days", "1", "--prices", goldenPrices}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.StopTimer()
			mem := storagetest.NewMemoryStore()
			storagetest.SeedArchive(b, mem, 800, 0)
			if tc.name == "show-large-parent" {
				seedBenchmarkParent(b, mem, 50)
			}
			if tc.name == "stats-narrow" {
				seedBenchmarkStats(b, mem, 800)
			}
			store := storagetest.NewMeasuredStore(mem, 0)
			home := b.TempDir()
			env := benchmarkEnv(b, home, store)
			b.ReportAllocs()
			for range b.N {
				if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				code := Run(tc.args, nil, io.Discard, io.Discard, env)
				b.StopTimer()
				if code != 0 {
					b.Fatalf("synthetic command exit %d", code)
				}
			}
			storagetest.ReportReadMetrics(b, store.Metrics())
		})
	}
}

func seedBenchmarkParent(b *testing.B, mem *storagetest.MemoryStore, children int) {
	b.Helper()
	ctx := context.Background()
	parentID := fmt.Sprintf("%08x%024x", 1, 1)
	parentKey, _ := archive.MetadataObjectKey("codex", parentID)
	data, err := mem.Get(ctx, parentKey)
	if err != nil {
		b.Fatal(err)
	}
	var parent archive.Metadata
	if err = json.Unmarshal(data, &parent); err != nil {
		b.Fatal(err)
	}
	for i := range children {
		id := fmt.Sprintf("%08x%024x", i+2, i+2)
		key, _ := archive.MetadataObjectKey("codex", id)
		data, err = mem.Get(ctx, key)
		if err != nil {
			b.Fatal(err)
		}
		var child archive.Metadata
		if err = json.Unmarshal(data, &child); err != nil {
			b.Fatal(err)
		}
		child.ParentSessionID = parentID
		child.ProjectID = parent.ProjectID
		data, err = json.Marshal(child)
		if err != nil {
			b.Fatal(err)
		}
		if err = mem.Put(ctx, key, data); err != nil {
			b.Fatal(err)
		}
		parent.LinkedSessions = append(parent.LinkedSessions, archive.LinkedSessionReference{SessionID: id, Relationship: "subagent", Status: archive.LinkedSessionPublished, ObservedAt: storagetest.BenchmarkTime})
	}
	data, err = json.Marshal(parent)
	if err != nil {
		b.Fatal(err)
	}
	if err = mem.Put(ctx, parentKey, data); err != nil {
		b.Fatal(err)
	}
}

// seedBenchmarkStats spans 90 days: a one-day query includes about 1/90 of
// sessions. Model and token fields exercise aggregation as well as selection.
func seedBenchmarkStats(b *testing.B, mem *storagetest.MemoryStore, count int) {
	b.Helper()
	ctx := context.Background()
	for i := range count {
		id := fmt.Sprintf("%08x%024x", i+1, i+1)
		key, _ := archive.MetadataObjectKey("codex", id)
		data, err := mem.Get(ctx, key)
		if err != nil {
			b.Fatal(err)
		}
		var m archive.Metadata
		if err = json.Unmarshal(data, &m); err != nil {
			b.Fatal(err)
		}
		m.CapturedAt = storagetest.BenchmarkTime.Add(24*time.Hour - time.Duration(i%90)*24*time.Hour)
		m.StartedAt = m.CapturedAt
		input, output := 1000+i, 200+i%100
		m.Counts = archive.Counts{InputTokens: &input, OutputTokens: &output}
		m.ModelTokens = []archive.ModelTokens{{Model: "gpt-5", InputTokens: &input, OutputTokens: &output}}

		data, err = json.Marshal(m)
		if err != nil {
			b.Fatal(err)
		}
		if err = mem.Put(ctx, key, data); err != nil {
			b.Fatal(err)
		}
		_, etag, err := mem.GetVersioned(ctx, key)
		if err != nil {
			b.Fatal(err)
		}
		revision, err := listingindex.NewRevision(key, data, etag)
		if err != nil {
			b.Fatal(err)
		}
		if err = listingindex.PutRevision(ctx, mem, revision); err != nil {
			b.Fatal(err)
		}
	}
}

// This is an actual command check, not another timed benchmark operation.
// A successful exhaustive command can skip damaged sidecars: that partial
// output must never be accepted as a cheaper complete benchmark result.
func TestBenchmarkCommandOutputRequiresCompleteSyntheticListing(t *testing.T) {
	for _, state := range []string{"cold", "warm", "changed"} {
		for _, limit := range []int{1, 50, 0} {
			t.Run(fmt.Sprintf("%s/%d", state, limit), func(t *testing.T) {
				c := storagetest.BenchCase{Sessions: 3, Limit: limit, CacheState: state}
				mem := storagetest.NewMemoryStore()
				storagetest.SeedArchive(t, mem, c.Sessions, 0)
				env := benchmarkEnv(t, t.TempDir(), mem)
				args := []string{"list", "--all-projects", "--json", "--limit", strconv.Itoa(limit)}
				if state != "cold" {
					if code := Run(args, nil, io.Discard, io.Discard, env); code != 0 {
						t.Fatalf("warmup exit %d", code)
					}
				}
				if state == "changed" {
					storagetest.SeedArchive(t, mem, c.Sessions, 1)
				}
				var output bytes.Buffer
				code := Run(args, nil, &output, io.Discard, env)
				if err := validateBenchmarkList(code, output.Bytes(), c); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	t.Run("successful-partial-command", func(t *testing.T) {
		mem := storagetest.NewMemoryStore()
		storagetest.SeedArchive(t, mem, 3, 0)
		key, err := archive.MetadataObjectKey("codex", "00000003000000000000000000000003")
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Put(t.Context(), key, []byte("{invalid")); err != nil {
			t.Fatal(err)
		}
		env := benchmarkEnv(t, t.TempDir(), mem)
		var output, diagnostics bytes.Buffer
		code := Run([]string{"list", "--all-projects", "--json", "--limit", "0"}, nil, &output, &diagnostics, env)
		var doc listDocument
		if err := json.Unmarshal(output.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if code != 0 || len(doc.Sessions) != 2 || diagnostics.Len() == 0 {
			t.Fatalf("partial command control: exit=%d returned=%d diagnostic-bytes=%d", code, len(doc.Sessions), diagnostics.Len())
		}
		c := storagetest.BenchCase{Sessions: 3, Limit: 0, CacheState: "cold"}
		if err := validateBenchmarkList(code, output.Bytes(), c); err == nil {
			t.Fatal("benchmark accepted successful partial listing")
		}
	})
}

func TestBenchmarkListRejectsIncorrectSuccessfulDocuments(t *testing.T) {
	mem := storagetest.NewMemoryStore()
	storagetest.SeedArchive(t, mem, 3, 0)
	env := benchmarkEnv(t, t.TempDir(), mem)
	var output bytes.Buffer
	code := Run([]string{"list", "--all-projects", "--json", "--limit", "0"}, nil, &output, io.Discard, env)
	c := storagetest.BenchCase{Sessions: 3, Limit: 0, CacheState: "cold"}
	if err := validateBenchmarkList(code, output.Bytes(), c); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*listDocument)
	}{
		{"omitted", func(doc *listDocument) { doc.Sessions = doc.Sessions[:2] }},
		{"returned-count", func(doc *listDocument) { doc.Returned = 2 }},
		{"unknown-total", func(doc *listDocument) { doc.TotalMatchedKnown = false }},
		{"wrong-total", func(doc *listDocument) {
			total := 2
			doc.TotalMatched = &total
		}},
		{"reordered", func(doc *listDocument) { doc.Sessions[0], doc.Sessions[1] = doc.Sessions[1], doc.Sessions[0] }},
		{"wrong-identity", func(doc *listDocument) { doc.Sessions[0].NativeSessionID = "other" }},
		{"stale-revision", func(doc *listDocument) { doc.Sessions[0].Title = "synthetic-1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc listDocument
			if err := json.Unmarshal(output.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&doc)
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateBenchmarkList(0, data, c); err == nil {
				t.Fatal("benchmark accepted incorrect successful document")
			}
		})
	}
}

type readPathCommand string

const (
	readPathListPicker    readPathCommand = "list-picker"
	readPathShowPicker    readPathCommand = "show-picker"
	readPathHandoffPicker readPathCommand = "handoff-picker"
	readPathShowShort     readPathCommand = "show-short"
	readPathHandoffQuery  readPathCommand = "handoff-query"
	readPathListPiped     readPathCommand = "list-piped"
)

type readPathCacheState string

const (
	readPathCold    readPathCacheState = "cold"
	readPathWarm    readPathCacheState = "warm"
	readPathChanged readPathCacheState = "changed"
	readPathNoCache readPathCacheState = "no-cache"
)

func (command readPathCommand) isPicker() bool {
	return command == readPathListPicker || command == readPathShowPicker || command == readPathHandoffPicker
}

// BenchmarkCommandReadPaths extends the command harness with the observed-size
// read paths. Timing includes local startup, inventory, refresh and rendering;
// remote provider-page metrics remain separate from those local costs.
func BenchmarkCommandReadPaths(b *testing.B) {
	for _, size := range storagetest.ReadPathSizes() {
		for _, state := range []readPathCacheState{readPathCold, readPathWarm, readPathChanged, readPathNoCache} {
			for _, command := range []readPathCommand{readPathListPicker, readPathShowPicker, readPathHandoffPicker, readPathShowShort, readPathHandoffQuery, readPathListPiped} {
				if state == readPathNoCache && command != readPathListPiped && command != readPathListPicker {
					continue
				}
				b.Run(fmt.Sprintf("sessions=%d/%s/%s", size, state, command), func(b *testing.B) {
					b.StopTimer()
					mem := storagetest.NewMemoryStore()
					storagetest.SeedReadPathArchive(b, mem, size, 0)
					baseline, err := storagetest.SnapshotMemoryStore(context.Background(), mem)
					if err != nil {
						b.Fatal(err)
					}
					home := b.TempDir()
					store := storagetest.NewMeasuredStore(mem, 0)
					store.ListingModel = &storagetest.ListingLatencyModel{}
					if os.Getenv("AGENT_ARCHIVE_READ_LATENCY") == "1" {
						// One sensitivity decomposition inside the observed full-page/GET ranges;
						// separate calibration of overhead and per-key costs remains outstanding.
						store.ListingModel.ListPageDelay = 200 * time.Millisecond
						store.ListingModel.ListKeyDelay = 400 * time.Microsecond
						store.GetDelay = 160 * time.Millisecond
						b.Log("synthetic latency: LIST page overhead200ms +400us/transferred key; GET160ms; assumed decomposition, not provider calibration")
					}
					env := benchmarkEnv(b, home, store)
					opens := 0
					env.OpenStore = func(config.Config) (storage.ObjectStore, error) { opens++; return store, nil }
					args := readPathBenchmarkArgs(command, state == readPathNoCache)
					b.ReportAllocs()
					for range b.N {
						mem, err = baseline.Clone(context.Background())
						if err != nil {
							b.Fatal(err)
						}
						store.MemoryStore = mem
						if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
							b.Fatal(err)
						}
						if state == readPathWarm || state == readPathChanged {
							// Restore baseline before warming, so every measured changed invocation
							// hydrates the same one percent rather than only the first iteration.
							warm := env
							warm.OpenStore = func(config.Config) (storage.ObjectStore, error) { return mem, nil }
							if code, _, diagnostic := runReadPathBenchmark(args, command, warm); code != 0 {
								b.Fatalf("warmup exit=%d: %s", code, diagnostic)
							}
							if state == readPathChanged {
								storagetest.SeedReadPathArchive(b, mem, size, 1)
							}
						}
						headers, err := mem.List(context.Background(), "")
						if err != nil {
							b.Fatal(err)
						}
						store.ListingModel.Headers = headers
						b.StartTimer()
						code, out, diagnostic := runReadPathBenchmark(args, command, env)
						b.StopTimer()
						if err := validateReadPathOutput(command, size, code, out); err != nil {
							b.Fatalf("%v: %s", err, diagnostic)
						}
					}

					// Observe local counts in a separate command/home after timing. Trace
					// span allocation and tree rendering must not distort ns/op or allocs/op.
					observed, err := baseline.Clone(context.Background())
					if err != nil {
						b.Fatal(err)
					}
					observedEnv := benchmarkEnv(b, b.TempDir(), observed)
					if state == readPathWarm || state == readPathChanged {
						if code, _, diagnostic := runReadPathBenchmark(args, command, observedEnv); code != 0 {
							b.Fatalf("observation warmup: %d %s", code, diagnostic)
						}
						if state == readPathChanged {
							storagetest.SeedReadPathArchive(b, observed, size, 1)
						}
					}
					observedEnv.LookupEnv = func(key string) (string, bool) {
						if key == envTrace {
							return "1", true
						}
						return "", false
					}
					code, output, diagnostic := runReadPathBenchmark(args, command, observedEnv)
					if err := validateReadPathOutput(command, size, code, output); err != nil {
						b.Fatal(err)
					}
					for name, count := range readPathLocalCounts(diagnostic) {
						b.ReportMetric(float64(count), name+"/op")
					}

					b.ReportMetric(float64(opens)/float64(b.N), "store-opens/op")
					storagetest.ReportReadMetrics(b, store.Metrics())
				})
			}
		}
	}
}

func readPathBenchmarkArgs(command readPathCommand, noCache bool) []string {
	var args []string
	switch command {
	case readPathListPicker:
		args = []string{"list", "--all-projects"}
	case readPathShowPicker:
		args = []string{"show"}
	case readPathHandoffPicker:
		args = []string{"handoff", "--all-projects", "--source", "archive"}
	case readPathShowShort:
		args = []string{"show", "00000001", "--json"}
	case readPathHandoffQuery:
		args = []string{"handoff", "00000001", "--source", "archive", "--format", "json"}
	case readPathListPiped:
		args = []string{"list", "--all-projects", "--limit", "50", "--json"}
	default:
		panic("unknown read path benchmark command")
	}
	if noCache {
		args = append(args, "--no-cache")
	}
	return args
}

func runReadPathBenchmark(args []string, command readPathCommand, env Env) (int, []byte, string) {
	var out, diagnostic bytes.Buffer
	var in io.Reader
	if command.isPicker() {
		input := strings.NewReader("q\n")
		in = input
		env.IsTerminal = func(stream any) bool { return stream == any(input) || stream == any(&out) }
		env.TerminalSize = func(io.Writer) (int, int, bool) { return 80, 24, true }
		env.openKeys = func(io.Reader) (keyTerminal, bool) { return newFakeKeys("q"), true }
	}
	code := Run(args, in, &out, &diagnostic, env)
	return code, out.Bytes(), diagnostic.String()
}

var readPathTraceCounter = regexp.MustCompile(`(rows-scanned|summary-B-hashed|summaries-decoded|entries-visited|key-dirs-evicted|evictions|registration reads|from cache|downloaded) ([0-9]+)`)

func readPathLocalCounts(diagnostic string) map[string]int64 {
	counts := map[string]int64{}
	for line := range strings.SplitSeq(diagnostic, "\n") {
		var prefix string
		switch {
		case strings.Contains(line, "read sidecars"):
			prefix = "body-"
		case strings.Contains(line, "catalog refresh validation"):
			prefix = "refresh-"
		case strings.Contains(line, "catalog remote validation"):
			prefix = "remote-validation-"
		case strings.Contains(line, "catalog candidate decoding"):
			prefix = "query-"
		case strings.Contains(line, "cache directory inventory"), strings.Contains(line, "cache key inventory"):
			prefix = "cache-"
		case strings.Contains(line, "cache eviction"), strings.Contains(line, "cache key eviction"):
			prefix = "cache-"
		case strings.Contains(line, "load registrations"):
			prefix = "local-"
		default:
			continue
		}
		for _, match := range readPathTraceCounter.FindAllStringSubmatch(line, -1) {
			count, _ := strconv.ParseInt(match[2], 10, 64)
			counts[prefix+strings.ReplaceAll(match[1], " ", "-")] += count
		}
	}
	return counts
}

// The observed-size gate runs in ordinary CI. Timings are advisory; stable
// request ceilings detect duplicated discovery/body reads without wall clocks.
func TestCommandReadPathOperationBudgets(t *testing.T) {
	t.Parallel()
	const size = 1800
	for _, command := range []readPathCommand{readPathListPicker, readPathShowPicker, readPathHandoffPicker, readPathShowShort, readPathHandoffQuery, readPathListPiped} {
		t.Run(string(command), func(t *testing.T) {
			t.Parallel()
			mem := storagetest.NewMemoryStore()
			storagetest.SeedReadPathArchive(t, mem, size, 0)
			home := t.TempDir()
			store := storagetest.NewMeasuredStore(mem, 0)
			store.ListingModel = &storagetest.ListingLatencyModel{}
			env := benchmarkEnv(t, home, store)
			args := readPathBenchmarkArgs(command, false)
			if code, _, diag := runReadPathBenchmark(args, command, env); code != 0 {
				t.Fatalf("warmup: %d %s", code, diag)
			}
			for _, changed := range []bool{false, true} {
				if changed {
					storagetest.SeedReadPathArchive(t, mem, size, 1)
				}
				headers, err := mem.List(t.Context(), "")
				if err != nil {
					t.Fatal(err)
				}
				store.ListingModel.Headers = headers
				store.Reset()
				code, out, diag := runReadPathBenchmark(args, command, env)
				if err := validateReadPathOutput(command, size, code, out); err != nil {
					t.Fatalf("changed=%v: %v %s", changed, err, diag)
				}
				m := store.Metrics()
				maxGets := int64(2)
				if changed {
					maxGets += size / 100
				}
				if m.Gets > maxGets || m.Lists > 20 || m.ListPages > 40 || m.ListKeys > 20000 || m.ListBytes > 8<<20 {
					t.Fatalf("changed=%v exceeded operation budget: %+v", changed, m)
				}
				if m.Lists == 0 || m.ListPages == 0 {
					t.Fatal("legacy discovery missing", m)
				}
			}
		})
	}
}

func validateReadPathOutput(command readPathCommand, size, code int, out []byte) error {
	if code != 0 {
		return fmt.Errorf("command exit %d", code)
	}
	switch command {
	case readPathListPiped:
		var doc listDocument
		if err := json.Unmarshal(out, &doc); err != nil {
			return err
		}
		if len(doc.Sessions) != 50 || doc.TotalMatched == nil || *doc.TotalMatched != size {
			return errors.New("incomplete list")
		}
		for i, m := range doc.Sessions {
			identity := size - i
			if m.SessionID != fmt.Sprintf("%08x%024x", identity, identity) || m.NativeSessionID != fmt.Sprintf("native-%08x", identity) {
				return errors.New("incorrect list order or identity")
			}
		}
	case readPathShowShort, readPathHandoffQuery:
		if !bytes.Contains(out, []byte("00000001000000000000000000000001")) || !bytes.Contains(out, []byte("native-00000001")) {
			return errors.New("incorrect selected identity")
		}
	case readPathListPicker, readPathShowPicker, readPathHandoffPicker:
		if !bytes.Contains(out, []byte("synthetic")) {
			return errors.New("picker omitted synthetic sessions")
		}
	default:
		return fmt.Errorf("unknown read path benchmark command %q", command)
	}
	return nil
}

func TestReadPathOutputRejectsIncorrectSuccessfulCommands(t *testing.T) {
	t.Parallel()
	for _, command := range []readPathCommand{readPathListPicker, readPathShowPicker, readPathHandoffPicker, readPathShowShort, readPathHandoffQuery, readPathListPiped} {
		if err := validateReadPathOutput(command, 1800, 0, []byte(`{"sessions":[]}`)); err == nil {
			t.Fatal("accepted incorrect successful output", command)
		}
	}
}

func TestReadPathLocalCountsSeparateRefreshQueryAndInventory(t *testing.T) {
	t.Parallel()
	diagnostic := "catalog refresh validation 1ms (rows-scanned 1800, summary-B-hashed 400000)\n" +
		"catalog candidate decoding 1ms (rows-scanned 50, summary-B-hashed 9000, summaries-decoded 50)\n" +
		"cache key inventory 1ms (entries-visited 1800)\n" +
		"cache directory inventory 1ms (entries-visited 64)\n" +
		"cache key eviction 1ms (key-dirs-evicted 1)\n" +
		"read sidecars 1ms (from cache 1800, downloaded 18)\n"
	got := readPathLocalCounts(diagnostic)
	if got["refresh-rows-scanned"] != 1800 || got["query-rows-scanned"] != 50 || got["query-summaries-decoded"] != 50 || got["cache-entries-visited"] != 1864 || got["cache-key-dirs-evicted"] != 1 || got["body-from-cache"] != 1800 || got["body-downloaded"] != 18 {
		t.Fatal(got)
	}
}
