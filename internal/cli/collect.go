package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/evidence"
	"github.com/wangjohn/agent-archive/internal/gitremote"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

var (
	errNotSetUp = errors.New("not set up yet; run `agent-archive setup` first")
	errPaused   = errors.New("collection is paused; run `agent-archive resume` first")
)

// collector-error.log is cut back to its last errorLogKeepBytes once it
// grows past errorLogMaxBytes.
const (
	errorLogMaxBytes  = 1 << 20
	errorLogKeepBytes = 256 << 10
)

// Deadlines for one pass. A pass holds the collector lock, and every
// scheduled tick that finds it held gives up quietly, so a pass that never
// ended would stop capture without a word.
//
// Collection has two. Past collectSoftDeadline the pass starts no new
// session, as when backfill is interrupted: the session in flight finishes
// and the rest keep their work for the next pass. collectHardDeadline cuts
// off the session in flight too, and is set well above the time the largest
// source a session can have (archive.MaxRecordBytes, 64 MiB) takes over a
// slow uplink, so one big upload is not cut off and restarted on every pass.
// A session cut off this way is not reported as failing. The retention sweep
// gets its own budget, so a slow collection cannot starve cleanup.
//
// Variables only so tests can shorten them.
var (
	collectSoftDeadline = 10 * time.Minute
	collectHardDeadline = 60 * time.Minute
	sweepTimeout        = 5 * time.Minute
)

// runCollectCommand implements the hidden `_collect` entry point
// install.LaunchAgent schedules every 60 seconds. Unlike `sync`, it never
// reports "already running" as a problem: a scheduled tick finding the
// previous one still working is the lock doing its job, not an error.
func runCollectCommand(_ []string, _ io.Writer, stderr io.Writer, env Env) int {
	// launchd appends this process's stderr to collector-error.log and never
	// rotates it. launchd runs one _collect at a time, so this process is the
	// file's only writer until it exits.
	if home, err := env.home(); err == nil {
		_ = local.TrimLog(filepath.Join(home, "collector-error.log"), errorLogMaxBytes, errorLogKeepBytes)
	}
	_, err := runOnePass(env, true)
	if err != nil {
		if errors.Is(err, errNotSetUp) || errors.Is(err, errPaused) {
			return 0
		}
		terminal.Printf(stderr, "agent-archive: collect: %v\n", err)
		return 1
	}
	return 0
}

// runOnePass loads configuration, honors pause, takes the machine lock, and
// runs one collector.Run pass. quietOnBusy controls whether a contended lock
// is reported as an error or treated as an expected, silent no-op.
//
// Once localStore exists, any failure before collector.Run gets its own
// chance to record Status is written into that same Status's last errors.
// Without this, a broken lock or bad storage credentials would fail every
// scheduled _collect tick while `status` kept reporting the last successful
// scan's last errors (typically none), leaving a misconfigured install
// looking healthy.
func runOnePass(env Env, quietOnBusy bool) (collector.Result, error) {
	return runPass(env, quietOnBusy, passOptions{})
}

// passOptions are what a caller watching one pass passes through to
// collector.Run: backfill's upload progress and Ctrl-C.
type passOptions struct {
	progress func(collector.Progress)
	stop     func() bool
}

