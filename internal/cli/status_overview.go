package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

const statusLivenessBudget = 150 * time.Millisecond

type overviewState string

const (
	overviewWorking     overviewState = "Working"
	overviewProcessing  overviewState = "Processing"
	overviewPaused      overviewState = "Paused"
	overviewAttention   overviewState = "Needs attention"
	overviewUnknown     overviewState = "Status unavailable"
	overviewNotSetUp    overviewState = "Not set up"
	overviewUninstalled overviewState = "Not installed"
)

type overviewAgent struct {
	name       string
	enabled    bool
	detected   bool
	observedAt time.Time
}

type statusOverview struct {
	state          overviewState
	configured     bool
	background     string
	lastCheck      string
	uploads        string
	queue          string
	problem        string
	next           string
	agents         []overviewAgent
	detectionKnown bool
	notices        int
}

func runStatusOverview(stdout, stderr io.Writer, env Env) int {
	view, err := readStatusOverview(env)
	if err != nil {
		terminal.Printf(stderr, "Cannot read archive status: %v\n", err)
		terminal.Println(stderr, "agent-archive status --verbose")
		return 1
	}
	printStatusOverview(stdout, view, env.now())
	return 0
}

// readStatusOverview never opens status.json or any per-session directory.
// Every advisory file has a fixed byte ceiling; unavailable evidence stays unknown.
func readStatusOverview(env Env) (view statusOverview, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), statusLivenessBudget)
	defer cancel()
	view = statusOverview{state: overviewNotSetUp, next: "agent-archive setup"}
	home, err := env.readHome()
	if err != nil {
		return view, err
	}
	// Before setup, an existing empty parent need not have archive permissions.
	if _, err := os.Lstat(filepath.Join(home, "config.json")); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(filepath.Join(home, "setup-transaction.json")); !errors.Is(err, os.ErrNotExist) {
			view.state, view.problem = overviewAttention, "Setup needs recovery."
		}
		return view, nil
	}
	root, err := local.OpenRootedHome(home)
	if errors.Is(err, os.ErrNotExist) {
		return view, nil
	}
	if err != nil {
		return view, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	cfg, found, _, err := config.LoadRootedObservedBounded(root, statusConfigLimit)
	if err != nil {
		return view, err
	}
	if !found {
		view.readSetupRecovery(root.Root)
		return view, nil
	}
	view.configured, view.state, view.next = true, overviewUnknown, overviewIssues
	view.background, view.lastCheck = "Unknown", "No saved check yet"
	view.uploads, view.queue = "No saved upload yet", "Unknown"
	now, id := env.now(), statusConfigurationID(cfg)
	var summary statusSummary
	saved, readErr := readStatusFile(root.Root, statusSummaryFile, statusFileLimit, &summary)
	valid := readErr == nil && saved && validStatusSummary(summary, id, now)
	if valid {
		view.applySummary(summary, now)
	} else {
		view.problem, view.next = "No current summary; the next collection records one.", overviewSync
	}
	view.readAgents(root.Root, cfg, env, now)
	view.readLiveness(ctx, root.Root, home, cfg, env, now, id)
	view.readHealth(root.Root, cfg, now)
	view.applyConfiguration(root.Root, cfg)
	return view, root.Check()
}

func (view *statusOverview) applySummary(summary statusSummary, now time.Time) {
	view.notices = summary.Notices
	if !summary.CompletedAt.IsZero() {
		view.lastCheck = "Completed " + relativeAge(now, summary.CompletedAt)
	}
	if !summary.LastPublishedAt.IsZero() && !summary.LastPublishedAt.After(now) {
		view.uploads = "Last succeeded " + relativeAge(now, summary.LastPublishedAt)
	}
	if summary.PendingKnown {
		view.queue = fmt.Sprintf("%d pending at last check", summary.Pending)
	}
	if summary.CompletedAt.IsZero() || now.Sub(summary.CompletedAt) > scanStaleAfter {
		view.problem, view.next = "No recent collection result; current health is unknown.", overviewSync
		if summary.PendingKnown {
			view.queue = "Unknown now; " + view.queue
		}
	} else {
		view.state = overviewWorking
	}
	if summary.Problem != statusProblemNone {
		view.attention(statusProblemText(summary.Problem), overviewIssues)
	} else if summary.VerificationFailures > 0 {
		view.attention("Some uploads have unresolved read-back failures.", overviewIssues)
	}
}

// The overview's next steps: the issues view, when the fix depends on what
// it shows, and sync, which runs a collection and prints its own errors.
const (
	overviewIssues = "agent-archive status --issues"
	overviewSync   = "agent-archive sync"
)

// attention records a problem that needs the user and the command that
// fixes it, replacing any earlier problem and its command.
func (view *statusOverview) attention(problem, next string) {
	view.state, view.problem, view.next = overviewAttention, problem, next
}

