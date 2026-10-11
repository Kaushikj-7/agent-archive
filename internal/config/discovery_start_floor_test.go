package config

import (
	"testing"
	"time"
)

func TestDiscoveryStartFloor(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 5, 21, 43, 0, 0, time.UTC)
	destination := Config{}.DestinationID()
	project := Config{Discovery: &DiscoveryConfig{Enabled: true, Authorizations: []DiscoveryAuthorization{
		{Agent: "codex", ProjectRoot: "/later", NativeStartFloor: at.Add(time.Hour), DestinationID: destination},
		{Agent: "codex", ProjectRoot: "/legacy", Intervals: []DiscoveryInterval{{Start: at}}, DestinationID: destination},
		{Agent: "codex", ProjectRoot: "/elsewhere", NativeStartFloor: at.Add(-time.Hour), DestinationID: "another-destination"},
		{Agent: "claude", ProjectRoot: "/other-app", NativeStartFloor: at.Add(-2 * time.Hour), DestinationID: destination},
		{Agent: "codex", ProjectRoot: "/unknown", DestinationID: destination},
	}}}
	if floor, ok := project.DiscoveryStartFloor("codex"); !ok || !floor.Equal(at) {
		t.Fatalf("included-projects floor = %v %v, want %v", floor, ok, at)
	}
	off := project
	off.Discovery = &DiscoveryConfig{Enabled: false, Authorizations: project.Discovery.Authorizations}
	if floor, ok := off.DiscoveryStartFloor("codex"); ok {
		t.Fatalf("discovery off has floor %v", floor)
	}
	if floor, ok := (Config{}).DiscoveryStartFloor("codex"); ok {
		t.Fatalf("no discovery has floor %v", floor)
	}
	if floor, ok := project.DiscoveryStartFloor("cursor"); ok {
		t.Fatalf("an app without authorizations has floor %v", floor)
	}

	all := Config{Discovery: &DiscoveryConfig{Enabled: true}, CodexCapture: &CodexCaptureConfig{Scope: CodexAllProjects,
		Authorization:       &DiscoveryAuthorization{Agent: "codex", NativeStartFloor: at.Add(-time.Hour), DestinationID: destination},
		SourceAuthorization: &DiscoveryAuthorization{Agent: "codex", NativeStartFloor: at, DestinationID: destination},
	}}
	if floor, ok := all.DiscoveryStartFloor("codex"); !ok || !floor.Equal(at) {
		t.Fatalf("all-projects floor = %v %v, want the later of capture and source", floor, ok)
	}
	capture := *all.CodexCapture
	capture.Authorization = &DiscoveryAuthorization{Agent: "codex", NativeStartFloor: at.Add(time.Hour), DestinationID: destination}
	all.CodexCapture = &capture
	if floor, ok := all.DiscoveryStartFloor("codex"); !ok || !floor.Equal(at.Add(time.Hour)) {
		t.Fatalf("all-projects floor = %v %v, want the capture floor", floor, ok)
	}
	for name, change := range map[string]func(*CodexCaptureConfig){
		"no source":  func(c *CodexCaptureConfig) { c.SourceAuthorization = nil },
		"no capture": func(c *CodexCaptureConfig) { c.Authorization = nil },
		"unknown floor": func(c *CodexCaptureConfig) {
			c.SourceAuthorization = &DiscoveryAuthorization{DestinationID: destination}
		},
		"another destination": func(c *CodexCaptureConfig) {
			c.SourceAuthorization = &DiscoveryAuthorization{NativeStartFloor: at, DestinationID: "another"}
		},
		"capture destination": func(c *CodexCaptureConfig) {
			c.Authorization = &DiscoveryAuthorization{NativeStartFloor: at, DestinationID: "another"}
		},
		"capture floor": func(c *CodexCaptureConfig) { c.Authorization = &DiscoveryAuthorization{DestinationID: destination} },
	} {
		changed := capture
		change(&changed)
		cfg := all
		cfg.CodexCapture = &changed
		if floor, ok := cfg.DiscoveryStartFloor("codex"); ok {
			t.Errorf("%s: floor %v", name, floor)
		}
	}
}
