package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// The old derivation delegates all native interpretation to the builtin parser.
// It simulates historical label/model attribution, without changing own activity.
type musePriorLabelsParser struct{ agentapi.TranscriptParser }

func (musePriorLabelsParser) Version() string { return "synthetic-prior-labels" }

func (p musePriorLabelsParser) Parse(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	a, err := p.TranscriptParser.Parse(ctx, b)
	if err == nil {
		a.Facts.Name = "Prior synthetic attribution"
		for i := range a.View.Turns {
			if a.View.Turns[i].Model != "" {
				a.View.Turns[i].Model = "synthetic-prior-model"
			}
		}
	}
	return a, err
}

type musePriorLabelsLookup struct{ agentapi.ParsersLookup }

func (p musePriorLabelsLookup) LookupParser(name string) (agentapi.TranscriptParser, bool) {
	parser, ok := p.ParsersLookup.LookupParser(name)
	if ok && name == "muse" {
		parser = musePriorLabelsParser{parser}
	}
	return parser, ok
}

type museRefreshStore struct {
	*storagetest.MemoryStore
	keys []string
}

func (s *museRefreshStore) Put(ctx context.Context, key string, raw []byte) error {
	s.keys = append(s.keys, key)
	return s.MemoryStore.Put(ctx, key, raw)
}

