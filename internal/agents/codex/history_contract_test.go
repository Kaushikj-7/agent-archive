package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

type accountingScenario string

const (
	accountingScenarioCarriedTotal        accountingScenario = "carried_total"
	accountingScenarioResetTotal          accountingScenario = "reset_total"
	accountingScenarioMixed               accountingScenario = "mixed"
	accountingScenarioAmbiguousTurn       accountingScenario = "ambiguous_turn"
	accountingScenarioPerCall             accountingScenario = "per_call"
	accountingScenarioAbsentChildBoundary accountingScenario = "absent_child_boundary"
)

func appendHistoryRows(tb testing.TB, ref agentapi.SourceRef, start uint64, rows ...map[string]any) []byte {
	tb.Helper()
	f, e := os.OpenFile(ref.Path, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		tb.Fatal(e)
	}
	for i, row := range rows {
		row["ordinal"] = start + uint64(i)
		if e := json.NewEncoder(f).Encode(row); e != nil {
			tb.Fatal(e)
		}
	}
	if e := f.Close(); e != nil {
		tb.Fatal(e)
	}
	raw, e := os.ReadFile(ref.Path)
	if e != nil {
		tb.Fatal(e)
	}
	return raw
}

func TestNestedHistoryDropsRemapAndRefilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, _ := historyFile(t, dir, threadA, threadA, 0, nil, "ancestor")
	raw := appendHistoryRows(t, base, 2, map[string]any{"type": "unknown_native_type", "secret": "synthetic-private-value"})
	middle, midraw := historyFile(t, dir, threadB, threadB, 3, map[string]any{"forked_from_id": threadA, "forked_from_ordinal_exclusive": 3, "history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 3, EndByteOffset: uint64(len(raw))}}, "own before revert")
	leaf, _ := historyFile(t, dir, rolloutC, threadB, 5, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadB, EndOrdinal: 5, EndByteOffset: uint64(len(midraw))}}, "own after revert")
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}, threadB: {middle}}}
	b := historyRead(t, historyPass(t, dir, l), leaf)
	if len(b.History.Spans) != 3 || !slices.Equal(b.Ordinals, []uint64{0, 1, 3, 4, 5, 6}) {
		t.Fatalf("drop remapping: %+v %v", b.History, b.Ordinals)
	}
	encoded, e := archive.BuildCompressedSource(b)
	if e != nil {
		t.Fatal(e)
	}
	var rawSafe bytes.Buffer
	if e := archive.EncodeSource(&rawSafe, b); e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(rawSafe.Bytes(), []byte("synthetic-private-value")) {
		t.Fatal("unknown secret retained")
	}
	round, e := archive.ReadSourceBundle(bytes.NewReader(encoded.Bytes), archive.DecodeOptions{})
	if e != nil {
		t.Fatal(e)
	}
	// Future privacy rules may drop another previously retained record.
	round.NativeRecords[1] = map[string]any{"type": "unknown_native_type"}
	filtered, e := (Filter{}).Refilter(t.Context(), round, time.Time{})
	if e != nil {
		t.Fatal(e)
	}
	if !slices.Equal(filtered.Ordinals, []uint64{0, 3, 4, 5, 6}) || filtered.History.Spans[0].EndRecord != 1 || filtered.History.Spans[1].FirstRecord != 1 {
		t.Fatalf("refilter remapping: %+v %v", filtered.History, filtered.Ordinals)
	}
	a, e := (Parser{}).Parse(t.Context(), b)
	if e != nil || len(a.View.Turns) != 2 {
		t.Fatalf("nested own activity: %+v %v", a.View, e)
	}
}

