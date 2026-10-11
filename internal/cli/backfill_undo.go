package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// runBackfillUndo implements `agent-archive backfill undo [IMPORT_ID] [--project
// DIR] [--yes] [--restore-retention]`: it removes the sessions an import
// registered from the bucket and from this machine, and excludes the projects the
// import added. Retention the import raised goes back after confirmation, or
// with --yes only when --restore-retention is given.
//
// setup.lock is held from before the plan until exit, so no setup or other
// backfill changes the configuration while the plan is shown. collector.lock
// is taken after confirming and held until exit, so no collector pass
// republishes a session mid-undo; the selection is then checked again, and
// undo stops if it grew. hooks.lock is held only to write the configuration.
func runBackfillUndo(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	fail := func(format string, args ...any) int {
		terminal.Printf(stderr, "agent-archive: backfill undo: "+format+"\n", args...)
		return 1
	}
	fs := env.newCommandFlags("backfill undo", stderr)
	project := fs.String("project", "", "only undo this project's sessions")
	yes := fs.Bool("yes", false, "skip the confirmation")
	restoreRetention := fs.Bool("restore-retention", false, "with --yes, also put back the shorter retention from before the import")
	// The ID may come before or after the flags.
	id, ok := fs.parseWithArgument(args)
	if !ok {
		return 2
	}
	if *restoreRetention && *project != "" {
		return fs.usageError("%s", "--restore-retention undoes a whole import's retention change; a --project undo leaves retention alone")
	}
	// Restoring a shorter retention deletes sessions that are not from the
	// import (hook-captured ones, other imports'). A confirmed undo shows
	// how many and asks; an unattended one (--yes) leaves retention as it
	// is unless --restore-retention asks for it.
	keepRetention := *yes && !*restoreRetention

	home, err := env.readHome()
	if err != nil {
		return fail("resolve home: %v", err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return fail("load config: %v", err)
	}
	if !found {
		return fail("%s", errNotSetUp.Error())
	}
	if refusal := undoRefusal(home, cfg); refusal != "" {
		return fail("%s", refusal)
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return fail("resolve user home: %v", err)
	}
	releaseSetup, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return fail("setup or a backfill is running; run undo again when it finishes. Nothing was changed.")
	}
	defer releaseSetup()

	batches, batch, err := selectUndoBatch(home, id)
	if err != nil {
		return fail("%v", err)
	}
	if batch == nil {
		terminal.Println(stdout, "No imports to undo.")
		return 0
	}
	return (undoCommand{
		home: home, userHome: userHome, cfg: cfg, batches: batches, batch: batch,
		project: *project, yes: *yes, keepRetention: keepRetention,
		stdin: stdin, stdout: stdout, stderr: stderr, env: env,
	}).run()
}

type undoCommand struct {
	home          string
	userHome      string
	cfg           config.Config
	batches       []backfill.Batch
	batch         *backfill.Batch
	project       string
	yes           bool
	keepRetention bool
	stdin         io.Reader
	stdout        io.Writer
	stderr        io.Writer
	env           Env
}

