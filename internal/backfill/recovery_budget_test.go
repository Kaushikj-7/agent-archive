package backfill

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// Many deleted-worktree sessions with a dozen stamped configured roots used to
// exhaust the planning metadata allowance (2*12*129 = 3,096) after about a
// dozen sessions, deterministically: every later session was
// project_budget_exhausted. All of them recover now, and a repository change
// before admission is still caught.
func TestDeletedWorktreePlanningMetadataBudgetIsPerSlice(t *testing.T) {
	tr := newTree(t)
	var projects []archive.ProjectActivation
	keys := map[string]string{}
	for i := range 12 {
		root := tr.repo(fmt.Sprintf("home/r%02d", i))
		projects = append(projects, project(root, true))
		keys[root] = archive.RepoKey(fmt.Sprintf("https://example.test/acme/r%02d", i))
	}
	target := projects[len(projects)-1].Root
	const sessions = 60
	for i := range sessions {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1)
		gone := tr.path(fmt.Sprintf("home/.codex/worktrees/wt%02d/r11", i))
		body := strings.Replace(codexTranscript(id, id, gone, fixedNow.Add(-time.Hour)), `"source":"cli"`, `"git":{"repository_url":"https://example.test/acme/r11"},"source":"cli"`, 1)
		tr.write(filepath.Join("home", codexFile(id)), body)
	}
	env := tr.env()
	env.RepositoryIdentity = func(_ context.Context, root string) sourcefacts.RepositoryIdentity {
		deps := make([]sourcefacts.RepositoryDependency, 20)
		for j := range deps {
			deps[j] = sourcefacts.RepositoryDependency{Path: filepath.Join(root, ".git", fmt.Sprintf("config.%d", j)), Stamp: "synthetic"}
		}
		return sourcefacts.RepositoryIdentity{Root: root, Key: keys[root], Known: true, Dependencies: deps}
	}
	stale := ""
	env.RepositoryIdentityCurrent = func(id sourcefacts.RepositoryIdentity) bool { return id.Root != stale }
	cfg := config.Config{Archive: archive.Config{Enabled: true, Projects: projects}}
	p := plan(t, env, nil, cfg, Filters{})
	for _, c := range p.Candidates {
		if c.Skip != "" || c.ProjectRoot != target || c.ProjectResolution == nil {
			t.Fatalf("%s: skip %s %+v", c.NativeSessionID, c.Skip, c.Diagnostic)
		}
	}
	if got := len(p.Imported()); got != sessions {
		t.Fatalf("imported %d of %d", got, sessions)
	}
	if err := p.CheckRecovery(t.Context()); err != nil {
		t.Fatal(err)
	}
	stale = projects[4].Root
	if err := p.CheckRecovery(t.Context()); err == nil {
		t.Fatal("changed configured repository admitted")
	}
}

// Recovery limits are mostly fixed, so the diagnostic must not promise that
// running the same plan again clears them.
func TestRecoveryBudgetDiagnosticDoesNotPromiseRetry(t *testing.T) {
	d := candidateDiagnostic(SkipWorktreeUnresolved, sourcefacts.RecoveryBudgetExhausted, "")
	if d == nil || d.Detail != DetailRecoveryBudget || d.Action == ActionRetry {
		t.Fatalf("%+v", d)
	}
	if text := diagnosticDescriptions[DetailRecoveryBudget].text; !strings.Contains(text, "--map-project") || strings.Contains(text, "retry the plan") {
		t.Fatal(text)
	}
}
