package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentskills"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/retention"
	"github.com/wangjohn/agent-archive/internal/scheduler"
	"github.com/wangjohn/agent-archive/internal/scheduler/host"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

func discardDraft(home string, draft setupDraft, active config.Config, env Env) error {
	if err := abandonGuidedStage(home, draft, active); err != nil {
		return err
	}
	refs := append([]string{}, draft.StagedRefs...)
	if draft.CredentialRef != "" && !containsString(refs, draft.CredentialRef) {
		refs = append(refs, draft.CredentialRef)
	}
	for _, ref := range refs {
		if ref == active.Storage.R2CredentialRef {
			continue
		}
		kc, err := env.credentialStore()
		if err != nil {
			return err
		}
		if err = kc.Delete(context.Background(), ref); err != nil && !errors.Is(err, credentials.ErrMissingCredential) {
			return err
		}
	}

	err := os.Remove(filepath.Join(home, "setup-draft.json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// destinationEqual reports whether two storage configurations are the same
// destination. It compares destination IDs, so a destination change is
// exactly a change of the ID registrations record.
func destinationEqual(a, b credentials.Config) bool {
	return config.DestinationID(a) == config.DestinationID(b)
}

// pendingSessionCounts splits the pending sessions in two. waiting counts
// those only waiting for their transcript (state.Outstanding's
// WaitingForTranscript): nothing of such a session can be published
// anywhere until a path arrives, so its queued request is not work a sync
// could finish. blocking counts everything else.
func pendingSessionCounts(home string, cfg config.Config) (blocking, waiting int, err error) {
	store := state.OpenReadOnly(home)
	regs, err := store.LoadRegistrations()
	if err != nil {
		return 0, 0, err
	}
	reqs, err := store.LoadRequests()
	if err != nil {
		return 0, 0, err
	}
	queued := state.QueuedRequests(reqs)
	for _, r := range regs {
		if !cfg.AcceptSession(r) {
			continue
		}
		owed, err := store.Outstanding(r, queued[r.ArchiveSessionID])
		if err != nil {
			return 0, 0, err
		}
		switch {
		case !owed.Pending():
		case owed.SyncCanFinish():
			blocking++
		default:
			waiting++
		}
	}
	return blocking, waiting, nil
}

// sessionsAdmittedInto counts the registrations that record cfg's
// destination and that cfg accepts: when setup switches back to a
// destination used before, these resume there. Subagents go with their
// parents and are not counted. A registration without a destination ID
// stays behind.
func sessionsAdmittedInto(home string, cfg config.Config) (int, error) {
	regs, err := state.OpenReadOnly(home).LoadRegistrations()
	if err != nil {
		return 0, err
	}
	id, count := cfg.DestinationID(), 0
	for _, r := range regs {
		if r.ParentSessionID == "" && r.DestinationID == id && cfg.AcceptSession(r) {
			count++
		}
	}
	return count, nil
}

func reviewChanges(home string, old, next config.Config, p *prompter, env Env) error {
	if old.EffectiveCodexCaptureScope() != next.EffectiveCodexCaptureScope() {
		if next.EffectiveCodexCaptureScope() == config.CodexAllProjects {
			p.warn("Codex scope expands to all current and future projects (Codex only).",
				"New admissions start after this local consent; old history and paused or excluded starts remain out of scope.")
		} else {
			p.warn("Codex scope reduces to included projects. Capture outside remaining scope stops; published archives are retained.")
		}
	}
	if old.MachineID == "" {
		return nil
	}
	if env.copiedFromAnotherMachine(old) {
		first, rest := copiedMachineWarning(home)
		p.warn(first, rest...)
	}
	if !destinationEqual(old.Storage, next.Storage) {
		// A session still waiting for its transcript has nothing a sync could
		// publish, so it must not hold the user at the old destination.
		pending, waiting, err := pendingSessionCounts(home, old)
		if err != nil {
			return err
		}
		if pending > 0 {
			return fmt.Errorf("%d session(s) still pending at the current destination; run agent-archive sync before changing storage", pending)
		}
		returning, err := sessionsAdmittedInto(home, next)
		if err != nil {
			return err
		}
		p.warn("Storage is changing. Sessions already archived stay at the old destination,",
			"and this machine stops adding to or cleaning up there. Nothing is deleted from either bucket;",
			"this machine's local copies are removed once they pass the retention period.")
		if returning > 0 {
			p.warn(fmt.Sprintf("%d session(s) from when this destination was used before resume uploading there,", returning),
				"and are deleted from it once they pass the retention period.")
		}
		if waiting > 0 {
			p.warn(fmt.Sprintf("%d session(s) never received a transcript (for example a Cursor chat with transcripts turned off).", waiting),
				"They published nothing and will not be captured at the new destination either.")
		}
	}
	if config.RetentionShorter(next.RetentionDays, old.RetentionDays) {
		store := state.OpenReadOnly(home)
		regs, err := store.LoadRegistrations()
		if err != nil {
			return err
		}
		cutoff := env.now().Add(-time.Duration(next.RetentionDays) * 24 * time.Hour)
		count, localCount := 0, 0
		preview, err := retention.NewExpiryPreview(store, env.now(), next.AcceptSession)
		if err != nil {
			return fmt.Errorf("cannot preview retention effect: %w", err)
		}
		for _, r := range regs {
			if r.ParentSessionID != "" && !r.NativeChild {
				continue
			}
			expired, err := preview.SessionExpired(r, config.RetentionDuration(next.RetentionDays))
			if err != nil {
				return fmt.Errorf("cannot preview retention effect: %w", err)
			}
			if expired {
				if next.InCurrentDestination(r) {
					count++
				} else {
					localCount++
				}
			}
		}

		if localCount > 0 {
			p.warn(fmt.Sprintf("Shorter retention: %s from previous destinations will be eligible for local cleanup; their remote archives stay untouched.", countNoun(localCount, "session")))
		}
		// The cutoff is a time of day, so the date says "on or before": a
		// session from earlier that day is counted too. It is the date where
		// the user is, as now is.
		if count > 0 {
			p.warn(fmt.Sprintf("Shorter retention: %s captured on or before %s will be eligible for deletion.", countNoun(count, "session"), cutoff.Format(time.DateOnly)),
				"Future cleanup also applies this policy.")
		}
	}
	return nil
}

func fileChange(path string, after []byte) (hooks.Change, error) {
	before, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return hooks.Change{}, err
	}
	return hooks.Change{Path: path, Before: before, After: after, Existed: err == nil, Mode: 0600}, nil
}

// carriedImportedHarnesses is the ImportedHarnesses a setup commits. Backfill
// writes the list, so it comes from the committed configuration, never a
// draft. An app whose hooks setup now installs moves to Harnesses, where it
// admits its imports too, and an app the person chose to stop publishing
// imports for (stopImported) is dropped.
func carriedImportedHarnesses(committed, harnesses, stopImported []string) []string {
	var out []string
	for _, app := range committed {
		if !containsString(harnesses, app) && !containsString(stopImported, app) {
			out = append(out, app)
		}
	}
	return out
}

func applySetup(home, userHome, executable string, old config.Config, next *config.Config, stopImported []string, env Env) error {
	if len(next.Harnesses) > 0 && setupNeedsProject(*next) {
		return errors.New("no project is included; include a project before saving these capture settings")
	}
	unlock, err := lockCollector(home, "setup", env.now())
	if err != nil {
		return fmt.Errorf("%s holds the collector lock; retry setup when it finishes: %w", lockHolder(home), err)
	}
	defer unlock()
	releaseHooks, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		return err
	}
	defer releaseHooks()
	current, _, err := config.Load(home)
	if err != nil {
		return err
	}
	// The background collector refreshes bucket privacy evidence in place
	// while setup is open. It is evidence, not a setting, so it neither counts
	// as a concurrent change nor gets overwritten by an older draft report.
	if !reflect.DeepEqual(withoutBucketPrivacy(current), withoutBucketPrivacy(old)) {
		return fmt.Errorf("settings changed while setup was open; restart setup to review the current settings")
	}
	if err := protectSetupWriter(home, current, *next); err != nil {
		return err
	}
	if fresher := freshestBucketPrivacy(*next, current.BucketPrivacy); fresher != nil {
		next.BucketPrivacy = fresher
	}
	mergeCommittedSetupState(old, next, stopImported)
	if err := prepareSetupConfig(home, executable, old, next, env); err != nil {
		return err
	}
	journal, err := planSetupTransaction(home, userHome, executable, old, next, env)
	if err != nil {
		return err
	}
	err = setupjournal.Commit(home, journal, env.backends())
	// A command file created and rolled back, or removed, leaves the
	// directories written for it; they go while empty.
	agentskills.RemoveEmptyDirs(env.agentRegistry(), userHome, claudeConfigDir(env.hookFiles(userHome)))
	agentskills.RemoveEmptyDirs(env.agentRegistry(), userHome, claudeConfigDir(env.installedHookFiles(userHome, old)))
	return err
}

// planAgentSkills is the journal changes for the agent skills of cfg: its
// apps' skill files, or, when it turns them off (cfg.NoSkills), the removal
// of every skill file of this installation's, in claudeDir and where
// Claude Code's configuration was when setup last ran (previousClaudeDir).
// Only a file setup wrote is replaced or removed (see agentskills.PlanInstall).
// While they are off the other files at the skills' paths are returned in
// kept, for setup to say it left them.
func planAgentSkills(userHome, claudeDir, previousClaudeDir string, cfg config.Config, executable, dataHome string, sources ...agentapi.SkillsLookup) (changes []hooks.Change, kept []string, err error) {
	ports := agentapi.SkillsLookup(productionAgents)
	if len(sources) > 0 {
		ports = sources[0]
	}
	if !cfg.NoSkills {
		changes, _, err = agentskills.PlanInstall(ports, userHome, claudeDir, cfg.Harnesses, executable, dataHome, previousClaudeDir)
		return changes, nil, err
	}
	if changes, _, err = agentskills.PlanInstall(ports, userHome, claudeDir, nil, executable, dataHome, previousClaudeDir); err != nil {
		return nil, nil, err
	}
	_, kept, err = agentskills.PlanRemoval(ports, userHome, claudeDir, dataHome)
	return changes, kept, err
}

// claudeConfigDir is Claude Code's configuration directory, which holds its
// hook file (files) and its skills.
func claudeConfigDir(files hooks.Files) string { return filepath.Dir(files["claude"]) }

// mergeCommittedSetupState carries operational ownership from the committed
// configuration, never from a resumable draft. It does no I/O.
func mergeCommittedSetupState(old config.Config, next *config.Config, stopImported []string) {
	config.PreserveWriterFence(next, old)
	// Operational ownership comes from committed state, never a resumable
	// draft. A crash after commit can leave a pre-commit draft on disk.
	next.RetiredMachineAssignments = append([]config.MachineAssignment(nil), old.RetiredMachineAssignments...)
	next.DestinationSince = old.DestinationSince
	next.PreviousDestinations = append([]credentials.Config(nil), old.PreviousDestinations...)
	next.ImportedHarnesses = carriedImportedHarnesses(old.ImportedHarnesses, next.Harnesses, stopImported)
	for _, ref := range old.RetiredCredentialRefs {
		if !containsString(next.RetiredCredentialRefs, ref) {
			next.RetiredCredentialRefs = append(next.RetiredCredentialRefs, ref)
		}
	}
}

// prepareSetupConfig checks the destination and newly included folders under
// setup's locks, then completes the settings the transaction will write.
func prepareSetupConfig(home, executable string, old config.Config, next *config.Config, env Env) error {
	var err error
	if old.MachineID != "" && !destinationEqual(old.Storage, next.Storage) {
		// The same rule as reviewChanges: a session waiting for its
		// transcript does not block the change, and like any unpublished
		// session it falls behind the new DestinationSince.
		pending, _, err := pendingSessionCounts(home, old)
		if err != nil {
			return err
		}
		if pending > 0 {
			return fmt.Errorf("new pending work appeared at the old destination; sync it before retrying")
		}
		next.DestinationSince = env.now().UTC()
		next.PreviousDestinations = append(next.PreviousDestinations, old.Storage)
	}
	if old.Storage.R2CredentialRef != "" && old.Storage.R2CredentialRef != next.Storage.R2CredentialRef {
		next.RetiredCredentialRefs = append(next.RetiredCredentialRefs, old.Storage.R2CredentialRef)
	}
	// Only a project this setup includes is checked: one that was already
	// included may be a folder backfill imported that no longer exists, and
	// it must not block every later change to the setup.
	alreadyIncluded := map[string]bool{}
	for _, project := range old.Archive.Projects {
		if project.Included {
			alreadyIncluded[project.Root] = true
		}
	}
	for _, project := range next.Archive.Projects {
		if project.Included && !alreadyIncluded[project.Root] {
			info, e := os.Stat(project.Root)
			if e != nil || !info.IsDir() {
				return fmt.Errorf("included project is no longer a directory: %s", project.Root)
			}
		}
	}
	if old.MachineID != "" {
		next.MachineID = old.MachineID
	} else if !config.ValidMachineID(next.MachineID) {
		next.MachineID = ""
	}
	if next.MachineID == "" {
		next.MachineID, err = local.ID()
		if err != nil {
			return err
		}
	}
	prepareMachineAssignment(old, next)
	if err := next.ValidateCloudflareTokenCommand(); err != nil {
		return err
	}
	if err := next.ValidateMachine(); err != nil {
		return err
	}
	// The machine this data directory was set up on, recorded once beside the
	// machine ID and kept when it differs from this one's (see
	// copiedFromAnotherMachine), so the warning does not go away by running
	// setup again on a copy. Linux only: nothing is recorded elsewhere.
	next.HostID = cmp.Or(old.HostID, env.hostFingerprint())
	next.SchemaVersion = config.SchemaVersion
	next.Paused = old.Paused
	next.PauseGeneration = old.PauseGeneration
	next.Archive.Enabled = true
	next.Archive.MachineID = next.MachineID
	next.Archive.SchemaVersion = 1
	for i := range next.Archive.Projects {
		for _, prior := range old.Archive.Projects {
			if prior.Root == next.Archive.Projects[i].Root {
				next.Archive.Projects[i].ActivatedAt = prior.ActivatedAt
				break
			}
		}
		if next.Archive.Projects[i].ActivatedAt.IsZero() {
			next.Archive.Projects[i].ActivatedAt = env.now().UTC()
		}
	}
	// Record the exact path the hooks and LaunchAgent are about to run, so
	// status checks them against it rather than against whatever path status
	// was later started through. It is written with the same transaction.
	next.InstalledExecutable = executable
	// The scheduler this setup defines the job under, so status, uninstall,
	// refresh and recovery address the job through the adapter that made it
	// (launchd is left out: see host.Recorded).
	next.BackgroundBackend = host.Recorded(env.scheduler().Name())
	return nil
}

// planSetupTransaction builds the hook, LaunchAgent, config, and job changes.
// It does not apply them; setupjournal.Commit owns that transaction boundary.
func planSetupTransaction(home, userHome, executable string, old config.Config, next *config.Config, env Env) (setupjournal.Journal, error) {
	// Hooks go where the apps read them in the environment setup runs in;
	// the paths are recorded so later commands find them without it.
	files := env.hookFiles(userHome)
	previousFiles := env.installedHookFiles(userHome, old)
	next.HookFiles = map[string]string{}
	for _, app := range next.Harnesses {
		next.HookFiles[app] = files[app]
	}
	if next.Discovery == nil {
		next.Discovery = old.Discovery
	}
	if next.Discovery != nil {
		d := *next.Discovery
		if len(d.CodexHomes) == 0 && files["codex"] != "" {
			d.CodexHomes = []string{filepath.Dir(files["codex"])}
		}
		next.Discovery = &d
	}
	if err := config.ReconcileDiscovery(next, old, env.now()); err != nil {
		return setupjournal.Journal{}, err
	}
	// Another installation's hooks in a file this one would install into
	// mean every session would be captured twice; they are its to remove.
	if problems := env.installation(home, userHome).otherInstallationProblems(files, next.Harnesses); len(problems) > 0 {
		return setupjournal.Journal{}, &otherInstallationError{problems: problems}
	}
	// Compose removals and installations through native ports before journaling;
	// shared destinations must have one original-to-final atomic change.
	inHooks := env.installation(home, userHome)
	changes, err := hooks.PlanReconfiguration(files, previousFiles, inHooks.hook(executable), inHooks.owner(), next.Harnesses, old.Harnesses)
	if err != nil {
		return setupjournal.Journal{}, err
	}
	// The agent skills (/handoff), for the apps chosen, or none while they
	// are turned off. Only a file setup wrote is replaced or removed.
	commands, _, err := planAgentSkills(userHome, claudeConfigDir(files), claudeConfigDir(previousFiles), *next, executable, env.installation(home, userHome).commandDataHome(), env.agentRegistry())
	if err != nil {
		return setupjournal.Journal{}, err
	}
	changes = append(changes, commands...)
	in := env.installation(home, userHome)
	// The collector gets the AWS files and PATH this storage was just
	// verified with; launchd would otherwise start it with none of them.
	plan, err := in.planJob(userHome, collectorJob(executable, home, env.collectorEnvironment(next.Storage)))
	if err != nil {
		return setupjournal.Journal{}, err
	}
	jobChanges, err := artifactChanges(plan.Artifacts)
	if err != nil {
		return setupjournal.Journal{}, err
	}
	changes = append(changes, jobChanges...)
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return setupjournal.Journal{}, err
	}
	change, err := fileChange(filepath.Join(home, "config.json"), append(data, '\n'))
	if err != nil {
		return setupjournal.Journal{}, err
	}
	changes = append(changes, change)
	job := env.jobStatus(userHome, plan.Ref)
	// Unknown refuses even a first setup: loading over a job launchd may
	// already run under this label is the one thing setup must not do.
	if job.State == scheduler.Unknown {
		return setupjournal.Journal{}, errors.New(unknownJobMessage(in.sched().Words(), problemOf(job)))
	}
	if job.State == scheduler.AnotherInstallation {
		problem, words := problemOf(job), in.sched().Words()
		return setupjournal.Journal{}, fmt.Errorf("%s's %s job was loaded from a %s other than %s, so it belongs to another installation; setup leaves it running and installs nothing over it. %s", words.Manager, plan.Ref, words.Definition, problem.Expected, problem.Fix)
	}
	// The jobs this installation's setup retires in favor of its own: the
	// prototype's upload job (the account's, retired only by its default
	// installation: a test installation must not change it) and the
	// collectors earlier releases installed under other labels.
	retirees, err := planRetirees(env, in, userHome)
	if err != nil {
		return setupjournal.Journal{}, err
	}
	legacy, relabeled, err := setupjournal.RetireeJobs(retirees)
	if err != nil {
		return setupjournal.Journal{}, err
	}
	var firstRelabeled *setupjournal.LegacyJob
	var moreRelabeled []*setupjournal.LegacyJob
	if len(relabeled) > 0 {
		firstRelabeled, moreRelabeled = relabeled[0], relabeled[1:]
	}
	journal := setupjournal.Journal{Legacy: legacy, Relabeled: firstRelabeled, MoreRelabeled: moreRelabeled, Changes: changes, Plist: jobChanges[0].Path, WasLoaded: jobActive(job.State), Backend: in.sched().Name(), JobRef: string(plan.Ref)}
	return journal, nil
}

