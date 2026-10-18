package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/backfill"
	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

func commitNativeImportPreview(ctx context.Context, env Env, home string, cfg config.Config, opts backfillCommandOptions, p *nativeImportPreview) (batch backfill.Batch, err error) {
	if p.inventory != nil {
		for _, row := range p.Rows {
			if row.Status == "selected" {
				if err := p.inventory.Check(ctx, row.NativeID, row.token); err != nil {
					return batch, errors.New("native source changed since preview; confirm a new preview")
				}
			}
		}
	}
	unlock, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return batch, err
	}
	defer unlock()
	guard, err := local.LockCollectorGuard(home)
	if err != nil {
		return batch, err
	}
	defer guard.Release()
	store, err := state.Open(home)
	if err != nil {
		return batch, err
	}
	batch, err = confirmNativeImportPreview(ctx, env, store, cfg, opts, p)
	if err != nil {
		return batch, err
	}
	if err := env.checkpoint("native import confirmed"); err != nil {
		return batch, err
	}
	// Existing state-owned stage recovery needs no native input. It rechecks
	// each actual stage/control/reservation independently before first ownership.
	if p.resume != nil {
		if err := store.ResumeImportAdmissions(ctx); err != nil {
			return batch, err
		}
	}
	handle, err := store.OpenConfirmedNativeImportBatch(ctx, batch.ID)
	if err != nil {
		return batch, err
	}
	defer func() { err = errors.Join(err, handle.Close()) }()
	for i := range p.Rows {
		row := &p.Rows[i]
		if row.Status != "selected" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return batch, err
		}
		key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: row.NativeID}
		id, owned, err := store.ArchiveSessionID(key)
		if err != nil {
			return batch, err
		}
		if owned {
			reg, found, err := store.LoadRegistration(id)
			if err != nil || !found || reg.NativeImport == nil || reg.NativeImport.Confirmation.BatchID != batch.ID || reg.NativeImport.Confirmation.BatchInstance != batch.NativeImport.Instance || reg.NativeImport.Confirmation.ScopeSHA256 != batch.NativeImport.ScopeSHA256 || reg.NativeImport.Membership.State != archive.NativeImportAttached || !nativeImportExistingOwnerMatches(reg, *row, batch.DestinationID) {
				return batch, errors.Join(errors.New("independent or changed owner is preserved; confirm a new preview"), err)
			}
			batch.AddSessions([]string{id}, nil)
		} else {
			if p.inventory == nil {
				if err := prepareResumedNativeImportInventory(ctx, cfg, p); err != nil {
					return batch, err
				}
			}
			if row.token == "" {
				located, err := p.inventory.Locate(ctx, agentapi.LocatorRequest{Purpose: agentapi.LocatorHistoricalImport, Root: row.candidate.Root, NativeID: row.NativeID})
				if err != nil || located.Status != agentapi.LocatorFound || len(located.Candidates) != 1 {
					return batch, errors.Join(errors.New("confirmed source unavailable; preserved batch can resume when exact source returns"), err)
				}
				row.ref, row.token = located.Candidates[0], located.Revision
			}
			reg, err := prepareAndCommitNativeImport(ctx, env, store, handle, *row, p)
			if err != nil {
				return batch, fmt.Errorf("import %s stopped at entry %d; its frozen scope is preserved, run sync and retry the same explicit filters", batch.ID, row.Ordinal)
			}
			batch.AddSessions([]string{reg.ArchiveSessionID}, nil)
		}
		if err := backfill.SaveBatch(home, batch); err != nil {
			return batch, err
		}
		if err := env.checkpoint("native import registered"); err != nil {
			return batch, err
		}
	}
	at := env.now().UTC()
	batch.CompletedAt = &at
	return batch, backfill.SaveBatch(home, batch)
}

func prepareAndCommitNativeImport(ctx context.Context, env Env, store *state.Store, handle *state.ConfirmedNativeImportBatch, row nativeImportRow, p *nativeImportPreview) (reg archive.SessionRegistration, err error) {
	if err := p.inventory.Check(ctx, row.NativeID, row.token); err != nil {
		return reg, err
	}
	pass, snapshot, err := openNativeImportSource(ctx, env, row.candidate.Root, row.ref, p.inventory, p.budget)
	if err != nil {
		return reg, err
	}
	_, filter, ok := env.agentRegistry().LookupSources("muse")
	if !ok {
		return reg, errors.Join(agentapi.ErrInvalidSourceBinding, snapshot.Close(), pass.Close())
	}
	prepared, err := store.PrepareNativeImport(ctx, handle, row.candidate, snapshot, snapshot.Input(), filter, agentapi.FilterContext{Filename: row.ref.Path, Limits: agentapi.ReadLimits{RawBytes: collector.MaxRawTranscriptBytes("muse"), RecordBytes: archive.MaxRecordBytes, FilteredBytes: collector.DefaultMaxTranscriptBytes, ReadBudget: p.budget}}, p.inventory, row.token, p.budget, pass.Close)
	if err != nil {
		return reg, errors.Join(err, snapshot.Close(), pass.Close())
	}
	defer func() { err = errors.Join(err, prepared.Close()) }()
	return store.CommitNativeImport(ctx, prepared)
}

