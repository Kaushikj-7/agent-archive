package archive

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"
)

// MaxImportBatchControlBytes is the existing import-control reader limit.
const MaxImportBatchControlBytes = 4 << 20

// NativeImportBatchControl is the state-owned view of an existing backfill
// control. It is not a registration capability. Batch progress stays backfill's.
type NativeImportBatchControl struct {
	ID            string                         `json:"id"`
	StartedAt     time.Time                      `json:"started_at"`
	CompletedAt   *time.Time                     `json:"completed_at,omitempty"`
	Filters       ImportBatchFilters             `json:"filters"`
	DestinationID string                         `json:"destination_id"`
	UndoneAt      *time.Time                     `json:"undone_at,omitempty"`
	NativeImport  *NativeImportBatchConfirmation `json:"native_import,omitempty"`
}

// DecodeNativeImportBatchControl classifies the private selector before the
// generic JSON syntax pass, so malformed/future authority is never discarded.
// Controls without the selector keep their ordinary decoder semantics.
func DecodeNativeImportBatchControl(raw []byte, next *NativeImportBatchControl) error {
	present, err := nativeImportRegistrationSelector(raw)
	if err != nil {
		if present || nativeImportKeyPossible(raw) {
			return errors.Join(ErrUnsupportedNativeImport, err)
		}
		return err
	}
	if nativeImportKeyPossible(raw) {
		present = true
	}
	if present {
		if err := nativeImportClosedObjectNulls(raw, MaxImportBatchControlBytes, []string{"id", "started_at", "filters", "destination_id", "native_import"}, []string{"completed_at", "projects_added", "apps_added", "projects_kept_out", "sessions", "staged_sessions", "subagents", "retention", "undone_at", "projects_excluded", "projects_kept", "projects_kept_for"}, []string{"projects_added", "apps_added", "sessions", "subagents"}); err != nil {
			return err
		}
	}
	var value NativeImportBatchControl
	if err := json.Unmarshal(raw, &value); err != nil {
		if present {
			return errors.Join(ErrUnsupportedNativeImport, err)
		}
		return err
	}
	if present {
		if err := value.Validate(context.Background()); err != nil {
			return err
		}
	}
	*next = value
	return nil
}

// Validate binds the closed section to its immutable enclosing batch header.
func (b NativeImportBatchControl) Validate(ctx context.Context) error {
	if b.NativeImport == nil || b.NativeImport.Validate(ctx) != nil || !ValidImportBatchID(b.ID) {
		return ErrUnsupportedNativeImport
	}
	scope := b.NativeImport.Scope
	if scope.BatchID != b.ID || scope.StartedAtUTC != b.StartedAt.UTC().Format(time.RFC3339Nano) || scope.DestinationID != b.DestinationID || !reflect.DeepEqual(scope.Filters, b.Filters) {
		return ErrUnsupportedNativeImport
	}
	if b.UndoneAt != nil && b.UndoneAt.IsZero() || b.CompletedAt != nil && b.CompletedAt.IsZero() {
		return ErrUnsupportedNativeImport
	}
	return nil
}

// MatchesRegistration checks current pre-ACK confirmation, never retained
// maintenance. A closed receipt cannot substitute a different selected source.
func (b NativeImportBatchControl) MatchesRegistration(ctx context.Context, reg SessionRegistration) error {
	if err := b.Validate(ctx); err != nil {
		return err
	}
	if reg.ValidateNativeImport() != nil || b.UndoneAt != nil {
		return ErrUnsupportedNativeImport
	}
	c := reg.NativeImport.Confirmation
	if b.ID != c.BatchID || b.NativeImport.Instance != c.BatchInstance || b.NativeImport.ScopeSHA256 != c.ScopeSHA256 || b.DestinationID != reg.DestinationID {
		return ErrUnsupportedNativeImport
	}
	for _, selected := range b.NativeImport.Scope.Selected {
		if selected.NativeID != reg.NativeSessionID {
			continue
		}
		if selected.ProjectID == reg.ProjectID && selected.OriginalTUTC == reg.SessionStartedAt.UTC().Format(time.RFC3339Nano) && selected.BindingSHA256 == c.SelectedBindingSHA256 && selected.Source == c.SelectedSource && selected.RootFingerprint == c.RootFingerprint && selected.RootRevision == reg.NativeBinding.RootRevision {
			return nil
		}
		return ErrUnsupportedNativeImport
	}
	return ErrUnsupportedNativeImport
}
