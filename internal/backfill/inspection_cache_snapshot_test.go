package backfill

import (
	"context"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/inspectioncache"
	"github.com/wangjohn/agent-archive/internal/trace"
)

type cacheInputDecoratorSources struct {
	agentapi.SourcesLookup
	input     string
	readError bool
}

func (s cacheInputDecoratorSources) LookupSources(name string) (agentapi.SourceProvider, agentapi.TranscriptFilter, bool) {
	p, f, ok := s.SourcesLookup.LookupSources(name)
	if name == "codex" && ok {
		p = cacheInputDecoratorProvider{p, s.input, s.readError}
	}
	return p, f, ok
}

type cacheInputDecoratorProvider struct {
	agentapi.SourceProvider
	input     string
	readError bool
}

func (p cacheInputDecoratorProvider) OpenPass(ctx context.Context, env agentapi.SourceEnvironment) (agentapi.SourcePass, error) {
	pass, err := p.SourceProvider.OpenPass(ctx, env)
	if err != nil {
		return nil, err
	}
	return cacheInputDecoratorPass{pass, p.input, p.readError}, nil
}

type cacheInputDecoratorPass struct {
	agentapi.SourcePass
	input     string
	readError bool
}

func (p cacheInputDecoratorPass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	snapshot, err := p.SourcePass.Read(ctx, ref, limits)
	if err != nil {
		return nil, err
	}
	return cacheInputDecoratorSnapshot{snapshot, snapshot.(agentapi.VerifiedContentSnapshot), p.input, p.readError}, nil
}

type cacheInputDecoratorSnapshot struct {
	agentapi.SourceSnapshot
	agentapi.VerifiedContentSnapshot
	input     string
	readError bool
}

func (s cacheInputDecoratorSnapshot) Input() agentapi.NativeInput {
	in := s.SourceSnapshot.Input()
	in.File = cacheInputDecoratorFile{in.File, s.input, s.readError}
	return in
}

type cacheInputDecoratorFile struct {
	agentapi.FileInput
	input     string
	readError bool
}

func (f cacheInputDecoratorFile) Length() int64 {
	if f.readError {
		return f.FileInput.Length()
	}
	return int64(len(f.input))
}

func (f cacheInputDecoratorFile) ReadAt(data []byte, offset int64) (int, error) {
	if f.readError {
		return 0, io.ErrUnexpectedEOF
	}
	return strings.NewReader(f.input).ReadAt(data, offset)
}

// The read-error case leaves byte identity/boundary unchanged and injects only
// a transient Input read failure. The remaining cases deliberately forward an
// underlying proof while substituting bytes, exposing capability inheritance.
func TestInspectionCacheHonorsActualSnapshotInput(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     string
		readError bool
	}{
		{"read-error", "", true},
		{"nonconversation", `{"type":"event_msg","payload":{"message":"synthetic event"}}` + "\n", false},
		{"different-conversation", `{"timestamp":"2026-09-23T19:00:00Z","type":"response_item","role":"user"}` + "\n", false},
		{"malformed-metadata", `{"type":"session_meta","payload":{"id":42}}` + "\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTree(t)
			root := tr.repo("home/repo")
			const id = "00000000-0000-4000-8000-000000000001"
			tr.write(filepath.Join("home", codexFile(id)), codexTranscript(id, id, root, fixedNow.Add(-time.Hour)))
			env := tr.env()
			actual := cacheInputDecoratorSources{env.Sources, tc.input, tc.readError}
			env.Sources = actual
			disabled := plan(t, env, nil, config.Config{}, Filters{})
			env.InspectionCache = inspectioncache.New(filepath.Join(t.TempDir(), "cold"))
			var observed []inspectioncache.Observation
			env.ObserveInspection = func(o inspectioncache.Observation) { observed = append(observed, o) }
			disableTrace := trace.Enable()
			t.Cleanup(disableTrace)
			cold := plan(t, env, nil, config.Config{}, Filters{})
			coldTrace := inspectionCacheTrace()
			disableTrace()
			if len(observed) != 0 || strings.Contains(coldTrace, "inspection_cache_lookups") {
				t.Errorf("decorated cold input acquired cache authority: observations=%d trace=%s", len(observed), coldTrace)
			}
			if !reflect.DeepEqual(disabled, cold) {
				t.Fatalf("cold differs: disabled=%+v cold=%+v", disabled.Candidates, cold.Candidates)
			}
			// Even a successful decorated conversation must not publish facts
			// under the underlying ordinary input's proof.
			if err := env.InspectionCache.StoreBatch(t.Context(), observed); err != nil {
				t.Fatal(err)
			}
			ordinary := tr.env()
			baseline := plan(t, ordinary, nil, config.Config{}, Filters{})
			ordinary.InspectionCache = env.InspectionCache
			if got := plan(t, ordinary, nil, config.Config{}, Filters{}); !reflect.DeepEqual(got, baseline) {
				t.Fatal("decorated observation poisoned ordinary planning")
			}
			cache := inspectioncache.New(filepath.Join(t.TempDir(), "warm"))
			prime := tr.env()
			prime.InspectionCache = cache
			var observations []inspectioncache.Observation
			prime.ObserveInspection = func(o inspectioncache.Observation) { observations = append(observations, o) }
			plan(t, prime, nil, config.Config{}, Filters{})
			if len(observations) != 1 {
				t.Fatalf("prime observations=%d", len(observations))
			}
			if err := cache.StoreBatch(t.Context(), observations); err != nil {
				t.Fatal(err)
			}
			env.InspectionCache = cache
			disableTrace = trace.Enable()
			t.Cleanup(disableTrace)
			warm := plan(t, env, nil, config.Config{}, Filters{})
			warmTrace := inspectionCacheTrace()
			disableTrace()
			if len(observed) != 0 || strings.Contains(warmTrace, "inspection_cache_lookups") {
				t.Errorf("decorated warm input acquired cache authority: observations=%d trace=%s", len(observed), warmTrace)
			}
			if !reflect.DeepEqual(disabled, warm) {
				t.Fatalf("warm cache bypassed decorated Input: disabled skip=%s warm skip=%s disabled start=%s warm start=%s", disabled.Candidates[0].Skip, warm.Candidates[0].Skip, disabled.Candidates[0].StartedAt, warm.Candidates[0].StartedAt)
			}
		})
	}
}
