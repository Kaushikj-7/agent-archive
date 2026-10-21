package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/platform"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/host"
	"github.com/wangjohn/agent-archive/internal/scheduler/launchd"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func overviewFixture(t *testing.T) (string, Env, config.Config, statusSummary) {
	t.Helper()
	home := t.TempDir()
	must(t, os.Chmod(home, 0700))
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	env := testEnv(t, home, now)
	env.Scheduler = newFakeScheduler(t, "loaded")
	must(t, config.Save(home, pairTestConfig(now, []string{"codex"}, t.TempDir())))
	cfg, _, err := config.Load(home)
	must(t, err)
	summary := statusSummary{Version: 1, ConfigurationID: statusConfigurationID(cfg), ObservedAt: now, CompletedAt: now, LastPublishedAt: now.Add(-time.Minute), PendingKnown: true, Pending: 3}
	must(t, local.Write(filepath.Join(home, statusSummaryFile), summary))
	return home, env, cfg, summary
}

func TestStatusOverviewDoesNotReadSessionHistory(t *testing.T) {
	t.Parallel()
	home, env, _, _ := overviewFixture(t)
	// Neither malformed legacy status nor unusable session directories may
	// trigger a history scan, a warning, or a fallback from the saved summary.
	for _, name := range []string{"status.json", "registrations", "published", "pending", "discovery"} {
		must(t, os.WriteFile(filepath.Join(home, name), []byte("not session state"), 0600))
	}
	must(t, local.Write(filepath.Join(home, "application-versions.json"), map[string]applicationDiscovery{
		"codex":  {Installed: true, ObservedAt: env.now().Add(-time.Hour)},
		"claude": {Installed: true, ObservedAt: env.now().Add(-time.Hour)},
	}))
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Error("status opened storage")
		return nil, errors.New("unexpected storage access")
	}
	var out strings.Builder
	if code := Run([]string{"status"}, nil, &out, &out, env); code != 0 {
		t.Fatalf("exit=%d: %s", code, &out)
	}
	for _, want := range []string{"Working", "Completed just now", "3 pending at last check", "Detected agents", "Codex", "Claude Code", "Archiving enabled", "Archiving off", "detected 1 hour ago", "agent-archive status --issues"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, &out)
		}
	}
	for _, unwanted := range []string{"not session state", "Capture\n", "Details\n", "corrupt", "transcript"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("overview included history detail %q:\n%s", unwanted, &out)
		}
	}
}

func TestStatusOverviewPrintsWithoutPager(t *testing.T) {
	t.Parallel()
	_, env, _, _ := overviewFixture(t)
	paged, command, direct := pagedRun(t, env, false, "status")
	if paged != "" || command != "" || !strings.Contains(direct, "Agent Archive  Working") {
		t.Fatalf("overview was paged: command=%q paged=%q direct=%q", command, paged, direct)
	}
}

type overviewCondition string

const (
	conditionPaused            overviewCondition = "paused"
	conditionDisabled          overviewCondition = "disabled"
	conditionBrokenExecutable  overviewCondition = "broken executable"
	conditionSetupRecovery     overviewCondition = "setup recovery"
	conditionFailedCredentials overviewCondition = "failed credentials"
	conditionFailedCollection  overviewCondition = "failed collection"
	conditionStuckCollector    overviewCondition = "stuck collector"
)

