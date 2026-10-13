package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/stats"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// BenchmarkMatchedCLI is a bounded, frozen comparison harness. It measures Run,
// including captured output, but excludes setup, warmup and validation. The
// large fixture is explicitly opt-in; it never selects the exhaustive matrix.
func BenchmarkMatchedCLI(b *testing.B) {
	sizes := []int{800}
	if os.Getenv("AGENT_ARCHIVE_MATCHED_LARGE") == "1" {
		sizes = append(sizes, 10000)
	}
	for _, size := range sizes {
		for _, state := range []string{"cold", "warm"} {
			for _, kind := range []string{"list", "list-text"} {
				limit := 50
				b.Run(fmt.Sprintf("sessions=%d/%s%d/%s", size, kind, limit, state), func(b *testing.B) {
					matchedBenchmark(b, size, state, kind, limit)
				})
			}
		}
		for _, kind := range []string{"show-exact", "show-short", "show-parent", "stats1", "stats7", "stats30", "stats90", "interactive"} {
			b.Run(fmt.Sprintf("sessions=%d/%s/cold", size, kind), func(b *testing.B) {
				matchedBenchmark(b, size, "cold", kind, 0)
			})
		}
	}
}

func matchedBenchmark(b *testing.B, size int, state, kind string, limit int) {
	b.StopTimer()
	mem, fixture := matchedFixture(b, size, strings.HasPrefix(kind, "stats") || kind == "interactive", kind == "show-parent")
	home := b.TempDir()
	store := &matchedStore{MeasuredStore: storagetest.NewMeasuredStore(mem, 0)}
	env := matchedEnv(b, home, store)
	args := matchedArgs(kind, limit)
	expected := matchedExpected(b, fixture, kind)
	b.Logf("fixture-sha256=%x objects-sha256=%x expected-sha256=%x", sha256.Sum256(matchedJSON(b, fixture)), matchedObjectHash(b, mem), sha256.Sum256(expected))
	var first time.Duration
	b.ReportAllocs()
	for range b.N {
		if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
			b.Fatal(err)
		}
		if state == "warm" {
			warmEnv := env
			warmEnv.OpenStore = func(config.Config) (storage.ObjectStore, error) { return mem, nil }
			var output, diagnostic bytes.Buffer
			code := Run(args, nil, &output, &diagnostic, warmEnv)
			if err := matchedValidate(code, output.Bytes(), diagnostic.Bytes(), fixture, kind, limit, expected, env); err != nil {
				b.Fatalf("warmup: %v", err)
			}
		}
		output := &matchedScreen{}
		var diagnostic bytes.Buffer
		var in io.Reader
		if kind == "interactive" {
			in = strings.NewReader("")
			env.IsTerminal = func(stream any) bool { return stream == any(in) || stream == any(output) }
			env.TerminalSize = func(io.Writer) (int, int, bool) { return 80, 24, true }
			env.openKeys = func(io.Reader) (keyTerminal, bool) { return newFakeKeys("w", "w", "q"), true }
		}
		b.StartTimer()
		output.start = time.Now()
		code := Run(args, in, output, &diagnostic, env)
		b.StopTimer()
		if err := matchedValidate(code, output.Bytes(), diagnostic.Bytes(), fixture, kind, limit, expected, env); err != nil {
			b.Fatal(err)
		}
		b.Logf("output-sha256=%x", sha256.Sum256(output.Bytes()))
		first += output.first
	}
	if kind == "interactive" {
		b.ReportMetric(float64(first.Nanoseconds())/float64(b.N), "usable-screen-ns/op")
	}
	storagetest.ReportReadMetrics(b, store.Metrics())
}

func matchedEnv(tb testing.TB, home string, store storage.ObjectStore) Env {
	tb.Helper()
	cfg := config.Config{MachineID: "bench", Storage: credentialsTestConfig(), Archive: archive.Config{SchemaVersion: 1, MachineID: "bench", Enabled: true}}
	if err := config.Save(home, cfg); err != nil {
		tb.Fatal(err)
	}
	return Env{Home: func() (string, error) { return home, nil }, UserHomeDir: func() (string, error) { return home, nil }, AccountHome: func() (string, error) { return home, nil }, TempDir: func() string { return home }, Now: func() time.Time { return storagetest.BenchmarkTime.Add(24 * time.Hour) }, LookupEnv: noEnv, OS: platform.Darwin, OpenStore: func(config.Config) (storage.ObjectStore, error) { return store, nil }, WorkingDir: func() (string, error) { return "", errors.New("no benchmark working directory") }, Interrupts: noInterrupts, repoKey: func(string) string { return "" }, IsTerminal: func(any) bool { return false }}
}

