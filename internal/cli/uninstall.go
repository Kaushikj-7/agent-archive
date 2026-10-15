package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agentskills"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/issuance"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// Uninstall leaves data and credentials available for reinstall unless the
// explicit destructive option and its separate confirmation (or --yes) are
// supplied.
func runUninstallCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	fs := env.newCommandFlags("uninstall", stderr)
	purge := fs.Bool("delete-local-data", false, "also delete owned local files and stored credentials")
	skipScheduler := fs.Bool("skip-scheduler", false, "go on when the background scheduler cannot be reached, without verifying the job stopped")
	yes := fs.Bool("yes", false, "skip the confirmations")
	if !fs.parseFlagsOnly(args) {
		return 2
	}
	if !*yes && !env.interactive(stdin) {
		terminal.Println(stderr, "agent-archive: uninstall: confirming needs a terminal. Nothing was changed. Run again with --yes to uninstall without asking."+env.overrideHint(stdin))
		return 1
	}
	if err := uninstall(*purge, *yes, *skipScheduler, stdin, stdout, env); err != nil {
		terminal.Printf(stderr, "Uninstall incomplete: %v\n", err)
		return 1
	}
	return 0
}

func uninstall(purge, yes, skipScheduler bool, stdin io.Reader, out io.Writer, env Env) error {
	// Resolved without creating it: the data directory of a stale
	// installation (a test one whose temporary folder is gone) may no
	// longer exist, and uninstalling it must not bring it back.
	home, err := env.readHome()
	if err != nil {
		return err
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return err
	}
	if err = checkRemovableHome(home, userHome); err != nil {
		return err
	}
	// The locks below need the directory, so a missing one is created for
	// the length of the uninstall and removed again at the end, with any
	// parents created with it. This defer runs after the locks' own.
	created := missingDirs(home)
	defer removeCreatedDirs(home, created)
	if err = os.MkdirAll(home, 0700); err != nil {
		return err
	}
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return err
	}
	// A purge releases its locks early so it can delete the lock files; every
	// release is idempotent so the deferred calls cannot unlock twice.
	release = releaseOnce(release)
	defer release()
	// Issuance takes issued.lock before the collector lock. A purge follows
	// that order and refuses before changing hooks, credentials or lineage.
	// Ordinary uninstall keeps issuance state and does not need this lock.
	releaseIssued, err := lockUninstallIssuance(home, purge)
	if err != nil {
		return err
	}
	defer releaseIssued()
	if setupjournal.TransactionPending(home) {
		return errors.New(recoveryPending(home))
	}
	// Fail before prompting when the settings are unreadable; they are
	// loaded again below, once the collector lock is held too.
	previewCfg, previewFound, err := config.Load(home)
	if err != nil {
		return err
	}
	previewPending, confirmed, err := confirmUninstall(purge, yes, home, previewCfg, previewFound, stdin, out)
	if err != nil || !confirmed {
		return err
	}
	unlock, err := lockCollector(home, "uninstall", env.now())
	if err != nil {
		return fmt.Errorf("%s holds the collector lock; retry uninstall when it finishes: %w", lockHolder(home), err)
	}
	unlock = releaseOnce(unlock)
	defer unlock()
	releaseHooks, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		return err
	}
	releaseHooks = releaseOnce(releaseHooks)
	defer releaseHooks()
	releaseSearch, err := local.NamedLockWait(home, "search-worker.lock", 2*time.Second)
	if err != nil {
		return fmt.Errorf("search indexing is finishing; retry uninstall: %w", err)
	}
	releaseSearch = releaseOnce(releaseSearch)
	defer releaseSearch()
	// Reload after acquiring the lock; pause/resume may have completed meanwhile.
	cfg, found, err := config.Load(home)
	if err != nil {
		return err
	}
	if err := checkPurgePendingGrowth(home, cfg, found, purge, previewPending); err != nil {
		return err
	}

	in := env.installation(home, userHome)
	hookFiles := env.installedHookFiles(userHome, cfg)
	changes, skipped, err := planUninstallFiles(userHome, hookFiles, in, installedApps(cfg, found, in.owner().Ports), env.agentRegistry())
	if err != nil {
		return err
	}
	// Another installation's hooks stay; say so, so nobody expects them gone.
	for _, problem := range in.otherInstallationProblems(hookFiles, uninstallHookApps(in.owner().Ports, hookFiles, nil, nil)) {
		skipped = append(skipped, "Kept: "+problem)
	}
	// The collector for this data directory, and any an earlier release
	// installed for it under another label. Never another directory's.
	remove, unverified, err := stopCollectors(collectorRefs(in, userHome), out, userHome, env, skipScheduler)
	if err != nil {
		return err
	}
	// Disable capture before removing hooks. A partial uninstall remains safely disabled.
	if err = disableCaptureForUninstall(home, &cfg, found); err != nil {
		return err
	}

	if err = hooks.Apply(changes); err != nil {
		if errors.Is(err, hooks.ErrChanged) {
			return fmt.Errorf("%w; capture is already disabled, so rerun agent-archive uninstall to finish", err)
		}
		return err
	}
	agentskills.RemoveEmptyDirs(env.agentRegistry(), userHome, claudeConfigDir(hookFiles))
	for _, path := range remove {
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for _, problem := range skipped {
		terminal.Println(out, problem)
	}
	if purge {
		// A timed-out hook can still be writing an admission intent without
		// hooks.lock. Hold its queue lock while deleting the retry directory.
		releaseAdmission, lockErr := local.NamedLock(home, "admission-intents.lock")
		if lockErr != nil {
			return fmt.Errorf("lock pending session starts before purge: %w", lockErr)
		}
		releaseAdmission = releaseOnce(releaseAdmission)
		defer releaseAdmission()
		// The purge releases the locks once their files are gone.
		if err := purgeLocalData(home, cfg, out, env, func() {
			releaseAdmission()
			releaseSearch()
			releaseHooks()
			unlock()
			releaseIssued()
			release()
		}); err != nil {
			return err
		}
	}
	printUninstallSummary(out, unverified)
	return nil
}

