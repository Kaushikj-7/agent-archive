package stats

import (
	"encoding/json"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

var prepareNow = time.Date(2026, 9, 20, 12, 0, 0, 0, newYork)

func statsJSON(t *testing.T, s Stats) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPreparedMatchesOriginalAccountingAcrossWindows(t *testing.T) {
	rng := rand.New(rand.NewPCG(17, 23))
	sessions, _ := randomArchive(rng, 800)
	// Include a cycle, orphan, duplicate, and missing IDs alongside the
	// generated archive's per-model, approximate and unknown token records.
	sessions = append(sessions,
		meta("cycle-a", "claude", prepareNow, parentOf("cycle-b")),
		meta("cycle-b", "claude", prepareNow, parentOf("cycle-a")),
		meta("orphan", "claude", prepareNow, parentOf("absent")),
		meta("", "cursor", prepareNow), sessions[0])
	for _, loc := range []*time.Location{time.UTC, newYork, time.FixedZone("offset", -7*3600)} {
		table := DefaultPriceTable()
		p := Prepare(sessions, PrepareOptions{Location: loc, PriceTable: table})
		shuffled := slices.Clone(sessions)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		q := Prepare(shuffled, PrepareOptions{Location: loc, PriceTable: table})
		for _, days := range []int{0, 7, 30, 90, 400} {
			for _, by := range []Grouping{GroupNone, GroupDay, GroupWeek, GroupMonth, GroupProject} {
				opts := Options{Now: prepareNow, Days: days, By: by, Location: loc, PriceTable: table, AllRows: true}
				want := statsJSON(t, legacyCompute(sessions, opts))
				if got := statsJSON(t, p.Compute(opts)); got != want {
					t.Fatalf("prepared differs for %s/%d/%s", loc, days, by)
				}
				if got := statsJSON(t, q.Compute(opts)); got != want {
					t.Fatalf("input order changes %s/%d/%s", loc, days, by)
				}
			}
		}
	}
}

func TestPreparedNativeChildAccountingAcrossWindows(t *testing.T) {
	root := meta("root", "codex", prepareNow, modelTokens("gpt-5", 100, 50, 0, 0))
	unknownParent := meta("unknown-parent", "codex", prepareNow, modelTokens("gpt-5", 20, 10, 0, 0))
	unknownParent.NativeChild = true
	missingParent := unknownParent
	missingParent.SessionID = "missing-parent"
	missingParent.ParentSessionID = "absent"
	resolved := unknownParent
	resolved.SessionID = "resolved"
	resolved.ParentSessionID = root.SessionID
	// A child's capture time must not move its accounting out of its root's window.
	resolved.CapturedAt = prepareNow.AddDate(0, 0, -40)
	for _, tc := range []struct {
		name     string
		sessions []archive.Metadata
		orphans  int
		children int
	}{
		{name: "ordinary root", sessions: []archive.Metadata{root}},
		{name: "unknown parent", sessions: []archive.Metadata{unknownParent}, orphans: 1},
		{name: "missing parent", sessions: []archive.Metadata{missingParent}, orphans: 1},
		{name: "resolved parent", sessions: []archive.Metadata{root, resolved}, children: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Prepare(tc.sessions, PrepareOptions{Location: newYork})
			shuffled := slices.Clone(tc.sessions)
			slices.Reverse(shuffled)
			q := Prepare(shuffled, PrepareOptions{Location: newYork})
			for _, days := range []int{1, 7, 30, 1} {
				opts := Options{Now: prepareNow, Days: days, Location: newYork, AllRows: true}
				want := legacyCompute(tc.sessions, opts)
				for _, result := range []struct {
					name  string
					stats Stats
				}{
					{"oracle", want},
					{"one-shot", Compute(tc.sessions, opts)},
					{"prepared", p.Compute(opts)},
					{"shuffled", q.Compute(opts)},
				} {
					cov := result.stats.Coverage
					if cov.Sessions != 1 || cov.OrphanSubagents != tc.orphans || cov.SubagentSessions != tc.children {
						t.Fatalf("%s/%d days: unexpected coverage %+v", result.name, days, cov)
					}
					if statsJSON(t, result.stats) != statsJSON(t, want) {
						t.Fatalf("%s/%d days differs from current accounting oracle", result.name, days)
					}
				}
			}
		})
	}
}

func TestPreparedOwnsInputsAndResults(t *testing.T) {
	sessions, _ := randomArchive(rand.New(rand.NewPCG(7, 7)), 80)
	sessions = append(sessions, meta("highlight", "claude", prepareNow, modelTokens("claude-opus-5-5", 1_000_000, 1_000_000, 0, 0), messages(2), compactions(1), skill("demo:skill"), mcp("demo-server", 3)))
	table := DefaultPriceTable()
	p := Prepare(sessions, PrepareOptions{Location: newYork, PriceTable: table})
	opts := Options{Now: prepareNow, Days: 400}
	want := statsJSON(t, p.Compute(opts))
	for i := range sessions {
		m := &sessions[i]
		for _, v := range []*int{m.Counts.Messages, m.Counts.Compactions, m.Counts.InputTokens, m.Counts.OutputTokens, m.Counts.Turns} {
			if v != nil {
				*v = 999999
			}
		}
		for j := range m.ModelTokens {
			if v := m.ModelTokens[j].InputTokens; v != nil {
				*v = 999999
			}
			m.ModelTokens[j].Model = "changed"
		}
		for j := range m.Models {
			for k := range m.Models[j].Attributes {
				m.Models[j].Attributes[k] = "changed"
			}
		}
		for j := range m.SkillsUsed {
			m.SkillsUsed[j].Name = "changed"
		}
		for j := range m.MCPCalls {
			m.MCPCalls[j].Name = "changed"
		}
		sessions[i] = archive.Metadata{}
	}
	for i := range table.Models {
		table.Models[i].InputPerMTok = 0
	}
	for i := range table.Sources {
		table.Sources[i] = "changed"
	}
	first := p.Compute(opts)
	for i := range first.Prices.Sources {
		first.Prices.Sources[i] = "changed result"
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 5 {
				if got := statsJSON(t, p.Compute(opts)); got != want {
					t.Error("prepared snapshot changed")
				}
			}
		})
	}
	wg.Wait()
}

func TestPreparedPriceAndTimezoneRequireNewSnapshot(t *testing.T) {
	sessions := []archive.Metadata{meta("boundary", "claude", time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC), modelTokens("claude-opus-5-5", 100, 50, 0, 0))}
	opts := Options{Now: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC), Days: 1}
	table := DefaultPriceTable()
	original := Prepare(sessions, PrepareOptions{Location: time.UTC, PriceTable: table})
	before := statsJSON(t, original.Compute(opts))
	for i := range table.Models {
		table.Models[i].InputPerMTok *= 2
	}
	changed := Prepare(sessions, PrepareOptions{Location: time.UTC, PriceTable: table})
	if statsJSON(t, changed.Compute(opts)) == before {
		t.Fatal("new prices were not prepared")
	}
	shifted := Prepare(sessions, PrepareOptions{Location: newYork, PriceTable: table})
	if shifted.Compute(opts).Coverage.Sessions != 0 {
		t.Fatal("new zone did not move boundary session")
	}
	if statsJSON(t, original.Compute(opts)) != before {
		t.Fatal("new preparation changed the old snapshot")
	}
}
