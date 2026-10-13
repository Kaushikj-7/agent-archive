package backfill

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/inspectioncache"
)

func TestVerifiedInspectionCachePreservesWholePlanAcrossCacheStates(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	const id = "00000000-0000-0000-0000-000000000001"
	source := filepath.Join("home", codexFile(id))
	raw := codexTranscript(id, id, repo, fixedNow.Add(-time.Hour))
	tr.write(source, raw)
	env := tr.env()
	baseline := plan(t, env, nil, config.Config{}, Filters{})
	dir := filepath.Join(t.TempDir(), "inspection")
	cache := inspectioncache.New(dir)
	env.InspectionCache = cache
	var observations []inspectioncache.Observation
	env.ObserveInspection = func(observed inspectioncache.Observation) { observations = append(observations, observed) }
	cold := plan(t, env, nil, config.Config{}, Filters{})
	if !reflect.DeepEqual(cold, baseline) {
		t.Fatalf("cold plan differs: %+v / %+v", cold, baseline)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("planner wrote cache: %v", err)
	}
	if len(observations) != 1 {
		t.Fatalf("successful ordinary observations = %d", len(observations))
	}
	if err := cache.StoreBatch(t.Context(), observations); err != nil {
		t.Fatal(err)
	}
	before := inspectioncache.OperationCounts()
	observations = nil
	warm := plan(t, env, nil, config.Config{}, Filters{})
	after := inspectioncache.OperationCounts()
	if !reflect.DeepEqual(warm, baseline) {
		t.Fatalf("warm plan differs: %+v / %+v", warm, baseline)
	}
	if after.Hits-before.Hits != 1 || len(observations) != 0 {
		t.Fatalf("warm reuse: before=%+v after=%+v observations=%d", before, after, len(observations))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			if err := os.WriteFile(filepath.Join(dir, entry.Name()), []byte("corrupt"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	corrupt := plan(t, env, nil, config.Config{}, Filters{})
	if !reflect.DeepEqual(corrupt, baseline) {
		t.Fatal("corrupt cache changed plan")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	evicted := plan(t, env, nil, config.Config{}, Filters{})
	if !reflect.DeepEqual(evicted, baseline) {
		t.Fatal("evicted cache changed plan")
	}
}

func TestVerifiedInspectionCacheRejectsSameSizeRestoredTimeRewrite(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	const id = "00000000-0000-0000-0000-000000000001"
	relative := filepath.Join("home", codexFile(id))
	raw := codexTranscript(id, id, repo, fixedNow.Add(-time.Hour))
	tr.write(relative, raw)
	env := tr.env()
	cache := inspectioncache.New(filepath.Join(t.TempDir(), "inspection"))
	env.InspectionCache = cache
	var observations []inspectioncache.Observation
	env.ObserveInspection = func(observed inspectioncache.Observation) { observations = append(observations, observed) }
	plan(t, env, nil, config.Config{}, Filters{})
	if len(observations) != 1 {
		t.Fatalf("ordinary observations=%d", len(observations))
	}
	if err := cache.StoreBatch(t.Context(), observations); err != nil {
		t.Fatal(err)
	}
	actual := filepath.Join(env.Home, codexFile(id))
	info, err := os.Stat(actual)
	if err != nil {
		t.Fatal(err)
	}
	// Change the valid conversation type while preserving every byte count.
	rewritten := bytes.Replace([]byte(raw), []byte(`"type":"response_item"`), []byte(`"type":"unknown_type_"`), 1)
	if len(rewritten) != len(raw) || bytes.Equal(rewritten, []byte(raw)) {
		t.Fatal("fixture did not rewrite equal-length conversation type")
	}
	if err := os.WriteFile(actual, rewritten, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(actual, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	before := inspectioncache.OperationCounts()
	got := plan(t, env, nil, config.Config{}, Filters{})
	after := inspectioncache.OperationCounts()
	env.InspectionCache, env.ObserveInspection = nil, nil
	want := plan(t, env, nil, config.Config{}, Filters{})
	if !reflect.DeepEqual(got, want) || after.Hits != before.Hits {
		t.Fatalf("same-size restored stamp reused facts: got=%+v want=%+v counters=%+v/%+v", got, want, before, after)
	}
}

type cacheInspectionOverride string

const (
	cacheFilterOverride              cacheInspectionOverride = "filter"
	cacheInspectorOverride           cacheInspectionOverride = "inspector"
	cacheConfiguredInspectorOverride cacheInspectionOverride = "configured-inspector"
)

func TestWarmInspectionCachePreservesActualFilterAndInspectorOverrides(t *testing.T) {
	tr := newTree(t)
	repo := tr.repo("home/repo")
	const id = "00000000-0000-0000-0000-000000000001"
	tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, repo, fixedNow.Add(-time.Hour)))
	cache := inspectioncache.New(filepath.Join(t.TempDir(), "inspection"))
	env := tr.env()
	env.InspectionCache = cache
	var observations []inspectioncache.Observation
	env.ObserveInspection = func(observed inspectioncache.Observation) { observations = append(observations, observed) }
	plan(t, env, nil, config.Config{}, Filters{})
	if len(observations) != 1 {
		t.Fatalf("ordinary observations = %d", len(observations))
	}
	if err := cache.StoreBatch(t.Context(), observations); err != nil {
		t.Fatal(err)
	}
	for _, override := range []cacheInspectionOverride{cacheFilterOverride, cacheInspectorOverride, cacheConfiguredInspectorOverride} {
		t.Run(string(override), func(t *testing.T) {
			env := tr.env()
			var calls atomic.Int64
			switch override {
			case cacheFilterOverride:
				env.Sources = retainedInspectionSources{env.Sources, &calls}
			case cacheInspectorOverride:
				env.Imports = decoratedImports{env.Imports, &calls}
			case cacheConfiguredInspectorOverride:
				env.Imports = configuredImports{env.Imports}
			}
			want := plan(t, env, nil, config.Config{}, Filters{})
			env.InspectionCache = cache
			var observed []inspectioncache.Observation
			env.ObserveInspection = func(o inspectioncache.Observation) { observed = append(observed, o) }
			before := inspectioncache.OperationCounts()
			got := plan(t, env, nil, config.Config{}, Filters{})
			after := inspectioncache.OperationCounts()
			if !reflect.DeepEqual(got, want) || after.Hits != before.Hits || len(observed) != 0 {
				t.Fatalf("override bypassed: got=%+v want=%+v hits=%d observations=%d", got, want, after.Hits-before.Hits, len(observed))
			}
			if override != cacheConfiguredInspectorOverride && calls.Load() != 2 {
				t.Fatalf("actual override calls = %d", calls.Load())
			}
		})
	}
}