func matchedArgs(kind string, limit int) []string {
	switch kind {
	case "list-text":
		return []string{"list", "--all-projects"}
	case "list":
		args := []string{"list", "--all-projects", "--json"}
		if limit != 50 {
			args = append(args, "--limit", strconv.Itoa(limit))
		}
		return args
	case "show-exact", "show-parent":
		return []string{"show", matchedID(1), "--json"}
	case "show-short":
		return []string{"show", "00000001", "--json"}
	case "interactive":
		return []string{"stats", "--days", "7"}
	default:
		return []string{"stats", "--days", strings.TrimPrefix(kind, "stats"), "--json", "--all"}
	}
}

func matchedID(n int) string { return fmt.Sprintf("%08x%024x", n, n) }

// The complete fixture remains outside timing, and is the exhaustive oracle's
// input. Stats spans 400 days and 100 projects; sources are placeholders only.
func matchedFixture(tb testing.TB, count int, statsCase, parentCase bool) (*storagetest.MemoryStore, []archive.Metadata) {
	tb.Helper()
	mem := storagetest.NewMemoryStore()
	storagetest.SeedArchive(tb, mem, count, 0)
	fixture := make([]archive.Metadata, count)
	for i := range count {
		key, err := archive.MetadataObjectKey("codex", matchedID(i+1))
		if err != nil {
			tb.Fatal(err)
		}
		raw, err := mem.Get(context.Background(), key)
		if err != nil {
			tb.Fatal(err)
		}
		if err := json.Unmarshal(raw, &fixture[i]); err != nil {
			tb.Fatal(err)
		}
		m := &fixture[i]
		if statsCase {
			m.CapturedAt = storagetest.BenchmarkTime.Add(24*time.Hour - time.Duration(i%400)*24*time.Hour)
			m.StartedAt = m.CapturedAt
			input, output := 1000+i, 200+i%100
			m.Counts = archive.Counts{InputTokens: &input, OutputTokens: &output}
			m.ModelTokens = []archive.ModelTokens{{Model: "gpt-5", InputTokens: &input, OutputTokens: &output}}
		}
		if parentCase && i > 0 && i <= 50 {
			m.ParentSessionID = matchedID(1)
			m.ProjectID = fixture[0].ProjectID
		}
		if parentCase && i == 0 {
			for child := 2; child <= min(count, 51); child++ {
				m.LinkedSessions = append(m.LinkedSessions, archive.LinkedSessionReference{SessionID: matchedID(child), Relationship: "subagent", Status: archive.LinkedSessionPublished, ObservedAt: storagetest.BenchmarkTime})
			}
		}
		if statsCase || parentCase {
			raw = matchedJSON(tb, m)
			if err := mem.Put(context.Background(), key, raw); err != nil {
				tb.Fatal(err)
			}
			_, etag, err := mem.GetVersioned(context.Background(), key)
			if err != nil {
				tb.Fatal(err)
			}
			revision, err := listingindex.NewRevision(key, raw, etag)
			if err != nil {
				tb.Fatal(err)
			}
			if err := listingindex.PutRevision(context.Background(), mem, revision); err != nil {
				tb.Fatal(err)
			}
		}
	}
	return mem, fixture
}

func matchedJSON(tb testing.TB, value any) []byte {
	tb.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		tb.Fatal(err)
	}
	return raw
}

