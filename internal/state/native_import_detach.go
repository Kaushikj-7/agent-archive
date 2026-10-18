package state

import (
	"context"
	"errors"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

type nativeImportDetachProof struct {
	store         *Store
	batchID       string
	receiptID     string
	controlDigest string
}

// DetachNativeImportBatchMembership removes only the creating batch's logical
// registration membership after its actual durable undo marker exists. It never
// deletes an owner, request, retained stage/publication, native file or config.
func (s *Store) DetachNativeImportBatchMembership(ctx context.Context, batchID, id, receiptID string) (detached bool, err error) {
	if s.hook || !archive.ValidImportBatchID(batchID) || !nativeReservationID(id) || !nativeReservationID(receiptID) {
		return false, archive.ErrUnsupportedNativeImport
	}
	bound, ctx, closeContext := s.withDurableContext(ctx)
	defer closeContext()
	read, closeRead := bound.WithImportResultScope(ctx)
	defer closeRead()
	held, err := s.prepareNativeImportLocks(ctx, batchID, id)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, held.Close()) }()
	var control archive.NativeImportBatchControl
	err = WithImportBatchLock(ctx, s.home, batchID, func(locked *local.RootedHome) error {
		var err error
		control, err = read.readNativeImportControl(ctx, locked, batchID)
		if err != nil {
			return err
		}
		if control.UndoneAt == nil {
			return archive.ErrUnsupportedNativeImport
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	digest, err := read.readyDigest(ctx, control)
	if err != nil {
		return false, err
	}
	proof := &nativeImportDetachProof{store: read, batchID: batchID, receiptID: receiptID, controlDigest: digest}
	cap := &nativeImportOwnerCommit{ctx: ctx, store: s, detach: proof}
	var selected archive.SessionRegistration
	err = read.writeUnderLock(lockedWrite{nativeImport: cap, path: s.registrationPath(id), lock: func() (func(), error) { return s.lockNativeImport(ctx, held, batchID, id) }, check: func() error {
		current, err := read.readNativeImportControl(ctx, held, batchID)
		if err != nil {
			return err
		}
		actual, err := read.readyDigest(ctx, current)
		if err != nil || actual != proof.controlDigest || current.UndoneAt == nil {
			return errors.Join(ErrReadySelectionFrozen, err)
		}
		if selected.NativeImport == nil {
			return archive.ErrUnsupportedNativeImport
		}
		if err := nativeImportUndoControlMatches(ctx, current, selected, receiptID); err != nil {
			return err
		}
		return errors.Join(ctx.Err(), held.Check())
	}, change: func(current fileSnapshot) (any, bool, error) {
		detached = false
		if !current.found {
			return nil, false, ErrSessionNotRegistered
		}
		var previous archive.SessionRegistration
		if err := archive.DecodeSessionRegistration(current.data, &previous); err != nil {
			return nil, false, err
		}
		if err := nativeImportUndoControlMatches(ctx, control, previous, receiptID); err != nil {
			return nil, false, err
		}
		selected = previous
		if previous.NativeImport.Membership.State == archive.NativeImportDetached {
			return nil, false, nil
		}
		receipt := *previous.NativeImport
		receipt.Membership.State = archive.NativeImportDetached
		selected.NativeImport = &receipt
		if err := proof.check(ctx, previous, selected, true); err != nil {
			return nil, false, err
		}
		detached = true
		return selected, true, nil
	}})
	if err != nil {
		return false, err
	}
	return detached, nil
}

func (p *nativeImportDetachProof) check(ctx context.Context, previous, next archive.SessionRegistration, found bool) error {
	if p == nil || p.store == nil || !found || previous.ValidateNativeImport() != nil || next.ValidateNativeImport() != nil || previous.NativeImport.Confirmation.BatchID != p.batchID || previous.NativeImport.Membership.ReceiptID != p.receiptID || previous.NativeImport.Membership.State != archive.NativeImportAttached || next.NativeImport.Membership.State != archive.NativeImportDetached {
		return archive.ErrUnsupportedNativeImport
	}
	normalized := next
	receipt := *next.NativeImport
	receipt.Membership.State = archive.NativeImportAttached
	normalized.NativeImport = &receipt
	before, err := p.store.readyDigest(ctx, previous)
	if err != nil {
		return err
	}
	after, err := p.store.readyDigest(ctx, normalized)
	if err != nil || before != after {
		return errors.Join(archive.ErrUnsupportedNativeImport, err)
	}
	return ctx.Err()
}

func nativeImportUndoControlMatches(ctx context.Context, control archive.NativeImportBatchControl, reg archive.SessionRegistration, receiptID string) error {
	if control.UndoneAt == nil || reg.ValidateNativeImport() != nil || reg.NativeImport.Membership.ReceiptID != receiptID {
		return archive.ErrUnsupportedNativeImport
	}
	// Closed normalization is for identity comparison only, never reattachment.
	normalized := reg
	receipt := *reg.NativeImport
	receipt.Membership.State = archive.NativeImportAttached
	normalized.NativeImport = &receipt
	control.UndoneAt = nil
	return control.MatchesRegistration(ctx, normalized)
}

// Original frozen stage identity survives only the closed membership detach
// transition. Generic registration CAS still compares exact receipt bytes.
func sameNativeImportStageReceipt(a, b *archive.NativeImport) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	left, err := archive.NativeImportIdentityDigest(a)
	if err != nil {
		return false
	}
	right, err := archive.NativeImportIdentityDigest(b)
	return err == nil && left == right
}