// This exercises admitted root publication and real retained regeneration, not
// a consumer-only sidecar edit. All files, identity and storage are synthetic.
func TestMuseRetainedMetadataRefreshReachesWarmConsumers(t *testing.T) {
	t.Parallel()
	f := newMuseRootCaptureFixture(t)
	// Use the already supported automatic-name record shape in synthetic data.
	// Replacing the final row preserves native identity/order without creating
	// a second interpretation path in any consumer.
	raw, err := os.ReadFile(f.path)
	must(t, err)
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	var named map[string]any
	must(t, json.Unmarshal(lines[len(lines)-1], &named))
	named["payload_type"] = "session.name.changed"
	named["payload"] = map[string]any{"session_id": museSyntheticID, "new_name": "Current synthetic name", "previous_name": nil, "source": "automatic", "operation_id": "00000000-0000-4000-8000-000000000003"}
	lines[len(lines)-1], err = json.Marshal(named)
	must(t, err)
	must(t, os.WriteFile(f.path, append(bytes.Join(lines, []byte("\n")), '\n'), 0600))
	registry, filter := museCountingRegistry(t)
	cloud := &museRefreshStore{MemoryStore: storagetest.NewMemoryStore()}
	f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return cloud, nil }
	f.env.Agents = registry
	now := f.observed
	f.env.Now = func() time.Time { return now }
	payload, err := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": museSyntheticID, "transcript_path": nil, "cwd": f.project, "source": "startup"})
	must(t, err)
	var stderr bytes.Buffer
	if code := runHookCommand([]string{"--harness", "muse"}, bytes.NewReader(payload), &stderr, f.env); code != 0 || stderr.Len() != 0 {
		t.Fatalf("root intent: %d %s", code, stderr.String())
	}
	opts := collector.Options{Sources: registry, Parsers: musePriorLabelsLookup{registry}, Decoders: registry, MachineID: f.cfg.MachineID, NativeRoots: f.cfg.NativeRoots, AcceptSession: f.cfg.AcceptSession, Now: func() time.Time { return now }, NativeLocatorFactory: func(ctx context.Context, pass collector.NativeLocatorPass) (agentapi.NativeLocatorLookup, func() error, error) {
		inventory, err := discovery.NewNativeInventory(ctx, f.home, pass.Roots, pass.Queries, pass.ReadBudget, builtin.ValidateNativeLocator)
		if err != nil {
			return nil, nil, err
		}
		return inventory, inventory.Close, nil
	}}
	collect := func() {
		t.Helper()
		result, err := collector.Run(t.Context(), f.store, cloud, opts)
		defer result.CloseStagedResults()
		if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
			t.Fatalf("collector: %+v %v", result, err)
		}
	}
	collect()
	regs, err := f.store.LoadRegistrations()
	must(t, err)
	if len(regs) != 1 || regs[0].NativeBinding == nil || regs[0].NativeAdmission == nil {
		t.Fatal("root bypassed normal admission")
	}
	key, err := archive.MetadataObjectKey("muse", regs[0].ArchiveSessionID)
	must(t, err)
	prior, bundle, err := reader.RefreshAndLoad(t.Context(), cloud, key, reader.Limits{})
	must(t, err)
	expected, err := analyzeSource(t.Context(), registry, bundle)
	must(t, err)
	labels, _ := archive.LabelsFromAnalysis(expected)
	if labels.Name != "Current synthetic name" {
		t.Fatal("retained name fixture did not establish the current label")
	}
	if prior.Name != "Prior synthetic attribution" || len(prior.Models) == 0 || prior.Models[0].Attributes["gen_ai.request.model"] != "synthetic-prior-model" {
		t.Fatal("old parser fixture did not establish stale labels and model")
	}
	// Warm the actual catalog and model-filtered statistics before publication.
	if _, errOut, code := runShow(t, f.env, prior.Name, "--harness", "muse", "--json"); code != 0 {
		t.Fatalf("warm name: %d %s", code, errOut)
	}
	var before statsDocument
	must(t, json.Unmarshal([]byte(mustRunStats(t, f.env, 0, "--harness", "muse", "--model", "synthetic-prior-model", "--json")), &before))
	if before.Coverage.Sessions != 1 {
		t.Fatal("old model was not searchable")
	}
	source, err := cloud.Get(t.Context(), prior.SourceBundle.Key)
	must(t, err)
	must(t, os.Remove(f.path))
	cloud.keys = nil
	filter.calls.Store(0)
	now = now.Add(time.Hour)
	opts.Parsers = registry
	collect()
	if filter.calls.Load() != 0 {
		t.Fatal("retained refresh filtered native data")
	}
	next, err := reader.ReadMetadata(t.Context(), cloud, key)
	must(t, err)
	parser, _ := registry.LookupParser("muse")
	if next.Name != labels.Name || next.Title != labels.Title || next.SourceBundle != prior.SourceBundle || !next.CapturedAt.Equal(prior.CapturedAt) || !next.MetadataDerivedAt.Equal(now) || next.Parser.Version != parser.Version() || !reflect.DeepEqual(next.Counts, prior.Counts) {
		t.Fatal("retained refresh changed source/activity age or failed to refresh derivation")
	}
	afterSource, err := cloud.Get(t.Context(), next.SourceBundle.Key)
	must(t, err)
	if !bytes.Equal(source, afterSource) || len(cloud.keys) != 2 || cloud.keys[0] != key || !strings.HasPrefix(cloud.keys[1], listingindex.V3Prefix) {
		t.Fatalf("metadata refresh rewrote source or omitted canonical listing: %v", cloud.keys)
	}
	for _, query := range []string{labels.Name, labels.Title} {
		shown, errOut, code := runShow(t, f.env, query, "--harness", "muse", "--json")
		if code != 0 {
			t.Fatalf("refreshed label %q: %d %s", query, code, errOut)
		}
		var got archive.Metadata
		must(t, json.Unmarshal([]byte(shown), &got))
		if got.Name != next.Name || !reflect.DeepEqual(got.Models, next.Models) {
			t.Fatal("warm reader kept old name/model")
		}
	}
	if _, _, code := runShow(t, f.env, prior.Name, "--harness", "muse", "--json"); code == 0 {
		t.Fatal("warm catalog kept stale name")
	}
	var oldModel statsDocument
	must(t, json.Unmarshal([]byte(mustRunStats(t, f.env, 0, "--harness", "muse", "--model", "synthetic-prior-model", "--json")), &oldModel))
	if oldModel.Coverage.Sessions != 0 || len(next.Models) == 0 {
		t.Fatal("model filter retained stale attribution")
	}
	model := next.Models[0].Attributes["gen_ai.request.model"]
	var current statsDocument
	must(t, json.Unmarshal([]byte(mustRunStats(t, f.env, 0, "--harness", "muse", "--model", model, "--json")), &current))
	if model == "" || current.Coverage.Sessions != 1 || current.Overview.Cost.Value != nil {
		t.Fatal("refreshed model disappeared or unavailable usage became priced")
	}
	page := mustRunStats(t, f.env, 0, "--harness", "muse", "--model", model, "--html", "--include-names")
	checkPageAgainstJSON(t, parsePage(t, page), current)
	for _, detail := range []string{"metadata", "full"} {
		records, errOut, code := evalLines(t, f.env, next.SessionID, "--harness", "muse", "--detail", detail)
		if code != 0 || len(records) != 1 {
			t.Fatalf("refreshed eval %s: %d %s", detail, code, errOut)
		}
		raw, err := json.Marshal(records[0])
		must(t, err)
		var export archive.EvalExport
		must(t, json.Unmarshal(raw, &export))
		if !reflect.DeepEqual(export.Models, next.Models) || !reflect.DeepEqual(export.CaptureGaps, next.CaptureGaps) || !reflect.DeepEqual(export.Counts, next.Counts) {
			t.Fatalf("eval %s kept stale metadata", detail)
		}
	}
}
