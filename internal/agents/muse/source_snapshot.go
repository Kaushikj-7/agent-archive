package muse

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/sourceio"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

const proofScratch = 64 << 10

// Matches the existing shared 256 KiB source-header ceiling; it is aggregate,
// not a separate allowance for each of the supported startup records.
const startupHeaderBytes = 256 << 10

type sourcePass struct {
	env               agentapi.SourceEnvironment
	live              map[*sourceSnapshot]bool
	prepared          *sourceSnapshot
	preparedRef       agentapi.SourceRef
	admissionRequired bool
	closed            bool
	err               error
}

type sourceSnapshot struct {
	owner       *sourcePass
	ref         agentapi.SourceRef
	file        *transcriptio.Snapshot
	limits      agentapi.ReadLimits
	scratch     []byte
	reservation *agentapi.NativeReadReservation
	proof       agentapi.SourceContentProof
	closed      bool
	err         error
}

func (p *sourcePass) Read(ctx context.Context, ref agentapi.SourceRef, limits agentapi.ReadLimits) (agentapi.SourceSnapshot, error) {
	if p.closed {
		return nil, agentapi.ErrClosed
	}
	if len(p.live) != 0 {
		return nil, agentapi.Wrap(agentapi.Unavailable, errors.New("muse source snapshot is still borrowed"))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ref.Kind != archive.SourceKindFile {
		return nil, errFormat
	}
	f, err := transcriptio.Open(p.env.Files, ref.Path, p.env.Policy)
	if err != nil {
		return nil, sourceio.Classify(err)
	}
	s := &sourceSnapshot{owner: p, ref: ref, file: f, limits: limits, reservation: p.env.ReadBudget.Reservation()}
	p.live[s] = true
	rawLimit, _ := boundedNativeLimits(limits)
	if f.Length() > rawLimit {
		return nil, errors.Join(agentapi.RawLimit(f.Length(), rawLimit, nil), s.Close())
	}
	if !s.reservation.Reserve(proofScratch) {
		return nil, errors.Join(errBudget, s.Close())
	}
	s.scratch = make([]byte, proofScratch)
	digest, err := s.hash(ctx, f.Length())
	if err != nil {
		return nil, errors.Join(err, s.Close())
	}
	s.proof = agentapi.SourceContentProof{Version: 1, Provider: "muse-ordinary", Boundary: f.Length(), SHA256: digest}
	return s, nil
}

func (s *sourceSnapshot) hash(ctx context.Context, boundary int64) ([32]byte, error) {
	var out [32]byte
	if s.closed || s.owner.closed {
		return out, agentapi.ErrClosed
	}
	if boundary < 0 || boundary > s.file.Length() {
		return out, transcriptio.ErrChanged
	}
	h := sha256.New()
	for offset := int64(0); offset < boundary; {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		n, err := s.file.ReadAt(s.scratch[:min(int64(len(s.scratch)), boundary-offset)], offset)
		if n > 0 {
			_, _ = h.Write(s.scratch[:n])
			offset += int64(n)
		}
		if err != nil && (!errors.Is(err, io.EOF) || offset != boundary) {
			return out, sourceio.Classify(err)
		}
		if n == 0 {
			return out, io.ErrUnexpectedEOF
		}
	}
	copy(out[:], h.Sum(nil))
	return out, nil
}

func (s *sourceSnapshot) ContentProofFor(snapshot agentapi.SourceSnapshot, in agentapi.NativeInput) bool {
	actual, ok := snapshot.(*sourceSnapshot)
	file, owned := in.File.(snapshotInput)
	return ok && actual == s && owned && file.snapshot == s && in.Records == nil && in.SubagentMeta == nil && !s.closed && !s.owner.closed
}

func (s *sourceSnapshot) ContentProof(ctx context.Context) (agentapi.SourceContentProof, error) {
	if err := s.VerifyContent(ctx); err != nil {
		return agentapi.SourceContentProof{}, err
	}
	return s.proof, nil
}

func (s *sourceSnapshot) VerifyContent(ctx context.Context) error {
	if s.closed || s.owner.closed {
		return agentapi.ErrClosed
	}
	return sourceio.Classify(s.file.CheckPrefixBuffer(ctx, s.proof.Boundary, s.proof.SHA256, s.scratch))
}

func strongSignature(proof agentapi.SourceContentProof) agentapi.SourceSignature {
	boundary := proof.Boundary
	if boundary < 0 {
		return agentapi.SourceSignature{}
	}
	var token [41]byte
	token[0] = 1 // SHA256 of the complete captured raw boundary.
	binary.BigEndian.PutUint64(token[1:9], uint64(boundary))
	copy(token[9:], proof.SHA256[:])
	return agentapi.SourceSignature{Version: 1, Provider: "file", Token: base64.RawURLEncoding.EncodeToString(token[:])}
}

func (s *sourceSnapshot) Observation() agentapi.SourceObservation {
	stamp := s.file.Stamp()
	return agentapi.SourceObservation{Present: true, Empty: s.proof.Boundary == 0, Size: s.proof.Boundary, Activity: stamp.ModifiedAt, Signature: strongSignature(s.proof)}
}

func (s *sourceSnapshot) Input() agentapi.NativeInput {
	return agentapi.NativeInput{File: snapshotInput{s}}
}

func (s *sourceSnapshot) Close() error {
	if s.closed {
		return s.err
	}
	s.closed = true
	delete(s.owner.live, s)
	if s.scratch != nil {
		s.reservation.Release(proofScratch)
		s.scratch = nil
	}
	s.err = agentapi.Wrap(agentapi.Cleanup, s.file.Close())
	return s.err
}

type snapshotInput struct{ snapshot *sourceSnapshot }

func (f snapshotInput) Length() int64 { return f.snapshot.file.Length() }

func (f snapshotInput) Stamp() transcriptio.Stamp { return f.snapshot.file.Stamp() }

func (f snapshotInput) ReadAt(b []byte, off int64) (int, error) {
	if f.snapshot.closed || f.snapshot.owner.closed {
		return 0, agentapi.ErrClosed
	}
	return f.snapshot.file.ReadAt(b, off)
}

func (f snapshotInput) Check() error {
	if f.snapshot.closed || f.snapshot.owner.closed {
		return agentapi.ErrClosed
	}
	return sourceio.Classify(f.snapshot.file.Check())
}

func (f snapshotInput) Records(ctx context.Context, tail bool, window, record int64, visit func([]byte) bool) (transcriptio.RecordWindow, error) {
	if f.snapshot.closed || f.snapshot.owner.closed {
		return transcriptio.RecordWindow{}, agentapi.ErrClosed
	}
	return f.snapshot.file.Records(ctx, tail, window, record, visit)
}

// startupFacts reads the physical prefix using the same strict grammar and root
// predicates as Filter. All transient private payloads are charged and discarded.
func (s *sourceSnapshot) startupFacts(ctx context.Context) (archive.NativeSourceBinding, error) {
	var empty archive.NativeSourceBinding
	if s.closed || s.owner.closed {
		return empty, agentapi.ErrClosed
	}
	reservation := s.owner.env.ReadBudget.Reservation()
	if !reservation.Reserve(4096) {
		return empty, errBudget
	}
	held := int64(4096)
	defer func() { reservation.Release(held) }()
	reader := bufio.NewReaderSize(io.NewSectionReader(s.file, 0, min(s.file.Length(), startupHeaderBytes)), 4096)
	_, limit := boundedNativeLimits(s.limits)
	rows := make([]retained, 0, 8)
	joins := nativeBodyJoins{}
	for line := int64(1); line <= 7; line++ {
		raw, charge, err := physicalLine(ctx, reader, limit, reservation)
		held += charge
		if err != nil {
			return empty, err
		}
		// Reserve decoder/private JSON expansion before interpreting raw bytes.
		private := int64(len(raw)) * 128
		if !reservation.Reserve(private) {
			return empty, errBudget
		}
		held += private
		decoded, _, err := decodeNativeRows(raw[:len(raw)-1], line, &joins)
		if err != nil {
			return empty, err
		}
		for _, row := range decoded {
			if row.ordinary == nil {
				return empty, errFormat
			}
			rows = append(rows, *row.ordinary)
		}
		needed := 4
		if len(rows) >= 3 && rows[2].Type == payloadRuntimeSessionCaptureSelection {
			needed = 8
		}
		if len(rows) >= 4 && rows[2].Type == payloadRuntimeSessionMetadata && rows[3].Type == payloadRuntimeSessionMetadata {
			needed = 5
		}
		if len(rows) < needed {
			continue
		}
		if err := validateRootRows(rows); err != nil {
			return empty, err
		}
		cwd := ""
		for _, r := range rows {
			if r.Type == payloadRuntimeSessionMetadata {
				cwd = str(r.Fields["workspace_root"])
			}
		}
		return archive.NativeSourceBinding{Agent: "muse", Version: 2, NativeID: rows[0].Stream.ID, NativeCreatedAt: time.UnixMicro(rows[0].Timestamp).UTC(), Cwd: cwd, StreamID: rows[0].Stream.ID, Path: s.ref.Path}, nil
	}
	return empty, errFormat
}

func (s *sourceSnapshot) AdmissionEvidence(ctx context.Context, admission agentapi.SourceAdmission) (agentapi.AdmissionEvidence, error) {
	return s.admissionEvidence(ctx, admission, false)
}

func (s *sourceSnapshot) admissionEvidence(ctx context.Context, admission agentapi.SourceAdmission, warm bool) (agentapi.AdmissionEvidence, error) {
	var empty agentapi.AdmissionEvidence
	previous, err := nativePreviousAdmission(admission)
	if err != nil {
		return empty, err
	}
	facts, err := s.startupFacts(ctx)
	if err != nil {
		return empty, err
	}
	if previous == nil {
		return s.firstBindingEvidence(ctx, admission, facts)
	}

	if root := s.owner.env.NativeRoot; root != nil {
		if root.Validate() != nil || root.Agent != facts.Agent || root.Revision != previous.RootRevision || !ordinaryRootLocator(root.Canonical, facts.Path) {
			return empty, agentapi.ErrInvalidSourceBinding
		}
	}
	facts.RootRevision = previous.RootRevision
	facts.Checkpoint = previous.Checkpoint
	// Path identity/layout is admission-only: arbitrary explicit files still read.
	if filepath.Base(facts.Path) != "session.jsonl" || filepath.Base(filepath.Dir(facts.Path)) != facts.NativeID || !ordinaryRootLocator(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(facts.Path))))), facts.Path) || facts.NativeID != admission.NativeID || !facts.PreservesFacts(previous) || !previous.PreservesFacts(&facts) || admission.Cwd != "" && admission.Cwd != facts.Cwd || !admission.NativeCreatedAt.IsZero() && !admission.NativeCreatedAt.Equal(facts.NativeCreatedAt) {
		return empty, agentapi.ErrInvalidSourceBinding
	}
	old := previous.Checkpoint
	digest, err := hex.DecodeString(old.SHA256)
	if err != nil {
		return empty, agentapi.ErrInvalidSourceBinding
	}
	var sum [32]byte
	copy(sum[:], digest)
	// Only exact recognized durable equality can share Signature's subsequent
	// independent whole-boundary verification. Changed bytes still prove the
	// previous prefix here, and cannot advance authority on the signature route.
	if !warm || old.Boundary != s.proof.Boundary || sum != s.proof.SHA256 {
		if err := s.file.CheckPrefixBuffer(ctx, old.Boundary, sum, s.scratch); err != nil {
			return empty, sourceio.Classify(err)
		}
	}
	if err := s.file.Check(); err != nil {
		return empty, sourceio.Classify(err)
	}
	facts.Checkpoint = &archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: s.proof.Boundary, SHA256: hex.EncodeToString(s.proof.SHA256[:])}
	return agentapi.AdmissionEvidence{Binding: agentapi.SourceBinding{Native: &facts}}, nil
}

