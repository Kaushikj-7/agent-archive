package muse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// effectBodies changes only private bodies in the existing synthetic root graph.
func effectBodies(t *testing.T, profile, outcome any) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(readSynthetic(t, "synthetic-native-omissions.jsonl")), []byte("\n"))
	for i, line := range lines {
		m, _, err := decodeObject(line)
		if err != nil {
			t.Fatal(err)
		}
		r := object(object(m["payload"])["record"])
		switch str(m["payload_type"]) {
		case "tool_batch.effect.started":
			r["parallel_profile"] = profile
		case "tool_batch.effect.terminal":
			r["outcome"] = outcome
		default:
			continue
		}
		lines[i], err = json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
	}
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

func readProfile() map[string]any {
	return map[string]any{"kind": "file_read", "subject": "PRIVATE_EFFECT_SUBJECT_SENTINEL"}
}

func completeOutcome() map[string]any {
	return map[string]any{"kind": "completed", "task_completion": map[string]any{"kind": "complete"}, "output_ref_count": json.Number("1")}
}

func failedOutcome() map[string]any {
	return map[string]any{"kind": "failed", "reason": "PRIVATE_EFFECT_REASON_SENTINEL"}
}

func TestToolEffectBodiesPreserveOmissionProjection(t *testing.T) {
	t.Parallel()
	baseline, err := filterBytes(t, readSynthetic(t, "synthetic-native-omissions.jsonl"), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	oldProfile := map[string]any{"kind": "ineligible"}
	oldOutcome := map[string]any{"kind": "completed", "output_ref_count": json.Number("1"), "task_completion": map[string]any{"kind": "terminal", "terminal": map[string]any{"kind": "completed"}}}
	for pi, profile := range []any{oldProfile, readProfile()} {
		for oi, outcome := range []any{oldOutcome, completeOutcome(), failedOutcome()} {
			t.Run(fmt.Sprintf("profile%d-outcome%d", pi, oi), func(t *testing.T) {
				t.Parallel()
				out, err := filterBytes(t, effectBodies(t, profile, outcome), agentapi.ReadLimits{})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(baseline, out) {
					t.Fatal("private body changed retained projection")
				}
				wire := bytes.Join(out.Records, []byte("\n"))
				if bytes.Contains(wire, []byte("PRIVATE_EFFECT_")) {
					t.Fatal("private effect body retained")
				}
				b := bundleFor(t, out)
				a, err := (Parser{}).Parse(t.Context(), b)
				if err != nil {
					t.Fatal(err)
				}
				if len(a.View.ToolCalls) != 0 || len(a.View.ToolResults) != 0 || a.Observability.ToolResults.Available() || a.Observability.ToolErrors.Available() {
					t.Fatal("effect body manufactured tool result facts")
				}
				baseAnalysis, err := (Parser{}).Parse(t.Context(), bundleFor(t, baseline))
				if err != nil || !reflect.DeepEqual(baseAnalysis, a) {
					t.Fatal("effect body changed metadata derivation", err)
				}
				for range 2 {
					again, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
					if err != nil || !reflect.DeepEqual(out, again) {
						t.Fatal("effect refilter changed projection", err)
					}
					b = bundleFor(t, again)
				}
				if !(Filter{}).EvidenceExtends(b, b) {
					t.Fatal("effect identity lost")
				}
			})
		}
	}
}

func TestToolEffectBodiesRejectUnknownShapes(t *testing.T) {
	t.Parallel()
	const (
		taskCompletion fieldName = "task_completion"
		outputRefCount fieldName = "output_ref_count"
	)
	type change struct {
		name    string
		profile bool
		mutate  func(map[string]any)
	}
	cases := []change{}
	for _, spec := range []struct {
		name    string
		profile bool
		value   map[string]any
	}{
		{"profile", true, readProfile()}, {"complete", false, completeOutcome()}, {"failed", false, failedOutcome()},
	} {
		for key := range spec.value {
			badValues := []any{nil, true, []any{}, map[string]any{}}
			if fieldName(key) != outputRefCount {
				badValues = append(badValues, json.Number("2"))
			}
			if fieldName(key) == fieldKind || fieldName(key) == taskCompletion || fieldName(key) == outputRefCount {
				badValues = append(badValues, "future")
			}
			for _, bad := range badValues {
				cases = append(cases, change{fmt.Sprintf("%s-%s-%T", spec.name, key, bad), spec.profile, func(m map[string]any) { m[key] = bad }})
			}
			cases = append(cases, change{spec.name + "-missing-" + key, spec.profile, func(m map[string]any) { delete(m, key) }})
		}
		cases = append(cases, change{spec.name + "-extra", spec.profile, func(m map[string]any) { m["future_private"] = true }})
	}
	// The complete and failed variants have different key sets; mutations must
	// start from their own variant so every refusal tests the intended boundary.
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p, o := readProfile(), completeOutcome()
			if len(c.name) >= 6 && c.name[:6] == "failed" {
				o = failedOutcome()
			}
			target := o
			if c.profile {
				target = p
			}
			c.mutate(target)
			out, err := filterBytes(t, effectBodies(t, p, o), agentapi.ReadLimits{})
			if err == nil || len(out.Records) != 0 {
				t.Fatal("malformed effect body admitted")
			}
		})
	}
	for i, count := range []any{json.Number("0"), json.Number("9007199254740991")} {
		t.Run(fmt.Sprintf("exact-count%d", i), func(t *testing.T) {
			t.Parallel()
			o := completeOutcome()
			o["output_ref_count"] = count
			if _, err := filterBytes(t, effectBodies(t, readProfile(), o), agentapi.ReadLimits{}); err != nil {
				t.Fatal(err)
			}
		})
	}
	for i, bad := range []any{-1, 1.5, json.Number("9007199254740992"), "1", false, nil} {
		t.Run(fmt.Sprintf("count%d", i), func(t *testing.T) {
			t.Parallel()
			o := completeOutcome()
			o["output_ref_count"] = bad
			out, err := filterBytes(t, effectBodies(t, readProfile(), o), agentapi.ReadLimits{})
			if err == nil || len(out.Records) != 0 {
				t.Fatal("invalid exact count admitted")
			}
		})
	}
	for i, bad := range []any{map[string]any{"kind": "completed"}, map[string]any{"kind": "complete", "terminal": map[string]any{"kind": "completed"}}, map[string]any{"kind": "terminal", "terminal": map[string]any{"kind": "complete"}}, map[string]any{"kind": "complete", "future": true}} {
		t.Run(fmt.Sprintf("completion%d", i), func(t *testing.T) {
			t.Parallel()
			o := completeOutcome()
			o["task_completion"] = bad
			out, err := filterBytes(t, effectBodies(t, readProfile(), o), agentapi.ReadLimits{})
			if err == nil || len(out.Records) != 0 {
				t.Fatal("unknown completion admitted")
			}
		})
	}
}

