package hookconfig

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

func TestPurePlansAndInspectionUseOnlyInjectedObservations(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"claude", "codex", "cursor"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			port := Configurator{Spec: fuzzSpec(name)}
			// Deliberately absent host paths: Plan and Inspect must use these bytes.
			file := agentapi.HookFile{Path: filepath.Join(t.TempDir(), "never-created", "settings"), Present: true, Bytes: []byte(`{"untouched":{"n":9007199254740993,"html":"<&"}}`), Mode: 0640, Regular: true}
			prior := append([]byte(nil), file.Bytes...)
			owner := agentapi.HookOwner{Executable: "/synthetic/bin/archive", DataHome: "/synthetic/home", DefaultDataHome: "/default", Locations: map[string]string{"/synthetic/home": "owned", "/alias": "owned"}}
			request := agentapi.HookPlanRequest{Action: agentapi.HookInstall, File: file, Owner: owner}
			first, err := port.Plan(request)
			if err != nil {
				t.Fatal(err)
			}
			again, err := port.Plan(request)
			if err != nil || !reflect.DeepEqual(first, again) || !bytes.Equal(file.Bytes, prior) {
				t.Fatalf("plan impure %v %v", again, err)
			}
			file.Bytes = first[0].After
			inspection, err := port.Inspect(agentapi.HookInspectionRequest{File: file, Owner: owner})
			if err != nil || inspection.State != agentapi.HookOwned || !inspection.Installed {
				t.Fatalf("inspection %+v %v", inspection, err)
			}
			foreign := owner
			foreign.DataHome = "/other"
			inspection, err = port.Inspect(agentapi.HookInspectionRequest{File: file, Owner: foreign})
			if err != nil || inspection.State != agentapi.HookForeign || len(inspection.Others) != 1 {
				t.Fatalf("foreign ownership %+v %v", inspection, err)
			}
			removal, err := port.Plan(agentapi.HookPlanRequest{Action: agentapi.HookRemove, File: file, Owner: foreign})
			if err != nil || len(removal) != 0 {
				t.Fatalf("foreign removal %+v %v", removal, err)
			}
			file.ReadError = errors.New("injected read refusal")
			inspection, err = port.Inspect(agentapi.HookInspectionRequest{File: file, Owner: owner})
			if err == nil || inspection.State != agentapi.HookUnreadable {
				t.Fatalf("read state %+v %v", inspection, err)
			}
			if changes, err := port.Plan(agentapi.HookPlanRequest{Action: agentapi.HookInstall, File: file, Owner: owner}); err == nil || len(changes) != 0 {
				t.Fatalf("unreadable overwrite %+v %v", changes, err)
			}
		})
	}
}

func TestOwnershipAliasUsesProvidedIdentityWithoutHostProbe(t *testing.T) {
	t.Parallel()
	port := Configurator{Spec: fuzzSpec("claude")}
	foreign := agentapi.HookOwner{Executable: "/bin/archive", DataHome: "/absent/alias", DefaultDataHome: "/absent/default"}
	installed, err := port.Plan(agentapi.HookPlanRequest{Action: agentapi.HookInstall, File: agentapi.HookFile{Path: "/absent/config"}, Owner: foreign})
	if err != nil {
		t.Fatal(err)
	}
	file := agentapi.HookFile{Path: "/absent/config", Present: true, Bytes: installed[0].After}
	own := agentapi.HookOwner{Executable: "/bin/archive", DataHome: "/absent/target", DefaultDataHome: "/absent/default", Locations: map[string]string{"/absent/target": "one-file", "/absent/alias": "one-file"}}
	inspection, err := port.Inspect(agentapi.HookInspectionRequest{File: file, Owner: own})
	if err != nil || inspection.State != agentapi.HookOwned || len(inspection.Others) != 0 {
		t.Fatalf("injected identity not honored %+v %v", inspection, err)
	}
}

