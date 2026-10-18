package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func replayPlannerFixture(t *testing.T) replayPlanRequest {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "archive λ's $(not-executed)")
	return replayPlanRequest{Version: 1, Executable: filepath.Join(root, "bin λ's", "archive"), DataHome: home, OwnershipLocations: map[string]string{home: home}, PrivateDirectory: filepath.Join(root, "launch-A"), Control: replayPlanControl{LaunchID: "launch-A", Replay: &archive.Replay{RunID: "run-A"}}, Settings: replayPlanObservation{Path: filepath.Join(root, "config", "settings.json"), Bytes: []byte{}}, Managed: replayPlanObservation{Bytes: []byte{}}}
}

func replayPlannerNoHost(t *testing.T) Env {
	t.Helper()
	return Env{UserHomeDir: func() (string, error) { t.Fatal("pure planner discovered HOME"); return "", errors.New("forbidden") }, Executable: func() (string, error) {
		t.Fatal("pure planner discovered executable")
		return "", errors.New("forbidden")
	}, LookupEnv: func(string) (string, bool) { t.Fatal("pure planner discovered environment"); return "", false }, OpenStore: func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("pure planner opened storage")
		return nil, errors.New("forbidden")
	}}
}

func callReplayPlanner(t *testing.T, request replayPlanRequest) (replayPlanResponse, []byte) {
	t.Helper()
	raw, err := json.Marshal(request)
	must(t, err)
	var out, diagnostic bytes.Buffer
	if code := runEvalCommand([]string{"replay-plan", "--harness", "muse"}, bytes.NewReader(raw), &out, &diagnostic, replayPlannerNoHost(t)); code != 0 {
		t.Fatalf("planner failed %d: %s", code, diagnostic.String())
	}
	if diagnostic.Len() != 0 {
		t.Fatal("successful planner emitted diagnostics")
	}
	var response replayPlanResponse
	must(t, json.Unmarshal(out.Bytes(), &response))
	if response.Version != 1 || response.Qualified || !response.PreparationOnly {
		t.Fatal("planner advertised a native capability")
	}
	return response, out.Bytes()
}

func TestEvalReplayPlanUsesActualPlannerWithoutHostEffects(t *testing.T) {
	request := replayPlannerFixture(t)
	response, raw := callReplayPlanner(t, request)
	if len(raw) > replayPlanByteLimit || response.OwnerPresent || len(response.Removal) != 0 {
		t.Fatal("absent owner was fabricated")
	}
	if response.ObservedSettings.Present || !bytes.Equal(response.ObservedSettings.Bytes, request.Settings.Bytes) {
		t.Fatal("observed absent settings changed")
	}
	port, _ := builtin.NewBuiltins().LookupHooks("muse")
	generator := port.(interface {
		Command(agentapi.HookOwner) (string, error)
	})
	command, err := generator.Command(agentapi.HookOwner{Executable: request.Executable, DataHome: request.DataHome})
	must(t, err)
	if response.Command != command || !bytes.Contains(response.Runner, []byte(shellQuote(command))) {
		t.Fatal("runner bypassed generated production command")
	}
	if response.NativeCommand != shellQuote(response.RunnerPath) || response.RunnerPath != filepath.Join(request.PrivateDirectory, "hook-runner-launch-A") {
		t.Fatal("runner escaped launch binding")
	}
	if !bytes.Contains(response.Runner, []byte("export "+archive.ReplayEnv+"="+shellQuote("run-A"))) {
		t.Fatal("normalized launch marker missing")
	}
	if len(response.OwnedInstall) != 1 || len(response.Install) != 1 || len(response.OwnedInstall[0].After) == 0 {
		t.Fatal("actual install plans missing")
	}
}

