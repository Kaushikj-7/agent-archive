package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
)

// pendingImportChildren counts selected children that have not materialized yet.
// Batch membership only selects a read; the candidate, admitted parent and exact
// retained stage must still agree. Nothing here supplies publication authority.
func pendingImportChildren(store *state.Store, cfg config.Config, regs []archive.SessionRegistration, batch backfill.Batch) (pending, blocked int, err error) {
	registered := make(map[string]archive.SessionRegistration, len(regs))
	for _, reg := range regs {
		registered[reg.ArchiveSessionID] = reg
	}
	seen := map[string]bool{}
	for _, id := range batch.Subagents {
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, found := registered[id]; found {
			continue // Its own Outstanding record already counts publication.
		}
		candidate, found, readErr := store.LoadSubagentCandidate(id)
		if readErr != nil {
			return pending, blocked, fmt.Errorf("read selected imported child %s: %w", id, readErr)
		}
		if !found || candidate.Origin != archive.SessionOriginImport || candidate.StagedImportSHA256 == "" {
			present, stageErr := store.HasImportStage(id)
			if present || stageErr != nil {
				return pending, blocked, fmt.Errorf("read selected imported child %s: %w", id, errors.Join(state.ErrDurableStorageRecovery, stageErr))
			}
			continue
		}
		parent, found := registered[candidate.ParentArchiveSessionID]
		if !found || !parent.InBatch(batch.ID) {
			continue
		}
		if candidate.ArchiveSessionID != id || candidate.ParentNativeSessionID != parent.NativeSessionID {
			return pending, blocked, state.ErrDurableStorageRecovery
		}
		current, readErr := pendingImportChildPolicy(store, cfg, parent, candidate, batch.ID)
		if readErr != nil {
			return pending, blocked, fmt.Errorf("read retained imported child %s: %w", id, readErr)
		}
		pending++
		if !current {
			blocked++
		}
	}
	return pending, blocked, nil
}

func pendingImportChildPolicy(store *state.Store, cfg config.Config, parent archive.SessionRegistration, candidate state.SubagentCandidate, batchID string) (bool, error) {
	// The expected owner is used only by the existing retained-stage validator.
	// Current policy is evaluated on the actual frozen child registration.
	owner := parent
	owner.ArchiveSessionID, owner.NativeSessionID = candidate.ArchiveSessionID, candidate.NativeSessionID
	owner.ParentSessionID = parent.ArchiveSessionID
	owner.Harness, owner.ProjectID, owner.ProjectRoot = candidate.Harness, candidate.ProjectID, candidate.ProjectRoot
	owner.HistoricalProject = candidate.HistoricalProject
	owner.StagedImportSHA256 = candidate.StagedImportSHA256
	read, closeRead := store.WithImportResultScope(context.Background())
	defer closeRead()
	stage, found, err := read.LoadImportStage(context.Background(), owner)
	if err != nil || !found || !parent.InBatch(batchID) || stage.Registration.Origin != archive.SessionOriginImport || !stage.Registration.InBatch(batchID) || stage.Registration.ParentNativeSessionID != parent.NativeSessionID {
		return false, errors.Join(state.ErrDurableStorageRecovery, err)
	}
	return cfg.AcceptSession(stage.Registration), nil
}

func uploadBatch(home, id string) (backfill.Batch, error) {
	batches, err := backfill.LoadBatches(home)
	if err != nil {
		return backfill.Batch{}, err
	}
	for _, batch := range batches {
		if batch.ID == id {
			return batch, nil
		}
	}
	return backfill.Batch{}, errors.Join(state.ErrDurableStorageRecovery, err)
}
