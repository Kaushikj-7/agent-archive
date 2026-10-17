package hookconfig

import (
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"slices"
)

func installed(data []byte, spec Spec, hook Hook) (bool, error) {
	command, err := hook.command(spec)
	if err != nil {
		return false, err
	}
	doc, err := parseDocument(data)
	if err != nil {
		return false, err
	}
	app := spec
	if app.SchemaVersionKey != "" {
		value, _ := doc.Root.Get(app.SchemaVersionKey)
		if !isOne(value) {
			return false, nil
		}
	}
	if app.Version {
		if v, _ := doc.Root.Get("version"); !isOne(v) {
			return false, nil
		}
	}
	hs, err := hooksObject(doc.Root)
	if err != nil || hs == nil {
		return false, err
	}
	names := spec.Events
	for _, event := range names {
		if _, ok := hs.Get(event); !ok {
			return false, nil
		}
	}
	for _, m := range hs.Members {
		groups, ok := m.Value.([]any)
		if !ok {
			return false, fmt.Errorf("invalid hook list for %s", m.Key)
		}
		handlers, err := handlerList(groups, app)
		if err != nil {
			return false, err
		}
		ours := 0
		for _, handler := range handlers {
			if kind, _, _ := classify(handler, app, hook); !hook.replaces(kind) {
				continue
			}
			got, _ := handler.Get("command")
			kind, _ := handler.Get("type")
			timeout, timeoutOK := handler.Get("timeout")
			timeoutNumber, numberOK := timeout.(json.Number)
			if got != command || (!app.Flat && kind != "command") || app.CommandMarkerOwnership && (!timeoutOK || !numberOK || timeoutNumber != "2") {
				return false, nil
			}
			ours++
		}
		want := 0
		if slices.Contains(names, m.Key) {
			want = 1
		}
		if ours != want {
			return false, nil
		}
	}
	return true, nil
}

func otherInstallations(data []byte, spec Spec, hook Hook) ([]agentapi.HookOtherOwner, []string, error) {
	doc, err := parseDocument(data)
	if err != nil {
		return nil, nil, err
	}
	hs, err := hooksObject(doc.Root)
	if err != nil || hs == nil {
		return nil, nil, err
	}
	app := spec
	var others []agentapi.HookOtherOwner
	var locations []string
	for _, m := range hs.Members {
		groups, ok := m.Value.([]any)
		if !ok {
			return nil, nil, fmt.Errorf("invalid hook list for %s", m.Key)
		}
		handlers, err := handlerList(groups, app)
		if err != nil {
			return nil, nil, err
		}
		for _, handler := range handlers {
			kind, dataHome, unreadable := classify(handler, app, hook)
			if unreadable == "" && (kind == kindOwn || kind == kindOther) && dataHome != "" && !slices.Contains(locations, dataHome) {
				locations = append(locations, dataHome)
			}
			if kind != kindOther {
				continue
			}
			other := agentapi.HookOtherOwner{DataHome: dataHome, Command: unreadable}
			if other.Command == "" && other.DataHome == "" {
				other.DataHome, other.Default = hook.DefaultDataHome, true
			}
			if !slices.Contains(others, other) {
				others = append(others, other)
			}
		}
	}
	return others, locations, nil
}
