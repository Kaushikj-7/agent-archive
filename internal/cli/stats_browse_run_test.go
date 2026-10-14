package cli

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// terminalRun is `stats` run through Run on a terminal: stdin and stdout are
// terminals to the Env, keys come from fake, and the pager copies what it is
// given. keysOpened says whether anything asked for the keys.
type terminalRun struct {
	stdout     string
	stderr     string
	code       int
	keysOpened bool
	fake       *fakeKeys
	frames     [][]string
	draws      []int32
}

// countingStore counts the reads made of a store.
type countingStore struct {
	*storagetest.MemoryStore
	gets atomic.Int32
}

func (s *countingStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	s.gets.Add(1)
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

func (s *countingStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets.Add(1)
	return s.MemoryStore.Get(ctx, key)
}

// runStatsOnTerminal runs `stats args` at a terminal of size with the keys.
func runStatsOnTerminal(t *testing.T, size fixedTerminal, tweak func(*Env), chunks []string, args ...string) terminalRun {
	t.Helper()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	return runStatsOnTerminalWith(t, env, mem, size, chunks, tweak, args...)
}

// runStatsOnTerminalWith is runStatsOnTerminal for an archive the caller
// published to mem.
func runStatsOnTerminalWith(t *testing.T, env Env, mem *storagetest.MemoryStore, size fixedTerminal, chunks []string, tweak func(*Env), args ...string) terminalRun {
	t.Helper()
	store := &countingStore{MemoryStore: mem}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	run := terminalRun{fake: newFakeKeys(chunks...)}
	stdin := strings.NewReader("")
	out := &statsScreenOutput{}
	env.IsTerminal = func(stream any) bool { return stream == any(stdin) || stream == any(out) }
	env.TerminalSize = func(io.Writer) (int, int, bool) {
		run.draws = append(run.draws, store.gets.Load())
		return size.width, size.height, true
	}
	env.openKeys = func(io.Reader) (keyTerminal, bool) { run.keysOpened = true; return run.fake, true }
	env.RunPager = func(_ context.Context, _ string, _ []string, in io.Reader, w, _ io.Writer) error {
		_, err := io.Copy(w, in)
		return err
	}
	if tweak != nil {
		tweak(&env)
	}
	var stderr strings.Builder
	run.code = Run(append([]string{"stats", "--prices", goldenPrices}, args...), stdin, out, &stderr, env)
	run.stdout, run.stderr = out.String(), stderr.String()
	run.frames = screenFrames(run.stdout)
	return run
}

// beforeFooter is the rows of a screen above its footer, which differs
// between the static screen (it names the flags) and the interactive one (the
// key bar does), with trailing blank rows dropped.
func beforeFooter(rows []string) []string {
	for i, row := range rows {
		if strings.HasPrefix(ansiEscape.ReplaceAllString(row, ""), "Estimated at list price") {
			rows = rows[:i]
			break
		}
	}
	for len(rows) > 0 && rows[len(rows)-1] == "" {
		rows = rows[:len(rows)-1]
	}
	return rows
}

// The interactive screen's window cycle is the same numbers as the static
// command with that --days, whichever window it starts in: the one read
// covers the longest window, its previous period and the month rank.
func TestStatsInteractiveWindowsMatchStaticRuns(t *testing.T) {
	t.Parallel()
	env, mem := statsEnv(t)
	publishStatsFixture(t, mem)
	memo := map[string][]string{}
	static := func(days string) []string {
		if rows, ok := memo[days]; ok {
			return rows
		}
		out := mustRunStats(t, env, 80, "--prices", goldenPrices, "--days", days)
		memo[days] = beforeFooter(strings.Split(out, "\n"))
		return memo[days]
	}
	for _, tc := range []struct {
		args  []string
		order []string
	}{
		{nil, []string{"30", "90", "7", "30"}},
		{[]string{"--days", "7"}, []string{"7", "30", "90", "7"}},
		{[]string{"--days", "90"}, []string{"90", "7", "30", "90"}},
		{[]string{"--days", "14"}, []string{"14", "30", "90", "7", "14"}},
	} {
		run := runStatsOnTerminal(t, fixedTerminal{80, 100}, nil, append(slices.Repeat([]string{"w"}, len(tc.order)-1), "q"), tc.args...)
		if run.code != 0 || run.stderr != "" || !run.keysOpened {
			t.Fatalf("%v: code %d stderr %q", tc.args, run.code, run.stderr)
		}
		if len(run.frames) != len(tc.order) {
			t.Fatalf("%v: %d frames, want %d", tc.args, len(run.frames), len(tc.order))
		}
		for i, days := range tc.order {
			got := beforeFooter(run.frames[i][:len(run.frames[i])-1])
			want := static(days)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("%v: window %s (frame %d) is not the static --days %s screen:\n%s\n---- static:\n%s", tc.args, days, i, days, strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		}
	}
}

// --since starts the screen on the days it names, as a custom window.
func TestStatsInteractiveSinceIsTheStartingWindow(t *testing.T) {
	t.Parallel()
	run := runStatsOnTerminal(t, fixedTerminal{100, 100}, nil, []string{"w", "q"}, "--since", "2026-09-20")
	if run.code != 0 || len(run.frames) != 2 {
		t.Fatalf("code %d, %d frames, stderr %q", run.code, len(run.frames), run.stderr)
	}
	if !strings.Contains(run.frames[0][0], "last 10 days") || !strings.Contains(run.frames[0][len(run.frames[0])-1], "w window 10d>30d") {
		t.Errorf("first frame: %q / %q", run.frames[0][0], run.frames[0][len(run.frames[0])-1])
	}
	if !strings.Contains(run.frames[1][0], "last 30 days") {
		t.Errorf("w from a custom window: %q", run.frames[1][0])
	}
}

// Nothing is read after the first screen: switching views and windows only
// counts what was read, so the store is asked no more.
func TestStatsInteractiveReadsTheArchiveOnce(t *testing.T) {
	t.Parallel()
	run := runStatsOnTerminal(t, fixedTerminal{80, 24}, nil, []string{"w", "d", "w", "p", "w", "m", "h", "\x1b", "q"})
	if len(run.draws) < 8 || run.draws[0] == 0 {
		t.Fatalf("reads at each draw: %v", run.draws)
	}
	for i, n := range run.draws {
		if n != run.draws[0] {
			t.Fatalf("the archive was read again: %v (draw %d)", run.draws, i)
		}
	}
}

// Filters apply on the screen, a window with nothing in it says so instead of
// closing the screen, and w moves on from it.
func TestStatsInteractiveFiltersAndEmptyWindows(t *testing.T) {
	t.Parallel()
	run := runStatsOnTerminal(t, fixedTerminal{100, 40}, nil, []string{"w", "w", "w", "q"}, "--harness", "cursor")
	if run.code != 0 || len(run.frames) != 4 {
		t.Fatalf("code %d, %d frames, stderr %q", run.code, len(run.frames), run.stderr)
	}
	if !strings.Contains(run.frames[0][0], "harness cursor") {
		t.Errorf("the filter is not in the title: %q", run.frames[0][0])
	}
	// 30d, 90d, 7d (nothing from Cursor in the last week), 30d.
	empty := strings.Join(run.frames[2], "\n")
	if !strings.Contains(empty, "No archived sessions match these filters") || !strings.Contains(run.frames[2][len(run.frames[2])-1], "w window 7d>30d") {
		t.Errorf("the empty window:\n%s", empty)
	}
	if strings.Contains(strings.Join(run.frames[3], "\n"), "No archived sessions") {
		t.Error("the next window is still empty")
	}
}

// An archive with nothing in the read range never opens the screen: it is
// the message and exit 0, as on the static path.
func TestStatsInteractiveNothingToShowIsTheMessage(t *testing.T) {
	t.Parallel()
	run := runStatsOnTerminal(t, fixedTerminal{100, 40}, nil, []string{"q"}, "--harness", "codex", "--model", "nonexistent")
	if run.code != 0 || run.keysOpened || len(run.frames) != 0 || !strings.Contains(run.stdout, "No archived sessions match these filters") {
		t.Fatalf("code %d, keys opened %v, stdout %q", run.code, run.keysOpened, run.stdout)
	}
}
