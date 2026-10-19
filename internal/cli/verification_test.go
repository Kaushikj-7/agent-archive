package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// publishSyntheticSessions registers n codex sessions for project and
// publishes each to remote one at a time, so publication times ascend with
// the session index. It returns the configuration and the store.
func publishSyntheticSessions(t *testing.T, home, project string, remote storage.ObjectStore, at time.Time, n int) (config.Config, *state.Store) {
	t.Helper()
	must(t, os.Chmod(home, 0700))
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		path := filepath.Join(project, fmt.Sprintf("synthetic-%d.jsonl", i))
		if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"type":"turn_context","model":"synthetic-%d"}`+"\n", i)), 0600); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("s%d", i)
		reg := archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: "n" + id, ProjectID: "p1", ProjectRoot: project, Harness: archive.Harness{Name: "codex"}, TranscriptPath: path, SessionStartedAt: at, RegisteredAt: at}
		if err := store.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
		publishedAt := at.Add(time.Duration(i) * time.Minute)
		result, err := collector.Run(context.Background(), store, remote, collector.Options{Parsers: productionAgents, Sources: productionAgents, MachineID: cfg.MachineID, Now: func() time.Time { return publishedAt }})
		if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
			t.Fatalf("session %d: %#v %v", i, result, err)
		}
	}
	return cfg, store
}

// removeRemoteSource deletes a published session's source object, leaving
// its metadata pointer in place.
func removeRemoteSource(t *testing.T, remote *storagetest.MemoryStore, id string) archive.Metadata {
	t.Helper()
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := reader.ReadMetadata(context.Background(), remote, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Delete(context.Background(), metadata.SourceBundle.Key); err != nil {
		t.Fatal(err)
	}
	return metadata
}

func TestPrivacyCleanupRequiresExactVerifiedPublication(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	remote := storagetest.NewMemoryStore()
	cfg, localStore := publishSyntheticSessions(t, home, project, remote, at, 1)
	regs, err := localStore.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations: %#v %v", regs, err)
	}
	reg := regs[0]
	key, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	current, err := reader.ReadMetadata(context.Background(), remote, key)
	if err != nil {
		t.Fatal(err)
	}
	if privacyPublicationVerified(home, cfg, localStore, reg, current) {
		t.Fatal("unverified publication accepted")
	}
	env := testEnv(t, home, at.Add(time.Minute))
	if _, err := verifyPublicationsWithin(context.Background(), home, cfg, env, localStore, remote); err != nil {
		t.Fatal(err)
	}
	if !privacyPublicationVerified(home, cfg, localStore, reg, current) {
		t.Fatal("verified publication rejected")
	}
	changed := current
	changed.SourceBundle.SHA256 = strings.Repeat("f", 64)
	if privacyPublicationVerified(home, cfg, localStore, reg, changed) {
		t.Fatal("changed remote source accepted")
	}
	changed = current
	changed.SourceBundle.CompressedBytes++
	if privacyPublicationVerified(home, cfg, localStore, reg, changed) {
		t.Fatal("changed remote source size accepted")
	}
	changed = current
	changed.SourceBundle.Key = strings.Replace(current.SourceBundle.Key, "source.", "source.other-", 1)
	if privacyPublicationVerified(home, cfg, localStore, reg, changed) {
		t.Fatal("changed remote source key accepted")
	}
	cfg.Storage.Bucket = "another-bucket"
	if privacyPublicationVerified(home, cfg, localStore, reg, current) {
		t.Fatal("changed destination accepted")
	}
}

func TestExcludedCurrentDestinationPublicationCanBeVerifiedForPrivacyCleanup(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	remote := storagetest.NewMemoryStore()
	cfg, localStore := publishSyntheticSessions(t, home, project, remote, at, 1)
	regs, err := localStore.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations: %#v %v", regs, err)
	}
	reg := regs[0]
	cfg.Archive.Projects[0].Included = false
	if cfg.AcceptSession(reg) || !cfg.InCurrentDestination(reg) {
		t.Fatal("test needs an excluded session in the current destination")
	}
	env := testEnv(t, home, at.Add(time.Minute))
	summary, err := verifyPublicationsWithin(context.Background(), home, cfg, env, localStore, remote)
	if err != nil || summary.Verified != 1 {
		t.Fatalf("excluded publication was not verified: %#v %v", summary, err)
	}
	key, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	current, err := reader.ReadMetadata(context.Background(), remote, key)
	if err != nil || !privacyPublicationVerified(home, cfg, localStore, reg, current) {
		t.Fatalf("verified publication cannot authorize privacy cleanup: %v", err)
	}
}

func TestReadBackFailureBacksOffAndDoesNotFailSync(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Now().UTC().Truncate(time.Second)
	remote := storagetest.NewMemoryStore()
	cfg, store := publishSyntheticSessions(t, home, project, remote, at, 1)
	metadata := removeRemoteSource(t, remote, "s0")
	source, err := archive.BuildCompressedSource(func() archive.SourceBundle {
		bundle, _, _, err := store.LoadLastPublished("s0")
		if err != nil {
			t.Fatal(err)
		}
		return bundle
	}())
	if err != nil {
		t.Fatal(err)
	}
	now := at.Add(time.Minute)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	env.Now = func() time.Time { return now }
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }

	var out, errOut bytes.Buffer
	if code := runSyncCommand(nil, &out, &errOut, env); code != 0 || strings.Contains(out.String(), "read-back") {
		t.Fatalf("missing remote object failed sync: exit=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Collector.LastError != "" || len(view.Collector.SessionIssues) != 0 {
		t.Fatalf("read-back failure pinned a collection error: %+v", view.Collector)
	}
	if app := view.Apps[0]; app.ReadBackVerified || app.VerificationState != "read_back_failed" || !strings.Contains(app.VerificationDetail, "attempt 1") || app.State != "published; read-back pending" {
		t.Fatalf("status did not report the failed read-back: %+v", app)
	}
	record, err := readVerification(home, "s0")
	if err != nil || record.Outcome != verificationOutcomeFailed || record.Attempts != 1 || !record.VerifiedAt.IsZero() || !record.NextRetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("record=%+v err=%v", record, err)
	}

	// Before the retry time nothing is attempted; each later failure waits
	// longer, up to the cap.
	if summary, err := verifyPublications(home, cfg, env, store, remote); err != nil || summary.Attempted != 0 || summary.Deferred != 1 {
		t.Fatalf("retried before backoff elapsed: %#v %v", summary, err)
	}
	for attempt, delay := range []time.Duration{5 * time.Minute, 30 * time.Minute, 6 * time.Hour, 24 * time.Hour, 24 * time.Hour} {
		now = record.NextRetryAt.Add(time.Second)
		if summary, err := verifyPublications(home, cfg, env, store, remote); err != nil || summary.Attempted != 1 || summary.Failed != 1 {
			t.Fatalf("attempt %d: %#v %v", attempt+2, summary, err)
		}
		if record, err = readVerification(home, "s0"); err != nil || record.Attempts != attempt+2 || !record.NextRetryAt.Equal(now.Add(delay)) {
			t.Fatalf("attempt %d: record=%+v err=%v", attempt+2, record, err)
		}
	}

	// Once the object is back the next scheduled attempt verifies it.
	if err := remote.Put(context.Background(), metadata.SourceBundle.Key, source.Bytes); err != nil {
		t.Fatal(err)
	}
	now = record.NextRetryAt.Add(time.Second)
	if summary, err := verifyPublications(home, cfg, env, store, remote); err != nil || summary.Verified != 1 {
		t.Fatalf("%#v %v", summary, err)
	}
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if app := view.Apps[0]; !app.ReadBackVerified || app.VerificationDetail != "" || app.VerificationState != "verified_at_recorded_time" {
		t.Fatalf("recovered read-back not reported: %+v", app)
	}
}

func TestReadBackMismatchIsDistinctFromTransientFailure(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	remote := storagetest.NewMemoryStore()
	cfg, store := publishSyntheticSessions(t, home, project, remote, at, 1)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	// Another machine's identity invalidates the record and the remote
	// metadata no longer belongs to this configuration.
	cfg.MachineID = "someone-else"
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if summary, err := verifyPublications(home, cfg, env, store, remote); err != nil || summary.Mismatched != 1 || summary.Failed != 0 {
		t.Fatalf("%#v %v", summary, err)
	}
	record, err := readVerification(home, "s0")
	if err != nil || record.Outcome != verificationOutcomeMismatch || record.Attempts != 1 || record.NextRetryAt.IsZero() {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if app := view.Apps[0]; app.VerificationState != "read_back_mismatch" || !strings.Contains(app.VerificationDetail, "mismatch") {
		t.Fatalf("%+v", app)
	}
}

func TestReadBackCapsAttemptsPerPassOldestFirst(t *testing.T) {
	t.Parallel()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	remote := storagetest.NewMemoryStore()
	total := maxVerificationsPerPass + 2
	cfg, store := publishSyntheticSessions(t, home, project, remote, at, total)
	for i := range total {
		removeRemoteSource(t, remote, fmt.Sprintf("s%d", i))
	}
	now := at.Add(time.Hour)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	env.Now = func() time.Time { return now }
	summary, err := verifyPublications(home, cfg, env, store, remote)
	if err != nil || summary.Attempted != maxVerificationsPerPass || summary.Deferred != 2 {
		t.Fatalf("%#v %v", summary, err)
	}
	for i := range total {
		record, err := readVerification(home, fmt.Sprintf("s%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if want := i < maxVerificationsPerPass; (record.Attempts == 1) != want {
			t.Fatalf("session %d attempted=%v want %v (%+v)", i, record.Attempts == 1, want, record)
		}
	}
	// The next pass reaches the two newest while the first five wait out
	// their backoff.
	if summary, err := verifyPublications(home, cfg, env, store, remote); err != nil || summary.Attempted != 2 || summary.Deferred != maxVerificationsPerPass {
		t.Fatalf("%#v %v", summary, err)
	}
}

func TestPassStorageHealthKeepsVerifiedDespiteUnrelatedErrors(t *testing.T) {
	t.Parallel()
	denied := &smithy.GenericAPIError{Code: "AccessDenied", Message: "no"}
	outage := &smithy.OperationError{ServiceID: "S3", OperationName: "PutObject", Err: errors.New("dial tcp: timeout")}
	local := errors.New("transcript was truncated, compacted, or rewritten")
	for _, tc := range []struct {
		name   string
		result collector.Result
		want   string
	}{
		{"published with local failure", collector.Result{Published: []string{"a"}, Errors: map[string]error{"b": local}}, "verified"},
		{"published with outage elsewhere", collector.Result{Published: []string{"a"}, Errors: map[string]error{"b": outage}}, "verified"},
		{"published with auth failure", collector.Result{Published: []string{"a"}, Errors: map[string]error{"b": fmt.Errorf("publish: %w", denied)}}, "authentication_failed"},
		{"nothing published, local failure", collector.Result{Errors: map[string]error{"b": local}}, "not_checked"},
		{"nothing published, outage", collector.Result{Errors: map[string]error{"b": outage}}, "not_checked"},
		{"nothing published, no errors", collector.Result{}, "not_checked"},
	} {
		if got := passStorageHealth(tc.result); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestAuthenticationStalenessSkipsPausedAndProbeRefreshesFirst(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Archive: archive.Config{Enabled: true}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	remote := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	if _, err := runOnePass(env, true); err != nil {
		t.Fatal(err)
	}
	stateAt := func(now time.Time) string {
		t.Helper()
		env.Now = func() time.Time { return now }
		view, err := readStatus(env)
		if err != nil {
			t.Fatal(err)
		}
		return view.Authentication.State
	}
	if got := stateAt(at.Add(storageHealthRefreshAfter + time.Minute)); got != "verified" {
		t.Fatalf("verified record called %q before the staleness threshold", got)
	}
	if got := stateAt(at.Add(storageHealthStaleAfter + time.Second)); got != "stale" {
		t.Fatalf("old record while active: %q", got)
	}
	// A background tick refreshes the probe before status would call it stale.
	env.Now = func() time.Time { return at.Add(storageHealthRefreshAfter + time.Second) }
	if _, err := runOnePass(env, true); err != nil {
		t.Fatal(err)
	}
	var health storageHealth
	if err := local.Read(filepath.Join(home, "storage-health.json"), &health); err != nil || !health.CheckedAt.Equal(at.Add(storageHealthRefreshAfter+time.Second)) {
		t.Fatalf("probe not refreshed: %+v %v", health, err)
	}
	// Paused: the probe deliberately does not run, so the last known state
	// stands with its checked time instead of turning stale.
	cfg.Paused = true
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if got := stateAt(at.Add(24 * time.Hour)); got != "verified" {
		t.Fatalf("paused install reported %q", got)
	}
}

func TestStatusOmitsEmptyAuthenticationContext(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	setUpTestConfig(t, home, "/project", time.Now())
	env := testEnv(t, home, time.Now())
	var out bytes.Buffer
	if code := runStatusCommand([]string{"--verbose"}, &out, os.Stderr, env); code != 0 {
		t.Fatalf("exit=%d output=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "   not checked yet\n") || strings.Contains(out.String(), "; )") || strings.Contains(out.String(), "0001") {
		t.Fatalf("output=%s", out.String())
	}
}

func TestStatusRequiresRecordedReadbackAndInvalidatesConfiguration(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	project := t.TempDir()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "synthetic.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"turn_context","model":"synthetic"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "s1", NativeSessionID: "n1", ProjectID: "p1", ProjectRoot: project, Harness: archive.Harness{Name: "codex", Version: "observed"}, TranscriptPath: path, SessionStartedAt: at, RegisteredAt: at}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	result, err := collector.Run(context.Background(), store, remote, collector.Options{Parsers: productionAgents, Sources: productionAgents, MachineID: cfg.MachineID, Now: func() time.Time { return at }})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("status attempted network")
		return nil, errors.New("unexpected")
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Apps[0].Published || view.Apps[0].ReadBackVerified {
		t.Fatalf("publication was presented as readback: %+v", view.Apps[0])
	}
	if summary, err := verifyPublications(home, cfg, env, store, remote); err != nil || summary.Verified != 1 || summary.Attempted != 1 {
		t.Fatalf("%#v %v", summary, err)
	}
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Apps[0].ReadBackVerified || view.Apps[0].VerifiedAt.IsZero() {
		t.Fatalf("missing verification: %+v", view.Apps[0])
	}
	// Once recorded, no remote call is needed for unchanged publication.
	if summary, err := verifyPublications(home, cfg, env, store, nil); err != nil || summary.Attempted != 0 {
		t.Fatalf("%#v %v", summary, err)
	}
	cfg.Storage.AWSProfile = "rotated"
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Apps[0].ReadBackVerified {
		t.Fatal("old credential context still verified")
	}
}

func TestBackgroundChecksStorageWithoutSessionsAndStatusDoesNotProbe(t *testing.T) {
	t.Parallel()
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Archive: archive.Config{Enabled: true}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	remote := storagetest.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	if _, err := runOnePass(env, true); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Authentication.State != "verified" || view.Authentication.Context != "background_collector" {
		t.Fatalf("%+v", view.Authentication)
	}
	objects, err := remote.List(context.Background(), "")
	if err != nil || len(objects) != 0 {
		t.Fatalf("synthetic probe leaked: %#v %v", objects, err)
	}
}

// Read-back is capped per pass. A large import must not hold a hook-captured
// publication, the only kind that verifies an app, behind the cap.
func TestVerificationReadsBackHookPublicationsBeforeImports(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	project := t.TempDir()
	cfg := pairTestConfig(now, []string{"codex"}, project)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	remote := storagetest.NewMemoryStore()
	for i := range maxVerificationsPerPass + 1 {
		saveImportedSession(t, store, now, "import-"+string(rune('a'+i)), project)
	}
	// The imports publish first, so they are the oldest publications.
	if result, err := collector.Run(context.Background(), store, remote, collector.Options{Parsers: productionAgents, Sources: productionAgents, MachineID: cfg.MachineID, Now: func() time.Time { return now }}); err != nil || len(result.Errors) != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	later := now.Add(time.Hour)
	publishPairSession(t, home, store, remote, cfg, later, "hook", project, false)
	summary, err := verifyPublications(home, cfg, testEnv(t, home, later), store, remote)
	if err != nil || summary.Attempted != maxVerificationsPerPass || summary.Deferred != 2 {
		t.Fatalf("summary=%#v err=%v", summary, err)
	}
	if record, err := readVerification(home, "hook"); err != nil || record.VerifiedAt.IsZero() {
		t.Fatalf("the hook publication waited behind imports: %#v %v", record, err)
	}
}