func matchedExpected(tb testing.TB, fixture []archive.Metadata, kind string) []byte {
	tb.Helper()
	if kind == "list-text" {
		sessions := append([]archive.Metadata(nil), fixture...)
		sortByActivity(sessions)
		view := listViews{sessions: sessions, full: true, limit: 50}.view
		format := listFormatOptions{Now: storagetest.BenchmarkTime.Add(24 * time.Hour), GroupByProject: true}
		choices := listChoices(sessionScope{}, format, false, sessions, 50, view, "")
		var out bytes.Buffer
		if err := printListTable(&out, choices.shown()); err != nil {
			tb.Fatal(err)
		}
		return out.Bytes()
	}
	if strings.HasPrefix(kind, "show") {
		view := sessionView{Metadata: fixture[0]}
		for _, link := range fixture[0].LinkedSessions {
			view.LinkedAvailability = append(view.LinkedAvailability, reader.LinkedAvailability{SessionID: link.SessionID, State: reader.LinkedStateMetadataAvailable})
		}
		return matchedJSON(tb, view)
	}
	if strings.HasPrefix(kind, "stats") {
		days, err := strconv.Atoi(strings.TrimPrefix(kind, "stats"))
		if err != nil {
			tb.Fatal(err)
		}
		now := storagetest.BenchmarkTime.Add(24 * time.Hour)
		return matchedJSON(tb, statsDocument{Version: statsSchemaVersion, GeneratedAt: now, Stats: stats.Compute(fixture, stats.Options{Now: now, Days: days, Location: time.UTC, AllRows: true})})
	}
	return nil
}

func matchedValidate(code int, output, diagnostic []byte, fixture []archive.Metadata, kind string, limit int, expected []byte, env Env) error {
	if code != 0 || len(diagnostic) != 0 {
		return fmt.Errorf("matched command exit=%d diagnostic-bytes=%d", code, len(diagnostic))
	}
	if kind == "list-text" {
		if !bytes.Equal(output, expected) {
			return errors.New("incomplete or incorrect matched text listing")
		}
		return nil
	}
	if kind == "list" {
		var doc listDocument
		if err := json.Unmarshal(output, &doc); err != nil {
			return err
		}
		want := min(len(fixture), limit)
		if doc.Version != listSchemaVersion || doc.Limit != limit || doc.Returned != want || len(doc.Sessions) != want || !doc.TotalMatchedKnown || doc.TotalMatched == nil || *doc.TotalMatched != len(fixture) || doc.Truncated != (want < len(fixture)) {
			return errors.New("incomplete matched listing")
		}
		for i, m := range doc.Sessions {
			// The list document's sessions are the same complete metadata shape.
			if !reflect.DeepEqual(m, fixture[len(fixture)-1-i]) {
				return errors.New("incorrect matched listing metadata/order")
			}
		}
		return nil
	}
	if kind == "interactive" {
		return matchedValidateFrames(output, fixture, env)
	}
	var got, want any
	if err := json.Unmarshal(output, &got); err != nil {
		return err
	}
	if err := json.Unmarshal(expected, &want); err != nil {
		return err
	}
	if !reflect.DeepEqual(got, want) {
		return errors.New("incomplete or incorrect matched JSON document")
	}
	return nil
}

func matchedValidateFrames(output []byte, fixture []archive.Metadata, env Env) error {
	frames := screenFrames(string(output))
	windows := []int{7, 30, 90}
	if len(frames) != len(windows) {
		return fmt.Errorf("matched screen frames=%d want=%d", len(frames), len(windows))
	}
	view := newStatsView(&matchedScreen{}, env)
	view.width, view.interactive = 80, true
	for n, days := range windows {
		if len(frames[n]) != 24 {
			return errors.New("incomplete matched terminal frame")
		}
		now := storagetest.BenchmarkTime.Add(24 * time.Hour)
		computed := stats.Compute(fixture, stats.Options{Now: now, Days: days, Location: time.UTC, AllRows: true})
		lines := renderPage(pageOverview, computed, view)
		for i := range 23 {
			want := ""
			if i < len(lines) {
				want = truncateVisible(lines[i], 80)
			}
			if frames[n][i] != want {
				return fmt.Errorf("incorrect matched frame=%d row=%d", n, i)
			}
		}
		footer := frames[n][23]
		if visibleWidth(footer) > 80 || !strings.Contains(footer, fmt.Sprintf("w window %dd>", days)) || !strings.Contains(footer, "q quit") {
			return errors.New("incorrect matched terminal footer")
		}
	}
	return nil
}

type matchedScreen struct {
	bytes.Buffer
	start time.Time
	first time.Duration
}

