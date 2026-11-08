package agy

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/discoveryio"
)

// NativeHeaders supplies purpose-specific Antigravity identity inspection.
type NativeHeaders struct{}

var (
	_ agentapi.NativeHeaderInspector = NativeHeaders{}
	_ agentapi.Discoverer            = NativeHeaders{}
)

// DefaultDirectories declares both known Antigravity data roots.
func (NativeHeaders) DefaultDirectories(home string) []string {
	return []string{
		filepath.Join(home, ".gemini", "antigravity-cli"),
		filepath.Join(home, ".gemini", "antigravity"),
	}
}

// Roots declares Antigravity native store traversal.
func (p NativeHeaders) Roots(l agentapi.NativeLocations, purpose agentapi.DiscoveryPurpose) []agentapi.NativeStoreRoot {
	dirs := l.Directories
	if dirs == nil {
		dirs = p.DefaultDirectories(l.UserHome)
	}
	var out []agentapi.NativeStoreRoot
	for _, dir := range dirs {
		out = append(out, agentapi.NativeStoreRoot{
			Harness:   "agy",
			Path:      filepath.Join(dir, "brain"),
			Depth:     3,
			Recursive: true,
			Suffix:    "transcript.jsonl",
		})
	}
	return out
}

// Discover emits native candidates without archive policy or a second inventory.
func (p NativeHeaders) Discover(ctx context.Context, r agentapi.DiscoveryRequest, emit func(agentapi.DiscoveryCandidate) error) (agentapi.DiscoveryReport, error) {
	roots := r.Roots
	if roots == nil {
		roots = p.Roots(r.Locations, r.Purpose)
	}
	return discoveryio.Files(ctx, r, agentmeta.Agy, roots, p, false, emit)
}

func extractConversationID(p string) string {
	cleaned := filepath.ToSlash(filepath.Clean(p))
	parts := strings.Split(cleaned, "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "brain" && validConversationID(parts[i+1]) {
			return parts[i+1]
		}
	}
	if len(parts) >= 4 && parts[len(parts)-1] == "transcript.jsonl" && parts[len(parts)-2] == "logs" && parts[len(parts)-3] == ".system_generated" {
		if validConversationID(parts[len(parts)-4]) {
			return parts[len(parts)-4]
		}
	}
	for i := len(parts) - 1; i >= 0; i-- {
		if validConversationID(parts[i]) {
			return parts[i]
		}
	}
	return ""
}

func extractDirectory(v map[string]any) string {
	if v == nil {
		return ""
	}
	if s, ok := v["cwd"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	if s, ok := v["workspacePaths"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	if paths, ok := v["workspacePaths"].([]any); ok {
		for _, p := range paths {
			if s, ok := p.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	if paths, ok := v["workspacePaths"].([]string); ok {
		for _, s := range paths {
			if strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	if s, ok := v["workspace_roots"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	if roots, ok := v["workspace_roots"].([]any); ok {
		for _, r := range roots {
			if s, ok := r.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	if roots, ok := v["workspace_roots"].([]string); ok {
		for _, s := range roots {
			if strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	if s, ok := v["workspace"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	if nested, ok := v["payload"].(map[string]any); ok {
		return extractDirectory(nested)
	}
	return ""
}

// InspectHeader inspects Antigravity transcript files for session identity, directory, and start time.
func (NativeHeaders) InspectHeader(r agentapi.NativeHeaderRequest) (agentapi.NativeHeader, error) {
	if r.Scan == nil {
		return agentapi.NativeHeader{}, fmt.Errorf("invalid native header request: scan required")
	}
	if r.Purpose != 0 && r.Purpose != agentapi.DiscoveryImport && r.Purpose != agentapi.DiscoveryHandoff && r.Purpose != agentapi.DiscoveryProjects && r.Purpose != agentapi.DiscoveryBoundedProjects {
		return agentapi.NativeHeader{}, fmt.Errorf("invalid native header request: unsupported purpose")
	}

	var h agentapi.NativeHeader
	pathConvID := extractConversationID(r.Path)
	if pathConvID == "" || !validConversationID(pathConvID) {
		h.IdentityMismatch = true
	} else {
		h.NativeID = pathConvID
	}

	var scanDecodeErr error
	err := r.Scan(func(line []byte) bool {
		var v map[string]any
		if err := json.Unmarshal(line, &v); err != nil {
			scanDecodeErr = err
			return true
		}
		if convID := firstString(v, "conversationId", "conversation_id", "sessionId", "session_id"); convID != "" {
			if !validConversationID(convID) {
				h.IdentityMismatch = true
			} else if h.NativeID != "" && h.NativeID != convID {
				h.IdentityMismatch = true
			} else if h.NativeID == "" {
				h.NativeID = convID
			}
		}
		if h.Directory == "" {
			if dir := extractDirectory(v); dir != "" {
				h.Directory = dir
			}
		}
		if h.StartedAt.IsZero() {
			if ts := firstString(v, "created_at", "timestamp"); ts != "" {
				if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
					h.StartedAt = parsed.UTC()
				} else if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
					h.StartedAt = parsed.UTC()
				}
			}
		}
		if h.NativeID != "" && h.Directory != "" && !h.StartedAt.IsZero() {
			return false
		}
		return true
	})
	if err != nil {
		return h, err
	}
	if scanDecodeErr != nil && h.NativeID == "" && h.Directory == "" && h.StartedAt.IsZero() {
		return h, scanDecodeErr
	}
	return h, nil
}

func validConversationID(value string) bool {
	if len(value) != 36 || strings.Count(value, "-") != 4 {
		return false
	}
	for i, r := range value {
		separator := i == 8 || i == 13 || i == 18 || i == 23
		if separator && r == '-' {
			continue
		}
		if separator {
			return false
		}
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
