package hookconfig

import (
	"bytes"
	"encoding/json"
	"strings"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/jsonedit"
	"testing"
)

const Owner = "agent-archive lifecycle capture"

const prototypeOwner = "Recording private skill-run evidence"

type testHook struct{ Executable string }

func fuzzSpec(name string) Spec {
	flat := name == "cursor"
	events := []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd", "SubagentStop"}
	switch agentmeta.ID(name) {
	case agentmeta.Claude:
		events = append(events, "StopFailure")
	case agentmeta.Codex:
		events = append(events, "Interrupt")
	case agentmeta.Cursor:
		events = []string{"sessionStart", "beforeSubmitPrompt", "afterAgentResponse", "stop", "sessionEnd", "subagentStop"}
	case agentmeta.Muse:
		panic("runtime-only Muse cannot configure hooks")
	}
	return Spec{Name: name, Events: events, Flat: flat, Version: flat, Owner: Owner, PrototypeOwner: prototypeOwner}
}

func Merge(data []byte, name string, h testHook) ([]byte, error) {
	return merge(data, fuzzSpec(name), Hook{Executable: h.Executable})
}

func Remove(data []byte, name string, h testHook) ([]byte, bool, error) {
	return remove(data, fuzzSpec(name), Hook{Executable: h.Executable})
}

func FuzzMergeRemove(f *testing.F) {
	for _, seed := range []string{
		"", "{}", "{}\n", `{"model":"opus"}`, "{\n\t\"model\": \"opus\"\n}\n",
		"{\r\n  \"model\": \"opus\"\r\n}\r\n", "{\"a\": 1,\n  \"b\": 2\n}\n",
		"{\n  \"hooks\": {},\n  \"model\": \"x\"\n}\n", `{"hooks": null}`,
		`{"version":1,"hooks":{"stop":[{"command":"echo hi"}]}}`, `{"x":1}`,
		"{\n  \"hooks\": {\n    \"Stop\": [\n      {\"hooks\": [{\"type\": \"command\", \"command\": \"echo hi\"}]}\n    ]\n  }\n}\n",
		`{"n": 12345678901234567890, "s": "a&b<c>é\/"}`, "\xef\xbb\xbf{}", `{"a":1,}`,
	} {
		for harness := range uint8(3) {
			f.Add([]byte(seed), harness)
		}
	}
	f.Fuzz(func(t *testing.T, input []byte, which uint8) {
		harness := []string{"claude", "codex", "cursor"}[which%3]
		if bytes.Contains(input, []byte(Owner)) || bytes.Contains(input, []byte(prototypeOwner)) {
			return
		}
		hook := testHook{Executable: "/usr/local/bin/agent-archive"}
		merged, err := Merge(input, harness, hook)
		if err != nil {
			return
		}
		if !json.Valid(merged) {
			t.Fatalf("invalid JSON:\n%s", merged)
		}
		again, err := Merge(merged, harness, hook)
		if err != nil || !bytes.Equal(again, merged) {
			t.Fatalf("merge is not idempotent (%v):\n%s\n---\n%s", err, merged, again)
		}
		removed, changed, err := Remove(merged, harness, testHook{})
		if err != nil || !changed {
			t.Fatalf("remove: changed=%v err=%v", changed, err)
		}
		if !json.Valid(removed) {
			t.Fatalf("remove left invalid JSON:\n%s", removed)
		}
		original, err := parseDocument(input)
		if err != nil {
			t.Fatalf("merge accepted what parse refuses: %v", err)
		}
		_, hadHooks := original.Root.Get("hooks")
		version, hadVersion := original.Root.Get("version")
		// An object with no members at all is laid out afresh when the hooks
		// go in, so only its emptiness comes back, not the whitespace inside
		// its braces.
		// Cursor's lone "version" goes with the hooks, since it is what
		// setup adds to a file it creates (see Remove).
		empty := len(original.Root.Members) == 0 || harness == "cursor" && !hadHooks && len(usersMembers(original.Root).Members) == 0
		if empty && !Empty(removed) {
			t.Fatalf("an empty file came back with %q", removed)
		}
		if !empty && !hadHooks && (harness != "cursor" || hadVersion && isOne(version)) && !bytes.Equal(removed, input) {
			t.Fatalf("round trip changed the file:\n%q\n---\n%q", input, removed)
		}
		// Whatever the shape, the user's members other than "hooks" and
		// "version" are untouched.
		left, err := parseDocument(removed)
		if err != nil {
			t.Fatal(err)
		}
		if !sameJSON(usersMembers(original.Root), usersMembers(left.Root)) {
			t.Fatalf("the user's settings changed:\n%q\n---\n%q", input, removed)
		}
	})
}

// usersMembers is root without the members setup owns, in file order.
func usersMembers(root *object) *object {
	out := &object{}
	for _, m := range root.Members {
		//lint:ignore LV1001 top-level member names of a user's JSON file are an open set; these two are setup's
		if m.Key != "hooks" && m.Key != "version" {
			out.Members = append(out.Members, m)
		}
	}
	return out
}

func sameJSON(a, b any) bool {
	var x, y bytes.Buffer
	return encodeValue(&x, a) == nil && encodeValue(&y, b) == nil && bytes.Equal(x.Bytes(), y.Bytes())
}

// Regression: hook ownership review, 2026-09 (1a9420b).

func encodeValue(dst *bytes.Buffer, v any) error { return jsonedit.EncodeValue(dst, v) }