// lockUninstallIssuance excludes key creation only when its lineage will be purged.
func lockUninstallIssuance(home string, purge bool) (func(), error) {
	if !purge {
		return func() {}, nil
	}
	release, err := local.NamedLock(home, "issued.lock")
	if err != nil {
		return nil, fmt.Errorf("key issuance is running; retry local-data purge when it finishes: %w", err)
	}
	return releaseOnce(release), nil
}

func printUninstallSummary(out io.Writer, unverified []unverifiedJob) {
	terminal.Println(out, "Bucket machine records and remote credentials remain. Remove access using your storage provider; uninstall does not revoke it.")
	if len(unverified) > 0 {
		printUnverifiedJobs(out, unverified)
		terminal.Println(out, "Uninstall complete, except that the background collector was not verified stopped (see above). Remote archives and the CLI executable were kept.")
		return
	}
	terminal.Println(out, "Uninstall complete. Remote archives and the CLI executable were kept.")
}

// confirmUninstall says what uninstall is about to do and asks, twice for a
// purge (or not at all with --yes). A purge first counts the sessions whose
// evidence it would delete before upload; that count is returned so it can
// be checked again under the locks. Declining is not an error: confirmed is
// false and nothing was changed.
func confirmUninstall(purge, yes bool, home string, previewCfg config.Config, previewFound bool, stdin io.Reader, out io.Writer) (previewPending int, confirmed bool, err error) {
	p := newPrompter(stdin, out)
	defer p.close()
	out = p.out
	guidedExplanation(out, "Remove integrations", "Remove the archive's hooks and background collector from this machine. Remote archives are kept.")
	if purge {
		terminal.Printf(out, "Also delete owned local state and credentials under %s.\n", home)
	} else {
		terminal.Println(out, "Local evidence, settings, and credentials will be kept. Run setup to reinstall.")
	}
	confirm := func(question string) (bool, error) {
		if yes {
			return true, nil
		}
		confirmed, err := p.guidedYesNo(question)
		if err == nil && !confirmed {
			terminal.Println(out, "Cancelled. No changes were made.")
		}
		return confirmed, err
	}
	if confirmed, err := confirm("Remove integrations?"); err != nil || !confirmed {
		return 0, false, err
	}
	if purge {
		pending, unreadable := unpublishedSessions(home, previewCfg, previewFound)
		for _, problem := range unreadable {
			terminal.Println(out, strings.Replace(problem, "status left it out", "it is deleted with the rest", 1))
		}
		previewPending = pending
		guidedExplanation(out, "Delete owned local data", fmt.Sprintf("%d pending session(s) and all owned local caches will be removed. Unpublished evidence cannot be recovered from the bucket.", pending))
		if confirmed, err := confirm("Delete local data and stored credentials too?"); err != nil || !confirmed {
			return 0, false, err
		}
	}
	return previewPending, true, nil
}

