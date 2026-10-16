package muse

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/discoveryio"
)

// NativeHeaders supplies bounded corroboration for local handoff only.
// The selected source still requires complete strict whole-file validation.
type NativeHeaders struct{}

// InspectHeader recognizes the supported exec or MSP startup prefix.
// Scan normalizes records, so these facts never certify physical file framing.
func (NativeHeaders) InspectHeader(req agentapi.NativeHeaderRequest) (agentapi.NativeHeader, error) {
	if req.Purpose != agentapi.DiscoveryHandoff || req.Scan == nil {
		return agentapi.NativeHeader{}, errFormat
	}
	rows := make([]retained, 0, 8)
	var decodeErr error
	complete := false
	scanErr := req.Scan(func(raw []byte) bool {
		if len(rows) == 0 {
			rows, decodeErr = frame(raw, 1)
		} else {
			var r retained
			r, decodeErr = envelope(raw, position{Line: int64(len(rows))})
			if decodeErr == nil {
				rows = append(rows, r)
			}
		}
		if decodeErr != nil {
			return false
		}
		needed := 4
		if len(rows) >= 3 && rows[2].Type == payloadRuntimeSessionCaptureSelection {
			needed = 8
		}
		if len(rows) < needed {
			return true
		}
		decodeErr = validateRootRows(rows)
		complete = decodeErr == nil
		return false
	})
	if err := errors.Join(scanErr, decodeErr); err != nil {
		return agentapi.NativeHeader{}, err
	}
	if !complete {
		return agentapi.NativeHeader{}, errFormat
	}
	owner := rows[0].Stream.ID
	if filepath.Base(req.Path) != "session.jsonl" || filepath.Base(filepath.Dir(req.Path)) != owner {
		return agentapi.NativeHeader{}, errFormat
	}
	workspace := ""
	for _, r := range rows {
		if r.Type == payloadRuntimeSessionMetadata {
			workspace = str(r.Fields["workspace_root"])
		}
	}
	return agentapi.NativeHeader{NativeID: owner, Directory: workspace, StartedAt: time.UnixMicro(rows[0].Timestamp).UTC()}, nil
}

// Roots declares only the one effective current data home's ordinary root layout.
func (NativeHeaders) Roots(loc agentapi.NativeLocations, purpose agentapi.DiscoveryPurpose) []agentapi.NativeStoreRoot {
	if purpose != agentapi.DiscoveryHandoff || !filepath.IsAbs(loc.DataHome) || !utf8.ValidString(loc.DataHome) || strings.ContainsRune(loc.DataHome, 0) {
		return nil
	}
	return []agentapi.NativeStoreRoot{{Harness: "muse", Path: filepath.Join(loc.DataHome, "muse", "sessions"), Depth: 4, Prefix: "session.jsonl", Suffix: "session.jsonl"}}
}

// DefaultDirectories has no configuration-directory fallback: Muse uses the
// caller's single qualified effective data home instead.
func (NativeHeaders) DefaultDirectories(string) []string { return nil }

// Discover emits exact ordinary root locators; it never grants capture authority.
func (p NativeHeaders) Discover(ctx context.Context, req agentapi.DiscoveryRequest, emit func(agentapi.DiscoveryCandidate) error) (agentapi.DiscoveryReport, error) {
	if req.Purpose != agentapi.DiscoveryHandoff || req.Stage != agentapi.DiscoveryReferences || emit == nil {
		return agentapi.DiscoveryReport{}, errFormat
	}
	roots := req.Roots
	if roots == nil {
		roots = p.Roots(req.Locations, req.Purpose)
	}
	return discoveryio.Files(ctx, req, agentmeta.Muse, roots, p, false, func(c agentapi.DiscoveryCandidate) error {
		if !ordinaryRootLocator(c.Root, c.Source.Path) {
			return nil
		}
		return emit(c)
	})
}

func ordinaryRootLocator(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) != 5 || parts[4] != "session.jsonl" {
		return false
	}
	if _, ok := uuid(parts[3]); !ok {
		return false
	}
	_, err = time.Parse("2006/01/02", strings.Join(parts[:3], "/"))
	return err == nil
}
