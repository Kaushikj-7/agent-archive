package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

type nativeImportPublicationState struct {
	Owner     string
	Released  string
	Published string
}

type nativeImportPublicationTarget struct {
	Admission   string
	Destination string
	Policy      string
	Adapter     string
	Predecessor string
	Metadata    string
	Preparation string
}

func nativeImportPendingTarget(p PendingPublication) nativeImportPublicationTarget {
	return nativeImportPublicationTarget{Admission: p.Preparation.AdmissionContext, Destination: p.Preparation.DestinationID, Policy: p.Preparation.PolicyContext, Adapter: p.Bundle.Capture.AdapterVersion, Predecessor: p.Preparation.PredecessorSHA256, Metadata: readySHA(p.MetadataBytes), Preparation: p.Preparation.SHA256}
}

// nativeImportPublicationCommit captures only bounded equality digests. The
// final rooted reread is under the registration's existing request lock, after
// encoding and before rename; no captured routing decision grants authority.
func (s *Store) nativeImportPublicationCommit(ctx context.Context, id string, target nativeImportPublicationTarget) (func(*local.AtomicWriteWitness, func() error) error, error) {
	before, historical, err := s.nativeImportPublicationState(ctx, id, target)
	if err != nil || !historical {
		return nil, err
	}
	return func(witness *local.AtomicWriteWitness, commit func() error) error {
		unlock, err := local.NamedLockWait(s.home, requestLockName(id), time.Second)
		if err != nil {
			return err
		}
		defer unlock()
		scoped := *s
		scoped.atomicWitness = witness
		after, historical, err := scoped.nativeImportPublicationState(ctx, id, target)
		if err != nil {
			return err
		}
		if !historical || before != after {
			return ErrReadySelectionFrozen
		}
		return commit()
	}, nil
}

func (s *Store) nativeImportPublicationState(ctx context.Context, id string, target nativeImportPublicationTarget) (nativeImportPublicationState, bool, error) {
	var result nativeImportPublicationState
	read, closeRead := s.WithReadBudget(ctx, s.resourceBudget)
	defer closeRead()
	reg, found, err := read.LoadRegistration(id)
	if err != nil {
		return result, false, fmt.Errorf("read historical publication owner: %w", err)
	}
	if !found || reg.NativeImport == nil {
		var released stagedReleaseReceipt
		err := read.readStagedControl(id, stagedReleaseReceiptName, &released)
		if err == nil && released.Selection.Core.NativeImportAuthority != "" {
			return result, true, ErrReadySelectionFrozen
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, false, err
		}
		return result, false, nil
	}
	fail := func(err error) (nativeImportPublicationState, bool, error) {
		return result, true, errors.Join(ErrReadySelectionFrozen, err)
	}
	cfg, found, err := config.Load(s.home)
	if err != nil || !found || !cfg.NativeImportProtection || cfg.Paused || !cfg.Archive.Enabled || !cfg.AcceptSession(reg) || cfg.DestinationID() != target.Destination || reg.DestinationID != target.Destination {
		return fail(errors.Join(errors.New("historical publication policy changed"), err))
	}
	expected, err := archive.NativeImportAdmissionContext(reg)
	if err != nil || expected != target.Admission {
		return fail(errors.Join(errors.New("historical publication admission changed"), err))
	}
	expectedPolicy := readySHA([]byte(archive.FilterVersion + "\x00" + target.Adapter + "\x00" + string(cfg.EffectiveSkillEvidence())))
	if target.Policy != expectedPolicy {
		return fail(errors.New("historical publication filter policy changed"))
	}
	published, err := read.LoadPublishedStateContext(ctx, id)
	if err != nil {
		return fail(fmt.Errorf("load historical selecting publication: %w", err))
	}
	if err := published.ValidateNativeImportAuthority(ctx, reg); err != nil {
		return fail(fmt.Errorf("validate historical released lineage: %w", err))
	}
	currentMetadata := readySHA(published.state.MetadataBytes)
	if currentMetadata != target.Predecessor {
		if currentMetadata != target.Metadata || published.state.Commit == nil || published.state.Commit.PreparationSHA256 != target.Preparation {
			return fail(errors.New("historical publication predecessor changed"))
		}
	}
	original, err := read.nativeImportReleasedSelection(ctx, reg)
	if err != nil {
		return fail(fmt.Errorf("read historical original selection: %w", err))
	}
	result.Owner, err = read.readyDigest(ctx, reg)
	if err != nil {
		return fail(err)
	}
	result.Released, err = read.readyDigest(ctx, original)
	if err != nil {
		return fail(err)
	}
	result.Published, err = read.readyDigest(ctx, published.state)
	if err != nil {
		return fail(err)
	}
	return result, true, nil
}
