package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func waitPickerSearch(t *testing.T, l *sessionPicker, rows []listRow) {
	t.Helper()
	deadline := time.After(time.Second)
	for !l.searchRead && l.searchFailure == nil {
		l.prepareSearch(rows)
		select {
		case <-deadline:
			t.Fatal("search did not finish")
		default:
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBrowserViewDoesNotLoadSearchAndFailedSearchDoesNotUseLoadedRows(t *testing.T) {
	calls := 0
	rows := formatSessionRows([]archive.Metadata{{SessionID: "loaded", Title: "needle", Harness: archive.Harness{Name: "codex"}}}, listFormatOptions{})
	l := &sessionPicker{filter: "needle", searchContext: func(context.Context) ([]listRow, error) { calls++; return nil, errors.New("remote search unavailable") }}
	defer l.stopOperations()
	if v := l.view(rows, -1, true, listFormatOptions{}); calls != 0 || len(v.rows) != 0 || !strings.Contains(v.footer, "Loading complete search") {
		t.Fatalf("view performed search or substituted a partial universe: calls=%d view=%+v", calls, v)
	}
	waitPickerSearch(t, l, rows)
	if v := l.view(rows, -1, true, listFormatOptions{}); len(v.rows) != 0 || !strings.Contains(v.footer, "Search failed") {
		t.Fatalf("failed search displayed loaded-page matches: %+v", v)
	}
	l.closeFilter()
	l.searchContext = func(context.Context) ([]listRow, error) { return rows, nil }
	l.setFilter("needle")
	waitPickerSearch(t, l, rows)
	if v := l.view(rows, -1, true, listFormatOptions{}); len(v.rows) != 1 {
		t.Fatal("retry failed", v)
	}
}

func TestBrowserCancelsAndJoinsSearchBeforeScopeChanges(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	l := &sessionPicker{filter: "needle", searchContext: func(ctx context.Context) ([]listRow, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return []listRow{{SessionID: "stale"}}, nil
	}}
	l.prepareSearch(nil)
	<-started
	l.setFilter("")
	select {
	case <-stopped:
	default:
		t.Fatal("search worker was not joined")
	}
	l.searchContext = func(context.Context) ([]listRow, error) { return []listRow{{SessionID: "fresh"}}, nil }
	l.setFilter("new")
	waitPickerSearch(t, l, nil)
	if len(l.searched) != 1 || l.searched[0].SessionID != "fresh" {
		t.Fatal("stale completion committed", l.searched)
	}
	l.stopOperations()
}

func TestSearchRestoresUncappedFieldsForRowsAlreadyDisplayed(t *testing.T) {
	table := formatSessionRows([]archive.Metadata{{SessionID: "one", Title: "bounded", Harness: archive.Harness{Name: "codex"}}}, listFormatOptions{})
	full := formatSessionRows([]archive.Metadata{{SessionID: "one", Title: "bounded and a long older needle", Harness: archive.Harness{Name: "codex"}}}, listFormatOptions{})
	got, n := filterRows(withTableNumbers(table, full), parseSessionQuery("needle"))
	if len(got) != 1 || n != 1 || got[0].Index != table[0].Index {
		t.Fatal("full matcher fields lost", got, n)
	}
}

func TestBrowserOlderAcceptsQuitAndJoinsSlowRead(t *testing.T) {
	for _, mode := range []string{"keys", "lines"} {
		t.Run(mode, func(t *testing.T) {
			stopped := make(chan struct{})
			l := &sessionPicker{older: &browserLoadAction{Label: "Older sessions", LoadContext: func(ctx context.Context) (bool, error) { <-ctx.Done(); close(stopped); return false, ctx.Err() }}}
			if mode == "keys" {
				l.keys = startKeys(newFakeKeys(later(0, "q")))
				defer l.keys.close()
			}
			_, err := l.loadOlder(newPrompter(strings.NewReader("q\n"), io.Discard), io.Discard)
			if err != nil || !l.operationQuit {
				t.Fatal("slow read prevented quit", err)
			}
			select {
			case <-stopped:
			default:
				t.Fatal("provider worker outlived browser")
			}
		})
	}
}

func TestBrowserWorkerPanicReturnsToOwnerAfterJoining(t *testing.T) {
	l := &sessionPicker{filter: "needle", searchContext: func(context.Context) ([]listRow, error) { panic("synthetic read panic") }}
	defer func() {
		if recovered := recover(); recovered != "synthetic read panic" {
			t.Fatalf("worker panic not returned to owner: %v", recovered)
		}
		if l.operation != nil {
			t.Fatal("worker still owned after completion")
		}
		l.stopOperations()
	}()
	l.prepareSearch(nil)
	_ = l.waitSearch(nil)
}

func TestBrowserSearchCompletionDoesNotWaitForPartialPipeLine(t *testing.T) {
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer output.Close()
	gate := make(chan struct{})
	l := &sessionPicker{filter: "needle", searchContext: func(context.Context) ([]listRow, error) { <-gate; return nil, nil }}
	defer l.stopOperations()
	p := newPrompter(input, io.Discard)
	l.prepareSearch(nil)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		_, _ = output.Write([]byte("part"))
		time.Sleep(10 * time.Millisecond)
		close(gate)
	}()
	answer, redraw, err := l.readBrowserLine(p, "", nil)
	<-writerDone
	if err != nil || !redraw || answer != "" {
		t.Fatal("search completion waited for a complete input line", answer, redraw, err)
	}
	if _, err = output.Write([]byte("ial\n")); err != nil {
		t.Fatal(err)
	}
	answer, redraw, err = l.readBrowserLine(p, "", nil)
	if err != nil || redraw || answer != "partial" {
		t.Fatal("redraw lost pending input", answer, redraw, err)
	}
}
