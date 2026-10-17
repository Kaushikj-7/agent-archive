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
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func committedRead(t *testing.T) []byte {
	t.Helper()
	return readSynthetic(t, "synthetic-committed-read-effects.jsonl")
}

func changeRead(t *testing.T, change func([]map[string]any) []map[string]any) []byte {
	t.Helper()
	var rows []map[string]any
	for line := range bytes.SplitSeq(bytes.TrimSpace(committedRead(t)), []byte("\n")) {
		m, _, err := decodeObject(line)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, m)
	}
	rows = change(rows)
	var out [][]byte
	for i, m := range rows {
		if i == 0 {
			out = append(out, bytes.SplitN(committedRead(t), []byte("\n"), 2)[0])
			continue
		}
		if i > 0 {
			if _, marker := m["retained_marker"]; marker {
				object(m["position"])["sequence"] = i + 2
			} else {
				m["sequence"] = i + 2
			}
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return append(bytes.Join(out, []byte("\n")), '\n')
}

func readRow(rows []map[string]any, typ, kind string) map[string]any {
	for _, r := range rows {
		p := object(r["payload"])
		if str(r["payload_type"]) == typ && (kind == "" || str(object(p["event"])["kind"]) == kind) {
			return r
		}
	}
	return nil
}

func TestCommittedReadEffectsSurviveNativeLoss(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, committedRead(t), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-committed-read-effects.golden.jsonl"), append(bytes.Join(out.Records, []byte("\n")), '\n'))
	wire := bytes.Join(out.Records, nil)
	if bytes.Contains(wire, []byte("PRIVATE_")) || bytes.Contains(wire, []byte("read_file")) || bytes.Contains(wire, []byte("file_read\"")) {
		t.Fatal("private read body retained")
	}
	if bytes.Count(wire, []byte(committedFileReadJoin)) != 1 {
		t.Fatal("missing fixed start classification")
	}
	b := bundleFor(t, out)
	encoded, _ := json.Marshal(b)
	b = archive.SourceBundle{}
	if json.Unmarshal(encoded, &b) != nil {
		t.Fatal("source roundtrip")
	}
	a, err := (Parser{}).Parse(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.View.ToolCalls) != 1 || len(a.View.ToolResults) != 0 || a.Observability.ToolResults.Available() || a.Observability.ToolErrors.Available() || a.Observability.FilesTouched.Available() || a.Observability.GitActivity.Available() {
		t.Fatal("invented effect facts")
	}
	call := a.View.ToolCalls[0]
	if call.Name != "" || call.ObservedName != "" || call.Invocation || call.ResultRecordIndex != nil || call.IsError != nil || call.OutputBytes != nil || call.Input != nil {
		t.Fatal("invented call facts")
	}
	for range 2 {
		again, e := (Filter{}).Refilter(t.Context(), b, time.Time{})
		if e != nil || !reflect.DeepEqual(out, again) {
			t.Fatal("refilter drift", e)
		}
		next := bundleFor(t, again)
		nextAnalysis, e := (Parser{}).Parse(t.Context(), next)
		if e != nil || !reflect.DeepEqual(a, nextAnalysis) {
			t.Fatal("retained-only full analysis drift", e)
		}
		if !(Filter{}).EvidenceExtends(b, next) {
			t.Fatal("stable classification lost")
		}
		b = next
	}
	info := archive.ParserInfo{Name: "muse", Version: (Parser{}).Version()}
	m, err := archive.BuildMetadataWithAnalysis(b, a, nil, "synthetic", time.Unix(2000000000, 0), time.Unix(2000000100, 0), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, info)
	if err != nil || m.Counts.ToolCalls == nil || *m.Counts.ToolCalls != 1 || m.Counts.ToolResults != nil || m.Counts.ToolErrors != nil || m.Counts.FilesTouched != nil || len(m.ToolsUsed) != 0 || len(m.GitActivity) != 0 || len(m.MCPCalls) != 0 {
		t.Fatal("metadata drift", err)
	}
	export, err := archive.BuildEvalExportWithAnalysis(b, a, m, archive.EvalExportSourceArchive, archive.EvalExportDetailFull, info)
	if err != nil || !reflect.DeepEqual(export.Counts, m.Counts) {
		t.Fatal("export drift", err)
	}
	handoff, err := archive.BuildHandoffWithAnalysis(b, a, &m, archive.HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := archive.BuildTranscriptWithAnalysis(b, a, archive.HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{handoff, transcript} {
		raw, _ := json.Marshal(value)
		if bytes.Contains(raw, []byte("synthetic-call")) || bytes.Contains(raw, []byte(committedFileReadJoin)) {
			t.Fatal("private classification escaped narrative")
		}
	}
}

func TestCommittedReadNativeJoinsRefuseContradictions(t *testing.T) {
	t.Parallel()
	unchanged := changeRead(t, func(rows []map[string]any) []map[string]any { return rows })
	if _, err := filterBytes(t, unchanged, agentapi.ReadLimits{}); err != nil {
		t.Fatal("mutation helper invalid baseline", err)
	}
	tests := map[string]func([]map[string]any) []map[string]any{}
	mutate := func(fn func(map[string]any, map[string]any)) func([]map[string]any) []map[string]any {
		return func(rows []map[string]any) []map[string]any {
			fn(readRow(rows, "runtime.session", "assistant_tool_calls_committed"), readRow(rows, "tool_batch.effect.started", ""))
			return rows
		}
	}
	for _, name := range []string{"shell", "muse.read_file", "READ_FILE", ""} {
		tests["committed-name-"+name] = mutate(func(call, _ map[string]any) {
			object(object(object(call["payload"])["event"])["tool_calls"].([]any)[0])["name"] = name
		})
	}
	for _, name := range []string{"shell", "muse.read_file", "READ_FILE"} {
		tests["effect-name-"+name] = mutate(func(_, effect map[string]any) { object(object(effect["payload"])["record"])["tool_name"] = name })
	}
	tests["ineligible-profile"] = mutate(func(_, e map[string]any) {
		object(object(e["payload"])["record"])["parallel_profile"] = map[string]any{"kind": "ineligible"}
	})
	tests["native-marker"] = mutate(func(_, e map[string]any) {
		object(object(e["payload"])["record"])["effect_join"] = committedFileReadJoin
	})
	tests["wrong-call"] = mutate(func(c, _ map[string]any) {
		object(object(object(c["payload"])["event"])["tool_calls"].([]any)[0])["call_id"] = "other-call"
	})
	tests["foreign-run"] = mutate(func(c, _ map[string]any) { object(c["payload"])["run_id"] = "99999999-9999-4999-8999-999999999999" })
	tests["missing-call"] = func(rows []map[string]any) []map[string]any {
		c := readRow(rows, "runtime.session", "assistant_tool_calls_committed")
		object(object(c["payload"])["event"])["kind"] = "status"
		return rows
	}
	tests["later-call"] = func(rows []map[string]any) []map[string]any {
		for i, r := range rows {
			if str(r["payload_type"]) == "tool_batch.effect.started" {
				rows[i-1], rows[i] = rows[i], rows[i-1]
				break
			}
		}
		return rows
	}
	tests["missing-prior-task"] = func(rows []map[string]any) []map[string]any {
		for _, r := range rows {
			p := object(r["payload"])
			if str(p["kind"]) == "task" && str(object(p["event"])["kind"]) == "proposed" {
				p["task_id"] = "99999999-9999-4999-8999-999999999999"
			}
		}
		return rows
	}
	tests["later-task"] = func(rows []map[string]any) []map[string]any {
		task, effect := -1, -1
		for i, r := range rows {
			p := object(r["payload"])
			if str(p["kind"]) == "task" && str(object(p["event"])["kind"]) == "proposed" {
				task = i
			}
			if str(r["payload_type"]) == "tool_batch.effect.started" {
				effect = i
			}
		}
		rows[task], rows[effect] = rows[effect], rows[task]
		return rows
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := filterBytes(t, changeRead(t, change), agentapi.ReadLimits{})
			if err == nil || len(out.Records) != 0 {
				t.Fatal("contradictory native read accepted")
			}
		})
	}
}

func TestCommittedReadRetainedMarkerAndTupleAreClosed(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, committedRead(t), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"missing", "unknown", "null", "integer", "terminal", "call", "task", "index", "missing-call", "legacy-call", "future-call", "gap"} {
		t.Run(kind, func(t *testing.T) {
			b := bundleFor(t, out)
			start, terminal, committed := -1, -1, -1
			for i, r := range b.NativeRecords {
				switch str(r["type"]) {
				case "tool_batch.effect.started":
					start = i
				case "tool_batch.effect.terminal":
					terminal = i
				}
				if str(object(r["fields"])["event_kind"]) == "assistant_tool_calls_committed" {
					committed = i
				}
			}
			f := object(b.NativeRecords[start]["fields"])
			switch kind {
			case "missing":
				delete(f, "effect_join")
			case "unknown":
				f["effect_join"] = "future"
			case "null":
				f["effect_join"] = nil
			case "integer":
				f["effect_join"] = 1
			case "terminal":
				object(b.NativeRecords[terminal]["fields"])["effect_join"] = committedFileReadJoin
			case "call":
				f["tool_call_id"] = "other-call"
			case "task":
				f["task_id"] = "99999999-9999-4999-8999-999999999999"
			case "index":
				f["model_call_index"] = json.Number("42")
			case "missing-call":
				object(b.NativeRecords[committed]["fields"])["tool_calls"] = []any{map[string]any{"call_id": "other-call"}}
			case "legacy-call":
				b.NativeRecords[committed]["fields"] = map[string]any{"kind": "run", "event_kind": "assistant_tool_calls_committed"}
			case "future-call":
				b.NativeRecords[committed], b.NativeRecords[start] = b.NativeRecords[start], b.NativeRecords[committed]
				for i, row := range b.NativeRecords {
					if i < 2 {
						continue
					}
					object(row["pos"])["line"] = i
					if str(row["kind"]) == "native_omission" {
						object(row["position"])["sequence"] = i + 1
					} else {
						row["seq"] = i + 1
					}
				}
			case "gap":
				b.Capture.Gaps = nil
			}
			if _, err := (Parser{}).Parse(t.Context(), b); err == nil {
				t.Fatal("invalid retained read accepted")
			}
			if kind != "gap" {
				if _, err := (Filter{}).Refilter(t.Context(), b, time.Time{}); err == nil {
					t.Fatal("invalid read refiltered")
				}
			}
			if (Filter{}).EvidenceExtends(bundleFor(t, out), b) {
				t.Fatal("changed evidence extends")
			}
		})
	}
	// Native model index is deliberately independent of the committed batch slot.
	raw := changeRead(t, func(rows []map[string]any) []map[string]any {
		for _, r := range rows {
			if strings.HasPrefix(str(r["payload_type"]), "tool_batch.effect.") {
				object(object(r["payload"])["record"])["model_call_index"] = json.Number("42")
			}
		}
		return rows
	})
	if _, err := filterBytes(t, raw, agentapi.ReadLimits{}); err != nil {
		t.Fatal("invented model-index slot restriction", err)
	}
}

