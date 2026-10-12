package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

// ImportStage freezes only privacy-filtered selected evidence before admission.
// The registration atomically selects its digest; an interrupted preparation
// cannot manufacture an admitted session.
type ImportStage struct {
	TypedSelection   bool                        `json:"typed_stage,omitempty"`
	ReleaseOnly      bool                        `json:"-"`
	Canceled         bool                        `json:"-"`
	CancellationOnly bool                        `json:"-"`
	Preserved        []archive.SourceBundle      `json:"preserved,omitempty"`
	Children         []SubagentCandidate         `json:"children,omitempty"`
	Version          int                         `json:"version"`
	Registration     archive.SessionRegistration `json:"registration"`
	Bundle           archive.SourceBundle        `json:"bundle"`
	SHA256           string                      `json:"sha256"`
	SkillEvidence    config.SkillEvidence        `json:"skill_evidence"`
}

// MaxImportStageBytes bounds a selected journal below the shared recovery
// reader's 128 MiB allowance, including encoded strings and ownership context.
const MaxImportStageBytes int64 = 32 << 20

func importStageRelative(id string) string {
	return filepath.Join("sessions", id, "import-stage", "stage.json")
}

func (p ImportStage) digest(ctx context.Context) (string, error) {
	p.SHA256 = ""
	p.Registration.StagedImportSHA256 = ""
	if p.TypedSelection {
		digest, err := boundedStagedWireDigest(ctx, p, MaxImportStageBytes)
		if errors.Is(err, agentmeta.ErrJSONWireLimit) || errors.Is(err, agentapi.ErrReadBudget) {
			return "", errors.Join(ErrDurableStorageCapacity, err)
		}
		return digest, err
	}
	if _, err := agentmeta.JSONWireBound(ctx, p, maxPendingHistoryBytes); err != nil {
		return "", errors.Join(ErrDurableStorageCapacity, err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if len(raw) > maxPendingHistoryBytes {
		return "", ErrDurableStorageCapacity
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ImportStages returns a bounded slice of durable preparation journals. The
// lexical cursor is progress only, never a proof of absence or permission.
func (s *Store) ImportStages(ctx context.Context, after string, limit int) (stages []ImportStage, next string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return nil, "", err
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	dir, err := home.Root.Open("sessions")
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	entries, err := dir.ReadDir(durableEntryLimit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, "", err
	}
	if len(entries) > durableEntryLimit {
		return nil, "", ErrDurableStorageRecovery
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	visited := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if !entry.IsDir() || entry.Name() <= after {
			continue
		}
		if !safeFileComponent(entry.Name()) {
			return nil, "", ErrDurableStorageRecovery
		}
		if visited == limit {
			return stages, next, nil
		}
		visited++
		next = entry.Name()
		path := importStageRelative(entry.Name())
		info, err := home.Root.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			stage, found, e := s.importStageAbsentDescriptor(ctx, entry.Name())
			if e != nil {
				return nil, "", e
			}
			if found {
				stages = append(stages, stage)
			}
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxPendingHistoryBytes {
			return nil, "", errors.Join(ErrDurableStorageRecovery, err)
		}
		pageBudget := s.resourceBudget
		if pageBudget == nil {
			pageBudget = agentapi.NewNativeReadBudget(128 << 20)
		}
		scratch, closeScratch := s.WithReadBudget(ctx, pageBudget)
		stage, found, err := scratch.loadPreparedImportStage(ctx, entry.Name())
		if err != nil || !found {
			closeScratch()
			return nil, "", errors.Join(ErrDurableStorageRecovery, err)
		}
		if stage.TypedSelection && s.resourceBudget == nil {
			closeScratch()
			return nil, "", ErrReadySelectionFrozen
		}
		stage.Bundle = archive.SourceBundle{}
		stage.Preserved = nil
		if err := s.retainImportDescriptor(ctx, stage); err != nil {
			closeScratch()
			return nil, "", err
		}
		closeScratch()

		var cancellation importCancellation
		if e := s.readImportControl(importCancelRelative(entry.Name()), &cancellation); e == nil {
			if cancellation.SHA256 != stage.SHA256 {
				return nil, "", ErrDurableStorageRecovery
			}
			stage.Canceled = true
		} else if !errors.Is(e, os.ErrNotExist) {
			return nil, "", e
		}
		stage.Bundle = archive.SourceBundle{}
		stage.Preserved = nil
		stages = append(stages, stage)
	}
	return stages, "", nil
}

// ResumeImportAdmissions recovers a bounded group after preparation or request
// crashes. The confirmed batch, current destination and authorization must still
// agree. Native deletion cannot invalidate already staged selected evidence.
func (s *Store) ResumeImportAdmissions(ctx context.Context) error {
	if s.resourceBudget == nil {
		cfg, present, err := config.Load(s.home)
		if err != nil {
			return err
		}
		if present && cfg.StagedSelectionProtection {
			read, closeRead := s.WithImportResultScope(ctx)
			defer closeRead()
			return read.ResumeImportAdmissions(ctx)
		}
	}

	cfg, found, err := config.Load(s.home)
	if err != nil {
		return err
	}
	if !found || !cfg.ImportStagingProtection || cfg.Paused || !cfg.Archive.Enabled {
		return nil
	}
	var cursor struct {
		After string `json:"after"`
	}
	if err := s.readImportControl("import-stage-cursor.json", &cursor); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	stages, next, closePage, err := s.ImportStagesOwned(ctx, cursor.After, 50)
	defer closePage()
	if err != nil {
		return err
	}
	for _, stage := range stages {
		if err := ctx.Err(); err != nil {
			return err
		}
		if stage.ReleaseOnly {
			continue
		}
		if stage.CancellationOnly {
			if err := s.removeImportCancellation(stage.Registration.ArchiveSessionID); err != nil {
				return err
			}
			continue
		}
		if stage.Canceled {
			if err := s.CancelImportStage(ctx, stage.Registration); err != nil {
				return err
			}
			continue
		}
		if err := s.resumePreparedImport(ctx, stage); err != nil {
			return err
		}
	}
	cursor.After = next
	return local.Write(filepath.Join(s.home, "import-stage-cursor.json"), cursor)
}

func (s *Store) resumePreparedImport(ctx context.Context, stage ImportStage) error {
	if stage.TypedSelection {
		cfg, present, err := config.Load(s.home)
		if err != nil || !present || !cfg.SupportsStagedSelection() {
			return errors.Join(ErrReadySelectionFrozen, err)
		}
	}
	reg := stage.Registration
	reg.StagedImportSHA256 = stage.SHA256
	// Children are materialized only after their parent's durable selection.
	if reg.ParentSessionID != "" {
		return nil
	}
	cfg, found, err := config.Load(s.home)
	if err != nil || !found {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if !cfg.ImportStagingProtection || !cfg.AcceptSession(reg) || cfg.DestinationID() != reg.DestinationID {
		return nil
	}
	var batch struct {
		ID            string     `json:"id"`
		DestinationID string     `json:"destination_id"`
		UndoneAt      *time.Time `json:"undone_at"`
	}
	if !safeFileComponent(reg.ImportBatch.Recorded()) {
		return ErrDurableStorageRecovery
	}
	if err := s.readImportControl(filepath.Join("imports", reg.ImportBatch.Recorded()+".json"), &batch); err != nil {
		return errors.Join(ErrDurableStorageRecovery, err)
	}
	if !reg.InBatch(batch.ID) || batch.DestinationID != reg.DestinationID {
		return ErrDurableStorageRecovery
	}
	if batch.UndoneAt != nil {
		return nil
	}
	return s.admitPreparedImport(ctx, stage, reg, batch.ID)
}

func (s *Store) admitPreparedImport(ctx context.Context, stage ImportStage, reg archive.SessionRegistration, batchID string) error {
	return config.WithImportStaging(s.home, func(g config.DurableStorageGuard) error {
		current, present, e := config.Load(s.home)
		if e != nil || !present {
			return errors.Join(ErrDurableStorageRecovery, e)
		}
		if current.Paused || !current.AcceptSession(reg) || current.DestinationID() != reg.DestinationID {
			return nil
		}
		existing, found, err := s.LoadRegistration(reg.ArchiveSessionID)
		if err != nil {
			return err
		}
		if found && (existing.StagedImportSHA256 != reg.StagedImportSHA256 || !existing.InBatch(batchID)) {
			return s.DropUnadmittedImportStage(ctx, stage)
		}
		children, err := s.preparedImportChildren(ctx, stage)
		if err != nil {
			return err
		}
		if !found {
			key := agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness(reg.Harness.Name)), NativeID: reg.NativeSessionID}
			_, err = s.RegisterReservedSession(key, reg.ArchiveSessionID, func(string) archive.SessionRegistration { return reg })
			if err != nil {
				return err
			}
		}
		for _, child := range children {
			if err := s.SaveSubagentCandidate(child); err != nil {
				return err
			}
		}
		publication, err := s.LoadPublishedStateContext(ctx, reg.ArchiveSessionID)
		if err != nil {
			return err
		}
		if _, _, published := publication.LastPublished(); !published {
			return s.SaveRequest(reg.ArchiveSessionID, "backfill", reg.AdmittedAt)
		}
		return nil
	})
}

// Validate checks the bounded journal digest, identity and selected source set.
func (p ImportStage) Validate(ctx context.Context, reg archive.SessionRegistration) error {
	if p.TypedSelection {
		if len(p.Children) > 64 {
			return ErrDurableStorageCapacity
		}
		if _, err := agentmeta.JSONWireBound(ctx, p.Children, 64<<10); err != nil {
			return errors.Join(ErrDurableStorageCapacity, err)
		}
	}
	expected, err := p.digest(ctx)
	if err != nil {
		return err
	}
	if (p.Version != 1 && p.Version != 2) || p.TypedSelection != (p.Version == 2) || p.SHA256 != expected {
		return ErrDurableStorageRecovery
	}
	if p.TypedSelection && (reg.Origin != archive.SessionOriginImport || reg.ImportBatch.Recorded() == "" || len(p.Preserved) != 0 || p.Bundle.History != nil || reg.HistoricalProject != nil || reg.Harness.Name == "cursor" || len(p.Children) > 64) {
		return ErrReadySelectionFrozen
	}
	if p.TypedSelection {
		if _, err := agentmeta.JSONWireBound(ctx, p.Children, 64<<10); err != nil {
			return errors.Join(ErrDurableStorageCapacity, err)
		}
	}
	return p.validateOwner(reg)
}

func (p ImportStage) validateOwner(reg archive.SessionRegistration) error {
	if reg.StagedImportSHA256 != "" && reg.StagedImportSHA256 != p.SHA256 || p.Registration.ArchiveSessionID != reg.ArchiveSessionID || p.Registration.NativeSessionID != reg.NativeSessionID || p.Registration.Harness.Name != reg.Harness.Name || p.Registration.ProjectID != reg.ProjectID || p.Registration.ProjectRoot != reg.ProjectRoot || !sameHistoricalProjectProof(p.Registration, reg) || !sameImportParentProof(p.Registration, reg) || p.Registration.DestinationID != reg.DestinationID || !p.Registration.InBatch(reg.ImportBatch.Recorded()) || p.Bundle.ArchiveSessionID != reg.ArchiveSessionID || p.Bundle.NativeSessionID != reg.NativeSessionID || p.Bundle.ProjectID != reg.ProjectID || p.Bundle.Capture.Harness.Name != reg.Harness.Name {
		return ErrDurableStorageRecovery
	}
	if len(p.Preserved) > archive.MaxHistorySpans {
		return ErrDurableStorageRecovery
	}
	for _, bundle := range p.Preserved {
		if bundle.ArchiveSessionID != reg.ArchiveSessionID || bundle.NativeSessionID != reg.NativeSessionID || bundle.ProjectID != reg.ProjectID || bundle.Capture.Harness.Name != reg.Harness.Name {
			return ErrDurableStorageRecovery
		}
		if err := archive.ValidateSourceBundle(bundle); err != nil {
			return err
		}
		if err := bundle.ValidateHistory(); err != nil {
			return err
		}
	}
	for _, child := range p.Children {
		if child.ParentArchiveSessionID != reg.ArchiveSessionID || child.ParentNativeSessionID != reg.NativeSessionID || child.ProjectID != reg.ProjectID || child.StagedImportSHA256 == "" {
			return ErrDurableStorageRecovery
		}
	}
	return errors.Join(p.Registration.Validate(), p.Bundle.ValidateHistory())
}

// SaveImportStage freezes one selected filtered source before admission.
func (s *Store) SaveImportStage(ctx context.Context, reg archive.SessionRegistration, bundle archive.SourceBundle, mode config.SkillEvidence, children ...SubagentCandidate) (string, error) {
	return s.SaveImportSourceSet(ctx, reg, bundle, nil, mode, children...)
}

// SaveImportSourceSet freezes the complete selected filtered revision set and children.
func (s *Store) SaveImportSourceSet(ctx context.Context, reg archive.SessionRegistration, bundle archive.SourceBundle, preserved []archive.SourceBundle, mode config.SkillEvidence, children ...SubagentCandidate) (string, error) {
	if !safeFileComponent(reg.ArchiveSessionID) {
		return "", ErrDurableStorageRecovery
	}
	stage := ImportStage{Version: 1, Registration: reg, Bundle: bundle, SkillEvidence: mode, Children: children, Preserved: preserved}
	cfg, present, err := config.Load(s.home)
	if err != nil {
		return "", err
	}
	if present && cfg.StagedSelectionProtection {
		if !cfg.SupportsStagedSelection() || len(preserved) != 0 || reg.HistoricalProject != nil || reg.Harness.Name == "cursor" {
			return "", ErrReadySelectionFrozen
		}
		stage.Version = 2
		stage.TypedSelection = true
		if len(children) > 64 {
			return "", ErrDurableStorageCapacity
		}
		if _, err := agentmeta.JSONWireBound(ctx, children, 64<<10); err != nil {
			return "", errors.Join(ErrDurableStorageCapacity, err)
		}
	}

	digest, err := stage.digest(ctx)
	if err != nil {
		return "", err
	}
	stage.SHA256 = digest
	if err := stage.Validate(ctx, reg); err != nil {
		return "", err
	}
	if err := validateImportStageCapacity(ctx, stage); err != nil {
		return "", err
	}
	err = config.WithImportStaging(s.home, func(g config.DurableStorageGuard) error {
		existing, found, err := s.LoadImportStage(ctx, reg)
		if err != nil {
			return err
		}
		if found {
			if existing.SHA256 != digest || existing.Version != stage.Version {
				return ErrSessionIdentityConflict
			}
			return nil
		}
		return s.writeDurableGuard(ctx, g, importStageRelative(reg.ArchiveSessionID), stage)
	})
	return digest, err
}

// Admission must not select a journal that the scheduled reader cannot load
// and refilter. The reader retains one encoded-size owner after its three-owner
// decode reservation, then reserves three largest-record owners and two output
// owners while applying current privacy rules.
func validateImportStageCapacity(ctx context.Context, stage ImportStage) error {
	return validateImportStageCapacityWithin(ctx, stage, MaxImportStageBytes, 128<<20)
}

// Explicit bounds keep capacity arithmetic testable without changing shared limits.
func validateImportStageCapacityWithin(ctx context.Context, stage ImportStage, maxBytes, allowance int64) error {
	n, err := agentmeta.JSONWireBound(ctx, stage, maxBytes)
	if err != nil {
		return errors.Join(ErrDurableStorageCapacity, err)
	}
	largest, retained := int64(0), int64(0)
	for _, bundle := range append([]archive.SourceBundle{stage.Bundle}, stage.Preserved...) {
		retained += int64(bundle.Capture.Boundary.RetainedBytes)
		for _, row := range bundle.NativeRecords {
			size, err := agentmeta.JSONWireBound(ctx, row, maxBytes)
			if err != nil {
				return errors.Join(ErrDurableStorageCapacity, err)
			}
			largest = max(largest, size)
		}
		for _, text := range bundle.NativeText {
			largest = max(largest, int64(len(text.Content)))
		}
	}
	scanner := int64(64 << 10)
	for scanner < largest+1 {
		scanner *= 2
	}
	remaining := allowance - n - 3*largest - scanner
	if retained > remaining/2 || remaining <= 0 {
		return errors.Join(ErrDurableStorageCapacity, errors.New("selected import evidence exceeds the durable recovery memory allowance"))
	}
	return nil
}

// LoadImportStage validates the selected digest and immutable admission identity.
func (s *Store) LoadImportStage(ctx context.Context, reg archive.SessionRegistration) (stage ImportStage, found bool, err error) {
	stage, found, err = s.loadPreparedImportStage(ctx, reg.ArchiveSessionID)
	if err != nil || !found {
		return stage, found, err
	}
	return stage, true, stage.validateOwner(reg)
}

func (s *Store) loadPreparedImportStage(ctx context.Context, id string) (stage ImportStage, found bool, err error) {
	reservation := s.resourceBudget.Reservation()
	if err := ctx.Err(); err != nil {
		return stage, false, err
	}
	reg := archive.SessionRegistration{ArchiveSessionID: id}

	if !safeFileComponent(reg.ArchiveSessionID) {
		return stage, false, ErrDurableStorageRecovery
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return stage, false, err
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	root, err := privateDirectory(home.Root, filepath.Dir(importStageRelative(id)), false)
	if errors.Is(err, os.ErrNotExist) {
		return stage, false, nil
	}
	if err != nil {
		return stage, false, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	info, err := root.Lstat("stage.json")
	if errors.Is(err, os.ErrNotExist) {
		return stage, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxPendingHistoryBytes {
		return stage, true, errors.Join(ErrDurableStorageRecovery, err)
	}
	file, err := root.Open("stage.json")
	if errors.Is(err, os.ErrNotExist) {
		return stage, false, nil
	}
	if err != nil {
		return stage, false, errors.Join(ErrDurableStorageRecovery, err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	openedInfo, err := file.Stat()
	if err == nil && !os.SameFile(info, openedInfo) {
		return stage, true, ErrDurableStorageRecovery
	}
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Size() != info.Size() || openedInfo.Size() > maxPendingHistoryBytes {
		return stage, true, errors.Join(ErrDurableStorageRecovery, err)
	}
	if s.resourceBudget != nil {
		n := info.Size()
		if !reservation.Reserve(3 * n) {
			return stage, true, errStateBudget
		}
		keep := false
		defer func() {
			if keep {
				reservation.Release(2 * n)
				*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(n) })
			} else {
				reservation.Release(3 * n)
			}
		}()
		// Ownership continues with the enclosing scoped state reader.
		defer func() {
			if err == nil {
				keep = true
			}
		}()
	}
	stage, err = s.decodePreparedImportStage(ctx, id, file, info.Size())
	return stage, true, err
}

// RemoveImportStage releases verified or explicitly cancelled selected evidence.
func (s *Store) RemoveImportStage(id string) error {
	stage, found, err := s.loadPreparedImportStage(s.durableContext(), id)
	if err != nil {
		return err
	}
	if found && stage.TypedSelection {
		return ErrReadySelectionFrozen
	}
	return s.removeImportFile(id, "stage.json")
}

func (s *Store) removeImportFile(id, name string) (err error) {
	if !safeFileComponent(id) {
		return ErrDurableStorageRecovery
	}
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	relative := filepath.Dir(importStageRelative(id))
	root, err := privateDirectory(home.Root, relative, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	err = root.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return err
	}
	return removeEmptyImportDirectory(home, relative)
}

// ResumeAllImportAdmissions is explicit import recovery under setup/collector
// ownership. Scheduled callers use the single-group method instead.
func (s *Store) ResumeAllImportAdmissions(ctx context.Context) error {
	cfg, found, err := config.Load(s.home)
	if err != nil {
		return err
	}
	if !found || cfg.Paused || !cfg.ImportStagingProtection {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.ResumeImportAdmissions(ctx); err != nil {
			return err
		}
		var cursor struct {
			After string `json:"after"`
		}
		if err := s.readImportControl("import-stage-cursor.json", &cursor); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if cursor.After == "" {
			return nil
		}
	}
}

// DropUnadmittedImportStage cancels a preparation superseded by an existing
// owner. It removes only its private filtered evidence, never registrations or
// remote objects that another admission owns.
func (s *Store) DropUnadmittedImportStage(ctx context.Context, stage ImportStage) error {
	if stage.TypedSelection {
		return ErrReadySelectionFrozen
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, child := range stage.Children {
		registered, admitted, err := s.LoadRegistration(child.ArchiveSessionID)
		if err != nil {
			return err
		}
		if admitted && registered.StagedImportSHA256 == child.StagedImportSHA256 {
			continue
		}
		candidate, referenced, err := s.LoadSubagentCandidate(child.ArchiveSessionID)
		if err != nil {
			return err
		}
		if referenced {
			if candidate.StagedImportSHA256 != child.StagedImportSHA256 || candidate.ParentArchiveSessionID != stage.Registration.ArchiveSessionID {
				return ErrDurableStorageRecovery
			}
			if err := s.removeSubagentCandidateContext(ctx, child.ArchiveSessionID); err != nil {
				return err
			}
		}
		prepared, found, err := s.loadPreparedImportStage(ctx, child.ArchiveSessionID)
		if err != nil {
			return err
		}
		if found {
			if prepared.SHA256 != child.StagedImportSHA256 || prepared.Registration.ParentSessionID != stage.Registration.ArchiveSessionID || !prepared.Registration.InBatch(stage.Registration.ImportBatch.Recorded()) {
				return ErrDurableStorageRecovery
			}
			if err := s.RemoveImportStage(child.ArchiveSessionID); err != nil {
				return err
			}
		}
	}
	return s.RemoveImportStage(stage.Registration.ArchiveSessionID)
}

// Control journals are read through the same home capability, with a fixed cap.
func (s *Store) readImportControl(relative string, value any) (err error) {
	home, err := local.OpenRootedHome(s.home)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, home.Close()) }()
	info, err := home.Root.Lstat(relative)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return ErrDurableStorageRecovery
	}
	file, err := home.Root.Open(relative)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) || opened.Size() != info.Size() {
		return ErrDurableStorageRecovery
	}
	decoder := json.NewDecoder(io.LimitReader(file, (4<<20)+1))
	if err = decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return ErrDurableStorageRecovery
	}
	return nil
}

// Cancellation is a small durable control record covered by each stage's quota
// reservation. It survives interrupted cleanup without admitting the selection.
type importCancellation struct {
	SHA256 string `json:"sha256"`
}

func importCancelRelative(id string) string {
	return filepath.Join("sessions", id, "import-stage", "cancel.json")
}

func (s *Store) removeImportCancellation(id string) error {
	return s.removeImportFile(id, "cancel.json")
}

// CancelImportStage journals unadmitted cleanup while protecting admitted owners.
func (s *Store) CancelImportStage(ctx context.Context, reg archive.SessionRegistration) error {
	return config.WithImportStaging(s.home, func(g config.DurableStorageGuard) error {
		cfg, present, err := config.Load(s.home)
		if err != nil {
			return err
		}
		if present && cfg.StagedSelectionProtection {
			stage, found, err := s.LoadImportStage(ctx, reg)
			if err != nil {
				return err
			}
			if found && stage.TypedSelection {
				return ErrReadySelectionFrozen
			}
		}

		admitted, found, err := s.LoadRegistration(reg.ArchiveSessionID)
		if err != nil {
			return err
		}
		if found && admitted.StagedImportSHA256 != "" {
			return nil
		}
		stage, found, err := s.LoadImportStage(ctx, reg)
		if err != nil || !found {
			return err
		}
		if stage.TypedSelection {
			return ErrReadySelectionFrozen
		}
		if err := s.writeDurableGuard(ctx, g, importCancelRelative(reg.ArchiveSessionID), importCancellation{SHA256: stage.SHA256}); err != nil {
			return err
		}
		if err := s.DropUnadmittedImportStage(ctx, stage); err != nil {
			return err
		}
		return s.removeImportCancellation(reg.ArchiveSessionID)
	})
}

func removeEmptyImportDirectory(home *local.RootedHome, relative string) (err error) {
	parent, err := privateDirectory(home.Root, filepath.Dir(relative), false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	info, err := parent.Lstat(filepath.Base(relative))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrDurableStorageRecovery
	}
	err = parent.Remove(filepath.Base(relative))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	dir, err := parent.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (s *Store) preparedImportChildren(ctx context.Context, stage ImportStage) (children []SubagentCandidate, err error) {
	for _, child := range stage.Children {
		registered, admitted, err := s.LoadRegistration(child.ArchiveSessionID)
		if err != nil {
			return nil, err
		}
		if admitted {
			if registered.StagedImportSHA256 != child.StagedImportSHA256 {
				return nil, ErrDurableStorageRecovery
			}
			continue // Its registration independently owns publication and recovery.
		}
		budget := agentapi.NewNativeReadBudget(128 << 20)
		if stage.TypedSelection && s.resourceBudget != nil {
			budget = s.resourceBudget
		}
		scratch, closeScratch := s.WithReadBudget(ctx, budget)
		prepared, found, err := scratch.loadPreparedImportStage(ctx, child.ArchiveSessionID)
		if err != nil {
			closeScratch()
			return nil, err
		}
		if !found || prepared.SHA256 != child.StagedImportSHA256 || prepared.Registration.ParentSessionID != stage.Registration.ArchiveSessionID || !prepared.Registration.InBatch(stage.Registration.ImportBatch.Recorded()) {
			closeScratch()
			return nil, ErrDurableStorageRecovery
		}
		closeScratch()
		children = append(children, child)
	}
	return children, nil
}

func sameImportParentProof(staged, current archive.SessionRegistration) bool {
	if staged.ParentSessionID == current.ParentSessionID {
		return true
	}
	// Native child links may resolve after independent admission. Their
	// immutable native relationship facts still select the same evidence.
	return staged.NativeChild && current.NativeChild &&
		staged.ParentNativeSessionID == current.ParentNativeSessionID &&
		staged.NativeRootSessionID == current.NativeRootSessionID &&
		current.CodexBinding != nil && current.CodexBinding.PreservesFacts(staged.CodexBinding)
}

func sameHistoricalProjectProof(a, b archive.SessionRegistration) bool {
	if a.HistoricalProject == nil || b.HistoricalProject == nil {
		return a.HistoricalProject == nil && b.HistoricalProject == nil
	}
	return a.HistoricalProject.Equal(b.HistoricalProject)
}

func (p *ImportStage) UnmarshalJSON(raw []byte) error {
	type plain ImportStage
	var header struct {
		Version        int  `json:"version"`
		TypedSelection bool `json:"typed_stage"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return err
	}
	var next plain
	if header.TypedSelection || header.Version == 2 {
		if int64(len(raw)) > MaxImportStageBytes {
			return ErrDurableStorageCapacity
		}
		if err := rejectStagedNullAliases(raw, []string{"version", "typed_stage", "sha256", "registration", "bundle", "children", "preserved", "skill_evidence"}); err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&next); err != nil {
			return ErrDurableStorageRecovery
		}
		if !next.TypedSelection || next.Version != 2 {
			return ErrDurableStorageRecovery
		}
	} else if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	*p = ImportStage(next)
	return nil
}

// retainImportDescriptor transfers the bounded descriptor/roster into the
// caller's captured page owner before releasing the full-stage read scratch.
func (s *Store) retainImportDescriptor(ctx context.Context, stage ImportStage) error {
	n, err := agentmeta.JSONWireBound(ctx, stage, 128<<20)
	if err != nil {
		return errors.Join(ErrDurableStorageCapacity, err)
	}
	if s.resourceBudget == nil {
		return nil
	}
	reservation := s.resourceBudget.Reservation()
	if !reservation.Reserve(n) {
		return errStateBudget
	}
	*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(n) })
	return nil
}

// ImportStagesOwned returns one captured page. The close must follow all
// descriptor/roster use; ordinary ImportStages retains its legacy API.
func (s *Store) ImportStagesOwned(ctx context.Context, after string, limit int) ([]ImportStage, string, func(), error) {
	budget := s.resourceBudget
	if budget == nil {
		budget = agentapi.NewNativeReadBudget(128 << 20)
	}
	read, closePage := s.WithReadBudget(ctx, budget)
	stages, next, err := read.ImportStages(ctx, after, limit)
	if err != nil {
		closePage()
		return nil, "", func() {}, err
	}
	return stages, next, closePage, nil
}

// WithImportResultScope keeps bounded copied results and nested page/child
// reads on one captured owner. Returned values must not outlive close.
func (s *Store) WithImportResultScope(ctx context.Context) (*Store, func()) {
	budget := s.resourceBudget
	if budget == nil {
		budget = agentapi.NewNativeReadBudget(128 << 20)
	}
	return s.WithReadBudget(ctx, budget)
}

// RetainImportID creates an independent result string while both page and
// receiving reservations coexist. The receiving scope owns it until close.
func (s *Store) RetainImportID(ctx context.Context, id string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !safeFileComponent(id) || s.resourceBudget == nil {
		return "", ErrDurableStorageRecovery
	}
	reservation := s.resourceBudget.Reservation()
	n := int64(len(id))
	if !reservation.Reserve(n) {
		return "", errStateBudget
	}
	copied := strings.Clone(id)
	*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(n) })
	return copied, nil
}

// Classification grants no deletion authority. The rooted immutable receipt
// selects the exact current registration digest; unknown or successor facts
// cannot be silently attributed to a different import.
func (s *Store) classifyReleasedImportOwner(ctx context.Context, id string, receipt stagedReleaseReceipt) (archive.SessionRegistration, error) {
	var reg archive.SessionRegistration
	found, err := s.readOwned(s.registrationPath(id), &reg)
	if err != nil || !found {
		return reg, errors.Join(ErrReadySelectionFrozen, err)
	}
	q := receipt.Selection
	if reg.Validate() != nil || reg.ArchiveSessionID != id || reg.Origin != archive.SessionOriginImport || reg.ImportBatch.Recorded() == "" || reg.StagedImportSHA256 != q.StageSHA256 || reg.DestinationID != q.Core.DestinationID {
		return reg, ErrReadySelectionFrozen
	}
	digest, err := s.readyDigest(ctx, reg)
	if err != nil || digest != q.Core.AdmissionContext {
		return reg, errors.Join(ErrReadySelectionFrozen, err)
	}
	return reg, nil
}

// importStageAbsentDescriptor checks immutable release attribution before cancellation.
func (s *Store) importStageAbsentDescriptor(ctx context.Context, id string) (ImportStage, bool, error) {
	var receipt stagedReleaseReceipt
	if e := s.readStagedControl(id, "release.json", &receipt); e == nil {
		if s.resourceBudget == nil {
			return ImportStage{}, false, ErrReadySelectionFrozen
		}
		reg, err := s.classifyReleasedImportOwner(ctx, id, receipt)
		if err != nil {
			return ImportStage{}, false, err
		}
		descriptor := ImportStage{Version: 2, TypedSelection: true, ReleaseOnly: true, SHA256: receipt.Selection.StageSHA256, Registration: reg}
		if err := s.retainImportDescriptor(ctx, descriptor); err != nil {
			return ImportStage{}, false, err
		}
		return descriptor, true, nil
	} else if !errors.Is(e, os.ErrNotExist) {
		return ImportStage{}, false, e
	}
	var cancel importCancellation
	if e := s.readImportControl(importCancelRelative(id), &cancel); e == nil {
		if _, e := hex.DecodeString(cancel.SHA256); e != nil || len(cancel.SHA256) != 64 {
			return ImportStage{}, false, ErrDurableStorageRecovery
		}
		return ImportStage{Canceled: true, CancellationOnly: true, SHA256: cancel.SHA256, Registration: archive.SessionRegistration{ArchiveSessionID: id}}, true, nil
	} else if !errors.Is(e, os.ErrNotExist) {
		return ImportStage{}, false, e
	}
	return ImportStage{}, false, nil
}

// decodePreparedImportStage shares the enclosing captured ledger; it owns only digest scratch.
func (s *Store) decodePreparedImportStage(ctx context.Context, id string, file io.Reader, size int64) (ImportStage, error) {
	var stage ImportStage
	reservation := s.resourceBudget.Reservation()
	decoder := json.NewDecoder(io.LimitReader(file, maxPendingHistoryBytes+1))
	if err := decoder.Decode(&stage); err != nil {
		return stage, errors.Join(ErrDurableStorageRecovery, err)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return stage, ErrDurableStorageRecovery
	}
	if stage.Registration.ArchiveSessionID != id {
		return stage, ErrDurableStorageRecovery
	}
	if stage.TypedSelection {
		const counterScratch = int64(32 << 10)
		if !reservation.Reserve(counterScratch) {
			return stage, errStateBudget
		}
		defer reservation.Release(counterScratch)
		cfg, present, e := config.Load(s.home)
		if e != nil || !present || !cfg.SupportsStagedSelection() || size > MaxImportStageBytes {
			return stage, errors.Join(ErrReadySelectionFrozen, e)
		}
	}
	return stage, stage.Validate(ctx, stage.Registration)
}
