package muse

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestModelAvailabilityUsesCompleteStartup(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"synthetic-exec-conversation.jsonl", "synthetic-model-resolution-omissions.jsonl"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := filterBytes(t, readSynthetic(t, name), agentapi.ReadLimits{})
			if err != nil {
				t.Fatal(err)
			}
			hasMissingModel := func(gaps []archive.CaptureGap) bool {
				return slices.ContainsFunc(gaps, func(gap archive.CaptureGap) bool { return gap.Code == "model_identity_unavailable" })
			}
			if hasMissingModel(out.Gaps) {
				t.Fatal("qualified startup model reported unavailable")
			}
			b := bundleFor(t, out)
			// A retained source from the previous filter may carry this false gap.
			b.Capture.Gaps = append(b.Capture.Gaps, archive.CaptureGap{Code: "model_identity_unavailable", Detail: "Startup metadata does not identify a model"})
			refiltered, err := (Filter{}).Refilter(t.Context(), b, time.Time{})
			if err != nil || hasMissingModel(refiltered.Gaps) {
				t.Fatalf("retained refilter did not repair model availability: %v", err)
			}
			b = bundleFor(t, refiltered)
			a, err := (Parser{}).Parse(t.Context(), b)
			if err != nil {
				t.Fatal(err)
			}
			h, err := archive.BuildHandoffWithAnalysis(b, a, nil, archive.HandoffOptions{})
			if err != nil || slices.ContainsFunc(h.Gaps, func(gap archive.HandoffGap) bool { return gap.Code == "model_identity_unavailable" }) {
				t.Fatalf("handoff lost model availability: %v", err)
			}
		})
	}
}

func TestEmptyStartupModelRemainsUnavailable(t *testing.T) {
	t.Parallel()
	raw := bytes.ReplaceAll(readSynthetic(t, "synthetic-exec-conversation.jsonl"), []byte(`"model_id":"synthetic-model"`), []byte(`"model_id":""`))
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(out.Gaps, func(gap archive.CaptureGap) bool { return gap.Code == "model_identity_unavailable" }) {
		t.Fatal("empty startup model reported available")
	}
}

func TestRetainedConversationDoesNotClaimToolMeasurements(t *testing.T) {
	t.Parallel()
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	var schemaID string
	for _, name := range []string{"metadata.schema.json", "eval-export.schema.json"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "schemas", name))
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		schemaID = doc.(map[string]any)["$id"].(string)
		if err = compiler.AddResource(schemaID, doc); err != nil {
			t.Fatal(err)
		}
	}
	schema, err := compiler.Compile(schemaID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := filterBytes(t, readSynthetic(t, "synthetic-exec-conversation.jsonl"), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	// bundleFor JSON-roundtrips the filtered bundle after removing the native file.
	b := bundleFor(t, out)
	a, err := (Parser{}).Parse(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	parser := archive.ParserInfo{Name: "muse", Version: (Parser{}).Version()}
	m, err := archive.BuildMetadataWithAnalysis(b, a, nil, "synthetic-machine", time.Unix(2000000000, 0), time.Unix(2000000100, 0), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, parser)
	if err != nil {
		t.Fatal(err)
	}
	checkCounts := func(c archive.Counts) {
		t.Helper()
		if c.Turns == nil || *c.Turns != 1 || c.Messages == nil || *c.Messages != 2 {
			t.Fatalf("qualified conversation counts: %+v", c)
		}
		for name, n := range map[string]*int{"calls": c.ToolCalls, "results": c.ToolResults, "files": c.FilesTouched, "commits": c.Commits, "pushes": c.Pushes, "created PRs": c.PRsCreated, "merged PRs": c.PRsMerged} {
			if n != nil {
				t.Errorf("%s became measured %d", name, *n)
			}
		}
	}
	checkCounts(m.Counts)
	// A current sidecar is trusted; an old parser sidecar must refresh false zeros.
	for _, version := range []string{parser.Version, "0.1.0"} {
		old := m
		old.Parser.Version = version
		if version != parser.Version {
			old.Counts.ToolCalls, old.Counts.ToolResults, old.Counts.FilesTouched = new(0), new(0), new(0)
			old.Counts.Commits, old.Counts.Pushes, old.Counts.PRsCreated, old.Counts.PRsMerged = new(0), new(0), new(0), new(0)
		}
		e, err := archive.BuildEvalExportWithAnalysis(b, a, old, archive.EvalExportSourceArchive, archive.EvalExportDetailFull, parser)
		if err != nil {
			t.Fatal(err)
		}
		checkCounts(e.Counts)
		if e.FilesEdited != nil || e.Prompts == nil || len(*e.Prompts) != 1 || e.FinalResponse == nil || e.Parser.Version != parser.Version {
			t.Fatalf("full export presence/parser: %+v", e)
		}
		if err = archive.ValidateEvalExport(e); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err = schema.Validate(value); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"files_edited"`) {
			t.Fatal("export serialized an unavailable files list")
		}
	}
	h, err := archive.BuildHandoffWithAnalysis(b, a, &m, archive.HandoffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !h.ToolResultsUnavailable || len(h.Gaps) == 0 {
		t.Fatalf("handoff availability/gaps: %+v", h)
	}
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"tool_results_unavailable":true`) || !strings.Contains(string(raw), "tool_facts_unavailable") {
		t.Fatalf("handoff JSON lost limits: %s", raw)
	}
	text := string(archive.RenderHandoffMarkdown(h, archive.HandoffRenderOptions{}))
	if !strings.Contains(text, "Tool results are unavailable") {
		t.Fatal("handoff text lost availability")
	}
}
