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
	for _, event := range []string{"SessionStart", "PreInvocation", "PostInvocation", "Stop"} {
		list, ok := hooks[event].([]any)
		if !ok || len(list) != 1 {
			t.Fatalf("missing hook entry for %s: %#v", event, hooks[event])
		}
		entry := list[0].(map[string]any)
		if entry["type"] != "command" || entry["hooks"] != nil {
			t.Fatalf("unexpected Antigravity hook entry: %#v", entry)
		}
		cmd, _ := entry["command"].(string)
		if !strings.Contains(cmd, "--event "+event) {
			t.Fatalf("command for %s missing --event flag: %s", event, cmd)
		}
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

func TestHooksPreserveEnabledToggle(t *testing.T) {
	existing := []byte(`{
  "agent-archive": {
    "enabled": false,
    "SessionStart": [
      {
        "type": "command",
        "command": "AGENT_ARCHIVE_HOME='/tmp/archive' '/usr/local/bin/agent-archive' _hook --harness agy --event SessionStart # agent-archive lifecycle capture",
        "timeout": "2"
      }
    ],
    "PreInvocation": [
      {
        "type": "command",
        "command": "AGENT_ARCHIVE_HOME='/tmp/archive' '/usr/local/bin/agent-archive' _hook --harness agy --event PreInvocation # agent-archive lifecycle capture",
        "timeout": "2"
      }
    ],
    "PostInvocation": [
      {
        "type": "command",
        "command": "AGENT_ARCHIVE_HOME='/tmp/archive' '/usr/local/bin/agent-archive' _hook --harness agy --event PostInvocation # agent-archive lifecycle capture",
        "timeout": "2"
      }
    ],
    "Stop": [
      {
        "type": "command",
        "command": "AGENT_ARCHIVE_HOME='/tmp/archive' '/usr/local/bin/agent-archive' _hook --harness agy --event Stop # agent-archive lifecycle capture",
        "timeout": "2"
      }
    ]
  }
}`)
	owner := agentapi.HookOwner{Executable: "/usr/local/bin/agent-archive", DataHome: "/tmp/archive"}
	inspection, err := Hooks().Inspect(agentapi.HookInspectionRequest{
		File:  agentapi.HookFile{Path: "/tmp/hooks.json", Bytes: existing, Present: true},
		Owner: owner,
	})
	if err != nil || !inspection.Installed {
		t.Fatalf("Inspect() on disabled hook = %#v, error = %v", inspection, err)
	}

	plan, err := Hooks().Plan(agentapi.HookPlanRequest{
		Action: agentapi.HookRemove,
		File:   agentapi.HookFile{Path: "/tmp/hooks.json", Bytes: existing, Present: true},
		Owner:  owner,
	})
	if err != nil {
		t.Fatalf("Plan(Remove) error = %v", err)
	}
	if strings.TrimSpace(string(plan[0].After)) != "{}" {
		t.Fatalf("remove left behind definition: %s", string(plan[0].After))
	}

	// Verify that installing hooks preserves existing "enabled": false
	initial := []byte(`{
  "agent-archive": {
    "enabled": false
  }
}`)
	installedPlan, err := Hooks().Plan(agentapi.HookPlanRequest{
		Action: agentapi.HookInstall,
		File:   agentapi.HookFile{Path: "/tmp/hooks.json", Bytes: initial, Present: true},
		Owner:  owner,
	})
	if err != nil {
		t.Fatalf("Plan(Install) error = %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(installedPlan[0].After, &doc); err != nil {
		t.Fatal(err)
	}
	def := doc["agent-archive"].(map[string]any)
	if def["enabled"] != false {
		t.Fatalf("Plan(Install) did not preserve enabled: false, got %#v", def["enabled"])
	}
	for _, event := range []string{"SessionStart", "PreInvocation", "PostInvocation", "Stop"} {
		list, ok := def[event].([]any)
		if !ok || len(list) != 1 {
			t.Fatalf("missing installed hook for %s", event)
		}
		entry := list[0].(map[string]any)
		cmd, _ := entry["command"].(string)
		if !strings.Contains(cmd, "--event "+event) {
			t.Fatalf("expected --event %s in command, got %s", event, cmd)
		}
	}
}
