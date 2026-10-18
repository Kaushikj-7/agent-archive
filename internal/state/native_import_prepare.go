package state

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// NativeImportCandidate identifies one ordinary root in an actual confirmed
// scope. Historical authority never uses a live startup observation or window.
type NativeImportCandidate struct {
	NativeID    string
	Root        archive.NativeRoot
	ProjectRoot string
	ProjectID   string
}

// PreparedNativeImport holds one concrete Filter and its independently checked
// source. Only the creating Store and still-open confirmation can consume it.
type PreparedNativeImport struct {
	store               *Store
	read                *Store
	closeRead           func()
	self                *PreparedNativeImport
	confirmation        *ConfirmedNativeImportBatch
	releaseConfirmation func()
	candidate           NativeImportCandidate
	selected            archive.NativeImportBatchSelection
	binding             archive.NativeSourceBinding
	snapshot            agentapi.SourceSnapshot
	input               agentapi.NativeInput
	content             agentapi.VerifiedContentSnapshot
	lookup              agentapi.NativeLocatorLookup
	selection           string
	filtered            archive.FilteredTranscript
	releaseOutput       func()
	closePass           func() error
	closed              bool
	err                 error
	reg                 archive.SessionRegistration
	filteredDigest      string
	adapter             preparedImportAdapter
	index               qualifiedSessionIndexEntry
	indexFound          bool
	stage               ImportStage
	committed           bool
}

// PrepareNativeImport filters exactly one proven selected ordinary source. It
// grants no owner or index authority and performs no durable writes. On success
// it owns snapshot and closePass; callers retain their ownership on failure.
func (s *Store) PrepareNativeImport(ctx context.Context, confirmation *ConfirmedNativeImportBatch, candidate NativeImportCandidate, snapshot agentapi.SourceSnapshot, input agentapi.NativeInput, filter agentapi.TranscriptFilter, filterContext agentapi.FilterContext, lookup agentapi.NativeLocatorLookup, selection string, budget *agentapi.NativeReadBudget, closePass func() error) (*PreparedNativeImport, error) {
	content, leased, evidence, valid := nativeAdmissionInputs(snapshot, input, filter)
	if s.hook || !valid || lookup == nil || selection == "" || budget == nil || closePass == nil || candidate.Root.Agent != "muse" || candidate.Root.Validate() != nil || !filepath.IsAbs(candidate.ProjectRoot) || !validNativeAdmissionProjectID(candidate.ProjectID) {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	release, err := confirmation.borrow(s)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			release()
		}
	}()
	if err := confirmation.checkCurrent(ctx, s); err != nil {
		return nil, err
	}
	selected, err := selectedNativeImport(confirmation, candidate)
	if err != nil {
		return nil, err
	}
	facts, err := evidence.AdmissionEvidence(ctx, agentapi.SourceAdmission{NativeID: candidate.NativeID})
	if err != nil {
		return nil, err
	}
	binding := facts.Binding.Native
	if facts.Binding.Codex != nil || !nativeImportSelectedBinding(candidate, selected, binding) {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	physical, known := sourcefacts.PhysicalProject(binding.Cwd)
	if !known || physical.Root != candidate.ProjectRoot {
		return nil, errors.New("native import source does not belong to confirmed physical project")
	}
	copied := *binding
	checkpoint := *binding.Checkpoint
	copied.Checkpoint = &checkpoint
	p := &PreparedNativeImport{store: s, confirmation: confirmation, releaseConfirmation: release, candidate: candidate, selected: selected, binding: copied, snapshot: snapshot, input: input, content: content, lookup: lookup, selection: selection, closePass: closePass}
	joined, readContext, closeContext := confirmation.read.withDurableContext(ctx)
	p.read, p.closeRead = joined.WithReadBudget(readContext, joined.resourceBudget)
	closeRead := p.closeRead
	p.closeRead = func() { closeRead(); closeContext() }
	defer func() {
		if !keep {
			p.closeRead()
		}
	}()
	p.self = p
	if err := p.verify(ctx); err != nil {
		return nil, err
	}
	filterContext.StartedAt = binding.NativeCreatedAt
	filtered, releaseOutput, err := leased.FilterLeased(ctx, input, filterContext, budget)
	if err != nil {
		if releaseOutput != nil {
			releaseOutput()
		}
		return nil, err
	}
	if filtered.History != nil || !nativeFilteredStartupMatches(filtered, &p.binding) {
		if releaseOutput != nil {
			releaseOutput()
		}
		return nil, agentapi.ErrInvalidSourceBinding
	}
	p.filtered, p.releaseOutput = filtered, releaseOutput
	p.adapter = preparedImportAdapter{name: filter.Name(), version: filter.Version()}
	p.filteredDigest, err = p.filterDigest(ctx)
	if err != nil {
		if releaseOutput != nil {
			releaseOutput()
		}
		return nil, err
	}
	if err := p.verify(ctx); err != nil {
		if releaseOutput != nil {
			releaseOutput()
		}
		return nil, err
	}
	keep = true
	return p, nil
}

func selectedNativeImport(h *ConfirmedNativeImportBatch, candidate NativeImportCandidate) (archive.NativeImportBatchSelection, error) {
	fingerprint, err := archive.NativeRootFingerprint(candidate.Root)
	if err != nil {
		return archive.NativeImportBatchSelection{}, err
	}
	for _, selected := range h.control.NativeImport.Scope.Selected {
		if selected.NativeID == candidate.NativeID && selected.ProjectID == candidate.ProjectID && selected.RootFingerprint == fingerprint && selected.RootRevision == candidate.Root.Revision && selected.Kind == archive.NativeImportOrdinaryRoot {
			return selected, nil
		}
	}
	return archive.NativeImportBatchSelection{}, ErrReadySelectionFrozen
}

