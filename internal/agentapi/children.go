package agentapi

import "context"

// ChildDiscoveryRequest supplies a qualified parent and read-only native host.
type ChildDiscoveryRequest struct {
	Parent NativeSession
	Source SourceRef
	Files  DiscoveryFiles
}

// ChildCandidate is native child identity and locator evidence before admission.
type ChildCandidate struct {
	ParentNativeID string
	RootNativeID   string
	SubagentID     string
	Role           ChildRole
	Coverage       ChildCoverage
	NativeID       string
	Source         SourceRef
	Bytes          int64
}

// ChildDiscoverer owns native child filename and directory conventions.
type ChildDiscoverer interface {
	DiscoverChildren(context.Context, ChildDiscoveryRequest, func(ChildCandidate) error) (DiscoveryReport, error)
}

// ChildrenLookup projects child discovery only for integrations implementing it.
type ChildrenLookup interface {
	LookupChildren(string) (ChildDiscoverer, bool)
}

// ChildRole describes observed native execution, never inherited consent.
type ChildRole string

const (
	// ChildDelegated describes an independently delegated conversation.
	ChildDelegated ChildRole = "delegated"
	// ChildWorkflow describes an independently identified workflow conversation.
	ChildWorkflow ChildRole = "workflow"
	// ChildObserver describes an independently identified observer conversation.
	ChildObserver ChildRole = "observer"
)

// ChildCoverage holds bounded, versioned completeness facts. Zero retains the
// existing Claude behavior; missing coverage never establishes absence.
type ChildCoverage struct {
	Version  int
	Complete bool
}