func confirmNativeImportPreview(ctx context.Context, env Env, store *state.Store, original config.Config, opts backfillCommandOptions, p *nativeImportPreview) (batch backfill.Batch, err error) {
	unlock, err := local.NamedLockWait(store.Home(), "hooks.lock", backfillHooksWait)
	if err != nil {
		return batch, err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return batch, err
	}
	cfg, found, err := config.Load(store.Home())
	if err != nil || !found {
		return batch, errors.Join(errNotSetUp, err)
	}
	if configFingerprint(cfg) != configFingerprint(original) {
		return batch, errors.New("configuration changed while preview was open; confirm a new preview")
	}
	if refusal := importRefusal(store.Home(), cfg); refusal != "" {
		return batch, errors.New(refusal)
	}
	if p.resume != nil {
		batch = *p.resume
		return batch, store.CheckNativeImportConfirmationScope(ctx, batch.NativeImport.Scope)
	}
	// This changes only historical import eligibility and its sticky writers.
	// Live capture roots/windows/harness enablement remain exactly as reviewed.
	cfg.NativeAuthorityProtection, cfg.NativeImportProtection = true, true
	cfg.DurableStorageProtection, cfg.PublicationCompositionProtection = true, true
	cfg.ImportStagingProtection, cfg.StagedSelectionProtection = true, true
	addedMuse := !slices.Contains(cfg.ImportedHarnesses, "muse")
	if addedMuse {
		cfg.ImportedHarnesses = append(slices.Clone(cfg.ImportedHarnesses), "muse")
	}
	if err := config.Save(store.Home(), cfg); err != nil {
		return batch, err
	}
	batch, err = backfill.OpenBatch(store.Home(), state.OpenReadOnly(store.Home()), nativeImportBatchFilters(opts, p.projects), cfg.DestinationID(), env.now())
	if err != nil {
		return batch, err
	}
	if batch.NativeImport != nil {
		return batch, errors.New("another confirmed import appeared; run backfill again to review it")
	}
	if addedMuse {
		batch.AppsAdded = append(batch.AppsAdded, "muse")
	}
	if err := backfill.SaveBatch(store.Home(), batch); err != nil {
		return batch, err
	}
	scope := archive.NativeImportBatchScope{BatchID: batch.ID, StartedAtUTC: batch.StartedAt.UTC().Format(time.RFC3339Nano), DestinationID: cfg.DestinationID(), Filters: archive.ImportBatchFilters(batch.Filters), SinceUTC: p.SinceUTC, UntilExclusiveUTC: p.UntilUTC, ProjectIDs: []string{}, Selected: []archive.NativeImportBatchSelection{}}
	for _, row := range p.Rows {
		if row.Status != "selected" {
			continue
		}
		scope.Selected = append(scope.Selected, row.selection)
		scope.ProjectIDs = append(scope.ProjectIDs, row.ProjectID)
	}
	slices.Sort(scope.ProjectIDs)
	scope.ProjectIDs = slices.Compact(scope.ProjectIDs)
	var raw []byte
	err = state.WithImportBatchLock(ctx, store.Home(), batch.ID, func(held *local.RootedHome) error {
		var err error
		raw, err = state.ReadImportBatchControlBytes(held, batch.ID)
		return err
	})
	if err != nil {
		return batch, err
	}
	sum := sha256.Sum256(raw)
	return backfill.ConfirmNativeImportBatch(ctx, store, batch, hex.EncodeToString(sum[:]), scope)
}

func prepareResumedNativeImportInventory(ctx context.Context, cfg config.Config, p *nativeImportPreview) error {
	p.budget = agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
	inventory, err := discovery.NewNativeImportInventory(ctx, cfg.NativeRoots, p.budget, builtin.ValidateNativeLocator)
	if err != nil {
		return err
	}
	p.inventory = inventory
	candidates, err := inventory.Candidates(ctx)
	if err != nil {
		return err
	}
	if !candidates.Complete {
		return errors.New("native namespace is incomplete; frozen batch remains pending")
	}
	return nil
}

func nativeImportExistingOwnerMatches(reg archive.SessionRegistration, row nativeImportRow, destination string) bool {
	if reg.NativeImport == nil || reg.NativeBinding == nil || reg.NativeSessionID != row.NativeID || reg.ProjectID != row.ProjectID || reg.ProjectRoot != row.Project || reg.DestinationID != destination || reg.NativeImport.Confirmation.SelectedSource != row.selection.Source || reg.NativeImport.Confirmation.SelectedBindingSHA256 != row.selection.BindingSHA256 || reg.NativeImport.Confirmation.RootFingerprint != row.selection.RootFingerprint || reg.NativeBinding.RootRevision != row.selection.RootRevision || reg.SessionStartedAt.UTC().Format(time.RFC3339Nano) != row.selection.OriginalTUTC {
		return false
	}
	digest, err := archive.NativeImportBindingDigest(reg.NativeBinding)
	return err == nil && digest == row.selection.BindingSHA256
}
