package muse

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func FuzzMuseHookSettings(f *testing.F) {
	f.Add([]byte(`{"schema_version":1,"hooks":{}}`))
	f.Add([]byte(`{"schema_version":2,"hooks":{}}`))
	f.Add([]byte(`{"schema_version":1,"SCHEMA_VERSION":1}`))
	f.Add([]byte(`{"raw":"\u003cforeign\u003e"}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 8<<10 {
			return
		}
		exerciseMuseHookSettings(t, raw, nil)
		// A qualified shell unrelated to this installation must keep its exact raw
		// bytes even when its opaque settings contain fuzzed JSON structures/escapes.
		if json.Valid(raw) {
			foreign := append([]byte(`{ "matcher" : "*", "hooks" : [{ "type" : "command", "command" : "echo foreign", "timeout" : 7, "opaque" : `), raw...)
			foreign = append(foreign, []byte(` }] }`)...)
			document := append([]byte(`{"schema_version":1,"hooks":{"Stop":[`), foreign...)
			document = append(document, []byte(`]}}`)...)
			exerciseMuseHookSettings(t, document, foreign)
		}
	})
}

func exerciseMuseHookSettings(t *testing.T, input, foreign []byte) {
	t.Helper()
	hooks := Hooks()
	owner := agentapi.HookOwner{Executable: "/synthetic/bin/archive", DefaultDataHome: "/synthetic/data"}
	original := bytes.Clone(input)
	file := agentapi.HookFile{Path: "/synthetic/settings.json", Present: len(input) != 0, Bytes: input, Regular: true, Mode: 0600}
	request := agentapi.HookPlanRequest{Action: agentapi.HookInstall, Owner: owner, File: file}
	changes, err := hooks.Plan(request)
	if !bytes.Equal(input, original) {
		t.Fatal("plan mutated caller settings")
	}
	if err != nil {
		return
	}
	if len(changes) > 1 {
		t.Fatal("plan expanded file scope")
	}
	if len(changes) == 1 {
		file.Bytes, file.Present = changes[0].After, true
	}
	inspection, err := hooks.Inspect(agentapi.HookInspectionRequest{Owner: owner, File: file})
	if err != nil || !inspection.Installed {
		t.Fatal("successful plan is not installed", inspection, err)
	}
	if len(foreign) != 0 && !bytes.Contains(file.Bytes, foreign) {
		t.Fatal("install changed foreign handler bytes")
	}
	request.File = file
	repeated, err := hooks.Plan(request)
	if err != nil || len(repeated) > 1 || len(repeated) == 1 && !bytes.Equal(repeated[0].After, file.Bytes) {
		t.Fatal("installation is not byte-idempotent", err)
	}
	request.Action = agentapi.HookRemove
	removed, err := hooks.Plan(request)
	if err != nil || len(removed) > 1 {
		t.Fatal("removal scope", err)
	}
	if len(removed) == 1 {
		file.Bytes = removed[0].After
	}
	inspection, err = hooks.Inspect(agentapi.HookInspectionRequest{Owner: owner, File: file})
	if err != nil || inspection.Owned || inspection.Installed {
		t.Fatal("removal retained own handlers", inspection, err)
	}
	if len(foreign) != 0 && !bytes.Contains(file.Bytes, foreign) {
		t.Fatal("removal changed foreign handler bytes")
	}
	if !bytes.Equal(input, original) {
		t.Fatal("inspection/removal mutated caller settings")
	}
}
