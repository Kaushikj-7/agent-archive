package discovery

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/gitremote"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/testutil/recoverytest"
)

func TestPersistedPartialRecoveryRenewsRestoredLocalOrigin(t *testing.T) {
	for _, competing := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated", true: "excluded_competitor"}[competing], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			store, cfg, at, root := fixture(t)
			base := t.TempDir()
			clone, destination, target := filepath.Join(base, "a-clone"), filepath.Join(base, "z-project"), filepath.Join(base, "mirror")
			git := func(path string, args ...string) {
				t.Helper()
				if out, err := gitremote.ExecRunner(t.Context(), path, append([]string{"-C", path}, args...)...); err != nil {
					t.Fatalf("synthetic Git %v: %s: %v", args, out, err)
				}
			}
			for _, path := range []string{clone, destination} {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				git(path, "init", "--quiet")
			}
			remote := "https://example.test/acme/repo"
			git(clone, "config", "remote.origin.url", target)
			git(destination, "config", "remote.origin.url", remote)
			cfg.Archive.Projects = []archive.ProjectActivation{{Root: clone, ProjectID: archive.ProjectID(clone), Included: false}, {Root: destination, ProjectID: archive.ProjectID(destination), Included: true, ActivatedAt: at}}
			if err := config.ReconcileDiscovery(&cfg, config.Config{}, at); err != nil {
				t.Fatal(err)
			}
			if err := config.Save(store.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			gone := filepath.Join(base, "gone")
			native := writeRollout(t, root, gone, at.Add(time.Minute), 81, "sessions")
			path := filepath.Join(root, "sessions", "rollout-2026-10-01T12-00-00-"+native+".jsonl")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw = bytes.Replace(raw, []byte(`"source":"cli"`), []byte(`"git":{"repository_url":"https://example.test/acme/repo"},"source":"cli"`), 1)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			// Establish the native owner's absence before the single resumed
			// scan, rather than using run's two-scan census convenience wrapper.
			if err := store.RequestSessionIndexRecovery(agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: native}); err != nil {
				t.Fatal(err)
			}
			if err := recoverytest.Exhaust(t.Context(), store, state.SessionIndexRecoverySlice, false); err != nil {
				t.Fatal(err)
			}
			observer, calls := &gitremote.IdentityObserver{}, map[string]int{}
			lookup := func(ctx context.Context, path string) sourcefacts.RepositoryIdentity {
				calls[path]++
				return observer.Lookup(ctx, path)
			}
			prior := catalog{Version: catalogVersion, Roots: []string{root}, Cache: map[string]cached{}}
			first := sourcefacts.NewRecoveryResolver(cfg.Archive.Projects, nil, canonicalProjectPath, lookup, &prior.Recovery)
			first.MaxOperations = 1
			if _, outcome := first.Recover(t.Context(), gone, archive.RepoKey(remote)); outcome != sourcefacts.RecoveryBudgetExhausted || prior.Recovery.Cursor != 1 || prior.Recovery.Entries[0].Failure != sourcefacts.RecoveryOriginMissing {
				t.Fatal("fixture did not capture a partial missing-origin observation", outcome, prior.Recovery)
			}
			catalogPath := filepath.Join(store.Home(), "discovery-catalog.json")
			if err := local.WriteCompact(catalogPath, prior); err != nil {
				t.Fatal(err)
			}
			var loaded catalog
			if err := local.Read(catalogPath, &loaded); err != nil || loaded.Recovery.Cursor != 1 || loaded.Recovery.Entries[0].Known {
				t.Fatal("partial recovery did not survive save/load", loaded.Recovery, err)
			}
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			git(target, "init", "--quiet")
			targetRemote := "https://example.test/acme/other"
			if competing {
				targetRemote = remote
			}
			git(target, "config", "remote.origin.url", targetRemote)
			h, err := runWithAdapters(t.Context(), store, cfg, Options{Sources: builtin.NewBuiltins(), Now: func() time.Time { return at.Add(2 * time.Minute) }, RepositoryIdentity: lookup, RepositoryIdentityCurrent: gitremote.ProjectIdentityCurrent}, []SourceAdapter{codexAdapter{supported: syntheticSupport}})
			want := 1
			if competing {
				want = 0
			}
			if err != nil || h.Registered != want || calls[clone] < 2 || calls[clone]+calls[destination] > 5 {
				t.Fatalf("restored origin did not renew through discovery: health=%+v calls=%v err=%v", h, calls, err)
			}
			if competing && h.Outcomes[string(sourcefacts.RecoveryAmbiguous)] == 0 {
				t.Fatal("excluded physical clone ceased competing", h)
			}
			regs, err := store.LoadRegistrations()
			if err != nil || len(regs) != want || (want == 1 && (regs[0].ProjectRoot != destination || regs[0].ProjectResolution == nil)) {
				t.Fatal("incorrect durable recovery ownership", regs, err)
			}
		})
	}
}
