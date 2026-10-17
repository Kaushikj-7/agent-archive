package state

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

// NativeAdmissionCandidate contains original observed authority, never an owner.
type NativeAdmissionCandidate struct {
	Intent      agentapi.AdmissionIntent
	Root        archive.NativeRoot
	ProjectRoot string
	ProjectID   string
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
	content, ok := snapshot.(agentapi.VerifiedContentSnapshot)
	leased, concrete := filter.(agentapi.LeasedTranscriptFilter)
	evidence, factsOK := snapshot.(agentapi.SourceAdmissionEvidence)
	if s.hook || !ok || !concrete || !factsOK || filter.Name() != "muse" || !leased.LeasedFilterFor(filter) || !content.ContentProofFor(snapshot, input) || lookup == nil || selection == "" || closePass == nil || budget == nil || input.File == nil {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	raw, err := json.Marshal(candidate)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &candidate); err != nil {
		return nil, err
	}
	observed := candidate.Intent
	if observed.Version != 2 || observed.NativeMode != agentapi.NativeStartupCandidate || observed.Harness != "muse" || observed.NativeAdmission == nil || observed.NativeAdmission.Validate() != nil || observed.NativeOwnerID != "" || len(observed.Effects) != 0 || observed.TranscriptPath != "" || observed.Replay != nil || observed.LastHead != nil || observed.Event != "SessionStart" || observed.ProjectRoot != candidate.ProjectRoot || observed.ObservedCwd == "" {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	facts, err := evidence.AdmissionEvidence(ctx, agentapi.SourceAdmission{NativeID: observed.NativeSessionID})
	if err != nil {
		return nil, err
	}
	binding := facts.Binding.Native
	if facts.Binding.Codex != nil || binding == nil || binding.Version != 2 || binding.Validate() != nil || binding.NativeID != observed.NativeSessionID || binding.RootRevision != candidate.Root.Revision || binding.Agent != "muse" || binding.ParentID != "" || binding.RootID != "" && binding.RootID != binding.NativeID {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	physical, known := sourcefacts.PhysicalProject(binding.Cwd)
	if !known || physical.Root != candidate.ProjectRoot {
		return nil, errors.New("native source does not belong to intended physical project")
	}
	now := time.Now().UTC()
	reg := archive.SessionRegistration{NativeSessionID: observed.NativeSessionID, Harness: archive.Harness{Name: "muse"}, NativeAdmission: observed.NativeAdmission, NativeBinding: binding, TranscriptPath: binding.Path, ProjectRoot: candidate.ProjectRoot, ProjectID: candidate.ProjectID, DestinationID: observed.DestinationID, SessionStartedAt: binding.NativeCreatedAt, RegisteredAt: now, AdmittedAt: now, HookObservedAt: observed.ObservedAt, Origin: archive.SessionOriginHook, StartedAtSource: archive.StartedAtSourceTranscript}
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
	if !filtered.NativeStartComplete || !filtered.NativeStartAt.Equal(binding.NativeCreatedAt) || len(filtered.SessionIDs) != 1 || filtered.SessionIDs[0] != binding.NativeID {
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
	// Identity allocation is in memory only and follows full source proof.
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: binding.NativeID}
	if err := s.generationReservationAllowed(key); err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	if _, owned, err := s.ArchiveSessionID(key); err != nil || owned {
		if release != nil {
			release()
		}
		return nil, errors.New("native startup identity is already owned or unresolved")
	}
	p.index, p.indexFound, err = s.readQualifiedIndex(key)
	if err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	if p.indexFound && !p.index.Absent {
		registered, e := s.matchingRegistration(key, p.index.ArchiveSessionID)
		if e != nil || registered || p.index.Reservation == "" {
			if release != nil {
				release()
			}
			return nil, errors.New("native startup already has an owner or conflicting index")
		}
		p.reg.ArchiveSessionID = p.index.ArchiveSessionID
	} else {
		if err := s.sessionIndexMissAllowed(); err != nil {
			if release != nil {
				release()
			}
			return nil, err
		}
		p.reg.ArchiveSessionID, err = local.ID()
	}
	if err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	if err := p.reg.Validate(); err != nil {
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
	if err := p.content.VerifyContent(ctx); err != nil {
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
	if !found || !cfg.NativeAdmissionProtection || !cfg.NativeAuthorityProtection || cfg.Paused || window == nil || !cfg.Archive.Enabled || !slices.Contains(cfg.Harnesses, "muse") || cfg.DestinationID() != i.DestinationID || cfg.PauseGeneration != i.PauseGeneration || !nativeDestinationConfigured(cfg.Storage) || at.Sub(i.ObservedAt) > 24*time.Hour || i.ObservedAt.After(at.Add(time.Minute)) || b.NativeCreatedAt.After(i.ObservedAt) || b.NativeCreatedAt.Before(window.Since) || i.ObservedAt.Before(window.Since) || b.NativeCreatedAt.Before(cfg.DestinationSince) {
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
	err := indexStore.writeUnderLock(lockedWrite{path: qualifiedSessionIndexPath(s.home, key), lock: lock, beforeLock: func() error { return p.verify(ctx) }, check: func() error { return p.checkPolicy(ctx, time.Now().UTC()) }, change: func(current fileSnapshot) (any, bool, error) {
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

func (p *nativeAdmissionCommit) check(previous, next archive.SessionRegistration, found bool) error {
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
	if p == nil || p.closed {
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
	p.err = errors.Join(p.snapshot.Close(), p.closePass())
	return p.err
}