func TestStatusOverviewReportsOverrides(t *testing.T) {
	t.Parallel()
	for _, condition := range []overviewCondition{conditionPaused, conditionDisabled, conditionBrokenExecutable, conditionSetupRecovery, conditionFailedCredentials, conditionFailedCollection, conditionStuckCollector} {
		t.Run(string(condition), func(t *testing.T) {
			t.Parallel()
			home, env, cfg, summary := overviewFixture(t)
			want := overviewAttention
			switch condition {
			case conditionPaused:
				cfg.Paused, want = true, overviewPaused
			case conditionDisabled:
				cfg.Archive.Enabled, want = false, overviewUninstalled
			case conditionBrokenExecutable:
				cfg.InstalledExecutable = filepath.Join(home, "missing-executable")
			case conditionSetupRecovery:
				must(t, os.WriteFile(filepath.Join(home, "setup-transaction.json"), []byte("{}"), 0600))
			case conditionFailedCredentials:
				must(t, local.Write(filepath.Join(home, "storage-health.json"), storageHealth{ConfigurationID: configurationID(cfg), State: "authentication_failed", CheckedAt: env.now()}))
			case conditionFailedCollection:
				summary.Problem = statusProblemCapture
				must(t, local.Write(filepath.Join(home, statusSummaryFile), summary))
			case conditionStuckCollector:
				unlock, err := lockCollector(home, "sync", env.now().Add(-3*time.Hour))
				must(t, err)
				defer unlock()
			}
			must(t, config.Save(home, cfg))
			view, err := readStatusOverview(env)
			must(t, err)
			if view.state != want || view.problem == "" {
				t.Fatalf("%s: %+v, want %s", condition, view, want)
			}
		})
	}
}

type summaryCondition string

const (
	summaryMissing         summaryCondition = "missing"
	summaryMalformed       summaryCondition = "malformed"
	summaryOversize        summaryCondition = "oversize"
	summarySymlink         summaryCondition = "symlink"
	summaryFutureVersion   summaryCondition = "future version"
	summaryDifferentConfig summaryCondition = "different config"
	summaryFutureClock     summaryCondition = "future clock"
	summaryStale           summaryCondition = "stale"
	summaryOldCompletion   summaryCondition = "old completion"
	summaryNegativeCount   summaryCondition = "negative count"
)

func TestStatusOverviewDoesNotTrustUnavailableSummaries(t *testing.T) {
	t.Parallel()
	for _, kind := range []summaryCondition{summaryMissing, summaryMalformed, summaryOversize, summarySymlink, summaryFutureVersion, summaryDifferentConfig, summaryFutureClock, summaryStale, summaryOldCompletion, summaryNegativeCount} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			home, env, _, summary := overviewFixture(t)
			path := filepath.Join(home, statusSummaryFile)
			switch kind {
			case summaryMissing:
				must(t, os.Remove(path))
			case summaryMalformed:
				must(t, os.WriteFile(path, []byte("{broken"), 0600))
			case summaryOversize:
				must(t, os.Truncate(path, statusFileLimit+1))
			case summarySymlink:
				must(t, os.Rename(path, path+".target"))
				must(t, os.Symlink(path+".target", path))
			case summaryFutureVersion:
				summary.Version++
			case summaryDifferentConfig:
				summary.ConfigurationID = "other"
			case summaryFutureClock:
				summary.ObservedAt = env.now().Add(time.Minute)
			case summaryStale:
				summary.ObservedAt = env.now().Add(-time.Hour)
				summary.CompletedAt = summary.ObservedAt
				summary.LastPublishedAt = summary.ObservedAt
			case summaryOldCompletion:
				summary.CompletedAt = env.now().Add(-time.Hour)
			case summaryNegativeCount:
				summary.Pending = -1
			}
			if kind != summaryMissing && kind != summaryMalformed && kind != summaryOversize && kind != summarySymlink {
				must(t, local.Write(path, summary))
			}
			view, err := readStatusOverview(env)
			must(t, err)
			if view.state != overviewUnknown || view.problem == "" {
				t.Fatalf("unavailable summary looked healthy: %+v", view)
			}
		})
	}
}

type overviewScheduler struct {
	scheduler.Scheduler
	inspect func(context.Context, scheduler.Site, scheduler.Ref) scheduler.Status
}

func (s overviewScheduler) Inspect(ctx context.Context, site scheduler.Site, ref scheduler.Ref) scheduler.Status {
	return s.inspect(ctx, site, ref)
}

