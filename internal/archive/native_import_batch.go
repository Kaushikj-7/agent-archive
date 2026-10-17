package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
)

const (
	// MaxNativeImportBatchBytes bounds the closed confirmation subsection.
	MaxNativeImportBatchBytes = 1 << 20
	// MaxNativeImportBatchSelections is a refusal limit, never truncation.
	MaxNativeImportBatchSelections = 256
)

// NativeImportBatchConfirmation freezes one reviewed local batch instance.
// It stores intent; it is not an opaque prepared source or first-owner proof.
type NativeImportBatchConfirmation struct {
	Contract    string                 `json:"contract"`
	Instance    string                 `json:"instance"`
	ScopeSHA256 string                 `json:"scope_sha256"`
	Scope       NativeImportBatchScope `json:"scope"`
}

// NativeImportBatchScope retains resolved bounds as well as original filters.
// No native path, prompt or raw source is retained in this control.
type NativeImportBatchScope struct {
	BatchID           string                       `json:"batch_id"`
	StartedAtUTC      string                       `json:"started_at_utc"`
	DestinationID     string                       `json:"destination_id"`
	Filters           ImportBatchFilters           `json:"filters"`
	SinceUTC          string                       `json:"since_utc"`
	UntilExclusiveUTC string                       `json:"until_exclusive_utc"`
	ProjectIDs        []string                     `json:"project_ids"`
	Selected          []NativeImportBatchSelection `json:"selected"`
}

// NativeImportBatchSelection names the ordinary import route, not observed
// ancestry/descendant coverage. Children require separately qualified proof.
type NativeImportBatchSelection struct {
	Kind            string           `json:"kind"`
	NativeID        string           `json:"native_id"`
	OriginalTUTC    string           `json:"original_t_utc"`
	ProjectID       string           `json:"project_id"`
	RootFingerprint string           `json:"root_fingerprint"`
	RootRevision    string           `json:"root_revision"`
	BindingSHA256   string           `json:"binding_sha256"`
	Source          NativeCheckpoint `json:"source"`
}

// NativeImportBatchScopeDigest validates before bounded canonical encoding.
// The scope hash never depends on mutable batch progress or relative dates now.
func NativeImportBatchScopeDigest(ctx context.Context, scope NativeImportBatchScope) (string, error) {
	if err := scope.validate(); err != nil {
		return "", err
	}
	prospective := NativeImportBatchConfirmation{Contract: "muse-historical-import-v1", Instance: strings.Repeat("0", 32), ScopeSHA256: strings.Repeat("0", 64), Scope: scope}
	if _, err := agentmeta.JSONWireBound(ctx, prospective, MaxNativeImportBatchBytes); err != nil {
		return "", errors.Join(ErrUnsupportedNativeImport, err)
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte("agent-archive/muse-historical-import-scope/v1\x00"))
	_, _ = h.Write(raw)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Validate checks complete immutable intent, not an externally supplied proof.
func (n *NativeImportBatchConfirmation) Validate(ctx context.Context) error {
	if n == nil || n.Contract != "muse-historical-import-v1" || !admissionHex(n.Instance, 32) || !admissionHex(n.ScopeSHA256, 64) {
		return ErrUnsupportedNativeImport
	}
	digest, err := NativeImportBatchScopeDigest(ctx, n.Scope)
	if err != nil || digest != n.ScopeSHA256 {
		return errors.Join(ErrUnsupportedNativeImport, err)
	}
	return nil
}

func nativeImportUTCTime(value string, optional bool) (time.Time, error) {
	if optional && value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.IsZero() || parsed.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, ErrUnsupportedNativeImport
	}
	return parsed, nil
}

func (s NativeImportBatchScope) validate() error {
	if !ValidImportBatchID(s.BatchID) || !nativeImportText(s.DestinationID, 4096) || !nativeImportOrderedStrings(s.ProjectIDs, false) || len(s.Selected) == 0 || len(s.Selected) > MaxNativeImportBatchSelections {
		return ErrUnsupportedNativeImport
	}
	started, err := nativeImportUTCTime(s.StartedAtUTC, false)
	if err != nil {
		return err
	}
	since, err := nativeImportUTCTime(s.SinceUTC, true)
	if err != nil {
		return err
	}
	until, err := nativeImportUTCTime(s.UntilExclusiveUTC, true)
	if err != nil || !since.IsZero() && !until.IsZero() && !since.Before(until) {
		return ErrUnsupportedNativeImport
	}
	if err := nativeImportBatchFiltersValid(s.Filters); err != nil {
		return err
	}
	previous := ""
	for _, selected := range s.Selected {
		birth, err := nativeImportUTCTime(selected.OriginalTUTC, false)
		if err != nil || birth.After(started) || !since.IsZero() && birth.Before(since) || !until.IsZero() && !birth.Before(until) || selected.validate() != nil || selected.NativeID <= previous || !slices.Contains(s.ProjectIDs, selected.ProjectID) {
			return ErrUnsupportedNativeImport
		}
		previous = selected.NativeID
	}
	return nil
}