func TestEvalReplayPlanPreservesActualForeignAndOwnedObservations(t *testing.T) {
	request := replayPlannerFixture(t)
	managedPath := filepath.Join(filepath.Dir(request.Settings.Path), "foreign-managed.json")
	foreign := []byte("{\n  \"schema_version\":1, \"hooks\":{\"SessionStart\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"/foreign/user handler λ\",\"timeout\":3}]}]},\n  \"managed_hooks_path\":" + strconvQuote(managedPath) + ",\"managed_hooks_env_vars\":[\"FOREIGN_KEY\"],\"mcp_servers\":{\"sentinel\":{\"command\":\"keep\"}}\n}\n")
	request.Settings.Present = true
	request.Settings.Regular = true
	request.Settings.Mode = 0600
	request.Settings.Bytes = foreign
	request.Managed = replayPlanObservation{Path: managedPath, Present: true, Regular: true, Mode: 0600, Bytes: []byte(`{"hooks":{},"foreign":{"keep":true}}`)}
	absent, _ := callReplayPlanner(t, request)
	if absent.OwnerPresent || len(absent.Removal) != 0 || !bytes.Equal(absent.ObservedSettings.Bytes, foreign) {
		t.Fatal("foreign-only observation became installed")
	}
	port, _ := builtin.NewBuiltins().LookupHooks("muse")
	install, err := port.Plan(agentapi.HookPlanRequest{Action: agentapi.HookInstall, File: agentapi.HookFile{Path: request.Settings.Path, Bytes: foreign, Present: true, Mode: 0600, Regular: true}, Owner: agentapi.HookOwner{Executable: request.Executable, DataHome: request.DataHome}})
	must(t, err)
	request.Settings.Bytes = install[0].After
	actual := append([]byte(nil), request.Settings.Bytes...)
	owned, _ := callReplayPlanner(t, request)
	if !owned.OwnerPresent || len(owned.Removal) != 1 || !bytes.Equal(owned.Removal[0].Before, actual) || !bytes.Equal(owned.ObservedSettings.Bytes, actual) {
		t.Fatal("removal was not bound to actual installed observation")
	}
	if !bytes.Equal(owned.ObservedManaged.Bytes, request.Managed.Bytes) || !bytes.Contains(owned.Removal[0].After, []byte("FOREIGN_KEY")) || !bytes.Contains(owned.Removal[0].After, []byte("mcp_servers")) {
		t.Fatal("foreign observation was lost")
	}
	oldHome := request.DataHome
	request.DataHome = filepath.Join(filepath.Dir(oldHome), "different-owner")
	request.OwnershipLocations[request.DataHome] = request.DataHome
	changed, _ := callReplayPlanner(t, request)
	if changed.OwnerPresent || len(changed.Removal) != 0 {
		t.Fatal("different installation was suppressed")
	}
	if !bytes.Equal(request.Settings.Bytes, actual) {
		t.Fatal("pure planning mutated caller observation")
	}
}

func strconvQuote(value string) string { raw, _ := json.Marshal(value); return string(raw) }

func TestEvalReplayPlanAnonymousAndObservedAliases(t *testing.T) {
	request := replayPlannerFixture(t)
	request.Control.Replay = &archive.Replay{}
	anonymous, _ := callReplayPlanner(t, request)
	if anonymous.Replay == nil || anonymous.Replay.RunID != "" || !bytes.Contains(anonymous.Runner, []byte("invalid replay marker")) {
		t.Fatal("anonymous marker changed to ordinary or raw input")
	}
	alias := filepath.Join(filepath.Dir(request.DataHome), "observed-alias")
	port, _ := builtin.NewBuiltins().LookupHooks("muse")
	install, err := port.Plan(agentapi.HookPlanRequest{Action: agentapi.HookInstall, File: agentapi.HookFile{Path: request.Settings.Path}, Owner: agentapi.HookOwner{Executable: request.Executable, DataHome: alias}})
	must(t, err)
	request.Settings = replayPlanObservation{Path: request.Settings.Path, Present: true, Regular: true, Mode: 0600, Bytes: install[0].After}
	request.OwnershipLocations[alias] = request.DataHome
	response, _ := callReplayPlanner(t, request)
	if !response.OwnerPresent || len(response.Removal) != 1 {
		t.Fatal("existing classifier ignored observed canonical equality")
	}
	delete(request.OwnershipLocations, alias)
	raw, err := json.Marshal(request)
	must(t, err)
	var out, diagnostic bytes.Buffer
	if code := runEvalCommand([]string{"replay-plan", "--harness", "muse"}, bytes.NewReader(raw), &out, &diagnostic, replayPlannerNoHost(t)); code == 0 || out.Len() != 0 {
		t.Fatal("unresolved alias was treated as foreign")
	}
}