// collectorRefs are the jobs uninstall stops: the installation's own and the
// collectors earlier releases installed for it, and never the prototype's job.
// What blocks setup about the prototype's does not block uninstall, which
// leaves it alone.
func collectorRefs(in installation, userHome string) []scheduler.Ref {
	jobs, _ := in.installed(userHome)
	var refs []scheduler.Ref
	for _, job := range jobs {
		if job.Alias != scheduler.Prototype {
			refs = append(refs, job.Ref)
		}
	}
	return append(refs, in.searchRole().ref())
}

// unverifiedJob is a background job uninstall went on without verifying it
// stopped, because the scheduler could not be reached (--skip-scheduler).
type unverifiedJob struct {
	ref scheduler.Ref
	// words are the nouns of the scheduler that could not be reached, kept here
	// because the summary is printed after a purge has deleted the
	// configuration that names the scheduler.
	words scheduler.Words
	// why is what stopped the scheduler from answering or stopping the job.
	why string
	// manual is the command that stops the job by hand, "" when the
	// scheduler names none.
	manual string
}

// stopCollectors stops the background collectors the jobs ref name, and
// returns the files of their definitions to remove once the rest of uninstall
// is done. A job the scheduler runs from another installation's definition
// stays, and so do its files: removing them would leave this installation with
// nothing to reinstall from.
//
// A job the scheduler cannot be asked about refuses uninstall, with the
// scheduler's own next step and the command that stops the job by hand; with
// skipScheduler it goes on: the stop is tried, the definition's files are
// removed all the same, and the job is returned unverified, for the summary to
// say it was not verified stopped.
func stopCollectors(refs []scheduler.Ref, out io.Writer, userHome string, env Env, skipScheduler bool) (remove []string, unverified []unverifiedJob, err error) {
	words := env.scheduler().Words()
	statuses := make([]scheduler.Status, len(refs))
	for i, ref := range refs {
		statuses[i] = env.jobStatus(userHome, ref)
		if statuses[i].State == scheduler.Unknown && !skipScheduler {
			return nil, nil, errors.New(uninstallUnknownMessage(words, problemOf(statuses[i])))
		}
	}
	for i, ref := range refs {
		status := statuses[i]
		if status.State == scheduler.Unknown {
			problem := problemOf(status)
			if !skipScheduler {
				return nil, nil, errors.New(uninstallUnknownMessage(words, problem))
			}
			// The scheduler said it cannot tell; asking it to stop the job
			// is the attempt, and whether it worked is the answer. One it
			// answers the second time is another installation's is left.
			err := env.unloadJob(userHome, ref)
			if notOwned(err) {
				leftAnotherInstallation(out, words, ref, status)
				continue
			}
			if err != nil {
				unverified = append(unverified, unverifiedJob{ref: ref, words: words, why: cmp.Or(problem.Reason, words.Tool+" did not say whether the job is loaded"), manual: problem.Manual})
			}
			remove = append(remove, status.Paths...)
			continue
		}
		if status.State == scheduler.AnotherInstallation {
			leftAnotherInstallation(out, words, ref, status)
			continue
		}
		if jobActive(status.State) {
			if err = env.unloadJob(userHome, ref); err != nil {
				if !skipScheduler {
					return nil, nil, fmt.Errorf("stop collector: %w", err)
				}
				if notOwned(err) {
					leftAnotherInstallation(out, words, ref, status)
					continue
				}
				unverified = append(unverified, failedStop(ref, words, err))
			}
		}
		remove = append(remove, status.Paths...)
	}
	return remove, unverified, nil
}

