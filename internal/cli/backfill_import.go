package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// checkpoint calls Env.backfillCheckpoint, a test's stand-in for a crash at
// step; it does nothing in a real run.
func (e Env) checkpoint(step string) error {
	if e.backfillCheckpoint == nil {
		return nil
	}
	return e.backfillCheckpoint(step)
}

// Lock waits. A collector pass can take a while; hooks hold hooks.lock for
// milliseconds.
const (
	backfillCollectorWait = 2 * time.Minute
	// setupImportCollectorWait is setup's: setup reloads the background
	// job, which often starts a collector pass just as setup finishes, and
	// setup should not sit behind it for minutes. Its import is retried
	// with backfill.
	setupImportCollectorWait = 20 * time.Second
	backfillHooksWait        = 5 * time.Second
)

// importPlan runs steps 4 to 6 of a confirmed import: it commits the
// configuration, registers the plan's sessions, and uploads them unless
// background is set. setup.lock is held until it returns.
func importPlan(env Env, stdout, stderr io.Writer, home string, plan backfill.Plan, fingerprint string, background bool) int {
	fail := func(format string, args ...any) int {
		terminal.Printf(stderr, "agent-archive: backfill: "+format+"\n", args...)
		return 1
	}
	releaseSetup, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return fail("setup or another backfill is running; run backfill again when it finishes. Nothing was changed.")
	}
	defer releaseSetup()
	return importPlanLocked(env, stdout, stderr, home, plan, fingerprint, background)
}

// importPlanLocked is importPlan for a caller that already holds setup.lock.
func importPlanLocked(env Env, stdout, stderr io.Writer, home string, plan backfill.Plan, fingerprint string, background bool) int {
	fail := func(format string, args ...any) int {
		terminal.Printf(stderr, "agent-archive: backfill: "+format+"\n", args...)
		return 1
	}
	reg, err := registerImportLocked(env, stdout, stderr, home, plan, fingerprint, backfillCollectorWait)
	defer reg.release()
	if errors.Is(err, errCollectorBusy) {
		return fail("%v; run backfill again. Nothing was changed", err)
	}
	var stopped *importStoppedError
	if errors.As(err, &stopped) {
		terminal.Printf(reg.out, "Stopped. %s registered as import %s; run agent-archive backfill again with the same options to finish it.\n", countNoun(stopped.sessions, "session"), stopped.batchID)
		return 1
	}
	if err != nil {
		return fail("%v", err)
	}
	printRegistered(reg.out, reg.batch.ID, reg.added, reg.result)
	printChangedAtAdmission(reg.out, reg.changed, "backfill again with the same options")

	if background {
		terminal.Println(reg.out, "The background collector uploads them. Run agent-archive status to follow it.")
		printImportHints(reg.out, reg.batch.ID)
		return 0
	}
	// Step 6: upload.
	if err := env.checkpoint("uploading"); err != nil {
		return fail("%v", err)
	}
	return uploadImport(env, reg.out, stderr, home, reg.batch.ID, reg.plan, reg.interrupt, &reg.activity)
}

// registeredImport is a committed, completely registered import, with the
// Ctrl-C watch that stays on through its upload.
type registeredImport struct {
	batch  backfill.Batch
	added  int
	result backfill.RegistrationResult
	// plan is what was imported: the confirmed plan without the sessions
	// whose evidence changed before the configuration was committed, which
	// changed counts (backfill.Plan.AdmitRecovery).
	plan    backfill.Plan
	changed int
	// out is the caller's writer, serialized against the watch's message.
	out       io.Writer
	interrupt *signalWatch
	activity  activityStop
	releases  []func()
}

// release ends the watch and drops the locks registration held. It is
// called once, whatever registerImportLocked returned.
func (r *registeredImport) release() {
	for i := len(r.releases) - 1; i >= 0; i-- {
		r.releases[i]()
	}
	r.releases = nil
}

// errCollectorBusy is an import that gave up waiting for a collector pass
// before changing anything.
var errCollectorBusy = errors.New("a collector pass is still running")