func TestHistoryCumulativeAccountingDoesNotBecomeOwnedUsage(t *testing.T) {
	t.Parallel()
	for _, kind := range []accountingScenario{accountingScenarioCarriedTotal, accountingScenarioResetTotal, accountingScenarioMixed, accountingScenarioAmbiguousTurn, accountingScenarioPerCall, accountingScenarioAbsentChildBoundary} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			extra := map[string]any{"parent_thread_id": threadA, "subagent_history_start_ordinal": 4}
			if kind == accountingScenarioAbsentChildBoundary {
				delete(extra, "subagent_history_start_ordinal")
			}
			leaf, _ := historyFile(t, dir, threadB, threadB, 0, extra)
			rows := []map[string]any{
				{"type": "turn_context", "payload": map[string]any{"model": "synthetic-context-model"}},
				{"type": "token_usage_record", "payload": map[string]any{"usage": map[string]any{"input_tokens": 100}}},
				{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "shell", "call_id": "inherited", "arguments": "{}"}},
			}
			field := "total_token_usage"
			value := 105
			if kind == accountingScenarioResetTotal {
				value = 5
			}
			if kind == accountingScenarioPerCall || kind == accountingScenarioAbsentChildBoundary {
				field = "usage"
				value = 5
			}
			if kind == accountingScenarioAmbiguousTurn {
				field = "turn_token_usage"
			}
			rows = append(rows, map[string]any{"type": "token_usage_record", "payload": map[string]any{field: map[string]any{"input_tokens": value}}})
			if kind == accountingScenarioMixed {
				rows = append(rows, map[string]any{"type": "token_usage_record", "payload": map[string]any{"usage": map[string]any{"input_tokens": 5}}})
			}
			appendHistoryRows(t, leaf, 1, rows...)
			l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}}
			b := historyRead(t, historyPass(t, dir, l), leaf)
			a, e := (Parser{}).Parse(t.Context(), b)
			if e != nil {
				t.Fatal(e)
			}
			want := 0
			if kind == accountingScenarioMixed || kind == accountingScenarioPerCall {
				want = 5
			}
			if kind == accountingScenarioAbsentChildBoundary {
				want = 105
			}
			if want == 0 && a.View.Tokens.Input != nil || want > 0 && (a.View.Tokens.Input == nil || *a.View.Tokens.Input != want) {
				t.Fatalf("own tokens: %+v want %d", a.View.Tokens, want)
			}
			unknown := kind != accountingScenarioPerCall && kind != accountingScenarioAbsentChildBoundary
			if a.Facts.TokenScopeUnknown != unknown {
				t.Fatalf("scope gap=%v want %v", a.Facts.TokenScopeUnknown, unknown)
			}
			if want > 0 && (len(a.View.ModelTokens) != 1 || a.View.ModelTokens[0].Model != "synthetic-context-model") {
				t.Fatalf("inherited model context: %+v", a.View.ModelTokens)
			}
			tools := 0
			if kind == accountingScenarioAbsentChildBoundary {
				tools = 1
			}
			if len(a.View.ToolCalls) != tools {
				t.Fatalf("inherited tool activity counted: %d", len(a.View.ToolCalls))
			}
			source, e := archive.BuildCompressedSource(b)
			if e != nil {
				t.Fatal(e)
			}
			key, e := archive.SourceObjectKey(b, source.SHA256)
			if e != nil {
				t.Fatal(e)
			}
			metadata, e := archive.BuildMetadataWithAnalysis(b, a, nil, "synthetic-machine", b.Capture.CapturedAt, b.Capture.CapturedAt, archive.SourceReference{Key: key, SHA256: source.SHA256, CompressedBytes: len(source.Bytes)}, archive.ParserInfo{Name: "codex", Version: (Parser{}).Version()})
			if e != nil {
				t.Fatal(e)
			}
			found := slices.ContainsFunc(metadata.CaptureGaps, func(g archive.CaptureGap) bool { return g.Code == "history_cumulative_tokens_unavailable" })
			if found != unknown {
				t.Fatalf("metadata token gap %v want %v", found, unknown)
			}
		})
	}
}

func TestHistorySelectionRejectsDisconnectedCycle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seed, _ := historyFile(t, dir, threadA, threadA, 0, nil, "tip")
	cycle, _ := historyFile(t, dir, rolloutC, threadA, 1, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: rolloutC, EndOrdinal: 1, EndByteOffset: 1}})
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Candidates: []agentapi.SourceRef{seed, cycle}}, rollouts: map[string][]agentapi.SourceRef{rolloutC: {cycle}}}
	if _, e := historyPass(t, dir, l).Read(t.Context(), seed, agentapi.ReadLimits{}); e == nil {
		t.Fatal("accepted disconnected candidate graph")
	}
}

func TestHistoryDefersValidUnterminatedLeafRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	leaf, raw := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA}, "complete", "unfinished newline")
	if e := os.WriteFile(leaf.Path, raw[:len(raw)-1], 0600); e != nil {
		t.Fatal(e)
	}
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}}
	b := historyRead(t, historyPass(t, dir, l), leaf)
	if len(b.NativeRecords) != 2 {
		t.Fatalf("unterminated record captured: %d", len(b.NativeRecords))
	}
}

