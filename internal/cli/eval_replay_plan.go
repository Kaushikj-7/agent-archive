package cli

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/filechange"
)

const replayPlanWireVersion = 1

const replayPlanByteLimit = 64 << 10

// These are private machine-wire values, not persisted archive metadata. Keep
// their fields closed instead of exposing future filechange.Change additions.
type replayPlanObservation struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Bytes   []byte `json:"bytes"`
	Mode    uint32 `json:"mode"`
	Regular bool   `json:"regular"`
}

type replayPlanControl struct {
	LaunchID string          `json:"launch_id"`
	Replay   *archive.Replay `json:"replay"`
}

type replayPlanRequest struct {
	Version            int                   `json:"version"`
	Executable         string                `json:"executable"`
	DataHome           string                `json:"data_home"`
	DefaultDataHome    string                `json:"default_data_home"`
	OwnershipLocations map[string]string     `json:"ownership_locations"`
	PrivateDirectory   string                `json:"private_directory"`
	Control            replayPlanControl     `json:"control"`
	Settings           replayPlanObservation `json:"settings"`
	Managed            replayPlanObservation `json:"managed"`
}

type replayPlanChange struct {
	Path    string `json:"Path"`
	Before  []byte `json:"Before"`
	After   []byte `json:"After"`
	Existed bool   `json:"Existed"`
	Mode    uint32 `json:"Mode"`
	Delete  bool   `json:"Delete"`
}

type replayPlanResponse struct {
	Version          int                   `json:"version"`
	Qualified        bool                  `json:"qualified"`
	PreparationOnly  bool                  `json:"preparation_only"`
	LaunchID         string                `json:"launch_id"`
	Replay           *archive.Replay       `json:"replay"`
	ObservedSettings replayPlanObservation `json:"observed_settings"`
	ObservedManaged  replayPlanObservation `json:"observed_managed"`
	OwnerPresent     bool                  `json:"owner_present"`
	Install          []replayPlanChange    `json:"install_with_foreign"`
	OwnedInstall     []replayPlanChange    `json:"owned_install"`
	Removal          []replayPlanChange    `json:"owned_removal"`
	Command          string                `json:"command"`
	Runner           []byte                `json:"runner_bytes"`
	RunnerPath       string                `json:"runner_path"`
	NativeCommand    string                `json:"native_command"`
}

func runEvalReplayPlan(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	flags := env.newCommandFlags("eval replay-plan", stderr)
	harness := flags.String("harness", "", "native planner (muse)")
	if !flags.parseFlagsOnly(args) {
		return 2
	}
	refuse := func(code string) int {
		_, _ = io.WriteString(stderr, "agent-archive: eval replay-plan: "+code+"\n")
		return 1
	}
	if agentmeta.ID(*harness) != agentmeta.Muse || flags.NArg() != 0 {
		return refuse("unsupported_harness")
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, replayPlanByteLimit+1))
	if err != nil || len(raw) > replayPlanByteLimit {
		return refuse("request_limit")
	}
	request, err := decodeReplayPlanRequest(raw)
	if err != nil {
		return refuse("request_invalid")
	}
	response, err := prepareReplayPlan(request, env.agentRegistry())
	if err != nil {
		return refuse("plan_unavailable")
	}
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded)+1 > replayPlanByteLimit {
		return refuse("response_limit")
	}
	encoded = append(encoded, '\n')
	if written, writeErr := stdout.Write(encoded); writeErr != nil || written != len(encoded) {
		return refuse("output_failed")
	}
	return 0
}

func replayPlanObject(value any, names ...string) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok || len(object) != len(names) {
		return nil, errors.New("closed replay object required")
	}
	for _, name := range names {
		if _, found := object[name]; !found {
			return nil, errors.New("unknown replay field")
		}
	}
	return object, nil
}

// replayPlanScalarEscapes rejects the unpaired UTF-16 escapes that encoding/json
// otherwise replaces with U+FFFD. The normal strict decoder still owns JSON
// syntax, duplicate keys, depth, and UTF-8 validation.
func replayPlanScalarEscapes(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || raw[i] != '\\' {
			continue
		}
		i++ // An escaped backslash or quote cannot begin another escape.
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		unit, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if unit >= 0xdc00 && unit <= 0xdfff {
			return false
		}
		if unit < 0xd800 || unit > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}