func (view *statusOverview) readAgents(root *os.Root, cfg config.Config, env Env, now time.Time) {
	var discoveries map[string]applicationDiscovery
	found, err := readStatusFile(root, "application-versions.json", statusFileLimit, &discoveries)
	view.detectionKnown = found && err == nil
	for _, name := range env.setupNames() {
		discovery, observed := discoveries[name]
		enabled := cfg.Archive.Enabled && slices.Contains(cfg.Harnesses, name)
		detected := view.detectionKnown && observed && discovery.Installed && !discovery.ObservedAt.IsZero() && !discovery.ObservedAt.After(now)
		if !detected && !enabled {
			continue
		}
		view.agents = append(view.agents, overviewAgent{name: name, enabled: enabled, detected: detected, observedAt: discovery.ObservedAt})
	}
}

func (view *statusOverview) readLiveness(ctx context.Context, root *os.Root, home string, cfg config.Config, env Env, now time.Time, id string) {
	userHome, err := env.userHomeDir()
	if err != nil {
		view.state, view.background = overviewUnknown, "Could not inspect background collection"
		return
	}
	sched := env.Scheduler
	if sched == nil {
		sched = env.namedScheduler(cfg.BackgroundBackend)
	}
	job := inspectStatusJob(ctx, home, userHome, env, sched)
	switch job.State {
	case scheduler.Loaded:
		view.background = "On · idle between checks"
	case scheduler.Running:
		view.background = "On · starting collection"
	case scheduler.Missing:
		view.background = "Not scheduled"
		view.attention("Background collection is not scheduled.", "agent-archive setup")
	case scheduler.AnotherInstallation:
		view.background = "Another installation"
		view.attention("The background job belongs to another installation.", "agent-archive setup")
	case scheduler.Unknown:
		view.background = "Could not confirm within the status budget"
		if view.state != overviewAttention {
			view.state = overviewUnknown
		}
	}
	var record collectorLockRecord
	found, recordErr := readStatusFile(root, collectorLockRecordName, 4096, &record)
	held, lockErr := statusCollectorLockHeld(root)
	if found && recordErr == nil && lockErr == nil && held && processAlive(record.PID) && !record.Since.IsZero() && !record.Since.After(now) {
		switch collectorHolder(record.Holder) {
		case collectorHolderLegacyCollect, collectorHolderSync, collectorHolderScheduled, collectorHolderBackfillUpload:
			view.background = "Collection active"
			if view.state == overviewWorking || view.state == overviewUnknown {
				view.state = overviewProcessing
			}
			view.readProgress(root, record, now, id)
			if now.Sub(record.Since) > collectLockStuckAfter {
				view.attention("Collection has exceeded its expected lifetime.", overviewIssues)
			}
		default:
			view.background = "Waiting for another operation"
		}
	} else if lockErr != nil || held {
		view.background = "Collection activity unavailable"
		if view.state != overviewAttention {
			view.state = overviewUnknown
		}
	}
	if job.Program != "" && executableProblem(job.Program) != "" {
		view.attention("The installed collector executable is unavailable.", "agent-archive setup --refresh")
	}
	if job.DefinitionErr != nil || len(job.Degraded) > 0 {
		view.attention("Background collection needs attention.", overviewIssues)
	}
}

// inspectStatusJob gives scheduler probes and legacy-label discovery one shared
// deadline. A stalled manager or definition cannot hold the overview open.
func inspectStatusJob(ctx context.Context, home, userHome string, env Env, sched scheduler.Scheduler) scheduler.Status {
	result := make(chan scheduler.Status, 1)
	go func() {
		in := env.installation(home, userHome)
		in.sched = func() scheduler.Scheduler { return sched }
		site, ref := userSite(userHome), in.ref()
		job := sched.Inspect(ctx, site, ref)
		if !job.Defined && ctx.Err() == nil {
			jobs, _ := sched.Installed(ctx, site, in.schedulerInstallation())
			for _, candidate := range jobs {
				if candidate.Alias == scheduler.EarlierLabel {
					job = sched.Inspect(ctx, site, candidate.Ref)
					break
				}
			}
		}
		result <- job
	}()
	select {
	case job := <-result:
		if ctx.Err() == nil {
			return job
		}
	case <-ctx.Done():
	}
	return scheduler.Status{State: scheduler.Unknown}
}