func FuzzDecoderNativeShapes(f *testing.F) {
	for _, value := range []string{"startup", "clear", "resume", "compact", "unknown", "  CLEAR  "} {
		f.Add(value, "/native/source", 0)
	}
	f.Fuzz(func(t *testing.T, source, path string, shape int) {
		if len(source)+len(path) > 1<<20 {
			t.Skip()
		}
		payload := map[string]any{"hook_event_name": "sessionStart", "conversation_id": "native", "source": source, "transcript_path": path}
		switch shape % 4 {
		case 1:
			payload["transcript_path"] = nil
		case 2:
			payload["transcript_path"] = map[string]any{"bad": true}
		case 3:
			delete(payload, "transcript_path")
		}
		port := Decoder{Spec: DecoderSpec{Agent: agentmeta.Cursor, Events: map[string]agentapi.EventKind{"sessionStart": agentapi.EventStart}, NullPathFresh: true, OwnedFilename: true}}
		before := payload["transcript_path"]
		first, err := port.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: time.Unix(1, 0)})
		if err != nil {
			t.Fatal(err)
		}
		again, err := port.Decode(context.Background(), agentapi.HookInput{Payload: payload, ObservedAt: time.Unix(1, 0)})
		if err != nil || !reflect.DeepEqual(first, again) || !reflect.DeepEqual(before, payload["transcript_path"]) {
			t.Fatal("decoder mutation or nondeterminism")
		}
	})
}

func TestInspectionReportsUnrelatedSettingsAsAbsent(t *testing.T) {
	t.Parallel()
	port := Configurator{Spec: Spec{Name: "synthetic", Owner: "agent-archive lifecycle capture", Events: []string{"begin"}}}
	in := agentapi.HookInspectionRequest{File: agentapi.HookFile{Path: "/synthetic/settings", Bytes: []byte(`{"unrelated":true}`), Present: true}, Owner: agentapi.HookOwner{Executable: "/synthetic/bin"}}
	result, err := port.Inspect(in)
	if err != nil || result.State != agentapi.HookAbsent || result.Installed {
		t.Fatalf("inspection %+v %v", result, err)
	}
}

func TestEventInCommandGeneratesEventFlag(t *testing.T) {
	spec := Spec{
		Name: "agy", EventInCommand: true, Owner: "test capture",
		Events: []string{"SessionStart", "Stop"}, Direct: true, Rootless: true, Definition: "agent-archive",
	}
	hook := Hook{Executable: "/bin/agent-archive"}
	cmdStart, err := hook.commandForEvent(spec, "SessionStart")
	if err != nil {
		t.Fatal(err)
	}
	want := "'/bin/agent-archive' _hook --harness agy --event SessionStart # test capture"
	if cmdStart != want {
		t.Fatalf("got %q, want %q", cmdStart, want)
	}

	hookRelocated := Hook{Executable: "/bin/agent-archive", DataHome: "/data/archive"}
	cmdStop, err := hookRelocated.commandForEvent(spec, "Stop")
	if err != nil {
		t.Fatal(err)
	}
	wantRelocated := "AGENT_ARCHIVE_HOME='/data/archive' '/bin/agent-archive' _hook --harness agy --event Stop # test capture"
	if cmdStop != wantRelocated {
		t.Fatalf("got %q, want %q", cmdStop, wantRelocated)
	}

	specNoEvent := Spec{
		Name: "codex", EventInCommand: false, Owner: "test capture",
	}
	cmdNoEvent, err := hook.commandForEvent(specNoEvent, "SessionStart")
	if err != nil {
		t.Fatal(err)
	}
	wantNoEvent := "'/bin/agent-archive' _hook --harness codex # test capture"
	if cmdNoEvent != wantNoEvent {
		t.Fatalf("got %q, want %q", cmdNoEvent, wantNoEvent)
	}

	cmdEmptyEvent, err := hook.commandForEvent(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	wantEmptyEvent := "'/bin/agent-archive' _hook --harness agy # test capture"
	if cmdEmptyEvent != wantEmptyEvent {
		t.Fatalf("got %q, want %q", cmdEmptyEvent, wantEmptyEvent)
	}
}

