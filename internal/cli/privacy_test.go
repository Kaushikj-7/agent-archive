package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type privateTestStore struct {
	*storagetest.MemoryStore
	calls int
}

type canceledPrivacyStore struct {
	*storagetest.MemoryStore
	sawCancellation bool
}

func (s *canceledPrivacyStore) InspectPrivacy(ctx context.Context) storage.PrivacyReport {
	s.sawCancellation = errors.Is(ctx.Err(), context.Canceled)
	return storage.UnknownPrivacy("s3")
}

func TestPrivacyInspectionUsesPassContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &canceledPrivacyStore{MemoryStore: storagetest.NewMemoryStore()}
	inspectBucketPrivacyContext(ctx, config.Config{Storage: credentialsTestConfig()}, store, time.Now())
	if !store.sawCancellation {
		t.Fatal("privacy inspection ignored the pass context")
	}
}

func (s *privateTestStore) InspectPrivacy(ctx context.Context) storage.PrivacyReport {
	s.calls++
	if _, ok := ctx.Deadline(); !ok {
		panic("privacy inspection requires timeout")
	}
	p := storage.UnknownPrivacy("s3")
	p.State = "verified_private"
	p.Reason = "all_bucket_public_access_blocks_enabled"
	return p
}

func TestPrivacyEvidenceIsScopedAndExpires(t *testing.T) {
	t.Parallel()
	cfg := config.Config{Storage: credentialsTestConfig()}
	at := time.Now().UTC()
	remote := &privateTestStore{MemoryStore: storagetest.NewMemoryStore()}
	cfg.BucketPrivacy = inspectBucketPrivacy(cfg, remote, at)
	if remote.calls != 1 {
		t.Fatal("inspection not called")
	}
	if currentBucketPrivacy(cfg, at).State != "verified_private" {
		t.Fatal("missing current evidence")
	}
	for _, when := range []time.Time{at.Add(25 * time.Hour), at.Add(-time.Second)} {
		if currentBucketPrivacy(cfg, when).State != "not_verified" {
			t.Fatal("stale/future evidence trusted")
		}
	}
	cfg.Storage.Bucket = "changed"
	changed := currentBucketPrivacy(cfg, at)
	if changed.State != "not_verified" || changed.Reason != "storage_configuration_changed" {
		t.Fatalf("%+v", changed)
	}
	if encoded, err := json.Marshal(changed); err != nil || strings.Contains(string(encoded), "checked_at") {
		t.Fatalf("never-inspected evidence must omit checked_at: %s %v", encoded, err)
	}
	var out bytes.Buffer
	sc := statusScreen{now: at, verbose: true}
	sc.printRows(&out, []statusRow{sc.privacyRow(changed)})
	if !strings.Contains(out.String(), "https://") || !strings.Contains(out.String(), "Bucket privacy not verified") || !strings.Contains(out.String(), "storage settings changed since the last check") || strings.Contains(out.String(), "checked 0") {
		t.Fatal(out.String())
	}
}

func TestSetupReviewReadsPrivacyWithEnvClock(t *testing.T) {
	t.Parallel()
	// Evidence collected two days ago on the frozen clock is fresh on that
	// clock; only the wall clock would call it stale.
	at := time.Now().UTC().Add(-48 * time.Hour)
	cfg := config.Config{Storage: credentialsTestConfig(), RetentionDays: defaultRetentionDays}
	cfg.BucketPrivacy = inspectBucketPrivacy(cfg, &privateTestStore{MemoryStore: storagetest.NewMemoryStore()}, at)
	var out bytes.Buffer
	p := newPrompter(strings.NewReader(""), &out)
	p.now = func() time.Time { return at }
	printReviewPrivacy(p, cfg)
	if !strings.Contains(out.String(), "native public access blocked") || strings.Contains(out.String(), "inspection_stale") {
		t.Fatal(out.String())
	}
	out.Reset()
	showSetupReview(p, cfg, setupReview{})
	if !strings.Contains(out.String(), "✓ Bucket is private") || strings.Contains(out.String(), "the last check is more than a day old") {
		t.Fatal(out.String())
	}
}