func (s *matchedScreen) Write(p []byte) (int, error) {
	n, err := s.Buffer.Write(p)
	if s.first == 0 && bytes.Contains(p, []byte(clearScreenSequence)) {
		s.first = time.Since(s.start)
	}
	return n, err
}

func (*matchedScreen) colorTerminal() bool { return false }

// The same validators used after timing must reject successful partial output.
func TestMatchedPerformanceRejectsPartialOutput(t *testing.T) {
	for _, kind := range []string{"show-parent", "stats7", "list", "list-text", "interactive"} {
		t.Run(kind, func(t *testing.T) {
			mem, fixture := matchedFixture(t, 60, strings.HasPrefix(kind, "stats") || kind == "interactive", kind == "show-parent")
			env := matchedEnv(t, t.TempDir(), mem)
			expected := matchedExpected(t, fixture, kind)
			in := strings.NewReader("")
			output := &matchedScreen{}
			var diagnostic bytes.Buffer
			if kind == "interactive" {
				env.IsTerminal = func(stream any) bool { return stream == any(in) || stream == any(output) }
				env.TerminalSize = func(io.Writer) (int, int, bool) { return 80, 24, true }
				env.openKeys = func(io.Reader) (keyTerminal, bool) { return newFakeKeys("w", "w", "q"), true }
			}
			code := Run(matchedArgs(kind, 50), in, output, &diagnostic, env)
			if err := matchedValidate(code, output.Bytes(), diagnostic.Bytes(), fixture, kind, 50, expected, env); err != nil {
				t.Fatal(err)
			}
			if err := matchedValidate(0, output.Bytes()[:output.Len()/2], nil, fixture, kind, 50, expected, env); err == nil {
				t.Fatal("accepted partial successful output")
			}
			if err := matchedValidate(0, output.Bytes(), []byte("warning"), fixture, kind, 50, expected, env); err == nil {
				t.Fatal("accepted command warning")
			}
		})
	}
}

// matchedStore freezes the bounded-versioned GET protocol on old and new
// sources. Older MeasuredStore lacks this method; newer one counts it. Calling
// its common GetVersioned path counts exactly one read on either source. Bodies
// here are tiny synthetic sidecars; the adapter checks bounds after copying.
// LIST timestamps are pinned because MemoryStore normally uses the host clock.
type matchedStore struct{ *storagetest.MeasuredStore }

func (s *matchedStore) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	data, etag, err := s.MeasuredStore.GetVersioned(ctx, key)
	if err != nil {
		return nil, "", err
	}
	if limit < 1 || int64(len(data)) > limit {
		return nil, "", storage.ErrObjectTooLarge
	}
	return data, etag, nil
}

func matchedHeaders(objects []storage.Object) []storage.Object {
	for i := range objects {
		objects[i].LastModified = storagetest.BenchmarkTime
	}
	return objects
}

func (s *matchedStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	objects, err := s.MeasuredStore.List(ctx, prefix)
	return matchedHeaders(objects), err
}

func (s *matchedStore) ListRange(ctx context.Context, prefix, after, through string) ([]storage.Object, error) {
	objects, err := s.MeasuredStore.ListRange(ctx, prefix, after, through)
	return matchedHeaders(objects), err
}

func (s *matchedStore) ListPage(ctx context.Context, prefix, continuation string, limit int32) (storage.ObjectPage, error) {
	page, err := s.MeasuredStore.ListPage(ctx, prefix, continuation, limit)
	page.Objects = matchedHeaders(page.Objects)
	return page, err
}

// matchedObjectHash fingerprints every stored key/body in deterministic key
// order. MemoryStore's host-clock write times are excluded and LIST returns
// their pinned replacements through matchedStore.
func matchedObjectHash(tb testing.TB, mem *storagetest.MemoryStore) []byte {
	tb.Helper()
	objects, err := mem.List(context.Background(), "")
	if err != nil {
		tb.Fatal(err)
	}
	hash := sha256.New()
	for _, object := range objects {
		body, err := mem.Get(context.Background(), object.Key)
		if err != nil {
			tb.Fatal(err)
		}
		_, _ = fmt.Fprintf(hash, "%d:%s:%d:", len(object.Key), object.Key, len(body))
		_, _ = hash.Write(body)
	}
	return hash.Sum(nil)
}
