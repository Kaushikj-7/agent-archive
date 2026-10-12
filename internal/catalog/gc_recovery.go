package catalog

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/wangjohn/agent-archive/internal/storage"
)

type gcCoordinatorWitness struct {
	Generation      uint64 `json:"Generation"`
	Seal            string `json:"Seal"`
	Hold            string `json:"Hold"`
	InventorySHA256 string `json:"InventorySHA256"`
}

type gcCompletion struct {
	Owner          string `json:"owner"`
	ReleasedSHA256 string `json:"released_sha256"`
}

type gcPhase string

const (
	gcPrepared  gcPhase = "prepared"
	gcReleasing gcPhase = "releasing"
)

type gcLink struct {
	Owner           string               `json:"owner"`
	Witness         gcCoordinatorWitness `json:"witness"`
	StateGeneration uint64               `json:"state_generation"`
	Inventory       []ObjectRef          `json:"inventory"`
	PriorETag       string               `json:"prior_etag"`
	PriorSHA256     string               `json:"prior_sha256"`
	LeasedSHA256    string               `json:"leased_sha256"`
	ReleasedSHA256  string               `json:"released_sha256,omitempty"`
	ReleasedHead    []byte               `json:"released_head,omitempty"`
	Phase           gcPhase              `json:"phase"`
}

func headHash(h CatalogHead) string { raw, _ := json.Marshal(h); return storage.SHA256Hex(raw) }

func inventoryHash(refs []ObjectRef) string {
	canonical := make([]ObjectRef, len(refs))
	copy(canonical, refs)
	raw, _ := json.Marshal(canonical)
	return storage.SHA256Hex(raw)
}

func (w *Writer) bindGCLease(ctx context.Context, _ CatalogHead, etag string, leased *CatalogHead, inventory []ObjectRef) error {
	_, actualETag, priorHash, err := w.observeGCHead(ctx)
	if err != nil {
		return err
	}
	if actualETag != etag {
		return ErrConflict
	}
	return w.Coordinator().change(ctx, func(state *admissions) error {
		if state.Seal == "" || state.Hold == "" || len(state.Owners) != 0 || state.GCLink != nil {
			return ErrAdmissionClosed
		}
		witness := gcCoordinatorWitness{state.Generation, state.Seal, state.Hold, inventoryHash(inventory)}
		leased.GCCoordinator = witness
		state.GCLink = &gcLink{Owner: leased.GCLease, Witness: witness, StateGeneration: state.Generation + 1, Inventory: append([]ObjectRef(nil), inventory...), PriorETag: etag, PriorSHA256: priorHash, LeasedSHA256: headHash(*leased), Phase: gcPrepared}
		return nil
	})
}

func (w *Writer) gcState(ctx context.Context, owner string) (admissions, error) {
	state, _, err := w.Coordinator().read(ctx)
	if err != nil {
		return state, err
	}
	link := state.GCLink
	if link == nil || link.Owner != owner || state.Generation != link.StateGeneration || state.Seal != link.Witness.Seal || state.Hold != link.Witness.Hold || len(state.Owners) != 0 || inventoryHash(link.Inventory) != link.Witness.InventorySHA256 {
		return state, ErrConflict
	}
	return state, nil
}

func (w *Writer) verifyGCSweep(ctx context.Context, h CatalogHead, etag string) error {
	state, err := w.gcState(ctx, h.GCLease)
	if err != nil {
		return err
	}
	if state.GCLink.Witness != h.GCCoordinator || state.GCLink.LeasedSHA256 != headHash(h) {
		return ErrConflict
	}
	fresh, version, err := w.Head(ctx)
	if err != nil {
		return err
	}
	if version != etag || headHash(fresh) != state.GCLink.LeasedSHA256 {
		return ErrConflict
	}
	return nil
}

