package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

const threadA = "11111111-1111-4111-8111-111111111111"

const threadB = "22222222-2222-4222-8222-222222222222"

const rolloutC = "33333333-3333-4333-8333-333333333333"

const rolloutD = "44444444-4444-4444-8444-444444444444"

type historyLookup struct {
	thread   agentapi.CodexRolloutSet
	rollouts map[string][]agentapi.SourceRef
	changed  bool
	reads    int
}

type ownershipScenario string

type failureScenario string

type mutationScenario string

const (
	historyScenarioFork            ownershipScenario = "fork"
	historyScenarioRevert          ownershipScenario = "revert"
	historyScenarioChildAbsent     ownershipScenario = "child_absent"
	historyScenarioChildZero       ownershipScenario = "child_zero"
	historyScenarioChildCopied     ownershipScenario = "child_copied"
	historyScenarioCompeting       failureScenario   = "competing"
	historyScenarioMissingBase     failureScenario   = "missing_base"
	historyScenarioSplitPrefix     failureScenario   = "split_prefix"
	historyScenarioOrdinalMismatch failureScenario   = "ordinal_mismatch"
	historyScenarioCycle           failureScenario   = "cycle"
	historyScenarioOutside         failureScenario   = "outside"
	historyScenarioStaleCurrent    failureScenario   = "stale_current"
	historyScenarioAppend          mutationScenario  = "append"
	historyScenarioRewrite         mutationScenario  = "rewrite"
	historyScenarioReplace         mutationScenario  = "replace"
	historyScenarioLocatorChange   mutationScenario  = "locator_change"
)

func (l *historyLookup) Thread(context.Context, string) (agentapi.CodexRolloutSet, error) {
	l.reads++
	return l.thread, nil
}

func (l *historyLookup) Rollout(_ context.Context, id string) ([]agentapi.SourceRef, error) {
	return l.rollouts[id], nil
}

func (l *historyLookup) Check(context.Context, string, string) error {
	if l.changed {
		return agentapi.Wrap(agentapi.Changed, transcriptio.ErrChanged)
	}
	return nil
}

func historyFile(tb testing.TB, dir, id, thread string, start uint64, extra map[string]any, texts ...string) (agentapi.SourceRef, []byte) {
	tb.Helper()
	meta := map[string]any{"id": thread, "cwd": dir, "timestamp": "2026-10-01T12:00:00Z", "cli_version": "0.160.0", "originator": "codex_cli_rs", "source": "cli", "history_mode": "paginated"}
	maps.Copy(meta, extra)
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	if err := enc.Encode(map[string]any{"type": "session_meta", "ordinal": start, "payload": meta}); err != nil {
		tb.Fatal(err)
	}
	for i, text := range texts {
		if err := enc.Encode(map[string]any{"type": "response_item", "ordinal": start + uint64(i) + 1, "timestamp": "2026-10-01T12:01:00Z", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}); err != nil {
			tb.Fatal(err)
		}
	}
	ref := agentapi.SourceRef{Path: filepath.Join(dir, "rollout-2026-10-01T12-00-00-"+id+".jsonl")}
	if err := os.WriteFile(ref.Path, out.Bytes(), 0600); err != nil {
		tb.Fatal(err)
	}
	return ref, out.Bytes()
}

func historyPass(tb testing.TB, root string, l *historyLookup) agentapi.SourcePass {
	tb.Helper()
	p, e := (SourceProvider{}).OpenPass(context.Background(), agentapi.SourceEnvironment{CodexRollouts: l, Policy: transcriptio.OpenPolicy{Root: root, RejectSymlinks: true}})
	if e != nil {
		tb.Fatal(e)
	}
	tb.Cleanup(func() {
		if e := p.Close(); e != nil {
			tb.Error(e)
		}
	})
	return p
}

func historyRead(tb testing.TB, p agentapi.SourcePass, ref agentapi.SourceRef) archive.SourceBundle {
	tb.Helper()
	s, e := p.Read(context.Background(), ref, agentapi.ReadLimits{})
	if e != nil {
		tb.Fatal(e)
	}
	defer func() {
		if e := s.Close(); e != nil {
			tb.Error(e)
		}
	}()
	f, e := (Filter{}).Filter(context.Background(), s.Input(), agentapi.FilterContext{})
	if e != nil {
		tb.Fatal(e)
	}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	reg := archive.SessionRegistration{ArchiveSessionID: "synthetic", NativeSessionID: f.History.ThreadID, ProjectID: "project", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "codex"}, TranscriptPath: ref.Path, SessionStartedAt: at, RegisteredAt: at}
	b, e := archive.NewSourceBundle(reg, Filter{}, f, at, nil)
	if e != nil {
		tb.Fatal(e)
	}
	if e := b.ValidateHistory(); e != nil {
		tb.Fatal(e)
	}
	return b
}

