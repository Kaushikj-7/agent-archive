package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
)

// A label read can take as long as reading a transcript's windows, so the
// picker reads several at once. Every read here waits until
// handoffLabelReaders are in flight: reading them one after another would
// leave the bubble durably blocked.
func TestPickerModelReadsLabelsConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		inFlight, peak := 0, 0
		release, released := make(chan struct{}), false
		m := &pickerModel{source: "local", pageSize: 20, labelBudget: 20, activity: map[pickerIdentity]time.Time{}, label: func(ctx context.Context, r archive.SessionRegistration, _ time.Time) (archive.Metadata, bool, error) {
			mu.Lock()
			inFlight++
			peak = max(peak, inFlight)
			if inFlight == handoffLabelReaders && peak == inFlight && !released {
				released = true
				close(release)
			}
			mu.Unlock()
			<-release
			mu.Lock()
			inFlight--
			mu.Unlock()
			return archive.Metadata{SessionID: r.ArchiveSessionID, Harness: r.Harness, Title: "title " + r.ArchiveSessionID}, r.ArchiveSessionID != "local-03", nil
		}}
		for i := range 10 {
			r := archive.SessionRegistration{ArchiveSessionID: fmt.Sprintf("local-%02d", i), Harness: archive.Harness{Name: "codex"}}
			m.regs = append(m.regs, r)
			m.activity[pickerID("codex", r.ArchiveSessionID)] = time.Unix(int64(1000-i), 0)
		}
		if err := m.prepare(t.Context()); err != nil {
			t.Fatal(err)
		}
		v, err := m.First(t.Context(), sessionScope{All: true})
		if err != nil {
			t.Fatal(err)
		}
		var titles []string
		for _, c := range v.Rows {
			titles = append(titles, c.row.metadata.Title)
		}
		want := []string{"title local-00", "title local-01", "title local-02", "title local-04", "title local-05", "title local-06", "title local-07", "title local-08", "title local-09"}
		if !slices.Equal(titles, want) || v.LabelAttempts != 10 || peak != handoffLabelReaders {
			t.Fatalf("titles %q, attempts %d, peak %d", titles, v.LabelAttempts, peak)
		}
	})
}

