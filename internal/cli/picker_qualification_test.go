package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type qualificationReads struct {
	Gets               int64
	Lists              int64
	Stats              int64
	Head               int64
	Coordinator        int64
	Nodes              int64
	Metadata           int64
	Sources            int64
	PayloadBytes       int64
	EstimatedListBytes int64
	NonProjectRoots    int64
}

type qualificationMeter struct {
	*storagetest.MeasuredStore
	mu              sync.Mutex
	reads           qualificationReads
	nonProjectRoots map[string]bool
}

func (m *qualificationMeter) record(key string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads.Gets++
	m.reads.PayloadBytes += int64(n)
	if m.nonProjectRoots[key] {
		m.reads.NonProjectRoots++
	}
	switch {
	case key == catalog.HeadKey:
		m.reads.Head++
	case key == catalog.CoordinatorKey:
		m.reads.Coordinator++
	case strings.HasPrefix(key, "catalog-v4/nodes/"):
		m.reads.Nodes++
	case strings.HasPrefix(key, "catalog-v4/metadata/") || strings.HasSuffix(key, "/metadata.json"):
		m.reads.Metadata++
	default:
		m.reads.Sources++
	}
}

func (m *qualificationMeter) snapshot() qualificationReads {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.reads
	metrics := m.Metrics()
	r.Lists = metrics.Lists
	r.EstimatedListBytes = metrics.ListBytes
	return r
}

func (m *qualificationMeter) Get(ctx context.Context, key string) (b []byte, e error) {
	b, e = m.MeasuredStore.Get(ctx, key)
	m.record(key, len(b))
	return
}

func (m *qualificationMeter) GetLimited(ctx context.Context, key string, limit int64) (b []byte, e error) {
	b, e = m.MeasuredStore.GetLimited(ctx, key, limit)
	m.record(key, len(b))
	return
}

func (m *qualificationMeter) GetVersioned(ctx context.Context, key string) (b []byte, v string, e error) {
	b, v, e = m.MeasuredStore.GetVersioned(ctx, key)
	m.record(key, len(b))
	return
}

func (m *qualificationMeter) GetCatalogVersion(ctx context.Context, key string, limit int64) (b []byte, v storage.CatalogObjectVersion, e error) {
	b, v, e = m.MeasuredStore.GetCatalogVersion(ctx, key, limit)
	m.record(key, len(b))
	return
}

func (m *qualificationMeter) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	m.mu.Lock()
	m.reads.Stats++
	m.mu.Unlock()
	return m.MeasuredStore.Stat(ctx, key)
}

func qualificationSubtract(a, b qualificationReads) qualificationReads {
	return qualificationReads{Gets: a.Gets - b.Gets, Lists: a.Lists - b.Lists, Stats: a.Stats - b.Stats, Head: a.Head - b.Head, Coordinator: a.Coordinator - b.Coordinator, Nodes: a.Nodes - b.Nodes, Metadata: a.Metadata - b.Metadata, Sources: a.Sources - b.Sources, PayloadBytes: a.PayloadBytes - b.PayloadBytes, EstimatedListBytes: a.EstimatedListBytes - b.EstimatedListBytes, NonProjectRoots: a.NonProjectRoots - b.NonProjectRoots}
}

// qualificationOracle uses the exhaustive product matcher and raw comparison,
// never picker memberships, cursors, rows or tree key encoding.
func qualificationOracle(metadata []archive.Metadata, command string) []archive.Metadata {
	scope := sessionScope{Label: "Qualification", Dir: qualificationProject, RepoKey: qualificationRepo, ProjectIDs: []string{archive.ProjectID(qualificationProject)}}
	return qualificationScopeOracle(metadata, command, scope)
}

