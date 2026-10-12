package discovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// A Codex rollout past the former 128 MiB proof limit, but within the Codex
// raw limit capture uses, is admitted: discovery refuses nothing capture
// reads. The file is sparse past its records (a hole, read as NUL bytes,
// then a newline), so it takes no disk space; proof hashes it whole but
// parses only the records up to the first task.
func TestDiscoveryAdmitsCodexRolloutOverFormerProofLimit(t *testing.T) {
	t.Parallel()
	store, cfg, at, root := fixture(t)
	id := writeRollout(t, root, cfg.Archive.Projects[0].Root, at.Add(time.Minute), 1, "sessions")
	path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+id+".jsonl")
	const size = 130 << 20
	file, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("\n"), size-1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() <= 128<<20 || info.Size() > archive.MaxCodexRawTranscriptBytes {
		t.Fatalf("fixture size: %v %v", info, err)
	}
	h, err := run(context.Background(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }}, syntheticSupport)
	if err != nil || h.Registered != 1 {
		t.Fatalf("health=%#v err=%v", h, err)
	}
}
