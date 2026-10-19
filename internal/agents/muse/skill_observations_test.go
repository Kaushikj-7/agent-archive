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
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// A synthetic own materialized run carries the observed native event grammar.
// No native transcript or original-file digest is a fixture input.
func nativeSkillReads(t *testing.T, count int, change func(map[string]any)) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(completionNative(t, count, 0, 0)), []byte("\n"))
	for i := len(lines) - count; i < len(lines); i++ {
		m, _, err := decodeObject(lines[i])
		if err != nil {
			t.Fatal(err)
		}
		p := object(m["payload"])
		p["event"] = map[string]any{"kind": "skill_read_observed", "skill_id": "pr10-local", "evidence_kind": "read_skill_tool", "evidence_hash": "sha256:" + strings.Repeat("a", 64), "observed_at_sequence": p["source_run_record_sequence"]}
		if change != nil {
			change(m)
		}
		lines[i], err = json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
	}
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

func TestNativeSkillReadPipeline(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, nativeSkillReads(t, 1, nil), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal("qualified native skill-read report refused", err)
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-skill-read.golden.jsonl"), append(bytes.Join(out.Records, []byte("\n")), '\n'))
	wire := bytes.Join(out.Records, nil)
	if bytes.Contains(wire, []byte("evidence_hash")) || bytes.Contains(wire, []byte(strings.Repeat("a", 64))) {
		t.Fatal("original-byte native hash retained")
	}
	b := bundleFor(t, out)
	want := []archive.SkillUse{{Name: "pr10-local", Evidence: archive.SkillUseEvidenceNativeInvocation}}
	for range 2 {
		a, err := (Parser{}).Parse(t.Context(), b)
		if err != nil || !reflect.DeepEqual(a.View.NativeSkillUses, want) || len(b.SupplementalEvidence) != 0 {
			t.Fatal("native evidence requires inventory or lost its own identity", err)
		}
		info := archive.ParserInfo{Name: "muse", Version: (Parser{}).Version()}
		metadata, err := archive.BuildMetadataWithAnalysis(b, a, nil, "synthetic", time.Unix(2000000000, 0), time.Unix(2000000100, 0), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("b", 64), CompressedBytes: 1}, info)
		if err != nil || !reflect.DeepEqual(metadata.SkillsUsed, want) || len(metadata.SkillsAvailable) != 0 {
			t.Fatal("native use conflated with eligibility or content hash", err)
		}
		next, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
		if err != nil || !reflect.DeepEqual(out.Records, next.Records) || !(Filter{}).EvidenceExtends(b, bundleFor(t, next)) {
			t.Fatal("native skill-read retained/refilter drift", err)
		}
		b = bundleFor(t, next)
	}
}

func TestNativeSkillReadDistinctSources(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, nativeSkillReads(t, 2, nil), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	b := bundleFor(t, out)
	a, err := (Parser{}).Parse(t.Context(), b)
	if err != nil || len(a.View.NativeSkillUses) != 2 {
		t.Fatal("distinct positive native reports lost", err)
	}
	info := archive.ParserInfo{Name: "muse", Version: (Parser{}).Version()}
	m, err := archive.BuildMetadataWithAnalysis(b, a, nil, "synthetic", time.Unix(2000000000, 0), time.Unix(2000000100, 0), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("b", 64), CompressedBytes: 1}, info)
	if err != nil || !reflect.DeepEqual(m.SkillsUsed, []archive.SkillUse{{Name: "pr10-local", Evidence: archive.SkillUseEvidenceNativeInvocation}}) {
		t.Fatal("metadata duplicated logical skill or invented count/hash", err)
	}
}

