package capture

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/state"
)

type museAdmissionContext struct{ intent agentapi.AdmissionIntent }

func prepareMuseAdmission(cfg config.Config, batch []agentapi.LifecycleEvent, observedAt time.Time) (*museAdmissionContext, error) {
	if len(batch) != 1 {
		return nil, errors.New("Muse hook requires one root observation")
	}
	e := batch[0]
	if e.Session.Agent != agentmeta.Muse || e.NativeHookContract != "muse-root-hooks-v1" || e.Kind != agentapi.EventStart || e.NativeEvent != "SessionStart" || e.Start.Kind != agentapi.FreshUnknown || e.Start.Path != "" || e.Start.Reason != agentapi.FreshnessUnknownSource || e.Locator != agentapi.LocatorNone || e.Source.Path != "" || e.Source.Key != "" || e.Child != nil || e.Deferred != agentapi.DeferredNone || e.NewOnly || len(e.Evidence) != 0 || cfg.MuseCapture == nil {
		return nil, errors.New("Muse startup observation requires committed admission window")
	}
	project, owned := ConfiguredProjectActivationFor(cfg, e.ProjectRoot)
	if !owned || !project.Included || project.ActivatedAt.IsZero() {
		return nil, errors.New("Muse startup requires explicit included project")
	}
	var root archive.NativeRoot
	for _, r := range cfg.NativeRoots {
		if r.Agent == "muse" {
			root = r
		}
	}
	digest, err := archive.NativeRootFingerprint(root)
	if err != nil {
		return nil, err
	}
	c := &museAdmissionContext{intent: agentapi.AdmissionIntent{Version: 2, NativeMode: agentapi.NativeStartupCandidate, Harness: "muse", Event: "SessionStart", NativeSessionID: e.Session.NativeID, ProjectRoot: project.Root, ObservedCwd: e.ProjectRoot, ObservedAt: observedAt.UTC(), DestinationID: cfg.DestinationID(), PauseGeneration: cfg.PauseGeneration, NativeAdmission: &archive.NativeAdmission{Contract: "muse-root-admission", HookContract: e.NativeHookContract, RootFingerprint: digest, WindowGeneration: cfg.MuseCapture.Generation, PolicyFingerprint: cfg.MuseCapture.PolicyFingerprint}}}
	if err := checkMuseAdmission(cfg, c, nil, observedAt); err != nil {
		return nil, err
	}
	return c, nil
}

func checkMuseAdmission(cfg config.Config, original *museAdmissionContext, owner *archive.SessionRegistration, at time.Time) error {
	if original == nil || owner != nil || !RecognizedAdmissionIntent(original.intent) || !MuseIntentEligible(cfg, original.intent, at) {
		return errors.New("Muse original admission window is no longer current")
	}
	return nil
}

// MuseIntentEligible checks original captured authority without renewing it.
func MuseIntentEligible(cfg config.Config, intent agentapi.AdmissionIntent, at time.Time) bool {
	if !RecognizedAdmissionIntent(intent) || intent.Version != 2 || intent.NativeMode != agentapi.NativeStartupCandidate || !cfg.NativeAdmissionProtection || !cfg.NativeAuthorityProtection || cfg.MuseCapture == nil || cfg.Paused || !cfg.Archive.Enabled || intent.DestinationID != cfg.DestinationID() || intent.PauseGeneration != cfg.PauseGeneration || at.Sub(intent.ObservedAt) > maxAdmissionIntentAge || intent.ObservedAt.After(at.Add(time.Minute)) || intent.ObservedAt.Before(cfg.MuseCapture.Since) {
		return false
	}
	a, window := intent.NativeAdmission, cfg.MuseCapture
	fingerprint, err := config.MuseAdmissionPolicyFingerprint(cfg)
	if err != nil || a.WindowGeneration != window.Generation || a.PolicyFingerprint != window.PolicyFingerprint || fingerprint != a.PolicyFingerprint {
		return false
	}
	matched := false
	for _, root := range cfg.NativeRoots {
		if root.Agent == "muse" {
			digest, err := archive.NativeRootFingerprint(root)
			matched = err == nil && digest == a.RootFingerprint
		}
	}
	project, included := archive.ProjectActivation{}, false
	for _, p := range cfg.Archive.Projects {
		if p.Root == intent.ProjectRoot {
			project = p
			included = p.Included
		}
	}
	return matched && included && project.Included && cfg.Archive.Eligible(project.Root, intent.ObservedAt)
}

