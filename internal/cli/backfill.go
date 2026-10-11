package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/cursorstore"
	"github.com/wangjohn/agent-archive/internal/gitremote"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

// backfillDay is the local day (YYYY-MM-DD) a --since or --until value
// names, in the forms parseTimeArg reads, or "" for "".
func backfillDay(value string, now time.Time) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	t, err := parseTimeArg(value, now, now.Location())
	if err != nil {
		return "", err
	}
	return t.In(now.Location()).Format("2006-01-02"), nil
}

// relativeTimeArg is a --since or --until value as typed when it is
// relative to now (an age: 30d, 12h), which names a different local day
// each day, or "" for a date, an RFC 3339 time, or nothing.
func relativeTimeArg(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, value); err == nil {
		return ""
	}
	if _, err := time.Parse("2006-01-02", value); err == nil {
		return ""
	}
	return value
}

// printInterruptedImport points at the latest import when it was
// interrupted and this run, with other options or another destination,
// would not continue it: the run starts a new import, and the interrupted
// one stays partial unless it is run again as it was.
func printInterruptedImport(out io.Writer, home string, plan backfill.Plan, cfg config.Config) {
	batches, err := backfill.LoadBatches(home)
	if err != nil || len(batches) == 0 {
		return
	}
	last := batches[len(batches)-1]
	if last.CompletedAt != nil || last.UndoneAt != nil || last.Matches(plan.BatchFilters(), cfg.DestinationID()) {
		return
	}
	if last.DestinationID != cfg.DestinationID() {
		terminal.Printf(out, "Import %s was interrupted, for a storage destination no longer configured;\nthis run starts a new import.\n", last.ID)
		return
	}
	flags, ok := last.Filters.Flags(plan, func(id string) (string, bool) {
		for _, p := range cfg.Archive.Projects {
			if p.ProjectID == id {
				return p.Root, true
			}
		}
		return "", false
	})
	if !ok {
		terminal.Printf(out, "Import %s was interrupted; this run, with other options, starts a new import.\n", last.ID)
		return
	}
	command := strings.TrimSpace("agent-archive backfill " + flags)
	terminal.Printf(out, "Import %s was interrupted; this run, with other options, starts a new\nimport. To finish %s instead, run:\n  %s\n", last.ID, last.ID, command)
}

// runBackfillCommand implements `agent-archive backfill`: it finds the
// sessions already on this machine, shows the plan, and after confirmation
// imports them (see dev/specs/backfill.md). `--dry-run [--json]` prints
// the plan and writes nothing, locally or remotely.
func runBackfillCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	// A copy of Cursor's database a killed backfill or collector left in the
	// temporary folder goes first, whatever this command is (history, undo,
	// and --dry-run included). A copy in use is never removed.
	cursorstore.RemoveStaleSnapshots()
	if len(args) > 0 && args[0] == "history" {
		return runBackfillHistory(args[1:], stdout, stderr, env)
	}
	if len(args) > 0 && args[0] == "undo" {
		return runBackfillUndo(args[1:], stdin, stdout, stderr, env)
	}
	opts, ok := parseBackfillOptions(args, stderr, env)
	if !ok {
		return 2
	}
	home, err := env.readHome()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: backfill: resolve home: %v\n", err)
		return 1
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: backfill: load config: %v\n", err)
		return 1
	}
	if !found {
		terminal.Println(stderr, "agent-archive: backfill: "+errNotSetUp.Error())
		return 1
	}
	// A dry run works while paused or while a setup transaction is pending:
	// it writes nothing. An import refuses both before it looks at anything.
	if !opts.dryRun {
		if refusal := importRefusal(home, cfg); refusal != "" {
			terminal.Println(stderr, "agent-archive: backfill: "+refusal)
			return 1
		}
		if !opts.yes && !env.interactive(stdin) {
			terminal.Println(stderr, "agent-archive: backfill: confirming an import needs a terminal. Nothing was changed. Run again with --yes to import without asking, or with --dry-run to see the plan."+env.overrideHint(stdin))
			return 1
		}
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: backfill: resolve user home: %v\n", err)
		return 1
	}

	style := styleFor(stdout)
	plan, ok := planBackfill(env, stdout, stderr, home, userHome, cfg, opts, style)
	if !ok {
		return 1
	}
	if handled, code := reportBackfillPlan(env, stdout, stderr, home, cfg, plan, opts, style); handled {
		return code
	}
	return importBackfillPlan(env, stdin, stdout, stderr, home, cfg, plan, opts)
}

