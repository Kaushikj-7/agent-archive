package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/stats"
)

func TestMuseCommittedToolsUseExistingArchiveConsumersAfterNativeLoss(t *testing.T) {
	t.Parallel()
	for _, fixture := range []struct {
		name                 string
		calls                int
		result, observations bool
		callID               string
	}{
		{"synthetic-own-tools.jsonl", 3, true, true, "call_synthetic_0"},
		{"synthetic-committed-read-effects.jsonl", 1, false, false, "synthetic-call"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			t.Parallel()
			testMuseArchiveToolConsumers(t, fixture.name, fixture.calls, fixture.result, fixture.observations, fixture.callID)
		})
	}
}

func testMuseArchiveToolConsumers(t *testing.T, fixture string, calls int, result, observations bool, callID string) {
	t.Helper()
	env, data, workspace, counter := museNativeEnv(t)
	raw := museNativeFixtureBytes(t, fixture, museSyntheticID, workspace)
	path := writeMuseNative(t, data, museSyntheticID, raw)
	var selected archive.FilteredTranscript
	counter.after = func(out archive.FilteredTranscript) { selected = out }
	local, stderr, code := runHandoff(t, env, "--latest", "--source", "local", "--harness", "muse", "--format", "json")
	if code != 0 || counter.calls.Load() != 1 || !strings.Contains(local, "tool_results_unavailable") {
		t.Fatalf("local handoff %d %s", code, stderr)
	}
	explicit, stderr, code := runHandoff(t, env, "--file", path, "--harness", "muse", "--format", "json")
	if code != 0 || !strings.Contains(explicit, "tool_results_unavailable") {
		t.Fatalf("explicit handoff %d %s", code, stderr)
	}
	f := newHandoffFixture(t, false)
	reg := archive.SessionRegistration{ArchiveSessionID: f.id, NativeSessionID: museSyntheticID, ProjectID: "local", ProjectRoot: workspace, Harness: archive.Harness{Name: "muse"}, SessionStartedAt: selected.NativeStartAt}
	b, err := archive.NewSourceBundle(reg, counter.TranscriptFilter, selected, env.Now(), nil)
	must(t, err)
	packed, err := archive.BuildCompressedSource(b)
	must(t, err)
	key, err := archive.SourceObjectKey(b, packed.SHA256)
	must(t, err)
	must(t, f.mem.Put(t.Context(), key, packed.Bytes))
	a, err := analyzeSource(t.Context(), env.Agents, b)
	must(t, err)
	parser, _ := env.agentRegistry().LookupParser("muse")
	m, err := archive.BuildMetadataWithAnalysis(b, a, nil, "synthetic-machine", selected.NativeStartAt, env.Now(), archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}, archive.ParserInfo{Name: "muse", Version: parser.Version()})
	must(t, err)
	summary := stats.Compute([]archive.Metadata{m}, stats.Options{Now: m.CapturedAt, Days: 1, Location: time.UTC, PriceTable: stats.DefaultPriceTable()})
	if summary.Overview.Cost.Value != nil || summary.Overview.Tokens.Value != nil || summary.Highlights.ToolErrors != nil || summary.Coverage.SessionsWithoutToolErrors != 1 || (summary.TokenObservations != nil) != observations {
		t.Fatal("stats invented tool status or priced observations")
	}
	mraw, err := json.Marshal(m)
	must(t, err)
	mkey, err := archive.MetadataObjectKey("muse", f.id)
	must(t, err)
	must(t, f.mem.Put(t.Context(), mkey, mraw))
	must(t, os.Remove(path))
	shown, stderr, code := runShow(t, f.env, f.id, "--harness", "muse", "--transcript", "--json", "--max-bytes", "0")
	if code != 0 {
		t.Fatalf("show %d %s", code, stderr)
	}
	decoder := json.NewDecoder(strings.NewReader(shown))
	var sidecar map[string]any
	must(t, decoder.Decode(&sidecar))
	// Frozen ed23 public normalized result/call fields. Unknown fields cannot
	// rescue old bool/int results, so these arrays must stay empty.
	var old struct {
		ToolCalls []struct {
			RecordIndex       int            `json:"record_index"`
			CallID            string         `json:"call_id,omitempty"`
			ParentID          string         `json:"parent_id,omitempty"`
			Model             string         `json:"model,omitempty"`
			Reasoning         string         `json:"reasoning_level,omitempty"`
			Name              string         `json:"name,omitempty"`
			Input             map[string]any `json:"input,omitempty"`
			ResultRecordIndex *int           `json:"result_record_index"`
			IsError           *bool          `json:"is_error"`
			OutputBytes       *int           `json:"output_bytes"`
		} `json:"tool_calls"`
		ToolResults []struct {
			RecordIndex int    `json:"record_index"`
			CallID      string `json:"call_id,omitempty"`
			IsError     bool   `json:"is_error,omitempty"`
			OutputBytes int    `json:"output_bytes"`
		} `json:"tool_results"`
	}
	must(t, decoder.Decode(&old))
	if len(old.ToolCalls) != calls || len(old.ToolResults) != 0 || (old.ToolCalls[0].ResultRecordIndex != nil) != result {
		t.Fatal("normalized show lost occurrence")
	}
	for _, c := range old.ToolCalls {
		if c.IsError != nil || c.OutputBytes != nil {
			t.Fatal("unknown tool status became false/zero")
		}
	}
	for _, detail := range []string{"full", "metadata"} {
		records, stderr, code := evalLines(t, f.env, f.id, "--harness", "muse", "--detail", detail)
		if code != 0 || len(records) != 1 {
			t.Fatalf("eval %s %d %s", detail, code, stderr)
		}
		counts := records[0]["counts"].(map[string]any)
		if counts["tool_calls"] != float64(calls) || counts["tool_results"] != nil || counts["tool_errors"] != nil || counts["files_touched"] != nil || records[0]["tools_used"] != nil || records[0]["mcp_calls"] != nil {
			t.Fatal("eval fabricated tool detail")
		}
	}
	handoff, stderr, code := runHandoff(t, f.env, f.id, "--source", "archive", "--harness", "muse", "--format", "json")
	if code != 0 {
		t.Fatalf("archive handoff %d %s", code, stderr)
	}
	var oldHandoff struct {
		ToolResultsUnavailable bool `json:"tool_results_unavailable"`
		Exchanges              []struct {
			Steps []struct {
				Tool *struct {
					Name          string `json:"name"`
					Summary       string `json:"summary,omitempty"`
					IsError       bool   `json:"is_error,omitempty"`
					Result        string `json:"result,omitempty"`
					ResultLines   int    `json:"result_lines,omitempty"`
					ResultBytes   int    `json:"result_bytes,omitempty"`
					ResultOmitted bool   `json:"result_omitted,omitempty"`
				} `json:"tool"`
			} `json:"steps"`
		} `json:"exchanges"`
	}
	must(t, json.Unmarshal([]byte(handoff), &oldHandoff))
	if !oldHandoff.ToolResultsUnavailable {
		t.Fatal("result availability lost")
	}
	for _, e := range oldHandoff.Exchanges {
		for _, s := range e.Steps {
			if s.Tool != nil {
				t.Fatal("old typed handoff received ambiguous result object")
			}
		}
	}
	for _, artifact := range []string{shown, handoff, local, string(mraw)} {
		if strings.Contains(artifact, "PRIVATE_") || strings.Contains(artifact, `"name":"tool"`) {
			t.Fatal("private or placeholder fact escaped")
		}
	}
	n := normalizedOutput{ToolCalls: a.View.ToolCalls, ToolResults: a.View.ToolResults}
	clone := cloneNormalized(n)
	dropOldestRecords(&clone, 1)
	if len(n.ToolCalls) != calls || len(clone.ToolResults) != 0 {
		t.Fatal("clone/trim changed source identity or invented results")
	}
	// Actual normalized document encoder preserves nullable individual facts.
	docs, err := normalizedDocuments(sessionView{Metadata: m}, n)
	must(t, err)
	if !bytes.Contains(docs, []byte(`"`+callID+`"`)) || bytes.Contains(docs, []byte(`"is_error"`)) || bytes.Contains(docs, []byte(`"output_bytes"`)) {
		t.Fatal("normalized document wire")
	}
}