func qualificationScopeOracle(metadata []archive.Metadata, command string, scope sessionScope) []archive.Metadata {
	var result []archive.Metadata
	for _, m := range metadata {
		if m.IsChild() || m.Replay != nil || !scope.contains(m, nil) {
			continue
		}
		if command == "handoff" && m.Counts.Turns != nil && *m.Counts.Turns == 0 {
			continue
		}
		result = append(result, m)
	}
	slices.SortFunc(result, func(a, b archive.Metadata) int {
		x, y := listingindex.ActivityTime(a), listingindex.ActivityTime(b)
		if !x.Equal(y) {
			if x.After(y) {
				return -1
			}
			return 1
		}
		if !a.CapturedAt.Equal(b.CapturedAt) {
			if a.CapturedAt.After(b.CapturedAt) {
				return -1
			}
			return 1
		}
		if a.Harness.Name != b.Harness.Name {
			return strings.Compare(a.Harness.Name, b.Harness.Name)
		}
		return strings.Compare(a.SessionID, b.SessionID)
	})
	return result
}

type qualificationPhase struct {
	NativeStats        int
	NativeActivityRefs int
	NativeOpens        int
	NativeBytes        int64
	ElapsedNS          int64
	AllocBytes         uint64
	AllocObjects       uint64
	Reads              qualificationReads
	Frame              string `json:"-"`
}

type qualificationResult struct {
	NativeStats          int
	NativeActivityRefs   int
	Format               int
	Sessions             int
	Registrations        int
	Command              string
	CacheState           string
	FixtureSHA256        string
	First                qualificationPhase
	Older                qualificationPhase
	SampledPeakHeapInuse uint64
	InitialHeapInuse     uint64
	NativeOpens          int
	NativeBytes          int64
}

type qualificationScreen struct {
	mu             sync.Mutex
	out            bytes.Buffer
	start          time.Time
	olderStart     time.Time
	meter          *qualificationMeter
	native         *qualificationNativeMeter
	before         runtime.MemStats
	first          qualificationPhase
	older          qualificationPhase
	olderTitle     string
	firstLastTitle string
	frameClear     bool
	olderStarted   bool
}

func (s *qualificationScreen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, e := s.out.Write(p)
	// The renderer clears separately from its complete buffered table write.
	// Retain that boundary; a spinner/clear-only write is never a usable frame.
	if bytes.Contains(p, []byte(clearScreenSequence)) || bytes.Contains(p, []byte(enterAltScreenSequence)) {
		s.frameClear = true
	}
	if !s.frameClear {
		return n, e
	}
	if s.first.ElapsedNS == 0 && !bytes.Contains(p, []byte(s.firstLastTitle)) {
		return n, e
	}
	if s.first.ElapsedNS != 0 && (!s.olderStarted || !bytes.Contains(p, []byte(s.olderTitle))) {
		return n, e
	}
	var mem runtime.MemStats
	if s.first.ElapsedNS == 0 {
		runtime.ReadMemStats(&mem)
		s.frameClear = false
		s.first = qualificationPhase{ElapsedNS: time.Since(s.start).Nanoseconds(), AllocBytes: mem.TotalAlloc - s.before.TotalAlloc, AllocObjects: mem.Mallocs - s.before.Mallocs, Reads: s.meter.snapshot(), Frame: string(p)}
		s.nativePhase(&s.first, false)
	} else if s.olderStarted && s.older.ElapsedNS == 0 && bytes.Contains(p, []byte(s.olderTitle)) {
		runtime.ReadMemStats(&mem)
		s.frameClear = false
		s.older = qualificationPhase{ElapsedNS: time.Since(s.olderStart).Nanoseconds(), AllocBytes: mem.TotalAlloc - s.before.TotalAlloc, AllocObjects: mem.Mallocs - s.before.Mallocs, Reads: qualificationSubtract(s.meter.snapshot(), s.first.Reads), Frame: string(p)}
		s.nativePhase(&s.older, true)
	}
	return n, e
}