// planRetirees is the jobs setup retires in favor of this installation's own,
// as found: each alias the scheduler lists (see installation.installed) with
// its definition on disk and whether it is loaded. What blocks setup is an
// error: an alias definition the scheduler does not recognize, one whose
// state cannot be read (setup will not retire a job it cannot tell is loaded),
// and a prototype job the scheduler runs from another installation's
// definition. An earlier-label collector the scheduler runs from another
// installation's definition is that installation's, and is left alone.
func planRetirees(env Env, in installation, userHome string) ([]scheduler.Retiree, error) {
	jobs, err := in.installed(userHome)
	if err != nil {
		return nil, err
	}
	words := in.sched().Words()
	var retirees []scheduler.Retiree
	for _, job := range jobs {
		if job.Alias == "" {
			continue
		}
		path := definitionPath(env.jobDefinition(userHome, job.Ref))
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		status := env.jobStatus(userHome, job.Ref)
		switch {
		case status.State == scheduler.Unknown && job.Alias == scheduler.Prototype:
			// Only an existing, recognized prototype job needs the scheduler
			// to answer: retiring it without knowing whether it is loaded
			// could leave the prototype uploader running with its definition
			// gone. With none on disk (the common fresh install) it is not
			// listed, so an unknown state never blocks setup for it.
			return nil, fmt.Errorf("cannot determine legacy upload job state; restore %s access and retry", words.Tool)
		case status.State == scheduler.Unknown:
			return nil, fmt.Errorf("cannot determine the state of %s; restore access to %s and retry", path, words.Tool)
		case status.State == scheduler.AnotherInstallation && job.Alias == scheduler.Prototype:
			problem := problemOf(status)
			return nil, fmt.Errorf("%s's legacy upload job was loaded from a %s other than %s; %s", words.Manager, words.Definition, problem.Expected, problem.Fix)
		case status.State == scheduler.AnotherInstallation:
			continue
		}
		retirees = append(retirees, scheduler.Retiree{
			Backend:   in.sched().Name(),
			Ref:       job.Ref,
			Alias:     job.Alias,
			Artifacts: []scheduler.Artifact{scheduler.FileArtifact(path, data, info.Mode().Perm())},
			WasLoaded: jobActive(status.State),
		})
	}
	return retirees, nil
}

