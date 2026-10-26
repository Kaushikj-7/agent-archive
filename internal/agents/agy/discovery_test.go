package agy

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestDiscoveryFindsBrainTranscripts(t *testing.T) {
	root := t.TempDir()
	conversation := "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(root, ".gemini", "antigravity-cli", "brain", conversation, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var got []agentapi.DiscoveryCandidate
	report, err := (Discovery{}).Discover(context.Background(), agentapi.DiscoveryRequest{
		Purpose:   agentapi.DiscoveryImport,
		Locations: agentapi.NativeLocations{UserHome: root},
		Files:     osDiscoveryFiles{},
		MaxFiles:  10,
	}, func(candidate agentapi.DiscoveryCandidate) error {
		got = append(got, candidate)
		return nil
	})
	if err != nil || report.Enumerated != 1 || len(got) != 1 {
		t.Fatalf("Discover() report=%#v candidates=%d err=%v", report, len(got), err)
	}
	if got[0].Session.NativeID != conversation || got[0].Session.Agent != "agy" {
		t.Fatalf("unexpected candidate: %#v", got[0])
	}
}

func TestDiscoveryRejectsMalformedConversationIDs(t *testing.T) {
	for _, id := range []string{
		"aaaaaaa-aaaaa-4aaa-8aaa-aaaaaaaaaaaaa",
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa!",
	} {
		if validConversationID(id) {
			t.Fatalf("validConversationID(%q) = true", id)
		}
	}
}

type osDiscoveryFiles struct{}

func (osDiscoveryFiles) ReadDir(path string) ([]fs.DirEntry, error) { return os.ReadDir(path) }
func (osDiscoveryFiles) Lstat(path string) (fs.FileInfo, error)     { return os.Lstat(path) }
func (osDiscoveryFiles) Open(path string) (io.ReadCloser, error)    { return os.Open(path) }
