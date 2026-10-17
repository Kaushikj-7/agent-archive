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
	defer directory.Close()
	entries, err := directory.ReadDir(128)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(p.local.Home(), "admission-intents", entry.Name())
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 16<<10+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(raw) > 16<<10 {
			continue
		}
		var intent agentapi.AdmissionIntent
		if json.Unmarshal(raw, &intent) != nil || !capture.RecognizedAdmissionIntent(intent) || intent.Version != 2 || !capture.MuseIntentEligible(cfg, intent, p.now) {
			continue
		}
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
			p.result.Errors["muse-startup:"+candidate.intent.NativeSessionID] = err
		}
	}
}

func (p *pass) admitStartupCandidate(candidate startupCandidate) (err error) {
	cfg, found, err := config.Load(p.local.Home())
	if err != nil || !found {
		return err
	}
	if !capture.MuseIntentEligible(cfg, candidate.intent, p.now) {
		return errors.New("Muse startup window changed")
	}
	key := agentmeta.SessionKey{Agent: agentmeta.Muse, NativeID: candidate.intent.NativeSessionID}
	if id, owned, err := p.local.ArchiveSessionID(key); err != nil {
		return err
	} else if owned {
		reg, found, err := p.local.LoadRegistration(id)
		if err != nil {
			return err
		}
		if found && reg.NativeAdmission != nil && *reg.NativeAdmission == *candidate.intent.NativeAdmission && reg.HookObservedAt.Equal(candidate.intent.ObservedAt) && reg.NativeBinding != nil && reg.NativeBinding.Version == 2 {
			return p.ackStartupCandidate(candidate)
		}
		return errors.New("Muse startup conflicts with existing owner")
	}
	result, err := p.opts.NativeLocators.Locate(p.ctx, agentapi.LocatorRequest{Purpose: agentapi.LocatorStartupCandidate, NativeID: candidate.intent.NativeSessionID, Root: candidate.root, Observed: candidate.intent.ObservedAt})
	if err != nil {
		return err
	}
	if result.Status != agentapi.LocatorFound || result.Current != nil || len(result.Candidates) != 1 || result.Revision == "" {
		return agentapi.Wrap(agentapi.Unavailable, errors.New("Muse startup requires complete unique inventory"))
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

func (p *pass) ackStartupCandidate(candidate startupCandidate) error {
	unlock, err := local.NamedLockWait(p.local.Home(), "admission-intents.lock", time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	var current agentapi.AdmissionIntent
	if err := local.Read(candidate.path, &current); err != nil {
		return err
	}
	a, _ := json.Marshal(current)
	b, _ := json.Marshal(candidate.intent)
	if string(a) != string(b) || !capture.RecognizedAdmissionIntent(current) {
		return errors.New("Muse startup changed before acknowledgement")
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
