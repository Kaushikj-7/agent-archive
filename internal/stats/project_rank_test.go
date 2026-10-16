package stats

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func projectNames(projects []Project) []string {
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.Name
	}
	return names
}

func costOf(usd float64) Cost {
	return Cost{USD: &usd}
}

func tokenCount(n int64) *int64 {
	return &n
}

// partialCost is a spend that leaves some tokens out because their model has no price.
func partialCost(usd float64, unpriced int64) Cost {
	return Cost{USD: &usd, Partial: true, UnpricedTokens: unpriced}
}

// The projects are ranked by spend, then tokens, sessions and name, whatever
// order they are given in, with the ones that have no price last, and a partly
// priced project on the spend it does have.
func TestProjectBeforeRanksBySpendThenTokensSessionsName(t *testing.T) {
	t.Parallel()
	nan := math.NaN()
	in := []Project{
		{Name: "unpriced-small", Sessions: 9, Tokens: tokenCount(10)},
		{Name: "tie-b", Sessions: 1, Tokens: tokenCount(500), Cost: costOf(50)},
		{Name: "cheap-huge", Sessions: 1, Tokens: tokenCount(9_000_000), Cost: costOf(5)},
		{Name: "unpriced-big", Sessions: 1, Tokens: tokenCount(1000)},
		{Name: "top", Sessions: 1, Tokens: tokenCount(1), Cost: costOf(90)},
		{Name: "tie-a", Sessions: 1, Tokens: tokenCount(500), Cost: costOf(50)},
		{Name: "tie-more-tokens", Sessions: 1, Tokens: tokenCount(600), Cost: costOf(50)},
		{Name: "tie-more-sessions", Sessions: 2, Tokens: tokenCount(500), Cost: costOf(50)},
		{Name: "partial", Sessions: 1, Tokens: tokenCount(7_000_000), Cost: partialCost(60, 6_999_000)},
		{Name: "no-tokens", Sessions: 3},
		{Name: "not-a-number", Sessions: 1, Tokens: tokenCount(1), Cost: Cost{USD: &nan}},
		{Name: "zero-spend", Sessions: 1, Tokens: tokenCount(2), Cost: costOf(0)},
	}
	want := []string{
		"top", "partial", "tie-more-tokens", "tie-more-sessions", "tie-a", "tie-b", "cheap-huge", "zero-spend",
		"unpriced-big", "unpriced-small", "not-a-number", "no-tokens",
	}
	rank := func(projects []Project) []string {
		ranked := slices.Clone(projects)
		sort.SliceStable(ranked, func(i, j int) bool { return projectBefore(ranked[i], ranked[j]) })
		return projectNames(ranked)
	}
	for shift := range in {
		rotated := append(slices.Clone(in[shift:]), in[:shift]...)
		if got := rank(rotated); !slices.Equal(got, want) {
			t.Fatalf("from %v: %v, want %v", projectNames(rotated), got, want)
		}
	}
	reversed := slices.Clone(in)
	slices.Reverse(reversed)
	if got := rank(reversed); !slices.Equal(got, want) {
		t.Errorf("reversed: %v, want %v", got, want)
	}
	rng := rand.New(rand.NewPCG(7, 11))
	for range 50 {
		shuffled := slices.Clone(in)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if got := rank(shuffled); !slices.Equal(got, want) {
			t.Fatalf("from %v: %v, want %v", projectNames(shuffled), got, want)
		}
	}
}

// projectBefore is a strict weak order over every kind of project, so a sort
// with it has one answer: nothing comes before itself, and two projects never
// come before each other.
func TestProjectBeforeIsAStrictOrder(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(3, 5))
	nan := math.NaN()
	random := func() Project {
		name, sessions := fmt.Sprintf("p%d", rng.IntN(4)), rng.IntN(3)
		var tokens *int64
		if rng.IntN(3) > 0 {
			tokens = tokenCount(int64(rng.IntN(3)))
		}
		var cost Cost
		switch rng.IntN(4) {
		case 0:
			cost = costOf(float64(rng.IntN(3)))
		case 1:
			cost = Cost{USD: &nan}
		case 2:
			cost = partialCost(float64(rng.IntN(3)), 1)
		}
		return Project{Name: name, Sessions: sessions, Tokens: tokens, Cost: cost}
	}
	var all []Project
	for range 40 {
		all = append(all, random())
	}
	for _, a := range all {
		if projectBefore(a, a) {
			t.Fatalf("%+v comes before itself", a)
		}
		for _, b := range all {
			if projectBefore(a, b) && projectBefore(b, a) {
				t.Fatalf("%+v and %+v each come before the other", a, b)
			}
			for _, c := range all {
				if projectBefore(a, b) && projectBefore(b, c) && !projectBefore(a, c) {
					t.Fatalf("%+v < %+v < %+v is not transitive", a, b, c)
				}
			}
		}
	}
}

