package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

const broadReadJoinMarker = "committed_broad_read_v1"

// This derives a native-shaped synthetic graph, never actual private evidence.
func changeBroadRead(t *testing.T, change func([]map[string]any) []map[string]any) []byte {
	t.Helper()
	return changeRead(t, func(rows []map[string]any) []map[string]any {
		call := readRow(rows, "runtime.session", "assistant_tool_calls_committed")
		item := object(object(object(call["payload"])["event"])["tool_calls"].([]any)[0])
		item["name"] = "read_skill"
		item["args"] = `{"name":"PRIVATE_SKILL_NAME_SENTINEL"}`
		start := readRow(rows, "tool_batch.effect.started", "")
		rec := object(object(start["payload"])["record"])
		rec["tool_name"] = "read_skill"
		rec["parallel_profile"] = map[string]any{"kind": "broad_read"}
		if change != nil {
			return change(rows)
		}
		return rows
	})
}

func TestCommittedBroadReadOmissionPipeline(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, changeBroadRead(t, nil), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal("qualified committed broad-read refused", err)
	}
	file, err := filterBytes(t, committedRead(t), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	expected := file
	expected.Records = make([][]byte, len(file.Records))
	for i, row := range file.Records {
		expected.Records[i] = bytes.ReplaceAll(row, []byte(committedFileReadJoin), []byte(broadReadJoinMarker))
	}
	expected.Boundary.RetainedBytes = 0
	for _, row := range expected.Records {
		expected.Boundary.RetainedBytes += len(row)
	}
	if !reflect.DeepEqual(expected, out) {
		t.Fatal("omission class changed unrelated file-read projection")
	}
	wire := bytes.Join(out.Records, nil)
	if bytes.Count(wire, []byte(broadReadJoinMarker)) != 1 || bytes.Contains(wire, []byte(committedFileReadJoin)) {
		t.Fatal("incorrect omission class")
	}
	for _, secret := range []string{"read_skill", "parallel_profile", "PRIVATE_", "args", "subject"} {
		if bytes.Contains(wire, []byte(secret)) {
			t.Fatal("private broad-read evidence retained")
		}
	}
	b := bundleFor(t, out)
	a, err := (Parser{}).Parse(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.View.ToolCalls) != 1 || len(a.View.NativeSkillUses) != 0 || len(a.View.ToolResults) != 0 {
		t.Fatal("omission class inflated skill use or result facts")
	}
	c := a.View.ToolCalls[0]
	if c.Name != "" || c.ObservedName != "" || c.Invocation || c.Input != nil || c.ResultRecordIndex != nil || c.OutputBytes != nil || c.IsError != nil {
		t.Fatal("omission class manufactured private call facts")
	}
	info := archive.ParserInfo{Name: "muse", Version: (Parser{}).Version()}
	metadata, err := archive.BuildMetadataWithAnalysis(b, a, nil, "synthetic", time.Unix(2000000000, 0), time.Unix(2000000100, 0), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, info)
	if err != nil || len(metadata.SkillsUsed) != 0 || len(metadata.ToolsUsed) != 0 || metadata.Counts.ToolResults != nil {
		t.Fatal("omission class manufactured usage", err)
	}
	for range 2 {
		again, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
		if err != nil || !reflect.DeepEqual(out, again) {
			t.Fatal("broad-read refilter drift", err)
		}
		next := bundleFor(t, again)
		analysis, err := (Parser{}).Parse(t.Context(), next)
		if err != nil || !reflect.DeepEqual(a, analysis) || !(Filter{}).EvidenceExtends(b, next) {
			t.Fatal("retained proof drift", err)
		}
		b = next
	}
}

