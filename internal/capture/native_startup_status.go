package capture

import (
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
)

// NativeStartupObservation is advisory queue state, never admission or proof.
// Pending is a lower bound when Complete is false. No native identity is exposed.
type NativeStartupObservation struct {
	Pending    int       `json:"pending"`
	ObservedAt time.Time `json:"observed_at,omitzero"`
	Complete   bool      `json:"complete"`
}

// ReadNativeStartupObservation reads only the existing private queue, bounded by
// its existing record count and two MiB aggregate decode scratch. It never opens
// a native source or creates a queue, state store, locator or recovery request.
func ReadNativeStartupObservation(ctx context.Context, home string, cfg config.Config, owners []archive.SessionRegistration, at time.Time) (observation NativeStartupObservation, readErr error) {
	observation.Complete = true
	defer func() {
		if readErr != nil {
			observation.Complete = false
		}
	}()
	if err := ctx.Err(); err != nil {
		return observation, err
	}
	root, err := os.OpenRoot(admissionIntentDir(home))
	if errors.Is(err, os.ErrNotExist) {
		return observation, nil
	}
	if err != nil {
		return observation, err
	}
	defer func() { readErr = errors.Join(readErr, root.Close()) }()
	directory, err := root.Open(".")
	if err != nil {
		return observation, err
	}
	defer func() { readErr = errors.Join(readErr, directory.Close()) }()
	entries, err := directory.ReadDir(maxAdmissionIntents + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return observation, err
	}
	if len(entries) > maxAdmissionIntents {
		observation.Complete = false
		entries = entries[:maxAdmissionIntents]
	}
	type pendingObservation struct {
		proof    archive.NativeAdmission
		observed time.Time
	}
	seen := map[string]pendingObservation{}
	remaining := int64(2 << 20)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return observation, err
		}
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		intent, charge, err := readStartupObservationIntent(root, entry.Name(), remaining)
		remaining -= charge
		if err != nil {
			observation.Complete = false
			continue
		}
		if !RecognizedAdmissionIntent(intent) || intent.NativeMode != agentapi.NativeStartupCandidate || !MuseIntentEligible(cfg, intent, at) {
			continue
		}
		previous, exists := seen[intent.NativeSessionID]
		if !exists || intent.ObservedAt.After(previous.observed) {
			seen[intent.NativeSessionID] = pendingObservation{proof: *intent.NativeAdmission, observed: intent.ObservedAt}
		}
	}
	// Only candidate identities occupy extra memory. Reuse the caller's owner
	// snapshot without constructing another unbounded registration index.
	for _, owner := range owners {
		pending, exists := seen[owner.NativeSessionID]
		if exists && owner.Harness.Name == "muse" && owner.NativeAdmission != nil && *owner.NativeAdmission == pending.proof && owner.NativeBinding != nil && owner.NativeBinding.Version == 2 && owner.NativeBinding.Validate() == nil {
			delete(seen, owner.NativeSessionID)
		}
	}
	observation.Pending = len(seen)
	for _, pending := range seen {
		if pending.observed.After(observation.ObservedAt) {
			observation.ObservedAt = pending.observed
		}
	}

	return observation, nil
}

func readStartupObservationIntent(root *os.Root, name string, remaining int64) (intent agentapi.AdmissionIntent, charge int64, readErr error) {
	file, err := root.Open(name)
	if err != nil {
		return intent, 0, err
	}
	defer func() { readErr = errors.Join(readErr, file.Close()) }()
	before, err := file.Stat()
	if err != nil {
		return intent, 0, err
	}
	if !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > remaining/6 {
		return intent, 0, errors.New("native startup observation exceeds read bound")
	}
	charge = 6 * before.Size()
	raw, err := io.ReadAll(io.LimitReader(file, before.Size()+1))
	if err != nil {
		return intent, charge, err
	}
	after, err := file.Stat()
	if err != nil {
		return intent, charge, err
	}
	if int64(len(raw)) != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return intent, charge, errors.New("native startup observation changed while reading")
	}
	err = json.Unmarshal(raw, &intent)
	return intent, charge, err
}