// importStoppedError is a registration Ctrl-C stopped between holds: the
// sessions registered before it are recorded in the import, which a run
// with the same options finishes.
type importStoppedError struct {
	batchID  string
	sessions int
}

func (e *importStoppedError) Error() string {
	return fmt.Sprintf("stopped after %s registered as import %s", countNoun(e.sessions, "session"), e.batchID)
}

// importRegistrationError is a registration that failed between holds,
// after registering sessions: the import record keeps them, and a run with
// the same options finishes it.
type importRegistrationError struct {
	err        error
	registered int
}

func (e *importRegistrationError) Error() string {
	return fmt.Sprintf("%v. %s registered before this; run agent-archive backfill again with the same options to finish.", e.err, countNoun(e.registered, "session"))
}

func (e *importRegistrationError) Unwrap() error { return e.err }

// registerImportLocked is steps 4 and 5 of a confirmed import, for a caller
// that holds setup.lock: it commits the configuration and registers the
// plan's sessions, then marks the import complete. It prints only spinners;
// the caller reports the result, and uploads or leaves that to the
// background collector. It waits up to collectorWait for a collector pass
// to finish, and returns errCollectorBusy when one is still running. The
// returned import's release is always called.
func registerImportLocked(env Env, stdout, stderr io.Writer, home string, plan backfill.Plan, fingerprint string, collectorWait time.Duration) (*registeredImport, error) {
	reg := &registeredImport{out: stdout}
	confirmationCtx, stopConfirmation := importConfirmationContext(env, stderr, plan)
	stopConfirmation = releaseOnce(stopConfirmation)
	reg.releases = append(reg.releases, stopConfirmation)
	// Step 4: commit the configuration, under collector.lock and hooks.lock.
	stopWait := startActivity(stdout, "Waiting for collector…")
	if env.importCollectorWait != nil {
		collectorWait = env.importCollectorWait(collectorWait)
	}
	releaseCollector, err := lockCollectorWait(home, "backfill import", env.now(), collectorWait)
	stopWait()
	if err != nil {
		return reg, errCollectorBusy
	}
	// collector.lock stays held through registration, so no pass runs
	// between a session's subagent candidates and its registration.
	releaseCollector = releaseOnce(releaseCollector)
	reg.releases = append(reg.releases, releaseCollector)
	// Waiting for the collector has its own allowance. Start the evidence
	// deadline only after that wait, retaining signal cancellation throughout.
	confirmationCtx, cancelConfirmation := context.WithTimeout(confirmationCtx, 30*time.Second)
	reg.releases = append(reg.releases, cancelConfirmation)
	commit, err := commitImport(confirmationCtx, env, home, plan, fingerprint)
	if err != nil {
		return reg, err
	}
	batch, admittedAt, plan := commit.batch, commit.admittedAt, commit.plan
	reg.added, reg.plan, reg.changed = commit.added, commit.plan, commit.changed
	stopConfirmation()
	if err := env.checkpoint("committed"); err != nil {
		return reg, err
	}

	// Ctrl-C from here on stops between registration holds, or before the
	// next session uploads. A second one, or SIGTERM, SIGHUP or SIGQUIT, quits at
	// once, after removing any copy of Cursor's database this process made.
	stdout = &lockedWriter{w: stdout}
	reg.out = stdout
	registrationCtx, cancelRegistration := context.WithCancel(context.Background())
	reg.releases = append(reg.releases, cancelRegistration)
	interrupt := watchSignals(env, stdout, "Stopping after the current session; press Ctrl-C again to quit.", func() { cancelRegistration(); reg.activity.invoke() })
	reg.interrupt = interrupt
	reg.releases = append(reg.releases, interrupt.release)

	// Step 5: register, in short holds of hooks.lock.
	store, err := state.Open(home)
	if err != nil {
		return reg, fmt.Errorf("open local store: %w", err)
	}
	candidates := plan.Imported()
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].StartedAt.Before(candidates[j].StartedAt) })
	stopRegister := startActivity(stdout, "Registering sessions…")
	reg.activity.set(stopRegister)
	registration := backfill.Registration{
		Context: registrationCtx,
		Sources: env.agentRegistry(),
		Home:    home, Store: store, Batch: batch.ID, AdmittedAt: admittedAt, DestinationID: batch.DestinationID,
		MaxHoldSteps: env.backfillHoldSteps, CursorDatabase: env.cursorDatabase(), RepoKey: env.repoKeyResolver(),
		AfterHold: func(sessions, subagents []string) error {
			batch.AddSessions(sessions, subagents)
			if err := backfill.SaveBatch(home, batch); err != nil {
				return err
			}
			return env.checkpoint("registered")
		},
		Stop: interrupt.requested,
	}
	result, err := registration.Run(candidates)
	reg.batch, reg.result = batch, result
	if err != nil {
		stopRegister()
		reg.activity.clear()
		// Whatever the last hold registered is in the store even if the
		// batch file missed it; record it before stopping.
		if reconcileErr := batch.Reconcile(store); reconcileErr == nil {
			_ = backfill.SaveBatch(home, batch)
		}
		reg.batch = batch
		if errors.Is(err, backfill.ErrStopped) {
			return reg, &importStoppedError{batchID: batch.ID, sessions: len(batch.Sessions)}
		}
		return reg, &importRegistrationError{err: err, registered: len(result.Sessions)}
	}
	if err := completeBatch(env, home, store, &batch); err != nil {
		stopRegister()
		reg.activity.clear()
		return reg, err
	}
	reg.batch = batch
	stopRegister()
	reg.activity.clear()
	releaseCollector()
	return reg, nil
}