func decodeReplayPlanRequest(raw []byte) (replayPlanRequest, error) {
	var request replayPlanRequest
	if !replayPlanScalarEscapes(raw) {
		return request, errors.New("invalid JSON scalar escape")
	}
	object, err := strictHookPayload(raw)
	if err != nil {
		return request, err
	}
	if _, err = replayPlanObject(object, "version", "executable", "data_home", "default_data_home", "ownership_locations", "private_directory", "control", "settings", "managed"); err != nil {
		return request, err
	}
	if err := validateReplayPlanObjectShape(object); err != nil {
		return request, err
	}
	if err = json.Unmarshal(raw, &request); err != nil {
		return request, err
	}
	if err := validateDecodedReplayPlanRequest(request); err != nil {
		return request, err
	}
	return request, nil
}

func validateReplayPlanObjectShape(object map[string]any) error {
	locations, ok := object["ownership_locations"].(map[string]any)
	if !ok || len(locations) > 32 {
		return errors.New("bounded ownership observations required")
	}
	for path, value := range locations {
		canonical, ok := value.(string)
		if !ok || !replayPlanPath(path) || !replayPlanPath(canonical) {
			return errors.New("invalid ownership observation")
		}
	}
	for _, name := range []string{"executable", "data_home", "default_data_home", "private_directory"} {
		if _, ok := object[name].(string); !ok {
			return errors.New("explicit path string required")
		}
	}
	control, err := replayPlanObject(object["control"], "launch_id", "replay")
	if err != nil {
		return err
	}
	replay, ok := control["replay"].(map[string]any)
	if !ok || len(replay) > 1 {
		return errors.New("normalized replay required")
	}
	if len(replay) == 1 {
		id, ok := replay["run_id"].(string)
		if !ok || !archive.IsReplayRunID(id) {
			return errors.New("invalid replay identifier")
		}
	}
	return validateReplayPlanObservationShapes(object)
}

func validateReplayPlanObservationShapes(object map[string]any) error {
	for _, name := range []string{"settings", "managed"} {
		observation, err := replayPlanObject(object[name], "path", "present", "bytes", "mode", "regular")
		if err != nil {
			return err
		}
		if _, ok := observation["bytes"].(string); !ok {
			return errors.New("encoded observation required")
		}
		if _, ok := observation["path"].(string); !ok {
			return errors.New("observed path string required")
		}
		if _, ok := observation["present"].(bool); !ok {
			return errors.New("observed presence boolean required")
		}
		if _, ok := observation["regular"].(bool); !ok {
			return errors.New("observed type boolean required")
		}
		if _, ok := observation["mode"].(float64); !ok {
			return errors.New("observed numeric mode required")
		}
	}
	return nil
}

func validateDecodedReplayPlanRequest(request replayPlanRequest) error {
	for _, canonical := range request.OwnershipLocations {
		if other, found := request.OwnershipLocations[canonical]; found && other != canonical {
			return errors.New("ambiguous canonical ownership")
		}
	}
	if request.DefaultDataHome != "" && !replayPlanPath(request.DefaultDataHome) {
		return errors.New("invalid default archive home")
	}
	if request.Version != replayPlanWireVersion || !archive.IsReplayRunID(request.Control.LaunchID) || request.Control.Replay == nil {
		return errors.New("unsupported replay control")
	}
	if !replayPlanPath(request.Executable) || !replayPlanPath(request.DataHome) || !replayPlanPath(request.PrivateDirectory) || !replayPlanPath(request.Settings.Path) {
		return errors.New("canonical absolute paths required")
	}
	for _, observation := range []replayPlanObservation{request.Settings, request.Managed} {
		if observation.Mode > 0777 || observation.Present && (!observation.Regular || !replayPlanPath(observation.Path)) {
			return errors.New("regular observed file required")
		}
		if !observation.Present && (len(observation.Bytes) != 0 || observation.Mode != 0 || observation.Regular) {
			return errors.New("absent observation has content")
		}
		if observation.Present {
			if !replayPlanScalarEscapes(observation.Bytes) {
				return errors.New("invalid observed JSON scalar escape")
			}
			if _, err := strictHookPayload(observation.Bytes); err != nil {
				return err
			}
		}
	}
	return nil
}