func (s *qualificationScreen) nativePhase(phase *qualificationPhase, older bool) {
	if s.native == nil {
		return
	}
	phase.NativeBytes, phase.NativeOpens, _, phase.NativeStats, phase.NativeActivityRefs = s.native.snapshot()
	if older {
		phase.NativeBytes -= s.first.NativeBytes
		phase.NativeOpens -= s.first.NativeOpens
		phase.NativeStats -= s.first.NativeStats
		phase.NativeActivityRefs -= s.first.NativeActivityRefs
	}
}

func (*qualificationScreen) colorTerminal() bool { return false }

type qualificationKeys struct {
	*fakeKeys
	screen    *qualificationScreen
	sentOlder bool
}

func (k *qualificationKeys) read(p []byte, wait time.Duration) (int, error) {
	s := k.screen
	s.mu.Lock()
	if s.first.ElapsedNS == 0 {
		s.mu.Unlock()
		if wait == 0 {
			return 0, nil
		} // readBurst probes readiness before the first draw.
		return 0, fmt.Errorf("terminal requested input before first usable frame")
	}
	if !k.sentOlder {
		k.sentOlder = true
		s.olderStarted = true
		s.olderStart = time.Now()
		runtime.ReadMemStats(&s.before)
		s.mu.Unlock()
		p[0] = 'o'
		return 1, nil
	}
	if s.older.ElapsedNS != 0 {
		s.mu.Unlock()
		p[0] = 'q'
		return 1, nil
	}
	s.mu.Unlock()
	if wait < 0 {
		wait = 10 * time.Millisecond
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	<-timer.C
	return 0, nil
}

func qualificationRun(tb testing.TB, fixture qualificationFixture, home, command, state string, registrations int) qualificationResult {
	tb.Helper()
	return qualificationRunScoped(tb, fixture, home, command, state, registrations, nil)
}

func qualificationRunScoped(tb testing.TB, fixture qualificationFixture, home, command, state string, registrations int, configure func(*Env) sessionScope) qualificationResult {
	tb.Helper()
	// Every command owns its mutable remote state, even when callers reuse a
	// universe across scope, registration, cache-state or command matrices.
	fixture = cloneQualificationFixture(tb, fixture)
	// TempDir subdirectories inherit the process umask. Real native reads
	// require a private archive home, independently of the runner's umask.
	if err := os.Chmod(home, 0o700); err != nil {
		tb.Fatal(err)
	}
	meter := &qualificationMeter{MeasuredStore: storagetest.NewMeasuredStore(fixture.raw, 0), nonProjectRoots: map[string]bool{fixture.head.Identity.Key: true, fixture.head.Capture.Key: true, fixture.head.Activity.Key: true}}
	remote, err := catalog.Wrap(meter)
	if err != nil {
		tb.Fatal(err)
	}
	env := benchmarkEnv(tb, home, remote)
	locals := qualificationRegistrations(tb, home, fixture.metadata, registrations, state == "native-labels")
	env.WorkingDir = func() (string, error) { return qualificationProject, nil }
	env.repoKey = func(string) string { return qualificationRepo }

	universe := fixture.metadata
	if len(locals) > 0 {
		universe = append(slices.Clone(universe), locals...)
	}
	oracle := qualificationOracle(universe, command)
	if configure != nil {
		oracle = qualificationScopeOracle(universe, command, configure(&env))
	}
	if len(oracle) < 100 {
		tb.Fatal("qualification requires at least two complete pages")
	}
	screen := &qualificationScreen{meter: meter, olderTitle: oracle[99].Title, firstLastTitle: oracle[49].Title}
	env.TerminalSize = func(io.Writer) (int, int, bool) {
		screen.mu.Lock()
		defer screen.mu.Unlock()
		if screen.olderStarted {
			return 120, 120, true
		}
		return 120, 70, true
	}
	keys := &qualificationKeys{fakeKeys: newFakeKeys(), screen: screen}
	in := strings.NewReader("")
	env.IsTerminal = func(stream any) bool { return stream == any(in) || stream == any(screen) }
	env.openKeys = func(io.Reader) (keyTerminal, bool) { return keys, true }
	native := &qualificationNativeMeter{}
	screen.native = native
	env.Agents = qualificationRegistry(tb, native)
	env.nativeFS = native
	var stderr bytes.Buffer
	runtime.GC()
	runtime.ReadMemStats(&screen.before)
	initial := screen.before.HeapInuse
	var peak uint64
	stop, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				var sample runtime.MemStats
				runtime.ReadMemStats(&sample)
				peak = max(peak, sample.HeapInuse)
			case <-stop:
				return
			}
		}
	}()
	screen.start = time.Now()
	args := []string{command}
	if command == "handoff" && registrations == 0 {
		args = append(args, "--source", "archive")
	}
	code := Run(args, in, screen, &stderr, env)
	close(stop)
	<-joined
	if code != 0 {
		tb.Fatalf("qualification Run %s failed: code=%d stderr=%s", command, code, stderr.String()+" synthetic frame="+screen.out.String())
	}
	if screen.first.ElapsedNS == 0 || screen.older.ElapsedNS == 0 {
		tb.Fatalf("missing actual first/Older complete frame: %s", screen.out.String())
	}
	qualificationValidateFrame(tb, screen.first.Frame, oracle[:50], 1)
	qualificationValidateFrame(tb, screen.older.Frame, oracle[:100], 1)
	nativeBytes, opens, active, nativeStats, nativeActivityRefs := native.snapshot()
	if active != 0 {
		tb.Fatal("native resources remain open")
	}
	result := qualificationResult{Format: qualificationFormat, Sessions: len(fixture.metadata), Registrations: registrations, Command: command, CacheState: state, FixtureSHA256: fixture.manifest, First: screen.first, Older: screen.older, InitialHeapInuse: initial, SampledPeakHeapInuse: max(initial, peak), NativeOpens: opens, NativeBytes: nativeBytes, NativeStats: nativeStats, NativeActivityRefs: nativeActivityRefs}
	if qualificationFormat == 11 && configure == nil {
		qualificationBudget(tb, result.First.Reads, fixture.height, registrations, 2, false)
		qualificationBudget(tb, result.Older.Reads, fixture.height, registrations, 2, true)
	}
	return result
}

