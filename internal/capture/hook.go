// Package capture applies validated lifecycle facts under shared archive policy.
package capture

import (
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"golang.org/x/text/unicode/norm"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RecordFailure leaves a content-free diagnostic using already decoded project facts.
func RecordFailure(home, harness, root string) {
	defer func() { _ = recover() }()
	cfg, found, err := config.Load(home)
	if err != nil || !found || !cfg.Archive.Enabled {
		return
	}
	project, owned := ConfiguredProjectActivationFor(cfg, root)
	if !owned || !project.Included {
		return
	}
	_ = RecordDiagnostic(home, Diagnostic{Code: DiagnosticHookFailed, Harness: archive.CanonicalHarness(harness), ProjectRoot: project.Root, ObservedAt: time.Now()})
}

// Option adjusts narrow capture dependencies.
type Option func(*eventOptions)

type eventOptions struct {
	repoKey     RepoKeyFunc
	replay      *archive.Replay
	gitHead     GitHeadFunc
	stat        func(string) (os.FileInfo, error)
	afterEffect func(effectName) error
}

type effectName string

const (
	effectRegistrationCreate effectName = "registration-create"
	effectRegistrationUpdate effectName = "registration-update"
	effectEvidenceSave       effectName = "evidence-save"
	effectLocatorUpdate      effectName = "locator-update"
	effectRequestSave        effectName = "request-save"
	effectChildReservation   effectName = "child-reservation"
	effectChildLink          effectName = "child-link"
	effectChildCandidate     effectName = "child-candidate"
	effectIntentAck          effectName = "intent-ack"
)

// WithReplay records the hook's replay marker only when a new session is admitted.
// Continuations preserve their original marker.
func WithReplay(value string) Option {
	return func(o *eventOptions) { o.replay = archive.ParseReplay(value) }
}

// RepoKeyFunc is the command-owned, bounded repository lookup.
type RepoKeyFunc func(string) string

// WithRepoKey injects a repository lookup before hooks.lock.
func WithRepoKey(f RepoKeyFunc) Option { return func(o *eventOptions) { o.repoKey = f } }

// GitHeadFunc returns HEAD and optional dirtiness from the reported working directory.
// The command owns this lookup; capture never runs git itself.
type GitHeadFunc func(dir string, withDirty bool) (string, *bool)

// WithGitHead injects a bounded working-directory commit lookup before hooks.lock.
func WithGitHead(f GitHeadFunc) Option { return func(o *eventOptions) { o.gitHead = f } }

// gitLookups contains only observations made by the current hook, never replay.
type gitLookups struct {
	repoKey   string
	startHead *archive.GitHead
	lastHead  *archive.GitHead
}

// HandleBatch is the common typed capture entry used by injected integrations.
func HandleBatch(home, harness string, batch []agentapi.LifecycleEvent, now time.Time, options ...Option) error {
	var o eventOptions
	for _, option := range options {
		option(&o)
	}
	return handleBatch(home, harness, batch, now, nil, nil, o)
}

// lockHooks substitutes a bounded lock operation for contention tests.
type lockHooks func(string, time.Duration) (func(), error)

// repoKeyBudget is the longest the hook waits for a repository key. The
// lookup bounds itself (internal/gitremote) but a hung mount can stall a
// process before its timeout starts, so the wait is bounded here too.
const repoKeyBudget = 600 * time.Millisecond

// hooksLockWait is how long a hook waits for hooks.lock when it did not spend
// time on a repository key, and minHooksLockWait the least it waits when it
// did.
const (
	hooksLockWait    = 750 * time.Millisecond
	minHooksLockWait = 150 * time.Millisecond
)

// lockWaitAfter is how long to wait for hooks.lock once spent has gone on the
// repository key: the usual wait less spent, and never less than the floor.
func lockWaitAfter(spent time.Duration) time.Duration {
	return min(hooksLockWait, max(hooksLockWait-spent, minHooksLockWait))
}

// boundedRepoKey is repoKey(root), or "" when it is nil, panics, does not
// return within repoKeyBudget, or does not return a RepoKey: a repository key
// never fails or delays a registration.
func boundedRepoKey(repoKey RepoKeyFunc, root string) string {
	if repoKey == nil {
		return ""
	}
	answer := make(chan string, 1)
	go func() {
		defer func() {
			if recover() != nil {
				answer <- ""
			}
		}()
		answer <- repoKey(root)
	}()
	timer := time.NewTimer(repoKeyBudget)
	defer timer.Stop()
	select {
	case key := <-answer:
		if archive.IsRepoKey(key) {
			return key
		}
	case <-timer.C:
	}
	return ""
}

func handleBatch(home, harness string, batch []agentapi.LifecycleEvent, now time.Time, lock lockHooks, afterLock func(), o eventOptions) error {
	if len(batch) == 0 {
		return nil
	}
	if err := validateBatchStructure(harness, batch, now); err != nil {
		return err
	}
	if setupjournal.TransactionPending(home) {
		filtered, err := filterBatchEvidence(batch)
		if err != nil {
			return err
		}
		return recordSetupBatch(home, filtered, now)
	}
	observedConfig, active, err := loadHookCaptureWindow(home, nil)
	if err != nil || !active {
		return err
	}
	batch, err = filterBatchEvidence(batch)
	if err != nil {
		return err
	}
	batch = resolveFreshness(batch, o.stat)
	prepareCodexFacts(harness, batch, observedConfig, now)
	lookupStarted := time.Now()
	lookups := batchGitLookups(home, batch, now, o)
	wait := lockWaitAfter(time.Since(lookupStarted))
	if lock == nil {
		lock = func(home string, wait time.Duration) (func(), error) {
			return local.NamedLockWait(home, "hooks.lock", wait)
		}
	}
	unlock, err := lock(home, wait)
	if err != nil {
		if errors.Is(err, local.ErrBusy) {
			queued, queueErr := queueEventBatchInGeneration(home, batch, now, observedConfig.PauseGeneration, lookups.lastHead, nil, o.replay)
			diagnosticErr := recordHookBusyEvent(home, batch[0], now)
			if queueErr != nil || diagnosticErr != nil {
				return fmt.Errorf("capture registration busy (admission queued: %t): %w; %w", queued, err, errors.Join(queueErr, diagnosticErr))
			}
			if queued {
				return nil
			}
		}
		return fmt.Errorf("capture registration busy; this hook was not recorded: %w", err)
	}
	defer unlock()
	if afterLock != nil {
		afterLock()
	}
	if setupjournal.TransactionPending(home) {
		return recordSetupBatch(home, batch, now)
	}
	cfg, active, err := loadHookCaptureWindow(home, &observedConfig)
	if err != nil || !active {
		return err
	}
	store, err := state.Open(home)
	if err != nil {
		return fmt.Errorf("open local store: %w", err)
	}
	store = store.ForHook()
	// An absent-parent followup must wait for this batch's later start. Keep
	// the whole content-free retry record until the native ordered effects
	// finish, just as lock-contention replay does.
	ordered, orderErr := needsOrderedAdmission(store, cfg, batch, now)
	// Let the existing loop request qualified-index recovery on lookup failure.
	if orderErr == nil && ordered {
		err = applyOrderedAdmission(home, store, cfg, batch, now, lookups, o.afterEffect, o.replay)
		if errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
			return requestBatchIndexRecovery(home, store, observedConfig, batch, batch[0], now, lookups.lastHead, err, o.replay)
		}
		return err
	}
	for _, event := range batch {
		err = applyEvent(home, store, cfg, event, now, lookups, o.afterEffect, o.replay)
		if errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
			return requestBatchIndexRecovery(home, store, observedConfig, batch, event, now, lookups.lastHead, err, o.replay)
		}
		if errors.Is(err, state.ErrSessionNotRegistered) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func requestBatchIndexRecovery(home string, store *state.Store, cfg config.Config, batch []agentapi.LifecycleEvent, event agentapi.LifecycleEvent, now time.Time, lastHead *archive.GitHead, cause error, replay *archive.Replay) error {
	key, err := eventKey(event)
	if err != nil {
		return err
	}
	recoveryErr := store.RequestSessionIndexRecovery(key)
	_, queueErr := queueEventBatchInGeneration(home, batch, now, cfg.PauseGeneration, lastHead, nil, replay)
	project, owned := hookProjectActivation(cfg, event, now)
	var diagnosticErr error
	if owned && project.Included {
		diagnosticErr = RecordDiagnostic(home, Diagnostic{Code: DiagnosticSessionIndexRecovery, Harness: string(event.Session.Agent), ProjectRoot: project.Root, ObservedAt: now})
	}
	return errors.Join(cause, recoveryErr, queueErr, diagnosticErr)
}

// Ordinary native batches have no followup before a proven deferred start;
// their existing loop needs no additional index lookup or allocation.
func needsOrderedAdmission(store *state.Store, cfg config.Config, batch []agentapi.LifecycleEvent, now time.Time) (bool, error) {
	followup := false
	for _, event := range batch {
		if event.Deferred == agentapi.DeferredFollowup {
			followup = true
		}
		if followup && event.Deferred == agentapi.DeferredStart && event.Start.Kind == agentapi.FreshExplicit {
			owner, owned := hookProjectActivation(cfg, event, now)
			if !owned || !owner.Included || (cfg.EffectiveCodexCaptureScope() != config.CodexAllProjects && !cfg.Archive.Eligible(owner.Root, now)) {
				return false, nil
			}
			key, err := eventKey(event)
			if err != nil {
				return false, err
			}
			found, err := HasRegistration(store, key)
			return !found, err
		}
	}
	return false, nil
}

func applyOrderedAdmission(home string, store *state.Store, cfg config.Config, batch []agentapi.LifecycleEvent, now time.Time, lookups gitLookups, after func(effectName) error, replay *archive.Replay) error {
	path, err := persistEventBatchInGeneration(home, batch, now, cfg.PauseGeneration, lookups.lastHead, nil, replay)
	if err != nil || path == "" {
		return err
	}
	var waiting []agentapi.LifecycleEvent
	for _, event := range batch {
		if event.Deferred == agentapi.DeferredFollowup {
			key, err := eventKey(event)
			if err != nil {
				return err
			}
			found, err := HasRegistration(store, key)
			if err != nil {
				return err
			}
			if !found {
				waiting = append(waiting, event)
				continue
			}
		}
		if err := applyEvent(home, store, cfg, event, now, lookups, after, replay); err != nil && !errors.Is(err, state.ErrSessionNotRegistered) {
			return err
		}
		if event.Kind == agentapi.EventStart && len(waiting) > 0 {
			key, err := eventKey(event)
			if err != nil {
				return err
			}
			found, err := HasRegistration(store, key)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			for _, pending := range waiting {
				if err := applyEvent(home, store, cfg, pending, now, lookups, after, replay); err != nil {
					return err
				}
			}
			waiting = nil
		}
	}
	if len(waiting) > 0 {
		return state.ErrSessionNotRegistered
	}
	return acknowledgeIntent(path, after)
}

func eventKey(event agentapi.LifecycleEvent) (agentmeta.SessionKey, error) {
	return agentmeta.NewSessionKey(string(event.Session.Agent), event.Session.NativeID)
}

// HasRegistration performs a bounded lookup for an exact qualified identity.
func HasRegistration(store *state.Store, key agentmeta.SessionKey) (bool, error) {
	_, found, err := store.ArchiveSessionID(key)
	return found, err
}

func resolveFreshness(batch []agentapi.LifecycleEvent, stat func(string) (os.FileInfo, error)) []agentapi.LifecycleEvent {
	if stat == nil {
		stat = os.Stat
	}
	for i := range batch {
		event := &batch[i]
		if event.Start.Kind != agentapi.FreshStat {
			continue
		}
		path := event.Start.Path
		event.Start.Kind = agentapi.FreshUnknown
		if path == "" || !filepath.IsAbs(path) {
			continue
		}
		info, err := stat(path)
		if errors.Is(err, os.ErrNotExist) || err == nil && info != nil && info.Mode().IsRegular() && info.Size() == 0 {
			event.Start.Kind = agentapi.FreshExplicit
		}
	}
	return batch
}

func recordHookBusyEvent(home string, event agentapi.LifecycleEvent, now time.Time) error {
	cfg, found, err := config.Load(home)
	if err != nil || !found || !cfg.Archive.Enabled || cfg.Paused {
		return err
	}
	project, owned := hookProjectActivation(cfg, event, now)
	if !owned || !project.Included {
		return nil
	}
	return recordDiagnostic(home, Diagnostic{Code: DiagnosticHookBusy, Harness: string(event.Session.Agent), ProjectRoot: project.Root, ObservedAt: now}, true)
}

func recordSetupBatch(home string, batch []agentapi.LifecycleEvent, now time.Time) error {
	for _, event := range batch {
		if event.Kind != agentapi.EventStart {
			continue
		}
		if event.NewOnly {
			key, err := eventKey(event)
			if err != nil {
				return err
			}
			registered, err := HasRegistration(state.OpenReadOnly(home), key)
			if err != nil || registered {
				return err
			}
		}
		cfg, found, err := config.Load(home)
		if err != nil {
			return err
		}
		if !found || !cfg.Archive.Enabled || cfg.Paused {
			return nil
		}
		project, owned := hookProjectActivation(cfg, event, now)
		if !owned || !project.Included {
			return nil
		}
		return RecordDiagnostic(home, Diagnostic{Code: DiagnosticSetupInProgress, Harness: string(event.Session.Agent), ProjectRoot: project.Root, ObservedAt: now})
	}
	return nil
}

func applyEvent(home string, store *state.Store, cfg config.Config, event agentapi.LifecycleEvent, now time.Time, lookups gitLookups, after func(effectName) error, replay *archive.Replay) error {
	if string(event.Session.Agent) == "codex" && cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects && event.CodexProjectRoot == "" {
		return RecordDiagnostic(home, Diagnostic{Code: DiagnosticProjectUnavailable, Harness: "codex", ObservedAt: now})
	}
	if string(event.Session.Agent) == "codex" && cfg.CodexCapture != nil && event.Kind != agentapi.EventStart {
		key, err := eventKey(event)
		if err != nil {
			return err
		}
		id, found, err := store.ArchiveSessionID(key)
		if err != nil {
			return err
		}
		if found {
			r, exists, err := store.LoadRegistration(id)
			if err != nil {
				return err
			}
			if exists && (r.CodexAdmission != nil || cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects) && !codexContinuationAccepted(cfg, event, r) {
				return nil
			}
		}
	}
	switch event.Kind {
	case agentapi.EventStart:
		return handleSessionStart(home, store, cfg, event, now, lookups, after, replay)
	case agentapi.EventTurnStart:
		return handleSessionActivity(store, event, now, after)
	case agentapi.EventStop, agentapi.EventResponse:
		key, err := eventKey(event)
		if err != nil {
			return err
		}
		found, err := HasRegistration(store, key)
		if err != nil {
			return err
		}
		if !found && event.Deferred == agentapi.DeferredFollowup {
			_, err = queueEventBatchInGeneration(home, []agentapi.LifecycleEvent{event}, now, cfg.PauseGeneration, nil, nil, replay)
			return err
		}
		return handleSessionStop(store, event, now, lookups.lastHead, after)
	case agentapi.EventSubagent:
		return handleSubagentStop(store, cfg, event, now, after)
	}
	return errors.New("invalid lifecycle effect")
}

func effectBoundary(after func(effectName) error, name effectName) error {
	if after != nil {
		return after(name)
	}
	return nil
}

func handleSessionStart(home string, store *state.Store, cfg config.Config, event agentapi.LifecycleEvent, now time.Time, lookups gitLookups, after func(effectName) error, replay *archive.Replay) error {
	key, err := eventKey(event)
	if err != nil {
		return err
	}
	blanket := string(key.Agent) == "codex" && cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects
	var owner archive.ProjectActivation
	var owned bool
	if !blanket {
		owner, owned = hookProjectActivation(cfg, event, now)
	}
	root := event.ProjectRoot
	if blanket {
		root = event.CodexProjectRoot
	}
	if owned && !blanket {
		root = owner.Root
	}
	existingID, found, err := store.ArchiveSessionID(key)
	if err != nil {
		return fmt.Errorf("look up archive session ID: %w", err)
	}
	if found {
		handled, err := continueHookSession(store, cfg, event, now, root, blanket, existingID, after)
		if handled || err != nil {
			return err
		}
	}
	var proof *archive.CodexAdmissionProof
	if blanket {
		token, allowed := cfg.CodexGeneration(root, event.CodexCwd, now, now)
		if root == "" || !allowed || token != event.CodexPolicyToken || event.Start.Kind != agentapi.FreshExplicit {
			return nil
		}
		proof = &archive.CodexAdmissionProof{Generation: token, Revision: cfg.CodexCapture.Revision, Cwd: event.CodexCwd}
	} else if !owned || !owner.Included {
		return nil
	}
	if code := declinedStart(cfg, root, now, event.Start); !blanket && code != "" {
		return RecordDiagnostic(home, Diagnostic{Code: code, Harness: string(key.Agent), ProjectRoot: root, ObservedAt: now})
	}
	harness := archive.Harness{Name: string(key.Agent)}
	applyObservation(&harness, event.Session)
	reg, err := store.RegisterOrMerge(key, func(id string) archive.SessionRegistration {
		return archive.SessionRegistration{CodexAdmission: proof, DiscoveryCwd: event.CodexCwd, ArchiveSessionID: id, NativeSessionID: key.NativeID, ProjectID: archive.ProjectID(root), ProjectRoot: root, RepoKey: lookups.repoKey, StartHead: lookups.startHead, Replay: replay, Harness: harness, TranscriptPath: event.Source.Path, SessionStartedAt: now, RegisteredAt: now, AdmittedAt: now, Origin: archive.SessionOriginHook, HookObservedAt: now, StartedAtSource: archive.StartedAtSourceHook, DestinationID: cfg.DestinationID()}
	})
	if err != nil {
		return fmt.Errorf("register session: %w", err)
	}
	if err := effectBoundary(after, effectRegistrationCreate); err != nil {
		return err
	}
	return saveLifecycleEvidence(store, reg.ArchiveSessionID, event, now, after)
}

func declinedStart(cfg config.Config, root string, now time.Time, start agentapi.StartEvidence) DiagnosticCode {
	if !cfg.Archive.Eligible(root, now) {
		return DiagnosticPreActivationStart
	}
	if start.Kind != agentapi.FreshExplicit {
		return DiagnosticUnknownSessionStart
	}
	return ""
}

var errContinuationDeclined = errors.New("continuation not accepted by the current configuration")

var errSessionIdentityConflict = errors.New("session identity conflicts with the accepted registration")

func applyObservation(target *archive.Harness, session agentapi.NativeSession) {
	if session.Version != "" {
		target.Version = session.Version
	}
	if session.Mode != "" {
		target.Mode = string(session.Mode)
	}
}

func applyLocator(reg *archive.SessionRegistration, event agentapi.LifecycleEvent) {
	if reg.CodexBinding != nil {
		if filepath.IsAbs(event.Source.Path) && len(event.Source.Path) <= 4096 {
			reg.CodexCandidatePath = event.Source.Path
		}
		return
	}
	if event.Source.Path == "" || !reg.ReadsTranscriptFile() || reg.Origin == archive.SessionOriginDiscovery {
		return
	}
	if event.Locator == agentapi.LocatorReplaceFile || event.Locator == agentapi.LocatorFillFile && reg.TranscriptPath == "" {
		reg.TranscriptPath = event.Source.Path
	}
}

func adoptLocator(store *state.Store, reg *archive.SessionRegistration, event agentapi.LifecycleEvent, after func(effectName) error) error {
	if event.Source.Path == "" || event.Locator != agentapi.LocatorFillFile || reg.TranscriptPath != "" || !reg.ReadsTranscriptFile() {
		return nil
	}
	key, err := eventKey(event)
	if err != nil {
		return err
	}
	found, err := store.UpdateRegistration(reg.ArchiveSessionID, func(current *archive.SessionRegistration) error {
		if archive.CanonicalHarness(current.Harness.Name) != string(key.Agent) || current.NativeSessionID != key.NativeID {
			return errSessionIdentityConflict
		}
		applyLocator(current, event)
		*reg = *current
		return nil
	})
	if err != nil {
		return fmt.Errorf("record transcript path: %w", err)
	}
	if !found {
		return state.ErrSessionNotRegistered
	}
	return effectBoundary(after, effectLocatorUpdate)
}

func handleSessionActivity(store *state.Store, event agentapi.LifecycleEvent, now time.Time, after func(effectName) error) error {
	key, err := eventKey(event)
	if err != nil {
		return err
	}
	id, found, err := store.ArchiveSessionID(key)
	if err != nil || !found {
		return err
	}
	reg, found, err := store.LoadRegistration(id)
	if err != nil || !found {
		return err
	}
	if reg.HistoricalProject != nil {
		return nil
	}
	if archive.CanonicalHarness(reg.Harness.Name) != string(key.Agent) || reg.NativeSessionID != key.NativeID {
		return nil
	}
	if err := adoptLocator(store, &reg, event, after); err != nil {
		return err
	}
	return saveLifecycleEvidence(store, id, event, now, after)
}

func saveLifecycleEvidence(store *state.Store, id string, event agentapi.LifecycleEvent, now time.Time, after func(effectName) error) error {
	if err := store.RecordHookObservation(id, now); err != nil {
		return err
	}
	for _, evidence := range event.Evidence {
		if err := store.SaveEvidence(id, event.Reason, now, evidence); err != nil {
			return err
		}
		if err := effectBoundary(after, effectEvidenceSave); err != nil {
			return err
		}
	}
	return nil
}

func handleSessionStop(store *state.Store, event agentapi.LifecycleEvent, now time.Time, lastHead *archive.GitHead, after func(effectName) error) error {
	key, err := eventKey(event)
	if err != nil {
		return err
	}
	id, found, err := store.ArchiveSessionID(key)
	if err != nil || !found {
		return err
	}
	reg, found, err := store.LoadRegistration(id)
	if err != nil || !found {
		return err
	}
	if reg.HistoricalProject != nil {
		return nil
	}
	if archive.CanonicalHarness(reg.Harness.Name) != string(key.Agent) || reg.NativeSessionID != key.NativeID {
		return errSessionIdentityConflict
	}
	if err := adoptLocator(store, &reg, event, after); err != nil {
		return err
	}
	if err := recordLastHead(store, reg, lastHead); err != nil {
		return err
	}
	if err := store.RecordHookObservation(id, now); err != nil {
		return err
	}
	if err := store.SaveRequest(id, event.Reason, now, event.Evidence...); err != nil {
		return err
	}
	return effectBoundary(after, effectRequestSave)
}

// loadHookCaptureWindow reads the active capture configuration. When observed
// is supplied, a hook waiting for the lock must still belong to that same
// uninterrupted window, even if pause and resume both finished during its wait.
func loadHookCaptureWindow(home string, observed *config.Config) (config.Config, bool, error) {
	cfg, found, err := config.Load(home)
	if err != nil {
		return config.Config{}, false, fmt.Errorf("load config: %w", err)
	}
	active := found && cfg.Archive.Enabled && !cfg.Paused
	if observed != nil && cfg.PauseGeneration != observed.PauseGeneration {
		active = false
	}
	return cfg, active, nil
}

// ConfiguredProjectActivationFor returns the configured project that owns
// root: the project whose root is root itself or its nearest configured
// ancestor, comparing resolved paths so a symlinked checkout still maps to the
// project it was registered under. The nearest ancestor wins, so a project
// nested inside another keeps its own identity (and its own inclusion
// decision). Excluded projects take part in the match: an excluded project
// nested in an included one must stay excluded rather than falling through to
// its parent.
func ConfiguredProjectActivationFor(cfg config.Config, root string) (archive.ProjectActivation, bool) {
	project, found, _ := configuredProjectActivationIdentity(cfg, root)
	return project, found
}

// configuredProjectActivationIdentity distinguishes unknown filesystem identity
// from a resolved location with no configured owner. Existing registrations
// retain their admission when the incoming location has no matching rule.
func configuredProjectActivationIdentity(cfg config.Config, root string) (archive.ProjectActivation, bool, bool) {
	if root == "" {
		return archive.ProjectActivation{}, false, false
	}
	candidate := resolvedPath(root)
	if candidate == "" {
		return archive.ProjectActivation{}, false, false
	}
	var best archive.ProjectActivation
	var locations resolvedLocationMatcher
	bestDepth, found, conflicting := -1, false, false
	for _, project := range cfg.Archive.Projects {
		configured := resolvedPath(project.Root)
		// An unresolved rule may be an exclusion; never fall through to an
		// included ancestor when its filesystem identity is unknown.
		if configured == "" {
			return archive.ProjectActivation{}, false, false
		}
		within, certain := locations.within(candidate, configured)
		if !certain {
			return archive.ProjectActivation{}, false, false
		}
		if !within {
			continue
		}
		// Equivalent Unicode spellings can differ in byte length. Resolved
		// directory depth, rather than spelling length, determines ownership.
		depth := strings.Count(strings.TrimRight(configured, string(filepath.Separator)), string(filepath.Separator))
		if depth < bestDepth {
			continue
		}
		if depth == bestDepth {
			conflicting = conflicting || project.Included != best.Included
			continue
		}
		best, bestDepth, found, conflicting = project, depth, true, false
	}
	if conflicting {
		return archive.ProjectActivation{}, false, false
	}
	return best, found, true
}

// resolvedLocationMatcher keeps prefix observations within one scope lookup.
// Shared ancestors need one stat each even when thousands of rules alias them.
// No observation survives this activation or intent ownership check.
type resolvedLocationMatcher struct {
	prefixes map[string]resolvedLocationPrefix
}

type resolvedLocationPrefix struct {
	info os.FileInfo
	err  error
}

func (m *resolvedLocationMatcher) statPrefix(path string) (os.FileInfo, error) {
	if prefix, exists := m.prefixes[path]; exists {
		return prefix.info, prefix.err
	}
	info, err := os.Stat(path)
	if m.prefixes == nil {
		m.prefixes = make(map[string]resolvedLocationPrefix)
	}
	m.prefixes[path] = resolvedLocationPrefix{info: info, err: err}
	return info, err
}

// within compares paths after strict symlink resolution.
// Differently spelled existing components may be the same directory on a
// case-insensitive volume. Stat only those differing prefixes; enumerating each
// ancestor directory to canonicalize every hook would make large homes costly.
// Case or Unicode-normalization variants of absent components have uncertain
// identity: callers must decline capture rather than bypass a possibly equivalent
// exclusion.
func (m *resolvedLocationMatcher) within(path, root string) (within, certain bool) {
	if local.PathWithin(path, root) {
		return true, true
	}
	if !filepath.IsAbs(path) || !filepath.IsAbs(root) || filepath.VolumeName(path) != filepath.VolumeName(root) {
		return false, true
	}
	volume := filepath.VolumeName(root)
	pathParts := strings.Split(strings.TrimPrefix(path[len(volume):], string(filepath.Separator)), string(filepath.Separator))
	rootParts := strings.Split(strings.TrimPrefix(root[len(volume):], string(filepath.Separator)), string(filepath.Separator))
	if len(pathParts) < len(rootParts) {
		return false, true
	}
	pathPrefix, rootPrefix := volume+string(filepath.Separator), volume+string(filepath.Separator)
	for i, part := range rootParts {
		pathPrefix = filepath.Join(pathPrefix, pathParts[i])
		rootPrefix = filepath.Join(rootPrefix, part)
		if pathParts[i] == part {
			continue
		}
		pathInfo, pathErr := m.statPrefix(pathPrefix)
		rootInfo, rootErr := m.statPrefix(rootPrefix)
		if pathErr != nil || rootErr != nil {
			if errors.Is(pathErr, os.ErrNotExist) && errors.Is(rootErr, os.ErrNotExist) && strings.EqualFold(norm.NFD.String(pathParts[i]), norm.NFD.String(part)) {
				return false, false
			}
			if pathErr != nil && !errors.Is(pathErr, os.ErrNotExist) || rootErr != nil && !errors.Is(rootErr, os.ErrNotExist) {
				return false, false
			}
			return false, true
		}
		if !pathInfo.IsDir() || !rootInfo.IsDir() || !os.SameFile(pathInfo, rootInfo) {
			return false, true
		}
	}
	return true, true
}

// resolvedPath resolves existing ancestors even when a descendant is absent.
// Lstat stops at a dangling symlink so it cannot become a lexical inclusion.
// An empty result means the filesystem identity could not be resolved safely.
func resolvedPath(path string) string {
	if path == "" {
		return ""
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	ancestor := path
	for {
		if _, err = os.Lstat(ancestor); err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return ""
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return ""
		}
		ancestor = next
	}
	resolved, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(ancestor, path)
	if err != nil {
		return ""
	}
	return filepath.Join(resolved, rel)
}

// hookProjectActivation consumes facts resolved before hooks.lock for all mode.
func hookProjectActivation(cfg config.Config, event agentapi.LifecycleEvent, now time.Time) (archive.ProjectActivation, bool) {
	if string(event.Session.Agent) == "codex" && cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
		token, ok := cfg.CodexGeneration(event.CodexProjectRoot, event.CodexCwd, now, now)
		return archive.ProjectActivation{Root: event.CodexProjectRoot, ProjectID: archive.ProjectID(event.CodexProjectRoot), Included: ok && token == event.CodexPolicyToken}, ok
	}
	return ConfiguredProjectActivationFor(cfg, event.ProjectRoot)
}