// failedStop is the loaded job ref names, whose stop failed with err, as
// uninstall --skip-scheduler reports it. A manager that could no longer be
// asked about the job when the stop came (Unload's *IndeterminateError) says
// what is wrong and the command that stops the job by hand, as for a job it
// could not describe in the first place.
func failedStop(ref scheduler.Ref, words scheduler.Words, err error) unverifiedJob {
	job := unverifiedJob{ref: ref, words: words, why: err.Error()}
	var indeterminate *scheduler.IndeterminateError
	if errors.As(err, &indeterminate) {
		job.why = cmp.Or(indeterminate.Problem.Reason, job.why)
		job.manual = indeterminate.Problem.Manual
	}
	return job
}

// leftAnotherInstallation says that the job ref names was left running, with
// its definition, because the scheduler runs it from another installation's.
func leftAnotherInstallation(out io.Writer, words scheduler.Words, ref scheduler.Ref, status scheduler.Status) {
	terminal.Printf(out, "Left %s's %s job running: it was loaded from another %s, so it belongs to another installation. %s was kept.\n", words.Manager, ref, words.Definition, definitionPath(status))
}

// notOwned reports whether err is the scheduler's refusal to stop a job it
// runs from another installation's definition.
func notOwned(err error) bool {
	var notOwned *scheduler.NotOwnedError
	return errors.As(err, &notOwned)
}

// uninstallUnknownMessage says that uninstall stops because the scheduler
// cannot say whether the job is loaded. An adapter that says what is wrong has
// it said, with its next step, the command that stops the job by hand, and the
// way to go on without the scheduler; otherwise it is the words it always was.
func uninstallUnknownMessage(words scheduler.Words, problem scheduler.Problem) string {
	if problem.Reason == "" {
		return fmt.Sprintf("cannot determine background job state; restore access to %s and retry", words.Tool)
	}
	message := fmt.Sprintf("cannot determine background job state: %s. %s.", problem.Reason, problem.Fix)
	if problem.Manual != "" {
		message += fmt.Sprintf(" To stop the job by hand, run this from a session that can reach %s: %s.", words.Manager, problem.Manual)
	}
	return message + " To uninstall anyway, without verifying the job stopped, run agent-archive uninstall --skip-scheduler."
}

// printUnverifiedJobs says which background jobs uninstall went on without
// verifying stopped, why, and the command that stops each by hand: deleting a
// definition under a job that is still loaded leaves it running.
func printUnverifiedJobs(out io.Writer, jobs []unverifiedJob) {
	for _, job := range jobs {
		words := job.words
		terminal.Printf(out, "Not verified stopped: %s's %s job may still be running, because %s.\n", words.Manager, job.ref, job.why)
		if job.manual != "" {
			terminal.Printf(out, "To stop it, run this from a session that can reach %s: %s\n", words.Manager, job.manual)
		} else {
			terminal.Printf(out, "To stop it, use %s from a session that can reach %s.\n", words.Tool, words.Manager)
		}
	}
}