var qualificationTitlePattern = regexp.MustCompile(`[QG][0-9]{6}`)

var qualificationNumberPattern = regexp.MustCompile(`^\s*([0-9]+)\s+`)

func qualificationFrameError(frame string, want []archive.Metadata, start int) error {
	got := qualificationTitlePattern.FindAllString(frame, -1)
	if len(got) != len(want) {
		return fmt.Errorf("complete frame has%d rows, want%d", len(got), len(want))
	}
	for i, m := range want {
		if got[i] != m.Title {
			return fmt.Errorf("row%d title%s differs from exhaustive%s", start+i, got[i], m.Title)
		}
	}
	ordinal := start
	for line := range strings.SplitSeq(frame, "\n") {
		if !qualificationTitlePattern.MatchString(line) {
			continue
		}
		number := qualificationNumberPattern.FindStringSubmatch(line)
		if len(number) != 2 {
			return fmt.Errorf("row%d lacks stable table number", ordinal)
		}
		n, err := strconv.Atoi(number[1])
		if err != nil || n != ordinal {
			return fmt.Errorf("row%d was renumbered", ordinal)
		}
		ordinal++
	}
	return nil
}

func qualificationValidateFrame(tb testing.TB, frame string, want []archive.Metadata, start int) {
	tb.Helper()
	if err := qualificationFrameError(frame, want, start); err != nil {
		tb.Fatal(err)
	}
}