func TestEvalReplayPlanRejectsClosedWireAndAggregateLimits(t *testing.T) {
	request := replayPlannerFixture(t)
	raw, err := json.Marshal(request)
	must(t, err)
	cases := [][]byte{append(append([]byte(nil), raw...), []byte(" {}")...),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"Version":1`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(raw, []byte(`"replay":{"run_id":"run-A"}`), []byte(`"replay":null`), 1),
		bytes.Replace(raw, []byte(`"run_id":"run-A"`), []byte(`"run_id":"run-A","run_id":"run-B"`), 1),
		bytes.Replace(raw, []byte(`"run_id":"run-A"`), []byte(`"run_id":"raw marker text"`), 1),
		bytes.Replace(raw, []byte(`"run_id":"run-A"`), []byte(`"Run_ID":"run-A"`), 1),
		bytes.Repeat([]byte(" "), replayPlanByteLimit+1)}
	for index, input := range cases {
		var out, diagnostic bytes.Buffer
		code := runEvalCommand([]string{"replay-plan", "--harness", "muse"}, bytes.NewReader(input), &out, &diagnostic, replayPlannerNoHost(t))
		if code == 0 || out.Len() != 0 || bytes.Contains(diagnostic.Bytes(), []byte("raw marker text")) {
			t.Fatalf("unsafe closed-wire case%d", index)
		}
	}
	request.Settings = replayPlanObservation{Path: request.Settings.Path, Present: true, Regular: true, Mode: 0600, Bytes: []byte(`{"schema_version":1,"foreign":"` + strings.Repeat("x", 22000) + `"}`)}
	raw, err = json.Marshal(request)
	must(t, err)
	if len(raw) > replayPlanByteLimit {
		t.Fatal("response limit fixture already exceeds request limit")
	}
	var out, diagnostic bytes.Buffer
	if code := runEvalCommand([]string{"replay-plan", "--harness", "muse"}, bytes.NewReader(raw), &out, &diagnostic, replayPlannerNoHost(t)); code == 0 || out.Len() != 0 || !strings.Contains(diagnostic.String(), "response_limit") {
		t.Fatal("aggregate response limit not enforced before stdout")
	}
}

func TestEvalReplayPlanRefusesUnsafeObservationsWithoutPartialOutput(t *testing.T) {
	mutations := []func(*replayPlanRequest){
		func(r *replayPlanRequest) { r.Executable = filepath.Join("relative", "archive") },
		func(r *replayPlanRequest) {
			r.PrivateDirectory = filepath.Dir(r.PrivateDirectory) + string(filepath.Separator) + ".." + string(filepath.Separator) + "escape"
		},
		func(r *replayPlanRequest) { r.DataHome += "\ninvalid" },
		func(r *replayPlanRequest) { r.Control.Replay = &archive.Replay{RunID: strings.Repeat("a", 129)} },
		func(r *replayPlanRequest) {
			r.Settings.Present = true
			r.Settings.Regular = false
			r.Settings.Bytes = []byte(`{"schema_version":1}`)
		},
		func(r *replayPlanRequest) {
			r.Settings.Present = true
			r.Settings.Regular = true
			r.Settings.Bytes = []byte(`{"schema_version":1,"schema_version":1}`)
		},
		func(r *replayPlanRequest) {
			r.Settings.Present = true
			r.Settings.Regular = true
			r.Settings.Bytes = []byte(`{"schema_version":2}`)
		},
		func(r *replayPlanRequest) { delete(r.OwnershipLocations, r.DataHome) },
		func(r *replayPlanRequest) {
			for i := range 33 {
				path := filepath.Join(filepath.Dir(r.DataHome), strings.Repeat("x", i+1))
				r.OwnershipLocations[path] = path
			}
		},
		func(r *replayPlanRequest) {
			r.Settings.Present = true
			r.Settings.Regular = true
			r.Settings.Bytes = []byte(`{"schema_version":1,"managed_hooks_path":"relative.json"}`)
		},
		func(r *replayPlanRequest) {
			r.Settings.Present = true
			r.Settings.Regular = true
			r.Settings.Bytes = []byte(`{"schema_version":1,"managed_hooks_path":` + strconvQuote(filepath.Join(filepath.Dir(r.DataHome), "missing.json")) + `}`)
		},
	}
	for index, mutate := range mutations {
		request := replayPlannerFixture(t)
		mutate(&request)
		raw, err := json.Marshal(request)
		must(t, err)
		var out, diagnostic bytes.Buffer
		if code := runEvalCommand([]string{"replay-plan", "--harness", "muse"}, bytes.NewReader(raw), &out, &diagnostic, replayPlannerNoHost(t)); code == 0 || out.Len() != 0 {
			t.Fatalf("unsafe observation case%d", index)
		}
	}
}

func FuzzEvalReplayPlanRequest(f *testing.F) {
	request := replayPlanRequest{Version: 1, Executable: "/synthetic/archive", DataHome: "/synthetic/data", OwnershipLocations: map[string]string{"/synthetic/data": "/synthetic/data"}, PrivateDirectory: "/synthetic/launch", Control: replayPlanControl{LaunchID: "launch-A", Replay: &archive.Replay{RunID: "run-A"}}, Settings: replayPlanObservation{Path: "/synthetic/settings.json", Bytes: []byte{}}, Managed: replayPlanObservation{Bytes: []byte{}}}
	seed, err := json.Marshal(request)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(`{"version":1,"version":2}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > replayPlanByteLimit {
			return
		}
		decoded, err := decodeReplayPlanRequest(raw)
		if err != nil {
			return
		}
		if decoded.Version != 1 || decoded.Control.Replay == nil || !archive.IsReplayRunID(decoded.Control.LaunchID) || decoded.Control.Replay.RunID != "" && !archive.IsReplayRunID(decoded.Control.Replay.RunID) {
			t.Fatal("accepted invalid normalized control")
		}
		canonical, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal("accepted request cannot be encoded")
		}
		if _, err = decodeReplayPlanRequest(canonical); err != nil {
			t.Fatal("accepted request cannot round trip")
		}
	})
}