func TestToolEffectBodiesPreserveIdentityJoins(t *testing.T) {
	t.Parallel()
	type identityField string
	const (
		runID          identityField = "run_id"
		stream         identityField = "stream"
		causationID    identityField = "causation_id"
		taskStream     identityField = "task_stream"
		modelCallIndex identityField = "model_call_index"
	)
	for oi, outcome := range []any{completeOutcome(), failedOutcome()} {
		raw := effectBodies(t, readProfile(), outcome)
		lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
		for i, line := range lines {
			m, _, err := decodeObject(line)
			if err != nil {
				t.Fatal(err)
			}
			if str(m["payload_type"]) != "tool_batch.effect.started" && str(m["payload_type"]) != "tool_batch.effect.terminal" {
				continue
			}
			for _, key := range []string{"effect_id", "task_id", "call_id", "model_call_index", "task_stream", "run_id", "stream", "causation_id"} {
				t.Run(fmt.Sprintf("outcome%d-line%d-%s", oi, i, key), func(t *testing.T) {
					t.Parallel()
					clone, _, err := decodeObject(line)
					if err != nil {
						t.Fatal(err)
					}
					p := object(clone["payload"])
					r := object(p["record"])
					switch identityField(key) {
					case runID:
						p[key] = "ffffffff-ffff-4fff-8fff-ffffffffffff"
					case stream:
						object(clone[key])["id"] = "ffffffff-ffff-4fff-8fff-ffffffffffff"
					case causationID:
						clone[key] = json.Number("1")
					case taskStream:
						object(r[key])["id"] = "ffffffff-ffff-4fff-8fff-ffffffffffff"
					case modelCallIndex:
						r[key] = json.Number("1")
					default:
						r[key] = "ffffffff-ffff-4fff-8fff-ffffffffffff"
					}
					assertNativeMutationRefused(t, lines, i, clone)
				})
			}
		}
	}
}

func TestToolEffectBodiesPreserveCommittedCalls(t *testing.T) {
	t.Parallel()
	for i, outcome := range []any{completeOutcome(), failedOutcome()} {
		t.Run(fmt.Sprintf("outcome%d", i), func(t *testing.T) {
			t.Parallel()
			raw := effectBodies(t, readProfile(), outcome)
			base, err := filterBytes(t, raw, agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			tools := bytes.Split(bytes.TrimSpace(ownTools(t)), []byte("\n"))
			for j, line := range tools[len(tools)-2:] {
				m, _, err := decodeObject(line)
				if err != nil {
					t.Fatal(err)
				}
				m["sequence"] = len(base.Records) + j + 1
				object(m["payload"])["source_run_record_sequence"] = 100 + j
				wire, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				raw = append(raw, append(wire, '\n')...)
			}
			out, err := filterBytes(t, raw, agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			b := bundleFor(t, out)
			a, err := (Parser{}).Parse(t.Context(), b)
			if err != nil || len(a.View.ToolCalls) != 3 || len(a.View.ToolResults) != 0 || a.Observability.ToolResults.Available() || a.Observability.ToolErrors.Available() {
				t.Fatal("effect changed committed call availability", err)
			}
			for j, c := range a.View.ToolCalls {
				if c.CallID != fmt.Sprintf("call_synthetic_%d", j) || (j != 1) != (c.ResultRecordIndex != nil) || c.IsError != nil || c.OutputBytes != nil {
					t.Fatal("effect changed committed call join")
				}
			}
			next, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
			if err != nil || !reflect.DeepEqual(out.Records, next.Records) {
				t.Fatal("combined effect/call refilter", err)
			}
		})
	}
}
