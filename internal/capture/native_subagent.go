package capture

import (
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"time"
)

// A Codex child stop is an observation about a stable native thread. It cannot
// license a new child registration, accept a mutable hook path, or manufacture
// a Claude composite identity. The native catalog admits new children itself.
func nativeCodexSubagentStop(store *state.Store, cfg config.Config, parent archive.SessionRegistration, event agentapi.LifecycleEvent, now time.Time) error {
	if event.Child == nil || event.Child.ID == "" || codexmeta.RolloutID(event.Child.ID+".jsonl") != event.Child.ID {
		return nil
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Codex, NativeID: event.Child.ID}
	id, known, err := store.ArchiveSessionID(key)
	if errors.Is(err, state.ErrSessionIndexRecoveryRequired) {
		return errors.Join(err, store.RequestSessionIndexRecovery(key))
	}
	if err != nil || !known {
		return err
	}
	child, found, err := store.LoadRegistration(id)
	if err != nil || !found {
		return err
	}
	if parent.HistoricalProject != nil || child.HistoricalProject != nil || !nativeStopRelationshipMatches(parent, child) || !cfg.AcceptSession(child) {
		return nil
	}
	if _, removed, err := store.Removal("codex", child.NativeSessionID); err != nil {
		return err
	} else if removed {
		return nil
	}
	if _, removed, err := store.Removal("codex", parent.NativeSessionID); err != nil {
		return err
	} else if removed {
		return nil
	}
	found, err = store.UpdateRegistration(id, func(current *archive.SessionRegistration) error {
		if !nativeStopRelationshipMatches(parent, *current) || !cfg.AcceptSession(*current) {
			return errors.New("native child stop relationship changed")
		}
		current.HookObservedAt = now
		return nil
	})
	if err != nil || !found {
		return err
	}
	if err := store.SaveRequest(id, "native-child-stop", now); err != nil {
		return err
	}
	evidence, err := archive.NewLinkedSessionEvidence(id, archive.LinkedSessionPending, now)
	if err != nil {
		return err
	}
	return store.SaveRequest(parent.ArchiveSessionID, "native-child-observed", now, evidence)
}

func nativeStopHome(reg archive.SessionRegistration) string {
	if reg.CodexBinding != nil && reg.CodexBinding.Home != "" {
		return reg.CodexBinding.Home
	}
	if reg.NativeSourceHome != "" {
		return reg.NativeSourceHome
	}
	return reg.DiscoveryRoot
}

func nativeStopRelationshipMatches(parent, child archive.SessionRegistration) bool {
	return child.Harness.Name == "codex" && child.NativeChild && child.ParentNativeSessionID == parent.NativeSessionID && child.ProjectID == parent.ProjectID && child.ProjectRoot == parent.ProjectRoot && child.DestinationID == parent.DestinationID && nativeStopHome(child) != "" && nativeStopHome(child) == nativeStopHome(parent)
}
