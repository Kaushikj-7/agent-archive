package muse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
	"os"
	"path/filepath"
)

// completionNative adds newly synthetic ordinary records to a checksum-valid
// synthetic permission root. The unchanged frame still authenticates its children.
func completionNative(t *testing.T, count int, input, output int64) []byte {
	t.Helper()
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	var previous map[string]any
	if json.Unmarshal(lines[len(lines)-1], &previous) != nil {
		t.Fatal("envelope")
	}
	for i := range count {
		m := map[string]any{}
		b, _ := json.Marshal(previous)
		_ = json.Unmarshal(b, &m)
		m["id"] = fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012x", i+1)
		m["sequence"] = 10 + i
		m["recorded_at"] = 1792083855882800 + i
		m["payload_type"] = "runtime.session"
		m["causation_id"] = nil
		m["payload"] = map[string]any{"kind": "run", "run_id": "33333333-3333-4333-8333-333333333333", "source_run_record_id": fmt.Sprintf("bbbbbbbb-bbbb-4bbb-8bbb-%012x", i+1), "source_run_record_sequence": 3 + i, "event": map[string]any{"kind": "model_completed", "duration_ms": 0, "model": "claude-sonnet-4", "usage": map[string]any{"input_tokens": input, "output_tokens": output, "cached_tokens": 17, "cache_read_tokens": 17, "cache_write_tokens": 0, "reasoning_tokens": 5}}}
		b, _ = json.Marshal(m)
		lines = append(lines, b)
	}
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

func TestOwnCompletionObservationsSurviveNativeLoss(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		n      int
		input  int64
		output int64
	}{{"single", 1, 101, 23}, {"same-run-two", 2, 101, 23}, {"recorded-zero", 1, 0, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := filterBytes(t, completionNative(t, tc.n, tc.input, tc.output), agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			b := bundleFor(t, out)
			a, err := (Parser{}).Parse(t.Context(), b)
			if err != nil {
				t.Fatal(err)
			}
			s := a.TokenObservations
			if s == nil || !s.Observed.Available() || *s.Input != int64(tc.n)*tc.input || *s.Output != int64(tc.n)*tc.output || s.Coverage.Available() || s.Composition.Available() || s.Pricing.Available() || a.View.Tokens.Input != nil || len(a.View.ModelTokens) != 0 {
				t.Fatalf("wrong observations %+v", s)
			}
			retained := bytes.Join(out.Records, nil)
			for _, key := range []string{"duration_ms", "cached_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "usage\""} {
				if bytes.Contains(retained, []byte(key)) {
					t.Fatalf("retained private %s", key)
				}
			}
			next, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
			if err != nil || !reflect.DeepEqual(out.Records, next.Records) || !(Filter{}).EvidenceExtends(b, b) {
				t.Fatal("post-loss agreement", err)
			}
			parser := archive.ParserInfo{Name: "muse", Version: (Parser{}).Version()}
			m, err := archive.BuildMetadataWithAnalysis(b, a, nil, "synthetic", time.Unix(2000000000, 0), time.Unix(2000000100, 0), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, parser)
			if err != nil {
				t.Fatal(err)
			}
			if m.Counts.InputTokens != nil || m.Counts.OutputTokens != nil || len(m.ModelTokens) != 0 || !reflect.DeepEqual(s, m.TokenObservations) {
				t.Fatal("legacy accounting populated")
			}
			e, err := archive.BuildEvalExportWithAnalysis(b, a, m, archive.EvalExportSourceArchive, archive.EvalExportDetailFull, parser)
			if err != nil || !reflect.DeepEqual(s, e.TokenObservations) {
				t.Fatal("export observations", err)
			}
			if err := archive.ValidateEvalExport(e); err != nil {
				t.Fatal(err)
			}
			validateObservationWire(t, "metadata.schema.json", m)
			validateObservationWire(t, "eval-export.schema.json", e)
			stale := m
			stale.Parser.Version = "0.3.0"
			stale.TokenObservations = nil
			refreshed, err := archive.BuildEvalExportWithAnalysis(b, a, stale, archive.EvalExportSourceArchive, archive.EvalExportDetailFull, parser)
			if err != nil || !reflect.DeepEqual(s, refreshed.TokenObservations) {
				t.Fatal("parser refresh lost observations", err)
			}
			h, err := archive.BuildHandoffWithAnalysis(b, a, &m, archive.HandoffOptions{})
			if err != nil || !reflect.DeepEqual(s, h.Session.TokenObservations) || !strings.Contains(string(archive.RenderHandoffMarkdown(h, archive.HandoffRenderOptions{})), "full usage, composition and cost unavailable") {
				t.Fatal("handoff observations", err)
			}
			// An older typed metadata reader ignores the optional envelope and sees no billable fields.
			raw, _ := json.Marshal(m)
			var old struct {
				Counts      archive.Counts        `json:"counts"`
				ModelTokens []archive.ModelTokens `json:"model_tokens"`
			}
			if json.Unmarshal(raw, &old) != nil || old.Counts.InputTokens != nil || len(old.ModelTokens) != 0 {
				t.Fatal("old reader priceability")
			}
			for _, code := range []string{"own_usage_observations_partial", "cache_reasoning_cost_unqualified"} {
				broken := cloneBundle(t, b)
				broken.Capture.Gaps = nil
				for _, g := range b.Capture.Gaps {
					if g.Code != code {
						broken.Capture.Gaps = append(broken.Capture.Gaps, g)
					}
				}
				if _, err := (Parser{}).Parse(t.Context(), broken); err == nil {
					t.Fatal("concealed observation coverage")
				}
				repaired, err := (Filter{}).Refilter(t.Context(), broken, time.Time{})
				if err != nil || !reflect.DeepEqual(out.Gaps, repaired.Gaps) {
					t.Fatal("gap reconstruction", err)
				}
			}
			if tc.name == "same-run-two" {
				golden.Check(t, filepath.Join("testdata", "synthetic-own-observations.golden.jsonl"), append(bytes.Join(out.Records, []byte("\n")), '\n'))
			}
		})
	}
}

