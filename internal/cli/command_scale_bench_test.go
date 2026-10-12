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

func benchmarkEnv(b testing.TB, home string, store storage.ObjectStore) Env {
	b.Helper()
	cfg := config.Config{MachineID: "bench", Storage: credentialsTestConfig(), Archive: archive.Config{SchemaVersion: 1, MachineID: "bench", Enabled: true}}
	if err := config.Save(home, cfg); err != nil {
		b.Fatal(err)
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