type replayScalarField string

const (
	replayScalarExecutable       replayScalarField = "executable"
	replayScalarDataHome         replayScalarField = "data_home"
	replayScalarDefaultDataHome  replayScalarField = "default_data_home"
	replayScalarPrivateDirectory replayScalarField = "private_directory"
	replayScalarSettings         replayScalarField = "settings"
	replayScalarManaged          replayScalarField = "managed"
	replayScalarMapKey           replayScalarField = "map-key"
	replayScalarMapValue         replayScalarField = "map-value"
)

// The direct CLI accepts raw JSON independently of Python's UTF-8 encoder.
func TestEvalReplayPlanRejectsLossyScalarEscapes(t *testing.T) {
	for _, field := range []replayScalarField{replayScalarExecutable, replayScalarDataHome, replayScalarDefaultDataHome, replayScalarPrivateDirectory, replayScalarSettings, replayScalarManaged, replayScalarMapKey, replayScalarMapValue} {
		for _, escape := range []string{`\ud800`, `\udc00`, `\ud800x`, `\ud800\u0041`, `\ud800\ud800`} {
			t.Run(string(field)+"/"+escape, func(t *testing.T) {
				request := replayPlannerFixture(t)
				raw, err := json.Marshal(request)
				must(t, err)
				var object map[string]any
				must(t, json.Unmarshal(raw, &object))
				switch field {
				case replayScalarSettings, replayScalarManaged:
					object[string(field)].(map[string]any)["path"] = "/SCALAR"
				case replayScalarMapKey:
					object["ownership_locations"].(map[string]any)["/SCALAR"] = request.DataHome
				case replayScalarMapValue:
					object["ownership_locations"].(map[string]any)[request.DataHome] = "/SCALAR"
				default:
					object[string(field)] = "/SCALAR"
				}
				raw, err = json.Marshal(object)
				must(t, err)
				raw = bytes.ReplaceAll(raw, []byte("SCALAR"), []byte(escape))
				var out, diagnostic bytes.Buffer
				code := runEvalCommand([]string{"replay-plan", "--harness", "muse"}, bytes.NewReader(raw), &out, &diagnostic, replayPlannerNoHost(t))
				if code == 0 || out.Len() != 0 || !strings.Contains(diagnostic.String(), "request_invalid") {
					t.Fatalf("lossy scalar accepted: code=%d output=%d diagnostic=%s", code, out.Len(), diagnostic.String())
				}
			})
		}
	}
	for _, value := range []struct {
		encoded string
		decoded string
	}{
		{`\ufffd`, "�"}, {"�", "�"}, {`\ud83d\ude80`, "🚀"}, {`\\ud800`, `\ud800`},
	} {
		request := replayPlannerFixture(t)
		request.Executable = "/SCALAR"
		raw, err := json.Marshal(request)
		must(t, err)
		raw = bytes.ReplaceAll(raw, []byte("SCALAR"), []byte(value.encoded))
		decoded, err := decodeReplayPlanRequest(raw)
		must(t, err)
		if decoded.Executable != "/"+value.decoded {
			t.Fatal("valid scalar or escaped literal changed")
		}
		var out, diagnostic bytes.Buffer
		if runEvalCommand([]string{"replay-plan", "--harness", "muse"}, bytes.NewReader(raw), &out, &diagnostic, replayPlannerNoHost(t)) != 0 {
			t.Fatalf("valid scalar refused: %s", diagnostic.String())
		}
	}
}

