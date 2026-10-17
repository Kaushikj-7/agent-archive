package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestRootHookObservationsRequireExactQualifiedIdentity(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			payload := map[string]any{"hook_event_name": name, "session_id": "Opaque_root-12", "cwd": "/synthetic/work", "transcript_path": nil, "unknown_body": "PRIVATE"}
			if name == "SessionStart" {
				payload["source"] = "startup"
			}
			if name == "SessionEnd" {
				payload["reason"] = "PRIVATE"
			}
			in := agentapi.HookInput{Payload: payload, Environment: map[string]string{"MUSE_SESSION_ID": "Opaque_root-12", "MUSE_THREAD_ID": "Opaque_root-12"}, ObservedAt: time.Now()}
			events, err := Decoder().Decode(context.Background(), in)
			if err != nil || len(events) != 1 {
				t.Fatalf("qualified observation: %+v %v", events, err)
			}
			e := events[0]
			if e.NativeHookContract != rootHookContract || e.Start.Kind != agentapi.FreshUnknown || e.Source.Path != "" || e.Locator != agentapi.LocatorNone || e.Deferred != agentapi.DeferredNone || len(e.Evidence) != 0 {
				t.Fatalf("observation gained authority: %+v", e)
			}
			if strings.Contains(e.Reason, "PRIVATE") {
				t.Fatal("raw reason escaped")
			}
			in.Environment["MUSE_THREAD_ID"] = "another"
			events, err = Decoder().Decode(context.Background(), in)
			if err != nil || len(events) != 0 {
				t.Fatal("mismatched IDs accepted")
			}
		})
	}
	for _, id := range []string{"", "_root", "-root", "root/path", "root\n", "røot", strings.Repeat("a", 129)} {
		input := agentapi.HookInput{Payload: map[string]any{"hook_event_name": "SessionStart", "session_id": id, "cwd": "/work", "transcript_path": nil, "source": "startup"}, Environment: map[string]string{"MUSE_SESSION_ID": id, "MUSE_THREAD_ID": id}}
		events, err := Decoder().Decode(context.Background(), input)
		if err != nil || len(events) != 0 {
			t.Fatalf("unsupported ID accepted %q", id)
		}
	}
}

func TestRootHookSettingsPreserveForeignBytesAndExactSchema(t *testing.T) {
	t.Parallel()
	h := Hooks()
	owner := agentapi.HookOwner{Executable: "/synthetic/bin/archive", DefaultDataHome: "/synthetic/data"}
	foreign := `{ "matcher" : "*", "hooks" : [ { "type" : "command", "command" : "echo \\u003cforeign", "timeout" : 7 } ] }`
	before := []byte("{\n  \"schema_version\" : 1,\n  \"private_setting\" : { \"raw\" : \"\\u003c\\u003e\" },\n  \"hooks\" : {\"Stop\": [" + foreign + "]}\n}\n")
	request := agentapi.HookPlanRequest{Action: agentapi.HookInstall, Owner: owner, File: agentapi.HookFile{Path: "/synthetic/settings.json", Bytes: before, Present: true, Regular: true, Mode: 0600}}
	changes, err := h.Plan(request)
	if err != nil || len(changes) != 1 {
		t.Fatalf("plan: %v %+v", err, changes)
	}
	after := changes[0].After
	if !bytes.Contains(after, []byte(foreign)) || !bytes.Contains(after, []byte(`"private_setting" : { "raw" : "\u003c\u003e" }`)) {
		t.Fatalf("foreign bytes rewritten: %s", after)
	}
	if bytes.Contains(after, []byte("statusMessage")) || bytes.Contains(after, []byte(`"version"`)) || !bytes.Contains(after, []byte("AGENT_ARCHIVE_HOME=")) {
		t.Fatalf("unqualified generated settings: %s", after)
	}
	inspect, err := h.Inspect(agentapi.HookInspectionRequest{Owner: owner, File: agentapi.HookFile{Path: request.File.Path, Bytes: after, Present: true, Regular: true}})
	if err != nil || !inspect.Installed {
		t.Fatalf("generated settings unhealthy: %+v %v", inspect, err)
	}
	mutated := bytes.ReplaceAll(after, []byte(`"timeout":2`), []byte(`"timeout":9`))
	inspect, err = h.Inspect(agentapi.HookInspectionRequest{Owner: owner, File: agentapi.HookFile{Path: request.File.Path, Bytes: mutated, Present: true, Regular: true}})
	if err != nil || inspect.Installed {
		t.Fatal("modified timeout was installed")
	}
	request.Action = agentapi.HookRemove
	request.File.Bytes = after
	changes, err = h.Plan(request)
	if err != nil || len(changes) != 1 || !bytes.Contains(changes[0].After, []byte(foreign)) {
		t.Fatalf("removal lost foreign handler: %v %+v", err, changes)
	}
	for _, raw := range []string{`{}`, `{"schema_version":null}`, `{"schema_version":"1"}`, `{"schema_version":2}`, `{"schema_version":1,"schema_version":1}`, `{"SCHEMA_VERSION":1}`, `{"schema_version":1,"SCHEMA_VERSION":1}`} {
		request.Action = agentapi.HookInstall
		request.File.Bytes = []byte(raw)
		if _, err := h.Plan(request); err == nil {
			t.Fatalf("ambiguous existing schema accepted: %s", raw)
		}
	}
	request.File.Present = false
	request.File.Bytes = nil
	changes, err = h.Plan(request)
	var generated struct {
		Schema int `json:"schema_version"`
	}
	if err == nil && len(changes) == 1 {
		err = json.Unmarshal(changes[0].After, &generated)
	}
	if err != nil || len(changes) != 1 || generated.Schema != 1 {
		t.Fatalf("new schema missing: %v %+v", err, changes)
	}
	if path := h.Location(agentapi.HookLocations{UserHome: "/synthetic/home", Environment: map[string]string{"XDG_CONFIG_HOME": "/custom/config"}}); path != "/custom/config/muse/settings.json" {
		t.Fatal(path)
	}
	if path := h.Location(agentapi.HookLocations{UserHome: "/synthetic/home", WorkingDirectory: "/wrong", Environment: map[string]string{"XDG_CONFIG_HOME": "relative"}}); path != filepath.FromSlash("/synthetic/home/.config/muse/settings.json") {
		t.Fatal(path)
	}
}

func FuzzRootHookDecoder(f *testing.F) {
	f.Add([]byte(`{"hook_event_name":"SessionStart","session_id":"opaque","cwd":"/work","transcript_path":null,"source":"startup"}`), "opaque", "opaque")
	f.Fuzz(func(t *testing.T, raw []byte, session, thread string) {
		if len(raw) > 16<<10 || len(session) > 256 || len(thread) > 256 {
			return
		}
		var payload map[string]any
		if json.Unmarshal(raw, &payload) != nil {
			return
		}
		events, err := Decoder().Decode(t.Context(), agentapi.HookInput{Payload: payload, Environment: map[string]string{"MUSE_SESSION_ID": session, "MUSE_THREAD_ID": thread}})
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Start.Kind != agentapi.FreshUnknown || event.Source.Path != "" || len(event.Evidence) != 0 || !hookNativeID(event.Session.NativeID) {
				t.Fatal("decoder manufactured capture authority")
			}
		}
	})
}