func TestNativeSkillReadRawRefusals(t *testing.T) {
	t.Parallel()
	cases := map[string]func(map[string]any, map[string]any){
		"missing-id":       func(p, e map[string]any) { delete(e, "skill_id") },
		"missing-digest":   func(p, e map[string]any) { delete(e, "evidence_hash") },
		"digest-null":      func(p, e map[string]any) { e["evidence_hash"] = nil },
		"digest-false":     func(p, e map[string]any) { e["evidence_hash"] = false },
		"digest-empty":     func(p, e map[string]any) { e["evidence_hash"] = "" },
		"digest-bare":      func(p, e map[string]any) { e["evidence_hash"] = strings.Repeat("a", 64) },
		"digest-prefix":    func(p, e map[string]any) { e["evidence_hash"] = "SHA256:" + strings.Repeat("a", 64) },
		"digest-uppercase": func(p, e map[string]any) { e["evidence_hash"] = "sha256:" + strings.Repeat("A", 64) },
		"digest-future":    func(p, e map[string]any) { e["evidence_hash"] = "future:" + strings.Repeat("a", 64) },
		"evidence-future":  func(p, e map[string]any) { e["evidence_kind"] = "read_file" },
		"evidence-case":    func(p, e map[string]any) { e["evidence_kind"] = "READ_SKILL_TOOL" },
		"event-case":       func(p, e map[string]any) { e["kind"] = "Skill_Read_Observed" },
		"event-future":     func(p, e map[string]any) { e["kind"] = "skill_read_observed_v2" },
		"event-extra":      func(p, e map[string]any) { e["future"] = true },
		"payload-extra":    func(p, e map[string]any) { p["future"] = true },
		"id-null":          func(p, e map[string]any) { e["skill_id"] = nil },
		"id-case":          func(p, e map[string]any) { e["skill_id"] = "PR10-local" },
		"id-path":          func(p, e map[string]any) { e["skill_id"] = "../pr10-local" },
		"id-namespace":     func(p, e map[string]any) { e["skill_id"] = "foreign:pr10-local" },
		"id-overlong":      func(p, e map[string]any) { e["skill_id"] = strings.Repeat("x", 81) },
		"id-privacy":       func(p, e map[string]any) { e["skill_id"] = "ghp_" + strings.Repeat("x", 36) },
		"sequence-zero":    func(p, e map[string]any) { e["observed_at_sequence"] = json.Number("0") },
		"sequence-string":  func(p, e map[string]any) { e["observed_at_sequence"] = "3" },
		"sequence-prior":   func(p, e map[string]any) { e["observed_at_sequence"] = json.Number("2") },
		"source-duplicate": func(p, e map[string]any) {
			p["source_run_record_sequence"] = json.Number("2")
			e["observed_at_sequence"] = json.Number("2")
		},
		"foreign-run": func(p, e map[string]any) { p["run_id"] = "99999999-9999-4999-8999-999999999999" },
		"task-role":   func(p, e map[string]any) { p["kind"] = "task" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw := nativeSkillReads(t, 1, func(m map[string]any) { p := object(m["payload"]); mutate(p, object(p["event"])) })
			out, err := filterBytes(t, raw, agentapi.ReadLimits{})
			if err == nil || len(out.Records) != 0 {
				t.Fatal("unqualified native skill-read admitted")
			}
		})
	}
	for _, name := range []skillNativeAuthorityCase{skillNativeAuthoritySchema, skillNativeAuthorityCausation, skillNativeAuthorityForeignOwner, skillNativeAuthorityDuplicateKey, skillNativeAuthorityDuplicateSourceId, skillNativeAuthorityUnmaterialized} {
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()
			raw := nativeSkillReads(t, 1, func(m map[string]any) {
				switch name {
				case skillNativeAuthorityDuplicateKey, skillNativeAuthorityUnmaterialized:
					// These cases mutate the complete wire below.
				case skillNativeAuthoritySchema:
					m["payload_schema_version"] = json.Number("2")
				case skillNativeAuthorityCausation:
					m["causation_id"] = "99999999-9999-4999-8999-999999999999"
				case skillNativeAuthorityForeignOwner:
					object(m["stream"])["id"] = "99999999-9999-4999-8999-999999999999"
				case skillNativeAuthorityDuplicateSourceId:
					object(m["payload"])["source_run_record_id"] = "44444444-4444-4444-8444-444444444444"
				}
			})
			if name == skillNativeAuthorityDuplicateKey {
				raw = bytes.Replace(raw, []byte(`"skill_id":"pr10-local"`), []byte(`"skill_id":"pr10-local","skill_id":"other"`), 1)
			}
			if name == skillNativeAuthorityUnmaterialized {
				raw = bytes.Replace(raw, []byte(`"semantic_kind":{"kind":"chat"}`), []byte(`"semantic_kind":{"kind":"other"}`), 1)
			}
			if out, err := filterBytes(t, raw, agentapi.ReadLimits{}); err == nil || len(out.Records) != 0 {
				t.Fatal("invalid native authority admitted")
			}
		})
	}
}

