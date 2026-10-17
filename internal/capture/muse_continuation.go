package capture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

// Existing-owner effects use the same queue, but never its startup inventory.
// They run before the legacy replay's hooks lock so native continuation writes
// can stage first and acquire hooks only for their individual final CAS.
func replayMuseOwnerIntents(home string, at time.Time, after func(effectName) error) error {
	cfg, found, err := config.Load(home)
	if err != nil || !found {
		return err
	}
	directory, err := os.Open(admissionIntentDir(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(maxAdmissionIntents)
	if err != nil && len(entries) == 0 {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(admissionIntentDir(home), entry.Name())
		var intent agentapi.AdmissionIntent
		if local.Read(path, &intent) != nil || !RecognizedAdmissionIntent(intent) || intent.NativeMode != agentapi.NativeOwnerContinuation {
			continue
		}
		if at.Sub(intent.ObservedAt) > maxAdmissionIntentAge || intent.ObservedAt.After(at.Add(time.Minute)) {
			continue
		}
		store, err := state.Open(home)
		if err != nil {
			return err
		}
		scoped, err := store.ForNativeContinuation(context.Background(), intent)
		if err != nil {
			continue
		}
		event := intent.Effects[0].Event
		event.Evidence = []archive.SupplementalEvidence{minimalReplayEvidence(archive.EvidenceKindLifecycleHook, event, intent.ObservedAt)}
		if event.Kind == agentapi.EventStop {
			event.Evidence = append(event.Evidence, minimalReplayEvidence(archive.EvidenceKindFinalResponse, event, intent.ObservedAt))
		}
		if err := applyEvent(home, scoped, cfg, event, intent.ObservedAt, gitLookups{}, after, nil); err != nil {
			return err
		}
		unlock, err := local.NamedLockWait(home, "admission-intents.lock", admissionQueueWait)
		if err != nil {
			return err
		}
		var current agentapi.AdmissionIntent
		same := local.Read(path, &current) == nil && RecognizedAdmissionIntent(current) && current.NativeOwnerID == intent.NativeOwnerID && current.ObservedAt.Equal(intent.ObservedAt) && current.Event == intent.Event && current.PauseGeneration == intent.PauseGeneration && current.NativeAdmission != nil && *current.NativeAdmission == *intent.NativeAdmission
		if same {
			err = acknowledgeIntent(path, after)
		}
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
