package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A first setup offers detected apps and the shared project selector, while
// keeping default retention. Current-folder setup never bypasses project consent.
func TestSetupFirstRunUsesDetectedAppsAndSharedProjectSelector(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.withApps(t, "codex", "claude")
	f.inWebApp(t)
	// One confirmation, then S3, profile, bucket, and start.
	out := f.runSetup(t, strings.Join([]string{"", "included-projects", "", "s3-existing", "work", "2", ""}, "\n")+"\n")
	if !setupContainsText(out, "Capture sessions from Codex and Claude Code?") {
		t.Fatalf("no applications question:\n%s", out)
	}
	for _, asked := range []string{"Include Codex?", "Keep sessions for how many days?"} {
		if setupContainsText(out, asked) {
			t.Fatalf("still asked %q:\n%s", asked, out)
		}
	}
	cfg, found, err := config.Load(f.home)
	if err != nil || !found {
		t.Fatalf("config: %v", err)
	}
	if strings.Join(cfg.Harnesses, ",") != "codex,claude" || cfg.RetentionDays != defaultRetentionDays || len(cfg.Archive.Projects) != 1 || !cfg.Archive.Projects[0].Included || !strings.HasSuffix(cfg.Archive.Projects[0].Root, "src/web-app") {
		t.Fatalf("config: apps %v, %d days, projects %+v", cfg.Harnesses, cfg.RetentionDays, cfg.Archive.Projects)
	}
}

// No to the combined question asks for the apps, then the projects, as
// setup always has.
func TestSetupFirstRunDecliningAsksAppsAndProjectsSeparately(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.withApps(t, "codex", "claude")
	f.inWebApp(t)
	out := f.runSetup(t, strings.Join([]string{"n", "n", "n", "y", "n", "", "s3-existing", "work", "2", ""}, "\n")+"\n")
	for _, want := range []string{"Capture sessions from Codex and Claude Code?", "Include Codex?", "Which projects?", "All 1 found projects (default)"} {
		if !setupContainsText(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	cfg, _, err := config.Load(f.home)
	if err != nil || strings.Join(cfg.Harnesses, ",") != "claude" {
		t.Fatalf("apps %v (%v)", cfg.Harnesses, err)
	}
}

// The storage instructions are short and point at the bucket guide, and
// the manual path is unchanged: the instructions come back to the menu.
func TestSetupStorageInstructionsPointAtTheBucketGuide(t *testing.T) {
	t.Parallel()
	f := newScreenFixture(t)
	f.withApps(t, "claude")
	f.inWebApp(t)
	out := f.runSetup(t, strings.Join([]string{"", "", storageMenuNumber(t, "help"), "s3-existing", "work", "2", ""}, "\n")+"\n")
	if !setupContainsText(out, bucketDocURL) {
		t.Fatalf("no link to the bucket guide:\n%s", out)
	}
	if n := strings.Count(out, "Where should your archive live?"); n != 2 {
		t.Fatalf("menu shown %d times, want again after the instructions:\n%s", n, out)
	}
	if setupContainsText(out, "Manage API tokens") {
		t.Fatalf("the long manual steps are back:\n%s", out)
	}
	if _, found, err := config.Load(f.home); err != nil || !found {
		t.Fatalf("manual setup did not finish: %v", err)
	}
}

// The menu is the two providers, the guided choices, then the instructions,
// in that order.
func TestStorageMenuOptionsOrder(t *testing.T) {
	t.Parallel()
	var keys []string
	for _, o := range storageMenuOptions() {
		keys = append(keys, o.Key)
	}
	if strings.Join(keys, ",") != "r2,s3" {
		t.Fatalf("menu = %v", keys)
	}
}

// opStore records every call and refuses the listing the credential probe
// makes, as a store does for a key it does not know.
type opStore struct {
	storage.ObjectStore
	mu       sync.Mutex
	ops      []string
	listFail error
}

func (s *opStore) record(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, op)
}

func (s *opStore) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

func (s *opStore) ListPage(ctx context.Context, prefix, continuation string, limit int32) (storage.ObjectPage, error) {
	s.record("probe")
	if s.listFail != nil {
		return storage.ObjectPage{}, s.listFail
	}
	return s.ObjectStore.(storage.PageLister).ListPage(ctx, prefix, continuation, limit)
}

func (s *opStore) Put(ctx context.Context, key string, value []byte) error {
	s.record("put")
	return s.ObjectStore.Put(ctx, key, value)
}

func newOpStoreEnv(t *testing.T, listFail error) (Env, *opStore, string) {
	t.Helper()
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	store := &opStore{ObjectStore: storagetest.NewMemoryStore(), listFail: listFail}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	return env, store, home
}

// A wrong R2 key fails at the cheap listing, before anything is written,
// with the diagnosis for a key the provider does not know.
func TestSetupBadR2KeyFailsAtTheProbeBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	refused := &smithy.OperationError{ServiceID: "S3", OperationName: "ListObjectsV2", Err: &smithy.GenericAPIError{Code: "InvalidAccessKeyId", Message: "The access key does not exist."}}
	env, store, home := newOpStoreEnv(t, refused)
	input := strings.TrimSuffix(r2SetupInput(t.TempDir(), "WRONGSECRET"), "y\n") + "cancel\n"
	output := setupRun(t, env, input, 1)
	if !setupContainsText(output, "Can't sign in to Cloudflare R2.") || !setupContainsText(output, "Fix: ") {
		t.Fatalf("no diagnosis:\n%s", output)
	}
	if !setupContainsText(output, "Enter the R2 access key again") {
		t.Fatalf("no way to enter the key again:\n%s", output)
	}
	if setupContainsText(output, "WRONGSECRET") {
		t.Fatalf("secret leaked:\n%s", output)
	}
	if ops := store.recorded(); strings.Join(ops, ",") != "probe" {
		t.Fatalf("calls = %v, want only the probe", ops)
	}
	if _, found, err := config.Load(home); err != nil || found {
		t.Fatalf("a failed check saved a configuration: %v %v", found, err)
	}
}

