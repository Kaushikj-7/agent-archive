package codex

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestNativeFilterLeasePressureReleaseAndIndependentProgress(t *testing.T) {
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, strings.Repeat("ordinary visible text ", 100))
	budget := agentapi.NewNativeReadBudget(2 << 20)
	reservation := budget.Reservation()
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := budget.Charged()
	pressure := budget.Available() - 1024
	if !reservation.Reserve(pressure) {
		t.Fatal("pressure")
	}
	c := agentapi.FilterContext{Filename: filepath.Base(ref.Path), Limits: agentapi.ReadLimits{RawBytes: 4 << 20, RecordBytes: archive.MaxRecordBytes}}
	_, release, err := (Filter{}).FilterLeased(t.Context(), snap.Input(), c, budget)
	release()
	if !agentapi.HasFailure(err, agentapi.Limit) {
		t.Fatalf("pressure was not pending limit: %v", err)
	}
	used, _ := budget.Charged()
	if used != before+pressure {
		t.Fatalf("refusal leaked %d", used-before-pressure)
	}
	reservation.Release(pressure)
	out, release, err := (Filter{}).FilterLeased(t.Context(), snap.Input(), c, budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Records) != 2 {
		t.Fatalf("lost native output %d", len(out.Records))
	}
	used, peak := budget.Charged()
	if used <= before || peak > 2<<20 {
		t.Fatalf("output lifetime %d/%d base%d", used, peak, before)
	}
	if err := snap.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	// Output remains independently usable after both native owners close.
	if !strings.Contains(string(out.Records[1]), "ordinary visible text") {
		t.Fatal("borrowed output after native close")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(release)
	}
	wg.Wait()
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("all owners closed but %d remains", used)
	}
}

func TestNativeFilterLeaseCancellationAndHistoryMetadataOwnership(t *testing.T) {
	dir := t.TempDir()
	a, raw := historyFile(t, dir, threadA, threadA, 0, nil, "copied", "old own")
	end := len(strings.SplitAfter(string(raw), "\n")[0]) + len(strings.SplitAfter(string(raw), "\n")[1])
	b, _ := historyFile(t, dir, threadB, threadA, 2, map[string]any{"history_base": map[string]any{"thread_id": threadA, "end_ordinal_exclusive": 2, "end_byte_offset": end}}, "new own")
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &b, Candidates: []agentapi.SourceRef{a, b}, Revision: "stable"}, rollouts: map[string][]agentapi.SourceRef{threadA: {a}, threadB: {b}}}
	budget := agentapi.NewNativeReadBudget(4 << 20)
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: budget, CodexRollouts: lookup})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := pass.Read(t.Context(), a, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := budget.Charged()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, release, err := (Filter{}).FilterLeased(ctx, snap.Input(), agentapi.FilterContext{}, budget)
	release()
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	used, _ := budget.Charged()
	if used != before {
		t.Fatalf("cancel leaked %d", used-before)
	}
	out, release, err := (Filter{}).FilterLeased(t.Context(), snap.Input(), agentapi.FilterContext{}, budget)
	if err != nil {
		t.Fatal(err)
	}
	if out.History == nil || len(out.Ordinals) != len(out.Records) {
		t.Fatal("lost owned spans/ordinals")
	}
	cloned := *out.History
	cloned.Spans = append([]archive.HistorySpan(nil), out.History.Spans...)
	ordinals := append([]uint64(nil), out.Ordinals...)
	if err := snap.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pass.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cloned, *out.History) || !reflect.DeepEqual(ordinals, out.Ordinals) {
		t.Fatal("native close changed owned history")
	}
	release()
	release()
	used, _ = budget.Charged()
	if used != 0 {
		t.Fatalf("history lease leaked %d", used)
	}
}