// run plans and confirms while runBackfillUndo holds setup.lock.
func (cmd undoCommand) run() int {
	home, userHome, cfg, batches, batch := cmd.home, cmd.userHome, cmd.cfg, cmd.batches, cmd.batch
	project, yes, keepRetention := cmd.project, cmd.yes, cmd.keepRetention
	stdin, stdout, stderr, env := cmd.stdin, cmd.stdout, cmd.stderr, cmd.env
	fail := func(format string, args ...any) int {
		terminal.Printf(stderr, "agent-archive: backfill undo: "+format+"\n", args...)
		return 1
	}
	bfEnv := env.backfillEnvironment(userHome, cfg)
	plan, err := backfill.PlanUndo(bfEnv, state.OpenReadOnly(home), cfg, batches, *batch, project)
	if err != nil {
		return fail("%v", err)
	}
	if keepRetention {
		plan = plan.WithRetentionKept()
	}
	// Nothing to do needs no confirmation, so these come before the
	// terminal requirement.
	if plan.Empty() {
		return cmd.finishEmpty(plan)
	}
	if !yes && !env.interactive(stdin) {
		return fail("%s", "confirming an undo needs a terminal. Nothing was changed. Run again with --yes to undo without asking."+env.overrideHint(stdin))
	}
	// The bucket must work before anything is confirmed, as for an import.
	// The check writes one test object and deletes it again.
	var bucket storage.ObjectStore
	if plan.Counts().Deleted > 0 {
		checkStyle := activityStyle(stdout)
		var stopCheck func()
		if checkStyle.live {
			stopCheck = checkStyle.spin(stdout, "Checking storage…").stop
		} else {
			terminal.Print(stdout, "Checking storage… ")
			stopCheck = func() {}
		}
		if bucket, err = env.openStore(cfg); err == nil {
			err = storage.VerifyAccess(context.Background(), bucket)
		}
		if err != nil {
			stopCheck()
			if checkStyle.live {
				terminal.Println(stdout, "Checking storage… failed.")
			} else {
				terminal.Println(stdout, "failed.")
			}
			terminal.Printf(stderr, "agent-archive: backfill undo: storage check failed: %v\n", err)
			if action := credentials.RecoveryAction(err); action != "" {
				terminal.Println(stderr, "agent-archive: backfill undo: "+action)
			}
			return fail("nothing was changed.")
		}
		stopCheck()
		if checkStyle.live {
			terminal.Println(stdout, "Checking storage… ready.")
		} else {
			terminal.Println(stdout, "ready.")
		}
		terminal.Println(stdout)
	}
	p := newPrompter(stdin, stdout)
	defer p.close()
	stdout = p.out
	backfill.RenderUndo(stdout, plan)
	terminal.Println(stdout)
	if !yes {
		confirmed, err := p.guidedYesNo(backfill.UndoQuestion(plan))
		if err != nil {
			return fail("%v. Nothing was changed.", err)
		}
		if !confirmed {
			terminal.Println(stdout, "Cancelled. Nothing was changed.")
			return 0
		}
	}

	return cmd.apply(plan, bfEnv, bucket)
}

// finishEmpty records configuration changes left unrecorded by an interrupted
// undo, then reports the already settled plan.
func (cmd undoCommand) finishEmpty(plan backfill.UndoPlan) int {
	batch := cmd.batch
	if len(plan.Settled.Excluded) > 0 || plan.Settled.RetentionRestored {
		batch.RecordUndone(plan.Settled)
		if err := backfill.SaveBatch(cmd.home, *batch); err != nil {
			terminal.Printf(cmd.stderr, "agent-archive: backfill undo: %v\n", err)
			return 1
		}
	}
	if cmd.project != "" {
		terminal.Printf(cmd.stdout, "No sessions from %s are left in import %s. Nothing was changed.\n", plan.ProjectDisplay(), batch.ID)
	} else {
		terminal.Printf(cmd.stdout, "Import %s has nothing left to undo. Nothing was changed.\n", batch.ID)
	}
	if r := plan.RetentionKept; r != nil {
		terminal.Printf(cmd.stdout, "Retention stays at %s. To put back the %d days from before the import, run agent-archive backfill undo %s --restore-retention\n", config.RetentionLabel(r.To), r.From, batch.ID)
	}
	return 0
}

