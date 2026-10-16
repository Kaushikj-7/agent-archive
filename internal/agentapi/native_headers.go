package agentapi

import (
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"time"
)

// DiscoveryPurpose separates compatibility import headers from verified native selection.
type DiscoveryPurpose uint8

// DiscoveryImport, DiscoveryHandoff and DiscoveryProjects keep each header purpose and its limits separate.
const (
	DiscoveryImport DiscoveryPurpose = iota + 1
	DiscoveryHandoff
	DiscoveryProjects
	// DiscoveryBoundedProjects inspects only the first record for interactive repository matching.
	DiscoveryBoundedProjects
)

// NativeHeader contains only inspected native identity and checkout evidence.
type NativeHeader struct {
	// CodexIdentity records native relationships independently of capture permission.
	CodexIdentity    *codexmeta.CodexIdentity
	NativeID         string
	Directory        string
	RepoKey          string // normalized recorded repository identity; never a remote URL
	StartedAt        time.Time
	IdentityMismatch bool
	SubagentOnly     bool
	// CapturePending names understood native history that still requires a complete reader.
	CapturePending string
}

// NativeHeaderRequest supplies a caller-owned bounded scan; codecs cannot open paths.
type NativeHeaderRequest struct {
	Purpose DiscoveryPurpose
	Path    string
	Scan    func(func([]byte) bool) error
}

// NativeStoreRoot declares native traversal layout for one purpose.
type NativeStoreRoot struct {
	Priority   int
	Historical bool
	Harness    string
	Path       string
	Recursive  bool
	Depth      int
	Prefix     string
	Suffix     string
}

// NativeLocations supplies qualified data locations and observed configuration directories.
type NativeLocations struct {
	UserHome string
	// DataHome is the caller-qualified absolute effective XDG data base, without
	// an agent suffix. Empty means unavailable; providers must not infer a fallback.
	// It supplies local read locations only, never capture permission.
	DataHome    string
	Directories []string
}

// NativeHeaderInspector interprets native headers without host operations.
type NativeHeaderInspector interface {
	Roots(NativeLocations, DiscoveryPurpose) []NativeStoreRoot
	InspectHeader(NativeHeaderRequest) (NativeHeader, error)
}

// NativeHeadersLookup projects native header inspection to read-only consumers.
type NativeHeadersLookup interface {
	LookupNativeHeaders(string) (NativeHeaderInspector, bool)
	NativeHeaderAgents() []string
}