func nativeImportSelectedBinding(candidate NativeImportCandidate, selected archive.NativeImportBatchSelection, binding *archive.NativeSourceBinding) bool {
	digest, err := archive.NativeImportBindingDigest(binding)
	if err != nil || binding.NativeID != candidate.NativeID || binding.RootRevision != candidate.Root.Revision || binding.ParentID != "" || binding.RootID != "" && binding.RootID != binding.NativeID {
		return false
	}
	relative, err := filepath.Rel(candidate.Root.Canonical, binding.Path)
	return err == nil && filepath.IsLocal(relative) && selected.BindingSHA256 == digest && selected.Source == *binding.Checkpoint && selected.OriginalTUTC == binding.NativeCreatedAt.UTC().Format(time.RFC3339Nano)
}

func (p *PreparedNativeImport) verify(ctx context.Context) error {
	if p == nil || p.closed || p.self != p || p.store == nil || !p.content.ContentProofFor(p.snapshot, p.input) {
		return agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.confirmation.checkCurrent(ctx, p.store); err != nil {
		return err
	}
	if p.filteredDigest != "" {
		digest, err := p.filterDigest(ctx)
		if err != nil || digest != p.filteredDigest {
			return errors.Join(agentapi.ErrInvalidSourceBinding, err)
		}
	}
	if err := p.checkPhysicalProject(); err != nil {
		return err
	}
	if err := p.input.File.Check(); err != nil {
		return err
	}
	if err := p.content.VerifyContent(ctx); err != nil {
		return err
	}
	proof, err := p.content.ContentProof(ctx)
	if err != nil {
		return err
	}
	checkpoint := p.selected.Source
	if proof.Version != checkpoint.Version || proof.Provider != checkpoint.Domain || proof.Boundary != checkpoint.Boundary || hex.EncodeToString(proof.SHA256[:]) != checkpoint.SHA256 {
		return agentapi.ErrInvalidSourceBinding
	}
	if err := p.input.File.Check(); err != nil {
		return err
	}
	return p.lookup.Check(ctx, p.candidate.NativeID, p.selection)
}

func (p *PreparedNativeImport) checkPhysicalProject() error {
	cfg, found, err := p.store.loadConfig()
	if err != nil || !found {
		return errors.Join(ErrReadySelectionFrozen, err)
	}
	for _, project := range cfg.Archive.Projects {
		if project.Root == p.candidate.ProjectRoot && project.ProjectID == p.candidate.ProjectID && project.Included {
			return nil
		}
	}
	return ErrReadySelectionFrozen
}

// Filtered borrows the one filtered result until Close. Current authority must
// still be rechecked by the eventual staged first-owner commit.
func (p *PreparedNativeImport) Filtered() (archive.FilteredTranscript, error) {
	if p == nil || p.closed || p.self != p || p.store == nil {
		return archive.FilteredTranscript{}, agentapi.ErrClosed
	}
	return p.filtered, nil
}

// Close expires the prepared source and releases output, source and batch loans.
func (p *PreparedNativeImport) Close() error {
	if p == nil {
		return nil
	}
	if p.self != p && p.store != nil {
		return agentapi.ErrClosed
	}
	if p.closed {
		return p.err
	}
	p.closed = true
	if p.releaseOutput != nil {
		p.releaseOutput()
	}
	p.filtered = archive.FilteredTranscript{}
	if p.snapshot != nil {
		p.err = errors.Join(p.err, p.snapshot.Close())
	}
	if p.closePass != nil {
		p.err = errors.Join(p.err, p.closePass())
	}
	// No charged bundle/control/source remains reachable through the expired
	// capability after its loans are released.
	p.stage = ImportStage{}
	p.reg = archive.SessionRegistration{}
	p.selected = archive.NativeImportBatchSelection{}
	p.binding = archive.NativeSourceBinding{}
	p.candidate = NativeImportCandidate{}
	p.adapter = preparedImportAdapter{}
	p.filteredDigest = ""
	p.snapshot = nil
	p.content = nil
	p.input = agentapi.NativeInput{}
	p.lookup = nil
	p.releaseOutput = nil
	p.closePass = nil
	closeRead, releaseConfirmation := p.closeRead, p.releaseConfirmation
	p.read = nil
	p.confirmation = nil
	p.closeRead = nil
	p.releaseConfirmation = nil
	if closeRead != nil {
		closeRead()
	}
	if releaseConfirmation != nil {
		releaseConfirmation()
	}
	return p.err
}

type preparedImportAdapter struct{ name, version string }

func (a preparedImportAdapter) Name() string    { return a.name }
func (a preparedImportAdapter) Version() string { return a.version }

func (p *PreparedNativeImport) filterDigest(ctx context.Context) (string, error) {
	return p.read.readyDigest(ctx, struct {
		Transcript      archive.FilteredTranscript `json:"transcript"`
		Records         [][]byte                   `json:"records"`
		Text            []string                   `json:"text"`
		Ordinals        []uint64                   `json:"ordinals"`
		ObservedHarness archive.Harness            `json:"observed_harness"`
	}{p.filtered, p.filtered.Records, p.filtered.Text, p.filtered.Ordinals, p.filtered.ObservedHarness})
}