// statusCollectorLockHeld observes an existing lock without creating a file or
// waiting for the collector. An unreadable lock is uncertainty, not idleness.
func statusCollectorLockHeld(root *os.Root) (held bool, err error) {
	f, err := root.OpenFile("collector.lock", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false, errors.Join(errors.New("collector lock is not a regular file"), err)
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func (view *statusOverview) readProgress(root *os.Root, record collectorLockRecord, now time.Time, id string) {
	var progress statusProgress
	found, err := readStatusFile(root, statusProgressFile, statusFileLimit, &progress)
	if id == "" || err != nil || !found || progress.Version != 1 || progress.ConfigurationID != id || progress.PID != record.PID || !progress.StartedAt.Equal(record.Since) || progress.UpdatedAt.Before(record.Since) || progress.UpdatedAt.After(now) || progress.Processed < 0 || progress.Published < 0 || progress.Published > progress.Processed {
		return
	}
	switch progress.Phase {
	case statusPreparing:
		view.background = "On · checking sources"
	case statusCollecting:
		view.background = fmt.Sprintf("On · processing · %d sessions completed this pass", progress.Processed)
	case statusVerifying:
		view.background = "On · verifying uploads"
	case statusFinishing:
		view.background = "On · finishing collection"
	default:
		return
	}
	view.background += " · progress " + relativeAge(now, progress.UpdatedAt)
}

func (view *statusOverview) readHealth(root *os.Root, cfg config.Config, now time.Time) {
	var health storageHealth
	found, err := readStatusFile(root, "storage-health.json", statusFileLimit, &health)
	if found && err == nil && health.ConfigurationID == configurationID(cfg) && !health.CheckedAt.After(now) {
		//lint:ignore LV1001 storageHealth.State is an untyped legacy string shared with collection and verification
		switch health.State {
		case "credentials_unavailable", "credentials_expired", "authentication_failed", "storage_unavailable":
			view.attention("Storage access failed on the last attempt.", overviewSync)
			view.uploads = "Access failed · " + view.uploads
		}
	}
	if currentBucketPrivacy(cfg, now).State == "public_or_risky" {
		view.attention("The saved storage check found public or risky access.", overviewIssues)
	}
}

func (view *statusOverview) applyConfiguration(root *os.Root, cfg config.Config) {
	if len(cfg.Harnesses) == 0 {
		view.attention("No coding agents are enabled for archiving.", overviewIssues)
	}
	if cfg.Paused {
		view.state, view.background, view.next = overviewPaused, "Paused", "agent-archive resume"
		view.problem = "Collection is paused."
	}
	if cfg.InstalledExecutable != "" && executableProblem(cfg.InstalledExecutable) != "" {
		view.attention("The installed archive executable is unavailable.", "agent-archive setup --refresh")
	}
	if !cfg.Archive.Enabled {
		view.state, view.problem, view.next = overviewUninstalled, "Local archive data is retained.", "agent-archive setup"
	}
	view.readSetupRecovery(root)
}

func (view *statusOverview) readSetupRecovery(root *os.Root) {
	if _, err := root.Lstat("setup-transaction.json"); !errors.Is(err, os.ErrNotExist) {
		view.state, view.problem, view.next = overviewAttention, "Setup needs recovery.", "agent-archive setup"
	}
}

func printStatusOverview(out io.Writer, view statusOverview, now time.Time) {
	s := styleFor(out)
	terminal.Printf(out, "%s  %s\n", s.bold("Agent Archive"), s.bold(string(view.state)))
	if view.configured {
		sc := statusScreen{style: s, now: now}
		terminal.Println(out)
		sc.printRows(out, []statusRow{
			{cells: []string{"Background", view.background}},
			{cells: []string{"Last check", view.lastCheck}},
			{cells: []string{"Uploads", view.uploads}},
			{cells: []string{"Queue", view.queue}},
		})
		printOverviewAgents(out, view, sc)
	}
	if view.problem != "" {
		terminal.Printf(out, "\n%s\n", view.problem)
	}
	if view.notices > 0 {
		terminal.Printf(out, "\n%d recorded notices; see issues.\n", view.notices)
	}
	terminal.Println(out)
	if view.state != overviewWorking && view.state != overviewProcessing {
		terminal.Println(out, s.cmd(view.next))
	}
	if view.configured {
		if view.next != overviewIssues || view.state == overviewWorking || view.state == overviewProcessing {
			terminal.Println(out, s.cmd("agent-archive status --issues"))
		}
		terminal.Println(out, s.cmd("agent-archive status --verbose"))
	}
}

func printOverviewAgents(out io.Writer, view statusOverview, sc statusScreen) {
	terminal.Printf(out, "\n%s\n", sc.style.bold("Detected agents"))
	if len(view.agents) == 0 {
		text := "None detected at the last observation"
		if !view.detectionKnown {
			text = "Detection unavailable; setup records installed agents"
		}
		terminal.Printf(out, "  %s\n", text)
		return
	}
	var rows []statusRow
	for _, agent := range view.agents {
		archiving := "Archiving off"
		if agent.enabled {
			archiving = "Archiving enabled"
		}
		detection := "detection unavailable"
		if agent.detected {
			detection = "detected " + relativeAge(sc.now, agent.observedAt)
		}
		rows = append(rows, statusRow{cells: []string{appName(agent.name), archiving}, detail: detection})
	}
	sc.printRows(out, rows)
}
