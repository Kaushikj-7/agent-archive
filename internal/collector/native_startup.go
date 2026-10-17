package collector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/capture"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type startupCandidate struct {
	path      string
	intent    agentapi.AdmissionIntent
	root      archive.NativeRoot
	projectID string
}

func (p *pass) loadStartupCandidates() error {
	cfg, found, err := config.Load(p.local.Home())
	if err != nil || !found {
		return err
	}
	directory, err := os.Open(filepath.Join(p.local.Home(), "admission-intents"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	entries, err := directory.ReadDir(128)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(p.local.Home(), "admission-intents", entry.Name())
		intent, charge, loaded := p.readStartupIntent(path)
		if !loaded {
			continue
		}

		if !capture.MuseIntentEligible(cfg, intent, p.now) {
			p.startupCharge.Release(charge)
			p.result.Errors["muse-startup-window"] = errors.New("native startup captured window is no longer eligible")
			continue
		}
		p.startupChargeBytes += charge
		candidate := startupCandidate{path: path, intent: intent}
		for _, root := range cfg.NativeRoots {
			if root.Agent == "muse" {
				candidate.root = root
			}
		}
		for _, project := range cfg.Archive.Projects {
			if project.Root == intent.ProjectRoot && project.Included {
				candidate.projectID = project.ProjectID
			}
		}
		p.startupCandidates = append(p.startupCandidates, candidate)
	}
	return nil
}

func (p *pass) admitStartupCandidates() {
	for _, candidate := range p.startupCandidates {
		if p.ctx.Err() != nil {
			return
		}
		if p.opts.NativeLocators == nil {
			continue
		}
		if err := p.admitStartupCandidate(candidate); err != nil {
			p.result.Errors["muse-startup"] = agentapi.Wrap(agentapi.Unavailable, errors.New("native startup proof is unavailable or no longer matches captured authority"))
		}
	}
}

func (p *pass) admitStartupCandidate(candidate startupCandidate) (err error) {
	cfg, found, err := config.Load(p.local.Home())
	if err != nil || !found {
		return err
	}
	if !capture.MuseIntentEligible(cfg, candidate.intent, p.now) {
		return errors.New("muse startup window changed")
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: candidate.intent.NativeSessionID}
	if id, owned, err := p.local.ArchiveSessionID(key); err != nil {
		return err
	} else if owned {
		reg, found, err := p.local.LoadRegistration(id)
		if err != nil {
			return err
		}
		if found && startupOwnerMatches(cfg, id, reg, candidate) {
			return p.ackStartupCandidate(candidate)
		}
		return errors.New("muse startup conflicts with existing owner")
	}
	result, err := p.opts.NativeLocators.Locate(p.ctx, agentapi.LocatorRequest{Purpose: agentapi.LocatorStartupCandidate, NativeID: candidate.intent.NativeSessionID, Root: candidate.root, Observed: candidate.intent.ObservedAt})
	if err != nil {
		return err
	}
	if result.Status != agentapi.LocatorFound || result.Current != nil || len(result.Candidates) != 1 || result.Revision == "" {
		return agentapi.Wrap(agentapi.Unavailable, errors.New("muse startup requires complete unique inventory"))
	}
	ref := result.Candidates[0]
	if ref.Kind != archive.SourceKindFile || ref.Key != candidate.intent.NativeSessionID {
		return agentapi.ErrInvalidSourceBinding
	}
	provider, filter, ok := p.opts.Sources.LookupSources("muse")
	if !ok {
		return agentapi.ErrInvalidSourceBinding
	}
	budget := p.opts.sourcePasses.env.ReadBudget
	defer budget.BeginScope(sessionReadCeiling)()
	env := p.opts.sourcePasses.env
	env.NativeRoot = &candidate.root
	env.NativeLocators = p.opts.NativeLocators
	env.Files = sourcefacts.RootOpener{Root: candidate.root.Canonical}
	env.Policy = transcriptio.OpenPolicy{Root: candidate.root.Canonical, RejectSymlinks: true}
	nativePass, err := provider.OpenPass(p.ctx, env)
	if err != nil {
		return err
	}
	transferred := false
	defer func() {
		if !transferred {
			err = errors.Join(err, nativePass.Close())
		}
	}()
	limits := agentapi.ReadLimits{RawBytes: maxRawBytes("muse", p.opts.maxTranscriptBytes()), RecordBytes: recordLimit, FilteredBytes: p.opts.maxTranscriptBytes(), ReadBudget: budget}
	snapshot, err := nativePass.Read(p.ctx, ref, limits)
	if err != nil {
		return err
	}
	defer func() {
		if !transferred {
			err = errors.Join(err, snapshot.Close())
		}
	}()
	prepared, err := p.local.PrepareNativeAdmission(p.ctx, state.NativeAdmissionCandidate{Intent: candidate.intent, Root: candidate.root, ProjectRoot: candidate.intent.ProjectRoot, ProjectID: candidate.projectID}, snapshot, snapshot.Input(), filter, agentapi.FilterContext{Filename: ref.Path, Limits: limits}, p.opts.NativeLocators, result.Revision, budget, nativePass.Close)
	if err != nil {
		return err
	}
	transferred = true
	defer func() { err = errors.Join(err, prepared.Close()) }()
	reg, err := p.local.CommitNativeAdmission(p.ctx, prepared)
	if err != nil {
		return err
	}
	if err := p.ackStartupCandidate(candidate); err != nil {
		return err
	}
	// The ordinary scan consumes the borrowed retained output. It never calls
	// the native Filter again and owns all normal staging/PUT/readback/status work.
	p.opts.preparedStartup = &preparedStartupReader{prepared: prepared, observation: snapshot.Observation(), id: reg.ArchiveSessionID}
	defer func() { p.opts.preparedStartup = nil }()
	p.scan(p.ctx, reg)
	return nil
}

// A duplicate startup may have a later hook timestamp or another cwd within the
// same project. It may retire only its observation; the durable owner remains
// authoritative and no continuation effects are synthesized here.
func startupOwnerMatches(cfg config.Config, id string, reg archive.SessionRegistration, candidate startupCandidate) bool {
	i, b := candidate.intent, reg.NativeBinding
	if !sameMuseStartupReplay(reg.Replay, i.Replay) || reg.Validate() != nil || reg.ArchiveSessionID != id || reg.Harness.Name != "muse" || reg.NativeSessionID != i.NativeSessionID || reg.NativeAdmission == nil || i.NativeAdmission == nil || *reg.NativeAdmission != *i.NativeAdmission || b == nil || b.Version != 2 || b.Agent != "muse" || b.NativeID != i.NativeSessionID || b.RootRevision != candidate.root.Revision || reg.ProjectRoot != i.ProjectRoot || reg.ProjectID != candidate.projectID || reg.DestinationID != i.DestinationID || i.ObservedAt.Before(b.NativeCreatedAt) {
		return false
	}
	observed, observedOK := capture.ConfiguredProjectActivationFor(cfg, i.ObservedCwd)
	native, nativeOK := capture.ConfiguredProjectActivationFor(cfg, b.Cwd)
	return observedOK && nativeOK && observed.Included && native.Included && observed.Root == reg.ProjectRoot && native.Root == reg.ProjectRoot && observed.ProjectID == reg.ProjectID && native.ProjectID == reg.ProjectID && !native.ActivatedAt.IsZero() && !b.NativeCreatedAt.Before(native.ActivatedAt)
}

func (p *pass) ackStartupCandidate(candidate startupCandidate) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	unlock, err := local.NamedLockWait(p.local.Home(), "admission-intents.lock", time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	if err := p.ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(candidate.path)
	if err != nil {
		return err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, agentapi.MaxNativeAdmissionIntentBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	var current agentapi.AdmissionIntent
	if len(raw) > agentapi.MaxNativeAdmissionIntentBytes {
		return errors.New("native intent exceeds private wire bound")
	}
	if err := json.Unmarshal(raw, &current); err != nil {
		return err
	}

	a, _ := json.Marshal(current)
	b, _ := json.Marshal(agentapi.AdmissionIntent(candidate.intent))
	if string(a) != string(b) || !capture.RecognizedAdmissionIntent(current) {
		return errors.New("muse startup changed before acknowledgement")
	}
	if err := p.ctx.Err(); err != nil {
		return err
	}
	return os.Remove(candidate.path)
}

type preparedStartupReader struct {
	prepared    *state.PreparedNativeAdmission
	observation agentapi.SourceObservation
	id          string
}

func (r preparedStartupReader) Signature(ctx context.Context) (sourceState, error) {
	return observe(archive.SourceKindFile, r.observation), ctx.Err()
}

func (r preparedStartupReader) Filter(ctx context.Context, _ archive.Adapter, _ int64) (archive.FilteredTranscript, sourceState, error) {
	if err := ctx.Err(); err != nil {
		return archive.FilteredTranscript{}, sourceState{}, err
	}
	filtered, err := r.prepared.Filtered()
	return filtered, observe(archive.SourceKindFile, r.observation), err
}

func (p *pass) releaseStartupCandidates() {
	p.startupCandidates = nil
	if p.startupCharge != nil {
		p.startupCharge.Release(p.startupChargeBytes)
	}
	p.startupChargeBytes = 0
}

func (p *pass) readStartupIntent(path string) (intent agentapi.AdmissionIntent, charge int64, loaded bool) {
	file, err := os.Open(path)
	if err != nil {
		return intent, 0, false
	}
	info, statErr := file.Stat()
	// Retain decoded strings, struct/slice overhead and raw decode scratch
	// within the existing native read headroom, alongside 84 MiB sources and
	// the existing 8 MiB inventory plus bounded query snapshot.
	if statErr != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > agentapi.MaxNativeAdmissionIntentBytes {
		_ = file.Close()
		return intent, 0, false
	}
	charge = 6*info.Size() + 4096
	if p.startupCharge == nil {
		p.startupCharge = p.opts.sourcePasses.env.ReadBudget.Reservation()
	}
	if charge > (2<<20)-p.startupChargeBytes || !p.startupCharge.Reserve(charge) {
		_ = file.Close()
		p.result.Errors["muse-startup-budget"] = agentapi.ReadBudgetLimit(errors.New("native candidate metadata unavailable"))
		return intent, 0, false
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, info.Size()+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(raw)) != info.Size() {
		p.startupCharge.Release(charge)
		return intent, 0, false
	}

	if json.Unmarshal(raw, &intent) != nil || !capture.RecognizedAdmissionIntent(intent) || intent.Version != 2 || intent.NativeMode != agentapi.NativeStartupCandidate {
		p.startupCharge.Release(charge)
		return intent, 0, false
	}
	return intent, charge, true
}

// First-owner classification is immutable, including an ordinary nil marker.
// A conflicting later startup remains queued instead of being acknowledged.
func sameMuseStartupReplay(a, b *archive.Replay) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.RunID == b.RunID
}