// purgeLocalData runs once hooks and the LaunchAgent are gone. It deletes
// every stored credential that cfg or the saved setup names, and every owned
// local file. It unlinks the lock files while they are still held, then calls
// releaseLocks, and removes the data directory itself if nothing unrelated
// remains in it.
func purgeLocalData(home string, cfg config.Config, out io.Writer, env Env, releaseLocks func()) error {
	refs := map[string]bool{}
	if slots, err := issuance.List(home); err == nil {
		for _, slot := range slots {
			refs[slot.SecretRef] = true
		}
	} else {
		terminal.Println(out, "Issuance ledger unreadable; additional credential references may remain in the credential store.")
	}
	for _, ref := range cfg.SpareCredentialRefs {
		refs[ref] = true
	}
	for _, ref := range cfg.RetiredCredentialRefs {
		refs[ref] = true
	}
	if cfg.Storage.R2CredentialRef != "" {
		refs[cfg.Storage.R2CredentialRef] = true
	}
	for _, old := range cfg.PreviousDestinations {
		if old.R2CredentialRef != "" {
			refs[old.R2CredentialRef] = true
		}
	}
	draft, _, problem, e := readDraft(home)
	if e != nil {
		return fmt.Errorf("read the saved setup %s: %w", draftPath(home), e)
	}
	if problem != "" {
		// It is deleted with the rest; only the Keychain items it may
		// name are out of reach.
		if note := unreadableDraftUninstallNote(credentialOS, draftPath(home), problem); note != "" {
			terminal.Println(out, note)
		}
	}
	if draft.CredentialRef != "" {
		refs[draft.CredentialRef] = true
	}
	for _, ref := range draft.StagedRefs {
		refs[ref] = true
	}
	// A Keychain that cannot delete an item must not strand the rest of
	// the purge: hooks and the LaunchAgent are already gone, so local
	// files are still removed and the items left behind are named, since
	// once config.json is gone nothing else records them.
	folder := lookCredentialFolder(home)
	undeleted, keychainErr := deleteCredentialRefs(env, refs)
	leftovers, e := removeLocalState(home)
	if e != nil {
		return e
	}
	// Local state is gone. Unlink the lock files while they are still
	// held, so a hook, collector, or setup that opens one from now on
	// creates a fresh inode it owns outright instead of acquiring this
	// one after its release; then release them and remove the directory
	// itself if nothing unrelated remains in it.
	removeLockFiles(home)
	releaseLocks()
	if len(leftovers) == 0 {
		if e := os.Remove(home); e != nil && !os.IsNotExist(e) && !isDirectoryNotEmpty(e) {
			return e
		}
	}
	var problems []string
	if len(leftovers) > 0 {
		problems = append(problems, fmt.Sprintf("unrelated files were kept in %s: %s", home, strings.Join(leftovers, ", ")))
	}
	if len(undeleted) > 0 {
		// The account names are opaque random references
		// ("setup-<hex>", see setup.go) that reveal nothing about the
		// stored secret, and after this purge nothing else records them,
		// so they are printed here on purpose (see the PR A3 ledger
		// entry). The recovery is uninstall-specific: there is no
		// configuration left to sync or re-run setup against.
		if problem := undeletedCredentialsProblem(credentialOS, home, undeleted, keychainErr, folder.afterPurge(home)); problem != "" {
			problems = append(problems, problem)
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// unpublishedSessions counts the sessions whose evidence a purge would
// delete before it was uploaded: what status reports as pending, under the
// configuration uninstall loaded. Without a configuration nothing says which
// sessions would have been uploaded, so every one with work outstanding
// counts. (A zero configuration would admit none: every registration records
// a destination it does not have.)
//
// A registration or request that cannot be read does not stop the purge,
// which deletes it anyway: it is named in unreadable, and its session is not
// counted.
func unpublishedSessions(home string, cfg config.Config, found bool) (count int, unreadable []string) {
	accept := cfg.AcceptSession
	if !found {
		accept = func(archive.SessionRegistration) bool { return true }
	}
	store := state.OpenReadOnly(home)
	regs, reqs, unreadable := statusState(home, store)
	queued := state.QueuedRequests(reqs)
	for _, reg := range regs {
		if !accept(reg) {
			continue
		}
		owed, err := store.Outstanding(reg, queued[reg.ArchiveSessionID])
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("Local state of session %s could not be read (%v).", reg.ArchiveSessionID, err))
			continue
		}
		if owed.Pending() {
			count++
		}
	}
	return count, unreadable
}

// installedApps is the apps whose hooks setup installed, per the committed
// configuration. An empty list in a configuration means every app (see
// config.Config.Harnesses); with no configuration at all, none is known.
func installedApps(cfg config.Config, found bool, lookups ...agentapi.HooksLookup) []string {
	if found && len(cfg.Harnesses) == 0 {
		if len(lookups) > 0 {
			return uninstallHookApps(lookups[0], hooks.Files(cfg.HookFiles), nil, nil)
		}
		return uninstallHookApps(productionAgents, hooks.Files(cfg.HookFiles), nil, nil)
	}
	return cfg.Harnesses
}

// uninstallHookApps starts with injected managed-hook owners and then includes
// recorded paths and installed owners. Codex keeps its established first position.
func uninstallHookApps(ports agentapi.HooksLookup, files, legacy hooks.Files, installed []string) []string {
	if ports == nil {
		ports = productionAgents
	}
	apps := append([]string(nil), ports.HookAgents()...)
	for i, app := range apps {
		if app == string(agentmeta.Codex) {
			copy(apps[1:i+1], apps[:i])
			apps[0] = app
			break
		}
	}
	var recorded []string
	for app := range files {
		recorded = append(recorded, app)
	}
	for app := range legacy {
		recorded = append(recorded, app)
	}
	sort.Strings(recorded)
	for _, app := range append(recorded, installed...) {
		if !containsString(apps, app) {
			apps = append(apps, app)
		}
	}
	return apps
}

// planUninstallHooks plans removing owner's handlers (see hooks.Hook) from
// every app's hook file (files), and from its legacy path too when that
// differs, where an earlier release may have left them. Another
// installation's handlers are never removed. An installed app's file must be readable, or
// its hooks would stay behind. Any other file is only checked for
// leftovers, so one that cannot be parsed (the user's own, half-edited
// ~/.cursor/hooks.json, say) is reported in skipped and left alone rather
// than blocking the collector's removal.
func planUninstallHooks(files, legacy hooks.Files, owner hooks.Hook, installed []string) (changes []hooks.Change, skipped []string, err error) {
	for _, app := range uninstallHookApps(owner.Ports, files, legacy, installed) {
		for i, set := range []hooks.Files{files, legacy} {
			if set[app] == "" {
				if i == 0 && containsString(installed, app) {
					return nil, nil, fmt.Errorf("missing installed hook settings path for %s", app)
				}
				continue
			}
			if i == 1 && local.CanonicalPath(legacy[app]) == local.CanonicalPath(files[app]) {
				continue
			}
			change, found, err := hooks.PlanRemovalOf(set, owner, app)
			if err != nil {
				if containsString(installed, app) && i == 0 {
					return nil, nil, err
				}
				skipped = append(skipped, fmt.Sprintf("Skipped %v. Setup did not install %s hooks there, so the file was left as it is.", err, appName(app)))
				continue
			}
			if found {
				duplicate := false
				for _, prior := range changes {
					// Hardlinks remain separate atomic-replacement destinations.
					if local.CanonicalPath(prior.Path) != local.CanonicalPath(change.Path) {
						continue
					}
					// Distinct owners may select one file only when their complete plans
					// agree. Otherwise refuse rather than lose handlers or apply stale bytes.
					if prior.Delete != change.Delete || prior.Mode != change.Mode || prior.Existed != change.Existed || !bytes.Equal(prior.After, change.After) || !bytes.Equal(prior.Before, change.Before) {
						return nil, nil, fmt.Errorf("hook owners have conflicting removal plans for %s", change.Path)
					}
					duplicate = true
					break
				}
				if !duplicate {
					changes = append(changes, change)
				}
			}
		}
	}
	return changes, skipped, nil
}

// planUninstallFiles is planUninstallHooks for the hook files setup
// installed into (files) and their legacy paths, followed by removing the
// agent skill files setup wrote (/handoff). A file at one of their paths that is
// not setup's stays, with a line in skipped.
func planUninstallFiles(userHome string, files hooks.Files, in installation, installed []string, lookups ...agentapi.SkillsLookup) (changes []hooks.Change, skipped []string, err error) {
	if changes, skipped, err = planUninstallHooks(files, legacyHookFiles(userHome, in.owner().Ports), in.owner(), installed); err != nil {
		return nil, nil, err
	}
	var sources agentapi.SkillsLookup = productionAgents
	if len(lookups) > 0 {
		sources = lookups[0]
	}
	removals, kept, err := agentskills.PlanRemoval(sources, userHome, claudeConfigDir(files), in.commandDataHome())
	if err != nil {
		return nil, nil, err
	}
	for _, path := range kept {
		skipped = append(skipped, fmt.Sprintf("Kept %s: it is not this agent-archive installation's (it lacks the marker line, or names another data directory).", displayPath(path, userHome)))
	}
	return append(changes, removals...), skipped, nil
}

// deleteCredentialRefs deletes every referenced Keychain item it can. It
// returns the references it could not delete, sorted, with the first error;
// an item that is already absent counts as deleted.
func deleteCredentialRefs(env Env, refs map[string]bool) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	sorted := make([]string, 0, len(refs))
	for ref := range refs {
		sorted = append(sorted, ref)
	}
	sort.Strings(sorted)
	kc, err := env.credentialStore()
	if err != nil {
		return sorted, err
	}
	var undeleted []string
	var firstErr error
	for _, ref := range sorted {
		if e := kc.Delete(context.Background(), ref); e != nil && !errors.Is(e, credentials.ErrMissingCredential) {
			undeleted = append(undeleted, ref)
			if firstErr == nil {
				firstErr = e
			}
		}
	}
	return undeleted, firstErr
}