func runPass(env Env, quietOnBusy bool, pass passOptions) (result collector.Result, resultErr error) {
	// Read-only until the configuration is found: sync before setup leaves
	// no data directory behind.
	home, err := env.readHome()
	if err != nil {
		return collector.Result{}, fmt.Errorf("resolve home: %w", err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return collector.Result{}, fmt.Errorf("load config: %w", err)
	}
	if !found {
		return collector.Result{}, errNotSetUp
	}
	if cfg.Paused {
		return collector.Result{}, errPaused
	}

	localStore, err := state.Open(home)
	if err != nil {
		return collector.Result{}, fmt.Errorf("open local store: %w", err)
	}

	unlock, err := lockCollector(home, collectorPassHolder(quietOnBusy, pass), env.now())
	if err != nil {
		if errors.Is(err, local.ErrBusy) {
			if quietOnBusy {
				return collector.Result{}, nil
			}
			return collector.Result{}, err
		}
		// Not recorded into Status here: without the lock, a concurrent
		// holder's own SaveStatus (from collector.Run or this same
		// function) could race an unguarded read-modify-write to
		// status.json and lose an update. Every failure below this point
		// runs only after the lock is held, so it can record safely.
		return collector.Result{}, fmt.Errorf("acquire lock: %w", err)
	}
	defer unlock()
	cfg, err = reloadPassConfig(home, env)
	if err != nil {
		return collector.Result{}, err
	}

	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), collectHardDeadline)
	defer cancel()
	stop := func() bool {
		return time.Since(started) >= collectSoftDeadline || (pass.stop != nil && pass.stop())
	}
	if err := checkDurablePassRoots(ctx, localStore); err != nil {
		recordPreflightError(localStore, err)
		return collector.Result{}, err
	}
	// Local identity recovery and admission must not depend on credentials or
	// storage availability. Source observation retains its own short budget.
	// The complete authority census precedes the application allowance. Give it
	// part of the remaining pass budget so a census slower than four seconds
	// does not consume every future slice before any owner can be applied.
	recoveryBudget := max(time.Nanosecond, (collectSoftDeadline-time.Since(started))/2)
	applicationBudget := min(state.SessionIndexRecoverySlice, recoveryBudget) * 3 / 4
	recoveryCtx, recoveryCancel := context.WithTimeout(ctx, recoveryBudget)
	_, recoveryErr := localStore.RecoverSessionIndexScheduled(recoveryCtx, max(time.Nanosecond, applicationBudget))
	if errors.Is(recoveryErr, context.DeadlineExceeded) && state.SessionIndexRecoveryInterrupted(recoveryErr) && ctx.Err() == nil {
		recoveryErr = nil
	}
	if recoveryErr != nil {
		recordPreflightError(localStore, recoveryErr)
	}
	recoveryCancel()
	rollouts, readHomes, err := passCodexRollouts(ctx, localStore, cfg, env)
	if err != nil {
		recordPreflightError(localStore, err)
		return collector.Result{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, rollouts.Close()) }()
	observer := &gitremote.IdentityObserver{}
	_, discoveryErr := discovery.Run(ctx, localStore, cfg, discovery.Options{Sources: registryFor(env), Now: env.Now, Stop: stop, Rollouts: rollouts, RepositoryIdentity: observer.Lookup, RepositoryIdentityCurrent: gitremote.ProjectIdentityCurrent})
	if discoveryErr != nil {
		recordPreflightError(localStore, errors.Join(recoveryErr, discoveryErr))
	}
	objectStore, cfg, err := openPassStorage(ctx, home, cfg, env, localStore, quietOnBusy)
	if err != nil {
		return collector.Result{}, err
	}

	// Registration gets an independent budget after capture and retention,
	// including their failure paths. It cannot cause either to be skipped.
	defer func() { _ = publishMachineLocked(context.Background(), home, cfg, env, objectStore, false) }()
	// The previous pass's time, read before this pass overwrites it: the
	// retention sweep checks the clock against it (see retention.Options).
	var previousScanAt time.Time
	if previous, err := localStore.LoadStatus(); err == nil {
		previousScanAt = previous.LastScanAt
	}
	labelHomes := []string{}
	if cfg.Discovery != nil {
		labelHomes = append(labelHomes, cfg.Discovery.CodexHomes...)
	}
	if len(labelHomes) == 0 {
		if userHome, err := env.userHomeDir(); err == nil {
			if file := env.hookFiles(userHome)["codex"]; file != "" {
				labelHomes = append(labelHomes, filepath.Dir(file))
			}
		}
	}
	result, err = collector.Run(ctx, localStore, objectStore, collector.Options{
		Labels:           env.labelProviders(cfg),
		LabelEnvironment: env.labelEnvironment(cfg, labelHomes),
		PrepareCodexCoverage: func(ctx context.Context, regs []archive.SessionRegistration) error {
			return rollouts.PrepareRegistered(ctx, cfg, regs, discovery.Options{Now: env.Now, Stop: stop})
		},
		SkipSessionIndexRecovery: true,
		CodexRollouts:            rollouts,
		PendingCodexRollouts:     pendingCodexRollouts(rollouts),
		ConfiguredCodexHomes:     readHomes,
		ResolveCodexReadHomes:    func(current config.Config) ([]string, error) { return trustedCodexReadHomes(current, env) },
		Parsers:                  parsersFor(env),
		Sources:                  registryFor(env),
		Decoders:                 env.agentRegistry(),
		MachineID:                cfg.MachineID,
		SupplementalEvidence:     skillObserver(env, cfg.EffectiveSkillEvidence()),
		SkillEvidence:            cfg.EffectiveSkillEvidence(),
		AcceptSession:            cfg.AcceptSession,
		Now:                      env.Now,
		RequireSkillUse:          cfg.RequireSkillUse,
		Progress:                 pass.progress,
		Stop:                     stop,
		CursorDatabase:           env.cursorDatabase(),
		RepoKey:                  env.repoKey,
	})
	// Collector status replaces its previous LastErrors. Preserve every local
	// preflight failure, even if recovery later succeeds or collection errors.
	for _, preflightErr := range []error{recoveryErr, discoveryErr} {
		if preflightErr != nil {
			addStatusProblem(localStore, preflightErr.Error())
		}
	}
	if err != nil {
		return result, err
	}

	summary, verifyErr, err := verifyAndRecordPass(ctx, home, cfg, env, localStore, objectStore, quietOnBusy, result)
	if err != nil {
		return result, err
	}

	return finishPassWithRetention(home, env, cfg, localStore, objectStore, previousScanAt, result, summary, verifyErr)
}

