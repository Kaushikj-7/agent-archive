package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// DeleteOwnedSession freezes exact selecting authority and retains its complete
// source union before removing metadata. Catalog commits retain their own CAS
// tombstone and snapshot ownership; they never use legacy namespace cleanup.
func DeleteOwnedSession(ctx context.Context, local *state.Store, store storage.ObjectStore, reg archive.SessionRegistration, reason state.RemovalReason, now time.Time) error {
	if catalog, ok := store.(interface{ CatalogMetadataAuthority() bool }); ok && catalog.CatalogMetadataAuthority() {
		return DeleteWholeSession(ctx, store, reg.Harness.Name, reg.ArchiveSessionID)
	}
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		return err
	}
	budget := agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	scoped, closeScope := local.WithReadBudget(ctx, budget)
	defer closeScope()
	local = scoped
	raw, release, err := readDeletionMetadata(ctx, store, key, budget)
	defer release()
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	request, have, err := local.LoadRequest(reg.ArchiveSessionID)
	if err != nil {
		return err
	}
	token := ""
	if have {
		token = request.Token
	}
	j, found, err := local.LoadSessionDeletion(reg)
	if err != nil {
		return err
	}
	if !found || j.Phase == state.DeletionPreparing || (j.Phase == state.DeletionRestored || j.Phase == state.DeletionCancelled) || reason == state.RemovalReasonUndo && j.Reason == state.RemovalReasonRetention {
		j, err = local.PrepareSessionDeletion(reg, reason, raw, now, token)
		if err != nil {
			return err
		}
	}
	if j.Reason != reason || j.Phase == state.DeletionRestoring {
		return state.ErrRemovalPending
	}
	if len(raw) > 0 && !bytes.Equal(raw, j.MetadataBytes) {
		return storage.ErrPublicationConflict
	}
	if j.Phase == state.DeletionPrepared {
		j, err = prepareOwnedSources(ctx, local, store, j, key, budget)
		if err != nil {
			return err
		}
	}
	if j.Phase == state.DeletionDeleting {
		j, err = deleteOwnedMetadata(ctx, local, store, j, key, budget)
		if err != nil {
			return err
		}
	}
	return deleteOwnedNamespace(ctx, local, store, j, key, budget)
}

func prepareOwnedSources(ctx context.Context, local *state.Store, store storage.ObjectStore, j state.SessionDeletion, key string, budget *agentapi.NativeReadBudget) (state.SessionDeletion, error) {
	reason := j.Reason
	var err error
	for i, ref := range j.References {
		if err = retainDeletingSource(ctx, local, store, j, i, ref, budget); err != nil {
			return j, err
		}
	}
	// Native-independent restoration is possible only after every exact source
	// is durable. The phase boundary is saved before a metadata delete is sent.
	if err = verifyDeletionPosition(ctx, local, store, j, key, true, budget); err != nil {
		return j, err
	}
	if changed, e := local.DeletionRequestChanged(j); e != nil || changed && reason == state.RemovalReasonRetention {
		return j, errors.Join(state.ErrDeletionWorkChanged, e)
	}
	j, err = local.AdvanceSessionDeletion(j, state.DeletionDeleting)
	if err != nil {
		return j, err
	}

	return j, nil
}

func deleteOwnedMetadata(ctx context.Context, local *state.Store, store storage.ObjectStore, j state.SessionDeletion, key string, budget *agentapi.NativeReadBudget) (state.SessionDeletion, error) {
	var err error

	if err = verifyDeletionPosition(ctx, local, store, j, key, false, budget); err != nil {
		return j, err
	}
	if err = store.Delete(ctx, key); err != nil {
		return j, fmt.Errorf("delete exact selecting metadata: %w", err)
	}
	fresh, release, e := readDeletionMetadata(ctx, store, key, budget)
	defer release()
	if !errors.Is(e, storage.ErrNotFound) {
		if e == nil && len(fresh) > 0 {
			return j, storage.ErrPublicationConflict
		}
		return j, e
	}
	j, err = local.AdvanceSessionDeletion(j, state.DeletionAbsent)
	if err != nil {
		return j, err
	}

	return j, nil
}

func deleteOwnedNamespace(ctx context.Context, local *state.Store, store storage.ObjectStore, j state.SessionDeletion, key string, budget *agentapi.NativeReadBudget) error {
	reg, reason := j.Registration, j.Reason
	if j.Phase == state.DeletionCleaned {
		return nil
	}
	if j.Phase != state.DeletionAbsent {
		return state.ErrRemovalPending
	}
	if changed, err := local.DeletionRequestChanged(j); err != nil || changed && reason == state.RemovalReasonRetention {
		return errors.Join(state.ErrDeletionWorkChanged, err)
	}
	objects, err := store.List(ctx, "sessions/"+reg.Harness.Name+"/"+reg.ArchiveSessionID+"/")
	if err != nil {
		return err
	}
	for _, object := range objects {
		if object.Key == key {
			return storage.ErrPublicationConflict
		}
		// A complete remote namespace is removed only while the exact intent and
		// absent selector remain current. New work stops before the next object.
		if err = verifyDeletionPosition(ctx, local, store, j, key, false, budget); err != nil {
			return err
		}
		if changed, e := local.DeletionRequestChanged(j); e != nil || changed && reason == state.RemovalReasonRetention {
			return errors.Join(state.ErrDeletionWorkChanged, e)
		}
		if !strings.HasPrefix(object.Key, "sessions/"+reg.Harness.Name+"/"+reg.ArchiveSessionID+"/") {
			return storage.ErrPublicationConflict
		}
		if err = verifyOwnedDeletionObject(ctx, store, object, budget); err != nil {
			return err
		}
		if err = verifyDeletionPosition(ctx, local, store, j, key, false, budget); err != nil {
			return err
		}
		if err = store.Delete(ctx, object.Key); err != nil {
			return err
		}
	}
	if err = verifyDeletionPosition(ctx, local, store, j, key, false, budget); err != nil {
		return err
	}
	if err = listingindex.DeleteSession(ctx, store, reg.Harness.Name, reg.ArchiveSessionID); err != nil {
		return err
	}
	_, err = local.AdvanceSessionDeletion(j, state.DeletionCleaned)
	return err
}