func (w *Writer) bindGCRelease(ctx context.Context, h CatalogHead, owner string) ([]byte, error) {
	var raw []byte
	err := w.Coordinator().change(ctx, func(state *admissions) error {
		link := state.GCLink
		if link == nil || link.Owner != owner || state.Generation != link.StateGeneration || state.Seal != link.Witness.Seal || state.Hold != link.Witness.Hold || len(state.Owners) != 0 {
			return ErrConflict
		}
		if link.ReleasedSHA256 != "" {
			link.StateGeneration = state.Generation + 1
			raw = append([]byte(nil), link.ReleasedHead...)
			return validateReleasedHead(h, raw)
		}
		h.GCLease = ""
		h.GCCoordinator = gcCoordinatorWitness{}
		id, err := NewMutationID()
		if err != nil {
			return err
		}
		h.Epoch = id
		h.Generation++
		raw, err = json.Marshal(h)
		if err != nil {
			return err
		}
		link.ReleasedHead = raw
		link.ReleasedSHA256 = storage.SHA256Hex(raw)
		link.Phase = gcReleasing
		link.StateGeneration = state.Generation + 1
		return nil
	})
	return raw, err
}

func (w *Writer) finishGCRelease(ctx context.Context, owner string) error {
	state, err := w.gcState(ctx, owner)
	if err != nil {
		return err
	}
	_, _, hash, err := w.observeGCHead(ctx)
	if err != nil {
		return err
	}
	if state.GCLink.ReleasedSHA256 == "" || hash != state.GCLink.ReleasedSHA256 {
		return ErrConflict
	}
	return w.Coordinator().change(ctx, func(current *admissions) error {
		if current.Generation != state.Generation || current.GCLink == nil || current.GCLink.Owner != owner || current.Hold != state.Hold || current.Seal != state.Seal || len(current.Owners) != 0 {
			return ErrConflict
		}
		current.GCReceipt = &gcCompletion{owner, state.GCLink.ReleasedSHA256}
		current.GCLink = nil
		current.Hold = ""
		current.Seal = ""
		return nil
	})
}

type recoveryCoordinator struct {
	writer     *Writer
	owner      string
	generation uint64
	external   Barrier
}

func (b recoveryCoordinator) Hold(ctx context.Context) ([]ObjectRef, func(), error) {
	state, err := b.writer.gcState(ctx, b.owner)
	if err != nil {
		return nil, nil, err
	}
	if state.Generation != b.generation {
		return nil, nil, ErrConflict
	}
	if b.external != nil {
		refs, release, err := b.external.Hold(ctx)
		if err != nil {
			return nil, nil, err
		}
		if release == nil {
			return nil, nil, ErrAdmissionClosed
		}
		if inventoryHash(refs) != inventoryHash(state.GCLink.Inventory) {
			release()
			return nil, nil, ErrConflict
		}
		return refs, release, nil
	}
	return append([]ObjectRef(nil), state.GCLink.Inventory...), func() {}, nil
}

// CatalogRecoveryBarrier only proves the exact recorded crashed GC ownership.
// It grants no collection or new writer admission and never expires into access.
func (s *Store) CatalogRecoveryBarrier(ctx context.Context, owner string) (Barrier, error) {
	state, err := s.Writer.gcState(ctx, owner)
	if err != nil {
		if completed := s.Writer.completedGC(ctx, owner); completed == nil {
			return completedCoordinator{s.Writer, owner}, nil
		}
		return nil, err
	}
	var external Barrier
	if provider, ok := s.ObjectStore.(interface {
		CatalogBarrier(context.Context) (Barrier, error)
	}); ok {
		external, err = provider.CatalogBarrier(ctx)
		if err != nil {
			return nil, err
		}
		if external == nil {
			return nil, ErrAdmissionClosed
		}
	}
	return recoveryCoordinator{writer: s.Writer, owner: owner, generation: state.Generation, external: external}, nil
}