func TestNativeCompletionClosedShapeRefusesWholeFile(t *testing.T) {
	t.Parallel()
	mutations := []func(map[string]any){
		func(m map[string]any) { object(m["payload"])["run_id"] = "cccccccc-cccc-4ccc-8ccc-cccccccccccc" },
		func(m map[string]any) {
			object(m["payload"])["source_run_record_id"] = "66666666-6666-4666-8666-666666666666"
		},
		func(m map[string]any) { object(m["payload"])["source_run_record_sequence"] = 2 },
		func(m map[string]any) { m["causation_id"] = "cccccccc-cccc-4ccc-8ccc-cccccccccccc" },
	}
	for _, key := range []string{"duration_ms", "model", "usage"} {
		k := key
		mutations = append(mutations, func(m map[string]any) { delete(object(object(m["payload"])["event"]), k) })
	}
	for _, key := range []string{"input_tokens", "output_tokens", "cached_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens"} {
		k := key
		mutations = append(mutations, func(m map[string]any) { delete(object(object(object(m["payload"])["event"])["usage"]), k) })
		for _, bad := range []any{nil, true, -1, 1.5, json.Number("9007199254740992")} {
			k, v := key, bad
			mutations = append(mutations, func(m map[string]any) { object(object(object(m["payload"])["event"])["usage"])[k] = v })
		}
	}
	mutations = append(mutations, func(m map[string]any) { object(object(m["payload"])["event"])["future"] = 0 }, func(m map[string]any) { object(object(object(m["payload"])["event"])["usage"])["future"] = 0 }, func(m map[string]any) {
		object(object(m["payload"])["event"])["model"] = map[string]any{"id": "unknown"}
	}, func(m map[string]any) { object(object(m["payload"])["event"])["model"] = "" })

	for _, bad := range []any{nil, true, -1, 1.5, json.Number("9007199254740992")} {
		v := bad
		mutations = append(mutations, func(m map[string]any) { object(object(m["payload"])["event"])["duration_ms"] = v })
	}
	mutations = append(mutations, func(m map[string]any) { object(object(m["payload"])["event"])["model"] = strings.Repeat("x", 129) })
	for i, mutate := range mutations {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			lines := bytes.Split(bytes.TrimSpace(completionNative(t, 1, 101, 23)), []byte("\n"))
			var m map[string]any
			_ = json.Unmarshal(lines[len(lines)-1], &m)
			mutate(m)
			lines[len(lines)-1], _ = json.Marshal(m)
			out, err := filterBytes(t, append(bytes.Join(lines, []byte("\n")), '\n'), agentapi.ReadLimits{})
			if err == nil || len(out.Records) != 0 {
				t.Fatal("unsupported completion accepted")
			}
		})
	}
}

