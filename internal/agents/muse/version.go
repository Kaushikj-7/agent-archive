package muse

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"regexp"
	"strings"
)

// VersionFacts separates the release identity from its native display build.
// An executable hash must come from a separate verified binary observation;
// the R-build identifier is not a hash and never supplies one.
type VersionFacts struct {
	Release      string
	DisplayBuild string
}

var versionLine = regexp.MustCompile(`^Muse Code ((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)) \(((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))-R([1-9][0-9]*\.(?:0|[1-9][0-9]*))\)$`)

// ParseVersion accepts only the qualified single-line native --version shape.
// It does not infer an unstamped child version from its parent or the installed
// application. Future or contradictory forms remain unknown.
func ParseVersion(output string) (VersionFacts, bool) {
	if len(output) > 256 || strings.ContainsAny(output, "\x00\r\n") {
		return VersionFacts{}, false
	}
	m := versionLine.FindStringSubmatch(output)
	if m == nil || m[1] != m[2] {
		return VersionFacts{}, false
	}
	return VersionFacts{Release: m[1], DisplayBuild: m[2] + "-R" + m[3]}, true
}

// VersionInspector observes only the injected PATH executable and fixed probe.
type VersionInspector struct{}

// ObserveVersion does not infer session versions from an installed application.
func (VersionInspector) ObserveVersion(e agentapi.VersionEnvironment) agentapi.ApplicationDiscovery {
	if e.Host == nil {
		return agentapi.ApplicationDiscovery{VersionState: "unknown"}
	}
	path, ok := e.Host.ResolveExecutable("muse")
	if !ok {
		return agentapi.ApplicationDiscovery{VersionState: "absent"}
	}
	result := agentapi.ApplicationDiscovery{Installed: true, VersionSource: path + " --version", VersionKind: "cli", VersionState: "unknown"}
	output, ok := e.Host.ProbeVersion(agentapi.VersionProbe{Kind: agentapi.VersionMuseCLI, Path: path})
	if !ok {
		return result
	}
	facts, ok := ParseVersion(output)
	if !ok {
		return result
	}
	result.Version, result.VersionState = facts.Release, "observed"
	return result
}