type backfillCommandOptions struct {
	filters    backfill.Filters
	dryRun     bool
	jsonOut    bool
	yes        bool
	background bool
}

type backfillPlanAction uint8

const (
	backfillPlanJSON backfillPlanAction = iota
	backfillPlanDryRun
	backfillPlanEmptyImport
	backfillPlanImport
)

// chooseBackfillPlanAction decides the command path from validated options
// and the read-only plan, before any storage check or import write.
func chooseBackfillPlanAction(opts backfillCommandOptions, plan backfill.Plan) backfillPlanAction {
	switch {
	case opts.jsonOut:
		return backfillPlanJSON
	case opts.dryRun:
		return backfillPlanDryRun
	case len(plan.Imported()) == 0:
		return backfillPlanEmptyImport
	default:
		return backfillPlanImport
	}
}

// parseBackfillOptions validates the command's selection and output choices
// before reading configuration or transcripts.
func parseBackfillOptions(args []string, stderr io.Writer, env Env) (backfillCommandOptions, bool) {
	fs := env.newCommandFlags("backfill", stderr)
	var harnesses, projects, mappings stringList
	fs.Var(&harnesses, "harness", "only sessions from this app (claude, codex, cursor); repeatable")
	fs.Var(&projects, "project", "only sessions in this project directory; repeatable")
	fs.Var(&mappings, "map-project", "map exact missing OLD_CWD=CONFIGURED_ROOT for this import; repeatable")
	since := fs.String("since", "", "only sessions started on or after this local date (YYYY-MM-DD)")
	until := fs.String("until", "", "only sessions started on or before this local date (YYYY-MM-DD)")
	includeHome := fs.Bool("include-home", false, "import sessions run from the home directory")
	includeTemp := fs.Bool("include-temp", false, "import sessions run from temporary directories")
	includeRemoved := fs.Bool("include-removed", false, "import sessions retention or undo removed")
	dryRun := fs.Bool("dry-run", false, "print the plan and exit; nothing is written")
	jsonOut := fs.Bool("json", false, "with --dry-run, print the plan as JSON")
	yes := fs.Bool("yes", false, "skip the confirmation")
	background := fs.Bool("background", false, "register the sessions and let the collector upload them")
	if !fs.parseFlagsOnly(args) {
		return backfillCommandOptions{}, false
	}
	usageError := func(message string) (backfillCommandOptions, bool) {
		fs.usageError("%s", message)
		return backfillCommandOptions{}, false
	}
	// Backfill selects whole local days, so --since and --until accept the
	// same forms as list's --since and name the local day they fall on.
	sinceDay, err := backfillDay(*since, env.now())
	if err != nil {
		return usageError("--since: " + err.Error())
	}
	untilDay, err := backfillDay(*until, env.now())
	if err != nil {
		return usageError("--until: " + err.Error())
	}
	projectMappings, err := backfill.ParseProjectMappings(mappings)
	if err != nil {
		return usageError(err.Error())
	}
	filters := backfill.Filters{
		ProjectMappings: projectMappings,
		Harnesses:       harnesses, Projects: projects, Since: sinceDay, Until: untilDay,
		SinceArg: relativeTimeArg(*since), UntilArg: relativeTimeArg(*until),
		IncludeHome: *includeHome, IncludeTemp: *includeTemp, IncludeRemoved: *includeRemoved,
	}
	filters, err = filters.Canonicalize(env.agentRegistry())
	if err != nil {
		return usageError(err.Error())
	}
	if *jsonOut && !*dryRun {
		return usageError("--json applies only to --dry-run")
	}
	if *dryRun && *background {
		return usageError("--background applies only to an import, not --dry-run")
	}

	return backfillCommandOptions{filters: filters, dryRun: *dryRun, jsonOut: *jsonOut, yes: *yes, background: *background}, true
}

