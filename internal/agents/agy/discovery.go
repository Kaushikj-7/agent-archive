package agy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// Discovery enumerates Antigravity CLI and desktop brain transcript files.
type Discovery struct{}

// DefaultDirectories declares both known Antigravity data roots.
func (Discovery) DefaultDirectories(home string) []string {
	return []string{
		filepath.Join(home, ".gemini", "antigravity-cli"),
		filepath.Join(home, ".gemini", "antigravity"),
	}
}

// Discover emits bounded transcript candidates without admitting capture.
func (d Discovery) Discover(ctx context.Context, r agentapi.DiscoveryRequest, emit func(agentapi.DiscoveryCandidate) error) (report agentapi.DiscoveryReport, err error) {
	if r.Purpose != agentapi.DiscoveryImport && r.Purpose != agentapi.DiscoveryHandoff {
		return report, fmt.Errorf("AGY discovery supports import and handoff only")
	}
	if r.Files == nil || emit == nil {
		return report, errors.New("discovery dependencies required")
	}
	for _, root := range d.DefaultDirectories(r.Locations.UserHome) {
		brain := filepath.Join(root, "brain")
		entries, readErr := r.Files.ReadDir(brain)
		if readErr != nil {
			if !errors.Is(readErr, fs.ErrNotExist) {
				report.Incomplete = true
			}
			continue
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			if !entry.IsDir() || !validConversationID(entry.Name()) {
				continue
			}
			path := filepath.Join(brain, entry.Name(), ".system_generated", "logs", "transcript.jsonl")
			info, statErr := r.Files.Lstat(path)
			if statErr != nil || !info.Mode().IsRegular() {
				continue
			}
			if r.MaxFiles > 0 && report.Enumerated >= r.MaxFiles {
				report.Incomplete = true
				return report, nil
			}
			report.Enumerated++
			candidate := agentapi.DiscoveryCandidate{
				Session: agentapi.NativeSession{Agent: agentmeta.Agy, NativeID: entry.Name()},
				Source:  agentapi.SourceRef{Kind: archive.SourceKindFile, Path: path},
				Root:    root, Bytes: info.Size(), SourceInfo: info, IdentityInspected: true,
			}
			if err := emit(candidate); err != nil {
				return report, err
			}
		}
	}
	return report, nil
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
