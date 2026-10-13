package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestBoundedHandoffSearchCountsRejectedNativeAttemptsAndKeepsArchiveMatches(t *testing.T) {
	f := newPickerFixture(t)
	registrations, err := state.OpenReadOnly(f.home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	var empty archive.SessionRegistration
	for _, reg := range registrations {
		if reg.ArchiveSessionID == f.noPrompt {
			empty = reg
		}
	}
	if empty.ArchiveSessionID == "" {
		t.Fatal("missing synthetic zero-prompt registration")
	}
	regs := make([]archive.SessionRegistration, 100)
	for i := range regs {
		regs[i] = empty
		regs[i].ArchiveSessionID = fmt.Sprintf("empty-%03d", i)
	}
	p := handoffPicker{ctx: context.Background(), env: f.env, home: f.home, archiveRead: true, readLimit: handoffTitleScanLimit, searchLabelBudget: handoffTitleScanLimit, localLabels: map[localLabelKey]localLabelResult{}}
	archived := []archive.Metadata{{SessionID: "archive-match", Title: "archive needle", Harness: archive.Harness{Name: "codex"}, CapturedAt: f.env.now()}}
	rows, total, more := p.rows(regs, archived, math.MaxInt)
	if len(p.localLabels) != handoffTitleScanLimit || total != -1 || !more {
		t.Fatalf("rejected reads escaped budget or proved completeness: attempts=%d total=%d more=%v", len(p.localLabels), total, more)
	}
	got, matched := filterRows(formatHandoffRows(rows, listFormatOptions{}), parseSessionQuery("needle"))
	if len(got) != 1 || matched != 1 || got[0].SessionID != "archive-match" {
		t.Fatal("local attempt refusal hid the complete archive match", got, matched)
	}
	l := &sessionPicker{filter: "missing", searchRead: true, searchPartial: true, searched: formatHandoffRows(rows, listFormatOptions{})}
	v := l.view(nil, -1, true, listFormatOptions{})
	if strings.Contains(v.footer, "No sessions match") || !strings.Contains(v.footer, "0 observed sessions") || !strings.Contains(v.footer, "local eligibility may be unknown") {
		t.Fatal("unknown local eligibility became a complete empty search", v.footer)
	}
}

func TestBrowserNumericSearchWaitAllowsQuitAndJoinsProvider(t *testing.T) {
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	defer func() { _ = output.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started, stopped := make(chan struct{}), make(chan struct{})
	waiting := &numericSearchWaitWriter{ready: make(chan struct{})}
	writerDone := make(chan struct{})
	l := &sessionPicker{ctx: ctx, env: Env{}, searchContext: func(ctx context.Context) ([]listRow, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}}
	go func() {
		defer close(writerDone)
		_, _ = output.Write([]byte("needle\n"))
		<-started
		_, _ = output.Write([]byte("1\n"))
		select {
		case <-waiting.ready:
			_, _ = output.Write([]byte("q\n"))
		case <-ctx.Done():
		}

	}()
	rows := []listRow{{SessionID: "displayed", Index: 1}}
	_, picked, err := l.pickRows(newPrompter(input, waiting), io.Discard, rows, -1, true, listFormatOptions{}, "show")
	l.stopOperations()
	<-writerDone
	if err != nil || picked {
		t.Fatal("numeric answer blocked quit until provider deadline", picked, err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("search provider was not joined")
	}
}

type numericSearchWaitWriter struct {
	ready chan struct{}
	once  sync.Once
}

func (w *numericSearchWaitWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "Searching; Enter clears") {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

func TestBoundedHandoffAsyncSearchKeepsUnknownLabelsAndReportsActivityErrors(t *testing.T) {
	f := newPickerFixture(t)
	remote := scopedPickerCatalog(t, f.mem)
	ctx := catalog.WithReadView(t.Context())
	snapshot, err := catalog.OpenSnapshot(ctx, remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	archived, err := loadSessionsForBrowse(f.env, f.mem, listOptions{}, io.Discard, "handoff")
	if err != nil {
		t.Fatal(err)
	}
	registrations, err := state.OpenReadOnly(f.home).LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	var original archive.SessionRegistration
	for _, reg := range registrations {
		if reg.ArchiveSessionID == f.notUploaded {
			original = reg
		}
	}
	if original.ArchiveSessionID == "" {
		t.Fatal("missing synthetic native registration")
	}
	regs := make([]archive.SessionRegistration, 100)
	for i := range regs {
		regs[i] = original
		regs[i].ArchiveSessionID = fmt.Sprintf("unreadable-%03d", i)
	}
	env := f.env
	now := env.now()
	attempts := 0
	env.Now = func() time.Time {
		attempts++
		if attempts == 1 {
			must(t, os.Remove(filepath.Join(f.project, "native-new.jsonl")))
		}
		return now
	}
	model := &pickerModel{labels: map[localLabelKey]localLabelResult{}}
	picker := handoffPicker{ctx: ctx, env: env, home: f.home, source: "auto", archiveRead: true}
	search := boundedHandoffSearch(env, remote, snapshot, model, handoffOptions{source: "auto"}, sessionScope{All: true}, regs, listFormatOptions{}, picker, &pickerSearchState{archived: archived, loaded: true}, io.Discard)
	rows, err := search(ctx)
	if err != nil || attempts != handoffTitleScanLimit || len(model.labels) != 0 {
		t.Fatalf("failed labels escaped attempted-read budget or poisoned cache: attempts=%d labels=%d err=%v", attempts, len(model.labels), err)
	}
	matches, count := filterRows(rows, parseSessionQuery("Archived elsewhere"))
	if count != 1 || len(matches) != 1 || matches[0].SessionID != f.archiveOnly {
		t.Fatal("unknown native eligibility hid complete archive matcher", matches, count)
	}
	view := (&sessionPicker{filter: "missing", searchRead: true, searchPartial: true, searched: rows}).view(nil, -1, true, listFormatOptions{})
	if strings.Contains(view.footer, "No sessions match") || !strings.Contains(view.footer, "0 observed sessions") {
		t.Fatal("unknown eligibility became complete empty", view.footer)
	}
	bindings := []builtin.Integration{}
	calls := 0
	for _, descriptor := range productionAgents.Catalog().All() {
		binding, ok := productionAgents.Lookup(string(descriptor.ID))
		if !ok {
			t.Fatal(descriptor.ID)
		}
		binding.Descriptor = agentmeta.Descriptor{ID: descriptor.ID}
		if descriptor.ID == agentmeta.Codex {
			binding.Sources = activityFailureSource{SourceProvider: binding.Sources, err: errors.New("synthetic asynchronous activity failure"), calls: &calls}
		}
		bindings = append(bindings, binding)
	}
	registry, err := builtin.New(productionAgents.Catalog(), bindings)
	if err != nil {
		t.Fatal(err)
	}
	env.Agents = registry
	picker.env = env
	search = boundedHandoffSearch(env, remote, snapshot, model, handoffOptions{source: "auto"}, sessionScope{All: true}, regs, listFormatOptions{}, picker, &pickerSearchState{archived: archived, loaded: true}, io.Discard)
	rows, err = search(ctx)
	if err == nil || len(rows) != 0 || calls != 1 {
		t.Fatalf("activity failure looked like complete search or repeated inventory: rows=%d calls=%d err=%v", len(rows), calls, err)
	}
}
