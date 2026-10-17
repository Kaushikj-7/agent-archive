package muse

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-archive/internal/filechange"
	"github.com/wangjohn/agent-archive/internal/jsonedit"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/hookconfig"
)

const rootHookContract = "muse-root-hooks-v1"

type hookConfigurator struct{ hookconfig.Configurator }

func (h hookConfigurator) Location(r agentapi.HookLocations) string {
	root := r.Environment["XDG_CONFIG_HOME"]
	if !filepath.IsAbs(root) {
		root = filepath.Join(r.UserHome, ".config")
	}
	return filepath.Join(root, "muse", "settings.json")
}

func (hookConfigurator) EnvironmentKeys() []string { return []string{"XDG_CONFIG_HOME"} }

// Hooks plans the four qualified native lifecycle handlers.
func Hooks() agentapi.HookConfigurator {
	return hookConfigurator{hookconfig.Configurator{Spec: hookconfig.Spec{SchemaVersionKey: "schema_version", CommandMarkerOwnership: true, PreserveForeignBytes: true, Name: "muse", Owner: "agent-archive lifecycle capture", Events: []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"}}}}
}

type hookDecoder struct{}

// Decoder interprets qualified root observations without inferring freshness.
func Decoder() agentapi.HookDecoder { return hookDecoder{} }

func (hookDecoder) NativeHookContract() agentapi.NativeHookContract {
	return agentapi.NativeHookContract{Contract: rootHookContract, EnvironmentKeys: []string{"MUSE_SESSION_ID", "MUSE_THREAD_ID"}, MaxEnvironmentValueBytes: 128}
}

func (d hookDecoder) DiagnosticProject(input agentapi.HookInput) string {
	events, _ := d.Decode(context.Background(), input)
	if len(events) == 1 {
		return events[0].ProjectRoot
	}
	return ""
}

func (hookDecoder) Decode(ctx context.Context, input agentapi.HookInput) ([]agentapi.LifecycleEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := input.Payload
	id, idOK := p["session_id"].(string)
	cwd, cwdOK := p["cwd"].(string)
	name, nameOK := p["hook_event_name"].(string)
	path, pathPresent := p["transcript_path"]
	if !idOK || !hookNativeID(id) || (agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: id}).Validate() != nil || !cwdOK || strings.ContainsAny(cwd, "\x00\r\n") || len(cwd) > 4096 || !utf8.ValidString(cwd) || !filepath.IsAbs(cwd) || !nameOK || !pathPresent || path != nil || input.Environment["MUSE_SESSION_ID"] != id || input.Environment["MUSE_THREAD_ID"] != id {
		return nil, nil
	}
	event := agentapi.LifecycleEvent{Session: agentapi.NativeSession{Agent: agentmeta.Muse, NativeID: id}, ProjectRoot: cwd, NativeEvent: name, NativeHookContract: rootHookContract, Reason: "native_root_observation"}
	switch name {
	case "SessionStart":
		if source, ok := p["source"].(string); !ok || source != "startup" {
			return nil, nil
		}
		if _, present := p["reason"]; present {
			return nil, nil
		}
		event.Kind = agentapi.EventStart
		event.Start = agentapi.StartEvidence{Kind: agentapi.FreshUnknown, Reason: agentapi.FreshnessUnknownSource}
	case "UserPromptSubmit", "Stop", "SessionEnd":
		if _, present := p["source"]; present {
			return nil, nil
		}
		if name == "SessionEnd" {
			if _, ok := p["reason"].(string); !ok {
				return nil, nil
			}
		} else if _, present := p["reason"]; present {
			return nil, nil
		}
		if name == "UserPromptSubmit" {
			event.Kind = agentapi.EventTurnStart
		} else {
			event.Kind = agentapi.EventStop
		}
	default:
		return nil, nil
	}
	return []agentapi.LifecycleEvent{event}, nil
}

func hookNativeID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		ch := id[i]
		alnum := ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9'
		if !alnum && (i == 0 || ch != '_' && ch != '-') {
			return false
		}
	}
	return true
}

func (h hookConfigurator) Command(owner agentapi.HookOwner) (string, error) {
	if owner.DataHome == "" {
		owner.DataHome = owner.DefaultDataHome
	}
	if owner.DataHome == "" {
		return "", errors.New("Muse hooks require an explicit archive home")
	}
	return h.Configurator.Command(owner)
}

func museHookSettings(raw []byte, present bool) error {
	doc, err := jsonedit.Parse(raw, "hooks", "schema_version")
	if err != nil {
		return err
	}
	for _, member := range doc.Root.Members {
		if strings.EqualFold(member.Key, "schema_version") && member.Key != "schema_version" {
			return errors.New("ambiguous Muse settings schema")
		}
	}
	value, found := doc.Root.Get("schema_version")
	if present && !found {
		return errors.New("existing Muse settings require schema_version 1")
	}
	if found {
		number, ok := value.(json.Number)
		if !ok || number != "1" {
			return errors.New("unsupported Muse settings schema")
		}
	}
	return nil
}

func (h hookConfigurator) Plan(r agentapi.HookPlanRequest) ([]filechange.Change, error) {
	if r.File.ReadError != nil {
		return nil, r.File.ReadError
	}
	if err := museHookSettings(r.File.Bytes, r.File.Present); err != nil {
		return nil, err
	}
	if r.Owner.DataHome == "" {
		r.Owner.DataHome = r.Owner.DefaultDataHome
	}
	return h.Configurator.Plan(r)
}

func (h hookConfigurator) Inspect(r agentapi.HookInspectionRequest) (agentapi.HookInspection, error) {
	if r.File.Present {
		if err := museHookSettings(r.File.Bytes, true); err != nil {
			return agentapi.HookInspection{State: agentapi.HookUnreadable, Reason: "settings_invalid"}, err
		}
	}
	if r.Owner.DataHome == "" {
		r.Owner.DataHome = r.Owner.DefaultDataHome
	}
	return h.Configurator.Inspect(r)
}