func TestCommittedReadFrozenStrictSchemaRefusesMarker(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "filter29-retained.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	id := doc.(map[string]any)["$id"].(string)
	if err = compiler.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	out, err := filterBytes(t, committedRead(t), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range out.Records {
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(line))
		if err != nil {
			t.Fatal(err)
		}
		m := v.(map[string]any)
		if str(m["type"]) != "tool_batch.effect.started" {
			continue
		}
		if schema.Validate(v) == nil {
			t.Fatal("old strict schema accepted fixed marker")
		}
		delete(object(m["fields"]), "effect_join")
		if err = schema.Validate(v); err != nil {
			t.Fatal("frozen schema baseline", err)
		}
	}
}

func TestCommittedReadLeaseFailureAndTransfer(t *testing.T) {
	t.Parallel()
	raw := committedRead(t)
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	out, release, err := (Filter{}).FilterLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, budget)
	used, peak := budget.Charged()
	if err != nil || used == 0 || len(out.Records) == 0 {
		release()
		t.Fatal("success lease", err, used)
	}
	t.Logf("conservative charged lease=%d peak=%d", used, peak)
	release()
	release()
	if used, _ = budget.Charged(); used != 0 {
		t.Fatal("success leak")
	}
	for _, limit := range []int64{4095, peak / 2, peak - 1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			budget := agentapi.NewNativeReadBudget(limit)
			out, release, err := (Filter{}).FilterLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, budget)
			release()
			used, _ := budget.Charged()
			if !errors.Is(err, agentapi.ErrReadBudget) || len(out.Records) != 0 || used != 0 {
				t.Fatal("partial allocation leaked", err, used)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, release, err = (Filter{}).FilterLeased(ctx, agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, budget)
	release()
	if used, _ = budget.Charged(); !errors.Is(err, context.Canceled) || used != 0 || len(out.Records) != 0 {
		t.Fatal("cancel leak", err, used)
	}
}

