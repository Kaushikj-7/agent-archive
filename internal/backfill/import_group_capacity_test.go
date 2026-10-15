package backfill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A canonical parent and two children fit the existing full quota together.
// Ordinary singleton publication headroom must not reject their linked group.
func TestImportPublicationHeadroomPreservesParentGroupAdmission(t *testing.T) {
	t.Parallel()
	r, c, cfg := stagingFixture(t)
	paths := []string{c.TranscriptPath, filepath.Join(c.ProjectRoot, "agent-child-1.jsonl"), filepath.Join(c.ProjectRoot, "agent-child-2.jsonl")}
	for i, path := range paths {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		prefix := claudeTranscript(c.NativeSessionID, c.ProjectRoot, c.StartedAt)
		agent := ""
		if i > 0 {
			agent = fmt.Sprintf("child-%d", i)
			prefix = subagentTranscript(c.NativeSessionID, agent, c.StartedAt)
		}
		if _, err = file.WriteString(prefix); err != nil {
			t.Fatal(err)
		}
		row := fmt.Sprintf(`{"type":"assistant","sessionId":%q,"agentId":%q,"timestamp":%q,"message":{"role":"assistant","content":%q}}`+"\n", c.NativeSessionID, agent, c.StartedAt.UTC().Format(time.RFC3339), strings.Repeat("a", 32<<10))
		for range (24 << 20) / (32 << 10) {
			if _, err = file.WriteString(row); err != nil {
				t.Fatal(err)
			}
		}
		if err = file.Close(); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			c.Subagents = append(c.Subagents, Subagent{Path: path, AgentID: agent})
		}
	}
	remote := storagetest.NewMemoryStore()
	drains, published := 0, 0
	r.OnCapacity = func(ctx context.Context) error {
		drains++
		result, err := collector.Run(ctx, r.Store, remote, collector.Options{Sources: testSources, Parsers: testSources, MachineID: "synthetic", Now: func() time.Time { return fixedNow.Add(time.Minute) }, AcceptSession: cfg.AcceptSession})
		defer result.CloseStagedResults()
		published += len(result.Published)
		t.Logf("group drain published=%d errors=%v top=%v", len(result.Published), result.Errors, err)
		if err != nil {
			return err
		}
		if len(result.Published) == 0 {
			return state.ErrDurableStorageCapacity
		}
		return nil
	}
	result, err := r.Run([]Candidate{c})
	if err != nil || len(result.Sessions) != 1 || len(result.Subagents) != 2 || drains != 0 {
		t.Fatalf("compatible group refused: %+v drains=%d err=%v", result, drains, err)
	}
}
