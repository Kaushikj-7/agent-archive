package muse

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"maps"
	"reflect"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
)

const (
	fieldSkillContentSHA      fieldName = "sha256"
	fieldSkillContentEvidence fieldName = "hash_evidence"
)

type skillContentKey struct {
	stream   string
	id       string
	sequence int64
	line     int64
	child    int64
	hasChild bool
}

func skillContentIdentity(r retained) skillContentKey {
	child := int64(0)
	hasChild := r.Position.Child != nil
	if hasChild {
		child = int64(*r.Position.Child)
	}
	return skillContentKey{stream: r.Stream.ID, id: r.ID, sequence: r.Sequence, line: r.Position.Line, hasChild: hasChild, child: child}
}

// This object is owned by the existing raw filter release, never by the registry.
// Pending values are private native hashes; finalized records contain safe S only.
type privateSkillContent struct {
	digest agentapi.SkillContentDigest
	event  []byte // exact projected eight-field record, never the native payload
}

type skillContentJoin struct {
	reservation *agentapi.NativeReadReservation
	held        *int64
	limits      agentapi.ReadLimits
	pending     map[skillContentKey]privateSkillContent
	rows        []logicalRow
	out         archive.FilteredTranscript
	restored    bool
	finalized   bool
	closed      bool
}

func (Filter) SkillContentJoinFor(a archive.Adapter) bool { _, ok := a.(Filter); return ok }

func (f Filter) FilterSkillContentLeased(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext, budget *agentapi.NativeReadBudget) (archive.FilteredTranscript, agentapi.SkillContentJoin, func(), error) {
	var join *skillContentJoin
	out, release, err := f.filterLeased(ctx, in, c, budget, &join)
	if err != nil {
		return out, nil, release, err
	}
	return out, join, release, nil
}

func (s *skillContentJoin) charge(n int64) error {
	if s.closed || !s.reservation.Reserve(n) {
		return errBudget
	}
	*s.held += n
	return nil
}

func (s *skillContentJoin) capture(r retained, payload map[string]any) error {
	if r.Type != payloadRuntimeSession || str(r.Fields["event_kind"]) != string(eventSkillReadObserved) {
		return nil
	}
	value := str(object(payload["event"])["evidence_hash"])
	if !digest(value) {
		return errFormat
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		return errFormat
	}
	// Physical decoding still funds this scratch; transfer its exact encoded
	// capacity and bounded map/key/value overhead before persistent insertion.
	n := int64(cap(encoded)) + 512
	if s.pending == nil {
		n += 1024
	}
	if err := s.charge(n); err != nil {
		return err
	}
	if s.pending == nil {
		s.pending = make(map[skillContentKey]privateSkillContent)
	}
	var h agentapi.SkillContentDigest
	decoded, err := hex.DecodeString(value[len("sha256:"):])
	if err != nil {
		return errFormat
	}
	copy(h[:], decoded)
	clear(decoded)
	k := skillContentIdentity(r)
	if _, exists := s.pending[k]; exists {
		return errFormat
	}
	s.pending[k] = privateSkillContent{digest: h, event: encoded}
	return nil
}

func (s *skillContentJoin) clearPending() {
	for k := range s.pending {
		s.pending[k] = privateSkillContent{}
	}
	clear(s.pending)
	s.pending = nil
}

func (s *skillContentJoin) close() {
	if s.closed {
		return
	}
	s.closed = true
	s.clearPending()
	s.rows, s.out = nil, archive.FilteredTranscript{}
}

