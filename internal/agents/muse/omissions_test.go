package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func TestOmittedNativeFamiliesSurviveNativeLoss(t *testing.T) {
	raw := readSynthetic(t, "synthetic-native-omissions.jsonl")
	out, e := filterBytes(t, raw, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(bytes.Join(out.Records, nil), []byte("PRIVATE_NATIVE_BODY_SENTINEL")) {
		t.Fatal("private body retained")
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-native-omissions.golden.jsonl"), append(bytes.Join(out.Records, []byte("\n")), '\n'))
	b := bundleFor(t, out)
	rawBundle, e := json.Marshal(b)
	if e != nil {
		t.Fatal(e)
	}
	if json.Unmarshal(rawBundle, &b) != nil {
		t.Fatal("bundle roundtrip")
	}
	a, e := (Parser{}).Parse(t.Context(), b)
	if e != nil || len(a.View.Turns) != 2 {
		t.Fatal("own conversation lost")
	}
	for _, code := range []omissionGapMutation{omitMarkerGap, omitBodyGap, omitBothGaps} {
		broken := cloneBundle(t, b)
		broken.Capture.Gaps = nil
		for _, gap := range b.Capture.Gaps {
			if omissionGapMutation(gap.Code) != code && (code != omitBothGaps || omissionGapMutation(gap.Code) != omitMarkerGap && omissionGapMutation(gap.Code) != omitBodyGap) {
				broken.Capture.Gaps = append(broken.Capture.Gaps, gap)
			}
		}
		if _, err := (Parser{}).Parse(t.Context(), broken); err == nil {
			t.Fatal("concealed omission accepted")
		}
		repaired, err := (Filter{}).Refilter(t.Context(), broken, time.Time{})
		if err != nil || !reflect.DeepEqual(out.Gaps, repaired.Gaps) {
			t.Fatal("omission repair failed")
		}
		if _, err := (Parser{}).Parse(t.Context(), bundleFor(t, repaired)); err != nil {
			t.Fatal("repaired omission refused")
		}
	}
	for range 2 {
		next, e := (Filter{}).Refilter(t.Context(), b, time.Time{})
		if e != nil || !reflect.DeepEqual(out.Records, next.Records) || !reflect.DeepEqual(out.Gaps, next.Gaps) {
			t.Fatal("native-loss projection or deterministic gaps changed")
		}
		b = bundleFor(t, next)
	}
	if !(Filter{}).EvidenceExtends(b, b) {
		t.Fatal("equal evidence refused")
	}
	for i, r := range b.NativeRecords {
		if num(r["v"]) == 2 {
			if _, present := r["ts_us"]; present {
				t.Fatal("invented timestamp")
			}
			if num(object(r["position"])["sequence"]) != int64(i+1) || num(object(r["pos"])["line"]) != int64(i) {
				t.Fatal("marker position changed")
			}
			for _, mutation := range []func(map[string]any){func(m map[string]any) { m["ts_us"] = 1 }, func(m map[string]any) { m["kind"] = "future" }, func(m map[string]any) { object(m["omitted_record"])["durability"] = "durable" }, func(m map[string]any) { object(m["position"])["id"] = b.NativeRecords[0]["id"] }} {
				clone := cloneBundle(t, b)
				mutation(clone.NativeRecords[i])
				assertRetainedRefused(t, clone)
				if (Filter{}).EvidenceExtends(b, clone) {
					t.Fatal("marker conflict extended evidence")
				}
			}
			deleted := cloneBundle(t, b)
			deleted.NativeRecords = append(deleted.NativeRecords[:i], deleted.NativeRecords[i+1:]...)
			assertRetainedRefused(t, deleted)
			reordered := cloneBundle(t, b)
			reordered.NativeRecords[i], reordered.NativeRecords[i+1] = reordered.NativeRecords[i+1], reordered.NativeRecords[i]
			assertRetainedRefused(t, reordered)
		}
	}
	metadata, err := archive.BuildMetadataWithAnalysis(b, a, nil, "synthetic-machine", time.Unix(2000000000, 0), time.Unix(2000000100, 0), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, archive.ParserInfo{Name: "muse", Version: (Parser{}).Version()})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range out.Gaps {
		count := 0
		for _, actual := range metadata.CaptureGaps {
			if actual == expected {
				count++
			}
		}
		if count != 1 {
			t.Fatal("metadata omitted coverage")
		}
	}
	metadataBytes, _ := json.Marshal(metadata)
	if bytes.Contains(metadataBytes, []byte("PRIVATE_NATIVE_BODY_SENTINEL")) {
		t.Fatal("metadata leaked private body")
	}
	handoff, e := archive.BuildHandoffWithAnalysis(b, a, nil, archive.HandoffOptions{})
	if e != nil {
		t.Fatal(e)
	}
	handoffBytes, _ := json.Marshal(handoff)
	if bytes.Contains(handoffBytes, []byte("PRIVATE_NATIVE_BODY_SENTINEL")) {
		t.Fatal("private body in handoff")
	}
}

func cloneBundle(t *testing.T, b archive.SourceBundle) archive.SourceBundle {
	t.Helper()
	raw, e := json.Marshal(b)
	if e != nil {
		t.Fatal(e)
	}
	var out archive.SourceBundle
	if json.Unmarshal(raw, &out) != nil {
		t.Fatal("clone")
	}
	return out
}

func assertRetainedRefused(t *testing.T, b archive.SourceBundle) {
	t.Helper()
	if _, e := (Parser{}).Parse(t.Context(), b); e == nil {
		t.Fatal("parser accepted malformed evidence")
	}
	out, e := (Filter{}).Refilter(t.Context(), b, time.Time{})
	if e == nil || len(out.Records) != 0 {
		t.Fatal("refilter returned malformed or partial evidence")
	}
}

func TestNewNativeShapesRecursivelyCloseEveryObject(t *testing.T) {
	raw := readSynthetic(t, "synthetic-native-omissions.jsonl")
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	for line, wire := range lines {
		if line == 0 {
			continue
		}
		m, _, e := decodeObject(wire)
		if e != nil {
			t.Fatal(e)
		}
		if _, ok := m["retained_marker"]; ok {
			continue
		}
		r, e := envelope(wire, position{Line: int64(line + 1)})
		if e != nil {
			t.Fatal(e)
		}
		if !newFamily(r) {
			continue
		}
		p := object(m["payload"])
		var visit func(any)
		visit = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				for k, old := range x {
					delete(x, k)
					assertNativeMutationRefused(t, lines, line, m)
					x[k] = old
					// Every new field is mandatory and non-null except the explicitly known null slots.
					if old != nil {
						x[k] = nil
						assertNativeMutationRefused(t, lines, line, m)
						x[k] = old
					}
					visit(old)
				}
				x["future_authority"] = true
				assertNativeMutationRefused(t, lines, line, m)
				delete(x, "future_authority")
			case []any:
				for _, item := range x {
					visit(item)
				}
			}
		}
		visit(p)
	}
}

