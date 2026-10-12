package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/codexmeta"
)

func (p *relatedSourcePass) genericLegacy(ref agentapi.SourceRef) bool {
	return p.env.LegacyUnboundRegistration && codexmeta.RolloutID(ref.Path) == "" && ref.Key != "" && codexmeta.RolloutID(ref.Key+".jsonl") != ref.Key
}

func (p *relatedSourcePass) registeredLegacySet(ctx context.Context, ref agentapi.SourceRef) (agentapi.CodexRolloutSet, error) {
	lookup, ok := p.env.CodexRollouts.(agentapi.CodexRegisteredRolloutLookup)
	if !ok {
		return agentapi.CodexRolloutSet{}, sourceFailure(agentapi.Unavailable, "registered locator lookup unavailable")
	}
	set, err := lookup.RegisteredThread(ctx, ref.Key)
	if err == nil && set.Current != nil {
		return set, sourceFailure(agentapi.Changed, "legacy current locator changed")
	}
	return set, err
}

func (p *relatedSourcePass) genericLegacySignature(ctx context.Context, ref agentapi.SourceRef) (agentapi.SourceObservation, error) {
	set, err := p.registeredLegacySet(ctx, ref)
	if err != nil {
		return agentapi.SourceObservation{}, err
	}
	observation, err := p.legacy.Signature(ctx, ref)
	if err != nil {
		return observation, err
	}
	sum := sha256.Sum256([]byte(observation.Signature.Token + "\x00" + set.LineageToken()))
	observation.Signature.Token = hex.EncodeToString(sum[:])
	return observation, p.env.CodexRollouts.Check(ctx, ref.Key, set.Revision)
}

func (p *relatedSourcePass) genericLegacyRead(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	set, err := p.registeredLegacySet(ctx, ref)
	if err != nil {
		return nil, err
	}
	snapshot, err := p.legacy.Read(ctx, ref, limits)
	if err != nil {
		return nil, err
	}
	if err := p.validateGenericLegacyInput(snapshot.Input()); err != nil {
		return nil, errors.Join(err, snapshot.Close())
	}
	if err := p.env.CodexRollouts.Check(ctx, ref.Key, set.Revision); err != nil {
		return nil, errors.Join(err, snapshot.Close())
	}
	return &registeredLegacySnapshot{SourceSnapshot: snapshot, owner: p, ref: ref, revision: set.Revision, lineage: set.LineageToken(), ctx: ctx}, nil
}

type registeredLegacySnapshot struct {
	agentapi.SourceSnapshot
	owner    *relatedSourcePass
	ref      agentapi.SourceRef
	revision string
	// lineage is the token genericLegacySignature hashes, so the signature a
	// read records matches the next pass's Signature of an unchanged source.
	lineage string
	ctx     context.Context
}

func (s *registeredLegacySnapshot) Observation() agentapi.SourceObservation {
	observation := s.SourceSnapshot.Observation()
	sum := sha256.Sum256([]byte(observation.Signature.Token + "\x00" + s.lineage))
	observation.Signature.Token = hex.EncodeToString(sum[:])
	return observation
}

func (s *registeredLegacySnapshot) Input() agentapi.NativeInput {
	input := s.SourceSnapshot.Input()
	if input.File != nil {
		input.File = registeredLegacyInput{FileInput: input.File, snapshot: s}
	}
	return input
}

type registeredLegacyInput struct {
	agentapi.FileInput
	snapshot *registeredLegacySnapshot
}

func (f registeredLegacyInput) Check() error {
	if err := f.FileInput.Check(); err != nil {
		return err
	}
	return f.snapshot.owner.env.CodexRollouts.Check(f.snapshot.ctx, f.snapshot.ref.Key, f.snapshot.revision)
}

// Preserve the already admitted legacy representation rather than fabricating
// a modern binding from optional/missing legacy producer or task evidence. The
// wrapped ordinary input still checks its exact prefix and real lookup token.
type legacyOrdinarySnapshot struct{ agentapi.SourceSnapshot }

// RelocatedFrom reports the rollout this snapshot read when the lookup found
// the registered rollout renamed: the same physical rollout ID and thread,
// at another locator the source pass opened under its confinement policy.
func (s legacyOrdinarySnapshot) RelocatedFrom(registered agentapi.SourceRef) (agentapi.SourceRef, bool) {
	ordinary, ok := s.SourceSnapshot.(*ordinarySnapshot)
	if !ok || ordinary.source == nil {
		return agentapi.SourceRef{}, false
	}
	read := ordinary.source.ref
	id := ordinary.source.identity
	rollout := codexmeta.RolloutID(registered.Path)
	moved := read.Path != registered.Path && rollout != "" && id.RolloutID == rollout && id.ThreadID == registered.Key
	return read, moved
}

func (s legacyOrdinarySnapshot) ValidateAdmission(ctx context.Context, a agentapi.SourceAdmission) error {
	if a.Binding != nil || !a.NativeCreatedAt.IsZero() {
		return sourceFailure(agentapi.Unavailable, "legacy compatibility requires unbound admission")
	}
	return s.SourceSnapshot.(agentapi.SourceAdmissionValidator).ValidateAdmission(ctx, a)
}

func (p *relatedSourcePass) validateGenericLegacyInput(input agentapi.NativeInput) error {
	if input.File == nil {
		return sourceFailure(agentapi.Unsafe, "legacy source is not a file")
	}
	if !p.reserve(sourceHeaderCharge) {
		return sourceFailure(agentapi.Limit, "legacy metadata scratch budget exhausted")
	}
	defer p.release(sourceHeaderCharge)
	raw := make([]byte, min(input.File.Length(), int64(64<<10)+1))
	n, err := input.File.ReadAt(raw, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	line, _, _ := bytes.Cut(raw[:n], []byte("\n"))
	if len(line) > 64<<10 {
		return sourceFailure(agentapi.Limit, "legacy metadata record limit")
	}
	meta, _, found, err := codexmeta.ParseCodexMeta(line)
	if err != nil {
		return sourceFailure(agentapi.FormatMismatch, "invalid legacy metadata")
	}
	if found {
		id, outcome := meta.Relationships()
		if outcome != "" || meta.HistoryMode != "" || id.Child || id.ForkID != "" || id.HistoryBase != nil {
			return sourceFailure(agentapi.Unavailable, "legacy source requires native selection")
		}
		// A generic hook fixture can omit metadata altogether. Understood
		// metadata still must describe ordinary local execution when present.
		if len(meta.Source) > 0 && !meta.LocalExecutionSource() {
			return sourceFailure(agentapi.Unsafe, "legacy execution source unsupported")
		}
	} else if len(bytes.TrimSpace(line)) > 0 && !json.Valid(line) {
		return sourceFailure(agentapi.FormatMismatch, "invalid legacy file")
	}
	return input.File.Check()
}
