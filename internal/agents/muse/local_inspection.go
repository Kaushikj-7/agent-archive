package muse

import (
	"context"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/discoveryio"
)

// LocalInspection provides purpose-specific historical and local observations.
// Its output is read-only evidence, never permission to capture a session.
type LocalInspection struct{}

// InspectHeader shares strict startup interpretation with native selection.
// Interactive project matching remains unavailable: its first-record contract
// cannot establish the workspace stored in a later startup record.
func (LocalInspection) InspectHeader(r agentapi.NativeHeaderRequest) (agentapi.NativeHeader, error) {
	switch r.Purpose {
	case agentapi.DiscoveryImport, agentapi.DiscoveryProjects, agentapi.DiscoveryHandoff:
		r.Purpose = agentapi.DiscoveryHandoff
		return (NativeHeaders{}).InspectHeader(r)
	case agentapi.DiscoveryBoundedProjects:
		return agentapi.NativeHeader{}, errFormat
	default:
		return agentapi.NativeHeader{}, errFormat
	}
}

// Roots uses the same qualified ordinary store for every supported purpose.
func (LocalInspection) Roots(loc agentapi.NativeLocations, purpose agentapi.DiscoveryPurpose) []agentapi.NativeStoreRoot {
	switch purpose {
	case agentapi.DiscoveryImport, agentapi.DiscoveryProjects, agentapi.DiscoveryHandoff:
		return (NativeHeaders{}).Roots(loc, agentapi.DiscoveryHandoff)
	case agentapi.DiscoveryBoundedProjects:
		return nil
	default:
		return nil
	}
}

// DefaultDirectories does not infer an effective XDG data location.
func (LocalInspection) DefaultDirectories(string) []string { return nil }

// Discover streams ordinary root references or bounded inspected identities.
// Copies are deliberately retained so shared import policy can detect conflicts.
func (p LocalInspection) Discover(ctx context.Context, r agentapi.DiscoveryRequest, emit func(agentapi.DiscoveryCandidate) error) (agentapi.DiscoveryReport, error) {
	if r.Purpose != agentapi.DiscoveryImport && r.Purpose != agentapi.DiscoveryProjects && r.Purpose != agentapi.DiscoveryHandoff || emit == nil {
		return agentapi.DiscoveryReport{}, errFormat
	}
	roots := r.Roots
	if roots == nil {
		roots = p.Roots(r.Locations, r.Purpose)
	}
	return discoveryio.Files(ctx, r, agentmeta.Muse, roots, p, false, func(c agentapi.DiscoveryCandidate) error {
		if !ordinaryRootLocator(c.Root, c.Source.Path) {
			return nil
		}
		return emit(c)
	})
}

// ProjectPaths leaves unsupported desktop and worktree conventions unavailable.
func (LocalInspection) ProjectPaths(agentapi.NativePathEnvironment) agentapi.NativeProjectPaths {
	return agentapi.NativeProjectPaths{}
}

// RecognizesTranscriptPath recognizes only the qualified ordinary root layout.
// It performs no reads and does not certify identity or project ownership.
func (p LocalInspection) RecognizesTranscriptPath(e agentapi.NativePathEnvironment, path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, root := range p.Roots(e.Locations, agentapi.DiscoveryProjects) {
		if ordinaryRootLocator(root.Path, path) {
			return true
		}
	}
	return false
}