func replayPlanPath(path string) bool {
	return len(path) > 0 && len(path) <= 4096 && utf8.ValidString(path) && !strings.ContainsAny(path, "\x00\r\n") && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func replayPlanWireChanges(changes []filechange.Change) []replayPlanChange {
	result := make([]replayPlanChange, 0, len(changes))
	for _, change := range changes {
		result = append(result, replayPlanChange{Path: change.Path, Before: append([]byte{}, change.Before...), After: append([]byte{}, change.After...), Existed: change.Existed, Mode: uint32(change.Mode.Perm()), Delete: change.Delete})
	}
	return result
}

func prepareReplayPlan(request replayPlanRequest, lookup agentapi.HooksLookup) (replayPlanResponse, error) {
	var response replayPlanResponse
	port, ok := lookup.LookupHooks(string(agentmeta.Muse))
	if !ok {
		return response, errors.New("muse hooks unavailable")
	}
	owner := agentapi.HookOwner{Executable: request.Executable, DataHome: request.DataHome, DefaultDataHome: request.DefaultDataHome}
	observed := agentapi.HookFile{Path: request.Settings.Path, Bytes: request.Settings.Bytes, Present: request.Settings.Present, Mode: fs.FileMode(request.Settings.Mode), Regular: request.Settings.Regular}
	// Inspection and removal operate on the ACTUAL observation. A hypothetical
	// install is never evidence that this owner is present in the user's file.
	inspection, err := port.Inspect(agentapi.HookInspectionRequest{File: observed, Owner: owner})
	if err != nil {
		return response, err
	}
	references := append([]string{owner.DataHome, owner.DefaultDataHome}, inspection.OwnershipLocations...)
	for _, reference := range references {
		if reference != "" {
			if _, found := request.OwnershipLocations[reference]; !found {
				return response, errors.New("missing ownership observation")
			}
		}
	}
	for _, other := range inspection.Others {
		if other.Command != "" || other.Default && owner.DefaultDataHome == "" {
			return response, errors.New("unresolved ownership observation")
		}
	}
	owner.Locations = request.OwnershipLocations
	inspection, err = port.Inspect(agentapi.HookInspectionRequest{File: observed, Owner: owner})
	if err != nil {
		return response, err
	}
	removal, err := port.Plan(agentapi.HookPlanRequest{Action: agentapi.HookRemove, File: observed, Owner: owner})
	if err != nil {
		return response, err
	}
	install, err := port.Plan(agentapi.HookPlanRequest{Action: agentapi.HookInstall, File: observed, Owner: owner})
	if err != nil {
		return response, err
	}
	owned, err := port.Plan(agentapi.HookPlanRequest{Action: agentapi.HookInstall, File: agentapi.HookFile{Path: observed.Path}, Owner: owner})
	if err != nil || len(owned) != 1 || len(install) != 1 || len(removal) > 1 || inspection.Owned != (len(removal) == 1) {
		return response, errors.New("unsupported planner result")
	}
	commandPort, ok := port.(interface {
		Command(agentapi.HookOwner) (string, error)
	})
	if !ok {
		return response, errors.New("generated command unavailable")
	}
	command, err := commandPort.Command(owner)
	if err != nil || len(command) > 4096 {
		return response, errors.New("generated command exceeds bound")
	}
	if err := validateReplayPlanManagedSource(request, observed); err != nil {
		return response, err
	}
	marker := request.Control.Replay.RunID
	if marker == "" {
		marker = "invalid replay marker"
	}
	runnerPath := filepath.Join(request.PrivateDirectory, "hook-runner-"+request.Control.LaunchID)
	if !replayPlanPath(runnerPath) {
		return response, errors.New("runner path exceeds bound")
	}
	nativeCommand := shellQuote(runnerPath)
	if len(nativeCommand) > 4096 {
		return response, errors.New("quoted runner command exceeds bound")
	}
	runner := []byte("#!/bin/sh\nexport " + archive.ReplayEnv + "=" + shellQuote(marker) + "\nexec /bin/sh -c " + shellQuote(command) + "\n")
	response = replayPlanResponse{Version: replayPlanWireVersion, PreparationOnly: true, LaunchID: request.Control.LaunchID, Replay: request.Control.Replay, ObservedSettings: request.Settings, ObservedManaged: request.Managed, OwnerPresent: inspection.Owned, Install: replayPlanWireChanges(install), OwnedInstall: replayPlanWireChanges(owned), Removal: replayPlanWireChanges(removal), Command: command, Runner: runner, RunnerPath: runnerPath, NativeCommand: nativeCommand}
	return response, nil
}

func validateReplayPlanManagedSource(request replayPlanRequest, observed agentapi.HookFile) error {
	// Preserve only a supplied, existing absolute managed source. Native relative
	// managed-source resolution is deliberately unavailable before qualification.
	settings := map[string]any{}
	if observed.Present {
		var err error
		settings, err = strictHookPayload(observed.Bytes)
		if err != nil {
			return err
		}
	}
	managedPath, present := settings["managed_hooks_path"]
	if present {
		path, ok := managedPath.(string)
		if !ok || !replayPlanPath(path) || !request.Managed.Present || request.Managed.Path != path {
			return errors.New("managed source observation required")
		}
	} else if request.Managed.Present || request.Managed.Path != "" {
		return errors.New("unreferenced managed observation")
	}
	return nil
}
