package agy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestNativeHeadersDefaultDirectories(t *testing.T) {
	t.Parallel()
	home := filepath.Join(string(filepath.Separator), "home", "user")
	dirs := (NativeHeaders{}).DefaultDirectories(home)
	if len(dirs) != 2 {
		t.Fatalf("expected 2 default directories, got %d: %v", len(dirs), dirs)
	}
	expected1 := filepath.Join(home, ".gemini", "antigravity-cli")
	expected2 := filepath.Join(home, ".gemini", "antigravity")
	if dirs[0] != expected1 || dirs[1] != expected2 {
		t.Fatalf("got %v, want [%s %s]", dirs, expected1, expected2)
	}
}

func TestNativeHeadersRoots(t *testing.T) {
	t.Parallel()
	home := filepath.Join(string(filepath.Separator), "home", "user")
	p := NativeHeaders{}

	// Default directories when Locations.Directories is nil
	roots := p.Roots(agentapi.NativeLocations{UserHome: home}, agentapi.DiscoveryImport)
	if len(roots) != 2 {
		t.Fatalf("expected 2 roots, got %d", len(roots))
	}
	for _, root := range roots {
		if root.Harness != "agy" {
			t.Errorf("expected Harness agy, got %s", root.Harness)
		}
		if root.Depth != 3 {
			t.Errorf("expected Depth 3, got %d", root.Depth)
		}
		if !root.Recursive {
			t.Errorf("expected Recursive true")
		}
		if root.Suffix != "transcript.jsonl" {
			t.Errorf("expected Suffix transcript.jsonl, got %s", root.Suffix)
		}
		if filepath.Base(root.Path) != "brain" {
			t.Errorf("expected Path to end in brain, got %s", root.Path)
		}
	}

	// Explicit directories override default directories
	customDir := filepath.Join(home, "custom", "agy")
	customRoots := p.Roots(agentapi.NativeLocations{UserHome: home, Directories: []string{customDir}}, agentapi.DiscoveryHandoff)
	if len(customRoots) != 1 {
		t.Fatalf("expected 1 custom root, got %d", len(customRoots))
	}
	expectedCustomPath := filepath.Join(customDir, "brain")
	if customRoots[0].Path != expectedCustomPath {
		t.Fatalf("got %s, want %s", customRoots[0].Path, expectedCustomPath)
	}
}

