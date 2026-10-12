package capture

import (
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
)

// An admission intent contains the minimum identity needed to retry a proven
// start or a validated locator follow-up. It deliberately has no raw hook
// payload or conversation text. The original start proof is recorded at hook
// time because a transcript may no longer be empty when the collector retries.
type admissionIntent = agentapi.AdmissionIntent

const (
	maxAdmissionIntents   = 128
	maxAdmissionIntentAge = 24 * time.Hour
	admissionQueueWait    = 200 * time.Millisecond
	// pruneAdmissionIntentsWait is setup's wait for the queue lock. Hooks sync
	// outside it, but a rename under it can still stall for over a second on
	// a busy machine. Setup runs on no hook's budget; this bound only keeps a
	// wedged holder from hanging it.
	pruneAdmissionIntentsWait = 10 * time.Second
	// clearAdmissionIntentsWait stays short: pause and resume clear holding
	// hooks.lock, which hooks wait on inside their own budget.
	clearAdmissionIntentsWait = 2 * time.Second
)

func admissionIntentDir(home string) string { return filepath.Join(home, "admission-intents") }

// ClearAdmissionIntents discards starts observed before a pause. Callers hold
// hooks.lock while changing the pause flag. Its generation also invalidates
// staged files and hooks waiting across a complete pause/resume interval.
func ClearAdmissionIntents(home string) error {
	unlock, err := local.NamedLockWait(home, "admission-intents.lock", clearAdmissionIntentsWait)
	if err != nil {
		return fmt.Errorf("lock admission intent queue: %w", err)
	}
	defer unlock()
	entries, err := os.ReadDir(admissionIntentDir(home))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := os.Remove(filepath.Join(admissionIntentDir(home), entry.Name())); err != nil && !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// PruneAdmissionIntents removes retry records whose project or destination
// setup changed. The queue lock also serializes this with hook-side writes.
func PruneAdmissionIntents(home string, cfg config.Config) error {
	unlock, err := local.NamedLockWait(home, "admission-intents.lock", pruneAdmissionIntentsWait)
	if err != nil {
		return fmt.Errorf("lock admission intent queue: %w", err)
	}
	defer unlock()
	entries, err := os.ReadDir(admissionIntentDir(home))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(admissionIntentDir(home), entry.Name())
		var intent admissionIntent
		readErr := local.Read(path, &intent)
		if readErr == nil && intent.DestinationID == cfg.DestinationID() && ((intent.CodexPolicyToken != "" && replayIntentEligible(cfg, intent, intent.ObservedAt)) || (intent.CodexPolicyToken == "" && intentProjectStillOwned(intent.ProjectRoot, cfg.Archive.Projects))) {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// Without retaining the hook's original cwd, an intent for a parent project
// cannot be distinguished from one that started in a newly configured nested
// project. Drop those ambiguous intents rather than admitting them under the
// parent after setup changes ownership.
func intentProjectStillOwned(root string, projects []archive.ProjectActivation) bool {
	resolvedRoot := resolvedPath(root)
	if resolvedRoot == "" {
		return false
	}
	included := false
	var locations resolvedLocationMatcher
	for _, project := range projects {
		resolvedProject := resolvedPath(project.Root)
		if resolvedProject == "" {
			return false
		}
		if project.Root == root {
			included = project.Included
			continue
		}
		within, certain := locations.within(resolvedProject, resolvedRoot)
		if !certain || within {
			return false
		}
	}
	return included
}

// queueEventBatchInGeneration binds a contended event to the capture
// window observed before its lock wait, rather than whichever window is active
// after that wait. A complete pause/resume cycle must not admit the old start.
func queueEventBatchInGeneration(home string, batch []agentapi.LifecycleEvent, now time.Time, generation string, lastHead *archive.GitHead, afterStage func(), replay ...*archive.Replay) (bool, error) {
	path, err := persistEventBatchInGeneration(home, batch, now, generation, lastHead, afterStage, replay...)
	return path != "", err
}

func persistEventBatchInGeneration(home string, batch []agentapi.LifecycleEvent, now time.Time, generation string, lastHead *archive.GitHead, afterStage func(), replay ...*archive.Replay) (string, error) {
	intent, queued, err := eventAdmissionIntent(home, batch, now)
	if err != nil || !queued {
		return "", err
	}
	if len(replay) > 0 {
		for _, effect := range intent.Effects {
			if effect.Event.Kind == agentapi.EventStart {
				intent.Replay = replay[0]
				break
			}
		}
	}
	// The hook asked git before it found hooks.lock busy; replay records that
	// observation instead of losing it.
	if lastHead.Valid() {
		intent.LastHead = lastHead
	}
	if intent.PauseGeneration != generation {
		return "", nil
	}
	id, err := local.ID()
	if err != nil {
		return "", err
	}
	path := filepath.Join(admissionIntentDir(home), fmt.Sprintf("%020d-%s.json", intent.ObservedAt.UnixNano(), id))
	staged, err := stageEventIntent(home, intent, path, local.StageInExistingDir)
	if err != nil || staged == nil {
		return "", err
	}
	defer staged.Discard()
	if afterStage != nil {
		afterStage()
	}
	committed, err := commitEventIntent(home, intent, staged)
	if err != nil || committed == nil {
		return "", err
	}
	if err := committed.SyncDir(); err != nil {
		return "", err
	}
	return path, nil
}

// Create the first queue directory under its lock, but sync every intent
// outside it. Staging never creates a directory, so a concurrent purge cannot
// be undone by a hook that was waiting for its disk write.
func stageEventIntent(home string, intent admissionIntent, path string, stage func(string, any) (*local.Staged, error)) (*local.Staged, error) {
	staged, err := stage(path, intent)
	if !os.IsNotExist(err) {
		return staged, err
	}
	unlock, err := local.NamedLockWait(home, "admission-intents.lock", admissionQueueWait)
	if err != nil {
		return nil, fmt.Errorf("lock admission intent queue: %w", err)
	}
	ok, err := eventIntentStillQueueable(home, intent)
	if err == nil && ok {
		err = os.MkdirAll(admissionIntentDir(home), 0700)
	}
	unlock()
	if err != nil || !ok {
		return nil, err
	}
	staged, err = stage(path, intent)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return staged, err
}

// commitAdmissionIntent puts an intent in the queue under the queue lock,
// unless the queue is full or the intent is no longer valid, and returns
// the committed file (nil when it queued nothing). staged is the intent
// already staged; the caller discards it if it is not committed.
func commitEventIntent(home string, intent admissionIntent, staged *local.Staged) (*local.Staged, error) {
	unlock, err := local.NamedLockWait(home, "admission-intents.lock", admissionQueueWait)
	if err != nil {
		return nil, fmt.Errorf("lock admission intent queue: %w", err)
	}
	defer unlock()
	ok, err := eventIntentStillQueueable(home, intent)
	if err != nil || !ok {
		return nil, err
	}
	if err := staged.Commit(); err != nil {
		return nil, err
	}
	return staged, nil
}

func eventAdmissionIntent(home string, batch []agentapi.LifecycleEvent, now time.Time) (admissionIntent, bool, error) {
	if setupjournal.TransactionPending(home) {
		return admissionIntent{}, false, nil
	}
	var effects []agentapi.ReplayEffect
	for _, event := range batch {
		start := event.Deferred == agentapi.DeferredStart && event.Start.Kind == agentapi.FreshExplicit
		followup := event.Deferred == agentapi.DeferredFollowup && event.Source.Path != ""
		if !start && !followup {
			continue
		}
		// Retain only the previous content-free retry observation, never text/model IDs.
		event.Evidence = nil
		event.Child = nil
		if start {
			event.Start = agentapi.StartEvidence{Kind: agentapi.FreshExplicit, Reason: agentapi.FreshnessRetainedProof}
		}
		effects = append(effects, agentapi.ReplayEffect{Event: event})
	}
	if len(effects) == 0 {
		return admissionIntent{}, false, nil
	}
	event := effects[0].Event
	cfg, found, err := config.Load(home)
	if err != nil || !found || !cfg.Archive.Enabled || cfg.Paused {
		return admissionIntent{}, false, err
	}
	project, owned := ConfiguredProjectActivationFor(cfg, event.ProjectRoot)
	blanket := string(event.Session.Agent) == "codex" && cfg.EffectiveCodexCaptureScope() == config.CodexAllProjects && event.CodexProjectRoot != ""
	if blanket {
		token, allowed := cfg.CodexGeneration(event.CodexProjectRoot, event.CodexCwd, now, now)
		if !allowed || token != event.CodexPolicyToken {
			return admissionIntent{}, false, nil
		}
		return admissionIntent{Version: 1, Harness: "codex", Event: event.NativeEvent, NativeSessionID: event.Session.NativeID, ProjectRoot: event.ProjectRoot, DestinationID: cfg.DestinationID(), ObservedAt: now.UTC(), PauseGeneration: cfg.PauseGeneration, TranscriptPath: event.Source.Path, Effects: effects, CodexPolicyToken: token}, true, nil
	}
	if !owned || !project.Included || !cfg.Archive.Eligible(project.Root, now) {
		return admissionIntent{}, false, nil
	}
	for i := range effects {
		effects[i].Event.ProjectRoot = project.Root
	}
	return admissionIntent{Version: 1, Harness: string(event.Session.Agent), Event: event.NativeEvent, NativeSessionID: event.Session.NativeID, ProjectRoot: project.Root, DestinationID: cfg.DestinationID(), ObservedAt: now.UTC(), PauseGeneration: cfg.PauseGeneration, TranscriptPath: event.Source.Path, Effects: effects}, true, nil
}

// admissionIntentStillQueueable is the check made under the queue lock
// before an intent goes in.
func eventIntentStillQueueable(home string, intent admissionIntent) (bool, error) {
	// A pause or setup may have committed while this hook waited for the
	// queue lock. Recheck after taking it so a stale config cannot write a
	// private retry record after the corresponding purge.
	cfg, found, err := config.Load(home)
	if err != nil || !found || !cfg.Archive.Enabled || cfg.Paused || setupjournal.TransactionPending(home) {
		return false, err
	}
	if intent.CodexPolicyToken != "" {
		if !replayIntentEligible(cfg, intent, intent.ObservedAt) {
			return false, nil
		}
	}
	current, owned := archive.ProjectActivation{}, false
	if intent.CodexPolicyToken == "" {
		current, owned = ConfiguredProjectActivationFor(cfg, intent.ProjectRoot)
	}
	if intent.CodexPolicyToken == "" && (!owned || !current.Included || current.Root != intent.ProjectRoot || cfg.DestinationID() != intent.DestinationID || cfg.PauseGeneration != intent.PauseGeneration || !cfg.Archive.Eligible(current.Root, intent.ObservedAt)) {
		return false, nil
	}
	entries, err := os.ReadDir(admissionIntentDir(home))
	if !os.IsNotExist(err) && err != nil {
		return false, err
	}
	// Only queued intents count. Hooks stage theirs in the same folder before
	// taking the lock, and those temporary files, this hook's own among them,
	// are not in the queue.
	queued := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			queued++
		}
	}
	if queued >= maxAdmissionIntents {
		return false, errors.New("admission intent queue is full")
	}
	return true, nil
}

type deferredFollowup struct {
	path   string
	intent admissionIntent
	events []agentapi.LifecycleEvent
}

// ReplayAdmissionIntents runs before the collector reads registrations. The
// current configuration and project activation are checked again; an intent
// never revives an excluded project. Successfully handled and expired intents
// are removed, while transient failures remain for the next pass. Hook
// registration is idempotent by native session ID.
func ReplayAdmissionIntents(home string, now time.Time, lookups ...agentapi.DecodersLookup) error {
	var lookup agentapi.DecodersLookup
	if len(lookups) > 0 {
		lookup = lookups[0]
	}
	return replayAdmissionIntents(home, now, lookup, nil)
}

type admissionReplayCursor struct {
	After string `json:"after"`
}

func replayProjectPreflight(home string, now time.Time) (map[string]sourcefacts.ProjectFacts, admissionReplayCursor) {
	preflight := map[string]sourcefacts.ProjectFacts{}
	entriesBefore, _ := os.ReadDir(admissionIntentDir(home))
	cursorPath := filepath.Join(home, "admission-replay-cursor.json")
	var cursor admissionReplayCursor
	_ = local.Read(cursorPath, &cursor)
	if len(cursor.After) > 128 || filepath.Base(cursor.After) != cursor.After {
		cursor.After = ""
	}
	// The cursor schedules metadata work only; eligibility and identity are
	// always checked from the original intent and current policy under the lock.
	start := sort.Search(len(entriesBefore), func(i int) bool { return entriesBefore[i].Name() > cursor.After })
	resolver := sourcefacts.NewProjectResolver()
	lastAttempt := ""
	for i := range entriesBefore {
		entry := entriesBefore[(start+i)%len(entriesBefore)]
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(admissionIntentDir(home), entry.Name())
		var intent admissionIntent
		if local.Read(path, &intent) == nil && intent.CodexPolicyToken != "" && len(intent.Effects) > 0 {
			if now.Sub(intent.ObservedAt) > maxAdmissionIntentAge || intent.ObservedAt.After(now.Add(time.Minute)) {
				continue
			}
			facts, ok := resolver.Resolve(intent.ProjectRoot)
			if resolver.Exhausted {
				if lastAttempt == "" {
					lastAttempt = entry.Name()
				}
				break
			}
			lastAttempt = entry.Name()
			if ok {
				preflight[path] = facts
			}
		}
	}
	return preflight, admissionReplayCursor{After: lastAttempt}
}

func replayAdmissionIntents(home string, now time.Time, lookup agentapi.DecodersLookup, after func(effectName) error) error {
	if setupjournal.TransactionPending(home) {
		return nil
	}
	preflight, cursor := replayProjectPreflight(home, now)
	unlock, err := local.NamedLockWait(home, "hooks.lock", 2*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	entries, err := os.ReadDir(admissionIntentDir(home))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	cfg, found, err := config.Load(home)
	if err != nil {
		return err
	}
	if !found || !cfg.Archive.Enabled || cfg.Paused {
		return nil
	}
	if cursor.After != "" {
		if err := local.Write(filepath.Join(home, "admission-replay-cursor.json"), cursor); err != nil {
			return fmt.Errorf("save admission replay progress: %w", err)
		}
	}
	store, err := state.Open(home)
	if err != nil {
		return err
	}
	var failures []error
	var deferred []deferredFollowup
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(admissionIntentDir(home), entry.Name())
		followup, err := replayAdmissionFile(home, store, cfg, path, now, lookup, after, preflight)
		if err != nil {
			failures = append(failures, err)
		}
		if followup != nil {
			deferred = append(deferred, *followup)
		}
	}
	for _, followup := range deferred {
		if err := replayDeferredFollowup(store, cfg, followup, after); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func replayAdmissionFile(home string, store *state.Store, cfg config.Config, path string, now time.Time, lookup agentapi.DecodersLookup, after func(effectName) error, preflights ...map[string]sourcefacts.ProjectFacts) (*deferredFollowup, error) {
	var intent admissionIntent
	if err := local.Read(path, &intent); err != nil {
		return nil, fmt.Errorf("read admission intent: %w", err)
	}
	if !replayIntentEligible(cfg, intent, now) {
		return nil, removeAdmissionIntent(path)
	}
	if intent.CodexPolicyToken != "" {
		if len(preflights) == 0 || len(intent.Effects) == 0 {
			return nil, nil
		}
		facts, ok := preflights[0][path]
		event := intent.Effects[0].Event
		if !ok || facts.Root != event.CodexProjectRoot || facts.Cwd != event.CodexCwd {
			return nil, nil
		}
	}
	events, err := intentEvents(intent, lookup)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, removeAdmissionIntent(path)
	}
	events, err = validateBatch(intent.Harness, events, intent.ObservedAt)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event.Session.NativeID != intent.NativeSessionID || string(event.Session.Agent) != archive.CanonicalHarness(intent.Harness) || event.ProjectRoot != intent.ProjectRoot {
			return nil, errors.New("admission intent effect ownership conflict")
		}
	}
	key, err := agentmeta.NewSessionKey(intent.Harness, intent.NativeSessionID)
	if err != nil {
		return nil, err
	}
	registered, err := HasRegistration(store, key)
	if err != nil {
		return nil, fmt.Errorf("look up admission intent: %w", err)
	}
	if registered {
		matches, err := replayRegistrationMatches(store, cfg, intent)
		if err != nil {
			return nil, err
		}
		if !matches {
			return nil, removeAdmissionIntent(path)
		}
	}
	hasStart := false
	for _, event := range events {
		if event.Deferred == agentapi.DeferredStart && event.Start.Kind == agentapi.FreshExplicit {
			hasStart = true
		}
	}
	if !registered && !hasStart && events[0].Deferred == agentapi.DeferredFollowup {
		return &deferredFollowup{path: path, intent: intent, events: events}, nil
	}
	if err := replayEffects(home, store, cfg, intent, events, registered, after); err != nil {
		return nil, err
	}
	return nil, acknowledgeIntent(path, after)
}

func replayIntentEligible(cfg config.Config, intent admissionIntent, now time.Time) bool {
	if now.Sub(intent.ObservedAt) > maxAdmissionIntentAge || intent.ObservedAt.After(now.Add(time.Minute)) {
		return false
	}
	if intent.CodexPolicyToken != "" {
		if intent.Harness != "codex" || len(intent.Effects) == 0 || intent.DestinationID != cfg.DestinationID() || intent.PauseGeneration != cfg.PauseGeneration {
			return false
		}
		e := intent.Effects[0].Event
		token, ok := cfg.CodexGeneration(e.CodexProjectRoot, e.CodexCwd, intent.ObservedAt, now)
		return ok && token == intent.CodexPolicyToken
	}
	project, owned := ConfiguredProjectActivationFor(cfg, intent.ProjectRoot)
	return owned && project.Included && project.Root == intent.ProjectRoot &&
		intent.DestinationID == cfg.DestinationID() && intent.PauseGeneration == cfg.PauseGeneration &&
		intentProjectStillOwned(intent.ProjectRoot, cfg.Archive.Projects) &&
		cfg.Archive.Eligible(project.Root, intent.ObservedAt)
}

func intentEvents(intent admissionIntent, lookup agentapi.DecodersLookup) ([]agentapi.LifecycleEvent, error) {
	if intent.Version == 1 {
		var events []agentapi.LifecycleEvent
		for _, effect := range intent.Effects {
			if len(effect.Event.Evidence) > 0 || effect.Event.Child != nil {
				return nil, errors.New("admission replay effects must be content-free")
			}
			events = append(events, effect.Event)
		}
		return events, nil
	}
	if intent.Version != 0 {
		return nil, errors.New("unsupported admission intent version")
	}
	if lookup == nil {
		return nil, errors.New("legacy admission decoder required")
	}
	decoder, ok := lookup.LookupDecoder(intent.Harness)
	if !ok {
		return nil, errors.New("legacy admission decoder unavailable")
	}
	legacy, ok := decoder.(agentapi.LegacyHookDecoder)
	if !ok {
		return nil, errors.New("legacy admission translation unavailable")
	}
	return legacy.DecodeLegacy(intent)
}

func replayEffects(home string, store *state.Store, cfg config.Config, intent admissionIntent, events []agentapi.LifecycleEvent, registered bool, after func(effectName) error) error {
	prepared := make([]agentapi.LifecycleEvent, len(events))
	copy(prepared, events)
	for i, event := range prepared {
		if event.Session.NativeID != intent.NativeSessionID || string(event.Session.Agent) != archive.CanonicalHarness(intent.Harness) || event.ProjectRoot != intent.ProjectRoot {
			return errors.New("admission intent effect identity conflict")
		}
		event.Evidence = nil
		if event.Kind == agentapi.EventStart || event.Kind == agentapi.EventStop {
			event.Evidence = append(event.Evidence, minimalReplayEvidence(archive.EvidenceKindLifecycleHook, event, intent.ObservedAt))
		}
		if event.Kind == agentapi.EventStop || event.Kind == agentapi.EventResponse {
			event.Evidence = append(event.Evidence, minimalReplayEvidence(archive.EvidenceKindFinalResponse, event, intent.ObservedAt))
		}
		prepared[i] = event
	}
	prepared, err := validateBatch(intent.Harness, prepared, intent.ObservedAt)
	if err != nil {
		return err
	}
	var waiting []agentapi.LifecycleEvent
	for _, event := range prepared {
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
		if registered && event.Deferred == agentapi.DeferredStart {
			key, err := eventKey(event)
			if err != nil {
				return err
			}
			id, found, err := store.ArchiveSessionID(key)
			if err != nil {
				return err
			}
			if !found {
				return state.ErrSessionNotRegistered
			}
			reg, found, err := store.LoadRegistration(id)
			if err != nil {
				return err
			}
			if !found {
				return state.ErrSessionNotRegistered
			}
			reg, err = completeReplayStart(store, cfg, intent, event, reg, after)
			if err != nil {
				return err
			}
			if err := adoptLocator(store, &reg, event, after); err != nil {
				return err
			}
			// A durable registration may precede its evidence write. Complete both
			// replay-safe effects before acknowledging the original proven intent.
			if err := saveLifecycleEvidence(store, id, event, intent.ObservedAt, after); err != nil {
				return err
			}
			continue
		}
		if err := applyEvent(home, store, cfg, event, intent.ObservedAt, gitLookups{lastHead: intent.LastHead}, after, intent.Replay); err != nil {
			return err
		}
		// Complete earlier waiting effects immediately after the admitting
		// start, before any subsequent native effect. The durable original
		// batch remains the retry record until all effects succeed.
		if event.Kind == agentapi.EventStart && len(waiting) > 0 {
			if err := applyWaitingReplayEffects(home, store, cfg, intent, waiting, after); err != nil {
				return err
			}
			waiting = nil
		}
	}
	if len(waiting) > 0 {
		return state.ErrSessionNotRegistered
	}
	return nil
}

// Complete later native start observations after an interrupted registration
// create, while preserving a newer live continuation and NewOnly semantics.
func completeReplayStart(store *state.Store, cfg config.Config, intent admissionIntent, event agentapi.LifecycleEvent, reg archive.SessionRegistration, after func(effectName) error) (archive.SessionRegistration, error) {
	if event.NewOnly || !sameReplayAdmission(reg, intent.ObservedAt) {
		return reg, nil
	}
	updated, err := store.UpdateRegistration(reg.ArchiveSessionID, func(current *archive.SessionRegistration) error {
		if !replayProjectMatches(cfg, intent, *current) || current.NativeSessionID != intent.NativeSessionID || archive.CanonicalHarness(current.Harness.Name) != archive.CanonicalHarness(intent.Harness) {
			return errContinuationDeclined
		}
		if sameReplayAdmission(*current, intent.ObservedAt) {
			applyLocator(current, event)
			applyObservation(&current.Harness, event.Session)
		}
		reg = *current
		return nil
	})
	if err != nil {
		return reg, err
	}
	if !updated {
		return reg, state.ErrSessionNotRegistered
	}
	return reg, effectBoundary(after, effectRegistrationUpdate)
}

// Existing replay provenance has observation-time granularity, not an intent ID.
// Only a hook admission whose three timestamps still match can be completed by
// an old start; an imported or subsequently refreshed registration is kept.
func sameReplayAdmission(reg archive.SessionRegistration, observed time.Time) bool {
	return reg.Origin == archive.SessionOriginHook && reg.StartedAtSource == archive.StartedAtSourceHook &&
		reg.AdmittedAt.Equal(observed) && reg.SessionStartedAt.Equal(observed) && reg.RegisteredAt.Equal(observed)
}

func applyWaitingReplayEffects(home string, store *state.Store, cfg config.Config, intent admissionIntent, waiting []agentapi.LifecycleEvent, after func(effectName) error) error {
	for _, event := range waiting {
		key, err := eventKey(event)
		if err != nil {
			return err
		}
		found, err := HasRegistration(store, key)
		if err != nil {
			return err
		}
		if !found {
			return state.ErrSessionNotRegistered
		}
		if err := applyEvent(home, store, cfg, event, intent.ObservedAt, gitLookups{lastHead: intent.LastHead}, after, intent.Replay); err != nil {
			return err
		}
	}
	return nil
}

func minimalReplayEvidence(kind archive.SupplementalEvidenceKind, event agentapi.LifecycleEvent, at time.Time) archive.SupplementalEvidence {
	return archive.SupplementalEvidence{Kind: kind, ObservedAt: at, Provenance: "hook:" + string(event.Session.Agent) + ":" + event.Reason, Payload: map[string]any{"event_name": event.NativeEvent}}
}

func replayDeferredFollowup(store *state.Store, cfg config.Config, followup deferredFollowup, after func(effectName) error) error {
	key, err := agentmeta.NewSessionKey(followup.intent.Harness, followup.intent.NativeSessionID)
	if err != nil {
		return err
	}
	registered, err := HasRegistration(store, key)
	if err != nil {
		return err
	}
	if !registered {
		return nil
	}
	matches, err := replayRegistrationMatches(store, cfg, followup.intent)
	if err != nil {
		return err
	}
	if !matches {
		return removeAdmissionIntent(followup.path)
	}
	if err := replayEffects(store.Home(), store, cfg, followup.intent, followup.events, true, after); err != nil {
		return err
	}
	return acknowledgeIntent(followup.path, after)
}

func acknowledgeIntent(path string, after func(effectName) error) error {
	if err := removeAdmissionIntent(path); err != nil {
		return err
	}
	return effectBoundary(after, effectIntentAck)
}

func removeAdmissionIntent(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func replayRegistrationMatches(store *state.Store, cfg config.Config, intent admissionIntent) (bool, error) {
	archiveID, found, err := store.ArchiveSessionID(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(intent.Harness)), NativeID: intent.NativeSessionID})
	if err != nil || !found {
		return false, err
	}
	reg, found, err := store.LoadRegistration(archiveID)
	if err != nil || !found {
		return false, err
	}
	return reg.HistoricalProject == nil && replayProjectMatches(cfg, intent, reg) &&
		archive.CanonicalHarness(reg.Harness.Name) == archive.CanonicalHarness(intent.Harness) && reg.NativeSessionID == intent.NativeSessionID && cfg.AcceptSession(reg), nil
}

// replayProjectMatches consumes the original intent facts already checked
// against physical resolution before hooks.lock; legacy root matching stays exact.
func replayProjectMatches(cfg config.Config, intent admissionIntent, reg archive.SessionRegistration) bool {
	if intent.CodexPolicyToken != "" {
		return len(intent.Effects) > 0 && codexContinuationAccepted(cfg, intent.Effects[0].Event, reg)
	}
	return cfg.AcceptSession(reg) && filepath.Clean(reg.ProjectRoot) == filepath.Clean(intent.ProjectRoot)
}