func TestStatusOverviewBoundsSchedulerWait(t *testing.T) {
	t.Parallel()
	_, env, _, _ := overviewFixture(t)
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		env.Scheduler = overviewScheduler{Scheduler: env.Scheduler, inspect: func(ctx context.Context, _ scheduler.Site, _ scheduler.Ref) scheduler.Status {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > statusLivenessBudget {
				t.Error("scheduler received no bounded deadline")
			}
			// Even an inspector that fails to honor cancellation cannot hold
			// the command open. The test releases it before leaving the bubble.
			<-release
			return scheduler.Status{State: scheduler.Loaded, Defined: true}
		}}
		view, err := readStatusOverview(env)
		must(t, err)
		if view.state != overviewUnknown || !strings.Contains(view.background, "budget") {
			t.Fatalf("timed-out scheduler looked healthy: %+v", view)
		}
	})
}

func TestStatusOverviewRequiresLiveLockForProgress(t *testing.T) {
	t.Parallel()
	home, env, cfg, _ := overviewFixture(t)
	unlock, err := lockCollector(home, "sync", env.now().Add(-time.Minute))
	must(t, err)
	defer unlock()
	w := beginStatusProgress(home, cfg, env)
	w.progress.Processed = 12
	w.phase(statusCollecting)
	view, err := readStatusOverview(env)
	must(t, err)
	if view.state != overviewProcessing || !strings.Contains(view.background, "12 sessions completed") {
		t.Fatalf("missing live progress: %+v", view)
	}
	w.progress.PID++
	w.write()
	view, err = readStatusOverview(env)
	must(t, err)
	if strings.Contains(view.background, "12 sessions") {
		t.Fatalf("accepted another process's progress: %+v", view)
	}
	unlock()
	view, err = readStatusOverview(env)
	must(t, err)
	if view.state == overviewProcessing || strings.Contains(view.background, "12 sessions") {
		t.Fatalf("accepted progress without a live lock: %+v", view)
	}
}

func TestStatusSummaryRecordsFinalPassAndPreservesFailures(t *testing.T) {
	t.Parallel()
	home, env, cfg, previous := overviewFixture(t)
	store, err := state.Open(home)
	must(t, err)
	must(t, store.SaveStatus(state.Status{LastScanAt: env.now(), LastPublishedAt: env.now(), PendingCount: 7}))
	w := beginStatusProgress(home, cfg, env)
	w.finish(cfg, nil, true)
	var summary statusSummary
	err = local.Read(filepath.Join(home, statusSummaryFile), &summary)
	must(t, err)
	if !validStatusSummary(summary, statusConfigurationID(cfg), env.now()) || summary.Pending != 7 || summary.Problem != statusProblemNone {
		t.Fatalf("summary did not record completed pass: %+v", summary)
	}
	if _, err := os.Stat(filepath.Join(home, statusProgressFile)); !os.IsNotExist(err) {
		t.Fatal("completed pass retained progress")
	}
	// A later preflight failure must not turn the old pass into a new success.
	env.Now = func() time.Time { return previous.ObservedAt.Add(time.Minute) }
	w = beginStatusProgress(home, cfg, env)
	w.finish(cfg, errors.New("verification failed"), true)
	err = local.Read(filepath.Join(home, statusSummaryFile), &summary)
	must(t, err)
	if summary.Problem != statusProblemCollection || !summary.CompletedAt.Equal(previous.CompletedAt) {
		t.Fatalf("failure renewed successful completion: %+v", summary)
	}
	// Scoped imports cannot renew global health or clear a global failure.
	w = beginStatusProgress(home, cfg, env)
	w.finish(cfg, nil, false)
	var after statusSummary
	err = local.Read(filepath.Join(home, statusSummaryFile), &after)
	must(t, err)
	if after != summary {
		t.Fatalf("scoped import changed global health: %+v", after)
	}
}

