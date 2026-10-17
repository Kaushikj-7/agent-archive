package muse

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"strings"
	"testing"
)

func TestVersionSeparatesReleaseAndDisplay(t *testing.T) {
	t.Parallel()
	for _, line := range []string{"Muse Code 1.4.8 (1.4.8-R6256.1)", "Muse Code 1.4.9 (1.4.9-R6297.1)"} {
		facts, ok := ParseVersion(line)
		if !ok || facts.Release != strings.Fields(line)[2] || facts.DisplayBuild == facts.Release {
			t.Fatalf("%q: %+v %v", line, facts, ok)
		}
	}
}

func TestVersionRejectsUnknownOrContradictoryForms(t *testing.T) {
	t.Parallel()
	for _, line := range []string{"", "1.4.9", "Muse Code 1.4.9", "Muse Code 1.4.9 (1.4.8-R6256.1)", "Muse Code 01.4.9 (01.4.9-R6297.1)", "Muse Code 1.4.9 (1.4.9-R0.1)", "Muse Code 1.4.9 (1.4.9-R6297.1) sha=abc", " Muse Code 1.4.9 (1.4.9-R6297.1)", "Muse Code 1.4.9 (1.4.9-R6297.1)\n", strings.Repeat("x", 257), "Muse Code 1.4.9 (1.4.9-R6297.1)\x00"} {
		if facts, ok := ParseVersion(line); ok || facts != (VersionFacts{}) {
			t.Fatalf("accepted %q: %+v", line, facts)
		}
	}
}

func FuzzVersion(f *testing.F) {
	f.Add("Muse Code 1.4.9 (1.4.9-R6297.1)")
	f.Add("Muse Code 1.4.9 (1.4.8-R6297.1)")
	f.Fuzz(func(t *testing.T, line string) {
		facts, ok := ParseVersion(line)
		if ok {
			again, valid := ParseVersion("Muse Code " + facts.Release + " (" + facts.DisplayBuild + ")")
			if !valid || again != facts {
				t.Fatal("noncanonical version")
			}
		}
	})
}

type versionHostFake struct {
	path    string
	output  string
	success bool
	probes  []agentapi.VersionProbe
}

func (*versionHostFake) Exists(string) (bool, bool) { panic("unexpected filesystem probe") }
func (h *versionHostFake) ResolveExecutable(name string) (string, bool) {
	if name != "muse" {
		panic("unexpected executable")
	}
	return h.path, h.path != ""
}
func (*versionHostFake) Directories(string) []agentapi.VersionDirectory {
	panic("unexpected directory scan")
}
func (h *versionHostFake) ProbeVersion(p agentapi.VersionProbe) (string, bool) {
	h.probes = append(h.probes, p)
	return h.output, h.success
}

func TestVersionInspectionUsesOneFixedUpdateDisabledProbe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path, output, state, version string
		success                      bool
	}{
		{"", "", "absent", "", false},
		{"/synthetic/muse", "", "unknown", "", false},
		{"/synthetic/muse", "unexpected", "unknown", "", true},
		{"/synthetic/muse", "Muse Code 1.4.9 (1.4.9-R6297.1)", "observed", "1.4.9", true},
	} {
		h := &versionHostFake{path: tc.path, output: tc.output, success: tc.success}
		got := (VersionInspector{}).ObserveVersion(agentapi.VersionEnvironment{Host: h})
		if got.VersionState != tc.state || got.Version != tc.version || got.Installed != (tc.path != "") {
			t.Fatalf("%+v", got)
		}
		if tc.path == "" {
			if len(h.probes) != 0 {
				t.Fatal("absent executable probed")
			}
			continue
		}
		if len(h.probes) != 1 || h.probes[0] != (agentapi.VersionProbe{Kind: agentapi.VersionMuseCLI, Path: tc.path}) {
			t.Fatalf("probes %+v", h.probes)
		}
	}
}