// planBackfill owns the planning spinner and interrupt watcher. It closes both
// before reporting an error or handing the completed plan to the next phase.
func planBackfill(env Env, stdout, stderr io.Writer, home, userHome string, cfg config.Config, opts backfillCommandOptions, style textStyle) (backfill.Plan, bool) {
	var stopLooking func()
	if !opts.jsonOut {
		label := backfill.SearchLine(opts.filters, env.agentRegistry().DiscoveryAgents())
		if style.live {
			stopLooking = style.spin(stdout, label).stop
		} else {
			terminal.Print(stdout, label+" ")
			stopLooking = func() {}
		}
	} else {
		stopLooking = func() {}
	}
	// Ctrl-C during planning cancels it, so the plan's copy of Cursor's
	// database is removed on the way out instead of left in the temporary
	// folder. A second Ctrl-C, SIGTERM, SIGHUP, or SIGQUIT quits at once, removing
	// the copy first.
	planCtx, stopPlanning := interruptibleContext(env, stderr)
	plan, err := buildBackfillPlan(planCtx, env, home, userHome, cfg, opts.filters)
	interrupted := planCtx.Err() != nil
	stopPlanning()
	stopLooking()
	if err != nil {
		if !opts.jsonOut {
			label := backfill.SearchLine(opts.filters, env.agentRegistry().DiscoveryAgents())
			switch {
			case interrupted && style.live:
				terminal.Println(stdout, label+" stopped.")
			case interrupted:
				terminal.Println(stdout, "stopped.")
			case style.live:
				terminal.Println(stdout, label+" failed.")
			default:
				terminal.Println(stdout)
			}
		}
		if interrupted {
			terminal.Println(stderr, "agent-archive: backfill: stopped. Nothing was changed.")
			return backfill.Plan{}, false
		}
		terminal.Printf(stderr, "agent-archive: backfill: %v\n", err)
		return backfill.Plan{}, false
	}
	return plan, true
}

// reportBackfillPlan handles JSON, dry-run, and empty-import paths. An empty
// import may finish an interrupted batch; a nonempty one needs a storage check.
func reportBackfillPlan(env Env, stdout, stderr io.Writer, home string, cfg config.Config, plan backfill.Plan, opts backfillCommandOptions, style textStyle) (bool, int) {
	action := chooseBackfillPlanAction(opts, plan)
	if action == backfillPlanJSON {
		if err := backfill.RenderJSON(stdout, plan); err != nil {
			terminal.Printf(stderr, "agent-archive: backfill: %v\n", err)
			return true, 1
		}
		return true, 0
	}
	if style.live {
		terminal.Printf(stdout, "%s %d found.\n", backfill.SearchLine(opts.filters, env.agentRegistry().DiscoveryAgents()), plan.Found())
	} else {
		terminal.Printf(stdout, "%d found.\n", plan.Found())
	}
	if action == backfillPlanDryRun {
		terminal.Println(stdout)
		backfill.RenderText(stdout, plan)
		terminal.Println(stdout)
		printInterruptedImport(stdout, home, plan, cfg)
		terminal.Println(stdout, "Dry run: nothing was changed.")
		return true, 0
	}
	if action == backfillPlanEmptyImport {
		terminal.Println(stdout)
		backfill.RenderText(stdout, plan)
		printInterruptedImport(stdout, home, plan, cfg)
		if err := finishInterruptedBatch(env, stdout, home, plan, cfg); err != nil {
			terminal.Printf(stderr, "agent-archive: backfill: %v\n", err)
			return true, 1
		}
		return true, 0
	}

	return false, 0
}