func TestCommittedReadApprovalOverlapNeverFallsThrough(t *testing.T) {
	t.Parallel()
	var request map[string]any
	for line := range bytes.SplitSeq(bytes.TrimSpace(readSynthetic(t, "synthetic-native-omissions.jsonl")), []byte("\n")) {
		m, _, err := decodeObject(line)
		if err != nil {
			t.Fatal(err)
		}
		p := object(m["payload"])
		if str(p["kind"]) == "approval" && str(object(p["event"])["kind"]) == "requested" {
			request = m
			break
		}
	}
	for _, when := range []string{"prior", "later"} {
		for _, overlap := range []string{"matching", "wrong-name", "task-only", "call-only"} {
			t.Run(when+"-"+overlap, func(t *testing.T) {
				raw := changeRead(t, func(rows []map[string]any) []map[string]any {
					wire, _ := json.Marshal(request)
					copy, _, err := decodeObject(wire)
					if err != nil {
						t.Fatal(err)
					}
					e := object(object(copy["payload"])["event"])
					e["tool_name"] = "read_file"
					if overlap == "wrong-name" {
						e["tool_name"] = "shell"
					}
					if overlap == "task-only" {
						e["tool_call_id"] = "other-call"
					}
					if overlap == "call-only" {
						e["task_id"] = "99999999-9999-4999-8999-999999999999"
						object(e["task_stream"])["id"] = e["task_id"]
					}
					index := len(rows)
					if when == "prior" {
						for i, r := range rows {
							if str(r["payload_type"]) == "tool_batch.effect.started" {
								index = i
								break
							}
						}
					}
					return append(append(append([]map[string]any{}, rows[:index]...), copy), rows[index:]...)
				})
				// The request alone is well-formed; refusal is the overlap in either order.
				out, err := filterBytes(t, raw, agentapi.ReadLimits{})
				if err == nil || len(out.Records) != 0 {
					t.Fatal("approval overlap entered read lane")
				}
			})
		}
	}
	// Retained sources cannot combine the classification with an overlapping
	// request either, even though private native names no longer exist.
	out, err := filterBytes(t, committedRead(t), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, when := range []string{"prior", "later"} {
		t.Run("retained-"+when, func(t *testing.T) {
			b := bundleFor(t, out)
			nativeBytes, _ := json.Marshal(request)
			r, err := envelope(nativeBytes, position{Line: 1})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(r)
			var m map[string]any
			if json.Unmarshal(raw, &m) != nil {
				t.Fatal("retained request")
			}
			index := len(b.NativeRecords)
			if when == "prior" {
				for i, row := range b.NativeRecords {
					if str(row["type"]) == "tool_batch.effect.started" {
						index = i
						break
					}
				}
			}
			b.NativeRecords = append(append(append([]map[string]any{}, b.NativeRecords[:index]...), m), b.NativeRecords[index:]...)
			for i, row := range b.NativeRecords {
				row["seq"] = i + 1
				if i >= 2 {
					object(row["pos"])["line"] = i
				}
				if str(row["kind"]) == "native_omission" {
					delete(row, "seq")
					object(row["position"])["sequence"] = i + 1
				}
			}
			if _, err := (Parser{}).Parse(t.Context(), b); err == nil {
				t.Fatal("overlapping retained request accepted")
			}
		})
	}
}

// cancelAfterCallInput cancels only after earlier committed-call rows have
// entered the raw index, before the final source consistency recheck completes.
type cancelAfterCallInput struct {
	memoryInput
	cancel context.CancelFunc
}

func (in cancelAfterCallInput) Check() error { in.cancel(); return context.Canceled }

func TestCommittedReadRefusalReleasesPrivateOwnership(t *testing.T) {
	t.Parallel()
	raw := committedRead(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	out, release, err := (Filter{}).FilterLeased(ctx, agentapi.NativeInput{File: cancelAfterCallInput{memoryInput{bytes.NewReader(raw)}, cancel}}, agentapi.FilterContext{}, budget)
	release()
	used, peak := budget.Charged()
	if !errors.Is(err, context.Canceled) || len(out.Records) != 0 || used != 0 || peak < 100000 {
		t.Fatal("post-call cancellation ownership", err, used, peak)
	}
	bad := changeRead(t, func(rows []map[string]any) []map[string]any {
		r := readRow(rows, "tool_batch.effect.terminal", "")
		object(object(r["payload"])["record"])["call_id"] = "other-call"
		return rows
	})
	out, release, err = (Filter{}).FilterLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(bad)}}, agentapi.FilterContext{}, budget)
	release()
	if used, _ := budget.Charged(); err == nil || len(out.Records) != 0 || used != 0 {
		t.Fatal("late tuple refusal leaked private state", err, used)
	}
}