// A key the probe accepts still goes through the full round trip, which
// stays the gate before anything is saved.
func TestSetupGoodR2KeyRunsTheProbeThenVerifyAccess(t *testing.T) {
	t.Parallel()
	env, store, home := newOpStoreEnv(t, nil)
	setupRun(t, env, r2SetupInput(t.TempDir(), "SECRET"), 0)
	ops := store.recorded()
	if len(ops) < 2 || ops[0] != "probe" || ops[1] != "put" {
		t.Fatalf("calls = %v, want the probe, then the round trip's upload", ops)
	}
	if cfg, found, err := config.Load(home); err != nil || !found || cfg.StorageVerifiedAt.IsZero() {
		t.Fatalf("config: %+v %v", cfg, err)
	}
}

// The probe covers an S3 profile the same way, and setup --yes, which
// shares the storage check, fails at it too.
func TestSetupYesBadProfileFailsAtTheProbe(t *testing.T) {
	t.Parallel()
	refused := &smithy.OperationError{ServiceID: "S3", OperationName: "ListObjectsV2", Err: &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}}
	env, store, home := newOpStoreEnv(t, refused)
	project := filepath.Join(t.TempDir(), "app")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	args := []string{"setup", "--yes", "--provider", "s3", "--bucket", "b", "--aws-profile", "p", "--region", "us-east-1", "--apps", "claude", "--project", project}
	if code := Run(args, strings.NewReader(""), &out, &errOut, env); code != 1 {
		t.Fatalf("exit %d\n%s%s", code, &out, &errOut)
	}
	if !setupContainsText(errOut.String(), "Access denied.") {
		t.Fatalf("no diagnosis:\n%s", &errOut)
	}
	if ops := store.recorded(); strings.Join(ops, ",") != "probe" {
		t.Fatalf("calls = %v, want only the probe", ops)
	}
	if _, found, err := config.Load(home); err != nil || found {
		t.Fatalf("saved despite the failed check: %v %v", found, err)
	}
}