func TestRelatedHistoryOwnershipAndRoundTrip(t *testing.T) {
	t.Parallel()
	for _, kind := range []ownershipScenario{historyScenarioFork, historyScenarioRevert, historyScenarioChildAbsent, historyScenarioChildZero, historyScenarioChildCopied} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "ancestor")
			ownThread := threadB
			extra := map[string]any{}
			switch kind {
			case historyScenarioFork:
				extra["forked_from_id"] = threadA
				extra["forked_from_ordinal_exclusive"] = 2
			case historyScenarioRevert:
				ownThread = threadA
			case historyScenarioChildAbsent, historyScenarioChildZero, historyScenarioChildCopied:
				extra["parent_thread_id"] = threadA
			}
			var leaf agentapi.SourceRef
			if kind == historyScenarioChildAbsent || kind == historyScenarioChildZero || kind == historyScenarioChildCopied {
				if kind == historyScenarioChildZero {
					extra["subagent_history_start_ordinal"] = 0
				}
				if kind == historyScenarioChildCopied {
					extra["subagent_history_start_ordinal"] = 2
				}
				leaf, _ = historyFile(t, dir, rolloutC, ownThread, 0, extra, "copied-or-owned", "own")
			} else {
				extra["history_base"] = codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}
				leaf, _ = historyFile(t, dir, rolloutC, ownThread, 2, extra, "own")
			}
			l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
			b := historyRead(t, historyPass(t, dir, l), leaf)
			parsed, e := (Parser{}).Parse(t.Context(), b)
			if e != nil {
				t.Fatal(e)
			}
			want := 1
			if kind == historyScenarioRevert || kind == historyScenarioChildAbsent || kind == historyScenarioChildZero {
				want = 2
			}
			if len(parsed.View.Turns) != want {
				t.Fatalf("turns %d want %d: %+v", len(parsed.View.Turns), want, parsed.View.Turns)
			}
			compressed, e := archive.BuildCompressedSource(b)
			if e != nil {
				t.Fatal(e)
			}
			round, e := archive.ReadSourceBundle(bytes.NewReader(compressed.Bytes), archive.DecodeOptions{})
			if e != nil {
				t.Fatal(e)
			}
			filtered, e := (Filter{}).Refilter(t.Context(), round, time.Time{})
			if e != nil {
				t.Fatal(e)
			}
			if len(filtered.Ordinals) != len(b.Ordinals) || len(filtered.History.Spans) != len(b.History.Spans) {
				t.Fatal("refilter lost spans")
			}
		})
	}
}

func TestRelatedSelectionAndDependencyFailures(t *testing.T) {
	t.Parallel()
	for _, kind := range []failureScenario{historyScenarioCompeting, historyScenarioMissingBase, historyScenarioSplitPrefix, historyScenarioOrdinalMismatch, historyScenarioCycle, historyScenarioOutside, historyScenarioStaleCurrent} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "before")
			boundary := uint64(len(raw))
			ordinal := uint64(2)
			baseID := threadA
			if kind == historyScenarioSplitPrefix {
				boundary--
			}
			if kind == historyScenarioOrdinalMismatch {
				ordinal = 3
			}
			if kind == historyScenarioCycle {
				baseID = rolloutC
			}
			leaf, _ := historyFile(t, dir, rolloutC, threadA, ordinal, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: baseID, EndOrdinal: ordinal, EndByteOffset: boundary}}, "after")
			l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
			p := historyPass(t, dir, l)
			switch kind {
			case historyScenarioCompeting:
				other, _ := historyFile(t, dir, rolloutD, threadA, 0, nil, "other")
				l.thread = agentapi.CodexRolloutSet{Candidates: []agentapi.SourceRef{leaf, other}}
			case historyScenarioMissingBase:
				l.rollouts = nil
			case historyScenarioOutside:
				outside, _ := historyFile(t, t.TempDir(), rolloutD, threadA, 0, nil, "outside")
				l.thread.Current = &outside
			case historyScenarioStaleCurrent:
				other, _ := historyFile(t, dir, rolloutD, threadB, 0, nil, "wrong")
				l.thread.Current = &other
			case historyScenarioSplitPrefix, historyScenarioOrdinalMismatch, historyScenarioCycle:
				// These cases already alter the physical history above.
			}
			s, e := p.Read(t.Context(), leaf, agentapi.ReadLimits{})
			if e == nil {
				_ = s.Close()
				t.Fatal("accepted invalid history")
			}
		})
	}
}

func TestRelatedSignatureChangesWithAncestorAndCurrentLocator(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, raw := historyFile(t, dir, threadA, threadA, 0, nil, "before")
	leaf, _ := historyFile(t, dir, rolloutC, threadA, 2, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadA, EndOrdinal: 2, EndByteOffset: uint64(len(raw))}}, "after")
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &base, Revision: "one"}, rollouts: map[string][]agentapi.SourceRef{threadA: {base}}}
	p := historyPass(t, dir, l)
	before, e := p.Signature(t.Context(), base)
	if e != nil {
		t.Fatal(e)
	}
	l.thread.Current = &leaf
	l.thread.Revision = "two"
	after, e := p.Signature(t.Context(), base)
	if e != nil {
		t.Fatal(e)
	}
	if before.Signature == after.Signature {
		t.Fatal("ordinary seed bypassed current locator")
	}
	f, e := os.OpenFile(base.Path, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, e = f.WriteString("\n")
	if e = errors.Join(e, f.Close()); e != nil {
		t.Fatal(e)
	}
	changed, e := p.Signature(t.Context(), base)
	if e != nil {
		t.Fatal(e)
	}
	if changed.Signature == after.Signature {
		t.Fatal("ancestor append did not invalidate signature")
	}
}

