package collector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

type preparedNativeValidation struct {
	snapshot    agentapi.SourceSnapshot
	content     agentapi.VerifiedContentSnapshot
	input       agentapi.NativeInput
	binding     *archive.NativeSourceBinding
	previous    *archive.NativeSourceBinding
	root        archive.NativeRoot
	proof       agentapi.SourceContentProof
	selection   func(context.Context) error
	closePass   func() error
	reservation *agentapi.NativeReadReservation
	scratch     []byte
	closed      bool
	committed   bool
	err         error
}

func (r providerReader) prepareNativeValidation(ctx context.Context, snapshot agentapi.SourceSnapshot, filter agentapi.TranscriptFilter, in agentapi.NativeInput, binding *archive.NativeSourceBinding, closePass func() error) (*preparedNativeValidation, error) {
	content, ok := snapshot.(agentapi.VerifiedContentSnapshot)
	concrete, owns := filter.(agentapi.LeasedTranscriptFilter)
	if !ok || !owns || !concrete.LeasedFilterFor(filter) || filter.Name() != "muse" || !content.ContentProofFor(snapshot, in) || in.File == nil || r.nativeRoot == nil || r.resourceOwner == nil || r.resourceOwner.local == nil {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	var previous *archive.NativeSourceBinding
	if r.admission.Binding != nil {
		previous = r.admission.Binding.Native
	}
	if !binding.AdvancesCheckpoint(previous) {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	selection := func(context.Context) error { return nil }
	if previous == nil {
		if r.nativeSelection == "" || r.nativeLocators == nil {
			return nil, errors.New("new native binding requires selected current locator authority")
		}
		selection = func(ctx context.Context) error {
			return r.nativeLocators.Check(ctx, r.admission.NativeID, r.nativeSelection)
		}
	}
	proof, err := content.ContentProof(ctx)
	if err != nil {
		return nil, err
	}
	cp := binding.Checkpoint
	if proof.Version != cp.Version || proof.Provider != cp.Domain || proof.Boundary != cp.Boundary || hex.EncodeToString(proof.SHA256[:]) != cp.SHA256 {
		return nil, agentapi.ErrInvalidSourceBinding
	}
	// Canonical physical project attribution happens before staging or hooks.lock.
	facts, known := sourcefacts.PhysicalProject(binding.Cwd)
	if !known || facts.Root != r.resourceOwner.reg.ProjectRoot {
		return nil, errors.New("native workspace contradicts admitted physical project")
	}
	bindingCopy := *binding
	checkpointCopy := *binding.Checkpoint
	bindingCopy.Checkpoint = &checkpointCopy
	binding = &bindingCopy
	if previous != nil {
		previousCopy := *previous
		priorCopy := *previous.Checkpoint
		previousCopy.Checkpoint = &priorCopy
		previous = &previousCopy
	}
	p := &preparedNativeValidation{snapshot: snapshot, content: content, input: in, binding: binding, previous: previous, root: *r.nativeRoot, proof: proof, selection: selection, closePass: closePass, reservation: r.resourceOwner.readBudget().Reservation()}
	if !p.reservation.Reserve(64 << 10) {
		return nil, agentapi.ReadBudgetLimit(errors.New("native validation scratch unavailable"))
	}
	p.scratch = make([]byte, 64<<10)
	return p, nil
}

func (p *preparedNativeValidation) verify(ctx context.Context) error {
	if p.closed || !p.content.ContentProofFor(p.snapshot, p.input) {
		return agentapi.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.previous != nil {
		old := p.previous.Checkpoint
		if old == nil || old.Validate() != nil || old.Boundary > p.input.File.Length() {
			return agentapi.ErrInvalidSourceBinding
		}
		h := sha256.New()
		for offset := int64(0); offset < old.Boundary; {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, err := p.input.File.ReadAt(p.scratch[:min(int64(len(p.scratch)), old.Boundary-offset)], offset)
			if n > 0 {
				_, _ = h.Write(p.scratch[:n])
				offset += int64(n)
			}
			if err != nil && (!errors.Is(err, io.EOF) || offset != old.Boundary) {
				return err
			}
			if n == 0 {
				return io.ErrUnexpectedEOF
			}
		}
		if hex.EncodeToString(h.Sum(nil)) != old.SHA256 {
			return agentapi.Wrap(agentapi.Changed, errors.New("native validated prefix changed"))
		}
	}
	if err := p.content.VerifyContent(ctx); err != nil {
		return err
	}
	return p.selection(ctx)
}

func (p *preparedNativeValidation) close() error {
	if p == nil {
		return nil
	}
	if p.closed {
		return p.err
	}
	p.closed = true
	p.err = errors.Join(p.snapshot.Close(), p.closePass())
	if p.scratch != nil {
		p.reservation.Release(64 << 10)
		p.scratch = nil
	}
	return p.err
}

func (s *sessionScan) closeNativeLeases() error {
	var err error
	for _, p := range s.nativeLeases {
		err = errors.Join(err, p.close())
	}
	s.nativeLeases = nil
	return err
}

func (s *sessionScan) persistNativeValidation(p *preparedNativeValidation) (err error) {
	if p == nil || p.committed {
		return nil
	}
	defer func() { err = errors.Join(err, p.close()) }()
	updated, err := s.local.CommitNativeValidation(s.ctx, s.reg, p.root, p.binding, p.verify)
	if err != nil {
		return err
	}
	p.committed = true
	s.reg = updated
	return nil
}