func codexContinuationAccepted(cfg config.Config, event agentapi.LifecycleEvent, r archive.SessionRegistration) bool {
	if event.CodexProjectRoot == "" || event.CodexCwd == "" || !cfg.AcceptSession(r) {
		return false
	}
	if r.CodexAdmission != nil {
		return event.CodexProjectRoot == r.ProjectRoot && cfg.CodexContinuationAllowed(event.CodexProjectRoot, event.CodexCwd)
	}
	return (event.CodexProjectRoot == r.ProjectRoot || local.PathWithin(event.CodexCwd, r.ProjectRoot)) && cfg.CodexContinuationAllowed(event.CodexProjectRoot, event.CodexCwd)
}

func prepareCodexFacts(harness string, batch []agentapi.LifecycleEvent, cfg config.Config, now time.Time) {
	if harness != "codex" || cfg.CodexCapture == nil {
		return
	}
	resolver := sourcefacts.NewProjectResolver()
	for i := range batch {
		facts, ok := resolver.Resolve(batch[i].ProjectRoot)
		if ok {
			batch[i].CodexProjectRoot = facts.Root
			batch[i].CodexCwd = facts.Cwd
			batch[i].CodexPolicyToken, _ = cfg.CodexGeneration(facts.Root, facts.Cwd, now, now)
		}
	}
}