func TestEvalReplayPlanBoundsQuotedRunnerCommand(t *testing.T) {
	request := replayPlannerFixture(t)
	// Many short legal components keep the raw path small while quotes expand.
	request.PrivateDirectory = "/private" + strings.Repeat("/a'", 700)
	if !replayPlanPath(request.PrivateDirectory) || len(shellQuote(filepath.Join(request.PrivateDirectory, "hook-runner-launch-A"))) <= 4096 {
		t.Fatal("fixture does not cross quoted command limit")
	}
	raw, err := json.Marshal(request)
	must(t, err)
	var out, diagnostic bytes.Buffer
	if runEvalCommand([]string{"replay-plan", "--harness", "muse"}, bytes.NewReader(raw), &out, &diagnostic, replayPlannerNoHost(t)) == 0 || out.Len() != 0 {
		t.Fatal("producer succeeded with an unconsumable quoted command")
	}
	// Exercise the greatest accepted quote-heavy path, without touching it.
	for n := 700; n > 0; n-- {
		request.PrivateDirectory = "/private" + strings.Repeat("/a'", n)
		if len(shellQuote(filepath.Join(request.PrivateDirectory, "hook-runner-launch-A"))) <= 4096 {
			response, _ := callReplayPlanner(t, request)
			if len(response.NativeCommand) > 4096 || len(response.NativeCommand) < 4089 {
				t.Fatal("accepted quote boundary changed")
			}
			return
		}
	}
	t.Fatal("no accepted quote boundary")
}
