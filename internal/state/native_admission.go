package state

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// NativeAdmissionCandidate contains original observed authority, never an owner.
type NativeAdmissionCandidate struct {
	Intent      agentapi.AdmissionIntent `json:"Intent"`
	Root        archive.NativeRoot       `json:"Root"`
	ProjectRoot string                   `json:"ProjectRoot"`
	ProjectID   string                   `json:"ProjectID"`
}

// PreparedNativeAdmission retains one successful concrete Filter and its live
// source proof. Filtered borrows its charged output until Close.
type PreparedNativeAdmission struct {
	store         *Store
	candidate     NativeAdmissionCandidate
	snapshot      agentapi.SourceSnapshot
	input         agentapi.NativeInput
	content       agentapi.VerifiedContentSnapshot
	lookup        agentapi.NativeLocatorLookup
	selection     string
	closePass     func() error
	releaseOutput func()
	filtered      archive.FilteredTranscript
	reg           archive.SessionRegistration
	index         qualifiedSessionIndexEntry
	indexFound    bool
	committed     bool
	closed        bool
	err           error
}

// PrepareNativeAdmission runs exactly one self-attested concrete leased Filter
// after original native time and current captured policy have passed preflight.
// It owns snapshot and closePass on success; callers close them on failure.
func (s *Store) PrepareNativeAdmission(ctx context.Context, candidate NativeAdmissionCandidate, snapshot agentapi.SourceSnapshot, input agentapi.NativeInput, filter agentapi.TranscriptFilter, filterContext agentapi.FilterContext, lookup agentapi.NativeLocatorLookup, selection string, budget *agentapi.NativeReadBudget, closePass func() error) (*PreparedNativeAdmission, error) {
	content, leased, evidence, valid := nativeAdmissionInputs(snapshot, input, filter)
	if s.hook || !valid || lookup == nil || selection == "" || closePass == nil || budget == nil {
		return nil, agentapi.ErrInvalidSourceBinding
	}

	var err error
	candidate, err = copyNativeAdmissionCandidate(candidate)
	if err != nil {
		return nil, err
	}
	observed := candidate.Intent
	if !nativeStartupObservationMatches(observed, candidate.ProjectRoot) {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	facts, err := evidence.AdmissionEvidence(ctx, agentapi.SourceAdmission{NativeID: observed.NativeSessionID})
	if err != nil {
		return nil, err
	}
	binding := facts.Binding.Native
	if facts.Binding.Codex != nil || !nativeStartupBindingMatches(binding, candidate) {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	physical, known := sourcefacts.PhysicalProject(binding.Cwd)
	if !known || physical.Root != candidate.ProjectRoot {
		return nil, errors.New("native source does not belong to intended physical project")
	}
	now := time.Now().UTC()
	var replay *archive.Replay
	if observed.Replay != nil {
		marker := *observed.Replay
		replay = &marker
	}
	reg := archive.SessionRegistration{Replay: replay, NativeSessionID: observed.NativeSessionID, Harness: archive.Harness{Name: "muse"}, NativeAdmission: observed.NativeAdmission, NativeBinding: binding, TranscriptPath: binding.Path, ProjectRoot: candidate.ProjectRoot, ProjectID: candidate.ProjectID, DestinationID: observed.DestinationID, SessionStartedAt: binding.NativeCreatedAt, RegisteredAt: now, AdmittedAt: now, HookObservedAt: observed.ObservedAt, Origin: archive.SessionOriginHook, StartedAtSource: archive.StartedAtSourceTranscript}
	p := &PreparedNativeAdmission{store: s, candidate: candidate, snapshot: snapshot, input: input, content: content, lookup: lookup, selection: selection, closePass: closePass, reg: reg}
	if err := p.checkPolicy(ctx, now); err != nil {
		return nil, err
	}
	if err := lookup.Check(ctx, binding.NativeID, selection); err != nil {
		return nil, err
	}
	proof, err := content.ContentProof(ctx)
	if err != nil {
		return nil, err
	}
	cp := binding.Checkpoint
	if proof.Version != cp.Version || proof.Provider != cp.Domain || proof.Boundary != cp.Boundary || hex.EncodeToString(proof.SHA256[:]) != cp.SHA256 {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	filterContext.StartedAt = binding.NativeCreatedAt
	filtered, release, err := leased.FilterLeased(ctx, input, filterContext, budget)
	if err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	if !nativeFilteredStartupMatches(filtered, binding) {
		if release != nil {
			release()
		}
		return nil, agentapi.ErrInvalidSourceBinding
	}
	p.filtered, p.releaseOutput = filtered, release
	if err = p.verify(ctx); err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	if err := p.allocateNativeAdmissionIdentity(); err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	return p, nil
}

func (p *PreparedNativeAdmission) verify(ctx context.Context) error {
	if p.closed || !p.content.ContentProofFor(p.snapshot, p.input) {
		return agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.input.File.Check(); err != nil {
		return err
	}
	if err := p.content.VerifyContent(ctx); err != nil {
		return err
	}
	if err := p.input.File.Check(); err != nil {
		return err
	}
	return p.lookup.Check(ctx, p.reg.NativeSessionID, p.selection)
}

func (p *PreparedNativeAdmission) checkPolicy(ctx context.Context, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(p.store.home, "setup-transaction.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("setup pending before native admission")
	}
	cfg, found, err := config.Load(p.store.home)
	if err != nil {
		return err
	}
	i, root, b := p.candidate.Intent, p.candidate.Root, p.reg.NativeBinding
	window := cfg.MuseCapture
	if !nativeAdmissionWindowMatches(cfg, found, p.reg, i, at) {
		return errors.New("native startup is outside original capture window")
	}
	policy, err := config.MuseAdmissionPolicyFingerprint(cfg)
	digest, rootErr := archive.NativeRootFingerprint(root)
	a := i.NativeAdmission
	if err != nil || rootErr != nil || policy != a.PolicyFingerprint || policy != window.PolicyFingerprint || window.Generation != a.WindowGeneration || a.RootFingerprint != digest || !slices.Contains(cfg.NativeRoots, root) {
		return errors.New("native startup policy changed")
	}
	selected := archive.ProjectActivation{}
	foundProject := false
	for _, project := range cfg.Archive.Projects {
		if local.PathWithin(b.Cwd, project.Root) && (!foundProject || len(project.Root) > len(selected.Root) || len(project.Root) == len(selected.Root) && !project.Included) {
			selected = project
			foundProject = true
		}
	}
	if !foundProject || !selected.Included || selected.Root != p.candidate.ProjectRoot || selected.ProjectID != p.candidate.ProjectID || selected.ActivatedAt.IsZero() || b.NativeCreatedAt.Before(selected.ActivatedAt) || !local.PathWithin(i.ObservedCwd, selected.Root) {
		return errors.New("native startup project activation changed")
	}
	if _, removed, err := p.store.Removal("muse", i.NativeSessionID); err != nil || removed {
		return errors.New("native startup was removed")
	}
	return nil
}

// CommitNativeAdmission atomically exposes a complete proof-bearing first owner.
// Reservation writes begin only after full Filter, independent verification and
// a locked current-policy/absent-owner comparison. Every sync stays outside hooks.
func (s *Store) CommitNativeAdmission(ctx context.Context, p *PreparedNativeAdmission) (archive.SessionRegistration, error) {
	if p == nil || p.store != s || p.closed {
		return archive.SessionRegistration{}, agentapi.ErrClosed
	}
	if p.committed {
		return p.reg, nil
	}
	s, ctx, closeContext := s.withDurableContext(ctx)
	defer closeContext()
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: p.reg.NativeSessionID}
	lock := func() (func(), error) {
		hooks, err := s.namedLockWait("hooks.lock", time.Second)
		if err != nil {
			return nil, err
		}
		request, err := s.lockRequest(p.reg.ArchiveSessionID)
		if err != nil {
			hooks()
			return nil, err
		}
		return func() { request(); hooks() }, nil
	}
	expected := p.index
	reserved := indexEntry(key, p.reg.ArchiveSessionID)
	authority, err := p.reservationAuthority()
	if err != nil {
		return archive.SessionRegistration{}, err
	}
	reserved.NativeReservation = authority
	if p.indexFound && p.index.Reservation != "" {
		reserved = p.index
	} else {
		token, err := local.ID()
		if err != nil {
			return archive.SessionRegistration{}, err
		}
		reserved.Reservation = token
	}
	indexStore := *s
	indexStore.indexSnapshots = true
	indexStore.onWriteSync = s.onIndexSync
	err = indexStore.writeUnderLock(lockedWrite{nativeReservation: p, path: qualifiedSessionIndexPath(s.home, key), lock: lock, beforeLock: func() error { return p.verify(ctx) }, check: func() error { return p.checkPolicy(ctx, time.Now().UTC()) }, change: func(current fileSnapshot) (any, bool, error) {
		if current.found != p.indexFound {
			return nil, false, errIndexMoved
		}
		if current.found {
			var existing qualifiedSessionIndexEntry
			if json.Unmarshal(current.data, &existing) != nil || existing != expected {
				return nil, false, errIndexMoved
			}
		}
		owner, found, err := s.LoadRegistration(p.reg.ArchiveSessionID)
		if err != nil || found {
			_ = owner
			return nil, false, errors.New("native startup owner appeared")
		}
		return reserved, true, nil
	}})
	if err != nil {
		return archive.SessionRegistration{}, err
	}
	p.index, p.indexFound = reserved, true
	if err := s.indexStep("reservation"); err != nil {
		return archive.SessionRegistration{}, err
	}
	prepared := &nativeAdmissionCommit{prepared: p}
	err = s.writeUnderLock(lockedWrite{path: s.registrationPath(p.reg.ArchiveSessionID), nativeAdmission: prepared, lock: lock, beforeLock: func() error { return p.verify(ctx) }, check: func() error {
		if err := p.checkPolicy(ctx, time.Now().UTC()); err != nil {
			return err
		}
		current, found, err := s.readQualifiedIndex(key)
		if err != nil {
			return err
		}
		if !found || current != reserved {
			return errIndexMoved
		}
		return nil
	}, change: func(current fileSnapshot) (any, bool, error) {
		if current.found {
			return nil, false, errors.New("native first owner changed")
		}
		return p.reg, true, nil
	}})
	if err != nil {
		return archive.SessionRegistration{}, err
	}
	if err := s.indexStep("registration"); err != nil {
		return archive.SessionRegistration{}, err
	}
	if err := s.finishSessionIndex(key, p.reg.ArchiveSessionID); err != nil {
		return archive.SessionRegistration{}, err
	}
	p.committed = true
	return p.reg, nil
}

type nativeAdmissionCommit struct{ prepared *PreparedNativeAdmission }

func (p *nativeAdmissionCommit) check(next archive.SessionRegistration, found bool) error {
	if found || p == nil || p.prepared == nil || p.prepared.closed {
		return errors.New("native admission requires absent owner")
	}
	a, _ := json.Marshal(p.prepared.reg)
	b, _ := json.Marshal(next)
	if string(a) != string(b) {
		return errors.New("native admission prepared owner changed")
	}
	return nil
}

// Filtered borrows the one successfully filtered output until Close.
func (p *PreparedNativeAdmission) Filtered() (archive.FilteredTranscript, error) {
	if p == nil || p.closed || p.store == nil {
		return archive.FilteredTranscript{}, agentapi.ErrClosed
	}
	return p.filtered, nil
}

// Close releases output first, then the snapshot and its owning native pass.
func (p *PreparedNativeAdmission) Close() error {
	if p == nil {
		return nil
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
	return p.err
}

func (p *PreparedNativeAdmission) allocateNativeAdmissionIdentity() error {
	var err error
	// Identity allocation is in memory only and follows full source proof.
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: p.reg.NativeSessionID}
	if err := p.store.generationReservationAllowed(key); err != nil {
		return err
	}
	if _, owned, err := p.store.ArchiveSessionID(key); err != nil || owned {
		return errors.New("native startup identity is already owned or unresolved")
	}
	p.index, p.indexFound, err = p.store.readQualifiedIndex(key)
	if err != nil {
		return err
	}
	if p.indexFound && !p.index.Absent {
		authority, err := p.reservationAuthority()
		if err != nil {
			return err
		}
		if !supportedNativeReservation(p.index.NativeReservation) || p.index.NativeReservation != authority {
			return errors.New("native pending reservation authority differs or is missing")
		}
		registered, e := p.store.matchingRegistration(key, p.index.ArchiveSessionID)
		if e != nil || registered || p.index.Reservation == "" {
			return errors.New("native startup already has an owner or conflicting index")
		}
		p.reg.ArchiveSessionID = p.index.ArchiveSessionID
	} else {
		if err := p.store.sessionIndexMissAllowed(); err != nil {
			return err
		}
		p.reg.ArchiveSessionID, err = local.ID()
	}
	if err != nil {
		return err
	}
	if err := p.reg.Validate(); err != nil {
		return err
	}
	return nil
}

func nativeStartupObservationMatches(observed agentapi.AdmissionIntent, projectRoot string) bool {
	return agentapi.ValidNativeAdmissionIntentShape(observed) && observed.NativeMode == agentapi.NativeStartupCandidate && observed.ProjectRoot == projectRoot
}

func nativeStartupBindingMatches(binding *archive.NativeSourceBinding, candidate NativeAdmissionCandidate) bool {
	return binding != nil && binding.Version == 2 && binding.Validate() == nil && binding.NativeID == candidate.Intent.NativeSessionID && binding.RootRevision == candidate.Root.Revision && binding.Agent == "muse" && binding.ParentID == "" && (binding.RootID == "" || binding.RootID == binding.NativeID)
}

func nativeAdmissionWindowMatches(cfg config.Config, found bool, reg archive.SessionRegistration, i agentapi.AdmissionIntent, at time.Time) bool {
	window, b := cfg.MuseCapture, reg.NativeBinding
	return (i.Replay == nil || cfg.NativeReplayProtection) && found && cfg.NativeAdmissionProtection && cfg.NativeAuthorityProtection && !cfg.Paused && window != nil && cfg.Archive.Enabled && slices.Contains(cfg.Harnesses, "muse") && cfg.DestinationID() == i.DestinationID && cfg.PauseGeneration == i.PauseGeneration && nativeDestinationConfigured(cfg.Storage) && at.Sub(i.ObservedAt) <= 24*time.Hour && !i.ObservedAt.After(at.Add(time.Minute)) && !b.NativeCreatedAt.After(i.ObservedAt) && !b.NativeCreatedAt.Before(window.Since) && !i.ObservedAt.Before(window.Since) && cfg.NativeValidationInCurrentDestination(reg, b)
}

func nativeAdmissionInputs(snapshot agentapi.SourceSnapshot, input agentapi.NativeInput, filter agentapi.TranscriptFilter) (agentapi.VerifiedContentSnapshot, agentapi.LeasedTranscriptFilter, agentapi.SourceAdmissionEvidence, bool) {
	content, ok := snapshot.(agentapi.VerifiedContentSnapshot)
	leased, concrete := filter.(agentapi.LeasedTranscriptFilter)
	evidence, factsOK := snapshot.(agentapi.SourceAdmissionEvidence)
	valid := ok && concrete && factsOK && filter.Name() == "muse" && leased.LeasedFilterFor(filter) && content.ContentProofFor(snapshot, input) && input.File != nil
	return content, leased, evidence, valid
}

func nativeFilteredStartupMatches(filtered archive.FilteredTranscript, binding *archive.NativeSourceBinding) bool {
	return filtered.NativeStartComplete && filtered.NativeStartAt.Equal(binding.NativeCreatedAt) && len(filtered.SessionIDs) == 1 && filtered.SessionIDs[0] == binding.NativeID
}

func copyNativeAdmissionCandidate(candidate NativeAdmissionCandidate) (NativeAdmissionCandidate, error) {
	if !agentapi.ValidNativeAdmissionIntentShape(candidate.Intent) || candidate.Intent.NativeMode != agentapi.NativeStartupCandidate || candidate.Root.Agent != "muse" || candidate.Root.Validate() != nil || candidate.ProjectRoot != candidate.Intent.ProjectRoot || !validNativeAdmissionProjectID(candidate.ProjectID) {
		return NativeAdmissionCandidate{}, agentapi.ErrInvalidSourceBinding
	}
	raw, err := json.Marshal(candidate.Intent)
	if err != nil {
		return NativeAdmissionCandidate{}, err
	}
	// Supplemental root/project fields contain immutable bounded strings. Only
	// the intent owns pointers/slices requiring a defensive copy.
	copied := candidate
	if err := json.Unmarshal(raw, &copied.Intent); err != nil {
		return NativeAdmissionCandidate{}, err
	}
	return copied, nil
}

func validNativeAdmissionProjectID(id string) bool {
	return len(id) <= 4096 && strings.TrimSpace(id) != "" && utf8.ValidString(id) && !strings.ContainsAny(id, "\x00\r\n")
}
