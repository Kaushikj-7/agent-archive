package agentapi

import "time"

// ApplicationDiscovery distinguishes installed observations from capture verification.
type ApplicationDiscovery struct {
	Installed     bool      `json:"installed"`
	Version       string    `json:"version,omitempty"`
	VersionSource string    `json:"version_source,omitempty"`
	VersionKind   string    `json:"version_kind,omitempty"`
	VersionState  string    `json:"version_state"`
	ObservedAt    time.Time `json:"observed_at"`
}

// VersionProbeKind restricts host execution to current supported probe operations.
type VersionProbeKind uint8

// VersionCLI, VersionBundle and VersionMuseCLI select fixed native observations.
const (
	VersionCLI VersionProbeKind = iota + 1
	VersionBundle
	// VersionMuseCLI disables Muse launcher automatic updates for --version.
	VersionMuseCLI
)

// VersionProbe supplies only a resolved executable or plist, never arbitrary argv.
type VersionProbe struct {
	Kind VersionProbeKind
	Path string
}

// VersionDirectory supplies read-only directory facts.
type VersionDirectory struct {
	Name      string
	Directory bool
}

// VersionHost is a narrow injected observation port; integrations spawn no processes.
type VersionHost interface {
	Exists(string) (present, directory bool)
	ResolveExecutable(string) (string, bool)
	Directories(string) []VersionDirectory
	ProbeVersion(VersionProbe) (string, bool)
}

// VersionEnvironment supplies purpose-specific host observations and locations.
type VersionEnvironment struct {
	UserHome string
	MacOS    bool
	Host     VersionHost
}

// VersionInspector owns native candidate locations and observation interpretation.
type VersionInspector interface {
	ObserveVersion(VersionEnvironment) ApplicationDiscovery
}

// VersionsLookup projects only implemented installed-version inspection ports.
type VersionsLookup interface {
	LookupVersionInspector(string) (VersionInspector, bool)
	VersionAgents() []string
}