func TestNamedDefinitionEnabledToggle(t *testing.T) {
	spec := Spec{
		Name: "agy", Flat: true, Direct: true, Rootless: true, Definition: "agent-archive",
		Owner: Owner, PrototypeOwner: prototypeOwner,
		Events: []string{"PreInvocation", "PostInvocation", "Stop"},
	}
	hook := Hook{Executable: "/usr/local/bin/agent-archive"}
	existing := []byte(`{
  "agent-archive": {
    "enabled": false,
    "PreInvocation": [
      {
        "type": "command",
        "command": "'/usr/local/bin/agent-archive' _hook --harness agy # agent-archive lifecycle capture",
        "timeout": "2"
      }
    ],
    "PostInvocation": [
      {
        "type": "command",
        "command": "'/usr/local/bin/agent-archive' _hook --harness agy # agent-archive lifecycle capture",
        "timeout": "2"
      }
    ],
    "Stop": [
      {
        "type": "command",
        "command": "'/usr/local/bin/agent-archive' _hook --harness agy # agent-archive lifecycle capture",
        "timeout": "2"
      }
    ]
  }
}`)
	merged, err := merge(existing, spec, hook)
	if err != nil {
		t.Fatalf("merge error = %v", err)
	}
	doc, err := parseDocument(merged)
	if err != nil {
		t.Fatalf("parse error = %v", err)
	}
	defObj, ok := doc.Root.Get("agent-archive")
	if !ok {
		t.Fatalf("missing agent-archive definition in %s", string(merged))
	}
	def, ok := defObj.(*object)
	if !ok {
		t.Fatalf("agent-archive is not an object: %#v", defObj)
	}
	enabledVal, ok := def.Get("enabled")
	if !ok || enabledVal != false {
		t.Fatalf("enabled toggle not preserved: got %#v", enabledVal)
	}

	installedOK, err := installed(existing, spec, hook)
	if err != nil || !installedOK {
		t.Fatalf("installed error = %v, ok = %v", err, installedOK)
	}

	removed, changed, err := remove(existing, spec, hook)
	if err != nil || !changed {
		t.Fatalf("remove error = %v, changed = %v", err, changed)
	}
	if strings.TrimSpace(string(removed)) != "{}" {
		t.Fatalf("remove left behind definition: %s", string(removed))
	}

	// Verify non-boolean enabled produces an error
	invalid := []byte(`{
  "agent-archive": {
    "enabled": "not-a-bool"
  }
}`)
	if _, err := merge(invalid, spec, hook); err == nil || !strings.Contains(err.Error(), "invalid boolean for enabled") {
		t.Fatalf("merge with non-boolean enabled expected 'invalid boolean for enabled', got: %v", err)
	}
	if _, err := installed(invalid, spec, hook); err == nil || !strings.Contains(err.Error(), "invalid boolean for enabled") {
		t.Fatalf("installed with non-boolean enabled expected 'invalid boolean for enabled', got: %v", err)
	}
	if _, _, err := otherInstallations(invalid, spec, hook); err == nil || !strings.Contains(err.Error(), "invalid boolean for enabled") {
		t.Fatalf("otherInstallations with non-boolean enabled expected 'invalid boolean for enabled', got: %v", err)
	}
}

func TestEventInCommandRoundtrip(t *testing.T) {
	spec := Spec{
		Name: "agy", Flat: true, Direct: true, Rootless: true, Definition: "agent-archive",
		Owner: Owner, PrototypeOwner: prototypeOwner, EventInCommand: true,
		Events: []string{"PreInvocation", "PostInvocation", "Stop"},
	}
	hook := Hook{Executable: "/usr/local/bin/agent-archive"}
	merged, err := merge(nil, spec, hook)
	if err != nil {
		t.Fatalf("merge error = %v", err)
	}
	doc, err := parseDocument(merged)
	if err != nil {
		t.Fatalf("parse error = %v", err)
	}
	defObj, ok := doc.Root.Get("agent-archive")
	if !ok {
		t.Fatalf("missing agent-archive definition in %s", string(merged))
	}
	def, ok := defObj.(*object)
	if !ok {
		t.Fatalf("agent-archive is not an object: %#v", defObj)
	}
	for _, event := range spec.Events {
		val, ok := def.Get(event)
		if !ok {
			t.Fatalf("missing event %s", event)
		}
		list, ok := val.([]any)
		if !ok || len(list) != 1 {
			t.Fatalf("invalid list for event %s: %#v", event, val)
		}
		handler, ok := list[0].(*object)
		if !ok {
			t.Fatalf("invalid handler for event %s", event)
		}
		cmd, _ := handler.Get("command")
		wantCmd, _ := hook.commandForEvent(spec, event)
		if cmd != wantCmd {
			t.Fatalf("event %s command mismatch: got %v, want %v", event, cmd, wantCmd)
		}
	}

	installedOK, err := installed(merged, spec, hook)
	if err != nil || !installedOK {
		t.Fatalf("installed error = %v, ok = %v", err, installedOK)
	}

	// Idempotent merge
	mergedAgain, err := merge(merged, spec, hook)
	if err != nil || !bytes.Equal(merged, mergedAgain) {
		t.Fatalf("merge not idempotent: got %s, want %s", string(mergedAgain), string(merged))
	}

	// Remove
	removed, changed, err := remove(merged, spec, hook)
	if err != nil || !changed {
		t.Fatalf("remove error = %v, changed = %v", err, changed)
	}
	if strings.TrimSpace(string(removed)) != "{}" {
		t.Fatalf("remove left behind definition: %s", string(removed))
	}
}

