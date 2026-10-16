package builtin

import (
	"reflect"
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

func TestMuseBindingOffersLocalHandoffWithoutCapture(t *testing.T) {
	t.Parallel()
	r := NewBuiltins()
	b, ok := r.Lookup(" MUSE-CODE ")
	if !ok || b.Descriptor.ID != agentmeta.Muse || b.Descriptor.DisplayName != "Muse Code" || !slices.Equal(b.Descriptor.Operations, []agentmeta.Operation{agentmeta.Runtime, agentmeta.Parse, agentmeta.Source, agentmeta.NativeInspection, agentmeta.HistoricalDiscovery}) {
		t.Fatalf("binding %+v; found %v", b, ok)
	}
	if b.Runtime == nil {
		t.Fatal("runtime implementation missing")
	}
	if b.Sources == nil || b.Filter == nil || b.Parser == nil {
		t.Fatal("local source/parser missing")
	}
	b.Sources, b.Filter, b.Parser = nil, nil, nil
	b.Runtime = nil
	b.NativeHeaders, b.Discovery, b.Preview = nil, nil, nil
	b.Descriptor = agentmeta.Descriptor{}
	if !reflect.DeepEqual(b, Integration{}) {
		t.Fatalf("unexpected Muse ports: %+v", b)
	}
	observations := r.Observations(agentapi.RuntimeEnvironment{LookupEnv: func(key string) (string, bool) {
		return "opaque-marker", key == "MUSE_THREAD_ID"
	}})
	if len(observations) != 1 || observations[0] != (agentapi.AgentRuntime{Agent: agentmeta.Muse, RuntimeObservation: agentapi.RuntimeObservation{PresenceKey: "MUSE_THREAD_ID"}}) {
		t.Fatalf("presence-only observation lost or upgraded: %+v", observations)
	}
	if got := agentmeta.SetupNames(r.Catalog()); !slices.Equal(got, []string{"codex", "claude", "cursor"}) {
		t.Fatalf("runtime-only integration offered for capture: %v", got)
	}
	for _, names := range [][]string{r.HookAgents(), r.SkillAgents(), r.VersionAgents()} {
		if slices.Contains(names, "muse") {
			t.Fatalf("unsupported projection: %v", names)
		}
	}
}
