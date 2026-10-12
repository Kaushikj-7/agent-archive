package config

import (
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// HistoricalProject has a stable identity and destination-bound import policy.
// It never authorizes hooks, filesystem discovery, or native continuation.
type HistoricalProject struct {
	ID          string                        `json:"id"`
	Label       string                        `json:"label"`
	Permissions []HistoricalProjectPermission `json:"permissions"`
}

// HistoricalProjectPermission permits retained imports in one destination.
type HistoricalProjectPermission struct {
	DestinationID string `json:"destination_id"`
	Enabled       bool   `json:"enabled"`
}

// ValidateHistoricalProjects validates identities and independent permissions.
func (c Config) ValidateHistoricalProjects() error {
	if len(c.HistoricalProjects) > 16384 {
		return errors.New("too many historical projects")
	}
	seen := map[string]bool{}
	for _, p := range c.HistoricalProjects {
		if !archive.IsHistoricalProjectID(p.ID) || !archive.ValidHistoricalProjectLabel(p.Label) || seen[p.ID] || len(p.Permissions) > 128 {
			return errors.New("invalid or duplicate historical project identity, label, or permission")
		}
		seen[p.ID] = true
		destinations := map[string]bool{}
		for _, permission := range p.Permissions {
			_, decodeErr := hex.DecodeString(permission.DestinationID)
			if len(permission.DestinationID) != 64 || decodeErr != nil || strings.ToLower(permission.DestinationID) != permission.DestinationID || destinations[permission.DestinationID] {
				return errors.New("invalid or duplicate historical project destination permission")
			}
			destinations[permission.DestinationID] = true
		}
	}
	return nil
}

// HistoricalProject finds a configured identity without consulting the filesystem.
func (c Config) HistoricalProject(id string) (HistoricalProject, bool) {
	for _, p := range c.HistoricalProjects {
		if p.ID == id {
			return p, true
		}
	}
	return HistoricalProject{}, false
}

// HistoricalPermission returns the recorded decision for an exact destination.
func (p HistoricalProject) HistoricalPermission(destination string) (enabled, found bool) {
	for _, permission := range p.Permissions {
		if permission.DestinationID == destination {
			return permission.Enabled, true
		}
	}
	return false, false
}

// AcceptHistoricalProject accepts retained imports only, with exclusions winning.
func (c Config) AcceptHistoricalProject(r archive.SessionRegistration) bool {
	if !slices.Contains(c.Harnesses, r.Harness.Name) && !slices.Contains(c.ImportedHarnesses, r.Harness.Name) {
		return false
	}
	if r.ValidateHistoricalProject() != nil || !c.HistoricalProjectProtection || !c.Archive.Enabled {
		return false
	}
	p, found := c.HistoricalProject(r.ProjectID)
	enabled, allowed := p.HistoricalPermission(r.DestinationID)
	if !found || !allowed || !enabled {
		return false
	}
	for _, cwd := range append([]string{r.HistoricalProject.OriginalCwd, r.HistoricalProject.CanonicalCwd}, r.HistoricalProject.SourceCwds...) {
		if !c.HistoricalSourceAllowed(cwd, cwd) {
			return false
		}
	}
	return true
}

// ReconcileHistoricalProjects preserves local identities and destination decisions.
// Portable pairing permissions cannot reactivate a local disabled project.
func ReconcileHistoricalProjects(existing, incoming []HistoricalProject) []HistoricalProject {
	out := slices.Clone(existing)
	for _, p := range incoming {
		index := slices.IndexFunc(out, func(v HistoricalProject) bool { return v.ID == p.ID })
		if index < 0 {
			p.Permissions = slices.Clone(p.Permissions)
			out = append(out, p)
			continue
		}
		current := out[index]
		current.Permissions = slices.Clone(current.Permissions)
		for _, permission := range p.Permissions {
			if _, found := current.HistoricalPermission(permission.DestinationID); !found {
				current.Permissions = append(current.Permissions, permission)
			}
		}
		out[index] = current
	}
	return out
}

// HistoricalProjectLabel returns the current display label for retained maintenance.
func (c Config) HistoricalProjectLabel(id string) string {
	p, _ := c.HistoricalProject(id)
	return p.Label
}

// HistoricalSourceAllowed applies explicit original and canonical exclusions.
func (c Config) HistoricalSourceAllowed(original, canonical string) bool {
	for _, rule := range c.Archive.Projects {
		if !rule.Included && (local.PathWithin(original, rule.Root) || local.PathWithin(canonical, rule.Root)) {
			return false
		}
	}
	return true
}
