package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func ownTools(t *testing.T) []byte { t.Helper(); return readSynthetic(t, "synthetic-own-tools.jsonl") }

func mutateToolNative(t *testing.T, raw []byte, offset int, change func(map[string]any)) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	var m map[string]any
	if json.Unmarshal(lines[len(lines)-offset], &m) != nil {
		t.Fatal("synthetic envelope")
	}
	change(m)
	lines[len(lines)-offset], _ = json.Marshal(m)
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

func TestOwnCommittedToolsSurviveNativeLoss(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, ownTools(t), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-own-tools.golden.jsonl"), append(bytes.Join(out.Records, []byte("\n")), '\n'))
	raw := bytes.Join(out.Records, nil)
	for _, private := range []string{"PRIVATE_", "response_id\":\"PRIVATE", "\"name\"", "\"args\"", "\"tool_name\""} {
		if bytes.Contains(raw, []byte(private)) {
			t.Fatal("private tool body retained")
		}
	}
	b := bundleFor(t, out)
	// Serialize the retained source before all consumers; no native file is available.
	wire, _ := json.Marshal(b)
	b = archive.SourceBundle{}
	if json.Unmarshal(wire, &b) != nil {
		t.Fatal("bundle roundtrip")
	}
	a, err := (Parser{}).Parse(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.View.ToolCalls) != 3 || len(a.View.ToolResults) != 0 || !a.Observability.ToolCalls.Available() || a.Observability.ToolResults.Available() || a.Observability.ToolErrors.Available() {
		t.Fatal("tool availability")
	}
	for i, c := range a.View.ToolCalls {
		if c.CallID != fmt.Sprintf("call_synthetic_%d", i) || c.Name != "" || c.ObservedName != "" || c.Invocation || c.Input != nil || c.ResultText != "" || c.IsError != nil || c.OutputBytes != nil || c.Model != "" || c.ParentID != "" || !reflect.DeepEqual(c.Action, archive.ToolAction{}) {
			t.Fatal("invented tool facts")
		}
		if (i != 1) != (c.ResultRecordIndex != nil) {
			t.Fatal("partial reordered result association")
		}
		if c.RecordIndex != len(b.NativeRecords)-2 || c.ResultRecordIndex != nil && *c.ResultRecordIndex != len(b.NativeRecords)-1 {
			t.Fatal("wrong physical indexes")
		}
	}
	again, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
	if err != nil || !reflect.DeepEqual(out.Records, again.Records) || !(Filter{}).EvidenceExtends(b, b) {
		t.Fatal("retained-only validation", err)
	}
	info := archive.ParserInfo{Name: "muse", Version: (Parser{}).Version()}
	m, err := archive.BuildMetadataWithAnalysis(b, a, nil, "synthetic", time.Unix(2000000000, 0), time.Unix(2000000100, 0), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, info)
	if err != nil {
		t.Fatal(err)
	}
	if m.Counts.ToolCalls == nil || *m.Counts.ToolCalls != 3 || m.Counts.ToolResults != nil || m.Counts.ToolErrors != nil || m.Counts.FilesTouched != nil || len(m.ToolsUsed) != 0 || len(m.MCPCalls) != 0 || len(m.GitActivity) != 0 || m.Counts.InputTokens != nil || m.TokenObservations == nil {
		t.Fatal("metadata manufactured detail")
	}
	e, err := archive.BuildEvalExportWithAnalysis(b, a, m, archive.EvalExportSourceArchive, archive.EvalExportDetailFull, info)
	if err != nil || !reflect.DeepEqual(e.Counts, m.Counts) || len(e.ToolsUsed) != 0 || len(e.MCPCalls) != 0 {
		t.Fatal("eval disagreement", err)
	}
	h, err := archive.BuildHandoffWithAnalysis(b, a, &m, archive.HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := archive.BuildTranscriptWithAnalysis(b, a, archive.HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	traw, _ := json.Marshal(transcript)
	if bytes.Contains(traw, []byte("call_synthetic")) {
		t.Fatal("unlisted call reached transcript")
	}
	hraw, _ := json.Marshal(h)
	if bytes.Contains(hraw, []byte("call_synthetic")) || bytes.Contains(hraw, []byte("result_record_index")) {
		t.Fatal("unlisted identity reached narrative")
	}
	// Frozen pre-change typed result/handoff readers must receive no newly
	// ambiguous false/zero result object. Calls retain nullable individual facts.
	var old struct {
		ToolCalls []struct {
			RecordIndex       int    `json:"record_index"`
			CallID            string `json:"call_id"`
			ResultRecordIndex *int   `json:"result_record_index"`
			IsError           *bool  `json:"is_error"`
			OutputBytes       *int   `json:"output_bytes"`
		} `json:"tool_calls"`
		ToolResults []struct {
			IsError     bool `json:"is_error"`
			OutputBytes int  `json:"output_bytes"`
		} `json:"tool_results"`
	}
	vraw, _ := json.Marshal(struct {
		ToolCalls   []archive.NormalizedToolCall   `json:"tool_calls"`
		ToolResults []archive.NormalizedToolResult `json:"tool_results"`
	}{a.View.ToolCalls, a.View.ToolResults})
	if json.Unmarshal(vraw, &old) != nil || len(old.ToolCalls) != 3 || len(old.ToolResults) != 0 || old.ToolCalls[0].ResultRecordIndex == nil || old.ToolCalls[0].IsError != nil || old.ToolCalls[0].OutputBytes != nil {
		t.Fatal("old typed normalized wire")
	}
	validateObservationWire(t, "metadata.schema.json", m)
	validateObservationWire(t, "eval-export.schema.json", e)
}

func TestToolNativeClosedGrammarAndJoins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		offset int
		change func(map[string]any)
	}{
		{"foreign-run", 2, func(m map[string]any) { object(m["payload"])["run_id"] = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee" }},
		{"source-replay", 1, func(m map[string]any) {
			object(m["payload"])["source_run_record_id"] = "dddddddd-dddd-4ddd-8ddd-000000000001"
		}},
		{"source-order", 1, func(m map[string]any) { object(m["payload"])["source_run_record_sequence"] = 1 }},
		{"causation", 2, func(m map[string]any) { m["causation_id"] = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee" }},
		{"unmatched-batch", 1, func(m map[string]any) { object(object(m["payload"])["event"])["batch_id"] = "different_message" }},
		{"wrong-call", 1, func(m map[string]any) {
			object(object(object(m["payload"])["event"])["results"].([]any)[0])["tool_call_id"] = "call_synthetic_1"
		}},
		{"duplicate-index", 1, func(m map[string]any) { a := object(object(m["payload"])["event"])["results"].([]any); a[1] = a[0] }},
		{"duplicate-call", 2, func(m map[string]any) {
			a := object(object(m["payload"])["event"])["tool_calls"].([]any)
			object(a[1])["call_id"] = object(a[0])["call_id"]
		}},
		{"duplicate-native-item", 2, func(m map[string]any) {
			a := object(object(m["payload"])["event"])["tool_calls"].([]any)
			object(a[1])["id"] = object(a[0])["id"]
		}},
	}
	for _, spec := range []struct {
		offset int
		array  string
		keys   []string
	}{{2, "tool_calls", []string{"id", "call_id", "name", "args"}}, {1, "results", []string{"tool_call_index", "tool_call_id", "text"}}} {
		for _, key := range spec.keys {
			for _, bad := range []any{nil, true, map[string]any{}, []any{}} {
				s, k, v := spec, key, bad
				cases = append(cases, struct {
					name   string
					offset int
					change func(map[string]any)
				}{fmt.Sprintf("bad-%s-%T", k, v), s.offset, func(m map[string]any) { object(object(object(m["payload"])["event"])[s.array].([]any)[0])[k] = v }})
			}
		}
		s := spec
		for _, bad := range []any{nil, true, []any{}} {
			v := bad
			cases = append(cases, struct {
				name   string
				offset int
				change func(map[string]any)
			}{fmt.Sprintf("array-%s-%T", s.array, v), s.offset, func(m map[string]any) { object(object(m["payload"])["event"])[s.array] = v }})
		}
	}

	for _, offset := range []int{1, 2} {
		for _, level := range []string{"payload", "event", "item"} {
			for _, operation := range []string{"missing", "extra"} {
				off, lev, op := offset, level, operation
				cases = append(cases, struct {
					name   string
					offset int
					change func(map[string]any)
				}{fmt.Sprintf("%s-%s-%d", op, lev, off), off, func(m map[string]any) {
					p := object(m["payload"])
					target := p
					if lev != "payload" {
						target = object(p["event"])
					}
					if lev == "item" {
						a := "results"
						if off == 2 {
							a = "tool_calls"
						}
						target = object(target[a].([]any)[0])
					}
					if op == "extra" {
						target["future_private"] = true
					} else {
						key := "source_run_record_id"
						if lev == "event" {
							key = "kind"
						}
						if lev == "item" {
							key = "tool_call_id"
							if off == 2 {
								key = "args"
							}
						}
						delete(target, key)
					}
				}})
			}
		}
	}
	for _, bad := range []any{-1, 1.5, 3, json.Number("9007199254740992")} {
		v := bad
		cases = append(cases, struct {
			name   string
			offset int
			change func(map[string]any)
		}{fmt.Sprintf("index-%v", v), 1, func(m map[string]any) {
			object(object(object(m["payload"])["event"])["results"].([]any)[0])["tool_call_index"] = v
		}})
	}
	for _, bad := range []string{"", " space", "path/value", "x.y", "_leading", strings.Repeat("x", 129), "ghp_" + strings.Repeat("x", 36)} {
		v := bad
		cases = append(cases, struct {
			name   string
			offset int
			change func(map[string]any)
		}{fmt.Sprintf("id-%d", len(v)), 2, func(m map[string]any) { object(object(m["payload"])["event"])["message_id"] = v }})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := filterBytes(t, mutateToolNative(t, ownTools(t), tc.offset, tc.change), agentapi.ReadLimits{})
			if err == nil || len(out.Records) != 0 {
				t.Fatal("malformed tools accepted")
			}
		})
	}
}

func TestRetainedToolGrammarAndLegacyCoverage(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, ownTools(t), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	b := bundleFor(t, out)
	for _, which := range []int{len(b.NativeRecords) - 2, len(b.NativeRecords) - 1} {
		for _, key := range []string{"run_id", "source_run_record_id", "source_run_record_sequence", "message_id", "tool_calls", "batch_id", "results"} {
			t.Run(fmt.Sprintf("partial-%d-%s", which, key), func(t *testing.T) {
				t.Parallel()
				bad := cloneBundle(t, b)
				f := object(bad.NativeRecords[which]["fields"])
				if _, ok := f[key]; !ok {
					return
				}
				delete(f, key)
				if _, err := (Parser{}).Parse(t.Context(), bad); err == nil {
					t.Fatal("partial enrichment treated as legacy")
				}
				if _, err := (Filter{}).Refilter(t.Context(), bad, time.Time{}); err == nil {
					t.Fatal("partial refilter")
				}
				if (Filter{}).EvidenceExtends(bad, bad) {
					t.Fatal("partial evidence")
				}
			})
		}
	}
	// Erase both complete branches as old filters did: legacy-only is readable,
	// with no observed calls and unavailable counts rather than measured zero.
	legacy := cloneBundle(t, b)
	for _, i := range []int{len(b.NativeRecords) - 2, len(b.NativeRecords) - 1} {
		f := object(legacy.NativeRecords[i]["fields"])
		legacy.NativeRecords[i]["fields"] = map[string]any{"kind": "run", "event_kind": f["event_kind"]}
	}
	a, err := (Parser{}).Parse(t.Context(), legacy)
	if err != nil || len(a.View.ToolCalls) != 0 || a.Observability.ToolCalls.Available() {
		t.Fatal("legacy counts", err)
	}
	mixed := cloneBundle(t, b)
	mixed.NativeRecords[len(b.NativeRecords)-1]["fields"] = map[string]any{"kind": "run", "event_kind": "tool_result_batch_committed"}
	a, err = (Parser{}).Parse(t.Context(), mixed)
	if err != nil || len(a.View.ToolCalls) != 3 || a.Observability.ToolCalls.Available() {
		t.Fatal("mixed count claimed exact", err)
	}
	open := cloneBundle(t, b)
	open.NativeRecords = open.NativeRecords[:len(open.NativeRecords)-1]
	a, err = (Parser{}).Parse(t.Context(), open)
	if err != nil || !a.Observability.ToolCalls.Available() || a.View.ToolCalls[0].ResultRecordIndex != nil {
		t.Fatal("open call prefix", err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "filter24-retained.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err = c.AddResource("frozen.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("frozen.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{len(b.NativeRecords) - 2, len(b.NativeRecords) - 1} {
		if s.Validate(b.NativeRecords[i]) == nil || s.Validate(legacy.NativeRecords[i]) != nil {
			t.Fatal("frozen actual filter24 schema compatibility")
		}
	}
}

func TestToolBatchesRejectReplaysAndPriorSourceCollisions(t *testing.T) {
	t.Parallel()
	for _, offset := range []int{1, 2} {
		t.Run(fmt.Sprintf("repeated-batch-%d", offset), func(t *testing.T) {
			t.Parallel()
			lines := bytes.Split(bytes.TrimSpace(ownTools(t)), []byte("\n"))
			var m map[string]any
			_ = json.Unmarshal(lines[len(lines)-offset], &m)
			m["id"] = "ffffffff-ffff-4fff-8fff-ffffffffffff"
			m["sequence"] = 14
			m["recorded_at"] = 1792083855882900
			p := object(m["payload"])
			p["source_run_record_id"] = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
			p["source_run_record_sequence"] = 7
			next, _ := json.Marshal(m)
			lines = append(lines, next)
			out, err := filterBytes(t, append(bytes.Join(lines, []byte("\n")), '\n'), agentapi.ReadLimits{})
			if err == nil || len(out.Records) != 0 {
				t.Fatal("repeated committed batch accepted")
			}
		})
	}
	// Every previously relevant source kind shares the same source-record domain.
	lines := bytes.Split(bytes.TrimSpace(ownTools(t)), []byte("\n"))
	for _, line := range lines[1 : len(lines)-2] {
		var m map[string]any
		_ = json.Unmarshal(line, &m)
		p := object(m["payload"])
		if p["kind"] != "run" || p["source_run_record_id"] == nil {
			continue
		}
		q := p["source_run_record_id"]
		bad := mutateToolNative(t, ownTools(t), 2, func(m map[string]any) { object(m["payload"])["source_run_record_id"] = q })
		if _, err := filterBytes(t, bad, agentapi.ReadLimits{}); err == nil {
			t.Fatal("cross-kind source replay accepted")
		}
	}
}

func TestRetainedToolJoinsRefuseContradictions(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, ownTools(t), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	b := bundleFor(t, out)
	callIndex, resultIndex := len(b.NativeRecords)-2, len(b.NativeRecords)-1
	cases := []func(archive.SourceBundle){
		func(b archive.SourceBundle) { object(b.NativeRecords[resultIndex]["fields"])["batch_id"] = "unmatched" },
		func(b archive.SourceBundle) {
			object(b.NativeRecords[resultIndex]["fields"])["run_id"] = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
		},
		func(b archive.SourceBundle) {
			object(b.NativeRecords[resultIndex]["fields"])["source_run_record_id"] = object(b.NativeRecords[callIndex]["fields"])["source_run_record_id"]
		},
		func(b archive.SourceBundle) {
			object(b.NativeRecords[resultIndex]["fields"])["source_run_record_sequence"] = 1
		},
		func(b archive.SourceBundle) {
			object(b.NativeRecords[callIndex]["fields"])["message_id"] = "ghp_" + strings.Repeat("x", 36)
		},
		func(b archive.SourceBundle) { object(b.NativeRecords[callIndex]["fields"])["tool_calls"] = []any{} },
	}
	for _, bad := range []any{nil, true, -1, 1.5, 3, json.Number("9007199254740992")} {
		v := bad
		cases = append(cases, func(b archive.SourceBundle) {
			object(object(b.NativeRecords[resultIndex]["fields"])["results"].([]any)[0])["tool_call_index"] = v
		})
	}
	cases = append(cases, func(b archive.SourceBundle) {
		object(object(b.NativeRecords[resultIndex]["fields"])["results"].([]any)[0])["tool_call_id"] = "call_synthetic_1"
	}, func(b archive.SourceBundle) {
		a := object(b.NativeRecords[resultIndex]["fields"])["results"].([]any)
		a[1] = a[0]
	})
	for i, change := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) { t.Parallel(); bad := cloneBundle(t, b); change(bad); assertRetainedRefused(t, bad) })
	}
}

func TestCommittedToolsRespectExistingLimitsAndCancellation(t *testing.T) {
	t.Parallel()
	raw := ownTools(t)
	for _, offset := range []int{1, 2} {
		large := mutateToolNative(t, raw, offset, func(m map[string]any) {
			event := object(object(m["payload"])["event"])
			array, key := "results", "text"
			if offset == 2 {
				array, key = "tool_calls", "args"
			}
			object(event[array].([]any)[0])[key] = strings.Repeat("x", maxString+1)
		})
		if _, err := filterBytes(t, large, agentapi.ReadLimits{}); err == nil {
			t.Fatal("oversized dropped body bypassed decoder")
		}
	}
	for _, limit := range []agentapi.ReadLimits{{RawBytes: int64(len(raw) - 1)}, {Records: 12}, {FilteredBytes: 100}} {
		if _, err := filterBytes(t, raw, limit); err == nil {
			t.Fatal("existing output/input limit bypassed")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	budget := agentapi.NewNativeReadBudget(8 << 20)
	out, release, err := (Filter{}).FilterLeased(ctx, agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, budget)
	release()
	used, _ := budget.Charged()
	if !errors.Is(err, context.Canceled) || len(out.Records) != 0 || used != 0 {
		t.Fatal("cancelled tool source leaked artifact/reservation")
	}
}

func TestCommittedToolIdentitiesAreIndependentAcrossOwnRuns(t *testing.T) {
	t.Parallel()
	lines := bytes.Split(bytes.TrimSpace(ownTools(t)), []byte("\n"))
	var accepted, started, materialized map[string]any
	for _, line := range lines[1:] {
		var m map[string]any
		_ = json.Unmarshal(line, &m)
		p := object(m["payload"])
		switch m["payload_type"] {
		case "runtime.user_intent.accepted":
			accepted = m
		case "runtime.user_intent.materialized":
			materialized = m
		case "runtime.session":
			if object(p["event"])["kind"] == "started" {
				started = m
			}
		}
	}
	if accepted == nil || started == nil || materialized == nil {
		t.Fatal("synthetic own graph")
	}
	const run = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	const intent = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	id := func(n int) string { return fmt.Sprintf("abcdefab-abcd-4abc-8abc-%012x", n) }
	var calls, results map[string]any
	_ = json.Unmarshal(lines[len(lines)-2], &calls)
	_ = json.Unmarshal(lines[len(lines)-1], &results)
	for i, m := range []map[string]any{accepted, started, materialized, calls, results} {
		m["id"] = id(i)
		m["sequence"] = 14 + i
		m["recorded_at"] = 1792083870000000 + i
		p := object(m["payload"])
		if i == 0 || i == 2 {
			p["intent_id"] = intent
		}
		if i == 1 || i >= 3 {
			p["run_id"] = run
		}
		if i == 2 {
			p["envelope_record_id"] = id(0)
			p["envelope_sequence"] = 14
			outcome := object(p["outcome"])
			outcome["run_id"] = run
			outcome["run_started_session_record_id"] = id(1)
		}
		encoded, _ := json.Marshal(m)
		lines = append(lines, encoded)
	}
	out, err := filterBytes(t, append(bytes.Join(lines, []byte("\n")), '\n'), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	b := bundleFor(t, out)
	a, err := (Parser{}).Parse(t.Context(), b)
	if err != nil || len(a.View.ToolCalls) != 6 || !a.Observability.ToolCalls.Available() {
		t.Fatal("same source/message/call IDs in different materialized runs collided", err)
	}
	if a.View.ToolCalls[0].CallID != a.View.ToolCalls[3].CallID || a.View.ToolCalls[0].ResultRecordIndex == nil || a.View.ToolCalls[3].ResultRecordIndex == nil || *a.View.ToolCalls[0].ResultRecordIndex == *a.View.ToolCalls[3].ResultRecordIndex {
		t.Fatal("cross-run result association")
	}
	assertRetainedRefused(t, func() archive.SourceBundle {
		bad := cloneBundle(t, b)
		object(bad.NativeRecords[len(b.NativeRecords)-1]["fields"])["run_id"] = "33333333-3333-4333-8333-333333333333"
		return bad
	}())
}

func manyCommittedCalls(t *testing.T, batches, items int) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(ownTools(t)), []byte("\n"))
	template := lines[len(lines)-2]
	lines = lines[:len(lines)-2]
	for n := range batches {
		var row map[string]any
		_ = json.Unmarshal(template, &row)
		row["id"] = fmt.Sprintf("eeeeeeee-eeee-4eee-8eee-%012x", n)
		row["sequence"] = 12 + n
		row["recorded_at"] = 1792083890000000 + n
		p := object(row["payload"])
		p["source_run_record_id"] = fmt.Sprintf("ffffffff-ffff-4fff-8fff-%012x", n)
		p["source_run_record_sequence"] = 5 + n
		event := object(p["event"])
		event["message_id"] = "m" + strconv.Itoa(n)
		calls := make([]any, items)
		for j := range items {
			id := strconv.Itoa(n*items + j)
			calls[j] = map[string]any{"id": "i" + id, "call_id": "c" + id, "name": "", "args": ""}
		}
		event["tool_calls"] = calls
		encoded, _ := json.Marshal(row)
		lines = append(lines, encoded)
	}
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

// Short opaque IDs cannot let persistent typed calls/maps outgrow their lease.
// The former 8x reservation accepted 16384 calls at 5 MiB even though the call
// structs and typed keys alone require more than 6 MiB on supported 64-bit builds.
func TestCommittedToolArraysReservePersistentExpansion(t *testing.T) {
	t.Parallel()
	raw := manyCommittedCalls(t, 128, 128)
	budget := agentapi.NewNativeReadBudget(5 << 20)
	out, release, err := (Filter{}).FilterLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, budget)
	used, _ := budget.Charged()
	release()
	release()
	after, _ := budget.Charged()
	if !errors.Is(err, agentapi.ErrReadBudget) || len(out.Records) != 0 || used != 0 || after != 0 {
		t.Fatal("many short calls escaped persistent expansion accounting", err, used, after)
	}
	var previous int64
	for _, shape := range []struct {
		batches int
		items   int
	}{{1, 32}, {8, 64}, {128, 128}} {
		native := manyCommittedCalls(t, shape.batches, shape.items)
		budget = agentapi.NewNativeReadBudget(256 << 20)
		out, release, err = (Filter{}).FilterLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(native)}}, agentapi.FilterContext{}, budget)
		if err != nil {
			release()
			t.Fatal("bounded control refused", err)
		}
		used, _ = budget.Charged()
		// This is a lower bound independent of the encoding multiplier: the
		// retained lease must at least cover actual normalized structs and keys.
		key := struct {
			session string
			run     string
			call    string
		}{}
		lower := int64(reflect.TypeFor[archive.NormalizedToolCall]().Size()+reflect.TypeOf(key).Size()+reflect.TypeFor[toolCallRef]().Size()+reflect.TypeFor[int]().Size()) * int64(shape.batches*shape.items)
		if used <= previous || used < lower {
			release()
			t.Fatal("tool reservation failed scaling/lower bound", used, lower)
		}
		previous = used
		b := bundleFor(t, out)
		a, parseErr := (Parser{}).Parse(t.Context(), b)
		if parseErr != nil || len(a.View.ToolCalls) != shape.batches*shape.items {
			release()
			t.Fatal("bounded leased controls lost identities", parseErr)
		}
		release()
		release()
		after, _ = budget.Charged()
		if after != 0 {
			t.Fatal("successful idempotent release leaked", after)
		}
	}
}