func (w *Writer) recoverLinkedGC(ctx context.Context, owner string) error {
	if w.readOnly {
		return ErrReadOnly
	}
	state, err := w.gcState(ctx, owner)
	if err != nil {
		if completed := w.completedGC(ctx, owner); completed == nil {
			return nil
		}
		return err
	}
	link := state.GCLink
	if err = w.markProtected(ctx, link.Inventory, map[string]bool{}); err != nil {
		return err
	}
	h, etag, hash, err := w.observeGCHead(ctx)
	if err != nil {
		return err
	}
	if link.ReleasedSHA256 != "" && hash == link.ReleasedSHA256 {
		return w.finishGCRelease(ctx, owner)
	}
	if hash == link.LeasedSHA256 && (h.GCLease != owner || h.GCCoordinator != link.Witness) {
		return ErrConflict
	}
	if hash != link.LeasedSHA256 && (hash != link.PriorSHA256 || etag != link.PriorETag) {
		return ErrConflict
	}
	raw, err := w.bindGCRelease(ctx, h, owner)
	if err != nil {
		return err
	}
	_, err = w.conditional.PutConditional(ctx, HeadKey, raw, storage.PutCondition{MatchETag: etag})
	if err != nil {
		return errors.Join(ErrCommitUnknown, err)
	}
	return w.finishGCRelease(ctx, owner)
}

// RecoverUnleasedSeal requires explicit owner and generation for a drained seal
// that never acquired a hold or GC link. It cannot discard unfinished owners.
func (c *Coordinator) RecoverUnleasedSeal(ctx context.Context, owner string, generation uint64) error {
	return c.change(ctx, func(state *admissions) error {
		if owner == "" || state.Seal != owner || state.Generation != generation || state.Hold != "" || state.GCLink != nil || len(state.Owners) != 0 {
			return ErrConflict
		}
		state.Seal = ""
		return nil
	})
}

// Observe exact raw bytes and same-version validator as well as the validated
// publication-clock projection. A changing observation fails closed.
func (w *Writer) observeGCHead(ctx context.Context) (CatalogHead, string, string, error) {
	h, etag, err := w.Head(ctx)
	if err != nil {
		return h, "", "", err
	}
	raw, version, err := w.versioned.GetCatalogVersion(ctx, HeadKey, 16<<10)
	if err != nil {
		return h, "", "", err
	}
	if version.ETag != etag {
		return h, "", "", ErrConflict
	}
	return h, etag, storage.SHA256Hex(raw), nil
}

type completedCoordinator struct {
	writer *Writer
	owner  string
}

func (b completedCoordinator) Hold(ctx context.Context) ([]ObjectRef, func(), error) {
	if err := b.writer.completedGC(ctx, b.owner); err != nil {
		return nil, nil, err
	}
	return nil, func() {}, nil
}

func (w *Writer) completedGC(ctx context.Context, owner string) error {
	state, _, err := w.Coordinator().read(ctx)
	if err != nil {
		return err
	}
	if state.GCLink != nil || state.GCReceipt == nil || owner == "" || state.GCReceipt.Owner != owner || state.Seal != "" || state.Hold != "" || len(state.Owners) != 0 {
		return ErrConflict
	}
	_, _, hash, err := w.observeGCHead(ctx)
	if err != nil {
		return err
	}
	if hash != state.GCReceipt.ReleasedSHA256 {
		return ErrConflict
	}
	return nil
}

func validateReleasedHead(base CatalogHead, raw []byte) error {
	var released CatalogHead
	if json.Unmarshal(raw, &released) != nil || released.Generation < base.Generation || released.Generation > base.Generation+1 || released.Epoch == base.Epoch {
		return ErrConflict
	}
	base.GCLease = ""
	base.GCCoordinator = gcCoordinatorWitness{}
	base.Epoch = released.Epoch
	base.Generation = released.Generation
	if headHash(base) != storage.SHA256Hex(raw) {
		return ErrConflict
	}
	return nil
}
