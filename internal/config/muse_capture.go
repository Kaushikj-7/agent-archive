package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// MuseCaptureConfig records one committed, forward-only root capture window.
type MuseCaptureConfig struct {
	Generation        string    `json:"generation"`
	Since             time.Time `json:"since"`
	PolicyFingerprint string    `json:"policy_fingerprint"`
}

// MuseAdmissionPolicyFingerprint covers the complete explicit root policy.
func MuseAdmissionPolicyFingerprint(c Config) (string, error) {
	var root archive.NativeRoot
	count := 0
	for _, candidate := range c.NativeRoots {
		if candidate.Agent == "muse" {
			root = candidate
			count++
		}
	}
	if count != 1 {
		return "", errors.New("Muse capture requires one explicit native root")
	}
	digest, err := archive.NativeRootFingerprint(root)
	if err != nil {
		return "", err
	}
	projects := slices.Clone(c.Archive.Projects)
	slices.SortFunc(projects, func(a, b archive.ProjectActivation) int {
		if a.Root < b.Root {
			return -1
		}
		if a.Root > b.Root {
			return 1
		}
		if a.ProjectID < b.ProjectID {
			return -1
		}
		if a.ProjectID > b.ProjectID {
			return 1
		}
		if a.Included != b.Included {
			if !a.Included {
				return -1
			}
			return 1
		}
		return a.ActivatedAt.Compare(b.ActivatedAt)
	})
	raw, err := json.Marshal(struct {
		Domain           string
		Root             string
		Enabled          bool
		Selected         bool
		Destination      string
		DestinationSince time.Time
		Projects         []archive.ProjectActivation
	}{"agent-archive/muse-admission-policy/v1", digest, c.Archive.Enabled, slices.Contains(c.Harnesses, "muse"), c.DestinationID(), c.DestinationSince, projects})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func museCaptureEnabled(c Config) bool {
	if !c.Archive.Enabled || c.Paused || !slices.Contains(c.Harnesses, "muse") || c.DestinationID() == "" || c.Storage.Bucket == "" || c.DestinationSince.IsZero() {
		return false
	}
	for _, p := range c.Archive.Projects {
		if p.Included && !p.ActivatedAt.IsZero() {
			return true
		}
	}
	return false
}

// ReconcileMuseCapture issues only committed policy transitions. Preview callers
// must not invoke it. Unrelated saves retain the exact generation and timestamp.
func ReconcileMuseCapture(next, previous Config, at time.Time) (Config, error) {
	floor := previous.MuseCaptureLastIssuedAt
	if previous.MuseCapture != nil && previous.MuseCapture.Since.After(floor) {
		floor = previous.MuseCapture.Since
	}
	next.MuseCaptureLastIssuedAt = floor
	next.NativeAdmissionProtection = next.NativeAdmissionProtection || previous.NativeAdmissionProtection || previous.MuseCapture != nil || !floor.IsZero()
	if !museCaptureEnabled(next) {
		next.MuseCapture = nil
		return next, nil
	}
	fingerprint, err := MuseAdmissionPolicyFingerprint(next)
	if err != nil {
		return Config{}, err
	}
	next.NativeAdmissionProtection = true
	next.NativeAuthorityProtection = true
	old := previous.MuseCapture
	if old != nil && old.PolicyFingerprint == fingerprint && previous.PauseGeneration == next.PauseGeneration && museCaptureEnabled(previous) {
		copied := *old
		next.MuseCapture = &copied
		return next, nil
	}
	if at.IsZero() || !floor.IsZero() && !at.After(floor) || at.Before(next.DestinationSince) {
		return Config{}, errors.New("Muse capture requires a forward window issuance time")
	}
	generation, err := local.ID()
	if err != nil {
		return Config{}, err
	}
	next.MuseCapture = &MuseCaptureConfig{Generation: generation, Since: at.UTC(), PolicyFingerprint: fingerprint}
	next.MuseCaptureLastIssuedAt = at.UTC()
	return next, nil
}

func validateMuseCapture(c Config) error {
	if !c.MuseCaptureLastIssuedAt.IsZero() && !c.NativeAdmissionProtection {
		return errors.New("Muse issuance floor requires admission protection")
	}
	if c.MuseCapture == nil {
		return nil
	}
	p := c.MuseCapture
	proof := archive.NativeAdmission{Contract: "muse-root-admission", RootFingerprint: p.PolicyFingerprint, PolicyFingerprint: p.PolicyFingerprint, WindowGeneration: p.Generation, HookContract: "muse-root-hooks-v1"}
	fingerprint, err := MuseAdmissionPolicyFingerprint(c)
	if err != nil || proof.Validate() != nil || !c.NativeAdmissionProtection || !c.NativeAuthorityProtection || !museCaptureEnabled(c) || p.Since.IsZero() || !p.Since.Equal(c.MuseCaptureLastIssuedAt) || p.Since.Before(c.DestinationSince) || p.PolicyFingerprint != fingerprint {
		return errors.New("invalid Muse capture window")
	}
	return nil
}

func validateMuseCaptureTransition(next, previous Config) error {
	if err := validateMuseCapture(next); err != nil {
		return err
	}
	old, current := previous.MuseCapture, next.MuseCapture
	if next.MuseCaptureLastIssuedAt.Before(previous.MuseCaptureLastIssuedAt) {
		return errors.New("Muse issuance floor cannot move backward")
	}
	if current == nil && !next.MuseCaptureLastIssuedAt.Equal(previous.MuseCaptureLastIssuedAt) {
		return errors.New("inactive Muse issuance floor cannot advance")
	}
	if current == nil {
		return nil
	}
	if old != nil && *old == *current {
		if previous.PauseGeneration != next.PauseGeneration {
			return errors.New("Muse capture must rotate after pause transitions")
		}
		return nil
	}
	if old != nil && (old.Generation == current.Generation || !current.Since.After(old.Since)) || !previous.MuseCaptureLastIssuedAt.IsZero() && !current.Since.After(previous.MuseCaptureLastIssuedAt) {
		return errors.New("Muse capture changes require a new forward window")
	}
	return nil
}

// UnmarshalJSON refuses future or ambiguous active-window authority.
func (p *MuseCaptureConfig) UnmarshalJSON(raw []byte) error {
	if len(raw) > 1024 {
		return errors.New("Muse capture window exceeds bound")
	}
	if err := exactAuthorityObject(raw, []string{"generation", "since", "policy_fingerprint"}); err != nil {
		return err
	}
	type plain MuseCaptureConfig
	var next plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&next); err != nil {
		return err
	}
	*p = MuseCaptureConfig(next)
	return nil
}