// RecognizedAdmissionIntent separates supported owned records from future wire
// before eligibility, acknowledgement, expiry or queue mutation.
func RecognizedAdmissionIntent(i agentapi.AdmissionIntent) bool {
	if i.Version == 0 || i.Version == 1 {
		return i.NativeMode == "" && i.NativeAdmission == nil && i.NativeOwnerID == "" && i.ObservedCwd == "" && i.Harness != "muse"
	}
	if i.Version == 2 && i.NativeMode == agentapi.NativeOwnerContinuation {
		if i.Harness != "muse" || i.NativeAdmission == nil || i.NativeAdmission.Validate() != nil || i.NativeOwnerID == "" || len(i.Effects) != 1 || i.TranscriptPath != "" || i.Replay != nil || i.LastHead != nil || i.ObservedAt.IsZero() {
			return false
		}
		e := i.Effects[0].Event
		return e.Session.Agent == agentmeta.Muse && e.Session.NativeID == i.NativeSessionID && e.NativeHookContract == "muse-root-hooks-v1" && e.NativeEvent == i.Event && (e.Kind == agentapi.EventTurnStart || e.Kind == agentapi.EventStop) && e.Source.Path == "" && e.Source.Key == "" && e.Locator == agentapi.LocatorNone && e.Start.Kind == agentapi.FreshUnknown && e.Child == nil && len(e.Evidence) == 0 && e.Deferred == agentapi.DeferredNone
	}
	return i.Version == 2 && i.Harness == "muse" && i.NativeMode == agentapi.NativeStartupCandidate && i.NativeAdmission != nil && i.NativeAdmission.Validate() == nil && i.NativeOwnerID == "" && i.Event == "SessionStart" && i.NativeSessionID != "" && i.ProjectRoot != "" && filepath.IsAbs(i.ObservedCwd) && i.TranscriptPath == "" && i.CodexPolicyToken == "" && i.CursorVersion == "" && i.ComposerMode == "" && len(i.Effects) == 0 && i.Replay == nil && i.LastHead == nil && !i.ObservedAt.IsZero()
}

func handleMuseBatch(home string, batch []agentapi.LifecycleEvent, now time.Time, lock lockHooks, afterLock func(), o eventOptions) error {
	cfg, found, err := config.Load(home)
	if err != nil || !found || setupjournal.TransactionPending(home) {
		return err
	}
	store, err := state.Open(home)
	if err != nil {
		return err
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: batch[0].Session.NativeID}
	id, owned, err := store.ForHook().ArchiveSessionID(key)
	if err != nil {
		return nil
	} // no hook-triggered index recovery
	var original *museAdmissionContext
	if owned {
		owner, found, err := store.LoadRegistration(id)
		if err != nil || !found {
			return err
		}
		if owner.NativeAdmission == nil || owner.NativeBinding == nil || owner.NativeBinding.Version != 2 || owner.NativeAdmission.Validate() != nil || owner.NativeBinding.Validate() != nil || !cfg.AcceptSession(owner) {
			return nil
		}
		project, included := ConfiguredProjectActivationFor(cfg, batch[0].ProjectRoot)
		if !included || !project.Included || project.Root != owner.ProjectRoot {
			return nil
		}
		proof := *owner.NativeAdmission
		event := batch[0]
		if event.Kind == agentapi.EventStart {
			event.Kind = agentapi.EventTurnStart
			event.Start = agentapi.StartEvidence{}
		}
		intent := agentapi.AdmissionIntent{Version: 2, Harness: "muse", NativeMode: agentapi.NativeOwnerContinuation, NativeOwnerID: id, NativeAdmission: &proof, NativeSessionID: key.NativeID, ProjectRoot: owner.ProjectRoot, ObservedCwd: batch[0].ProjectRoot, ObservedAt: now.UTC(), Event: event.NativeEvent, DestinationID: owner.DestinationID, PauseGeneration: cfg.PauseGeneration, Effects: []agentapi.ReplayEffect{{Event: event}}}
		if _, err := store.ForNativeContinuation(context.Background(), intent); err != nil {
			return nil
		}
		original = &museAdmissionContext{intent: intent}
	} else {
		if batch[0].Kind != agentapi.EventStart {
			return nil
		}
		original, err = prepareMuseAdmission(cfg, batch, now)
		if err != nil {
			return nil
		}
	}
	id, err = local.ID()
	if err != nil {
		return err
	}
	path := filepath.Join(admissionIntentDir(home), fmt.Sprintf("%020d-%s.json", now.UnixNano(), id))
	staged, err := stageEventIntent(home, original.intent, path, local.StageInExistingDir)
	if err != nil || staged == nil {
		return err
	}
	defer staged.Discard()
	if lock == nil {
		lock = func(home string, wait time.Duration) (func(), error) {
			return local.NamedLockWait(home, "hooks.lock", wait)
		}
	}
	unlock, err := lock(home, time.Second)
	if err != nil {
		return err
	}
	if afterLock != nil {
		afterLock()
	}
	current, found, err := config.Load(home)
	if err == nil && found && !setupjournal.TransactionPending(home) {
		if original.intent.NativeMode == agentapi.NativeStartupCandidate {
			err = checkMuseAdmission(current, original, nil, now)
		} else {
			_, err = store.ForNativeContinuation(context.Background(), original.intent)
		}
	} else if err == nil {
		err = errors.New("Muse capture setup changed")
	}
	if err == nil {
		_, err = commitEventIntent(home, original.intent, staged)
	}
	unlock()
	if err != nil {
		return err
	}
	return staged.SyncDir()
}
