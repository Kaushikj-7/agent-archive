package cli

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Each label read filters a whole transcript, so the picker reads several at
// once. Every read here waits until handoffLabelReaders are in flight: reading
// them one after another would leave the bubble durably blocked.
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
		if got := ids(tc.picker.labelWants(all, tc.limit)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: wants %q, want %q", tc.name, got, tc.want)
		}
	}
}
