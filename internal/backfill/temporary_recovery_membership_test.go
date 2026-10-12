package backfill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/gitremote"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

type temporaryRecoveryIdentity string

const (
	temporaryRecoverySameKey        temporaryRecoveryIdentity = "same_key"
	temporaryRecoveryDifferentKey   temporaryRecoveryIdentity = "different_key"
	temporaryRecoveryKeyless        temporaryRecoveryIdentity = "keyless"
	temporaryRecoveryFailedOrigin   temporaryRecoveryIdentity = "failed_origin"
	temporaryRecoveryDanglingMarker temporaryRecoveryIdentity = "dangling_marker"
	temporaryRecoveryPlainFolder    temporaryRecoveryIdentity = "plain_folder"
)

// A temporary clone is excluded from default capture, but not from the observed
// repository union used to assign an absent session to a unique live checkout.
// Use the production Git identity observer on isolated synthetic repositories.
func TestTemporaryCloneRemainsRecordedRecoveryMembership(t *testing.T) {
	for _, include := range []bool{false, true} {
		for _, identity := range []temporaryRecoveryIdentity{temporaryRecoverySameKey, temporaryRecoveryDifferentKey, temporaryRecoveryKeyless, temporaryRecoveryFailedOrigin, temporaryRecoveryDanglingMarker, temporaryRecoveryPlainFolder} {
			t.Run(string(identity)+map[bool]string{false: "/default", true: "/include_temp"}[include], func(t *testing.T) {
				tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
				t.Setenv("HOME", tr.home)
				t.Setenv("XDG_CONFIG_HOME", tr.path("isolated-config"))
				t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
				t.Setenv("GIT_CONFIG_GLOBAL", tr.path("isolated-global-config"))
				clone := tr.mkdir("tmp/clone")
				remote := "https://example.test/acme/repo"
				temporaryRecoveryGit(t, root, "init", "--quiet")
				temporaryRecoveryGit(t, root, "config", "remote.origin.url", remote)
				cloneRemote := remote
				switch identity {
				case temporaryRecoveryDifferentKey:
					cloneRemote = "https://example.test/acme/other"
				case temporaryRecoveryKeyless:
					cloneRemote = tr.path("local-origin")
				case temporaryRecoverySameKey, temporaryRecoveryFailedOrigin, temporaryRecoveryDanglingMarker, temporaryRecoveryPlainFolder:
					// These cases retain the original remote.
				}
				if identity == temporaryRecoveryDanglingMarker {
					if err := os.Symlink(filepath.Join(clone, "missing-git"), filepath.Join(clone, ".git")); err != nil {
						t.Fatal(err)
					}
				} else if identity != temporaryRecoveryPlainFolder {
					temporaryRecoveryGit(t, clone, "init", "--quiet")
					temporaryRecoveryGit(t, clone, "config", "remote.origin.url", cloneRemote)
				}
				cloneID := "00000000-0000-0000-0000-000000000042"
				tr.write(filepath.Join("home", codexFile(cloneID)), codexTranscript(cloneID, cloneID, clone, fixedNow.Add(-time.Hour)))
				observedClone := false
				observer := gitremote.IdentityObserver{Run: func(ctx context.Context, dir string, args ...string) ([]byte, error) {
					if identity == temporaryRecoveryFailedOrigin && dir == clone && len(args) == 6 && args[2] == "config" && args[3] == "-z" && args[4] == "--get-all" && args[5] == "remote.origin.url" {
						return nil, errors.New("synthetic origin read failure")
					}
					return gitremote.ExecRunner(ctx, dir, args...)
				}}
				env.RepositoryIdentity = func(ctx context.Context, path string) sourcefacts.RepositoryIdentity {
					observedClone = observedClone || path == clone
					return observer.Lookup(ctx, path)
				}
				env.RepositoryIdentityCurrent = gitremote.ProjectIdentityCurrent
				p := plan(t, env, nil, cfg, Filters{IncludeTemp: include})
				missing := candidate(t, p, goneID)
				want := map[temporaryRecoveryIdentity]sourcefacts.RecoveryOutcome{temporaryRecoverySameKey: sourcefacts.RecoveryAmbiguous, temporaryRecoveryFailedOrigin: sourcefacts.RecoveryInventoryUnavailable, temporaryRecoveryDanglingMarker: sourcefacts.RecoveryInventoryUnavailable}[identity]
				if want != "" {
					if missing.Skip != SkipWorktreeUnresolved || missing.ProjectResolution != nil || missing.ProjectRoot != "" || missing.Diagnostic == nil || missing.Diagnostic.Detail != DiagnosticDetail(want) {
						t.Fatalf("temporary evidence must prevent false uniqueness (%s): %+v diagnostic %+v", want, missing, missing.Diagnostic)
					}
				} else if missing.Skip != "" || missing.ProjectRoot != root || missing.ProjectResolution == nil || missing.ProjectResolution.Method != "recorded_repository" {
					t.Fatalf("unique ordinary checkout must remain recoverable: %+v", missing)
				}
				if observedClone != (identity != temporaryRecoveryPlainFolder) {
					t.Fatalf("checkout membership observation=%v for %s", observedClone, identity)
				}
				if candidate(t, p, "00000000-0000-0000-0000-000000000011").Skip != "" {
					t.Fatal("ordinary live session lost progress")
				}
				temp := candidate(t, p, cloneID)
				if temp.ProjectRoot != clone || temp.ProjectKind != ProjectKindTemporary || temp.ProjectIncluded || (include && temp.Skip != "") || (!include && temp.Skip != SkipTemporaryDirectory) {
					t.Fatalf("temporary capture eligibility changed: %+v", temp)
				}
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatalf("unchanged observations failed renewal: %v", err)
				}
				if _, err := ApplyToConfig(&cfg, p, fixedNow); err != nil {
					t.Fatal(err)
				}
				configuredClone := false
				for _, project := range cfg.Archive.Projects {
					configuredClone = configuredClone || project.Root == clone
				}
				if configuredClone != include {
					t.Fatalf("temporary membership became implicit capture authorization: configured=%v include=%v", configuredClone, include)
				}
				if identity == temporaryRecoveryDifferentKey || identity == temporaryRecoveryKeyless {
					temporaryRecoveryGit(t, clone, "config", "remote.origin.url", remote)
					if err := p.CheckRecovery(t.Context()); err == nil {
						t.Fatal("temporary clone gained the recorded key without invalidating the unique proof")
					}
				}
			})
		}
	}
}

func temporaryRecoveryGit(t *testing.T, root string, args ...string) {
	t.Helper()
	if out, err := gitremote.ExecRunner(t.Context(), root, append([]string{"-C", root}, args...)...); err != nil {
		t.Fatalf("synthetic repository Git %v: %s: %v", args, out, err)
	}
}