func (p *sourcePass) ValidateSourceAdmission(ctx context.Context, ref agentapi.SourceRef, admission agentapi.SourceAdmission) error {
	if admission.Binding != nil {
		p.admissionRequired = true
	}
	if p.prepared != nil {
		if err := p.prepared.Close(); err != nil {
			return err
		}
		p.prepared = nil
	}
	snap, err := p.Read(ctx, ref, agentapi.ReadLimits{})
	if err != nil {
		return err
	}
	s := snap.(*sourceSnapshot)
	if _, err = s.admissionEvidence(ctx, admission, true); err != nil {
		return errors.Join(err, s.Close())
	}
	p.prepared, p.preparedRef = s, ref
	return nil
}

func (p *sourcePass) Signature(ctx context.Context, ref agentapi.SourceRef) (out agentapi.SourceObservation, err error) {
	s := p.prepared
	if s != nil {
		p.prepared = nil
		if p.preparedRef != ref {
			return out, errors.Join(agentapi.ErrInvalidSourceBinding, s.Close())
		}
	} else {
		if p.admissionRequired {
			return out, agentapi.Wrap(agentapi.Unavailable, agentapi.ErrInvalidSourceBinding)
		}
		snap, e := p.Read(ctx, ref, agentapi.ReadLimits{})
		if e != nil {
			return out, e
		}
		s = snap.(*sourceSnapshot)
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	if err = s.VerifyContent(ctx); err != nil {
		return out, err
	}
	return s.Observation(), nil
}

func (p *sourcePass) Close() error {
	if p.closed {
		return p.err
	}
	for s := range p.live {
		p.err = errors.Join(p.err, s.Close())
	}
	p.closed = true
	p.prepared = nil
	return p.err
}

func nativePreviousAdmission(admission agentapi.SourceAdmission) (*archive.NativeSourceBinding, error) {
	if admission.NativeID == "" {
		return nil, agentapi.Wrap(agentapi.Unavailable, agentapi.ErrInvalidSourceBinding)
	}
	if admission.Binding == nil {
		return nil, nil
	}
	if admission.Binding.Codex != nil || admission.Binding.Native == nil {
		return nil, agentapi.Wrap(agentapi.Unavailable, agentapi.ErrInvalidSourceBinding)
	}
	previous := admission.Binding.Native
	if previous.Version != 2 || previous.Validate() != nil {
		return nil, agentapi.Wrap(agentapi.Unavailable, agentapi.ErrInvalidSourceBinding)
	}
	return previous, nil
}
func (s *sourceSnapshot) firstBindingEvidence(ctx context.Context, admission agentapi.SourceAdmission, facts archive.NativeSourceBinding) (agentapi.AdmissionEvidence, error) {
	var empty agentapi.AdmissionEvidence
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	root := s.owner.env.NativeRoot
	opener, confined := s.owner.env.Files.(transcriptio.RootOpener)
	if root == nil || root.Validate() != nil || root.Agent != "muse" || !confined || opener.Root != root.Canonical || s.owner.env.Policy.Root != root.Canonical || !s.owner.env.Policy.RejectSymlinks || !ordinaryRootLocator(root.Canonical, facts.Path) || filepath.Base(filepath.Dir(facts.Path)) != facts.NativeID || facts.NativeID != admission.NativeID || s.ref.Key != admission.NativeID || admission.Cwd != "" && admission.Cwd != facts.Cwd || !admission.NativeCreatedAt.IsZero() && !admission.NativeCreatedAt.Equal(facts.NativeCreatedAt) {
		return empty, agentapi.ErrInvalidSourceBinding
	}
	facts.RootRevision = root.Revision
	facts.Checkpoint = &archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: s.proof.Boundary, SHA256: hex.EncodeToString(s.proof.SHA256[:])}
	if err := s.file.Check(); err != nil {
		return empty, err
	}
	return agentapi.AdmissionEvidence{Binding: agentapi.SourceBinding{Native: &facts}}, nil
}