func nativeImportOrderedStrings(values []string, empty bool) bool {
	if values == nil || !empty && len(values) == 0 || len(values) > MaxNativeImportBatchSelections {
		return false
	}
	previous := ""
	for _, value := range values {
		if !nativeImportText(value, 4096) || value <= previous {
			return false
		}
		previous = value
	}
	return true
}

func nativeImportBatchFiltersValid(f ImportBatchFilters) error {
	if !nativeImportOrderedStrings(f.Harnesses, true) || !nativeImportOrderedStrings(f.ProjectIDs, true) || f.MappingDigest != "" && !admissionHex(f.MappingDigest, 64) {
		return ErrUnsupportedNativeImport
	}
	for _, value := range []string{f.Since, f.Until, f.SinceArg, f.UntilArg} {
		if value != "" && !nativeImportText(value, 128) {
			return ErrUnsupportedNativeImport
		}
	}
	return nil
}

func (s NativeImportBatchSelection) validate() error {
	if s.Kind != "ordinary-root" || !nativeIdentity("muse", s.NativeID) || !nativeImportText(s.ProjectID, 4096) || !admissionHex(s.RootFingerprint, 64) || !nativeToken(s.RootRevision) || !admissionHex(s.BindingSHA256, 64) || s.Source.Validate() != nil {
		return ErrUnsupportedNativeImport
	}
	return nil
}

// UnmarshalJSON is closed and bounded without adding a custom value marshaler.
func (n *NativeImportBatchConfirmation) UnmarshalJSON(raw []byte) error {
	if err := nativeImportClosedObject(raw, MaxNativeImportBatchBytes, []string{"contract", "instance", "scope_sha256", "scope"}, nil); err != nil {
		return err
	}
	type plain NativeImportBatchConfirmation
	var next plain
	if err := json.Unmarshal(raw, &next); err != nil {
		return errors.Join(ErrUnsupportedNativeImport, err)
	}
	// JSON decoding has no caller context. Validation is bounded to the fixed
	// subsection limit; state callers additionally validate with their context.
	value := NativeImportBatchConfirmation(next)
	if err := value.Validate(context.Background()); err != nil {
		return err
	}
	*n = value
	return nil
}

// UnmarshalJSON preserves exact resolved scope and original filter wire fields.
func (s *NativeImportBatchScope) UnmarshalJSON(raw []byte) error {
	if err := nativeImportClosedObject(raw, MaxNativeImportBatchBytes, []string{"batch_id", "started_at_utc", "destination_id", "filters", "since_utc", "until_exclusive_utc", "project_ids", "selected"}, nil); err != nil {
		return err
	}
	type plain NativeImportBatchScope
	var next plain
	if err := json.Unmarshal(raw, &next); err != nil {
		return errors.Join(ErrUnsupportedNativeImport, err)
	}
	var fields struct {
		Filters json.RawMessage `json:"filters"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if err := nativeImportClosedObject(fields.Filters, MaxNativeImportBatchBytes, []string{"harnesses", "project_ids", "include_home", "include_temp", "include_removed"}, []string{"mapping_digest", "since", "until", "since_arg", "until_arg"}); err != nil {
		return err
	}
	value := NativeImportBatchScope(next)
	if err := value.validate(); err != nil {
		return err
	}
	*s = value
	return nil
}

// UnmarshalJSON refuses future source kinds and selection fields.
func (s *NativeImportBatchSelection) UnmarshalJSON(raw []byte) error {
	if err := nativeImportClosedObject(raw, MaxNativeImportBatchBytes, []string{"kind", "native_id", "original_t_utc", "project_id", "root_fingerprint", "root_revision", "binding_sha256", "source"}, nil); err != nil {
		return err
	}
	type plain NativeImportBatchSelection
	var next plain
	if err := json.Unmarshal(raw, &next); err != nil {
		return errors.Join(ErrUnsupportedNativeImport, err)
	}
	value := NativeImportBatchSelection(next)
	if value.validate() != nil {
		return ErrUnsupportedNativeImport
	}
	if _, err := nativeImportUTCTime(value.OriginalTUTC, false); err != nil {
		return err
	}
	*s = value
	return nil
}

// ValidImportBatchID is the existing import control filename grammar.
func ValidImportBatchID(id string) bool { return importBatchIDPattern.MatchString(id) }

var importBatchIDPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}-[1-9][0-9]{0,5}$`)