// completeBatch rebuilds the batch's sessions from the registrations, which
// are the source of truth, and marks it complete.
func completeBatch(env Env, home string, store *state.Store, batch *backfill.Batch) error {
	if err := batch.Reconcile(store); err != nil {
		return err
	}
	completed := env.now().UTC()
	batch.CompletedAt = &completed
	return backfill.SaveBatch(home, *batch)
}

// finishInterruptedBatch completes the latest import when it was
// interrupted, a run with the same options finds nothing left to import,
// and so no later run would ever complete it.
func finishInterruptedBatch(env Env, stdout io.Writer, home string, plan backfill.Plan, cfg config.Config) error {
	batches, err := backfill.LoadBatches(home)
	if err != nil || len(batches) == 0 {
		return err
	}
	last := batches[len(batches)-1]
	if !last.Continues(plan.BatchFilters(), cfg.DestinationID()) {
		return nil
	}
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return errors.New("setup or another backfill is running; run backfill again when it finishes")
	}
	defer release()
	// No collector pass or retention may remove what is being listed.
	stopWait := startActivity(stdout, "Waiting for collector…")
	releaseCollector, err := lockCollectorWait(home, "backfill import", env.now(), backfillCollectorWait)
	stopWait()
	if err != nil {
		return errors.New("a collector pass is still running; run backfill again")
	}
	defer releaseCollector()
	// Read the batch again under the locks: an undo or another run may have
	// changed it since it was checked.
	if batches, err = backfill.LoadBatches(home); err != nil || len(batches) == 0 {
		return err
	}
	latest := batches[len(batches)-1]
	if latest.ID != last.ID || !latest.Continues(plan.BatchFilters(), cfg.DestinationID()) {
		return nil
	}
	last = latest
	if err := completeBatch(env, home, state.OpenReadOnly(home), &last); err != nil {
		return err
	}
	terminal.Printf(stdout, "\nImport %s, which was interrupted, is complete: %s registered.\n", last.ID, countNoun(len(last.Sessions), "session"))
	return nil
}

