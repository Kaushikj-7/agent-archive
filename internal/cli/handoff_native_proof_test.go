package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/nativesessions"
)

type countedRecheckPass struct {
	agentapi.SourcePass
	reads int
}

func (p *countedRecheckPass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	p.reads++
	return p.SourcePass.Read(ctx, ref, limits)
}

func TestNativeCapableCodexKeepsSecondHandleRecheck(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	id := "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(f.codex, "rollout-2026-10-01T12-00-00-"+id+".jsonl")
	meta := map[string]any{"id": id, "cwd": f.cwd, "timestamp": "2026-10-01T12:00:00Z", "cli_version": "0.160.0", "originator": "codex_cli_rs", "source": "cli", "history_mode": "paginated"}
	first, err := json.Marshal(map[string]any{"type": "session_meta", "ordinal": 0, "payload": meta})
	must(t, err)
	second, err := json.Marshal(map[string]any{"type": "response_item", "ordinal": 1, "timestamp": "2026-10-01T12:01:00Z", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Synthetic capable Codex selection"}}}})
	must(t, err)
	raw := append(append(first, '\n'), append(second, '\n')...)
	must(t, os.WriteFile(path, raw, 0600))
	meter := &nativeReadMeter{}
	directory, err := meter.EvalSymlinks(f.cwd)
	must(t, err)
	c := nativesessions.Candidate{NativeID: id, Directory: directory, StartedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Ref: nativesessions.Ref{Harness: "codex", Path: path, Store: f.codex}}
	pass, snapshot, input, err := openNativeSource(t.Context(), meter, c, f.env.agentRegistry())
	must(t, err)
	defer func() { must(t, snapshot.Close()); must(t, pass.Close()) }()
	content, capable := snapshot.(agentapi.VerifiedContentSnapshot)
	if !capable || !content.ContentProofFor(snapshot, input) {
		t.Fatal("fixture did not exercise actual capable Codex snapshot")
	}
	c.Stamp = input.File.Stamp()
	counted := &countedRecheckPass{SourcePass: pass}
	beforeBytes, beforeOpens, _, _ := meter.counts()
	must(t, recheckNativeSource(t.Context(), counted, c.Ref, snapshot, input))
	afterBytes, afterOpens, _, _ := meter.counts()
	if counted.reads != 1 || afterBytes <= beforeBytes {
		t.Fatalf("capable Codex bypassed original recheck: reads=%d bytes=%d", counted.reads, afterBytes-beforeBytes)
	}
	t.Logf("capable Codex recheck: pass_reads=%d descriptor_bytes=%d opens=%d raw=%d", counted.reads, afterBytes-beforeBytes, afterOpens-beforeOpens, len(raw))
	must(t, snapshot.Close())
	must(t, pass.Close())
	target, err := handoffFromNative(t.Context(), c, meter, f.env)
	must(t, err)
	if target.native == nil || target.native.Ref.Harness != "codex" || len(target.bundle.NativeRecords) != 2 {
		t.Fatal("capable Codex selected route failed")
	}
	_, _, active, _ := meter.counts()
	if active != 0 {
		t.Fatal("Codex recheck retained handles", active)
	}
}