func TestNativeHeadersInspectHeader(t *testing.T) {
	t.Parallel()
	const validID = "11111111-1111-4111-8111-111111111111"
	p := NativeHeaders{}

	t.Run("valid path and matching content", func(t *testing.T) {
		path := filepath.Join(string(filepath.Separator), "home", "user", ".gemini", "antigravity-cli", "brain", validID, ".system_generated", "logs", "transcript.jsonl")
		content := []byte(`{"conversationId":"` + validID + `","cwd":"/workspace/my-project","created_at":"2026-11-08T10:00:00Z"}`)
		h, err := p.InspectHeader(agentapi.NativeHeaderRequest{
			Purpose: agentapi.DiscoveryImport,
			Path:    path,
			Scan: func(visit func([]byte) bool) error {
				visit(content)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h.IdentityMismatch {
			t.Fatalf("expected no identity mismatch, got mismatch")
		}
		if h.NativeID != validID {
			t.Fatalf("got NativeID %s, want %s", h.NativeID, validID)
		}
		if h.Directory != "/workspace/my-project" {
			t.Fatalf("got Directory %s, want /workspace/my-project", h.Directory)
		}
		wantTime, _ := time.Parse(time.RFC3339, "2026-11-08T10:00:00Z")
		if !h.StartedAt.Equal(wantTime.UTC()) {
			t.Fatalf("got StartedAt %v, want %v", h.StartedAt, wantTime)
		}
	})

	t.Run("workspacePaths array fallback for directory", func(t *testing.T) {
		path := filepath.Join("brain", validID, ".system_generated", "logs", "transcript.jsonl")
		line1 := []byte(`{"step_index":0,"type":"SYSTEM_MESSAGE","created_at":"2026-11-08T10:00:00Z"}`)
		line2 := []byte(`{"step_index":1,"type":"USER_INPUT","workspacePaths":["/my/workspace/root"]}`)
		h, err := p.InspectHeader(agentapi.NativeHeaderRequest{
			Purpose: agentapi.DiscoveryHandoff,
			Path:    path,
			Scan: func(visit func([]byte) bool) error {
				if visit(line1) {
					visit(line2)
				}
				return nil
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if h.IdentityMismatch {
			t.Fatal("expected no identity mismatch")
		}
		if h.NativeID != validID {
			t.Fatalf("got NativeID %s, want %s", h.NativeID, validID)
		}
		if h.Directory != "/my/workspace/root" {
			t.Fatalf("got Directory %s, want /my/workspace/root", h.Directory)
		}
	})

	t.Run("content mismatched conversation ID triggers mismatch", func(t *testing.T) {
		path := filepath.Join("brain", validID, ".system_generated", "logs", "transcript.jsonl")
		const otherID = "22222222-2222-4222-8222-222222222222"
		content := []byte(`{"conversationId":"` + otherID + `","cwd":"/proj"}`)
		h, err := p.InspectHeader(agentapi.NativeHeaderRequest{
			Purpose: agentapi.DiscoveryImport,
			Path:    path,
			Scan: func(visit func([]byte) bool) error {
				visit(content)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !h.IdentityMismatch {
			t.Fatal("expected identity mismatch for conflicting IDs")
		}
	})

	t.Run("content malformed conversation ID triggers mismatch", func(t *testing.T) {
		path := filepath.Join("brain", validID, ".system_generated", "logs", "transcript.jsonl")
		content := []byte(`{"conversationId":"not-a-valid-uuid","cwd":"/proj"}`)
		h, err := p.InspectHeader(agentapi.NativeHeaderRequest{
			Purpose: agentapi.DiscoveryImport,
			Path:    path,
			Scan: func(visit func([]byte) bool) error {
				visit(content)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !h.IdentityMismatch {
			t.Fatal("expected identity mismatch for invalid content ID")
		}
	})

	t.Run("path malformed conversation ID triggers mismatch", func(t *testing.T) {
		path := filepath.Join("brain", "not-a-uuid", ".system_generated", "logs", "transcript.jsonl")
		content := []byte(`{"cwd":"/proj"}`)
		h, err := p.InspectHeader(agentapi.NativeHeaderRequest{
			Purpose: agentapi.DiscoveryImport,
			Path:    path,
			Scan: func(visit func([]byte) bool) error {
				visit(content)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !h.IdentityMismatch {
			t.Fatal("expected identity mismatch for invalid path ID")
		}
	})

	t.Run("nil scanner returns error", func(t *testing.T) {
		_, err := p.InspectHeader(agentapi.NativeHeaderRequest{
			Purpose: agentapi.DiscoveryImport,
			Path:    filepath.Join("brain", validID, "transcript.jsonl"),
			Scan:    nil,
		})
		if err == nil {
			t.Fatal("expected error for nil scan")
		}
	})

	t.Run("unsupported purpose returns error", func(t *testing.T) {
		_, err := p.InspectHeader(agentapi.NativeHeaderRequest{
			Purpose: agentapi.DiscoveryPurpose(99),
			Path:    filepath.Join("brain", validID, "transcript.jsonl"),
			Scan: func(func([]byte) bool) error {
				return nil
			},
		})
		if err == nil {
			t.Fatal("expected error for unsupported purpose")
		}
	})
}

func TestNativeHeadersDiscover(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	conversation := "33333333-3333-4333-8333-333333333333"
	path := filepath.Join(root, ".gemini", "antigravity-cli", "brain", conversation, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	record := map[string]any{
		"conversationId": conversation,
		"cwd":            "/tmp/test-project",
		"created_at":     "2026-11-08T12:00:00Z",
	}
	bytes, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(bytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	p := NativeHeaders{}
	var candidates []agentapi.DiscoveryCandidate
	report, err := p.Discover(context.Background(), agentapi.DiscoveryRequest{
		Purpose:     agentapi.DiscoveryImport,
		Stage:       agentapi.DiscoveryIdentities,
		Locations:   agentapi.NativeLocations{UserHome: root},
		Files:       osDiscoveryFiles{},
		MaxFiles:    10,
		HeaderBytes: 4096,
		RecordBytes: 4096,
	}, func(c agentapi.DiscoveryCandidate) error {
		candidates = append(candidates, c)
		return nil
	})
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if report.Enumerated != 1 || len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got enumerated=%d len=%d", report.Enumerated, len(candidates))
	}
	cand := candidates[0]
	if cand.Session.NativeID != conversation {
		t.Fatalf("got NativeID %s, want %s", cand.Session.NativeID, conversation)
	}
	if cand.Session.Agent != "agy" {
		t.Fatalf("got Agent %s, want agy", cand.Session.Agent)
	}
	if cand.Header.Directory != "/tmp/test-project" {
		t.Fatalf("got Header.Directory %s, want /tmp/test-project", cand.Header.Directory)
	}
}
