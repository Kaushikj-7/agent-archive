package archive

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// HistoricalProjectProof binds a confirmed import to an opaque project and
// destination. OriginalCwd is private admission evidence, never public metadata.
type HistoricalProjectProof struct {
	Version       int      `json:"version"`
	ProjectID     string   `json:"project_id"`
	DestinationID string   `json:"destination_id"`
	OriginalCwd   string   `json:"original_cwd"`
	CanonicalCwd  string   `json:"canonical_cwd"`
	Label         string   `json:"label"`
	SourceCwds    []string `json:"source_cwds,omitempty"`
}

// IsHistoricalProjectID recognizes the opaque random 128-bit project identity.
func IsHistoricalProjectID(id string) bool {
	if len(id) != 32 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// ValidateHistoricalProject rejects import-only proof used as live authority.
func (r SessionRegistration) ValidateHistoricalProject() error {
	h := r.HistoricalProject
	if h == nil {
		return errors.New("historical project proof is required")
	}
	if h.Version != 1 || !IsHistoricalProjectID(h.ProjectID) || h.ProjectID != r.ProjectID || h.DestinationID == "" || h.DestinationID != r.DestinationID || !r.Imported() || r.ImportBatch.IsZero() || r.ProjectRoot != "" || r.CodexAdmission != nil || r.ProjectResolution != nil || r.CaptureFrozen || r.PreviousGenerationID != "" {
		return errors.New("invalid historical import ownership proof")
	}
	if len(h.SourceCwds) > 2*MaxHistorySpans {
		return errors.New("historical source ownership evidence exceeds limit")
	}
	for _, cwd := range append([]string{h.OriginalCwd, h.CanonicalCwd}, h.SourceCwds...) {
		if !validHistoricalCwd(cwd) {
			return errors.New("invalid historical original cwd evidence")
		}
	}
	if !ValidHistoricalProjectLabel(h.Label) {
		return errors.New("historical project label must be bounded plain text")
	}
	return nil
}

// ValidHistoricalProjectLabel accepts a human display label independent of paths.
func ValidHistoricalProjectLabel(label string) bool {
	if !utf8.ValidString(label) || strings.TrimSpace(label) != label || label == "" || len(label) > 128 {
		return false
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Equal compares every immutable private import proof field.
func (p *HistoricalProjectProof) Equal(other *HistoricalProjectProof) bool {
	if p == nil || other == nil {
		return p == nil && other == nil
	}
	return p.Version == other.Version && p.ProjectID == other.ProjectID && p.DestinationID == other.DestinationID && p.OriginalCwd == other.OriginalCwd && p.CanonicalCwd == other.CanonicalCwd && p.Label == other.Label && slices.Equal(p.SourceCwds, other.SourceCwds)
}

func (r SessionRegistration) validateProjectOwnership() error {
	if r.HistoricalProject != nil {
		return r.ValidateHistoricalProject()
	}
	if strings.TrimSpace(r.ProjectID) == "" || strings.TrimSpace(r.ProjectRoot) == "" {
		return errors.New("project ID and project root are required")
	}
	return nil
}

// RetainedOnly reports an explicit frozen generation or historical import snapshot.
func (r SessionRegistration) RetainedOnly() bool {
	return r.CaptureFrozen || r.HistoricalProject != nil
}

func validHistoricalCwd(cwd string) bool {
	if !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd || len(cwd) > 4096 || !utf8.ValidString(cwd) {
		return false
	}
	for _, r := range cwd {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