func TestPickerQualificationFrameOracleRejectsIncompleteAndReorderedOutput(t *testing.T) {
	want := qualificationOracle(qualificationMetadata(1400, 0), "list")[:50]
	var titles []string
	for _, m := range want {
		titles = append(titles, fmt.Sprintf("%d %s", len(titles)+1, m.Title))
	}
	if err := qualificationFrameError(strings.Join(titles, "\n"), want, 1); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func([]string) []string{
		func(s []string) []string { return s[:49] },
		func(s []string) []string { s[1] = s[0]; return s },
		func(s []string) []string { s[1], s[0] = s[0], s[1]; return s },
		func(s []string) []string { return append(s, "51 Q999999") },
	} {
		if err := qualificationFrameError(strings.Join(mutate(slices.Clone(titles)), "\n"), want, 1); err == nil {
			t.Fatal("accepted incomplete, duplicate, reordered or expanded output")
		}
	}
}

func qualificationBudget(tb testing.TB, r qualificationReads, height, registrations, ranges int, older bool) {
	tb.Helper()
	// The scale query has S=2 disjoint memberships; the boundary control
	// supplies S=32. P=51 is handoff's sealed lookahead (browse uses50).
	// A page has at most
	// S*H*(P+1+4) node visits: one path per candidate/lookahead and four
	// boundary/count paths per range. Canonical lookup costs at most R*H.
	// For R=0 the adapter prefetch fills First and one page fills Older.
	pages, batches, identities := int64(1), int64(0), int64(0)
	if registrations > 0 {
		// The model allows2+ceil(R/50) refill pages per operation. First
		// additionally includes the adapter prefetch and ceil(R/100) exact
		// identity batches; Older reuses the classification.
		pages = 2 + int64((registrations+49)/50)
		if !older {
			pages++
			batches = int64((registrations + 99) / 100)
			identities = int64(registrations)
		}
	}
	pageNodes := int64(ranges * height * (51 + 1 + 4))
	maxNodes := pages*pageNodes + identities*int64(height)
	// Each page/batch has one head/coordinator observation; opening supplies
	// the first pair. A zero-page model return can add one head check, and
	// handoff Older adds one entry check. These are counted even when cached.
	maxHeads, maxCoordinators := pages+batches+1, pages+batches
	if older {
		maxHeads++
	}
	// Each logical reader operation limits tree bytes to32MiB. Head and
	// coordinator bodies have independent16KiB and4MiB limits. Keep their
	// full worst-case allowance instead of relying on small fixture bodies.
	maxNodeBytes := pages*min(pageNodes*(128<<10), 32<<20) + min(identities*int64(height)*(128<<10), batches*(32<<20))
	maxPayload := maxNodeBytes + maxHeads*(16<<10) + maxCoordinators*(4<<20)
	if r.Lists != 0 || r.Metadata != 0 || r.Sources != 0 || r.NonProjectRoots != 0 || r.Nodes > maxNodes || r.PayloadBytes > maxPayload || r.Head > maxHeads || r.Coordinator > maxCoordinators {
		tb.Fatalf("bounded CLI phase exceeded contract (older=%v height=%d registrations=%d nodes<=%d heads<=%d coordinators<=%d bytes<=%d): %+v", older, height, registrations, maxNodes, maxHeads, maxCoordinators, maxPayload, r)
	}
	if older && (r.Head == 0 || (registrations == 0 && r.Coordinator == 0)) {
		tb.Fatal("Older did not make a fresh remote observation")
	}
}

// qualificationDefaultFixture returns private writable state from the cached
// full-size universe; construction is excluded from instrumented CLI phases.
func qualificationDefaultFixture(tb testing.TB) qualificationFixture {
	tb.Helper()
	return newQualificationFixture(tb, qualificationMetadata(1400, 0))
}

func TestPickerQualificationRealCLIPhases(t *testing.T) {
	sizes := []int{1400}
	if os.Getenv("AGENT_ARCHIVE_PERF") == "1" {
		sizes = []int{10000, 100000}
	}
	for _, size := range sizes {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			var fixture qualificationFixture
			if size == 1400 {
				fixture = qualificationDefaultFixture(t)
			} else {
				fixture = newQualificationFixture(t, qualificationMetadata(size, 0))
			}
			for _, command := range []string{"handoff", "list", "show"} {
				t.Run(command, func(t *testing.T) {
					result := qualificationRun(t, fixture, t.TempDir(), command, "cold", 0)
					t.Log(string(qualificationJSON(t, result)))
				})
			}
		})
	}
}