func TestNativeSkillReadRetainedRefusals(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, nativeSkillReads(t, 1, nil), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []skillRetainedCase{skillRetainedDigest, skillRetainedFuture, skillRetainedEvidence, skillRetainedSelfSequence, skillRetainedSourceSequence, skillRetainedForeignRun, skillRetainedForeignOwner, skillRetainedIdCase, skillRetainedIdPrivacy, skillRetainedDuplicateSource} {
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()
			b := bundleFor(t, out)
			row := b.NativeRecords[len(b.NativeRecords)-1]
			f := object(row["fields"])
			switch name {
			case skillRetainedDigest:
				f["evidence_hash"] = "sha256:" + strings.Repeat("a", 64)
			case skillRetainedFuture:
				f["future"] = true
			case skillRetainedEvidence:
				f["evidence_kind"] = "read_file"
			case skillRetainedSelfSequence:
				f["observed_at_sequence"] = json.Number("2")
			case skillRetainedSourceSequence:
				f["source_run_record_sequence"] = json.Number("2")
				f["observed_at_sequence"] = json.Number("2")
			case skillRetainedForeignRun:
				f["run_id"] = "99999999-9999-4999-8999-999999999999"
			case skillRetainedForeignOwner:
				object(row["stream"])["id"] = "99999999-9999-4999-8999-999999999999"
			case skillRetainedIdCase:
				f["skill_id"] = "PR10-local"
			case skillRetainedIdPrivacy:
				f["skill_id"] = "ghp_" + strings.Repeat("x", 36)
			case skillRetainedDuplicateSource:
				f["source_run_record_id"] = "44444444-4444-4444-8444-444444444444"
			}
			if _, err := (Parser{}).Parse(t.Context(), b); err == nil {
				t.Fatal("unqualified retained event parsed")
			}
			if _, err := (Filter{}).Refilter(t.Context(), b, time.Time{}); err == nil {
				t.Fatal("unqualified retained event refreshed")
			}
			if (Filter{}).EvidenceExtends(bundleFor(t, out), b) {
				t.Fatal("changed native evidence extends")
			}
		})
	}
}

func TestNativeSkillReadDoesNotInventEvidence(t *testing.T) {
	t.Parallel()
	failedText := mutateToolNative(t, ownTools(t), 1, func(result map[string]any) {
		event := object(object(result["payload"])["event"])
		items, ok := event["results"].([]any)
		if str(result["payload_type"]) != "runtime.session" || str(event["kind"]) != "tool_result_batch_committed" || !ok || len(items) != 2 {
			t.Fatal("synthetic committed result fixture changed")
		}
		for _, item := range items {
			object(item)["text"] = "Synthetic failed read result text, not typed native status."
		}
	})
	for name, raw := range map[string][]byte{"conversation": completionNative(t, 0, 0, 0), "broad-omission": changeBroadRead(t, nil), "file-read": committedRead(t), "calls-results": ownTools(t), "failed-result-text": failedText} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := filterBytes(t, raw, agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			b := bundleFor(t, out)
			b.SupplementalEvidence = []archive.SupplementalEvidence{{Kind: archive.EvidenceKindSkillInventory, Provenance: "filesystem:muse", Payload: map[string]any{"coverage": "installed_only", "scope": "user_muse_config", "skills": []any{map[string]any{"name": "pr10-local", "sha256": strings.Repeat("a", 64)}}}}}
			a, err := (Parser{}).Parse(t.Context(), b)
			if err != nil || len(a.View.NativeSkillUses) != 0 {
				t.Fatal("inventory/call/omission manufactured native use", err)
			}
			info := archive.ParserInfo{Name: "muse", Version: (Parser{}).Version()}
			m, err := archive.BuildMetadataWithAnalysis(b, a, nil, "synthetic", time.Unix(2000000000, 0), time.Unix(2000000100, 0), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("b", 64), CompressedBytes: 1}, info)
			if err != nil || len(m.SkillsUsed) != 0 || len(m.SkillsAvailable) != 1 {
				t.Fatal("installed inventory or unrelated result manufactured metadata use", err)
			}
		})
	}
}