// verifyAndRecordPass performs read-back verification and records per-session
// failures, returning the summary it recorded for them ("" when none failed).
// Verification is returned separately so retention still runs first.
func verifyAndRecordPass(ctx context.Context, home string, cfg config.Config, env Env, localStore *state.Store, objectStore storage.ObjectStore, quietOnBusy bool, result collector.Result) (summary string, verifyErr error, err error) {
	// A read-back verification failure is reported after the retention
	// sweep: it is no reason to skip cleanup.
	if _, err := verifyPublicationsWithin(ctx, home, cfg, env, localStore, objectStore); err != nil {
		verifyErr = fmt.Errorf("read-back verification: %w", err)
	}
	if health := passStorageHealth(result); health != "not_checked" {
		if err := recordStorageHealth(home, cfg, env, quietOnBusy, health); err != nil {
			return "", verifyErr, err
		}
	}
	if len(result.Errors) > 0 {
		// In place of collector.Run's own count of the same sessions; the
		// pass's other problems stay.
		summary, err = recordSessionIssues(localStore, result.Errors, subagentLookup(localStore), collector.FailedSessionsProblem(len(result.Errors)))
		if err != nil {
			return "", verifyErr, err
		}
	}
	return summary, verifyErr, nil
}

// finishPassWithRetention gives cleanup its own deadline after collection.
// It runs under the caller's collector lock and preserves verification errors
// until after the sweep has completed.
func finishPassWithRetention(home string, env Env, cfg config.Config, localStore *state.Store, objectStore storage.ObjectStore, previousScanAt time.Time, result collector.Result, summary string, verifyErr error) (collector.Result, error) {
	sweepCtx, cancelSweep := context.WithTimeout(context.Background(), sweepTimeout)
	defer cancelSweep()
	sweepOptions := retention.Options{
		PrivacyVerified: func(reg archive.SessionRegistration, current archive.Metadata) bool {
			return privacyPublicationVerified(home, cfg, localStore, reg, current)
		},
		Now: env.Now,
		// Retention sweeps every session this machine registered, including
		// ones cfg.AcceptSession no longer admits for publication: an excluded
		// project's already-published sessions still own objects in this
		// bucket and must age out of it. Only a session admitted into another
		// destination published somewhere else, and for those the sweep
		// prunes local state without touching this bucket. That is decided by
		// the registration's destination ID, or for an older registration
		// without one by its admission (not its start) against
		// DestinationSince.
		CurrentDestination: cfg.InCurrentDestination,
		// Outstanding work defers expiry only when the collector will do it.
		Publishable:    cfg.AcceptSession,
		SessionMaxAge:  config.RetentionDuration(cfg.RetentionDays),
		PreviousScanAt: previousScanAt,
	}
	if env.sweepClock != nil {
		env.sweepClock(&sweepOptions)
	}
	sweepResult, sweepErr := retention.Sweep(sweepCtx, localStore, objectStore, sweepOptions)
	if sweepErr != nil {
		sweepErr = fmt.Errorf("collection succeeded but retention cleanup failed: %w", sweepErr)
		if verifyErr != nil {
			addStatusProblem(localStore, verifyErr.Error())
		}
		addStatusProblem(localStore, sweepErr.Error())
		return result, errors.Join(verifyErr, sweepErr)
	}
	if len(sweepResult.Errors) > 0 {
		recordRetentionErrors(localStore, &result, sweepResult, summary)
	}
	// A clock that disagrees with the storage service's holds every deletion
	// by age until it is fixed, which status must say. A hold for one pass
	// after a long gap (the machine was off) clears itself and says nothing.
	if held := sweepResult.Held; held != nil && !errors.Is(held, retention.ErrClockJumped) {
		addStatusProblem(localStore, fmt.Sprintf("retention: %v", held))
	}
	if verifyErr != nil {
		addStatusProblem(localStore, verifyErr.Error())
		return result, verifyErr
	}
	return result, nil
}

