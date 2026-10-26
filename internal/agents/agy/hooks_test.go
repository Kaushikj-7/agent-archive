package agy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestHooksUseAntigravityDirectLifecycleShape(t *testing.T) {
	changes, err := Hooks().Plan(agentapi.HookPlanRequest{
		Action: agentapi.HookInstall,
		File:   agentapi.HookFile{Path: "/tmp/hooks.json"},
		Owner:  agentapi.HookOwner{Executable: "/usr/local/bin/agent-archive", DataHome: "/tmp/archive"},
	})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(changes[0].After, &document); err != nil {
		t.Fatal(err)
	}
	if document["hooks"] != nil {
		t.Fatalf("Antigravity definition must not be nested under hooks: %#v", document)
	}
	hooks := document["agent-archive"].(map[string]any)
	entry := hooks["PreInvocation"].([]any)[0].(map[string]any)
	if entry["type"] != "command" || entry["hooks"] != nil {
		t.Fatalf("unexpected Antigravity hook entry: %#v", entry)
	}
	owner := agentapi.HookOwner{Executable: "/usr/local/bin/agent-archive", DataHome: "/tmp/archive"}
	inspection, err := Hooks().Inspect(agentapi.HookInspectionRequest{File: agentapi.HookFile{Path: "/tmp/hooks.json", Bytes: changes[0].After, Present: true}, Owner: owner})
	if err != nil || !inspection.Installed {
		t.Fatalf("Inspect() = %#v, error = %v", inspection, err)
	}
	removed, err := Hooks().Plan(agentapi.HookPlanRequest{Action: agentapi.HookRemove, File: agentapi.HookFile{Path: "/tmp/hooks.json", Bytes: changes[0].After, Present: true}, Owner: owner})
	if err != nil || len(removed) != 1 || strings.TrimSpace(string(removed[0].After)) != "{}" {
		t.Fatalf("remove plan = %#v, error = %v", removed, err)
	}
}