func TestStatusSummarySizeDoesNotGrowWithIssues(t *testing.T) {
	t.Parallel()
	home, env, cfg, _ := overviewFixture(t)
	record := state.Status{LastScanAt: env.now(), SessionIssues: map[string]string{}, IssueCounts: map[string]int{issueCaptureFailed: 5000}}
	for i := range 5000 {
		record.SessionIssues[fmt.Sprintf("synthetic-session-%d", i)] = issueCaptureFailed
	}
	record.SetLastErrors("5000 sessions failed to capture or upload")
	store, err := state.Open(home)
	must(t, err)
	must(t, store.SaveStatus(record))
	must(t, beginStatusProgress(home, cfg, env).saveSummary(cfg, nil, true))
	data, err := os.ReadFile(filepath.Join(home, statusSummaryFile))
	must(t, err)
	if len(data) > 1024 || strings.Contains(string(data), "synthetic-session") {
		t.Fatalf("summary retained per-session evidence (%d bytes)", len(data))
	}
}

func TestStatusSummaryDoesNotInheritAnotherDestinationsUpload(t *testing.T) {
	t.Parallel()
	home, env, cfg, _ := overviewFixture(t)
	cfg.Storage.Bucket = "new-destination"
	must(t, config.Save(home, cfg))
	store, err := state.Open(home)
	must(t, err)
	// The legacy collector record retains the old destination's timestamp.
	must(t, store.SaveStatus(state.Status{LastScanAt: env.now(), LastPublishedAt: env.now().Add(-time.Minute)}))
	must(t, beginStatusProgress(home, cfg, env).saveSummary(cfg, nil, true))
	var summary statusSummary
	must(t, local.Read(filepath.Join(home, statusSummaryFile), &summary))
	if !summary.LastPublishedAt.IsZero() {
		t.Fatalf("old upload attributed to the new destination: %+v", summary)
	}
}

func TestStatusProgressThrottlesReportsAndForwardsEvents(t *testing.T) {
	t.Parallel()
	home, env, cfg, _ := overviewFixture(t)
	now := env.now()
	env.Now = func() time.Time { return now }
	w := beginStatusProgress(home, cfg, env)
	forwarded := 0
	observe := w.observe(func(collector.Progress) { forwarded++ })
	for range 10 {
		observe(collector.Progress{Published: true})
	}
	var saved statusProgress
	must(t, local.Read(filepath.Join(home, statusProgressFile), &saved))
	if saved.Processed != 0 || forwarded != 10 {
		t.Fatalf("unthrottled reports or lost events: saved=%+v forwarded=%d", saved, forwarded)
	}
	now = now.Add(statusProgressInterval)
	observe(collector.Progress{})
	must(t, local.Read(filepath.Join(home, statusProgressFile), &saved))
	if saved.Processed != 11 || saved.Published != 10 || forwarded != 11 || !saved.UpdatedAt.Equal(now) {
		t.Fatalf("progress not updated: saved=%+v forwarded=%d", saved, forwarded)
	}
}

func TestCollectionWritesOverviewSummary(t *testing.T) {
	t.Parallel()
	home, env, _, _ := overviewFixture(t)
	result, err := runOnePass(env, true)
	defer result.CloseStagedResults()
	must(t, err)
	view, err := readStatusOverview(env)
	must(t, err)
	if view.state != overviewWorking || view.lastCheck != "Completed just now" || view.queue != "0 pending at last check" {
		t.Fatalf("completed collection not reflected: %+v", view)
	}
	if _, err := os.Stat(filepath.Join(home, statusProgressFile)); !os.IsNotExist(err) {
		t.Fatal("collection left progress behind")
	}
}