func BenchmarkPickerQualification(b *testing.B) {
	if os.Getenv("AGENT_ARCHIVE_PERF") != "1" {
		b.Skip("10k/100k qualification requires AGENT_ARCHIVE_PERF=1")
	}
	sizes := []int{10000, 100000}
	if os.Getenv("AGENT_ARCHIVE_PICKER_SIZE") == "10000" {
		sizes = []int{10000}
	}
	if os.Getenv("AGENT_ARCHIVE_PICKER_SIZE") == "100000" {
		sizes = []int{100000}
	}
	for _, size := range sizes {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			b.StopTimer()
			fixture := newQualificationFixture(b, qualificationMetadata(size, 0))
			changedFixture := newQualificationFixture(b, qualificationMetadata(size, 1))
			for _, command := range []string{"handoff", "list", "show"} {
				for _, state := range []string{"cold", "warm", "changed"} {
					b.Run(command+"/"+state, func(b *testing.B) {
						b.StopTimer()
						home := b.TempDir()
						current := fixture
						if state == "changed" {
							current = changedFixture
						}
						for range b.N {
							if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
								b.Fatal(err)
							}
							if state != "cold" {
								_ = qualificationRun(b, fixture, home, command, "warmup", 0)
							}
							// qualificationRun emits phase-specific allocation and wall-time metrics.
							// Standard ns/op is deliberately unused because fixture/validation/sampler
							// boundaries do not coincide with one whole-command Go benchmark timer.
							result := qualificationRun(b, current, home, command, state, 0)
							b.Log(string(qualificationJSON(b, result)))
							b.ReportMetric(float64(result.First.ElapsedNS), "first-frame-ns/op")
							b.ReportMetric(float64(result.Older.ElapsedNS), "older-frame-ns/op")
							b.ReportMetric(float64(result.First.AllocBytes), "first-B/op")
							b.ReportMetric(float64(result.First.AllocObjects), "first-allocs/op")
							b.ReportMetric(float64(result.SampledPeakHeapInuse), "sampled-heap-inuse-B")
							b.ReportMetric(float64(result.First.Reads.Gets), "first-GET/op")
							b.ReportMetric(float64(result.First.Reads.PayloadBytes), "first-payload-B/op")
						}
					})
				}
			}
		})
	}
}

func qualificationRegistrations(tb testing.TB, home string, metadata []archive.Metadata, count int, labels bool) []archive.Metadata {
	tb.Helper()
	if count == 0 {
		return nil
	}
	eligible := qualificationOracle(metadata, "handoff")
	if count > len(eligible) {
		tb.Fatal("native inventory exceeds relevant synthetic roots")
	}
	local, err := state.Open(home)
	if err != nil {
		tb.Fatal(err)
	}
	var added []archive.Metadata
	for i, m := range eligible[:count] {
		if labels {
			m.SessionID = "local-" + m.SessionID
			m.Title = fmt.Sprintf("Q%06d", 900000+i)
			m.CapturedAt = time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Second)
			m.Counts.Turns = nil
			added = append(added, m)
		}
		path := filepath.Join(home, "native", m.SessionID+".jsonl")
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			tb.Fatal(err)
		}
		content := fmt.Sprintf(`{"type":"session_meta","timestamp":%q,"payload":{"id":%q,"cwd":%q}}`+"\n", m.CapturedAt.Format(time.RFC3339Nano), m.SessionID, qualificationProject)
		if labels {
			content += fmt.Sprintf(`{"type":"response_item","timestamp":%q,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`+"\n", m.CapturedAt.Format(time.RFC3339Nano), m.Title)
		}
		if err = os.WriteFile(path, []byte(content), 0o600); err != nil {
			tb.Fatal(err)
		}
		if err = os.Chtimes(path, m.CapturedAt, m.CapturedAt); err != nil {
			tb.Fatal(err)
		}
		reg := archive.SessionRegistration{ArchiveSessionID: m.SessionID, NativeSessionID: m.SessionID, ProjectID: m.ProjectID, ProjectRoot: qualificationProject, Harness: m.Harness, TranscriptPath: path, RepoKey: qualificationRepo, RegisteredAt: m.CapturedAt, SessionStartedAt: m.CapturedAt}
		if err = local.SaveRegistration(reg); err != nil {
			tb.Fatal(err)
		}
	}
	return added
}

