package builtin

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agents/hookconfig"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // registers -update for go test ./... -update
)

const (
	testID                 agentmeta.ID        = "test"
	aliasID                agentmeta.ID        = "alias"
	unknownID              agentmeta.ID        = "unknown"
	brokenOperation        agentmeta.Operation = "broken"
	unimplementedOperation agentmeta.Operation = "unimplemented"
)

type fakeLauncher struct{}

func (*fakeLauncher) Executables() agentapi.Executables {
	return agentapi.Executables{Names: []string{"test"}, Install: "Test"}
}

func (*fakeLauncher) Args(r agentapi.LaunchRequest) ([]string, error) { return []string{r.Prompt}, nil }

func TestRegistryConstructorMatrix(t *testing.T) {
	c, err := agentmeta.New([]agentmeta.Descriptor{{ID: testID, Aliases: []string{"alias"}, DisplayName: "Test"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(nil, nil); err == nil {
		t.Fatal("accepted nil catalog")
	}
	valid := Integration{Descriptor: agentmeta.Descriptor{ID: testID}, Launcher: &fakeLauncher{}}
	var typedNil *fakeLauncher
	for _, tc := range []struct {
		name     string
		bindings []Integration
		wantErr  bool
	}{
		{"launch-only", []Integration{valid}, false},
		{"missing", nil, true}, {"duplicate", []Integration{valid, valid}, true},
		{"nil", []Integration{{Descriptor: valid.Descriptor}}, true},
		{"typed-nil", []Integration{{Descriptor: valid.Descriptor, Launcher: typedNil}}, true},
		{"unknown", []Integration{{Descriptor: agentmeta.Descriptor{ID: unknownID}, Launcher: &fakeLauncher{}}}, true},
		{"alias-binding", []Integration{{Descriptor: agentmeta.Descriptor{ID: aliasID}, Launcher: &fakeLauncher{}}}, true},
		{"metadata-override", []Integration{{Descriptor: agentmeta.Descriptor{ID: testID, DisplayName: "Override"}, Launcher: &fakeLauncher{}}}, true},
		{"operation-promise", []Integration{{Descriptor: agentmeta.Descriptor{ID: testID, Operations: []agentmeta.Operation{agentmeta.Launch}}, Launcher: &fakeLauncher{}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(c, tc.bindings)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
	r, err := New(c, []Integration{valid})
	if err != nil {
		t.Fatal(err)
	}
	b, ok := r.Lookup(" ALIAS ")
	if !ok || b.Descriptor.Operations[0] != agentmeta.Launch {
		t.Fatalf("lookup %+v %v", b, ok)
	}
	b.Descriptor.Aliases[0] = "broken"
	b.Descriptor.Operations[0] = brokenOperation
	supporting := r.Supporting(agentmeta.Launch)
	supporting[0].Descriptor.Aliases[0] = "broken"
	supporting[0].Descriptor.Operations[0] = brokenOperation
	d, _ := r.Catalog().Lookup("alias")
	if d.Aliases[0] != "alias" || d.Operations[0] != agentmeta.Launch {
		t.Fatalf("mutable registry %+v", d)
	}
	if len(r.Supporting(unimplementedOperation)) != 0 {
		t.Fatal("fabricated operation")
	}
}

type fakeRuntime struct{}

func (*fakeRuntime) Detect(agentapi.RuntimeEnvironment) agentapi.RuntimeObservation {
	return agentapi.RuntimeObservation{NativeID: "native", PresenceKey: "TEST_RUNTIME"}
}

func (*fakeRuntime) SessionEnvironmentKeys() []string { return []string{"TEST_RUNTIME", "TEST_PARENT"} }

func TestRuntimeProjectionAndCleanupUnion(t *testing.T) {
	t.Parallel()
	c, err := agentmeta.New([]agentmeta.Descriptor{{ID: testID, DisplayName: "Test"}, {ID: aliasID, DisplayName: "Alias"}})
	if err != nil {
		t.Fatal(err)
	}
	bindings := []Integration{{Descriptor: agentmeta.Descriptor{ID: testID}, Runtime: &fakeRuntime{}}, {Descriptor: agentmeta.Descriptor{ID: aliasID}, Launcher: &fakeLauncher{}, Runtime: &fakeRuntime{}}}
	r, err := New(c, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Launcher(string(testID)); ok {
		t.Fatal("runtime-only agent fabricated launcher")
	}
	if len(r.Supporting(agentmeta.Runtime)) != 2 || len(r.Supporting(agentmeta.Launch)) != 1 {
		t.Fatal("wrong operation projection")
	}
	d, _ := r.Catalog().Lookup(string(testID))
	if len(d.Operations) != 1 || d.Operations[0] != agentmeta.Runtime {
		t.Fatalf("runtime-only projection %+v", d)
	}
	observations := r.Observations(agentapi.RuntimeEnvironment{})
	if len(observations) != 2 || observations[0].Agent != testID || observations[1].Agent != aliasID {
		t.Fatalf("runtime order %+v", observations)
	}
	keys := r.SessionEnvironmentKeys()
	if len(keys) != 2 || keys[0] != "TEST_RUNTIME" || keys[1] != "TEST_PARENT" {
		t.Fatalf("cleanup union %q", keys)
	}
	keys[0] = "MUTATED"
	if r.SessionEnvironmentKeys()[0] != "TEST_RUNTIME" {
		t.Fatal("mutable cleanup union")
	}
	var typedNil *fakeRuntime
	bindings[0].Runtime = typedNil
	if _, err := New(c, bindings); err == nil {
		t.Fatal("accepted typed-nil runtime")
	}
}

func TestSourceBindingCoherenceAndNarrowLookup(t *testing.T) {
	catalog, err := agentmeta.New([]agentmeta.Descriptor{{ID: agentmeta.Codex, DisplayName: "Codex", Aliases: []string{"native-codex"}}})
	if err != nil {
		t.Fatal(err)
	}
	var nilProvider *sourceio.FileProvider
	var nilFilter *codex.Filter
	for _, binding := range []Integration{
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Codex}, Sources: codex.SourceProvider{}},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Codex}, Filter: codex.Filter{}},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Codex}, Sources: nilProvider, Filter: codex.Filter{}},
		{Descriptor: agentmeta.Descriptor{ID: agentmeta.Codex}, Sources: codex.SourceProvider{}, Filter: nilFilter},
	} {
		if _, err := New(catalog, []Integration{binding}); err == nil {
			t.Fatal("accepted incoherent source binding")
		}
	}
	r, err := New(catalog, []Integration{{Descriptor: agentmeta.Descriptor{ID: agentmeta.Codex}, Sources: codex.SourceProvider{}, Filter: codex.Filter{}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Supporting(agentmeta.Source)) != 1 || len(r.Supporting(agentmeta.Launch)) != 0 {
		t.Fatal("operation promise without implementation")
	}
	if _, _, ok := r.LookupSources(" NATIVE-CODEX "); !ok {
		t.Fatal("source alias unresolved")
	}
	if n := testing.AllocsPerRun(100, func() { r.LookupSources("native-codex") }); n != 0 {
		t.Fatalf("source lookup allocates %g", n)
	}
}

func TestHookOperationProjectionsAgreeWithCatalog(t *testing.T) {
	t.Parallel()
	r := NewBuiltins()
	for _, want := range r.Catalog().All() {
		got, ok := r.Lookup(string(want.ID))
		if !ok || !reflect.DeepEqual(got.Descriptor, want) {
			t.Fatalf("lookup differs from derived catalog: %+v, want %+v", got.Descriptor, want)
		}
	}
	for _, operation := range []agentmeta.Operation{agentmeta.ManagedHooks, agentmeta.LifecycleHooks} {
		for _, got := range r.Supporting(operation) {
			want, _ := r.Catalog().Lookup(string(got.Descriptor.ID))
			if !reflect.DeepEqual(got.Descriptor, want) {
				t.Fatalf("%s projection differs from catalog: %+v, want %+v", operation, got.Descriptor, want)
			}
		}
	}
}

// Narrow operational lookup must not clone public metadata on each hook.
func TestLookupDecoderPreservesResolutionWithoutMetadataAllocation(t *testing.T) {
	r := NewBuiltins()
	for _, name := range []string{"codex", "claude", "claude-code", "cursor"} {
		full, ok := r.Lookup(name)
		narrow, found := r.LookupDecoder(name)
		if !ok || !found || !reflect.DeepEqual(narrow, full.Decoder) {
			t.Fatalf("decoder %s changed", name)
		}
		allocs := testing.AllocsPerRun(100, func() { _, _ = r.LookupDecoder(name) })
		t.Logf("%s narrow lookup allocations=%g", name, allocs)
		if allocs != 0 {
			t.Errorf("%s narrow lookup clones metadata: %g allocations", name, allocs)
		}
	}
	for _, name := range []string{"missing", ""} {
		if d, ok := r.LookupDecoder(name); ok || d != nil {
			t.Fatalf("unknown decoder %q", name)
		}
	}
}

type narrowTestDecoder struct{}

func (*narrowTestDecoder) Decode(context.Context, agentapi.HookInput) ([]agentapi.LifecycleEvent, error) {
	return nil, nil
}

func TestNarrowDecoderInjectedAliasAndMissingCapability(t *testing.T) {
	c, err := agentmeta.New([]agentmeta.Descriptor{{ID: testID, DisplayName: "Test", Aliases: []string{"alias"}}, {ID: unknownID, DisplayName: "Unknown"}})
	if err != nil {
		t.Fatal(err)
	}
	decoder := &narrowTestDecoder{}
	r, err := New(c, []Integration{{Descriptor: agentmeta.Descriptor{ID: testID}, Decoder: decoder}, {Descriptor: agentmeta.Descriptor{ID: unknownID}, Launcher: &fakeLauncher{}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"test", "alias", " ALIAS "} {
		if d, ok := r.LookupDecoder(name); !ok || d != decoder {
			t.Fatalf("alias %q: %v %v", name, d, ok)
		}
	}
	if d, ok := r.LookupDecoder("unknown"); ok || d != nil {
		t.Fatal("fabricated absent decoder")
	}
	b, _ := r.Lookup("alias")
	b.Descriptor.Aliases[0] = "broken"
	b.Descriptor.Operations[0] = brokenOperation
	if d, ok := r.LookupDecoder("alias"); !ok || d != decoder {
		t.Fatal("full descriptor mutation corrupted narrow lookup")
	}
}

func TestNativeDecodeEvidenceAndInputSnapshots(t *testing.T) {
	r := NewBuiltins()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, name := range []string{"claude", "codex", "cursor"} {
		port, _ := r.LookupDecoder(name)
		decoder := port.(hookconfig.Decoder)
		var events []string
		for event := range decoder.Spec.Events {
			events = append(events, event)
		}
		slices.Sort(events)
		for _, event := range events {
			payload := map[string]any{"hook_event_name": event, "session_id": "native", "conversation_id": "conversation", "cwd": "/synthetic/project", "source": "startup", "transcript_path": "/synthetic/source", "agent_id": "child", "agent_transcript_path": "/synthetic/child", "agent_type": "explorer", "generation_id": "turn", "model": "model", "message_id": "message", "status": "completed", "last_assistant_message": "final", "text": "response", "prompt": "PRIVATE-PROMPT", "tool_input": "PRIVATE-TOOL", "model_params": []any{map[string]any{"id": "safe", "value": "chosen", "extra": "PRIVATE-EXTRA"}}}
			before, _ := json.Marshal(payload)
			batch, err := port.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: at})
			if err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(payload)
			if string(before) != string(after) {
				t.Fatalf("%s/%s mutated input", name, event)
			}
			encoded, err := json.Marshal(batch)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"PRIVATE-PROMPT", "PRIVATE-TOOL", "PRIVATE-EXTRA"} {
				if strings.Contains(string(encoded), private) {
					t.Fatalf("%s/%s leaked %s", name, event, private)
				}
			}
			for _, item := range batch {
				if item.NativeEvent != event || item.Reason != strings.ToLower(event) {
					t.Fatalf("%s/%s native identity changed", name, event)
				}
				if item.Kind == agentapi.EventStop && len(item.Evidence) != 2 {
					t.Fatalf("%s/%s stop lost distinct evidence", name, event)
				}
				for _, e := range item.Evidence {
					if e.Provenance != "hook:"+name+":"+strings.ToLower(event) || !e.ObservedAt.Equal(at) {
						t.Fatal("evidence provenance/time changed")
					}
				}
			}
			t.Logf("SNAPSHOT %s/%s %s", name, event, encoded)
		}
	}
}

// Native parser policy changes must not refresh unrelated agents' metadata.
func TestCodexIdentityParserVersionIsScoped(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{"codex": "0.28.0", "claude": "0.21.0", "cursor": "0.23.0"} {
		parser, ok := NewBuiltins().LookupParser(name)
		if !ok || parser.Version() != want {
			t.Fatalf("%s parser version: %v", name, parser)
		}
	}
}