func TestPublishedCollectionKeepsOverviewCurrent(t *testing.T) {
	t.Parallel()
	home, env, cfg, _ := overviewFixture(t)
	project := cfg.Archive.Projects[0].Root
	path := writeCodexTranscript(t, project)
	must(t, handleTestHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-overview", "cwd": project, "transcript_path": path}, env.now()))
	result, err := runOnePass(env, true)
	defer result.CloseStagedResults()
	must(t, err)
	if len(result.Published) != 1 {
		t.Fatalf("published=%d", len(result.Published))
	}
	view, err := readStatusOverview(env)
	must(t, err)
	if view.state != overviewWorking || view.uploads != "Last succeeded just now" {
		t.Fatalf("publication invalidated its summary: %+v", view)
	}
}

func TestStatusIssuesIsExplicitAndShowsGaps(t *testing.T) {
	t.Parallel()
	home, env, _, _ := overviewFixture(t)
	store, err := state.Open(home)
	must(t, err)
	must(t, store.SaveStatus(state.Status{LastScanAt: env.now(), SessionIssues: map[string]string{
		"synthetic-session": issueTranscriptSizeLimit,
	}}))
	var out strings.Builder
	if code := Run([]string{"status", "--issues"}, nil, &out, &out, env); code != 0 || !strings.Contains(out.String(), "Agent Archive · Issues") || !strings.Contains(out.String(), "synthetic-session") {
		t.Fatalf("issues exit=%d: %s", code, &out)
	}
	for _, conflict := range []string{"--verbose", "--json", "codex"} {
		if code := Run([]string{"status", "--issues", conflict}, nil, io.Discard, io.Discard, env); code != 2 {
			t.Errorf("--issues %s exit=%d", conflict, code)
		}
	}
}

func TestStatusIssuesIncludesDiscoveryFailures(t *testing.T) {
	t.Parallel()
	view := statusView{State: "Ready", Apps: []appStatus{{Name: "codex", Discovery: &discovery.Health{Enabled: true, Outcomes: map[string]int{"unsupported_producer": 2, "source_unavailable": 1}}}}}
	var out strings.Builder
	printStatusIssues(&out, view, statusScreen{})
	for _, want := range []string{"source is unavailable", "Last scan skipped 2", "unsupported producer: 2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, &out)
		}
	}
}

func BenchmarkStatusOverview(b *testing.B) {
	for _, sessions := range []int{0, 2000} {
		b.Run(fmt.Sprintf("sessions=%d", sessions), func(b *testing.B) {
			home, userHome, project := b.TempDir(), b.TempDir(), b.TempDir()
			if err := os.Chmod(home, 0700); err != nil {
				b.Fatal(err)
			}
			now := time.Now().UTC()
			cfg := pairTestConfig(now, []string{"codex"}, project)
			if err := config.Save(home, cfg); err != nil {
				b.Fatal(err)
			}
			cfg, _, err := config.Load(home)
			if err != nil {
				b.Fatal(err)
			}
			summary := statusSummary{Version: 1, ConfigurationID: statusConfigurationID(cfg), ObservedAt: now, CompletedAt: now, PendingKnown: true}
			if err := local.Write(filepath.Join(home, statusSummaryFile), summary); err != nil {
				b.Fatal(err)
			}
			// Sparse synthetic bodies model over 1 GiB of history, outside the
			// measured interval. The overview must never open these bodies.
			for i := range sessions {
				id := fmt.Sprintf("session-%d", i)
				path := filepath.Join(home, "published", id+".json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					b.Fatal(err)
				}
				f, err := os.Create(path)
				if err != nil {
					b.Fatal(err)
				}
				if err := errors.Join(f.Truncate(768<<10), f.Close()); err != nil {
					b.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(home, "registrations"), 0700); err != nil {
					b.Fatal(err)
				}
				data, err := json.Marshal(archive.SessionRegistration{ArchiveSessionID: id, Harness: archive.Harness{Name: "codex"}, ProjectID: archive.ProjectID(project), ProjectRoot: project, SessionStartedAt: now, RegisteredAt: now, AdmittedAt: now})
				if err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, "registrations", id+".json"), data, 0600); err != nil {
					b.Fatal(err)
				}
			}
			env := Env{Home: func() (string, error) { return home, nil }, UserHomeDir: func() (string, error) { return userHome, nil }, AccountHome: func() (string, error) { return userHome, nil }, Now: func() time.Time { return now }, LookupEnv: noEnv, OS: platform.Darwin, Scheduler: overviewScheduler{Scheduler: host.New(platform.Darwin, nil), inspect: func(context.Context, scheduler.Site, scheduler.Ref) scheduler.Status {
				return scheduler.Status{State: scheduler.Loaded, Defined: true}
			}}}
			if view, err := readStatusOverview(env); err != nil || view.state != overviewWorking {
				b.Fatalf("benchmark did not enter the healthy path: %+v %v", view, err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if code := runStatusCommand(nil, io.Discard, io.Discard, env); code != 0 {
					b.Fatal(code)
				}
			}
		})
	}
}

