package agy

import (
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/versionprobe"
)

// VersionInspector owns native installed-version candidates for Antigravity.
type VersionInspector struct{}

var _ agentapi.VersionInspector = VersionInspector{}

// Candidates lists native executables in their preference order.
func (p VersionInspector) Candidates(e agentapi.VersionEnvironment) []string {
	paths := []string{
		"agy",
		filepath.Join(e.UserHome, ".local", "bin", "agy"),
		filepath.Join(e.UserHome, ".gemini", "antigravity-cli", "bin", "agy"),
		filepath.Join(e.UserHome, ".gemini", "antigravity", "bin", "agy"),
	}
	return paths
}

// ObserveVersion performs only injected read/probe operations.
func (p VersionInspector) ObserveVersion(e agentapi.VersionEnvironment) agentapi.ApplicationDiscovery {
	return versionprobe.Commands(e.Host, "agy", p.Candidates(e))
}