// apply holds collector.lock through the recheck, transaction, and removal.
func (cmd undoCommand) apply(plan backfill.UndoPlan, bfEnv backfill.Environment, bucket storage.ObjectStore) int {
	home, cfg, batches, batch := cmd.home, cmd.cfg, cmd.batches, cmd.batch
	project, keepRetention := cmd.project, cmd.keepRetention
	stdout, stderr, env := cmd.stdout, cmd.stderr, cmd.env
	fail := func(format string, args ...any) int {
		terminal.Printf(stderr, "agent-archive: backfill undo: "+format+"\n", args...)
		return 1
	}
	var found bool

	stopWait := startActivity(stdout, "Waiting for collector…")
	releaseCollector, err := lockCollectorWait(home, "backfill undo", env.now(), backfillCollectorWait)
	stopWait()
	if err != nil {
		return fail("a collector pass is still running; run undo again. Nothing was changed.")
	}
	defer releaseCollector()
	// Recheck under the locks: a pass that ran during the prompt can have
	// registered a subagent, or a hook resumed a session.
	fingerprint := configFingerprint(cfg)
	if cfg, found, err = config.Load(home); err != nil {
		return fail("reload config: %v", err)
	}
	if !found {
		return fail("%s", errNotSetUp.Error())
	}
	if refusal := undoRefusal(home, cfg); refusal != "" {
		return fail("%s", refusal)
	}
	if configFingerprint(cfg) != fingerprint {
		return fail("the configuration changed while this was open; run undo again. Nothing was changed.")
	}
	confirmed := plan
	if plan, err = backfill.PlanUndo(bfEnv, state.OpenReadOnly(home), cfg, batches, *batch, project); err != nil {
		return fail("%v", err)
	}
	if keepRetention {
		plan = plan.WithRetentionKept()
	}
	if plan.Grew(confirmed) {
		return fail("the import changed while this was open; run undo again to review it. Nothing was changed.")
	}
	if err := env.checkpoint("undoing"); err != nil {
		return fail("%v", err)
	}

	if plan.Counts().Deleted > 0 && bucket == nil {
		// The recheck found a bucket delete the confirmed plan did not
		// have; Grew refuses that, so this is only a guard.
		return fail("the import changed while this was open; run undo again to review it. Nothing was changed.")
	}
	store, err := state.Open(home)
	if err != nil {
		return fail("open local store: %v", err)
	}
	now := env.now().UTC()
	// The batch is marked undone and the configuration changed before any
	// session is removed: once the projects are excluded, nothing of theirs
	// is published again, even if undo is interrupted, and an import undone
	// in part is never continued. A rerun finishes the sessions.
	changes, err := commitUndo(env, home, batch, plan, fingerprint, now)
	if err != nil {
		return fail("%v", err)
	}
	if err := env.checkpoint("undo configured"); err != nil {
		return fail("%v", err)
	}
	if len(changes.Excluded) > 0 || changes.RetentionRestored {
		batch.RecordUndone(changes)
		if err := backfill.SaveBatch(home, *batch); err != nil {
			return fail("%v. The configuration was already changed. Run undo again to remove the sessions.", err)
		}
	}
	if err := env.checkpoint("undo recorded"); err != nil {
		return fail("%v", err)
	}
	stopRemove := startActivity(stdout, "Removing imported sessions…")
	result := plan.Remove(context.Background(), store, bucket, now)
	stopRemove()
	return reportUndo(stdout, stderr, *batch, plan, changes, result)
}

// undoRefusal says why an undo cannot start now, or "". Like an import, it
// waits for a pending setup and for resume: pause suspends remote cleanup.
func undoRefusal(home string, cfg config.Config) string {
	switch {
	case setupjournal.TransactionPending(home):
		return "setup needs recovery; run agent-archive setup first. Nothing was changed."
	case cfg.Paused:
		return errPaused.Error() + ". Nothing was changed."
	}
	return ""
}

// selectUndoBatch returns every import and the one named id, or the latest
// when id is empty, or nil when there are no imports. Every batch file must
// be readable: the latest cannot be chosen past one that is not, and which
// projects an undo excludes depends on the others (see backfill.PlanUndo).
func selectUndoBatch(home, id string) ([]backfill.Batch, *backfill.Batch, error) {
	batches, err := backfill.LoadBatches(home)
	if err != nil {
		return nil, nil, fmt.Errorf("%w. Nothing was changed. Repair the file to undo any import", err)
	}
	if id == "" {
		if len(batches) == 0 {
			return nil, nil, nil
		}
		return batches, &batches[len(batches)-1], nil
	}
	for i := range batches {
		if batches[i].ID == id {
			return batches, &batches[i], nil
		}
	}
	return nil, nil, fmt.Errorf("there is no import %q. Run agent-archive backfill history to see import IDs", id)
}