// openPassStorage verifies scheduled collector access and refreshes health and
// bucket privacy evidence while the caller still holds collector.lock.
func openPassStorage(ctx context.Context, home string, cfg config.Config, env Env, localStore *state.Store, quietOnBusy bool) (storage.ObjectStore, config.Config, error) {
	objectStore, err := env.openStoreContext(ctx, cfg)
	if err != nil {
		storeErr := fmt.Errorf("open storage: %w", err)
		recordPreflightError(localStore, storeErr)
		_ = recordStorageHealth(home, cfg, env, quietOnBusy, "credentials_unavailable")
		return nil, cfg, storeErr
	}
	if quietOnBusy {
		var prior storageHealth
		healthErr := local.Read(filepath.Join(home, "storage-health.json"), &prior)
		if healthErr != nil || prior.ConfigurationID != configurationID(cfg) || prior.Context != "background_collector" || prior.State != "verified" || env.now().Sub(prior.CheckedAt) > storageHealthRefreshAfter {
			probeErr := storage.VerifyAccess(ctx, objectStore)
			state := "verified"
			if probeErr != nil {
				state = storageFailureState(probeErr)
			}
			if err := recordStorageHealth(home, cfg, env, true, state); err != nil {
				return nil, cfg, err
			}
			if probeErr != nil {
				// Only a fixed message is recorded: the SDK's own can carry
				// what a credential_process printed.
				message := "background storage access failed; restore credentials or connectivity and retry"
				if credentials.CredentialProcessFailed(probeErr) {
					message = backgroundCredentialProcessFailure
				}
				recordPreflightError(localStore, errors.New(message))
				return nil, cfg, probeErr
			}
		}
		// Privacy evidence is persisted in config.json exactly as setup saves
		// it, so status and setup review read one source. The config was
		// re-read under the lock above, so the save cannot lose another
		// writer's update.
		if bucketPrivacyNeedsRefresh(cfg, env.now()) {
			cfg.BucketPrivacy = inspectBucketPrivacyContext(ctx, cfg, objectStore, env.now())
			if err := config.Save(home, cfg); err != nil {
				return nil, cfg, fmt.Errorf("save bucket privacy evidence: %w", err)
			}
		}
	}

	return objectStore, cfg, nil
}

// collectorPassHolder names the lock owner in status and busy errors.
func collectorPassHolder(quietOnBusy bool, pass passOptions) string {
	switch {
	case pass.progress != nil:
		return "backfill upload"
	case quietOnBusy:
		return "scheduled collection"
	default:
		return "sync"
	}
}