func TestHistoryCacheRejectsReplacedLocator(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	leaf, raw := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA}, "original")
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}}
	p := historyPass(t, dir, l)
	s, e := p.Read(t.Context(), leaf, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	if e := os.Rename(leaf.Path, leaf.Path+".old"); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(leaf.Path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := p.Read(t.Context(), leaf, agentapi.ReadLimits{}); agentapi.Failure(e) != agentapi.Changed {
		t.Fatalf("cached replacement: %v", e)
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
}

func TestHistoryBudgetRejectsTooManyDependencies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l := &historyLookup{rollouts: map[string][]agentapi.SourceRef{}}
	var leaf agentapi.SourceRef
	var raw []byte
	var previous string
	for i := range 65 {
		id := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i+1)
		extra := map[string]any{}
		if i > 0 {
			extra["history_base"] = codexmeta.CodexHistoryPosition{RolloutID: previous, EndOrdinal: uint64(i), EndByteOffset: uint64(len(raw))}
		}
		leaf, raw = historyFile(t, dir, id, threadA, uint64(i), extra)
		l.rollouts[id] = []agentapi.SourceRef{leaf}
		previous = id
	}
	l.thread.Current = &leaf
	p := historyPass(t, dir, l)
	if _, e := p.Read(t.Context(), leaf, agentapi.ReadLimits{}); agentapi.Failure(e) != agentapi.Limit {
		t.Fatalf("dependency cap: %v", e)
	}
	if e := p.Close(); e != nil {
		t.Fatal(e)
	}
	state := p.(*relatedSourcePass)
	if state.bytes != 0 || len(state.files) != 0 {
		t.Fatal("budget retained after error close")
	}
}

func TestOrdinaryAdmissionUsesCapturedHandle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, "own")
	p, e := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = p.Close() }()
	s, e := p.Read(t.Context(), ref, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	v, ok := s.(agentapi.SourceAdmissionValidator)
	if !ok {
		t.Fatal("ordinary admission missing")
	}
	if e := v.ValidateAdmission(t.Context(), agentapi.SourceAdmission{NativeID: threadA, Cwd: dir}); e != nil {
		t.Fatal(e)
	}
	if e := v.ValidateAdmission(t.Context(), agentapi.SourceAdmission{NativeID: threadB, Cwd: dir}); agentapi.Failure(e) != agentapi.Unsafe {
		t.Fatalf("wrong admission accepted: %v", e)
	}
	_ = s.Close()
	if e := v.ValidateAdmission(t.Context(), agentapi.SourceAdmission{NativeID: threadA}); !errors.Is(e, agentapi.ErrClosed) {
		t.Fatalf("closed admission: %v", e)
	}
}

func TestSharedPrefixCacheStillProvesNativeRewrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "before")
	leaf, _ := historyFile(t, dir, threadB, threadB, 2, map[string]any{"parent_thread_id": threadA, "history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "own")
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	p := historyPass(t, dir, lookup)
	historyRead(t, p, leaf)
	s, e := p.Read(t.Context(), leaf, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	if p.(*relatedSourcePass).cacheHits != 1 {
		t.Fatal("validated shared prefix not reused")
	}
	changed := bytes.Replace(raw, []byte("before"), []byte("mutate"), 1)
	if e := os.WriteFile(base.Path, changed, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := (Filter{}).Filter(t.Context(), s.Input(), agentapi.FilterContext{}); agentapi.Failure(e) != agentapi.Changed {
		t.Fatalf("cached rewrite escaped native proof: %v", e)
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	if e := p.Close(); e != nil {
		t.Fatal(e)
	}
	if p.(*relatedSourcePass).bytes != 0 {
		t.Fatal("charged cache survived close")
	}
}

func TestCompleteConnectedLineageSelectsUniqueTip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "before")
	leaf, _ := historyFile(t, dir, rolloutC, threadA, 2, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "after")
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Candidates: []agentapi.SourceRef{leaf, base}}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	b := historyRead(t, historyPass(t, dir, lookup), base)
	if b.History.ActiveRolloutID != rolloutC || len(b.NativeRecords) != 4 {
		t.Fatalf("unique tip selection: %+v", b.History)
	}
}

// A retained coverage proof can lack a segment created after it, so without
// a current locator its candidates cannot choose the tip whose content is
// published. Signature may still use them to recognize an unchanged source,
// and a current locator chooses the tip on its own.
func TestRetainedProofSelectsNoLineageTipForRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "before")
	leaf, _ := historyFile(t, dir, rolloutC, threadA, 2, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "after")
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Complete: true, Retained: true, Candidates: []agentapi.SourceRef{leaf, base}}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	p := historyPass(t, dir, lookup)
	if _, e := p.Signature(t.Context(), base); e != nil {
		t.Fatalf("signature refused a retained proof: %v", e)
	}
	if s, e := p.Read(t.Context(), base, agentapi.ReadLimits{}); e == nil {
		_ = s.Close()
		t.Fatal("read chose a lineage tip from a retained proof")
	} else if agentapi.Failure(e) != agentapi.Unavailable {
		t.Fatalf("retained proof refusal %v", e)
	}
	lookup.thread.Current = &leaf
	if b := historyRead(t, historyPass(t, dir, lookup), base); b.History.ActiveRolloutID != rolloutC {
		t.Fatalf("current locator with a retained proof: %+v", b.History)
	}
}