func TestHandoffPickerLabelWantsStopsWhereRowsWould(t *testing.T) {
	row := func(id string, unbuilt bool) handoffPickerRow {
		var reg *archive.SessionRegistration
		if unbuilt {
			reg = &archive.SessionRegistration{ArchiveSessionID: id, Harness: archive.Harness{Name: "codex"}}
		}
		return handoffPickerRow{active: time.Unix(1000, 0), unbuilt: reg}
	}
	all := []handoffPickerRow{row("a", true), row("archived", false), row("cached", true), row("b", true), row("c", true), row("d", true)}
	cached := pickerLocalLabelKey(*all[2].unbuilt, all[2].active, "auto")
	ids := func(wants []labelWant) (out []string) {
		for _, w := range wants {
			out = append(out, w.reg.ArchiveSessionID)
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		picker handoffPicker
		limit  int
		want   []string
	}{
		{"limit", handoffPicker{source: "auto"}, 4, []string{"a", "cached", "b"}},
		{"cached without prompt", handoffPicker{source: "auto", localLabels: map[localLabelKey]localLabelResult{cached: {}}}, 4, []string{"a", "b", "c"}},
		{"cached with prompt", handoffPicker{source: "auto", localLabels: map[localLabelKey]localLabelResult{cached: {hasPrompt: true}}}, 4, []string{"a", "b"}},
		{"read limit", handoffPicker{source: "auto", readLimit: 2}, 10, []string{"a"}},
		{"search budget", handoffPicker{source: "auto", searchLabelBudget: 2}, 10, []string{"a", "cached"}},
	} {
		if got := ids(tc.picker.labelWants(all, tc.limit, 0, 0)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: wants %q, want %q", tc.name, got, tc.want)
		}
	}
	// After "a" turns out to have no prompt, rows plans again from the next
	// row, with nothing accepted and one search attempt spent.
	if got := ids((handoffPicker{source: "auto"}).labelWants(all[1:], 2, 0, 1)); !slices.Equal(got, []string{"cached"}) {
		t.Errorf("replan after a promptless row: %q", got)
	}
	if got := ids((handoffPicker{source: "auto", searchLabelBudget: 2}).labelWants(all[1:], 10, 0, 1)); !slices.Equal(got, []string{"cached"}) {
		t.Errorf("replan within the search budget: %q", got)
	}
}

func TestPickerModelLabelWantsStopsWhereArchiveOrderIsUnknown(t *testing.T) {
	local := func(id string, at int64) pickerCandidate {
		reg := &archive.SessionRegistration{ArchiveSessionID: id, Harness: archive.Harness{Name: "codex"}}
		return pickerCandidate{row: handoffPickerRow{active: time.Unix(at, 0), unbuilt: reg}}
	}
	remote := catalog.PickerItem{Row: catalog.PickerRow{Harness: "codex", SessionID: "remote", Activity: time.Unix(850, 0)}}
	ids := func(wants []labelWant) (out []string) {
		for _, w := range wants {
			out = append(out, w.reg.ArchiveSessionID)
		}
		return out
	}
	m := &pickerModel{source: "auto", labels: map[localLabelKey]localLabelResult{}}
	for _, tc := range []struct {
		name       string
		remoteDone bool
		need       int
		budget     int
		want       []string
	}{
		{"next archive page unread", false, 10, 10, []string{"new"}},
		{"archive read to the end", true, 10, 10, []string{"new", "mid", "old"}},
		{"budget", true, 10, 2, []string{"new", "mid"}},
		{"buffered row fills the page", true, 2, 10, []string{"new"}},
	} {
		st := &pickerScopeState{locals: []pickerCandidate{local("new", 900), local("mid", 800), local("old", 600)}, buffer: []catalog.PickerItem{remote}, remoteDone: tc.remoteDone}
		if got := ids(m.labelWants(st, tc.need, tc.budget)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: wants %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A later ensure fills every idle slot, even while earlier reads still run.
func TestLabelReadsFillIdleWorkersAfterMorePlanned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		inFlight, peak := 0, 0
		release := make(chan struct{})
		r := newLabelReads(t.Context(), func(ctx context.Context, reg archive.SessionRegistration, _ time.Time) (archive.Metadata, bool, error) {
			mu.Lock()
			inFlight++
			peak = max(peak, inFlight)
			mu.Unlock()
			<-release
			mu.Lock()
			inFlight--
			mu.Unlock()
			return archive.Metadata{}, true, nil
		})
		want := func(ids ...string) (out []labelWant) {
			for _, id := range ids {
				reg := archive.SessionRegistration{ArchiveSessionID: id, Harness: archive.Harness{Name: "codex"}}
				out = append(out, labelWant{key: pickerLocalLabelKey(reg, time.Time{}, "auto"), reg: reg})
			}
			return out
		}
		r.ensure(want("a", "b", "c"))
		synctest.Wait()
		r.ensure(want("d", "e"))
		synctest.Wait()
		mu.Lock()
		got := peak
		mu.Unlock()
		close(release)
		r.stop()
		if got != handoffLabelReaders {
			t.Fatalf("peak reads in flight %d, want %d", got, handoffLabelReaders)
		}
	})
}

// A Cursor database chat is read through a snapshot of the whole database, so
// the plans leave it for the row loop to read by itself, in turn.
func TestLabelWantsLeaveCursorChatsToTheRowLoop(t *testing.T) {
	cursor := &archive.SessionRegistration{ArchiveSessionID: "cursor", Harness: archive.Harness{Name: "cursor"}, SourceKind: archive.SourceKindCursorSQLite, SourceKey: "composer"}
	file := &archive.SessionRegistration{ArchiveSessionID: "file", Harness: archive.Harness{Name: "codex"}}
	rows := []handoffPickerRow{{active: time.Unix(900, 0), unbuilt: cursor}, {active: time.Unix(800, 0), unbuilt: file}}
	ids := func(wants []labelWant) (out []string) {
		for _, w := range wants {
			out = append(out, w.reg.ArchiveSessionID)
		}
		return out
	}
	if got := ids((handoffPicker{source: "auto"}).labelWants(rows, 10, 0, 0)); !slices.Equal(got, []string{"file"}) {
		t.Errorf("rows plan %q", got)
	}
	if got := ids((handoffPicker{source: "auto", searchLabelBudget: 1}).labelWants(rows, 10, 0, 0)); len(got) != 0 {
		t.Errorf("the Cursor read still spends the search budget: %q", got)
	}
	st := &pickerScopeState{locals: []pickerCandidate{{row: rows[0]}, {row: rows[1]}}, remoteDone: true}
	m := &pickerModel{source: "auto", labels: map[localLabelKey]localLabelResult{}}
	if got := ids(m.labelWants(st, 10, 10)); !slices.Equal(got, []string{"file"}) {
		t.Errorf("model plan %q", got)
	}
	if got := ids(m.labelWants(st, 10, 1)); len(got) != 0 {
		t.Errorf("the Cursor read still spends the model budget: %q", got)
	}
}

// Label reads that read a transcript whole, because its head holds no prompt,
// take turns while the picker reads several labels at once, so their read
// limits are not held in memory together. Each parse here holds a fake
// second: started together, all of them would be in flight at once.
func TestHandoffPickerReadsOneTranscriptWholeAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPickerFixture(t)
		parser := &peakParser{}
		var bindings []builtin.Integration
		for _, d := range productionAgents.Catalog().All() {
			binding, ok := productionAgents.Lookup(string(d.ID))
			if !ok {
				t.Fatal("missing builtin")
			}
			binding.Descriptor = agentmeta.Descriptor{ID: d.ID}
			if d.ID == agentmeta.Claude {
				parser.TranscriptParser = binding.Parser
				binding.Parser = parser
			}
			bindings = append(bindings, binding)
		}
		var err error
		f.env.Agents, err = builtin.New(agentmeta.Builtins(), bindings)
		must(t, err)
		var ids []string
		for i := range handoffLabelReaders {
			native := fmt.Sprintf("claude-late-%d", i)
			active := f.env.now().Add(time.Duration(3+i) * time.Hour)
			ids = append(ids, f.addSession(t, "claude", native, "placeholder", active))
			f.rewriteLargeClaudeTranscript(t, native, active, nil,
				[]string{`{"type":"user","uuid":"u1","sessionId":"` + native + `","timestamp":"2026-01-02T00:00:00Z","message":{"role":"user","content":"Prompt past the head"}}`}, nil)
		}
		out, errOut, code := runPicker(t, f.env, "q\n")
		for _, id := range ids {
			if line := pickerLine(t, out, id); code != 0 || !strings.Contains(line, "Prompt past the head") {
				t.Fatalf("code=%d stderr=%s\n%s", code, errOut, out)
			}
		}
		if parser.parses < handoffLabelReaders || parser.peak != 1 {
			t.Fatalf("%d whole reads, %d at once", parser.parses, parser.peak)
		}
	})
}

// peakParser counts the parses in flight, each held for a fake second.
type peakParser struct {
	agentapi.TranscriptParser
	mu       sync.Mutex
	inFlight int
	peak     int
	parses   int
}

func (p *peakParser) Parse(ctx context.Context, bundle archive.SourceBundle) (archive.Analysis, error) {
	p.mu.Lock()
	p.inFlight++
	p.parses++
	p.peak = max(p.peak, p.inFlight)
	p.mu.Unlock()
	time.Sleep(time.Second)
	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	return p.TranscriptParser.Parse(ctx, bundle)
}