func (s *skillContentJoin) Restore(ctx context.Context, prior archive.SourceBundle) (out archive.FilteredTranscript, err error) {
	defer func() {
		if err != nil {
			s.clearPending()
			s.finalized = true
		}
	}()
	if s.closed || s.restored || s.finalized {
		return archive.FilteredTranscript{}, agentapi.ErrClosed
	}
	s.restored = true
	if err := ctx.Err(); err != nil {
		s.clearPending()
		return archive.FilteredTranscript{}, err
	}
	if prior.SchemaVersion == 0 || len(s.pending) == 0 || !priorSkillContentEvent(prior) {
		return s.out, nil
	}
	// The collector separately binds the archive/project owner. Old privacy
	// versions have no supported proof to restore, and retain existing refresh policy.
	if prior.Capture.AdapterVersion != (Filter{}).Version() || prior.Capture.FilterVersion != archive.FilterVersion {
		return s.out, nil
	}
	if prior.NativeSessionID != s.out.LocalIdentity.ID {
		return archive.FilteredTranscript{}, errFormat
	}
	rows, release, err := s.priorRows(ctx, prior)
	if err != nil {
		return archive.FilteredTranscript{}, err
	}
	defer release()
	proofs, err := s.priorProofs(ctx, rows)
	if err != nil {
		return archive.FilteredTranscript{}, err
	}
	out, err = s.enrich(ctx, proofs)
	if err == nil {
		for k := range proofs {
			s.pending[k] = privateSkillContent{}
			delete(s.pending, k)
		}
	}
	return out, err
}

// This is only a no-allocation work gate, never proof validation. Previously
// hashless events must also be consumed so an active observation cannot rewrite
// a published prefix. Every matching event takes the strict validation below.
func priorSkillContentEvent(prior archive.SourceBundle) bool {
	for _, row := range prior.NativeRecords {
		f := object(row["fields"])
		if str(f["event_kind"]) == string(eventSkillReadObserved) {
			return true
		}
	}
	return false
}

func (s *skillContentJoin) priorRows(ctx context.Context, prior archive.SourceBundle) (rows []logicalRow, release func(), err error) {
	release = func() {}
	var n int64
	for _, row := range prior.NativeRecords {
		size, err := agentmeta.JSONWireBound(ctx, row, archive.MaxRecordBytes)
		if err != nil {
			return nil, release, err
		}
		multiplier := int64(8)
		if enrichedTools(object(row["fields"])) {
			multiplier = 128
		}
		n += size*multiplier + 512
		if n > s.reservation.Available() {
			return nil, release, errBudget
		}
	}
	if err = s.charge(n); err != nil {
		return nil, release, err
	}
	release = func() { s.reservation.Release(n); *s.held -= n; n = 0 }
	defer func() {
		if err != nil {
			release()
		}
	}()
	rows, err = retainedRows(ctx, prior)
	if err != nil {
		return nil, release, err
	}
	for i, row := range rows {
		if err = ctx.Err(); err != nil {
			return nil, release, err
		}
		safe, privacyErr := privacyLogical(row, i+1, func(string, int, string) {})
		if privacyErr != nil || !reflect.DeepEqual(row, safe) {
			return nil, release, errFormat
		}
	}
	if validateLogical(rows) != nil || !validOmissionGaps(rows, prior.Capture.Gaps) {
		return nil, release, errFormat
	}
	return rows, release, nil
}

func (s *skillContentJoin) priorProofs(ctx context.Context, rows []logicalRow) (map[skillContentKey]string, error) {
	proofs := map[skillContentKey]string{}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if row.ordinary == nil {
			continue
		}
		r := *row.ordinary
		if r.Type != payloadRuntimeSession || str(r.Fields["event_kind"]) != string(eventSkillReadObserved) {
			continue
		}
		k := skillContentIdentity(r)
		pending, found := s.pending[k]
		if !found {
			continue
		}
		if !sameSkillEvent(r, pending.event) {
			return nil, errFormat
		}
		hash := str(r.Fields["sha256"])
		if hash != "" && hex.EncodeToString(pending.digest[:]) != hash {
			return nil, errFormat
		}
		// Empty S deliberately consumes a previously hashless event without
		// enriching it. Only events absent from the validated prior baseline
		// remain eligible for a new current-observation proof.
		proofs[k] = hash
	}
	return proofs, nil
}