// reloadPassConfig runs under collector.lock. Setup or pause may have
// changed configuration since the read-only preflight above.
func reloadPassConfig(home string, env Env) (config.Config, error) {
	pruneHandoffs(home, env.now())
	if setupjournal.TransactionPending(home) {
		return config.Config{}, errors.New(recoveryPending(home))
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return config.Config{}, err
	}
	if !found || !cfg.Archive.Enabled {
		return config.Config{}, errNotSetUp
	}
	if cfg.Paused {
		return config.Config{}, errPaused
	}
	return cfg, nil
}

// passStorageHealth derives the storage-access evidence one pass produced. A
// publication is a real round trip, so it stands as "verified" even when
// other sessions failed for reasons unrelated to storage (a rewritten
// transcript, a size limit). Only a genuine storage error can downgrade it:
// an authentication failure always wins, and a plain storage outage leaves
// the pass unchecked only when nothing was published through it.
func passStorageHealth(result collector.Result) string {
	health := "not_checked"
	if len(result.Published) > 0 {
		health = "verified"
	}
	for _, sessionErr := range result.Errors {
		if !isStorageError(sessionErr) {
			continue
		}
		if state := storageFailureState(sessionErr); state != "storage_unavailable" {
			return state
		}
		if len(result.Published) == 0 {
			health = "not_checked"
		}
	}
	return health
}

// recordRetentionErrors merges retention.Sweep's per-session failures into
// result, so sync's existing report/exit-code logic (which only knows about
// collector.Result) covers them too without its own retention-specific
// path, and records them in the Status with the pass's collection failures
// (see recordSessionIssues): otherwise a retention failure would never
// reach `status` at all, since, unlike collector.Run, Sweep does not
// persist a Status of its own.
//
// The summary it records covers the pass's own failed sessions too, so it
// takes the place of previous, the summary verifyAndRecordPass recorded for
// them (recomputed now, it could read differently: a registration the sweep
// removed counts as a session, not a subagent); the pass's other problems
// stay.
func recordRetentionErrors(localStore *state.Store, result *collector.Result, sweep retention.Result, previous string) {
	if result.Errors == nil {
		result.Errors = map[string]error{}
	}
	for id, sweepErr := range sweep.Errors {
		// A session that also failed collection keeps that error beside
		// this one.
		addSessionError(result.Errors, id, fmt.Errorf("%w: %w", errRetentionFailed, sweepErr))
	}
	_, _ = recordSessionIssues(localStore, result.Errors, subagentLookup(localStore), previous)
}

// addSessionError records err against a session, after any error the pass
// already recorded for it.
func addSessionError(errs map[string]error, id string, err error) {
	if previous := errs[id]; previous != nil {
		err = errors.Join(previous, err)
	}
	errs[id] = err
}

// addStatusProblem adds problem to the Status's last errors, after whatever
// the pass already recorded there, rather than replacing them: once
// collector.Run has saved this pass's Status, a later step's failure is one
// more problem of the same pass. Best effort, like recordPreflightError.
func addStatusProblem(localStore *state.Store, problem string) {
	status, err := localStore.LoadStatus()
	if err != nil {
		return
	}
	status.AddLastError(problem)
	_ = localStore.SaveStatus(status)
}

// backgroundCredentialProcessFailure is the Status.LastError a background
// pass records when the S3 profile's credential_process could not supply
// credentials, which status recognizes to say what to check.
const backgroundCredentialProcessFailure = "background storage access failed: the AWS profile's credential_process could not supply credentials"

// recordPreflightError persists a failure that happened before collector.Run
// could record its own Status, so `status` reflects it. It replaces the
// problems recorded there, which are the previous pass's; a failure after
// collector.Run is added with addStatusProblem instead. Best-effort: if the
// status write itself fails, the original error is still what the caller
// returns and reports.
func recordPreflightError(localStore *state.Store, preflightErr error) {
	status, err := localStore.LoadStatus()
	if err != nil {
		return
	}
	status.SetLastErrors(preflightErr.Error())
	// The counts described the summary this error replaces.
	status.IssueCounts = nil
	_ = localStore.SaveStatus(status)
}