func TestCommittedReadMultipleEffectsDoNotCreateCalls(t *testing.T) {
	t.Parallel()
	raw := changeRead(t, func(rows []map[string]any) []map[string]any {
		clone := func(original map[string]any, id string) map[string]any {
			wire, _ := json.Marshal(original)
			r, _, err := decodeObject(wire)
			if err != nil {
				t.Fatal(err)
			}
			r["id"] = id
			object(object(r["payload"])["record"])["effect_id"] = "99999999-9999-4999-8999-999999999999"
			return r
		}
		start := clone(readRow(rows, "tool_batch.effect.started", ""), "99999999-9999-4999-8999-999999999997")
		terminal := clone(readRow(rows, "tool_batch.effect.terminal", ""), "99999999-9999-4999-8999-999999999998")
		for i, r := range rows {
			if str(r["payload_type"]) == "tool_batch.effect.terminal" {
				return append(append(append([]map[string]any{}, rows[:i+1]...), start, terminal), rows[i+1:]...)
			}
		}
		t.Fatal("missing terminal")
		return nil
	})
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := (Parser{}).Parse(t.Context(), bundleFor(t, out))
	if err != nil || len(a.View.ToolCalls) != 1 || len(a.View.ToolResults) != 0 {
		t.Fatal("effect multiplicity became call/result facts", err)
	}
}