func TestHistoryRejectsActualPhysicalCycle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	offset := uint64(1)
	var leaf, base agentapi.SourceRef
	for range 5 {
		var raw []byte
		leaf, raw = historyFile(t, dir, rolloutC, threadA, 1, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: rolloutD, EndOrdinal: 1, EndByteOffset: offset}})
		base, _ = historyFile(t, dir, rolloutD, threadA, 1, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: rolloutC, EndOrdinal: 1, EndByteOffset: offset}})
		if offset == uint64(len(raw)) {
			break
		}
		offset = uint64(len(raw))
	}
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}, rollouts: map[string][]agentapi.SourceRef{rolloutC: {leaf}, rolloutD: {base}}}
	p := historyPass(t, dir, lookup)
	if _, e := p.Read(t.Context(), leaf, agentapi.ReadLimits{}); e != nil {
		var classified *agentapi.SourceError
		if !errors.As(e, &classified) || classified.Kind != agentapi.Unsafe || !strings.Contains(classified.Unwrap().Error(), "cycle") {
			t.Fatalf("physical cycle: %v", e)
		}
	} else {
		t.Fatal("physical cycle accepted")
	}
	if p.(*relatedSourcePass).bytes != 0 {
		t.Fatal("failed graph retained unused dependencies")
	}
}

func TestRelatedPassPreservesCleanupFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	leaf, _ := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA}, "own")
	fault := errors.New("synthetic close failure")
	files := &historyIO{closeErr: fault}
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}}
	p, e := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{Files: files, CodexRollouts: lookup})
	if e != nil {
		t.Fatal(e)
	}
	historyRead(t, p, leaf)
	for range 2 {
		if e := p.Close(); !errors.Is(e, fault) || !agentapi.HasFailure(e, agentapi.Cleanup) {
			t.Fatalf("lost terminal cleanup failure: %v", e)
		}
	}
	if files.closes != 1 || p.(*relatedSourcePass).bytes != 0 {
		t.Fatalf("cleanup retry/resource leak: %+v", files)
	}
}

func TestRelatedRecordLimitIsTypedAndReleasesBudget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	leaf, _ := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA}, "own")
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}}
	p := historyPass(t, dir, lookup)
	if _, e := p.Read(t.Context(), leaf, agentapi.ReadLimits{RecordBytes: 64}); agentapi.Failure(e) != agentapi.Limit || !errors.Is(e, archive.ErrRecordTooLarge) {
		t.Fatalf("record limit classification: %v", e)
	}
	if p.(*relatedSourcePass).bytes != 0 {
		t.Fatal("record failure retained budget")
	}
	historyRead(t, p, leaf)
}

func TestRelatedMillionRecordLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("full million-record physical history limit; daily and exact candidates")
	}
	t.Parallel()
	dir := t.TempDir()
	leaf, _ := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA, "history_mode": "legacy"})
	f, e := os.OpenFile(leaf.Path, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.Write(bytes.Repeat([]byte("{\"type\":\"event_msg\"}\n"), archive.MaxHistoryRecords))
	if e = errors.Join(e, f.Close()); e != nil {
		t.Fatal(e)
	}
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}}
	p := historyPass(t, dir, lookup)
	if _, e := p.Read(t.Context(), leaf, agentapi.ReadLimits{}); agentapi.Failure(e) != agentapi.Limit {
		t.Fatalf("aggregate record cap: %v", e)
	}
	if p.(*relatedSourcePass).bytes != 0 {
		t.Fatal("record count failure retained budget")
	}
}