func TestNativeSkillReadLeaseAndCancellation(t *testing.T) {
	t.Parallel()
	raw := nativeSkillReads(t, 1, nil)
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	out, release, err := (Filter{}).FilterLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, budget)
	used, peak := budget.Charged()
	if err != nil || used == 0 || len(out.Records) == 0 {
		release()
		t.Fatal("native skill-read lease", err)
	}
	release()
	release()
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("native skill-read lease leaked")
	}
	for _, limit := range []int64{4095, peak / 2, peak - 1} {
		b := agentapi.NewNativeReadBudget(limit)
		out, release, err := (Filter{}).FilterLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, b)
		release()
		if used, _ := b.Charged(); !errors.Is(err, agentapi.ErrReadBudget) || len(out.Records) != 0 || used != 0 {
			t.Fatal("partial skill-read lease leaked", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, release, err = (Filter{}).FilterLeased(ctx, agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{}, budget)
	release()
	if used, _ := budget.Charged(); !errors.Is(err, context.Canceled) || len(out.Records) != 0 || used != 0 {
		t.Fatal("skill-read cancel leaked", err)
	}
}

func TestNativeSkillReadOldStrictSchemaRefuses(t *testing.T) {
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
	if err := compiler.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		t.Fatal(err)
	}
	out, err := filterBytes(t, nativeSkillReads(t, 1, nil), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range out.Records {
		m, err := jsonschema.UnmarshalJSON(bytes.NewReader(row))
		if err != nil {
			t.Fatal(err)
		}
		isSkill := str(object(m.(map[string]any)["fields"])["event_kind"]) == string(eventSkillReadObserved)
		if (schema.Validate(m) != nil) != isSkill {
			t.Fatal("old strict schema changed unrelated rows or accepted new event")
		}
	}
}

type skillNativeAuthorityCase string

const (
	skillNativeAuthoritySchema            skillNativeAuthorityCase = "schema"
	skillNativeAuthorityCausation         skillNativeAuthorityCase = "causation"
	skillNativeAuthorityForeignOwner      skillNativeAuthorityCase = "foreign-owner"
	skillNativeAuthorityDuplicateKey      skillNativeAuthorityCase = "duplicate-key"
	skillNativeAuthorityDuplicateSourceId skillNativeAuthorityCase = "duplicate-source-id"
	skillNativeAuthorityUnmaterialized    skillNativeAuthorityCase = "unmaterialized"
)

type skillRetainedCase string

const (
	skillRetainedDigest          skillRetainedCase = "digest"
	skillRetainedFuture          skillRetainedCase = "future"
	skillRetainedEvidence        skillRetainedCase = "evidence"
	skillRetainedSelfSequence    skillRetainedCase = "self-sequence"
	skillRetainedSourceSequence  skillRetainedCase = "source-sequence"
	skillRetainedForeignRun      skillRetainedCase = "foreign-run"
	skillRetainedForeignOwner    skillRetainedCase = "foreign-owner"
	skillRetainedIdCase          skillRetainedCase = "id-case"
	skillRetainedIdPrivacy       skillRetainedCase = "id-privacy"
	skillRetainedDuplicateSource skillRetainedCase = "duplicate-source"
)