func TestCollectionRefreshesBucketPrivacyEvidence(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	remote := &privateTestStore{MemoryStore: storagetest.NewMemoryStore()}
	stale := inspectBucketPrivacy(cfg, remote, at.Add(-bucketPrivacyRefreshAfter-time.Minute))
	remote.calls = 0
	cfg.BucketPrivacy = stale
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	if _, err := runOnePass(env, true); err != nil {
		t.Fatal(err)
	}
	saved, _, err := config.Load(home)
	if err != nil || remote.calls != 1 || saved.BucketPrivacy == nil || saved.BucketPrivacy.CheckedAt == nil || !saved.BucketPrivacy.CheckedAt.Equal(at) {
		t.Fatalf("stale evidence not refreshed: calls %d, %+v, %v", remote.calls, saved.BucketPrivacy, err)
	}
	if view, err := readStatus(env); err != nil || view.Privacy != "verified_private" {
		t.Fatalf("status after refresh: %+v %v", view, err)
	}
	if _, err := runOnePass(env, true); err != nil || remote.calls != 1 {
		t.Fatalf("fresh evidence re-inspected: calls %d, %v", remote.calls, err)
	}
	if _, err := runOnePass(env, false); err != nil || remote.calls != 1 {
		t.Fatalf("manual sync inspected: calls %d, %v", remote.calls, err)
	}
	// A paused install never opens storage, so stale evidence stays as it is.
	env.Now = func() time.Time { return at.Add(bucketPrivacyStaleAfter + time.Hour) }
	if _, err := config.SetPaused(home, true); err != nil {
		t.Fatal(err)
	}
	if _, err := runOnePass(env, true); !errors.Is(err, errPaused) || remote.calls != 1 {
		t.Fatalf("paused pass inspected: calls %d, %v", remote.calls, err)
	}
	if view, err := readStatus(env); err != nil || view.PrivacyEvidence.Reason != "inspection_stale" {
		t.Fatalf("paused status: %+v %v", view.PrivacyEvidence, err)
	}
}

func TestSetupToleratesBackgroundPrivacyRefresh(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	remote := &privateTestStore{MemoryStore: storagetest.NewMemoryStore()}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	setupRun(t, env, s3SetupInput("synthetic", "us-east-1", "profile", true, false, false, project), 0)
	// While the wizard is open, a collector tick refreshes the evidence on disk.
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		cfg, _, err := config.Load(home)
		if err != nil {
			return nil, err
		}
		cfg.BucketPrivacy = inspectBucketPrivacy(cfg, remote, at.Add(time.Hour))
		cfg.BucketPrivacy.State, cfg.BucketPrivacy.Reason = "public_or_risky", "public_bucket_policy"
		return remote, config.Save(home, cfg)
	}
	setupRun(t, env, "retention\ncustom\n30\ny\n", 0)
	cfg, _, err := config.Load(home)
	if err != nil || cfg.RetentionDays != 30 || cfg.BucketPrivacy == nil || cfg.BucketPrivacy.State != "public_or_risky" || !cfg.BucketPrivacy.CheckedAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("background evidence lost: %+v %v", cfg.BucketPrivacy, err)
	}
}

func TestSetupPersistsPrivacyAndStatusNeverInspects(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	remote := &privateTestStore{MemoryStore: storagetest.NewMemoryStore()}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	output := setupRun(t, env, s3SetupInput("synthetic", "us-east-1", "profile", true, false, false, project), 0)
	if !strings.Contains(output, "✓ Bucket is private") {
		t.Fatal(output)
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found || cfg.BucketPrivacy == nil {
		t.Fatalf("privacy missing: %+v %v", cfg, err)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("status opened remote store")
		return nil, nil
	}
	view, err := readStatus(env)
	if err != nil || view.Privacy != "verified_private" {
		t.Fatalf("status: %+v %v", view, err)
	}
	if remote.calls != 1 {
		t.Fatalf("repeated inspection: %d", remote.calls)
	}
	env.Now = func() time.Time { return at.Add(25 * time.Hour) }
	view, err = readStatus(env)
	if err != nil || view.Privacy != "not_verified" || view.PrivacyEvidence.Reason != "inspection_stale" {
		t.Fatalf("stale status: %+v %v", view, err)
	}
}
