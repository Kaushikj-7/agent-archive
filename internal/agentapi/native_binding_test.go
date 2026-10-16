package agentapi

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestSourceBindingCodexRoundTripIsLossless(t *testing.T) {
	zero := uint64(0)
	at := time.Date(2026, 10, 1, 1, 2, 3, 456, time.UTC)
	original := archive.CodexSourceBinding{Version: 1, FirstNativeTaskAt: at, FirstNativeTaskID: "task", PhysicalProducerVersion: "v", PhysicalProducerOriginator: "producer", Child: true, NativeThreadID: "thread", NativeCreatedAt: at, SelectedCwd: "/selected", Cwd: "/cwd", ProducerSource: `{"subagent":{"thread_spawn":{"parent_thread_id":"parent"}}}`, RootID: "root", ParentID: "parent", OwnStart: &zero, PhysicalRolloutID: "rollout", Path: "/native/rollout", Home: "/native"}
	carrier := BindCodex(&original)
	if carrier.Native != nil || carrier.Codex != &original {
		t.Fatal("carrier flattened facts")
	}
	raw, err := json.Marshal(archive.SessionRegistration{CodexBinding: carrier.Codex})
	if err != nil {
		t.Fatal(err)
	}
	var restored archive.SessionRegistration
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, *restored.CodexBinding) {
		t.Fatal("Codex facts lost")
	}
	if BindCodex(nil) != nil || (*SourceBinding)(nil).CodexBinding() != nil {
		t.Fatal("legacy nil semantics changed")
	}
	for _, binding := range []*SourceBinding{{}, {Codex: &original, Native: &archive.NativeSourceBinding{}}, {Native: &archive.NativeSourceBinding{Agent: "other"}}} {
		if binding.ValidateFor("codex") == nil {
			t.Fatal("ambiguous or foreign tag accepted")
		}
	}
}