func continueHookSession(store *state.Store, cfg config.Config, event agentapi.LifecycleEvent, now time.Time, root string, blanket bool, existingID string, after func(effectName) error) (bool, error) {
	key, err := eventKey(event)
	if err != nil {
		return true, err
	}

	if event.NewOnly {
		return true, nil
	}
	updated, err := store.UpdateRegistration(existingID, func(existing *archive.SessionRegistration) error {
		if existing.HistoricalProject != nil || !cfg.AcceptSession(*existing) || ((blanket || existing.CodexAdmission != nil) && !codexContinuationAccepted(cfg, event, *existing)) {
			return errContinuationDeclined
		}
		if archive.CanonicalHarness(existing.Harness.Name) != string(key.Agent) || existing.NativeSessionID != key.NativeID {
			return errSessionIdentityConflict
		}
		if blanket && root != "" && root != existing.ProjectRoot && !local.PathWithin(event.CodexCwd, existing.ProjectRoot) {
			return errSessionIdentityConflict
		}
		if !blanket && existing.CodexAdmission == nil {
			configured, found, resolved := configuredProjectActivationIdentity(cfg, root)
			if !resolved {
				return errContinuationDeclined
			}
			if found && filepath.Clean(configured.Root) != filepath.Clean(existing.ProjectRoot) {
				return errSessionIdentityConflict
			}
		}
		applyLocator(existing, event)
		existing.RegisteredAt = now
		existing.HookObservedAt = now
		applyObservation(&existing.Harness, event.Session)
		return nil
	})
	if errors.Is(err, errContinuationDeclined) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	if updated {
		if err := effectBoundary(after, effectRegistrationUpdate); err != nil {
			return true, err
		}
		return true, saveLifecycleEvidence(store, existingID, event, now, after)
	}

	return false, nil
}

