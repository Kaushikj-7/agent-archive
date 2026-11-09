package agy

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

type mockVersionHost struct {
	executables map[string]string
	probes      map[string]string
}

func (m *mockVersionHost) Exists(path string) (bool, bool) {
	_, ok := m.probes[path]
	return ok, false
}

func (m *mockVersionHost) ResolveExecutable(name string) (string, bool) {
	path, ok := m.executables[name]
	return path, ok
}

func (m *mockVersionHost) Directories(string) []agentapi.VersionDirectory {
	return nil
}

func (m *mockVersionHost) ProbeVersion(probe agentapi.VersionProbe) (string, bool) {
	ver, ok := m.probes[probe.Path]
	return ver, ok
}

func TestVersionInspectorCandidates(t *testing.T) {
	t.Parallel()
	v := VersionInspector{}
	home := filepath.Join(string(filepath.Separator), "home", "user")
	candidates := v.Candidates(agentapi.VersionEnvironment{UserHome: home})
	if len(candidates) < 1 {
		t.Fatalf("expected at least 1 candidate, got %d", len(candidates))
	}
	if candidates[0] != "agy" {
		t.Fatalf("expected agy first, got %v", candidates[0])
	}
	for _, c := range candidates {
		if filepath.Ext(c) == ".exe" {
			t.Fatalf("unexpected .exe in candidates: %s", c)
		}
	}
	expectedHomeCandidate := filepath.Join(home, ".local", "bin", "agy")
	if !slices.Contains(candidates, expectedHomeCandidate) {
		t.Fatalf("expected candidates to contain %s, got %v", expectedHomeCandidate, candidates)
	}
}

func TestVersionInspectorObserveVersion(t *testing.T) {
	t.Parallel()
	v := VersionInspector{}
	host := &mockVersionHost{
		executables: map[string]string{
			"agy": "/usr/local/bin/agy",
		},
		probes: map[string]string{
			"/usr/local/bin/agy": "1.2.3",
		},
	}
	disc := v.ObserveVersion(agentapi.VersionEnvironment{Host: host})
	if !disc.Installed || disc.Version != "1.2.3" || disc.VersionState != "observed" {
		t.Fatalf("unexpected discovery: %+v", disc)
	}
}