func TestPickerQualificationNativeHomeIsPrivate(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	result := qualificationRun(t, qualificationDefaultFixture(t), home, "handoff", "native-labels", 20)
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 || result.NativeOpens == 0 || result.NativeBytes == 0 {
		t.Fatal("qualification did not prepare a private home and read real native labels")
	}
}

func TestPickerQualificationNativeCostIsSeparate(t *testing.T) {
	fixture := qualificationDefaultFixture(t)
	for _, mode := range []string{"cold", "native-labels"} {
		for _, count := range []int{20, 120} {
			t.Run(fmt.Sprintf("%s/%d", mode, count), func(t *testing.T) {
				result := qualificationRun(t, fixture, t.TempDir(), "handoff", mode, count)
				if result.NativeActivityRefs == 0 || result.NativeStats == 0 {
					t.Fatal("native inventory was not actually observed")
				}
				if mode == "native-labels" && result.NativeBytes == 0 {
					t.Fatal("local-only labels were not actually read")
				}
				if result.NativeBytes > 2*int64(min(count, 100))*nativeWindowBytes {
					t.Fatal("two page operations exceeded their 50-attempt native window budget")
				}
				t.Log(string(qualificationJSON(t, result)))
			})
		}
	}
}

func BenchmarkPickerQualificationNative(b *testing.B) {
	if os.Getenv("AGENT_ARCHIVE_PERF") != "1" {
		b.Skip("native/archive scale qualification requires AGENT_ARCHIVE_PERF=1")
	}
	sizes := []int{10000, 100000}
	if os.Getenv("AGENT_ARCHIVE_PICKER_SIZE") == "10000" {
		sizes = []int{10000}
	}
	if os.Getenv("AGENT_ARCHIVE_PICKER_SIZE") == "100000" {
		sizes = []int{100000}
	}
	for _, size := range sizes {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			b.StopTimer()
			fixture := newQualificationFixture(b, qualificationMetadata(size, 0))
			for _, mode := range []string{"cold", "native-labels"} {
				for _, count := range []int{20, 120} {
					b.Run(fmt.Sprintf("%s/%d", mode, count), func(b *testing.B) {
						b.StopTimer()
						for range b.N {
							result := qualificationRun(b, fixture, b.TempDir(), "handoff", mode, count)
							b.Log(string(qualificationJSON(b, result)))
							b.ReportMetric(float64(result.First.ElapsedNS), "first-frame-ns/op")
							b.ReportMetric(float64(result.NativeOpens), "native-open/op")
							b.ReportMetric(float64(result.NativeBytes), "native-B/op")
						}
					})
				}
			}
		})
	}
}

type qualificationNativeMeter struct {
	nativeReadMeter
	stats        int
	activityRefs int
}

func (m *qualificationNativeMeter) snapshot() (int64, int, int, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bytes, m.opens, m.active, m.stats, m.activityRefs
}

func (m *qualificationNativeMeter) Lstat(path string) (os.FileInfo, error) {
	m.mu.Lock()
	m.stats++
	m.mu.Unlock()
	return m.OS.Lstat(path)
}

type qualificationActivityProvider struct {
	agentapi.SourceProvider
	meter *qualificationNativeMeter
}