// A warmed prefix's count still passes through the aggregate production cap.
// Physical streaming to that cap remains in TestRelatedMillionRecordLimit.
func TestRelatedCachedRecordCountLimitIsTypedAndReleasesBudget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "before")
	leaf, _ := historyFile(t, dir, threadB, threadB, 2, map[string]any{"parent_thread_id": threadA, "history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "own")
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	p := historyPass(t, dir, lookup)
	historyRead(t, p, leaf)
	pass := p.(*relatedSourcePass)
	facts := pass.files[base.Path].validated
	if facts == nil || facts.records != 2 {
		t.Fatal("small prefix was not validated")
	}
	facts.records = archive.MaxHistoryRecords + 1
	_, err := p.Read(t.Context(), leaf, agentapi.ReadLimits{})
	var sourceErr *agentapi.SourceError
	if agentapi.Failure(err) != agentapi.Limit || !errors.As(err, &sourceErr) || sourceErr.Cause != agentapi.CauseHistoryRecordLimit {
		t.Fatalf("cached count failure classification: %v", err)
	}
	if pass.bytes != 0 {
		t.Fatal("record count failure retained budget")
	}
	historyRead(t, p, leaf)
}

func TestOrdinaryCurrentLookupKeepsLegacySourceAndRevalidates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, "own")
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &ref, Revision: "one"}}
	p := historyPass(t, dir, lookup)
	observed, err := p.Signature(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	s, err := p.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.Observation().Signature != observed.Signature {
		t.Fatal("read and scan signatures disagree")
	}
	out, err := (Filter{}).Filter(t.Context(), s.Input(), agentapi.FilterContext{})
	if err != nil || out.History != nil || len(out.Records) != 2 {
		t.Fatalf("ordinary capture: %+v %v", out, err)
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	reg := archive.SessionRegistration{ArchiveSessionID: "synthetic", NativeSessionID: threadA, ProjectID: "project", ProjectRoot: dir, Harness: archive.Harness{Name: "codex"}, SessionStartedAt: at}
	bundle, err := archive.NewSourceBundle(reg, Filter{}, out, at, nil)
	if err != nil || bundle.SchemaVersion != archive.SourceSchemaVersion {
		t.Fatalf("ordinary schema: %d %v", bundle.SchemaVersion, err)
	}
	if err := archive.CheckHistoryMutation(bundle, archive.Metadata{}); err != nil {
		t.Fatal(err)
	}
	lookup.changed = true
	if err := s.Input().File.Check(); agentapi.Failure(err) != agentapi.Changed {
		t.Fatalf("current locator change: %v", err)
	}
	if err := s.(agentapi.SourceAdmissionValidator).ValidateAdmission(t.Context(), agentapi.SourceAdmission{NativeID: threadA}); agentapi.Failure(err) != agentapi.Changed {
		t.Fatalf("admission locator change: %v", err)
	}
}

func TestHistoryEmptyBasePrefixIsValidOnlyAtOrdinalZero(t *testing.T) {
	t.Parallel()
	for _, ordinal := range []uint64{0, 1} {
		t.Run(strconv.FormatUint(ordinal, 10), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			base, _ := historyFile(t, dir, threadA, threadA, 0, nil, "discarded")
			leaf, _ := historyFile(t, dir, rolloutC, threadA, ordinal, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: ordinal, EndByteOffset: 0}}, "own")
			lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
			p := historyPass(t, dir, lookup)
			if ordinal != 0 {
				if _, err := p.Read(t.Context(), leaf, agentapi.ReadLimits{}); agentapi.Failure(err) != agentapi.Unsafe {
					t.Fatalf("contradictory empty boundary: %v", err)
				}
				return
			}
			bundle := historyRead(t, p, leaf)
			if len(bundle.NativeRecords) != 2 || bundle.History.Spans[0].EndRecord != 0 {
				t.Fatalf("empty prefix: %+v", bundle.History)
			}
			parsed, err := (Parser{}).Parse(t.Context(), bundle)
			if err != nil || len(parsed.View.Turns) != 1 {
				t.Fatalf("empty revert ownership: %+v %v", parsed.View, err)
			}
		})
	}
}

func TestOrdinaryFailedReadReleasesCachedResources(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, "own")
	p, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if _, err := p.Read(t.Context(), ref, agentapi.ReadLimits{RawBytes: 1}); agentapi.Failure(err) != agentapi.Limit {
		t.Fatalf("ordinary limit: %v", err)
	}
	owned := p.(*relatedSourcePass)
	if owned.bytes != 0 || len(owned.files) != 0 {
		t.Fatalf("failed ordinary read retained %d bytes and %d files", owned.bytes, len(owned.files))
	}
}
