package state

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// CheckNativeImportConfirmationScope checks current eligibility only. It grants
// no source, reservation or first-owner permission, and never opens native input.
func (s *Store) CheckNativeImportConfirmationScope(ctx context.Context, scope archive.NativeImportBatchScope) error {
	if _, err := archive.NativeImportBatchScopeDigest(ctx, scope); err != nil {
		return err
	}
	cfg, found, err := s.loadConfig()
	if err != nil || !found || !cfg.NativeImportProtection || !cfg.NativeAuthorityProtection || !cfg.SupportsStagedSelection() || !cfg.Archive.Enabled || cfg.Paused || !slices.Contains(cfg.ImportedHarnesses, "muse") || cfg.DestinationID() != scope.DestinationID {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	if err := nativeImportScopeProjects(cfg, scope, time.Now().UTC()); err != nil {
		return err
	}
	for _, selected := range scope.Selected {
		matched := false
		for _, root := range cfg.NativeRoots {
			if root.Agent != "muse" || root.Revision != selected.RootRevision {
				continue
			}
			fingerprint, err := archive.NativeRootFingerprint(root)
			if err != nil {
				return err
			}
			matched = matched || fingerprint == selected.RootFingerprint
		}
		if !matched {
			return ErrReadySelectionFrozen
		}
	}
	return ctx.Err()
}

func nativeImportScopeProjects(cfg config.Config, scope archive.NativeImportBatchScope, at time.Time) error {
	for _, id := range scope.ProjectIDs {
		matched := false
		for _, project := range cfg.Archive.Projects {
			if !project.Included || archive.ProjectID(project.Root) != id {
				continue
			}
			// Admission is now, not the historical native birth. The physical
			// project must still be proved independently by the source factory.
			candidate := archive.SessionRegistration{Harness: archive.Harness{Name: "muse"}, Origin: archive.SessionOriginImport, ProjectRoot: project.Root, ProjectID: id, DestinationID: scope.DestinationID, AdmittedAt: at}
			matched = cfg.AcceptSession(candidate)
			break
		}
		if !matched {
			return ErrReadySelectionFrozen
		}
	}
	return nil
}

// ConfirmedNativeImportBatch retains actual closed reviewed intent. Its private
// Store binding and lifetime cannot be supplied by a caller-made confirmation.
// The handle itself grants no source or registration authority.
type ConfirmedNativeImportBatch struct {
	mu        sync.Mutex
	store     *Store
	self      *ConfirmedNativeImportBatch
	read      *Store
	control   archive.NativeImportBatchControl
	identity  string
	closeRead func()
	users     int
	closed    bool
}

// OpenConfirmedNativeImportBatch opens an existing actual, unfinished reviewed
// batch under its batch lock, charging retained data to the existing read ledger.
func (s *Store) OpenConfirmedNativeImportBatch(ctx context.Context, id string) (*ConfirmedNativeImportBatch, error) {
	if s.hook || !archive.ValidImportBatchID(id) {
		return nil, archive.ErrUnsupportedNativeImport
	}
	read, closeRead := s.WithImportResultScope(ctx)
	h := &ConfirmedNativeImportBatch{store: s, read: read, closeRead: closeRead}
	err := WithImportBatchLock(ctx, s.home, id, func(held *local.RootedHome) error {
		control, err := read.readConfirmedNativeImportControl(ctx, held, id)
		if err != nil {
			return err
		}
		if err := read.CheckNativeImportConfirmationScope(ctx, control.NativeImport.Scope); err != nil {
			return err
		}
		identity, err := read.nativeImportConfirmationIdentity(ctx, control)
		if err != nil {
			return err
		}
		h.control, h.identity = control, identity
		return nil
	})
	if err != nil {
		closeRead()
		return nil, err
	}
	h.self = h
	return h, nil
}

func (s *Store) readConfirmedNativeImportControl(ctx context.Context, held *local.RootedHome, id string) (archive.NativeImportBatchControl, error) {
	var control archive.NativeImportBatchControl
	info, err := held.Root.Lstat(filepath.Join("imports", id+".json"))
	if err != nil || info.Size() < 1 || info.Size() > archive.MaxImportBatchControlBytes {
		return control, errors.Join(ErrReadySelectionFrozen, err)
	}
	reservation := s.readReservation()
	n := 3 * info.Size()
	if !reservation.Reserve(n) {
		return control, errStateBudget
	}
	keep := false
	defer func() {
		if !keep {
			reservation.Release(n)
		}
	}()
	raw, err := readImportBatchBytesBounded(held, id, info.Size())
	if err != nil {
		return control, err
	}
	if err := archive.DecodeNativeImportBatchControl(raw, &control); err != nil {
		return control, err
	}
	if err := control.Validate(ctx); err != nil {
		return control, err
	}
	if control.ID != id || control.CompletedAt != nil || control.UndoneAt != nil {
		return control, ErrReadySelectionFrozen
	}
	*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(n) })
	keep = true
	return control, ctx.Err()
}