func (p qualificationActivityProvider) Activities(ctx context.Context, e agentapi.SourceEnvironment, refs []agentapi.SourceRef) (map[agentapi.SourceRef]time.Time, error) {
	p.meter.mu.Lock()
	p.meter.activityRefs += len(refs)
	p.meter.mu.Unlock()
	e.Files = qualificationFiles{base: e.Files, meter: p.meter}
	return p.SourceProvider.(agentapi.ActivityProvider).Activities(ctx, e, refs)
}

func (p qualificationActivityProvider) OpenPass(ctx context.Context, e agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	e.Files = qualificationFiles{base: e.Files, meter: p.meter}
	return p.SourceProvider.OpenPass(ctx, e)
}

type qualificationFiles struct {
	base  transcriptio.Opener
	meter *qualificationNativeMeter
}

func (f qualificationFiles) opener() transcriptio.Opener {
	if f.base != nil {
		return f.base
	}
	return transcriptio.OS{}
}

func (f qualificationFiles) Lstat(path string) (os.FileInfo, error) {
	f.meter.mu.Lock()
	f.meter.stats++
	f.meter.mu.Unlock()
	return f.opener().Lstat(path)
}

func (f qualificationFiles) EvalSymlinks(path string) (string, error) {
	return f.opener().EvalSymlinks(path)
}

func (f qualificationFiles) OpenRegular(path string) (transcriptio.File, error) {
	file, err := f.opener().OpenRegular(path)
	if err != nil {
		return nil, err
	}
	m := &f.meter.nativeReadMeter
	m.mu.Lock()
	m.opens++
	m.active++
	m.peak = max(m.peak, m.active)
	m.mu.Unlock()
	return &nativeMeterFile{File: file, meter: m}, nil
}

func qualificationRegistry(tb testing.TB, meter *qualificationNativeMeter) *builtin.Registry {
	tb.Helper()
	base := builtin.NewBuiltins()
	var bindings []builtin.Integration
	for _, d := range base.Catalog().All() {
		binding, ok := base.Lookup(string(d.ID))
		if !ok {
			tb.Fatal("missing synthetic registry binding")
		}
		if _, ok = binding.Sources.(agentapi.ActivityProvider); ok {
			binding.Sources = qualificationActivityProvider{SourceProvider: binding.Sources, meter: meter}
		}
		binding.Descriptor = agentmeta.Descriptor{ID: d.ID}
		bindings = append(bindings, binding)
	}
	registry, err := builtin.New(base.Catalog(), bindings)
	if err != nil {
		tb.Fatal(err)
	}
	return registry
}

func TestPickerQualificationMeterCountsAttemptsWithoutDuplicateForwarding(t *testing.T) {
	raw := &qualifiedCLIStore{storagetest.NewMemoryStore()}
	for _, key := range []string{catalog.HeadKey, catalog.CoordinatorKey, "catalog-v4/metadata/fixture.json"} {
		qualificationPut(t, raw, key, []byte("data"))
	}
	m := &qualificationMeter{MeasuredStore: storagetest.NewMeasuredStore(raw, 0)}
	if _, err := m.GetLimited(t.Context(), catalog.HeadKey, 4); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.GetCatalogVersion(t.Context(), catalog.CoordinatorKey, 4); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.GetVersioned(t.Context(), "catalog-v4/metadata/fixture.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetLimited(t.Context(), catalog.HeadKey, 2); !errors.Is(err, storage.ErrObjectTooLarge) {
		t.Fatal(err)
	}
	if _, err := m.Get(t.Context(), "catalog-v4/nodes/missing.json"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.Get(ctx, "sessions/codex/missing/source.jsonl.gz"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := m.List(t.Context(), "sessions"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stat(t.Context(), catalog.HeadKey); err != nil {
		t.Fatal(err)
	}
	got := m.snapshot()
	want := qualificationReads{Gets: 6, Lists: 1, Stats: 1, Head: 2, Coordinator: 1, Nodes: 1, Metadata: 1, Sources: 1, PayloadBytes: 12}
	if got != want {
		t.Fatalf("attempted calls/returned bytes were omitted or counted twice: got%+v want%+v", got, want)
	}
}