// uninstallLockFiles are the lock files a purge unlinks after every other
// local entry is gone and while it still holds them: held, they are what
// keeps a hook, collector, or setup from acting on a half-deleted directory,
// and unlinking before release means a later opener gets its own inode.
var uninstallLockFiles = []string{"search-worker.lock", "admission-intents.lock", "hooks.lock", "collector.lock", "issued.lock", "setup.lock"}

// missingDirs is dir and each of its parents that does not exist, deepest
// first: what os.MkdirAll(dir) would create.
func missingDirs(dir string) []string {
	var missing []string
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); !errors.Is(err, fs.ErrNotExist) {
			return missing
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			return missing
		}
	}
}

// removeCreatedDirs removes what uninstall created in a data directory that
// did not exist (its lock files, then created, deepest first), each only if
// nothing else has appeared in it: an installation whose directory was
// already gone is left without one.
func removeCreatedDirs(home string, created []string) {
	if len(created) == 0 {
		return
	}
	removeLockFiles(home)
	_ = os.Remove(filepath.Join(home, collectorLockRecordName))
	for _, dir := range created {
		// A purge has already removed an emptied data directory itself.
		if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return
		}
	}
}

func removeLockFiles(home string) {
	for _, name := range uninstallLockFiles {
		_ = os.Remove(filepath.Join(home, name))
	}
}

func isDirectoryNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}

// releaseOnce makes a lock release safe to call more than once. Calling a
// local.NamedLock release twice would unlock and close its file descriptor
// again, by which time the number may belong to another open file.
func releaseOnce(release func()) func() {
	var once sync.Once
	return func() { once.Do(release) }
}

// checkRemovableHome refuses to touch a data directory that is obviously
// not agent-archive's own: AGENT_ARCHIVE_HOME pointed at the user's home
// directory or a filesystem root. removeLocalState is the second guard: it
// only ever deletes entries agent-archive itself creates.
func checkRemovableHome(home, userHome string) error {
	clean := filepath.Clean(home)
	if clean == filepath.Clean(userHome) || filepath.Dir(clean) == clean {
		return fmt.Errorf("refusing to remove %s: not an agent-archive data directory", home)
	}
	return nil
}

// localStateEntries is every top-level entry agent-archive creates under its
// data directory besides state.Store's own (state.OwnedEntries, which
// removeLocalState adds): internal/config's config.json and setup's files,
// backfill's import batches (imports/), local.Lock's lock file and its
// record, the collector's advisory files, the LaunchAgent's log files,
// `list`'s disposable metadata cache (reader.OpenMetadataCache), and the
// untrimmed handoffs `handoff` saves, and the credentials folder a build
// without a Keychain keeps R2 keys in. Keep it in sync with those packages;
// an entry missing here is left behind by uninstall (and reported), never
// silently deleted.
var localStateEntries = []string{
	"discovery-catalog.json", "discovery-health.json", "discovery-refused-starts.json",
	machineRegistrationFile, "config.json", "setup-draft.json", "setup-transaction.json", "imports",
	"storage-health.json", "capture-diagnostics.json", "diagnostics.lock", "application-versions.json",
	"admission-intents", "admission-intents.lock", "admission-replay-cursor.json",
	"search", "search-worker.lock", "search.log", "search-error.log",
	"collector.lock", collectorLockRecordName, "collector.log", "collector-error.log",
	"cache", handoffDir, "purge-plans", "issued", "issued.lock", "revocations", "revocations.lock", ownKeyFile,
	// The credential files kept where there is no Keychain (Linux).
	credentials.CredentialsDirName,
}