func TestRetainedCompletionLegacyAndEnrichmentClosure(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, completionNative(t, 2, 101, 23), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	b := bundleFor(t, out)
	last := len(b.NativeRecords) - 1
	legacy := cloneBundle(t, b)
	object(legacy.NativeRecords[last])["fields"] = map[string]any{"kind": "run", "event_kind": "model_completed"}
	a, err := (Parser{}).Parse(t.Context(), legacy)
	if err != nil || *a.TokenObservations.Input != 101 {
		t.Fatal("mixed legacy", err)
	}
	legacy.NativeRecords[last-1]["fields"] = map[string]any{"kind": "run", "event_kind": "model_completed"}
	a, err = (Parser{}).Parse(t.Context(), legacy)
	if err != nil || a.TokenObservations != nil {
		t.Fatal("legacy invented observations", err)
	}
	for _, key := range []string{"run_id", "source_run_record_id", "source_run_record_sequence", "model", "input_tokens", "output_tokens"} {
		bad := cloneBundle(t, b)
		delete(object(bad.NativeRecords[last]["fields"]), key)
		assertRetainedRefused(t, bad)
	}
	bad := cloneBundle(t, b)
	object(bad.NativeRecords[last]["fields"])["source_run_record_id"] = object(bad.NativeRecords[last-1]["fields"])["source_run_record_id"]
	assertRetainedRefused(t, bad)
	bad = cloneBundle(t, b)
	object(bad.NativeRecords[last]["fields"])["input_tokens"] = json.Number("9007199254740991")
	assertRetainedRefused(t, bad)
	// Frozen actual filter23 schema refuses the enriched variant.
	raw, err := os.ReadFile(filepath.Join("testdata", "filter23-retained.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	id := doc.(map[string]any)["$id"].(string)
	if compiler.AddResource(id, doc) != nil {
		t.Fatal("schema")
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	if schema.Validate(b.NativeRecords[last]) == nil {
		t.Fatal("old schema accepted enriched observations")
	}
}

func TestCompletionSourceIdentityIsScopedToRun(t *testing.T) {
	t.Parallel()
	raw := completionNative(t, 1, 101, 23)
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	replacements := map[string]string{
		"22222222-2222-4222-8222-222222222222": "22222222-2222-4222-8222-222222222223",
		"33333333-3333-4333-8333-333333333333": "33333333-3333-4333-8333-333333333334",
		"00000000-0000-0000-0000-0000000003ed": "cccccccc-cccc-4ccc-8ccc-000000000001",
		"00000000-0000-0000-0000-0000000003ee": "cccccccc-cccc-4ccc-8ccc-000000000002",
	}
	// Ordinary records5..8 and10 contain the second accepted→started→materialized
	// graph and completion; the checksum-valid initial frame remains unchanged.
	for i, index := range []int{3, 4, 5, 6, 8} {
		body := string(lines[index])
		for old, next := range replacements {
			body = strings.ReplaceAll(body, old, next)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(body), &m)
		m["sequence"] = 11 + i
		m["id"] = fmt.Sprintf("cccccccc-cccc-4ccc-8ccc-%012x", i+1)
		if i == 2 {
			object(m["payload"])["envelope_sequence"] = 11
		}
		encoded, _ := json.Marshal(m)
		lines = append(lines, encoded)
	}
	out, err := filterBytes(t, append(bytes.Join(lines, []byte("\n")), '\n'), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := (Parser{}).Parse(t.Context(), bundleFor(t, out))
	if err != nil || *a.TokenObservations.Input != 202 || *a.TokenObservations.Output != 46 {
		t.Fatal("source Q coalesced across runs", err)
	}
}

func validateObservationWire(t *testing.T, name string, value any) {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	id := ""
	for _, file := range []string{"metadata.schema.json", "eval-export.schema.json"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "schemas", file))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		resource := doc.(map[string]any)["$id"].(string)
		if err = c.AddResource(resource, doc); err != nil {
			t.Fatal(err)
		}
		if file == name {
			id = resource
		}
	}
	schema, err := c.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err = schema.Validate(doc); err != nil {
		t.Fatal(err)
	}
}

func TestCompletionObservationsIgnoreRollupsAndPreserveOmissions(t *testing.T) {
	t.Parallel()
	raw := completionNative(t, 1, 101, 23)
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	var m map[string]any
	_ = json.Unmarshal(lines[len(lines)-1], &m)
	for i, kind := range []string{"goal_usage_attribution", "resource_usage_sampled", "terminal"} {
		m["id"] = fmt.Sprintf("eeeeeeee-eeee-4eee-8eee-%012x", i+1)
		m["sequence"] = 11 + i
		p := object(m["payload"])
		p["source_run_record_sequence"] = 4 + i
		p["event"] = map[string]any{"kind": kind, "usage": map[string]any{"input_tokens": 999, "output_tokens": 999}, "private": "PRIVATE_ROLLUP"}
		encoded, _ := json.Marshal(m)
		lines = append(lines, encoded)
	}
	out, err := filterBytes(t, append(bytes.Join(lines, []byte("\n")), '\n'), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := (Parser{}).Parse(t.Context(), bundleFor(t, out))
	if err != nil || *a.TokenObservations.Input != 101 || bytes.Contains(bytes.Join(out.Records, nil), []byte("PRIVATE_ROLLUP")) {
		t.Fatal("counted rollups", err)
	}
	// Compose the enriched branch with620's ordinary/marker omission union.
	omitted := readSynthetic(t, "synthetic-native-omissions.jsonl")
	base, err := filterBytes(t, omitted, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	var completion map[string]any
	one := bytes.Split(bytes.TrimSpace(completionNative(t, 1, 101, 23)), []byte("\n"))
	_ = json.Unmarshal(one[len(one)-1], &completion)
	completion["sequence"] = len(base.Records) + 1
	p := object(completion["payload"])
	p["source_run_record_sequence"] = 100
	encoded, _ := json.Marshal(completion)
	combined := append(append([]byte(nil), omitted...), append(encoded, '\n')...)
	out, err = filterBytes(t, combined, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	b := bundleFor(t, out)
	a, err = (Parser{}).Parse(t.Context(), b)
	if err != nil || *a.TokenObservations.Input != 101 {
		t.Fatal("omission union observations", err)
	}
	again, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
	if err != nil || !reflect.DeepEqual(out.Records, again.Records) || !(Filter{}).EvidenceExtends(b, b) {
		t.Fatal("combined retained union", err)
	}
}

func TestCompletionCountersDoNotAssertComposition(t *testing.T) {
	t.Parallel()
	lines := bytes.Split(bytes.TrimSpace(completionNative(t, 1, 0, 1)), []byte("\n"))
	var m map[string]any
	_ = json.Unmarshal(lines[len(lines)-1], &m)
	usage := object(object(object(m["payload"])["event"])["usage"])
	// Counter presence and exact bounds are qualified; no overlap or alias
	// arithmetic is inferred from these deliberately unrelated synthetic values.
	usage["cached_tokens"] = 99
	usage["cache_read_tokens"] = 100
	usage["cache_write_tokens"] = 101
	usage["reasoning_tokens"] = 102
	object(object(m["payload"])["event"])["model"] = "token = PRIVATE_MODEL_SENTINEL"
	lines[len(lines)-1], _ = json.Marshal(m)
	out, err := filterBytes(t, append(bytes.Join(lines, []byte("\n")), '\n'), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.Join(out.Records, nil), []byte("PRIVATE_MODEL_SENTINEL")) {
		t.Fatal("model label bypassed shared privacy")
	}
	a, err := (Parser{}).Parse(t.Context(), bundleFor(t, out))
	if err != nil || *a.TokenObservations.Input != 0 || *a.TokenObservations.Output != 1 || a.TokenObservations.Composition.Available() || a.TokenObservations.Pricing.Available() {
		t.Fatal("inferred composition", err)
	}
}