// openConfiguredStore resolves cfg.Storage into a live ObjectStore. Only R2
// keeps its secret in the credential store (the Keychain on macOS, a private
// file elsewhere), so only R2 opens it (through credentialStore,
// Env.credentialStore), and a store that is unavailable (a macOS build
// without cgo, say) fails only an R2 configuration.
func openConfiguredStore(cfg config.Config, credentialStore func() (credentials.CredentialStore, error)) (storage.ObjectStore, error) {
	return openConfiguredStoreContext(context.Background(), cfg, credentialStore)
}

func openConfiguredStoreContext(ctx context.Context, cfg config.Config, credentialStore func() (credentials.CredentialStore, error)) (storage.ObjectStore, error) {
	var store credentials.CredentialStore
	// Spelled as storage.NewConfiguredStore reads it.
	if strings.EqualFold(strings.TrimSpace(cfg.Storage.Provider), credentials.ProviderR2) {
		var err error
		if store, err = credentialStore(); err != nil {
			return nil, storageOpenError(credentialOS, err)
		}
	}
	return storage.NewConfiguredStore(ctx, cfg.Storage, store)
}

// skillObserver shares bounded observations within a pass: user-scope skill
// roots are read once per harness, project-scope roots once per
// harness/project. Each read carries its own instruction-content cap, so a
// pass reads at most that cap per harness plus that cap per project.
//
// An imported session gets no observation: today's skills attached to a
// session that ran before them would be false evidence.
func skillObserver(env Env, mode config.SkillEvidence) func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
	userCache := map[string][]archive.SupplementalEvidence{}
	projectCache := map[string][]archive.SupplementalEvidence{}
	return func(reg archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
		if reg.Imported() || mode == config.SkillEvidenceNone {
			return nil, nil
		}
		userScope, ok := userCache[reg.Harness.Name]
		if !ok {
			userHome, err := env.userHomeDir()
			if err != nil {
				return nil, err
			}
			userScope, err = evidence.ObserveSkills(evidence.SkillOptions{Harness: reg.Harness.Name, UserHome: userHome, ObservedAt: at, Mode: mode, Locations: skillEvidenceRoots(env, reg.Harness.Name, agentapi.SkillLocations{UserHome: userHome})})
			if err != nil {
				return nil, err
			}
			userCache[reg.Harness.Name] = userScope
		}
		projectKey := reg.Harness.Name + "\x00" + reg.ProjectRoot
		projectScope, ok := projectCache[projectKey]
		if !ok {
			var err error
			projectScope, err = evidence.ObserveSkills(evidence.SkillOptions{Harness: reg.Harness.Name, ProjectRoot: reg.ProjectRoot, ObservedAt: at, Mode: mode, Locations: skillEvidenceRoots(env, reg.Harness.Name, agentapi.SkillLocations{ProjectRoot: reg.ProjectRoot})})
			if err != nil {
				return nil, err
			}
			projectCache[projectKey] = projectScope
		}
		return append(append([]archive.SupplementalEvidence(nil), userScope...), projectScope...), nil
	}
}

func skillEvidenceRoots(env Env, name string, l agentapi.SkillLocations) []agentapi.SkillRoot {
	if p, ok := env.agentRegistry().LookupSkills(name); ok {
		return p.EvidenceRoots(l)
	}
	return nil
}

// pendingCodexRollouts requests the existing pass owner's metadata view lazily.
func pendingCodexRollouts(lookup *discovery.CodexRolloutLookup) func() agentapi.CodexRolloutLookup {
	return func() agentapi.CodexRolloutLookup {
		if lookup == nil {
			return nil
		}
		return lookup.MetadataInventory()
	}
}

// This short read scope binds the cached rooted observation to this pass deadline.
func checkDurablePassRoots(ctx context.Context, store *state.Store) error {
	scoped, closeScope := store.WithReadBudget(ctx, nil)
	defer closeScope()
	obligations, err := scoped.DurableStorageObligations()
	if err != nil {
		return err
	}
	for _, obligation := range obligations {
		if obligation.SessionID == "" {
			return state.ErrDurableStorageRecovery
		}
	}
	return ctx.Err()
}