func TestCommittedBroadReadRawRefusals(t *testing.T) {
	t.Parallel()
	cases := map[string]func(map[string]any, map[string]any){
		"committed-name": func(c, e map[string]any) { c["name"] = "read_file" },
		"start-name":     func(c, e map[string]any) { e["tool_name"] = "read_file" },
		"namespace-name": func(c, e map[string]any) { c["name"] = "muse.read_skill"; e["tool_name"] = "muse.read_skill" },
		"empty-name":     func(c, e map[string]any) { c["name"] = ""; e["tool_name"] = "" },
		"call":           func(c, e map[string]any) { e["call_id"] = "other-call" },
		"profile-subject": func(c, e map[string]any) {
			e["parallel_profile"] = map[string]any{"kind": "broad_read", "subject": "PRIVATE_SUBJECT"}
		},
		"profile-file-read": func(c, e map[string]any) { e["parallel_profile"] = readProfile() },
		"profile-null":      func(c, e map[string]any) { e["parallel_profile"] = nil },
		"profile-extra":     func(c, e map[string]any) { e["parallel_profile"] = map[string]any{"kind": "broad_read", "extra": true} },
		"profile-future":    func(c, e map[string]any) { e["parallel_profile"] = map[string]any{"kind": "future"} },
		"raw-marker":        func(c, e map[string]any) { e["effect_join"] = broadReadJoinMarker },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw := changeBroadRead(t, func(rows []map[string]any) []map[string]any {
				call := readRow(rows, "runtime.session", "assistant_tool_calls_committed")
				c := object(object(object(call["payload"])["event"])["tool_calls"].([]any)[0])
				e := object(object(readRow(rows, "tool_batch.effect.started", "")["payload"])["record"])
				mutate(c, e)
				return rows
			})
			out, err := filterBytes(t, raw, agentapi.ReadLimits{})
			if err == nil || len(out.Records) != 0 {
				t.Fatal("unqualified broad-read admitted")
			}
		})
	}
	for _, kind := range []string{"foreign-owner", "foreign-run", "foreign-task", "later-call", "missing-call", "duplicate-call", "terminal-before-start"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			raw := changeBroadRead(t, func(rows []map[string]any) []map[string]any {
				call := readRow(rows, "runtime.session", "assistant_tool_calls_committed")
				start := readRow(rows, "tool_batch.effect.started", "")
				switch kind {
				case "foreign-owner":
					object(start["stream"])["id"] = "99999999-9999-4999-8999-999999999999"
				case "foreign-run":
					object(call["payload"])["run_id"] = "99999999-9999-4999-8999-999999999999"
				case "foreign-task":
					for _, row := range rows {
						if strings.HasPrefix(str(row["payload_type"]), "tool_batch.effect.") {
							rec := object(object(row["payload"])["record"])
							rec["task_id"] = "99999999-9999-4999-8999-999999999999"
							object(rec["task_stream"])["id"] = rec["task_id"]
						}
					}
				case "missing-call":
					object(object(call["payload"])["event"])["tool_calls"] = []any{}
				case "duplicate-call":
					e := object(object(call["payload"])["event"])
					items := e["tool_calls"].([]any)
					e["tool_calls"] = append(items, items[0])
				case "later-call":
					for i, row := range rows {
						if str(row["payload_type"]) == "tool_batch.effect.started" {
							rows[i-1], rows[i] = rows[i], rows[i-1]
							break
						}
					}
				case "terminal-before-start":
					first, last := -1, -1
					for i, row := range rows {
						if str(row["payload_type"]) == "tool_batch.effect.started" {
							first = i
						}
						if str(row["payload_type"]) == "tool_batch.effect.terminal" {
							last = i
						}
					}
					rows[first], rows[last] = rows[last], rows[first]
				}
				return rows
			})
			out, err := filterBytes(t, raw, agentapi.ReadLimits{})
			if err == nil || len(out.Records) != 0 {
				t.Fatal("unqualified broad-read identity/order admitted")
			}
		})
	}
}

func TestCommittedBroadReadRetainedProofRefusals(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, changeBroadRead(t, nil), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"missing-marker", "unknown-marker", "null-marker", "terminal-marker", "call", "task", "index", "owner", "missing-call", "duplicate-start"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			b := bundleFor(t, out)
			start, terminal, committed := -1, -1, -1
			for i, row := range b.NativeRecords {
				switch str(row["type"]) {
				case "tool_batch.effect.started":
					start = i
				case "tool_batch.effect.terminal":
					terminal = i
				}
				if str(object(row["fields"])["event_kind"]) == "assistant_tool_calls_committed" {
					committed = i
				}
			}
			f := object(b.NativeRecords[start]["fields"])
			switch kind {
			case "missing-marker":
				delete(f, "effect_join")
			case "unknown-marker":
				f["effect_join"] = "future"
			case "null-marker":
				f["effect_join"] = nil
			case "terminal-marker":
				object(b.NativeRecords[terminal]["fields"])["effect_join"] = broadReadJoinMarker
			case "call":
				f["tool_call_id"] = "other-call"
			case "task":
				f["task_id"] = "99999999-9999-4999-8999-999999999999"
			case "index":
				f["model_call_index"] = json.Number("42")
			case "owner":
				object(b.NativeRecords[start]["stream"])["id"] = "99999999-9999-4999-8999-999999999999"
			case "missing-call":
				object(b.NativeRecords[committed]["fields"])["tool_calls"] = []any{map[string]any{"call_id": "other-call"}}
			case "duplicate-start":
				b.NativeRecords[terminal] = b.NativeRecords[start]
			}
			if _, err := (Parser{}).Parse(t.Context(), b); err == nil {
				t.Fatal("invalid retained broad-read accepted")
			}
			if _, err := (Filter{}).Refilter(t.Context(), b, time.Time{}); err == nil {
				t.Fatal("invalid retained broad-read refiltered")
			}
			if (Filter{}).EvidenceExtends(bundleFor(t, out), b) {
				t.Fatal("changed broad-read evidence extends")
			}
		})
	}
}

func TestCommittedBroadReadFrozenStrictSchema(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "filter30-retained.schema.json"))
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
	for _, broad := range []bool{false, true} {
		raw := committedRead(t)
		if broad {
			raw = changeBroadRead(t, nil)
		}
		out, err := filterBytes(t, raw, agentapi.ReadLimits{})
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
			err = schema.Validate(v)
			if broad && err == nil {
				t.Fatal("old strict file-read schema accepted broad class")
			}
			if !broad && err != nil {
				t.Fatal("old strict file-read bytes changed", err)
			}
		}
	}
}