func TestRelatedCapturedPrefixAcceptsAppendAndRejectsRewrite(t *testing.T) {
	t.Parallel()
	for _, kind := range []mutationScenario{historyScenarioAppend, historyScenarioRewrite, historyScenarioReplace, historyScenarioLocatorChange} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			leaf, raw := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA}, "own")
			l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}}
			p := historyPass(t, dir, l)
			s, e := p.Read(t.Context(), leaf, agentapi.ReadLimits{})
			if e != nil {
				t.Fatal(e)
			}
			defer func() { _ = s.Close() }()
			switch kind {
			case historyScenarioAppend:
				f, e := os.OpenFile(leaf.Path, os.O_APPEND|os.O_WRONLY, 0600)
				if e != nil {
					t.Fatal(e)
				}
				_, e = f.WriteString("{\"ordinal\":2,\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"later\"}}\n")
				if e = errors.Join(e, f.Close()); e != nil {
					t.Fatal(e)
				}
			case historyScenarioRewrite:
				if e := os.WriteFile(leaf.Path, bytes.Replace(raw, []byte("own"), []byte("new"), 1), 0600); e != nil {
					t.Fatal(e)
				}
			case historyScenarioReplace:
				if e := os.Remove(leaf.Path); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(leaf.Path, raw, 0600); e != nil {
					t.Fatal(e)
				}
			case historyScenarioLocatorChange:
				l.changed = true
			}
			out, e := (Filter{}).Filter(t.Context(), s.Input(), agentapi.FilterContext{})
			if kind == historyScenarioAppend {
				if e != nil || len(out.Records) != 2 {
					t.Fatalf("append prefix %d %v", len(out.Records), e)
				}
			} else if agentapi.Failure(e) != agentapi.Changed {
				t.Fatalf("rewrite not retryable: %v", e)
			}
		})
	}
}

func TestRelatedLimitsAndCleanup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	leaf, _ := historyFile(t, dir, threadB, threadB, 0, map[string]any{"parent_thread_id": threadA}, "own")
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}}
	p := historyPass(t, dir, l)
	if _, e := p.Read(t.Context(), leaf, agentapi.ReadLimits{RawBytes: 1}); agentapi.Failure(e) != agentapi.Limit {
		t.Fatalf("byte cap: %v", e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e := p.Read(ctx, leaf, agentapi.ReadLimits{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if e := p.Close(); e != nil {
		t.Fatal(e)
	}
	owned := p.(*relatedSourcePass)
	if owned.bytes != 0 || len(owned.files) != 0 || len(owned.live) != 0 {
		t.Fatal("pass retained resources")
	}
}

func TestForkThenRevertKeepsLogicalBoundary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fork, raw := historyFile(t, dir, threadB, threadB, 0, map[string]any{"forked_from_id": threadA, "forked_from_ordinal_exclusive": 2}, "inherited", "owned before revert")
	leaf, _ := historyFile(t, dir, rolloutC, threadB, 3, map[string]any{"history_base": codexmeta.CodexHistoryPosition{RolloutID: threadB, EndOrdinal: 3, EndByteOffset: uint64(len(raw))}}, "owned after revert")
	l := &historyLookup{thread: agentapi.CodexRolloutSet{Current: &leaf, Revision: "one"}, rollouts: map[string][]agentapi.SourceRef{threadB: {fork}}}
	b := historyRead(t, historyPass(t, dir, l), leaf)
	a, e := (Parser{}).Parse(t.Context(), b)
	if e != nil {
		t.Fatal(e)
	}
	if len(a.View.Turns) != 2 || b.History.OwnStart == nil || *b.History.OwnStart != 2 {
		t.Fatalf("lost logical fork ownership: %+v %+v", b.History, a.View.Turns)
	}
}

func TestOrdinaryActiveAppendMakesProgress(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, "first")
	p, e := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = p.Close() }()
	for i := range 3 {
		s, e := p.Read(t.Context(), ref, agentapi.ReadLimits{})
		if e != nil {
			t.Fatal(e)
		}
		f, e := os.OpenFile(ref.Path, os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		_, e = fmt.Fprintf(f, "{\"type\":\"event_msg\",\"ordinal\":%d,\"payload\":{\"type\":\"task_started\"}}\n", i+2)
		if e = errors.Join(e, f.Close()); e != nil {
			t.Fatal(e)
		}
		out, e := (Filter{}).Filter(t.Context(), s.Input(), agentapi.FilterContext{})
		if e != nil {
			t.Fatal(e)
		}
		if out.History != nil || len(out.Records) != i+2 {
			t.Fatalf("prefix progress %d %d", i, len(out.Records))
		}
		if e := s.Close(); e != nil {
			t.Fatal(e)
		}
	}
}