func verifyDeletionPosition(ctx context.Context, local *state.Store, store storage.ObjectStore, j state.SessionDeletion, key string, requirePresent bool, budget *agentapi.NativeReadBudget) error {
	scoped, closeScope := local.WithReadBudget(ctx, budget)
	defer closeScope()
	local = scoped
	if err := ctx.Err(); err != nil {
		return err
	}
	current, found, err := local.LoadSessionDeletion(j.Registration)
	if err != nil {
		return err
	}
	if !found || current.IntentSHA256() != j.IntentSHA256() || current.Phase != j.Phase {
		return state.ErrDurableStorageRecovery
	}
	if err := verifyRetainedDeletionSet(ctx, local, j, budget); err != nil {
		return err
	}
	raw, release, err := readDeletionMetadata(ctx, store, key, budget)
	defer release()
	if errors.Is(err, storage.ErrNotFound) {
		if requirePresent && len(j.MetadataBytes) > 0 {
			return storage.ErrPublicationConflict
		}
		return nil
	}
	if err != nil {
		return err
	}
	if len(j.MetadataBytes) == 0 || !bytes.Equal(raw, j.MetadataBytes) || j.Phase == state.DeletionAbsent || j.Phase == state.DeletionCleaned {
		return storage.ErrPublicationConflict
	}
	return nil
}

func retainDeletingSource(ctx context.Context, local *state.Store, store storage.ObjectStore, j state.SessionDeletion, index int, ref archive.SourceReference, budget *agentapi.NativeReadBudget) error {
	scoped, closeScope := local.WithReadBudget(ctx, budget)
	defer closeScope()
	reservation := budget.Reservation()
	if !reservation.Reserve(int64(ref.CompressedBytes)) {
		return agentapi.ReadBudgetLimit(errors.New("deletion source exceeds shared read budget"))
	}
	defer reservation.Release(int64(ref.CompressedBytes))
	getter, ok := store.(storage.LimitedGetter)
	if !ok {
		return storage.ErrVersionedReadUnavailable
	}
	data, err := getter.GetLimited(ctx, ref.Key, int64(ref.CompressedBytes))
	if err != nil {
		return err
	}
	if len(data) != ref.CompressedBytes || !storage.VerifySHA256(data, ref.SHA256) {
		return storage.ErrChecksumMismatch
	}
	var metadata archive.Metadata
	if err = json.Unmarshal(j.MetadataBytes, &metadata); err != nil {
		return err
	}
	var release func()
	if index == 0 {
		_, release, err = reader.DecodeReferencedSourceLeased(ctx, metadata, data, reader.Limits{}, budget)
	} else {
		_, release, err = reader.DecodeRevisionSourceLeased(ctx, metadata, metadata.History.Preserved[index-1].RevisionID, data, reader.Limits{}, budget)
	}
	if release != nil {
		defer release()
	}
	if err != nil {
		return err
	}
	return scoped.StageDeletionSource(j, ref, data)
}

func verifyRetainedDeletionSet(ctx context.Context, local *state.Store, j state.SessionDeletion, budget *agentapi.NativeReadBudget) error {
	for _, ref := range j.References {
		scoped, closeScope := local.WithReadBudget(ctx, budget)
		_, err := scoped.ReadDeletionSource(j.Registration.ArchiveSessionID, ref)
		closeScope()
		if err != nil {
			return err
		}
	}
	return nil
}

func verifyOwnedDeletionObject(ctx context.Context, store storage.ObjectStore, object storage.Object, budget *agentapi.NativeReadBudget) error {
	name := object.Key[strings.LastIndex(object.Key, "/")+1:]
	sha, ok := strings.CutPrefix(name, "source.")
	if !ok {
		return state.ErrDurableStorageRecovery
	}
	sha, ok = strings.CutSuffix(sha, ".jsonl.gz")
	if !ok || len(sha) != 64 || object.Size <= 0 || object.Size > 32<<20 {
		return state.ErrDurableStorageRecovery
	}
	reservation := budget.Reservation()
	if !reservation.Reserve(object.Size) {
		return agentapi.ReadBudgetLimit(errors.New("deletion object exceeds shared read budget"))
	}
	defer reservation.Release(object.Size)
	getter, ok := store.(storage.LimitedGetter)
	if !ok {
		return storage.ErrVersionedReadUnavailable
	}
	data, err := getter.GetLimited(ctx, object.Key, object.Size)
	if err != nil {
		return err
	}
	if int64(len(data)) != object.Size || !storage.VerifySHA256(data, sha) {
		return storage.ErrChecksumMismatch
	}
	return nil
}

func readDeletionMetadata(ctx context.Context, store storage.ObjectStore, key string, budget *agentapi.NativeReadBudget) ([]byte, func(), error) {
	const limit = 32 << 20
	reservation := budget.Reservation()
	if !reservation.Reserve(limit) {
		return nil, func() {}, agentapi.ReadBudgetLimit(errors.New("deletion metadata exceeds shared read budget"))
	}
	raw, err := storage.ReadPublicationMetadata(ctx, store, key)
	reservation.Release(limit - int64(len(raw)))
	return raw, func() { reservation.Release(int64(len(raw))) }, err
}