// commitUndo marks the batch undone and then writes the configuration
// change, under hooks.lock, after checking that the configuration is still
// the one the plan was made from. It returns the IDs of the projects it
// excluded and whether it restored retention; the caller records them in the
// batch.
func commitUndo(env Env, home string, batch *backfill.Batch, plan backfill.UndoPlan, fingerprint string, now time.Time) (backfill.UndoChanges, error) {
	markUndone := func() error {
		batch.UndoneAt = &now
		batch.RecordKept(plan.KeepProjects)
		batch.RecordUndone(plan.Settled)
		if err := backfill.SaveBatch(home, *batch); err != nil {
			return fmt.Errorf("%w. Nothing was changed", err)
		}
		return nil
	}
	if len(plan.ExcludeProjects) == 0 && len(plan.RemoveApps) == 0 && plan.RestoreRetention == nil && len(plan.RemoveKeptOut) == 0 {
		return backfill.UndoChanges{}, markUndone()
	}
	releaseHooks, err := local.NamedLockWait(home, "hooks.lock", backfillHooksWait)
	if err != nil {
		return backfill.UndoChanges{}, errors.New("capture hooks are busy; run undo again. Nothing was changed")
	}
	defer releaseHooks()
	cfg, found, err := config.Load(home)
	if err != nil {
		return backfill.UndoChanges{}, fmt.Errorf("load config: %w", err)
	}
	if !found {
		return backfill.UndoChanges{}, errNotSetUp
	}
	if configFingerprint(cfg) != fingerprint {
		return backfill.UndoChanges{}, errors.New("the configuration changed while this was open; run undo again. Nothing was changed")
	}
	if err := markUndone(); err != nil {
		return backfill.UndoChanges{}, err
	}
	if err := env.checkpoint("undo marked"); err != nil {
		return backfill.UndoChanges{}, err
	}
	previous := cfg
	previous.Archive.Projects = slices.Clone(cfg.Archive.Projects)
	changes := plan.ApplyToConfig(&cfg)
	if err := config.ReconcileDiscovery(&cfg, previous, now); err != nil {
		return backfill.UndoChanges{}, fmt.Errorf("update discovery authorization: %w", err)
	}
	if err := config.Save(home, cfg); err != nil {
		return backfill.UndoChanges{}, fmt.Errorf("save config: %w. The import is marked undone but nothing was removed; run undo again", err)
	}
	return changes, nil
}

func reportUndo(stdout, stderr io.Writer, batch backfill.Batch, plan backfill.UndoPlan, changes backfill.UndoChanges, result backfill.UndoResult) int {
	subagents := map[string]bool{}
	for _, s := range plan.Sessions {
		if s.Registration.IsChild() {
			subagents[s.Registration.ArchiveSessionID] = true
		}
	}
	split := func(ids []string) (int, int) {
		sessions, subs := 0, 0
		for _, id := range ids {
			if subagents[id] {
				subs++
			} else {
				sessions++
			}
		}
		return sessions, subs
	}
	var parts []string
	if s, a := split(result.Deleted); s+a > 0 {
		parts = append(parts, "deleted "+backfill.SessionsAndSubagents(s, a)+" from the archive")
	}
	if s, a := split(result.Forgotten); s+a > 0 {
		parts = append(parts, "forgot "+backfill.SessionsAndSubagents(s, a)+" from a previous storage destination")
	}
	if len(changes.Excluded) > 0 {
		parts = append(parts, "excluded "+countNoun(len(changes.Excluded), "project"))
	}
	if changes.RetentionRestored && plan.RestoreRetention != nil {
		parts = append(parts, fmt.Sprintf("restored retention to %d days", plan.RestoreRetention.From))
	}
	if len(parts) > 0 {
		line := strings.Join(parts, ", ")
		terminal.Printf(stdout, "Import %s: %s.\n", batch.ID, strings.ToUpper(line[:1])+line[1:])
	}
	if len(result.Failed) == 0 {
		return 0
	}
	ids := make([]string, 0, len(result.Failed))
	for id := range result.Failed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		terminal.Printf(stderr, "agent-archive: backfill undo: session %s: %v\n", id, result.Failed[id])
	}
	terminal.Printf(stderr, "agent-archive: backfill undo: %s could not be removed and %s still registered; run agent-archive backfill undo %s again to finish.\n",
		countNoun(len(ids), "session"), isAre(len(ids)), batch.ID)
	return 1
}
