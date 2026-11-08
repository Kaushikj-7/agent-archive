package hookconfig

import (
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/filechange"
	"path/filepath"
)

// Location interprets native environment roots without reading the filesystem.
func (c Configurator) Location(r agentapi.HookLocations) string {
	root := filepath.Join(r.UserHome, c.Spec.DefaultDirectory)
	if v := r.Environment[c.Spec.RootEnvironment]; c.Spec.RootEnvironment != "" && v != "" {
		root = v
		if !filepath.IsAbs(root) {
			root = filepath.Join(r.WorkingDirectory, root)
		}
	}
	return filepath.Join(root, c.Spec.Filename)
}

// Plan computes changes solely from the injected file and ownership observations.
func (c Configurator) Plan(r agentapi.HookPlanRequest) ([]filechange.Change, error) {
	if r.File.ReadError != nil {
		return nil, fmt.Errorf("cannot read %s: %w", r.File.Path, r.File.ReadError)
	}
	if !filepath.IsAbs(r.File.Path) {
		return nil, errors.New("hook configuration path must be absolute")
	}
	var after []byte
	var err error
	removed := false
	switch r.Action {
	case agentapi.HookInstall:
		after, err = merge(r.File.Bytes, c.Spec, Hook(r.Owner))
	case agentapi.HookRemove:
		if !r.File.Present {
			return nil, nil
		}
		after, removed, err = remove(r.File.Bytes, c.Spec, Hook(r.Owner))
		if err == nil && !removed {
			return nil, nil
		}
	default:
		return nil, errors.New("invalid hook plan action")
	}
	if err != nil {
		return nil, err
	}
	mode := r.File.Mode
	if !r.File.Present {
		mode = 0600
	}
	return []filechange.Change{{Path: r.File.Path, Before: append([]byte(nil), r.File.Bytes...), After: after, Existed: r.File.Present, Mode: mode, Delete: removed && Empty(after) && r.File.Regular}}, nil
}

// Inspect reports explicit settings state and native owner references without I/O.
func (c Configurator) Inspect(r agentapi.HookInspectionRequest) (agentapi.HookInspection, error) {
	if r.File.ReadError != nil {
		return agentapi.HookInspection{State: agentapi.HookUnreadable, Reason: "settings_unreadable"}, r.File.ReadError
	}
	if !r.File.Present {
		return agentapi.HookInspection{State: agentapi.HookAbsent, Reason: "settings_absent"}, nil
	}
	inspectionOwner := r.Owner
	if inspectionOwner.Executable == "" {
		inspectionOwner.Executable = string(filepath.Separator)
	}
	healthy, err := installed(r.File.Bytes, c.Spec, Hook(inspectionOwner))
	if err != nil {
		return agentapi.HookInspection{State: agentapi.HookUnreadable, Reason: "settings_invalid"}, err
	}
	others, locations, err := otherInstallations(r.File.Bytes, c.Spec, Hook(r.Owner))
	if err != nil {
		return agentapi.HookInspection{State: agentapi.HookUnreadable, Reason: "settings_invalid"}, err
	}
	_, owned, err := remove(r.File.Bytes, c.Spec, Hook(r.Owner))
	if err != nil {
		return agentapi.HookInspection{State: agentapi.HookUnreadable, Reason: "settings_invalid"}, err
	}
	state := agentapi.HookAbsent
	reason := "hooks_absent"
	if owned {
		state = agentapi.HookOwned
		reason = "hooks_stale"
	}
	if healthy {
		reason = "hooks_installed"
	}
	if len(others) > 0 {
		state = agentapi.HookForeign
		reason = "foreign_installation"
	}
	return agentapi.HookInspection{Owned: owned, State: state, Installed: healthy, Reason: reason, Others: others, OwnershipLocations: locations}, nil
}

// Command builds the owned command with the integration's declared native name.
func (c Configurator) Command(owner agentapi.HookOwner) (string, error) {
	return Hook(owner).command(c.Spec)
}

// CommandForEvent builds the owned command for an event with the integration's declared native name.
func (c Configurator) CommandForEvent(owner agentapi.HookOwner, event string) (string, error) {
	return Hook(owner).commandForEvent(c.Spec, event)
}

// EnvironmentKeys declares native root overrides to the observation caller.
func (c Configurator) EnvironmentKeys() []string {
	if c.Spec.RootEnvironment == "" {
		return nil
	}
	return []string{c.Spec.RootEnvironment}
}