// lockedWriter serializes writes, so the interruption's message never
// interleaves with the command's own output.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// underlyingWriter is w without a lockedWriter around it, for terminal
// checks.
func underlyingWriter(w io.Writer) io.Writer {
	if p, ok := w.(*promptWriter); ok {
		return underlyingWriter(p.w)
	}
	if l, ok := w.(*lockedWriter); ok {
		return underlyingWriter(l.w)
	}
	return w
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// activityStop holds the current spinner's stop func so Ctrl-C can clear the
// line without racing the main goroutine as spinners start and stop.
type activityStop struct {
	mu sync.Mutex
	fn func()
}

func (a *activityStop) set(fn func()) {
	a.mu.Lock()
	a.fn = fn
	a.mu.Unlock()
}

func (a *activityStop) clear() { a.set(nil) }

// invoke runs the current stop func, if any. It is safe to call from the
// signal watcher while set/clear run on the main goroutine.
func (a *activityStop) invoke() {
	a.mu.Lock()
	fn := a.fn
	a.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func importConfirmationContext(env Env, stderr io.Writer, plan backfill.Plan) (context.Context, func()) {
	recovered := false
	for _, c := range plan.Imported() {
		recovered = recovered || c.ProjectResolution != nil
	}
	if !recovered {
		return context.WithCancel(context.Background())
	}
	return interruptibleContext(env, stderr)
}

// committedImport is a committed configuration change and its batch file.
// plan is what the import admits: the confirmed plan without the sessions
// whose evidence changed since it was made, which changed counts.
type committedImport struct {
	batch      backfill.Batch
	admittedAt time.Time
	added      int
	plan       backfill.Plan
	changed    int
}

// commitImport is step 4. It renews recovered ownership first, outside
// hooks.lock (backfill.Plan.AdmitRecovery): a session whose evidence changed
// is left out rather than refusing the import. With collector.lock held, it
// then takes hooks.lock, rereads the configuration, and checks that it is the
// one the plan was made from. It then stamps the admission time and writes
// the batch file, which records every configuration change
// (backfill.ConfigChanges), then the new projects, apps, and retention, for
// the sessions left. The batch file is written first, so a crash in between
// leaves a batch that names projects it did not add, never projects added
// without a record.
func commitImport(ctx context.Context, env Env, home string, confirmed backfill.Plan, fingerprint string) (committedImport, error) {
	plan, changed, err := confirmed.AdmitRecovery(ctx)
	if err != nil {
		return committedImport{}, err
	}
	batch, admittedAt, added, err := commitAdmitted(ctx, env, home, plan, fingerprint)
	return committedImport{batch: batch, admittedAt: admittedAt, added: added, plan: plan, changed: changed}, err
}

// commitAdmitted is commitImport for the admitted plan.
func commitAdmitted(ctx context.Context, env Env, home string, plan backfill.Plan, fingerprint string) (batch backfill.Batch, admittedAt time.Time, added int, err error) {
	releaseHooks, err := local.NamedLockWait(home, "hooks.lock", backfillHooksWait)
	if err != nil {
		return batch, admittedAt, 0, errors.New("capture hooks are busy; run backfill again. Nothing was changed")
	}
	defer releaseHooks()
	if err := ctx.Err(); err != nil {
		return batch, admittedAt, 0, err
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return batch, admittedAt, 0, fmt.Errorf("load config: %w", err)
	}
	if !found {
		return batch, admittedAt, 0, errNotSetUp
	}
	if refusal := importRefusal(home, cfg); refusal != "" {
		return batch, admittedAt, 0, errors.New(refusal)
	}
	if configFingerprint(cfg) != fingerprint {
		return batch, admittedAt, 0, errors.New("the configuration changed while this was open; run backfill again. Nothing was changed")
	}
	now := env.now()
	admittedAt = now.UTC()
	if err := backfill.CheckClock(cfg, plan, admittedAt); err != nil {
		return batch, admittedAt, 0, fmt.Errorf("%w. Nothing was changed", err)
	}
	batch, err = backfill.OpenBatch(home, state.OpenReadOnly(home), plan.BatchFilters(), cfg.DestinationID(), now)
	if errors.Is(err, backfill.ErrUnreadableImport) {
		return batch, admittedAt, 0, fmt.Errorf("%w. Nothing was changed. Repair the unreadable file in %s, then run backfill again. Moving it out of the folder also lets backfill run, but its sessions stay archived and backfill undo can no longer remove them", err, filepath.Join(home, "imports"))
	}
	if err != nil {
		return batch, admittedAt, 0, fmt.Errorf("%w. Nothing was changed", err)
	}
	previous := cfg
	previous.Archive.Projects = slices.Clone(cfg.Archive.Projects)
	changes, err := backfill.ApplyToConfig(&cfg, plan, admittedAt)
	if err != nil {
		return batch, admittedAt, 0, fmt.Errorf("%w. Nothing was changed", err)
	}
	if err := config.ReconcileDiscovery(&cfg, previous, admittedAt); err != nil {
		return batch, admittedAt, 0, fmt.Errorf("update discovery authorization: %w", err)
	}
	batch.AddChanges(changes)
	if err := backfill.SaveBatch(home, batch); err != nil {
		return batch, admittedAt, 0, err
	}
	if err := env.checkpoint("batch saved"); err != nil {
		return batch, admittedAt, 0, err
	}
	if err := ctx.Err(); err != nil {
		return batch, admittedAt, 0, err
	}
	if err := config.Save(home, cfg); err != nil {
		return batch, admittedAt, 0, fmt.Errorf("save config: %w", err)
	}
	return batch, admittedAt, len(changes.ProjectIDs), nil
}

func printRegistered(out io.Writer, batchID string, added int, result backfill.RegistrationResult) {
	line := ""
	if added > 0 {
		line = fmt.Sprintf("Added %s. ", countNoun(added, "project"))
	}
	line += "Registered " + countNoun(len(result.Sessions), "session")
	if n := len(result.Subagents); n > 0 {
		line += " and " + countNoun(n, "subagent transcript")
	}
	terminal.Printf(out, "%s as import %s.\n", line, batchID)
	var skipped []string
	if n := result.AlreadyArchived; n > 0 {
		skipped = append(skipped, fmt.Sprintf("%d already in the archive", n))
	}
	if n := result.Gone; n > 0 {
		skipped = append(skipped, fmt.Sprintf("%d whose transcript is gone", n))
	}
	if n := result.NotAdmitted; n > 0 {
		skipped = append(skipped, fmt.Sprintf("%d no longer accepted by the setup", n))
	}
	if n := result.StartInFuture; n > 0 {
		skipped = append(skipped, fmt.Sprintf("%d starting in the future (check the clock)", n))
	}
	if n := result.Invalid; n > 0 {
		skipped = append(skipped, fmt.Sprintf("%d that could not be registered", n))
	}
	if n := result.SubagentsInvalid; n > 0 {
		skipped = append(skipped, countNoun(n, "subagent transcript")+" whose record conflicts with an earlier one or is incomplete")
	}
	if len(skipped) > 0 {
		terminal.Printf(out, "Not registered: %s.\n", strings.Join(skipped, ", "))
	}
}

// printChangedAtAdmission says how many confirmed sessions were left out
// because their project or source evidence changed after the plan was made,
// and what imports them (retry, already styled).
func printChangedAtAdmission(out io.Writer, changed int, retry string) {
	if changed == 0 {
		return
	}
	them := "them"
	if changed == 1 {
		them = "it"
	}
	terminal.Printf(out, "Not imported: %s whose project or source evidence changed after the plan was made. Run %s to import %s.\n", countNoun(changed, "session"), retry, them)
}

func printImportHints(out io.Writer, batchID string) {
	s := styleFor(underlyingWriter(out))
	terminal.Println(out, "See them with "+s.cmd("agent-archive list --imported")+".")
	terminal.Println(out, "Undo with "+s.cmd("agent-archive backfill undo "+batchID)+".")
}

// uploadBusyGiveUp is how long upload waits on a collector pass that holds
// the lock without finishing any of the import's sessions.
const uploadBusyGiveUp = 2 * time.Minute

// uploadImport is step 6: collector passes, as sync runs them, until no
// session of the batch has work left or a pass makes no progress. Ctrl-C
// ends the pass after the session in flight; what is left is uploaded by
// the background collector.
func uploadImport(env Env, stdout, stderr io.Writer, home, batchID string, plan backfill.Plan, interrupt *signalWatch, activity *activityStop) int {
	sizes := map[agentmeta.SessionKey]int64{}
	for _, c := range plan.Imported() {
		size := c.Bytes
		for _, sub := range c.Subagents {
			size += sub.Bytes
		}
		key, err := agentmeta.NewSessionKey(c.Harness, c.NativeSessionID)
		if err != nil {
			terminal.Printf(stderr, "agent-archive: backfill: %v\n", err)
			return 1
		}
		sizes[key] = size
	}
	u := &upload{env: env, home: home, batch: batchID, sizes: sizes, terminal: env.isTerminal(underlyingWriter(stdout)), out: stdout}
	if activity != nil {
		activity.set(u.stopFinishing)
		defer activity.clear()
	}
	if err := u.refresh(); err != nil {
		terminal.Printf(stderr, "agent-archive: backfill: %v\n", err)
		return 1
	}
	// Ctrl-C is looked for before each session and between passes, so the
	// session in flight always finishes.
	stop := interrupt.requested

	var passErr error
	lastProgress := time.Now()
	for len(u.pending) > 0 && !stop() {
		before := len(u.pending)
		_, err := runPass(env, false, passOptions{progress: u.observe, stop: stop})
		u.stopFinishing()
		if refreshErr := u.refresh(); refreshErr != nil && err == nil {
			err = refreshErr
		}
		if len(u.pending) < before {
			lastProgress = time.Now()
		}
		if errors.Is(err, local.ErrBusy) {
			// The background collector is running a pass, which uploads
			// these sessions too. Wait for it.
			if time.Since(lastProgress) > uploadBusyGiveUp {
				break
			}
			stopBusy := startActivity(stdout, "Waiting for collector…")
			if activity != nil {
				activity.set(stopBusy)
			}
			for wait := 0; wait < 20 && !stop(); wait++ {
				time.Sleep(100 * time.Millisecond)
			}
			stopBusy()
			if activity != nil {
				activity.set(u.stopFinishing)
			}
			continue
		}
		if err != nil {
			passErr = err
			break
		}
		u.draw(!u.terminal)
		if len(u.pending) >= before {
			break
		}
	}
	u.stopFinishing()
	if u.terminal && u.drawn {
		terminal.Println(stdout)
	}

	switch {
	case len(u.pending) == 0:
		terminal.Printf(stdout, "Uploaded %s (%s).\n", countNoun(u.total, "session"), backfill.FormatSize(u.totalBytes))
	case interrupt.requested():
		terminal.Printf(stdout, "Stopped. The remaining %s will be uploaded by the background collector.\n", countNoun(len(u.pending), "session"))
	case errors.Is(passErr, errPaused):
		terminal.Printf(stdout, "Collection is paused. The remaining %s will be uploaded after agent-archive resume.\n", countNoun(len(u.pending), "session"))
	default:
		terminal.Printf(stdout, "%s not uploaded yet. The background collector keeps trying; run agent-archive status to follow it.\n", countNoun(len(u.pending), "session"))
	}
	printImportHints(stdout, batchID)
	if interrupt.requested() || errors.Is(passErr, errPaused) {
		return 0
	}
	if passErr != nil {
		terminal.Printf(stderr, "agent-archive: backfill: upload: %v\n", passErr)
		return 1
	}
	if len(u.pending) > 0 {
		return 1
	}
	return 0
}

// upload tracks a batch's sessions while they upload.
type upload struct {
	env      Env
	home     string
	batch    string
	sizes    map[agentmeta.SessionKey]int64
	terminal bool
	out      io.Writer
	drawn    bool
	// finish is the spinner after every session of the batch has published
	// but the collector pass is still wrapping up (read-back, status writes).
	// finishMu guards it: observe may start it while Ctrl-C stops it.
	finishMu sync.Mutex
	finish   *spinner

	total      int
	totalBytes int64
	// pending maps each session still to upload to its size.
	pending map[string]int64
}

// refresh recounts the batch's sessions from the local store: registrations
// in the batch, not subagents, that still have upload work.
func (u *upload) refresh() error {
	cfg, found, err := config.Load(u.home)
	if err != nil {
		return err
	}
	if !found {
		return errNotSetUp
	}
	store := state.OpenReadOnly(u.home)
	regs, err := store.LoadRegistrations()
	if err != nil {
		return err
	}
	u.total, u.totalBytes, u.pending = 0, 0, map[string]int64{}
	for _, reg := range regs {
		if !reg.InBatch(u.batch) || !independentImportedSession(reg) {
			continue
		}
		size := u.size(reg)
		u.total++
		u.totalBytes += size
		waiting, err := importPending(store, cfg, reg)
		if err != nil {
			return err
		}
		if waiting {
			u.pending[reg.ArchiveSessionID] = size
		}
	}
	return nil
}

func (u *upload) size(reg archive.SessionRegistration) int64 {
	if size, ok := u.sizes[agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}]; ok {
		return size
	}
	if !reg.ReadsTranscriptFile() {
		// A Cursor database chat has no file to measure. Its last capture's
		// retained bytes stand in, when it has one; measuring the chat
		// itself would mean copying Cursor's database.
		bundle, _, found, err := state.OpenReadOnly(u.home).LoadLastPublished(reg.ArchiveSessionID)
		if err == nil && found {
			return int64(bundle.Capture.Boundary.RetainedBytes)
		}
		return 0
	}
	if info, err := os.Stat(reg.TranscriptPath); err == nil {
		return info.Size()
	}
	return 0
}

// observe is the collector's progress callback: a published session of the
// batch is done.
func (u *upload) observe(p collector.Progress) {
	if _, ok := u.pending[p.ArchiveSessionID]; !ok || !p.Published {
		return
	}
	delete(u.pending, p.ArchiveSessionID)
	if len(u.pending) == 0 {
		// Live terminals replace the count line with a finishing spinner
		// while the pass wraps up. Non-TTY keeps one count line per pass
		// (drawn after runPass returns), so a mid-pass empty pending set
		// that refresh later revises does not print a false "done" line.
		u.startFinishing()
		return
	}
	if u.terminal {
		u.draw(false)
	}
}

// startFinishing replaces the upload progress line with a spinner while the
// collector pass finishes after the last session of the batch has published.
func (u *upload) startFinishing() {
	u.finishMu.Lock()
	defer u.finishMu.Unlock()
	if u.finish != nil {
		return
	}
	if u.terminal && u.drawn {
		terminal.Print(u.out, "\r\x1b[K")
		u.drawn = false
	}
	style := activityStyle(u.out)
	if !style.live {
		return
	}
	u.finish = style.spin(u.out, "Finishing upload…")
}

func (u *upload) stopFinishing() {
	u.finishMu.Lock()
	sp := u.finish
	u.finish = nil
	u.finishMu.Unlock()
	if sp != nil {
		sp.stop()
	}
}

// draw shows the progress line: redrawn in place on a terminal, or printed
// once per pass otherwise.
func (u *upload) draw(newline bool) {
	u.stopFinishing()
	var pendingBytes int64
	for _, size := range u.pending {
		pendingBytes += size
	}
	line := fmt.Sprintf("Uploading: %d of %s, %s of %s", u.total-len(u.pending), countNoun(u.total, "session"), backfill.FormatSize(u.totalBytes-pendingBytes), backfill.FormatSize(u.totalBytes))
	if newline {
		terminal.Println(u.out, line)
		return
	}
	terminal.Printf(u.out, "\r%-72s", line)
	u.drawn = true
}

// runBackfillHistory implements `agent-archive backfill history`: each
// import with its ID, start, sessions, projects added, and upload state. It
// reads local state only.
func runBackfillHistory(args []string, stdout, stderr io.Writer, env Env) int {
	if !env.newCommandFlags("backfill history", stderr).parseFlagsOnly(args) {
		return 2
	}
	home, err := env.readHome()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: backfill history: resolve home: %v\n", err)
		return 1
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		terminal.Printf(stderr, "agent-archive: backfill history: load config: %v\n", err)
		return 1
	}
	if !found {
		terminal.Println(stderr, notSetUpMessage)
		return 1
	}
	batches, err := backfill.LoadBatches(home)
	if err != nil {
		// An unreadable import file is named; the others are still listed.
		terminal.Printf(stderr, "agent-archive: backfill history: %v\n", err)
	}
	if len(batches) == 0 && err != nil {
		terminal.Println(stdout, "No import could be read. Repair or move the files named above, then run agent-archive backfill history again.")
		return 1
	}
	if len(batches) == 0 {
		terminal.Println(stdout, "No imports yet. Run agent-archive backfill --dry-run to see what an import would do.")
		return 0
	}
	store := state.OpenReadOnly(home)
	regs, err := store.LoadRegistrations()
	if err != nil {
		terminal.Printf(stderr, "agent-archive: backfill history: %v\n", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	terminal.Println(tw, "IMPORT\tSTARTED\tSESSIONS\tPROJECTS ADDED\tUPLOAD")
	loc := env.now().Location()
	for i, b := range batches {
		state, err := batchUploadState(store, cfg, regs, b, i == len(batches)-1)
		if err != nil {
			terminal.Printf(stderr, "agent-archive: backfill history: %v\n", err)
			return 1
		}
		terminal.Printf(tw, "%s\t%s\t%d\t%d\t%s\n", b.ID, b.StartedAt.In(loc).Format("2006-01-02 15:04"), len(b.Sessions), len(b.ProjectsAdded), state)
	}
	if err := tw.Flush(); err != nil {
		terminal.Printf(stderr, "agent-archive: backfill history: %v\n", err)
		return 1
	}
	return 0
}

// batchUploadState is history's UPLOAD column: undone, or partly undone
// with what is left; interrupted, nothing registered, removed (none of its
// sessions is registered any more), how many are waiting, or uploaded. Only
// the latest import can still be finished by running backfill again.
func batchUploadState(store *state.Store, cfg config.Config, regs []archive.SessionRegistration, b backfill.Batch, latest bool) (string, error) {
	registered, subagents, waiting := 0, 0, 0
	for _, reg := range regs {
		if !reg.InBatch(b.ID) {
			continue
		}
		if !independentImportedSession(reg) {
			subagents++
			continue
		}
		registered++
		pending, err := importPending(store, cfg, reg)
		if err != nil {
			return "", err
		}
		if pending {
			waiting++
		}
	}
	if b.UndoneAt != nil {
		projects := 0
		for _, p := range cfg.Archive.Projects {
			// A project undo kept for another import is that import's now.
			if p.Included && slices.Contains(b.ProjectsAdded, p.ProjectID) && !slices.Contains(b.ProjectsExcluded, p.ProjectID) && !slices.Contains(b.ProjectsKept, p.ProjectID) {
				projects++
			}
		}
		switch {
		case registered > 0:
			return fmt.Sprintf("partly undone; %s left", countNoun(registered, "session")), nil
		case subagents > 0:
			return fmt.Sprintf("partly undone; %s left", countNoun(subagents, "subagent transcript")), nil
		case projects > 0:
			return fmt.Sprintf("partly undone; %s left", countNoun(projects, "project")), nil
		}
		return "undone", nil
	}
	switch {
	case b.CompletedAt == nil && latest:
		return fmt.Sprintf("interrupted; %d registered; run agent-archive backfill with the same options to finish", registered), nil
	case b.CompletedAt == nil:
		return fmt.Sprintf("interrupted; %d registered", registered), nil
	case len(b.Sessions)+len(b.Subagents) == 0:
		return "nothing registered", nil
	case registered == 0:
		return "removed", nil
	case waiting > 0:
		return fmt.Sprintf("%d waiting", waiting), nil
	}
	return "uploaded", nil
}