func sameSkillEvent(prior retained, expected []byte) bool {
	fields := make(map[string]any, len(prior.Fields)-2)
	for k, v := range prior.Fields {
		if fieldName(k) != fieldSkillContentSHA && fieldName(k) != fieldSkillContentEvidence {
			fields[k] = v
		}
	}
	prior.Fields = fields
	encoded, err := json.Marshal(prior)
	return err == nil && bytes.Equal(encoded, expected)
}

func (s *skillContentJoin) Finalize(ctx context.Context, safe []agentapi.SkillContentDigest) (archive.FilteredTranscript, error) {
	defer s.clearPending()
	if s.closed || !s.restored || s.finalized {
		s.finalized = true
		return archive.FilteredTranscript{}, agentapi.ErrClosed
	}
	s.finalized = true
	if err := ctx.Err(); err != nil {
		return archive.FilteredTranscript{}, err
	}
	if len(safe) == 0 || len(s.pending) == 0 {
		return s.out, nil
	}
	n := int64(len(safe))*128 + int64(len(s.pending))*128 + 2048
	if err := s.charge(n); err != nil {
		return archive.FilteredTranscript{}, err
	}
	defer func() { s.reservation.Release(n); *s.held -= n }()
	allowed := make(map[agentapi.SkillContentDigest]bool, len(safe))
	for _, d := range safe {
		allowed[d] = true
	}
	proofs := make(map[skillContentKey]string)
	for k, pending := range s.pending {
		if allowed[pending.digest] {
			proofs[k] = hex.EncodeToString(pending.digest[:])
		}
	}
	return s.enrich(ctx, proofs)
}

func (s *skillContentJoin) enrich(ctx context.Context, proofs map[skillContentKey]string) (archive.FilteredTranscript, error) {
	if len(proofs) == 0 {
		return s.out, nil
	}
	n := int64(len(s.rows)) * 24
	changed := 0
	for i, row := range s.rows {
		if row.ordinary != nil && proofs[skillContentIdentity(*row.ordinary)] != "" && str(row.ordinary.Fields["sha256"]) == "" {
			changed++
			n += int64(len(s.out.Records[i])+256)*8 + 1024
		}
	}
	if changed == 0 {
		return s.out, nil
	}
	if err := s.charge(n); err != nil {
		return archive.FilteredTranscript{}, err
	}
	// Old encodings remain conservatively funded until the one output release;
	// there are at most two transitions and no live probe borrows at this point.
	out := s.out
	out.Records = append([][]byte(nil), out.Records...)
	for i, row := range s.rows {
		if err := ctx.Err(); err != nil {
			return archive.FilteredTranscript{}, err
		}
		if row.ordinary == nil {
			continue
		}
		h := proofs[skillContentIdentity(*row.ordinary)]
		if h == "" || str(row.ordinary.Fields["sha256"]) != "" {
			continue
		}
		r := *row.ordinary
		r.Fields = make(map[string]any, len(row.ordinary.Fields)+2)
		maps.Copy(r.Fields, row.ordinary.Fields)
		r.Fields["sha256"], r.Fields["hash_evidence"] = strings.Clone(h), skillContentEvidence
		if !skillReadFields(r) {
			return archive.FilteredTranscript{}, errFormat
		}
		safe, err := privacyLogical(ordinaryRow(r), i+1, func(string, int, string) {})
		if err != nil {
			return archive.FilteredTranscript{}, err
		}
		raw, err := json.Marshal(safe)
		if err != nil {
			return archive.FilteredTranscript{}, errFormat
		}
		out.Boundary.RetainedBytes += len(raw) - len(out.Records[i])
		ceiling := s.limits.FilteredBytes
		if ceiling <= 0 {
			ceiling = archive.MaxRecordBytes
		}
		if int64(out.Boundary.RetainedBytes) > ceiling {
			return archive.FilteredTranscript{}, archive.ErrRecordTooLarge
		}
		out.Records[i] = raw
		s.rows[i] = safe
	}
	s.out = out
	return out, nil
}