func TestCommittedBroadReadLeaseAndCancellation(t *testing.T) {
	t.Parallel()
	raw := changeBroadRead(t, nil)
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	out, release, err := (Filter{}).FilterLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, budget)
	used, peak := budget.Charged()
	if err != nil || used == 0 || len(out.Records) == 0 {
		release()
		t.Fatal("broad-read success lease", err)
	}
	release()
	release()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("broad-read success leak")
	}
	for _, limit := range []int64{4095, peak / 2, peak - 1} {
		b := agentapi.NewNativeReadBudget(limit)
		out, release, err := (Filter{}).FilterLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, b)
		release()
		used, _ := b.Charged()
		if !errors.Is(err, agentapi.ErrReadBudget) || len(out.Records) != 0 || used != 0 {
			t.Fatal("broad-read partial allocation leak", err, used)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, release, err = (Filter{}).FilterLeased(ctx, agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, budget)
	release()
	if used, _ := budget.Charged(); !errors.Is(err, context.Canceled) || used != 0 || len(out.Records) != 0 {
		t.Fatal("broad-read cancel leak", err, used)
	}
}

func TestCommittedBroadReadApprovalOverlap(t *testing.T) {
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
	if request == nil {
		t.Fatal("synthetic approval request unavailable")
	}
	for _, when := range []string{"prior", "later"} {
		for _, overlap := range []string{"matching", "wrong-name", "task-only", "call-only"} {
			t.Run(when+"-"+overlap, func(t *testing.T) {
				t.Parallel()
				raw := changeBroadRead(t, func(rows []map[string]any) []map[string]any {
					wire, _ := json.Marshal(request)
					copy, _, err := decodeObject(wire)
					if err != nil {
						t.Fatal(err)
					}
					e := object(object(copy["payload"])["event"])
					e["tool_name"] = "read_skill"
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
						for i, row := range rows {
							if str(row["payload_type"]) == "tool_batch.effect.started" {
								index = i
								break
							}
						}
					}
					return append(append(append([]map[string]any{}, rows[:index]...), copy), rows[index:]...)
				})
				out, err := filterBytes(t, raw, agentapi.ReadLimits{})
				if err == nil || len(out.Records) != 0 {
					t.Fatal("approval overlap admitted broad-read lane")
				}
			})
		}
	}
	// Retained proofs reject either ordering independently of erased native names.
	out, err := filterBytes(t, changeBroadRead(t, nil), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, when := range []string{"prior", "later"} {
		t.Run("retained-"+when, func(t *testing.T) {
			t.Parallel()
			b := bundleFor(t, out)
			wire, _ := json.Marshal(request)
			r, err := envelope(wire, position{Line: 1})
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(r)
			var row map[string]any
			if json.Unmarshal(encoded, &row) != nil {
				t.Fatal("request projection")
			}
			index := len(b.NativeRecords)
			if when == "prior" {
				for i, item := range b.NativeRecords {
					if str(item["type"]) == "tool_batch.effect.started" {
						index = i
						break
					}
				}
			}
			b.NativeRecords = append(append(append([]map[string]any{}, b.NativeRecords[:index]...), row), b.NativeRecords[index:]...)
			for i, item := range b.NativeRecords {
				if i < 2 {
					continue
				}
				object(item["pos"])["line"] = i
				if str(item["kind"]) == "native_omission" {
					object(item["position"])["sequence"] = i + 1
				} else {
					item["seq"] = i + 1
				}
			}
			if _, err := (Parser{}).Parse(t.Context(), b); err == nil {
				t.Fatal("retained broad-read overlap accepted")
			}
			if _, err := (Filter{}).Refilter(t.Context(), b, time.Time{}); err == nil {
				t.Fatal("retained broad-read overlap refiltered")
			}
		})
	}
}

func TestCommittedBroadReadLateRefusalReleasesOwnership(t *testing.T) {
	t.Parallel()
	raw := changeBroadRead(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	out, release, err := (Filter{}).FilterLeased(ctx, agentapi.NativeInput{File: cancelAfterCallInput{memoryInput{bytes.NewReader(raw)}, cancel}}, agentapi.FilterContext{}, budget)
	release()
	used, peak := budget.Charged()
	if !errors.Is(err, context.Canceled) || len(out.Records) != 0 || used != 0 || peak < 100000 {
		t.Fatal("broad-read post-call ownership", err, used, peak)
	}
	bad := changeBroadRead(t, func(rows []map[string]any) []map[string]any {
		row := readRow(rows, "tool_batch.effect.terminal", "")
		object(object(row["payload"])["record"])["call_id"] = "other-call"
		return rows
	})
	out, release, err = (Filter{}).FilterLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(bad)}}, agentapi.FilterContext{}, budget)
	release()
	if used, _ := budget.Charged(); err == nil || len(out.Records) != 0 || used != 0 {
		t.Fatal("broad-read late refusal ownership", err, used)
	}
}