// removeLocalState deletes agent-archive's own entries under home (see
// localStateEntries, plus local.WriteBytes's ".pending-*" temp files) and
// then the directory itself. It never deletes anything else: a data
// directory a user pointed AGENT_ARCHIVE_HOME at may hold their own files,
// and those are returned as leftover, with the directory left in place,
// rather than removed.
func removeLocalState(home string) (leftover []string, err error) {
	entries, err := os.ReadDir(home)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, name := range append(state.OwnedEntries(), localStateEntries...) {
		known[name] = true
	}
	for _, entry := range entries {
		name := entry.Name()
		//lint:ignore LV1001 file names found in the state directory, an open set; these are the lock files
		if name == "search-worker.lock" || name == "setup.lock" || name == "hooks.lock" || name == "collector.lock" || name == "admission-intents.lock" || name == "issued.lock" {
			continue
		}
		if !known[name] && !strings.HasPrefix(name, ".pending-") && !isMovedAside(name, known) {
			leftover = append(leftover, name)
			continue
		}
		if err := os.RemoveAll(filepath.Join(home, name)); err != nil {
			return nil, err
		}
	}
	if len(leftover) > 0 {
		return leftover, nil
	}
	// Lock files are skipped here because they are still held; uninstall
	// removes them after releasing them (see removeLockFiles).
	return nil, nil
}

func checkPurgePendingGrowth(home string, cfg config.Config, found, purge bool, preview int) error {
	if purge {
		if pending, _ := unpublishedSessions(home, cfg, found); pending > preview {
			return errors.New("new pending evidence appeared while confirming; rerun uninstall to review it")
		}
	}
	return nil
}

func disableCaptureForUninstall(home string, cfg *config.Config, found bool) error {
	if !found {
		return nil
	}
	cfg.Archive.Enabled = false
	return config.Save(home, *cfg)
}
