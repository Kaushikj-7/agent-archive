package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
func replayMuseOwnerIntents(ctx context.Context, home string, at time.Time, after func(effectName) error) error {
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
	defer func() { _ = directory.Close() }()
	entries, err := directory.ReadDir(maxAdmissionIntents)
	if err != nil && len(entries) == 0 {
		return nil
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(admissionIntentDir(home), entry.Name())
		intent, readErr := readMuseIntent(path)
		if readErr != nil || !RecognizedAdmissionIntent(intent) || intent.Version != 2 {
			continue
		}
		if at.Sub(intent.ObservedAt) > maxAdmissionIntentAge {
			// Expiry removes only this exact recognized observation. It grants no
			// ownership or effects and never interprets future private authority.
			if err := ackMuseOwnerIntent(ctx, home, path, intent, nil); err != nil {
				return err
			}
			continue
		}
		if intent.NativeMode != agentapi.NativeOwnerContinuation || intent.ObservedAt.After(at.Add(time.Minute)) {
			continue
		}
		store, err := state.Open(home)
		if err != nil {
			return err
		}
		scoped, err := store.ForNativeContinuation(ctx, intent)
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
		if err := ackMuseOwnerIntent(ctx, home, path, intent, after); err != nil {
			return err
		}

	}
	return nil
}

func readMuseIntent(path string) (intent agentapi.AdmissionIntent, readErr error) {
	file, err := os.Open(path)
	if err != nil {
		return intent, err
	}
	defer func() { readErr = errors.Join(readErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return intent, err
	}
	if !info.Mode().IsRegular() || info.Size() > agentapi.MaxNativeAdmissionIntentBytes {
		return intent, errors.New("native intent exceeds private wire bound")
	}
	raw, err := io.ReadAll(io.LimitReader(file, agentapi.MaxNativeAdmissionIntentBytes+1))
	if err != nil {
		return intent, err
	}
	if len(raw) > agentapi.MaxNativeAdmissionIntentBytes {
		return intent, errors.New("native intent exceeds private wire bound")
	}
	readErr = json.Unmarshal(raw, &intent)
	return intent, readErr
}

func ackMuseOwnerIntent(ctx context.Context, home, path string, intent agentapi.AdmissionIntent, after func(effectName) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := local.NamedLockWait(home, "admission-intents.lock", admissionQueueWait)
	if err != nil {
		return err
	}
	current, readErr := readMuseIntent(path)
	if readErr == nil && RecognizedAdmissionIntent(current) && ctx.Err() == nil {
		a, _ := json.Marshal(current)
		b, _ := json.Marshal(intent)
		if bytes.Equal(a, b) {
			err = removeAdmissionIntent(path)
			if err == nil {
				err = effectBoundary(after, effectIntentAck)
			}
		}
	}

	unlock()
	if err != nil {
		return err
	}
	return ctx.Err()
}