// When the fix for the overview's problem is known, the overview names that
// command on its own line rather than sending the user to status --issues.
func TestStatusOverviewNamesTheCommandThatFixesItsProblem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, home string, env *Env, cfg config.Config, summary statusSummary)
		want  string
	}{
		{"not scheduled", func(t *testing.T, _ string, env *Env, _ config.Config, _ statusSummary) {
			env.Scheduler = newFakeScheduler(t, "missing")
		}, "agent-archive setup"},
		{"another installation", func(t *testing.T, _ string, env *Env, _ config.Config, _ statusSummary) {
			env.Scheduler = newFakeScheduler(t, string(scheduler.AnotherInstallation))
		}, "agent-archive setup"},
		{"collector executable unavailable", func(t *testing.T, home string, env *Env, _ config.Config, _ statusSummary) {
			userHome, err := env.userHomeDir()
			must(t, err)
			ref := env.installation(home, userHome).ref()
			plist, err := launchd.LaunchAgent(filepath.Join(home, "missing-executable"), home, string(ref), nil)
			must(t, err)
			path := launchd.PlistPath(userSite(userHome), ref)
			must(t, os.MkdirAll(filepath.Dir(path), 0o700))
			must(t, os.WriteFile(path, plist, 0o600))
		}, "agent-archive setup --refresh"},
		{"no current summary", func(t *testing.T, home string, _ *Env, _ config.Config, _ statusSummary) {
			must(t, os.Remove(filepath.Join(home, statusSummaryFile)))
		}, "agent-archive sync"},
		{"no recent result", func(t *testing.T, home string, env *Env, _ config.Config, summary statusSummary) {
			summary.CompletedAt = env.now().Add(-2 * scanStaleAfter)
			must(t, local.Write(filepath.Join(home, statusSummaryFile), summary))
		}, "agent-archive sync"},
		{"storage access failed", func(t *testing.T, home string, env *Env, cfg config.Config, _ statusSummary) {
			must(t, local.Write(filepath.Join(home, "storage-health.json"), storageHealth{ConfigurationID: configurationID(cfg), State: "authentication_failed", CheckedAt: env.now()}))
		}, "agent-archive sync"},
		{"failed collection", func(t *testing.T, home string, _ *Env, _ config.Config, summary statusSummary) {
			summary.Problem = statusProblemCapture
			must(t, local.Write(filepath.Join(home, statusSummaryFile), summary))
		}, "agent-archive status --issues"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, env, cfg, summary := overviewFixture(t)
			tc.stage(t, home, &env, cfg, summary)
			var out bytes.Buffer
			if code := Run([]string{"status"}, nil, &out, &out, env); code != 0 {
				t.Fatalf("exit=%d: %s", code, &out)
			}
			lines := strings.Split(out.String(), "\n")
			if !slices.Contains(lines, tc.want) {
				t.Fatalf("overview lacks a line %q:\n%s", tc.want, &out)
			}
			// The issues view is still offered, after the fix.
			if i, j := slices.Index(lines, tc.want), slices.Index(lines, "agent-archive status --issues"); j < i {
				t.Errorf("overview offers status --issues before %q:\n%s", tc.want, &out)
			}
		})
	}
}