// boundedGitHead is gitHead's answer for dir as a GitHead observed at now,
// or nil when gitHead is nil, panics, does not return within repoKeyBudget,
// or does not return a full object name: the commit never fails or delays a
// registration. The waiting goroutine may outlive the call; its answer is
// then dropped.
func boundedGitHead(gitHead GitHeadFunc, dir string, withDirty bool, now time.Time) *archive.GitHead {
	if gitHead == nil || dir == "" || !filepath.IsAbs(dir) {
		return nil
	}
	type result struct {
		sha   string
		dirty *bool
	}
	answer := make(chan result, 1)
	go func() {
		defer func() {
			if recover() != nil {
				answer <- result{}
			}
		}()
		sha, dirty := gitHead(dir, withDirty)
		answer <- result{sha, dirty}
	}()
	timer := time.NewTimer(repoKeyBudget)
	defer timer.Stop()
	select {
	case got := <-answer:
		var dirty *bool
		if withDirty && got.dirty != nil {
			dirty = new(*got.dirty)
		}
		if head := (&archive.GitHead{SHA: got.sha, Dirty: dirty, ObservedAt: now}); head.Valid() {
			return head
		}
	case <-timer.C:
	}
	return nil
}

// recordLastHead saves head as the registration's LastHead when it names a
// different commit from the one recorded, preserving that commit's first-seen
// time. Each successful observation advances a local watermark so an older
// delayed stop cannot undo a newer stop at the same commit. The write goes
// through UpdateRegistration, so a session retention forgot meanwhile is
// reported as state.ErrSessionNotRegistered.
func recordLastHead(store *state.Store, reg archive.SessionRegistration, head *archive.GitHead) error {
	if !head.Valid() {
		return nil
	}
	found, err := store.UpdateRegistration(reg.ArchiveSessionID, func(current *archive.SessionRegistration) error {
		latest := current.LastHeadSeenAt
		if latest == nil && current.LastHead != nil {
			latest = &current.LastHead.ObservedAt
		}
		if latest != nil && head.ObservedAt.Before(*latest) {
			return nil
		}
		if current.LastHead == nil || current.LastHead.SHA != head.SHA {
			current.LastHead = head
		}
		observed := head.ObservedAt
		current.LastHeadSeenAt = &observed
		return nil
	})
	if err != nil {
		return fmt.Errorf("record last HEAD: %w", err)
	}
	if !found {
		return state.ErrSessionNotRegistered
	}
	return nil
}