func (s *Store) nativeImportConfirmationIdentity(ctx context.Context, control archive.NativeImportBatchControl) (string, error) {
	// Ordinary progress and completion timestamps are not part of immutable
	// intent. A completed or undone current control is rejected separately.
	return s.readyDigest(ctx, struct {
		ID            string                                 `json:"id"`
		StartedAt     time.Time                              `json:"started_at"`
		Filters       archive.ImportBatchFilters             `json:"filters"`
		DestinationID string                                 `json:"destination_id"`
		Confirmation  *archive.NativeImportBatchConfirmation `json:"native_import"`
	}{control.ID, control.StartedAt, control.Filters, control.DestinationID, control.NativeImport})
}

func (h *ConfirmedNativeImportBatch) borrow(store *Store) (func(), error) {
	if h == nil {
		return nil, ErrReadySelectionFrozen
	}
	h.mu.Lock()
	if h.closed || h.self != h || h.store != store || h.closeRead == nil {
		h.mu.Unlock()
		return nil, ErrReadySelectionFrozen
	}
	h.users++
	h.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			h.users--
			closeRead := h.finishCloseLocked()
			h.mu.Unlock()
			if closeRead != nil {
				closeRead()
			}
		})
	}, nil
}

func (h *ConfirmedNativeImportBatch) finishCloseLocked() func() {
	if !h.closed || h.users != 0 {
		return nil
	}
	closeRead := h.closeRead
	h.closeRead = nil
	return closeRead
}

// Close expires the handle immediately. In-flight checks retain their charged
// input until they return; repeated Close calls are harmless.
func (h *ConfirmedNativeImportBatch) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	if h.self != h {
		h.mu.Unlock()
		return ErrReadySelectionFrozen
	}
	h.closed = true
	closeRead := h.finishCloseLocked()
	h.mu.Unlock()
	if closeRead != nil {
		closeRead()
	}
	return nil
}

func (h *ConfirmedNativeImportBatch) checkCurrent(ctx context.Context, store *Store) error {
	release, err := h.borrow(store)
	if err != nil {
		return err
	}
	defer release()
	read, closeRead := h.read.WithReadBudget(ctx, h.read.resourceBudget)
	defer closeRead()
	return WithImportBatchLock(ctx, store.home, h.control.ID, func(held *local.RootedHome) error {
		current, err := read.readConfirmedNativeImportControl(ctx, held, h.control.ID)
		if err != nil {
			return err
		}
		identity, err := read.nativeImportConfirmationIdentity(ctx, current)
		if err != nil || identity != h.identity {
			return errors.Join(ErrReadySelectionFrozen, err)
		}
		if err := read.CheckNativeImportConfirmationScope(ctx, current.NativeImport.Scope); err != nil {
			return err
		}
		return h.checkOpen(store)
	})
}

func (h *ConfirmedNativeImportBatch) checkOpen(store *Store) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.self != h || h.store != store || h.closeRead == nil {
		return ErrReadySelectionFrozen
	}
	return nil
}
