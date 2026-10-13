package backfill

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/agents/importconfig"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// Embedding the concrete codec deliberately carries its optional capabilities.
// They must not attest this override as the original codec.
type inspectionCodec = codex.Filter

type retainedInspectionFilter struct {
	inspectionCodec
	calls *atomic.Int64
}

func (f retainedInspectionFilter) Filter(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext) (archive.FilteredTranscript, error) {
	f.calls.Add(1)
	return f.inspectionCodec.Filter(ctx, in, c)
}

type retainedInspectionSources struct {
	agentapi.SourcesLookup
	calls *atomic.Int64
}

func (s retainedInspectionSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := s.SourcesLookup.LookupSources(name)
	if name == "codex" && ok {
		f = retainedInspectionFilter{calls: s.calls}
	}
	return p, f, ok
}

func TestStreamingInspectionPreservesWholeCandidatesAndDecoratorFallback(t *testing.T) {
	for _, suffix := range []string{
		"", // Normal conversation.
		`{"type":"event_msg","payload":{"message":"visible event"}}` + "\n",
		`{"type":"session_meta","payload":{"id":"wrong","history_mode":"unsupported"}}` + "\n",
		`{"type":"session_meta","payload":{"id":"wrong","forked_from_id":"ancestor"}}` + "\n",
		"{invalid json}\n",
	} {
		t.Run(suffix, func(t *testing.T) {
			tr := newTree(t)
			repo := tr.repo("home/repo")
			const id = "00000000-0000-0000-0000-000000000001"
			start := fixedNow.Add(-time.Hour)
			raw := codexTranscript(id, id, repo, start) + suffix
			tr.write(filepath.Join("home", codexFile(id)), raw)
			for _, filters := range []Filters{{}, {Since: fixedNow.AddDate(0, 0, 1).Format("2006-01-02")}} {
				env := tr.env()
				got := plan(t, env, nil, config.Config{}, filters)
				var calls atomic.Int64
				env.Sources = retainedInspectionSources{env.Sources, &calls}
				want := plan(t, env, nil, config.Config{}, filters)
				if !reflect.DeepEqual(got.Candidates, want.Candidates) {
					t.Fatalf("candidate difference: summary=%+v retained=%+v", got.Candidates, want.Candidates)
				}
				if calls.Load() == 0 && !strings.Contains(suffix, "session_meta") {
					t.Fatal("embedded capability bypassed Filter override")
				}
			}
		})
	}
}

// Embedding an interpreter must not bypass its InspectImport override.
type summaryInspection interface {
	agentapi.ImportInspector
	agentapi.SummaryImportInspector
}

type decoratedImportInspector struct {
	summaryInspection
	calls *atomic.Int64
}

func (p decoratedImportInspector) InspectImport(ctx context.Context, r agentapi.ImportInspectionRequest) (agentapi.ImportInspection, error) {
	p.calls.Add(1)
	out, err := p.summaryInspection.InspectImport(ctx, r)
	out.Conversation = false
	return out, err
}

type decoratedImports struct {
	agentapi.ImportsLookup
	calls *atomic.Int64
}

func (p decoratedImports) LookupImport(name string) (agentapi.ImportInspector, bool) {
	inspector, ok := p.ImportsLookup.LookupImport(name)
	if name == "codex" && ok {
		return decoratedImportInspector{summaryInspection: inspector.(summaryInspection), calls: p.calls}, true
	}
	return inspector, ok
}

func TestStreamingInspectionPreservesInspectorOverride(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	const id = "00000000-0000-0000-0000-000000000001"
	tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, repo, fixedNow.Add(-time.Hour)))
	env := tr.env()
	var inspected, filtered atomic.Int64
	env.Imports = decoratedImports{env.Imports, &inspected}
	got := plan(t, env, nil, config.Config{}, Filters{})
	env.Sources = retainedInspectionSources{env.Sources, &filtered}
	want := plan(t, env, nil, config.Config{}, Filters{})
	if !reflect.DeepEqual(got.Candidates, want.Candidates) {
		t.Fatalf("inspector override bypassed: summary=%+v retained=%+v", got.Candidates, want.Candidates)
	}
	if inspected.Load() != 2 {
		t.Fatalf("override called %d times, want 2", inspected.Load())
	}
}

type configuredImports struct{ agentapi.ImportsLookup }

func (p configuredImports) LookupImport(name string) (agentapi.ImportInspector, bool) {
	if name == "codex" {
		return importconfig.Provider{PreferHeaderStart: true, NativeStart: true, ConversationTypes: []string{"event_msg"}}, true
	}
	return p.ImportsLookup.LookupImport(name)
}

func TestStreamingInspectionPreservesConfiguredConversationPolicy(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	const id = "00000000-0000-0000-0000-000000000001"
	tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, repo, fixedNow.Add(-time.Hour)))
	env := tr.env()
	env.Imports = configuredImports{env.Imports}
	got := plan(t, env, nil, config.Config{}, Filters{})
	var calls atomic.Int64
	env.Sources = retainedInspectionSources{env.Sources, &calls}
	want := plan(t, env, nil, config.Config{}, Filters{})
	if !reflect.DeepEqual(got.Candidates, want.Candidates) {
		t.Fatalf("configured conversation policy changed: summary=%+v retained=%+v", got.Candidates, want.Candidates)
	}
}