// importBackfillPlan checks storage and confirmation before entering the
// existing import transaction. That transaction and its locks are unchanged.
func importBackfillPlan(env Env, stdin io.Reader, stdout, stderr io.Writer, home string, cfg config.Config, plan backfill.Plan, opts backfillCommandOptions) int {
	p := newPrompter(stdin, stdout)
	defer p.close()
	stdout = p.out
	// Step 2: storage must work before anything is confirmed. The check
	// writes one test object and deletes it again.
	checkStyle := activityStyle(stdout)
	var stopCheck func()
	if checkStyle.live {
		stopCheck = checkStyle.spin(stdout, "Checking storage…").stop
	} else {
		terminal.Print(stdout, "Checking storage… ")
		stopCheck = func() {}
	}
	if err := checkStorage(env, cfg); err != nil {
		stopCheck()
		if checkStyle.live {
			terminal.Println(stdout, "Checking storage… failed.")
		} else {
			terminal.Println(stdout, "failed.")
		}
		terminal.Printf(stderr, "agent-archive: backfill: storage check failed: %v\n", err)
		if action := credentials.RecoveryAction(err); action != "" {
			terminal.Println(stderr, "agent-archive: backfill: "+action)
		}
		terminal.Println(stderr, "agent-archive: backfill: nothing was imported.")
		return 1
	}
	stopCheck()
	if checkStyle.live {
		terminal.Println(stdout, "Checking storage… ready.")
	} else {
		terminal.Println(stdout, "ready.")
	}
	terminal.Println(stdout)
	backfill.RenderText(stdout, plan)
	terminal.Println(stdout)
	printInterruptedImport(stdout, home, plan, cfg)

	// Step 3: confirm. edit raises the retention of the whole archive and
	// shows the plan again with the new deletion date.
	if !opts.yes {
		confirmed, err := confirmImport(p, stdout, &plan, cfg.RetentionDays)
		if err != nil {
			terminal.Printf(stderr, "agent-archive: backfill: %v. Nothing was changed.\n", err)
			return 1
		}
		if !confirmed {
			terminal.Println(stdout, "Cancelled. Nothing was changed.")
			return 0
		}
	}
	return importPlan(env, stdout, stderr, home, plan, configFingerprint(cfg), opts.background)
}

// setupImportSince is the lookback of setup's import, as backfill --since
// reads it: the 7 local days before setup.
const setupImportSince = "7d"

// setupImportRetryCommand is the backfill command a person runs when setup's
// import did not happen.
const setupImportRetryCommand = "agent-archive backfill --since " + setupImportSince

// importRecentSessions follows a committed setup: it imports the sessions of
// the last 7 days (backfill --since 7d) in the included projects and
// configured apps, without asking, as backfill --yes --background would:
// with the same plan, safety checks, and import record, so backfill history
// lists it and backfill undo removes it again. The background collector
// uploads them. It never adds a project, kept-out folder, or app: sessions
// whose project is not already included are left to backfill. It says how
// many older sessions the same projects hold.
//
// setup already holds setup.lock and has just checked storage. Setup is done
// whatever happens here, so a failure is reported, never returned. It does
// nothing while capture is paused, with no included project or app, or when
// backfill would refuse to import.
func importRecentSessions(p *prompter, errOut io.Writer, home, userHome string, env Env) {
	cfg, found, err := config.Load(home)
	if err != nil || !found || importRefusal(home, cfg) != "" {
		return
	}
	var roots []string
	for _, project := range cfg.Archive.Projects {
		if project.Included {
			roots = append(roots, project.Root)
		}
	}
	if len(roots) == 0 || len(cfg.Harnesses) == 0 {
		return
	}
	notImported := func(err error) {
		// Setup's import has no options to repeat; the retry is the command
		// below, so only the cause is said.
		var stopped *importStoppedError
		var registration *importRegistrationError
		switch {
		case errors.As(err, &stopped):
			err = errors.New("stopped")
		case errors.As(err, &registration):
			err = registration.err
		}
		reason := strings.TrimRight(strings.TrimSpace(err.Error()), ".")
		terminal.Printf(p.out, "Recent sessions were not imported: %s. Run %s to retry.\n", reason, p.style.cmd(setupImportRetryCommand))
	}
	since, err := backfillDay(setupImportSince, env.now())
	if err != nil {
		notImported(err)
		return
	}
	// One plan of every session in the included projects gives both the
	// recent import and the count of older ones (backfill.Plan.Recent).
	filters := backfill.Filters{Harnesses: cfg.Harnesses, Projects: roots}
	const label = "Looking for sessions from the last 7 days…"
	releasePrompts := p.suspendPrompts()
	stopLooking := func() {}
	if p.style.live {
		stopLooking = p.style.spin(p.out, label).stop
	}
	// Ctrl-C while planning stops the import, never setup.
	planCtx, stopPlanning := interruptibleContext(env, errOut)
	full, err := buildBackfillPlan(planCtx, env, home, userHome, cfg, filters)
	interrupted := planCtx.Err() != nil
	stopPlanning()
	stopLooking()
	releasePrompts()
	if interrupted {
		terminal.Println(p.out, "Stopped looking for recent sessions. Setup is complete. Import them later with "+p.style.cmd(setupImportRetryCommand)+".")
		return
	}
	if err != nil {
		notImported(err)
		return
	}
	plan, older, err := full.Recent(cfg, since, setupImportSince)
	if err != nil {
		notImported(err)
		return
	}
	if len(plan.Imported()) > 0 {
		reg, err := registerImportLocked(env, p.out, errOut, home, plan, configFingerprint(cfg), setupImportCollectorWait)
		reg.release()
		if err != nil {
			notImported(err)
			return
		}
		if apps := registeredByApp(home, reg.batch.ID); len(reg.result.Sessions) > 0 {
			terminal.Printf(p.out, "Imported %s from the last 7 days%s. Uploading in the background.\n", countNoun(len(reg.result.Sessions), "session"), apps)
		}
		printChangedAtAdmission(p.out, reg.changed, p.style.cmd(setupImportRetryCommand))
	}
	if older > 0 {
		them := "them"
		if older == 1 {
			them = "it"
		}
		terminal.Printf(p.out, "%s: run %s to import %s.\n", countNoun(older, "older session"), p.style.cmd("agent-archive backfill"), them)
	}
}