func assertNativeMutationRefused(t *testing.T, lines [][]byte, index int, m map[string]any) {
	t.Helper()
	changed := append([][]byte(nil), lines...)
	wire, _ := json.Marshal(m)
	changed[index] = wire
	out, e := filterBytes(t, append(bytes.Join(changed, []byte("\n")), '\n'), agentapi.ReadLimits{})
	if e == nil || len(out.Records) != 0 {
		t.Fatalf("malformed new object admitted at physical %d", index+1)
	}
}

func TestNewRetainedFamiliesRejectIdentityAndOrderingMutations(t *testing.T) {
	out, e := filterBytes(t, readSynthetic(t, "synthetic-native-omissions.jsonl"), agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	b := bundleFor(t, out)
	for i, m := range b.NativeRecords {
		wire, _ := json.Marshal(m)
		row, e := decodeLogical(wire)
		if e != nil {
			t.Fatal(e)
		}
		if row.ordinary == nil || !newFamily(*row.ordinary) {
			continue
		}
		for k, v := range object(m["fields"]) {
			if slices.Contains([]fieldName{fieldKind, fieldEventKind, fieldSourceRunRecordID}, fieldName(k)) {
				continue
			}
			clone := cloneBundle(t, b)
			f := object(clone.NativeRecords[i]["fields"])
			if _, ok := v.(string); ok {
				f[k] = "ffffffff-ffff-4fff-8fff-ffffffffffff"
			} else {
				f[k] = 99999
			}
			assertRetainedRefused(t, clone)
		}
		// A new row is not accepted if its finite native discriminator/schema is changed.
		for _, k := range []string{"payload_schema", "type"} {
			clone := cloneBundle(t, b)
			clone.NativeRecords[i][k] = "future"
			assertRetainedRefused(t, clone)
		}
	}
}

func TestOmissionOpenPrefixesAndMarkerChronology(t *testing.T) {
	raw := readSynthetic(t, "synthetic-native-omissions.jsonl")
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	for end := 6; end <= len(lines); end++ {
		out, e := filterBytes(t, append(bytes.Join(lines[:end], []byte("\n")), '\n'), agentapi.ReadLimits{})
		if e != nil {
			t.Fatalf("consistent prefix %d: %v", end, e)
		}
		if _, e := (Parser{}).Parse(t.Context(), bundleFor(t, out)); e != nil {
			t.Fatal(e)
		}
	}
	out, e := filterBytes(t, raw, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	rows, e := retainedRows(t.Context(), bundleFor(t, out))
	if e != nil {
		t.Fatal(e)
	}
	for _, r := range rows {
		if r.omission != nil {
			if _, present := r.timestamp(); present {
				t.Fatal("marker timestamp present")
			}
			if !r.valid() {
				t.Fatal("invalid union")
			}
			duplicate := r
			duplicate.ordinary = rows[0].ordinary
			if _, e := json.Marshal(duplicate); e == nil {
				t.Fatal("ambiguous union serialized")
			}
		}
	}
}

func TestEnrichedAssistantDropsOnlyTypedProviderItem(t *testing.T) {
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	out, e := filterBytes(t, raw, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	m, _, _ := decodeObject(lines[6])
	event := object(object(m["payload"])["event"])
	event["provider_item_id"] = "PRIVATE_PROVIDER_ITEM_SENTINEL"
	// Frozen cdbcd5f8 native assistant gate had exactly these four keys.
	if keys(event, "kind", "message_id", "response_id", "text") {
		t.Fatal("old native assistant gate admitted enriched event")
	}
	wire, _ := json.Marshal(m)
	lines[6] = wire
	next, e := filterBytes(t, append(bytes.Join(lines, []byte("\n")), '\n'), agentapi.ReadLimits{})
	if e != nil || !reflect.DeepEqual(out.Records, next.Records) {
		t.Fatal("enriched assistant changed retained graph")
	}
	for _, bad := range []any{nil, 1, true, map[string]any{"kind": "future"}} {
		event["provider_item_id"] = bad
		assertNativeMutationRefused(t, lines, 6, m)
	}
}

// The frozen pre-policy schema is taken verbatim from cdbcd5f8. It represents
// actual old retained-reader dispatch rather than an asserted version number.

func TestFrozenOldReaderRefusesEveryNewRetainedVariant(t *testing.T) {
	raw, e := os.ReadFile(filepath.Join("testdata", "retained-v1-before-omissions.schema.json"))
	if e != nil {
		t.Fatal(e)
	}
	doc, e := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if e != nil {
		t.Fatal(e)
	}
	compiler := jsonschema.NewCompiler()
	id := doc.(map[string]any)["$id"].(string)
	if e = compiler.AddResource(id, doc); e != nil {
		t.Fatal(e)
	}
	schema, e := compiler.Compile(id)
	if e != nil {
		t.Fatal(e)
	}
	out, e := filterBytes(t, readSynthetic(t, "synthetic-native-omissions.jsonl"), agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	for _, wire := range out.Records {
		row, e := decodeLogical(wire)
		if e != nil {
			t.Fatal(e)
		}
		v, e := jsonschema.UnmarshalJSON(bytes.NewReader(wire))
		if e != nil {
			t.Fatal(e)
		}
		err := schema.Validate(v)
		newVariant := row.omission != nil || row.ordinary != nil && newFamily(*row.ordinary)
		if newVariant && err == nil || !newVariant && err != nil {
			t.Fatal("old-reader compatibility changed")
		}
	}
}

func TestExactModelResolutionStartup(t *testing.T) {
	raw := readSynthetic(t, "synthetic-model-resolution-omissions.jsonl")
	out, e := filterBytes(t, raw, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-model-resolution-omissions.golden.jsonl"), append(bytes.Join(out.Records, []byte("\n")), '\n'))
	b := bundleFor(t, out)
	if _, e := (Parser{}).Parse(t.Context(), b); e != nil {
		t.Fatal(e)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	first, _, _ := decodeObject(lines[1])
	second, _, _ := decodeObject(lines[2])
	for _, mutate := range []func(map[string]any){
		func(m map[string]any) { object(object(m["payload"])["record"])["workspace_root"] = "/work/foreign" },
		func(m map[string]any) { object(object(m["payload"])["record"])["provider_id"] = "foreign" },
		func(m map[string]any) { object(object(m["payload"])["record"])["web_search_mode"] = "foreign" },
		func(m map[string]any) { object(object(object(m["payload"])["record"])["build"])["sha"] = "foreign" },
		func(m map[string]any) { delete(object(object(m["payload"])["record"]), "model_id") },
		func(m map[string]any) {
			m["recorded_at"] = first["recorded_at"]
			m["causation_id"] = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		},
		func(m map[string]any) { m["recorded_at"] = 1 },
	} {
		clone, _ := json.Marshal(second)
		m, _, _ := decodeObject(clone)
		mutate(m)
		assertNativeMutationRefused(t, lines, 2, m)
	}
	for _, mutate := range []func(archive.SourceBundle){
		func(b archive.SourceBundle) { object(b.NativeRecords[2]["fields"])["model_id"] = "forbidden" },
		func(b archive.SourceBundle) { delete(object(b.NativeRecords[3]["fields"]), "model_id") },
		func(b archive.SourceBundle) { object(b.NativeRecords[3]["fields"])["provider_id"] = "foreign" },
		func(b archive.SourceBundle) {
			b.NativeRecords[3]["causation_id"] = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		},
		func(b archive.SourceBundle) { b.NativeRecords[3]["ts_us"] = b.NativeRecords[0]["ts_us"] },
		func(b archive.SourceBundle) { b.NativeRecords[4] = b.NativeRecords[3] },
	} {
		clone := cloneBundle(t, b)
		mutate(clone)
		assertRetainedRefused(t, clone)
	}
	header, e := (NativeHeaders{}).InspectHeader(agentapi.NativeHeaderRequest{Purpose: agentapi.DiscoveryHandoff, Path: "/store/00000000-0000-7000-8000-000000000001/session.jsonl", Scan: func(visit func([]byte) bool) error {
		for _, line := range lines {
			if !visit(line) {
				break
			}
		}
		return nil
	}})
	if e != nil || header.NativeID != out.SessionIDs[0] || !header.StartedAt.Equal(out.NativeStartAt) {
		t.Fatal("shared startup header refused")
	}
}

func TestNativeMarkerAndUnknownDiscriminatorsRefuseWholeFile(t *testing.T) {
	raw := readSynthetic(t, "synthetic-native-omissions.jsonl")
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	for i, line := range lines {
		if i == 0 {
			continue
		}
		m, _, e := decodeObject(line)
		if e != nil {
			t.Fatal(e)
		}
		if _, present := m["retained_marker"]; present {
			for k, old := range m {
				delete(m, k)
				assertNativeMutationRefused(t, lines, i, m)
				m[k] = old
				m[k] = nil
				assertNativeMutationRefused(t, lines, i, m)
				m[k] = old
			}
			for _, obj := range []map[string]any{m, object(m["stream"]), object(m["position"]), object(m["omitted_record"])} {
				obj["future_authority"] = true
				assertNativeMutationRefused(t, lines, i, m)
				delete(obj, "future_authority")
			}
			for k, old := range object(m["omitted_record"]) {
				object(m["omitted_record"])[k] = "future"
				assertNativeMutationRefused(t, lines, i, m)
				object(m["omitted_record"])[k] = old
			}
			m["recorded_at"] = 1
			assertNativeMutationRefused(t, lines, i, m)
			delete(m, "recorded_at")
		} else {
			r, e := envelope(line, position{Line: int64(i + 1)})
			if e != nil {
				t.Fatal(e)
			}
			if !newFamily(r) {
				continue
			}
			old := m["payload_schema_version"]
			m["payload_schema_version"] = 99
			assertNativeMutationRefused(t, lines, i, m)
			m["payload_schema_version"] = old
			p := object(m["payload"])
			event := object(p["event"])
			if event == nil {
				event = object(p["record"])
			}
			old = event["kind"]
			event["kind"] = "future"
			assertNativeMutationRefused(t, lines, i, m)
			event["kind"] = old
		}
	}
}

func TestMarkerAddsNoActivityOrBirth(t *testing.T) {
	raw := readSynthetic(t, "synthetic-exec-conversation.jsonl")
	before, e := filterBytes(t, raw, agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	markerRaw, _ := json.Marshal(map[string]any{"retained_marker": "omitted_live_only", "schema_version": 1, "stream": stream{streamSession, before.SessionIDs[0]}, "position": nativePosition{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaab", 10}, "omitted_record": taskDelta})
	after, e := filterBytes(t, append(raw, append(markerRaw, '\n')...), agentapi.ReadLimits{})
	if e != nil {
		t.Fatal(e)
	}
	a, e := (Parser{}).Parse(t.Context(), bundleFor(t, before))
	if e != nil {
		t.Fatal(e)
	}
	b, e := (Parser{}).Parse(t.Context(), bundleFor(t, after))
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(a, b) || !before.NativeStartAt.Equal(after.NativeStartAt) {
		t.Fatal("timestamp-free marker changed own analysis")
	}
	if !(Filter{}).EvidenceExtends(bundleFor(t, before), bundleFor(t, after)) {
		t.Fatal("valid marker append prefix refused")
	}
}

func TestPrivateMetadataCopiesStayChargedUntilComparison(t *testing.T) {
	raw := readSynthetic(t, "synthetic-model-resolution-omissions.jsonl")
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	for _, i := range []int{1, 2} {
		m, _, e := decodeObject(lines[i])
		if e != nil {
			t.Fatal(e)
		}
		object(object(m["payload"])["record"])["web_search_mode"] = strings.Repeat("x", 64<<10)
		lines[i], e = json.Marshal(m)
		if e != nil {
			t.Fatal(e)
		}
	}
	raw = append(bytes.Join(lines, []byte("\n")), '\n')
	for _, mode := range []string{"success", "budget", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			limit := int64(32 << 20)
			if mode == "budget" {
				limit = 12 << 20
			}
			budget := agentapi.NewNativeReadBudget(limit)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			observed := ctx
			if mode == "cancel" {
				observed = &countedContext{Context: ctx, stop: 40, cancel: cancel}
			}
			out, release, e := (Filter{}).FilterLeased(observed, agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, budget)
			if mode == "success" && e != nil {
				t.Fatal(e)
			}
			if mode == "budget" && !errors.Is(e, agentapi.ErrReadBudget) {
				t.Fatal("private comparison escaped budget")
			}
			if mode == "cancel" && !errors.Is(e, context.Canceled) {
				t.Fatal("comparison cancellation escaped")
			}
			if e != nil && len(out.Records) != 0 {
				t.Fatal("partial artifact on refusal")
			}
			release()
			release()
			used, peak := budget.Charged()
			if used != 0 || peak < 8<<20 {
				t.Fatalf("private-copy reservation/release invalid: %d/%d", used, peak)
			}
		})
	}
}

type omissionGapMutation string

const (
	omitMarkerGap omissionGapMutation = "muse_ephemeral_task_delta_omitted"
	omitBodyGap   omissionGapMutation = "muse_private_native_bodies_omitted"
	omitBothGaps  omissionGapMutation = "both"
)