// The list of projects is cut after it is ranked by spend, not before: five
// projects with the most tokens (all cache reads, which are cheap) do not push
// out two output-heavy ones that cost more. A project with no priced cost goes
// after every priced one, however many tokens it has.
func TestProjectsAreRankedBySpendBeforeTheCut(t *testing.T) {
	t.Parallel()
	var sessions []archive.Metadata
	at := day(time.September, 20, 10)
	for i := range 5 {
		sessions = append(sessions, meta(fmt.Sprintf("cache-%d", i), "claude", at, project(fmt.Sprintf("cache-%d", i)),
			modelTokens("claude-opus-5-5", 0, 0, 10_000_000+i*1_000_000, 0)))
	}
	for i := range 2 {
		sessions = append(sessions, meta(fmt.Sprintf("out-%d", i), "claude", at, project(fmt.Sprintf("output-%d", i)),
			modelTokens("claude-opus-5-5", 0, 1_000_000+i*100_000, 0, 0)))
	}
	// Twice the tokens of any of them, and no price for the model.
	sessions = append(sessions, meta("mystery", "codex", at, project("mystery"), modelTokens("no-such-model", 90_000_000, 0, 0, 0)))
	// A project whose sessions report no tokens at all.
	sessions = append(sessions, meta("cursor", "cursor", at, project("cursor-only")))

	all := Compute(sessions, Options{Now: now, Location: newYork, AllRows: true})
	wantAll := []string{"output-1", "output-0", "cache-4", "cache-3", "cache-2", "cache-1", "cache-0", "mystery", "cursor-only"}
	if got := projectNames(all.Projects); !slices.Equal(got, wantAll) {
		t.Fatalf("every project: %v, want %v", got, wantAll)
	}
	for n := 1; n <= len(wantAll)+1; n++ {
		got := Compute(sessions, Options{Now: now, Location: newYork, TopN: n})
		want := wantAll[:min(n, len(wantAll))]
		if !slices.Equal(projectNames(got.Projects), want) || got.TotalProjects != len(wantAll) {
			t.Errorf("TopN %d: %v of %d, want %v of %d", n, projectNames(got.Projects), got.TotalProjects, want, len(wantAll))
		}
	}
	// The default keeps the top five by spend: the two output-heavy projects are
	// in, though five others have more tokens.
	if got := projectNames(Compute(sessions, Options{Now: now, Location: newYork}).Projects); !slices.Equal(got, wantAll[:DefaultTopN]) {
		t.Errorf("default: %v, want %v", got, wantAll[:DefaultTopN])
	}
	// Grouping by project ranks the same way.
	grouped := Compute(sessions, Options{Now: now, Location: newYork, By: GroupProject}).Groups
	var keys []string
	for _, row := range grouped.Rows {
		keys = append(keys, row.Key)
	}
	if !slices.Equal(keys, wantAll) {
		t.Errorf("--by project rows: %v, want %v", keys, wantAll)
	}
}

// Whatever the archive holds, and whatever order its sessions arrive in, the
// top N projects are the N with the highest spend: none left out has a higher
// spend than one that is shown, a project with no priced cost is never before
// one that has it, the cut is a prefix of the full ranking, and the list does
// not depend on the input's order.
func TestTopProjectsBySpendNeverOmitADearerProject(t *testing.T) {
	t.Parallel()
	seeds := uint64(40)
	if testing.Short() {
		// Keep the same seeded archives and all assertions for each seed.
		seeds = 10
	}
	for seed := uint64(1); seed <= seeds; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*104729))
		sessions, _ := randomArchive(rng, 40+rng.IntN(160))
		base := Options{Now: time.Date(2026, time.October, 1, 0, 0, 0, 0, newYork), Location: newYork, Days: 400}
		full := base
		full.AllRows = true
		everything := Compute(sessions, full)
		checkProjectRanking(t, seed, everything.Projects)
		for _, n := range []int{1, 2, 3, 5, len(everything.Projects), len(everything.Projects) + 3} {
			o := base
			o.TopN = n
			got := Compute(sessions, o)
			if got.TotalProjects != len(everything.Projects) {
				t.Errorf("seed %d TopN %d: %d projects in all, %d in the total", seed, n, len(everything.Projects), got.TotalProjects)
			}
			kept := everything.Projects[:min(n, len(everything.Projects))]
			if mustJSON(t, got.Projects) != mustJSON(t, kept) {
				t.Fatalf("seed %d TopN %d: %v is not the top of %v", seed, n, projectNames(got.Projects), projectNames(everything.Projects))
			}
			for i, shown := range got.Projects {
				for _, left := range everything.Projects[len(got.Projects):] {
					sv, sok := spendOf(shown.Cost)
					lv, lok := spendOf(left.Cost)
					if (lok && !sok) || (lok && sok && lv > sv) {
						t.Errorf("seed %d TopN %d: %q (row %d, %v) is shown but %q (%v) is left out", seed, n, shown.Name, i, shown.Cost.USD, left.Name, left.Cost.USD)
					}
				}
			}
			for range 3 {
				shuffled := slices.Clone(sessions)
				rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
				if again := Compute(shuffled, o); mustJSON(t, again.Projects) != mustJSON(t, got.Projects) {
					t.Fatalf("seed %d TopN %d: a shuffled archive gives %v, not %v", seed, n, projectNames(again.Projects), projectNames(got.Projects))
				}
			}
		}
	}
}

// checkProjectRanking checks a whole list: spend never goes up down the list,
// the unpriced come after every priced project, and ties on spend go by
// tokens, then sessions, then name.
func checkProjectRanking(tb testing.TB, seed uint64, projects []Project) {
	tb.Helper()
	for i := 1; i < len(projects); i++ {
		prev, cur := projects[i-1], projects[i]
		pv, pok := spendOf(prev.Cost)
		cv, cok := spendOf(cur.Cost)
		switch {
		case !pok && cok:
			tb.Fatalf("seed %d: unpriced %q is before priced %q", seed, prev.Name, cur.Name)
		case pok && cok && cv > pv:
			tb.Fatalf("seed %d: %q ($%v) is before dearer %q ($%v)", seed, prev.Name, pv, cur.Name, cv)
		}
		if pok == cok && (!pok || pv == cv) && !projectBefore(prev, cur) {
			tb.Fatalf("seed %d: %+v and %+v tie on spend and are not in the tie-break order", seed, prev, cur)
		}
	}
}