// batchGitLookups asks at most once for this validated batch's exact identity
// and working directory. All lookups share the hook's existing wait budget.
func batchGitLookups(home string, batch []agentapi.LifecycleEvent, now time.Time, o eventOptions) gitLookups {
	if o.repoKey == nil && o.gitHead == nil {
		return gitLookups{}
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found || !cfg.Archive.Enabled || cfg.Paused {
		return gitLookups{}
	}
	event := batch[0]
	owner, owned := hookProjectActivation(cfg, event, now)
	key, err := eventKey(event)
	if err != nil {
		return gitLookups{}
	}
	store := state.OpenReadOnly(home)
	id, registered, err := store.ArchiveSessionID(key)
	if err != nil {
		return gitLookups{}
	}
	hasStart, hasStop := false, false
	for _, effect := range batch {
		hasStart = hasStart || effect.Kind == agentapi.EventStart && gitStartAllowed(cfg, effect, owner, owned, now)
		hasStop = hasStop || effect.Kind == agentapi.EventStop
	}
	if !registered && hasStart {
		var startHead *archive.GitHead
		done := make(chan struct{})
		go func() { defer close(done); startHead = boundedGitHead(o.gitHead, event.ProjectRoot, true, now) }()
		repoKey := boundedRepoKey(o.repoKey, owner.Root)
		<-done
		// A stop in the same observation batch sees the same HEAD; do not run git twice.
		var lastHead *archive.GitHead
		if hasStop && startHead.Valid() {
			lastHead = &archive.GitHead{SHA: startHead.SHA, ObservedAt: now}
		}
		return gitLookups{repoKey: repoKey, startHead: startHead, lastHead: lastHead}
	}
	if registered && hasStop {
		reg, found, err := store.LoadRegistration(id)
		if err == nil && found && gitStopAllowed(cfg, event, reg, owner, owned) {
			return gitLookups{lastHead: boundedGitHead(o.gitHead, event.ProjectRoot, false, now)}
		}
	}
	return gitLookups{}
}

// gitStartAllowed shares the hook admission boundary without granting new scope.
func gitStartAllowed(cfg config.Config, event agentapi.LifecycleEvent, owner archive.ProjectActivation, owned bool, now time.Time) bool {
	if !owned || !owner.Included {
		return false
	}
	if string(event.Session.Agent) == "codex" && cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects {
		return event.Start.Kind == agentapi.FreshExplicit
	}
	return declinedStart(cfg, owner.Root, now, event.Start) == ""
}

// gitStopAllowed preserves admitted physical ownership across permission edits.
func gitStopAllowed(cfg config.Config, event agentapi.LifecycleEvent, reg archive.SessionRegistration, owner archive.ProjectActivation, owned bool) bool {
	if reg.HistoricalProject != nil || !cfg.AcceptSession(reg) || reg.ParentSessionID != "" {
		return false
	}
	if string(event.Session.Agent) == "codex" && (reg.CodexAdmission != nil || cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects) {
		return codexContinuationAccepted(cfg, event, reg)
	}
	return owned && owner.Included && filepath.Clean(reg.ProjectRoot) == filepath.Clean(owner.Root)
}