// otherInstallationError is a setup refused because another installation's
// hooks are in a hook file it would install into (see
// describeOtherInstallations). Rerunning setup stops there again until they
// are gone, which only the user can decide.
type otherInstallationError struct{ problems []string }

func (e *otherInstallationError) Error() string { return strings.Join(e.problems, "\n") }

func (e *otherInstallationError) guidance() string {
	return "Nothing was installed; your answers are saved. Once the other installation's hooks are gone (or this installation has its own HOME), run agent-archive setup to continue."
}

// recoveryPending is what a command other than setup says while an
// interrupted setup's record exists: where the record is, and both ways
// out.
func recoveryPending(home string) string {
	return fmt.Sprintf("setup was interrupted and needs recovery (recorded in %s). Run agent-archive setup to recover it; if setup reports a file changed outside setup, run agent-archive setup --abandon-recovery to keep your files as they are now", setupjournal.JournalPath(home))
}

// abandonRecovery discards an interrupted setup's record without touching
// any file it lists: hook files, the LaunchAgent, and settings all stay as
// they are now, which is the way out when recovery refuses to overwrite a
// file edited since (see setupjournal.Restore). Setup afterwards reviews and
// reinstalls from there.
func abandonRecovery(out io.Writer, env Env) error {
	home, err := env.home()
	if err != nil {
		return err
	}
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return err
	}
	defer release()
	unlock, err := lockCollector(home, "setup", env.now())
	if err != nil {
		return fmt.Errorf("%s holds the collector lock; retry when it finishes: %w", lockHolder(home), err)
	}
	defer unlock()
	releaseHooks, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		return err
	}
	defer releaseHooks()
	var journal setupjournal.Journal
	err = local.Read(setupjournal.JournalPath(home), &journal)
	if os.IsNotExist(err) {
		terminal.Println(out, "No interrupted setup to discard. Nothing was changed.")
		return nil
	}
	if state.IsUndecodable(err) {
		// Nothing in it can be trusted, so nothing in it is acted on; it is
		// kept for anyone who wants to see what setup was doing.
		aside, moveErr := moveAside(setupjournal.JournalPath(home))
		if moveErr != nil {
			return moveErr
		}
		terminal.Printf(out, "The interrupted setup's record %s could not be read (%v). It was moved to %s, and every file was kept as it is now.\n", setupjournal.JournalPath(home), err, aside)
		terminal.Println(out, "Next: run agent-archive setup to review your settings; it reinstalls the hooks and starts the background collector again.")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", setupjournal.JournalPath(home), err)
	}
	if err = os.Remove(setupjournal.JournalPath(home)); err != nil {
		return err
	}
	terminal.Printf(out, "Discarded the interrupted setup recorded in %s. These files were kept as they are now:\n", setupjournal.JournalPath(home))
	for _, c := range journal.Changes {
		terminal.Printf(out, "  %s\n", c.Path)
	}
	terminal.Println(out, "The background collector may be stopped: the interrupted setup can have stopped it before it was interrupted, and nothing restarts it now.")
	terminal.Println(out, "Next: run agent-archive setup to review your settings; it reinstalls the hooks and starts the background collector again. agent-archive status shows what is running.")
	return nil
}