// registeredByApp is " (Codex 2, Claude Code 1)": the sessions import
// batchID registered, by app, most first; "" when the store cannot say.
func registeredByApp(home, batchID string) string {
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil {
		return ""
	}
	counts := map[string]int{}
	for _, reg := range regs {
		if reg.InBatch(batchID) && reg.ParentSessionID == "" {
			counts[appName(reg.Harness.Name)]++
		}
	}
	if len(counts) == 0 {
		return ""
	}
	apps := make([]string, 0, len(counts))
	for app := range counts {
		apps = append(apps, app)
	}
	slices.SortFunc(apps, func(a, b string) int {
		if counts[a] != counts[b] {
			return counts[b] - counts[a]
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, len(apps))
	for i, app := range apps {
		parts[i] = fmt.Sprintf("%s %d", app, counts[app])
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// interruptibleContext returns a context that the first Ctrl-C cancels,
// saying on out that it is stopping. A second Ctrl-C, or SIGTERM, SIGHUP or SIGQUIT
// at any point, ends the process at once, after removing this process's
// copies of Cursor's database (see watchSignals). stop ends the watch and
// waits for it; it is called once.
func interruptibleContext(env Env, out io.Writer) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	watch := watchSignals(env, out, "agent-archive: backfill: stopping; press Ctrl-C again to quit.", cancel)
	return ctx, func() {
		watch.release()
		cancel()
	}
}

// exitOnSignal ends the process on a signal that stops backfill at once: a
// second Ctrl-C, or SIGTERM, SIGHUP or SIGQUIT. It first removes the copies of
// Cursor's database this process made, which the Readers holding them would
// otherwise never close: a copy of every chat left in the temporary folder
// until a later sweep. The exit status is the shell's for the signal. A test
// sets Env.exitOnSignal instead.
func exitOnSignal(sig os.Signal) {
	exitAfterSignal(sig, cursorstore.RemoveOwnSnapshots, os.Exit)
}

// exitAfterSignal is exitOnSignal with the snapshot removal and the exit
// passed in, so a test can check their order and the exit status.
func exitAfterSignal(sig os.Signal, removeOwnSnapshots func(), exit func(code int)) {
	removeOwnSnapshots()
	code := 1
	if s, ok := sig.(syscall.Signal); ok {
		code = 128 + int(s)
	}
	exit(code)
}

// signalWatch watches, while backfill works, for the signals env.interrupts
// delivers: Ctrl-C, SIGTERM, SIGHUP, and SIGQUIT. The first Ctrl-C calls onFirst and
// says so on out, and the work stops at its next safe point. A second
// Ctrl-C, or a SIGTERM, SIGHUP or SIGQUIT (sent by a closing terminal or a process
// manager, which will not wait), calls exitOnSignal.
type signalWatch struct {
	stop   func()
	done   chan struct{}
	exited chan struct{}
	seen   atomic.Bool
}

func watchSignals(env Env, out io.Writer, message string, onFirst func()) *signalWatch {
	signals, stop := env.interrupts()
	exit := exitOnSignal
	if env.exitOnSignal != nil {
		exit = env.exitOnSignal
	}
	w := &signalWatch{stop: releaseOnce(stop), done: make(chan struct{}), exited: make(chan struct{})}
	handle := func(sig os.Signal) {
		if sig == os.Interrupt && w.seen.CompareAndSwap(false, true) {
			onFirst()
			terminal.Println(out, message)
			return
		}
		w.seen.Store(true)
		exit(sig)
	}
	// A signal already waiting is handled before the work starts.
	select {
	case sig := <-signals:
		handle(sig)
	default:
	}
	go func() {
		defer close(w.exited)
		for {
			select {
			case sig := <-signals:
				handle(sig)
			case <-w.done:
				return
			}
		}
	}()
	return w
}

// requested reports whether a stop was asked for. It never blocks.
func (w *signalWatch) requested() bool { return w.seen.Load() }

// release ends the watch and waits for it, so nothing is written after the
// command returns. It is called once.
func (w *signalWatch) release() {
	close(w.done)
	<-w.exited
	w.stop()
}

// importRefusal says why an import cannot start now, or "".
func importRefusal(home string, cfg config.Config) string {
	switch {
	case setupjournal.TransactionPending(home):
		return "setup needs recovery; run agent-archive setup first. Nothing was changed."
	case cfg.Paused:
		return errPaused.Error() + ". Nothing was changed."
	case !cfg.Archive.Enabled:
		return "integrations are not installed; run agent-archive setup to reinstall. Nothing was changed."
	}
	return ""
}

// checkStorage runs the setup round trip against the configured bucket.
func checkStorage(env Env, cfg config.Config) error {
	store, err := env.openStore(cfg)
	if err != nil {
		return err
	}
	return storage.VerifyAccess(context.Background(), store)
}

// confirmImport asks `Import N sessions from M projects? [y/N/edit]`. The
// default is No. edit asks for a longer retention period, which it stores
// in plan, and shows the plan again. Retention applies to the whole archive,
// so edit never goes below configured, the retention set now, and does
// nothing while retention is off: either would delete sessions already
// archived (backfill.ApplyToConfig refuses it too).
func confirmImport(p *prompter, out io.Writer, plan *backfill.Plan, configured int) (bool, error) {
	for {
		answer, err := p.guidedChoice(promptModel{Question: fmt.Sprintf("Import %s from %s?", countNoun(len(plan.Imported()), "session"), countNoun(len(plan.Projects()), "project")), Default: "no", Primary: []option{{"yes", "Import these sessions"}, {"no", "Skip for now"}}, Secondary: []actionOption{{"edit", "e", "Edit retention"}}})
		if err != nil {
			return false, err
		}
		switch strings.ToLower(answer) {
		case "", "n", "no":
			return false, nil
		case "y", "yes":
			return true, nil
		case "e", "edit":
			if configured <= 0 {
				terminal.Println(out, "Retention is off, so no session is deleted; there is nothing to keep longer.")
				continue
			}
			guidedExplanation(out, "Edit retention", "Retention applies to every session in the archive, not only these.", fmt.Sprintf("Here it can only be raised from %d days, and undo puts %d back. Shorten it in setup.", configured, configured))
			days, err := p.guidedRetention(plan.RetentionDays)
			if err != nil {
				return false, err
			}
			if days < configured {
				// A shorter retention deletes sessions already archived,
				// hook-captured ones too, which an import never does.
				terminal.Printf(out, "Retention stays at %d days: a shorter period would delete sessions\nalready in the archive. Shorten it in setup.\n", plan.RetentionDays)
				continue
			}
			plan.RetentionDays = days
			backfill.RenderText(out, *plan)
			terminal.Println(out)
		default:
			terminal.Println(out, "Please enter y, n, or edit.")
		}
	}
}

// countNoun and isAre are backfill's, so the CLI and the plans it prints
// count alike.
var (
	countNoun = backfill.CountNoun
	isAre     = backfill.IsAre
)

// configFingerprint identifies the configuration a plan was made from. The
// collector refreshes bucket privacy evidence in place, which is evidence
// rather than a setting, so it is left out, as setup leaves it out when it
// checks for concurrent changes.
func configFingerprint(cfg config.Config) string {
	data, _ := json.Marshal(withoutBucketPrivacy(cfg))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// backfillTempDirs are the temporary folders backfill skips sessions from,
// and setup will not offer as a project: the operating system's defaults
// and $TMPDIR, or the list a test sets.
func (e Env) backfillTempDirs() []string {
	if e.BackfillTempDirs != nil {
		return e.BackfillTempDirs
	}
	temps := backfill.Environment{OS: e.OS}.DefaultTempDirs()
	if tmp, ok := e.lookupEnv("TMPDIR"); ok && strings.TrimSpace(tmp) != "" {
		temps = append(temps, strings.TrimSpace(tmp))
	}
	return temps
}

// backfillEnvironment is what planning reads: the user's home, the
// temporary directories, and the clock. Files are read from the real file
// system, and Cursor's database is opened read-only to count the chats only
// it holds.
func (e Env) backfillEnvironment(userHome string, cfg config.Config) backfill.Environment {
	dirs := e.nativeSessionDirectories(userHome, cfg)
	observer := &gitremote.IdentityObserver{}
	env := backfill.Environment{
		RepositoryIdentity:        observer.Lookup,
		RepositoryIdentityCurrent: gitremote.ProjectIdentityCurrent,
		Home:                      userHome, NativeDirectories: dirs, Sources: e.agentRegistry(), Discovery: e.agentRegistry(), DatabaseCatalogs: e.agentRegistry(), NativePaths: e.agentRegistry(), Worktrees: e.agentRegistry(), Workspaces: e.agentRegistry(), Children: e.agentRegistry(), Imports: e.agentRegistry(),
		TempDirs: e.backfillTempDirs(), Now: e.now, OS: e.OS,
		// XDG_CONFIG_HOME places Cursor's data folder off macOS.
		Getenv: e.getenv,
	}
	env.CursorDatabase = backfill.CursorDatabaseReaderFor(env)
	env.CursorRecoveryDatabase = backfill.CursorRecoveryDatabaseReaderFor(env)
	return env
}

// nativeSessionDirectories are the declared configuration locations for discovery
// in, resolved as setup resolves their hook files (hooks.ResolveFiles): the
// default ~/.claude and ~/.codex, the folders CLAUDE_CONFIG_DIR and
// CODEX_HOME name in this command's environment, and those setup recorded
// installing hooks into (config.HookFiles), which a shell without the
// variables still finds. A session found in two of them is imported once.
func (e Env) nativeSessionDirectories(userHome string, cfg config.Config) map[string][]string {
	out := map[string][]string{}
	add := func(dirs []string, dir string) []string {
		if dir == "" || !filepath.IsAbs(dir) || slices.Contains(dirs, filepath.Clean(dir)) {
			return dirs
		}
		return append(dirs, filepath.Clean(dir))
	}
	observed := e.hookFiles(userHome)
	for _, name := range e.agentRegistry().DiscoveryAgents() {
		p, _ := e.agentRegistry().LookupDiscovery(name)
		var dirs []string
		for _, dir := range p.DefaultDirectories(userHome) {
			dirs = add(dirs, dir)
		}
		for _, files := range []hooks.Files{observed, cfg.HookFiles} {
			if path := files[name]; path != "" {
				dirs = add(dirs, filepath.Dir(path))
			}
		}
		out[name] = dirs
	}
	return out
}

// archiveState answers backfill.ArchiveState from this machine's local store
// and configuration.
type archiveState struct {
	store *state.Store
	cfg   config.Config
}

func newArchiveState(home string, cfg config.Config) archiveState {
	return archiveState{store: state.OpenReadOnly(home), cfg: cfg}
}

// Classify reports already_archived when the native session has a
// registration the configuration accepts, and registered_not_admitted when it
// has one the configuration no longer accepts. An index entry without a
// registration does not count, as for hooks (capture.HasRegistration).
func (s archiveState) Classify(harness, nativeSessionID string) (backfill.SkipReason, error) {
	archiveID, found, err := s.store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(harness)), NativeID: nativeSessionID})
	if err != nil {
		return "", err
	}
	if found {
		reg, found, err := s.store.LoadRegistration(archiveID)
		if err != nil {
			return "", err
		}
		if found {
			if s.cfg.AcceptSession(reg) {
				return backfill.SkipAlreadyArchived, nil
			}
			return backfill.SkipRegisteredNotAdmitted, nil
		}
	}
	// Retention and undo leave a removal record when they forget a session,
	// so a transcript still on disk is not imported again.
	record, found, err := s.store.Removal(harness, nativeSessionID)
	if err != nil || !found {
		return "", err
	}
	if record.Reason == state.RemovalReasonUndo {
		return backfill.SkipRemovedByUndo, nil
	}
	return backfill.SkipRemovedByRetention, nil
}