func recoverSetup(home string, env Env) error {
	return setupjournal.Recover(home, env.backends(), func() (func(), error) { return lockCollector(home, "setup", env.now()) })
}

func withoutBucketPrivacy(cfg config.Config) config.Config {
	cfg.BucketPrivacy = nil
	return cfg
}

// freshestBucketPrivacy returns the newer of cfg's own report and candidate
// when candidate was checked for cfg's storage configuration, otherwise nil.
func freshestBucketPrivacy(cfg config.Config, candidate *storage.PrivacyReport) *storage.PrivacyReport {
	if candidate == nil || candidate.CheckedAt == nil || candidate.ConfigurationID != privacyConfigurationID(cfg) {
		return nil
	}
	if own := cfg.BucketPrivacy; own != nil && own.CheckedAt != nil && own.ConfigurationID == candidate.ConfigurationID && !own.CheckedAt.Before(*candidate.CheckedAt) {
		return nil
	}
	return candidate
}

func prepareMachineAssignment(old config.Config, next *config.Config) {
	if next.MachineAssignment != nil && (next.MachineAssignment.DestinationID != next.DestinationID() || (old.Storage.R2CredentialRef != next.Storage.R2CredentialRef && reflect.DeepEqual(next.MachineAssignment, old.MachineAssignment))) {
		next.MachineAssignment = nil
	}
	// Retire only committed provenance, inside the same configuration transaction.
	if a := old.MachineAssignment; a != nil && a.Kind == config.MachineAssignmentR2Own && !reflect.DeepEqual(a, next.MachineAssignment) {
		present := false
		for _, prior := range next.RetiredMachineAssignments {
			present = present || prior.DestinationID == a.DestinationID && prior.AccessKeyID == a.AccessKeyID
		}
		if !present {
			next.RetiredMachineAssignments = append(next.RetiredMachineAssignments, *a)
		}
	}
	if next.MachineName == "" {
		next.MachineName = "unnamed"
		if config.ValidMachineID(next.MachineID) {
			next.MachineName = "unnamed-" + next.MachineID[:4]
		}
	}
}

// protectSetupWriter fences protected rollback snapshots before journal planning.
// Callers hold hooks.lock after settling any previous setup transaction.
func protectSetupWriter(home string, current config.Config, proposed ...config.Config) error {
	if setupjournal.TransactionPending(home) {
		return errors.New("setup pending before writer protection")
	}
	if len(proposed) > 0 && ((proposed[0].CodexCapture != nil && current.CodexCapture == nil) || (proposed[0].CodexHistoryProtection && !current.CodexHistoryProtection) || (proposed[0].GenerationProtection && !current.GenerationProtection) || (proposed[0].DurableStorageProtection && !current.DurableStorageProtection) || (proposed[0].RetentionProtection && !current.RetentionProtection) || (proposed[0].ImportStagingProtection && !current.ImportStagingProtection)) {
		if _, found, err := config.Load(home); err != nil {
			return err
		} else if found {
			config.PreserveWriterFence(&current, proposed[0])
			if err := config.Save(home, current); err != nil {
				return err
			}
		}
	}
	if current.Discovery == nil && current.CodexCapture == nil {
		return nil
	}
	return config.ProtectIdentityWriter(home)
}
