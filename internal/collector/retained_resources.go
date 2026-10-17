package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

var errRetainedBudget = agentapi.ReadBudgetLimit(errors.New("retained work exceeds shared data budget"))

func (s *sessionScan) readBudget() *agentapi.NativeReadBudget {
	if s.opts.sourcePasses != nil {
		return s.opts.sourcePasses.env.ReadBudget
	}
	if s.retainedBudget == nil {
		if shared, ok := s.opts.CodexRollouts.(agentapi.CodexRolloutResourceBudget); ok {
			s.retainedBudget = shared.NativeReadBudget()
		}
		if s.retainedBudget == nil {
			s.retainedBudget = agentapi.NewNativeReadBudget(agentapi.PassReadBudget)
		}
	}
	return s.retainedBudget
}

// releaseRetained ends the scan's ownership of retained logical data. Scratch
// used only during a bounded operation is released by that operation instead.
func (s *sessionScan) releaseRetained() {
	s.releaseRetainedAfter(0)
}

// releaseRetainedAfter releases only work created after a scope marker. A caller
// must not use any data owned by the scope after returning.
func (s *sessionScan) releaseRetainedAfter(mark int) {
	for i := len(s.retainedReleases) - 1; i >= mark; i-- {
		s.retainedReleases[i]()
	}
	s.retainedReleases = s.retainedReleases[:mark]
}

func (s *sessionScan) retainCharge(n int64) error {
	b := s.readBudget()
	reservation := b.Reservation()
	if !reservation.Reserve(n) {
		return errRetainedBudget
	}
	s.retainedReleases = append(s.retainedReleases, func() { reservation.Release(n) })
	return nil
}

func (s *sessionScan) historyGet(key string, limit int64) ([]byte, error) {
	b := s.readBudget()
	reservation := b.Reservation()
	// Sidecars are mutable: stat provides a bounded allocation ceiling, never
	// authority. Exact bytes/schema/predecessor checks still run after GET.
	if statter, ok := s.sourceSetRemote().(storage.ObjectStatter); ok && strings.HasSuffix(key, "/metadata.json") {
		info, err := statter.Stat(s.ctx, key)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, err
		}
		// Missing stat is not the final mutable-object read: keep the original
		// bounded GET preflight so a provider/race cannot hide newly present data.
		if err == nil {
			if info.Size <= 0 || info.Size > limit {
				return nil, storage.ErrObjectTooLarge
			}
			limit = info.Size
		}
	}
	if limit <= 0 || limit > historyCompressedLimit {
		return nil, errRetainedBudget
	}
	if !reservation.Reserve(limit) {
		return nil, errRetainedBudget
	}
	data, err := historyLimitedGet(s.ctx, s.sourceSetRemote(), key, limit)
	if err != nil {
		reservation.Release(limit)
		return nil, err
	}
	n := int64(len(data))
	reservation.Release(limit - n)
	s.retainedReleases = append(s.retainedReleases, func() { reservation.Release(n) })
	return data, nil
}

func (s *sessionScan) historyStage(stage state.PendingSource) ([]byte, error) {
	n := int64(stage.Reference.CompressedBytes)
	if n <= 0 || n > historyCompressedLimit {
		return nil, errRetainedBudget
	}
	b := s.readBudget()
	reservation := b.Reservation()
	if !reservation.Reserve(n) {
		return nil, errRetainedBudget
	}
	data, err := s.local.ReadPendingSource(s.id(), stage)
	if err != nil {
		reservation.Release(n)
		return nil, err
	}
	s.retainedReleases = append(s.retainedReleases, func() { reservation.Release(n) })
	return data, nil
}

func (s *sessionScan) decodeReferenced(metadata archive.Metadata, data []byte) (archive.SourceBundle, error) {
	bundle, release, err := reader.DecodeReferencedSourceLeased(s.ctx, metadata, data, reader.Limits{}, s.readBudget())
	if err == nil {
		s.retainedReleases = append(s.retainedReleases, release)
	}
	return bundle, err
}

func (s *sessionScan) decodeRevision(metadata archive.Metadata, revision string, data []byte) (archive.SourceBundle, error) {
	bundle, release, err := reader.DecodeRevisionSourceLeased(s.ctx, metadata, revision, data, reader.Limits{}, s.readBudget())
	if err == nil {
		s.retainedReleases = append(s.retainedReleases, release)
	}
	return bundle, err
}

// refilterRetained reserves each overlapping representation separately. The
// input-row encoder buffer, its returned row, the scanner and parsed input row
// coexist with accumulated encoded output, the safe encoder buffer and decoded
// output. Heap object headers and allocator capacity are measured separately.
func (s *sessionScan) refilterRetained(adapter archive.Adapter, bundle archive.SourceBundle) (archive.SourceBundle, error) {
	return s.refilterRetainedFor(s.reg, adapter, bundle)
}

func (s *sessionScan) refilterRetainedFor(reg archive.SessionRegistration, adapter archive.Adapter, bundle archive.SourceBundle) (archive.SourceBundle, error) {
	filter, ok := adapter.(agentapi.TranscriptFilter)
	if !ok {
		return archive.SourceBundle{}, errRetainedBudget
	}
	out, release, err := agentapi.RefilterRetainedSource(s.ctx, reg, filter, bundle, s.readBudget())
	if err == nil {
		s.retainedReleases = append(s.retainedReleases, release)
	}
	return out, err
}

// compressSource keeps the canonical codec and charges output before each
// append. Line encoding has two owners: json.Marshal's encoder and result.
func (s *sessionScan) compressSource(bundle archive.SourceBundle) (archive.CompressedSource, error) {
	packed, release, err := agentapi.CompressRetainedSource(s.ctx, bundle, s.readBudget())
	if err == nil {
		s.retainedReleases = append(s.retainedReleases, release)
	}
	return packed, err
}

func (s *sessionScan) marshalRetained(value any) ([]byte, error) {
	b := s.readBudget()
	reservation := b.Reservation()
	const scratch = 32 << 10
	if !reservation.Reserve(scratch) {
		return nil, errRetainedBudget
	}
	available := reservation.Available()
	n, err := agentmeta.JSONWireBound(s.ctx, value, available/2)
	if errors.Is(err, agentmeta.ErrJSONWireLimit) {
		reservation.Refuse(available + 2)
	}
	reservation.Release(scratch)
	if err != nil {
		return nil, errors.Join(errRetainedBudget, err)
	}
	if !reservation.Reserve(n) {
		return nil, errRetainedBudget
	}
	defer reservation.Release(n)
	if !reservation.Reserve(n) {
		return nil, errRetainedBudget
	}
	data, err := json.Marshal(value)
	if err != nil {
		reservation.Release(n)
		return nil, err
	}
	actual := int64(len(data))
	if actual > n {
		reservation.Release(n)
		return nil, errRetainedBudget
	}
	reservation.Release(n - actual)
	s.retainedReleases = append(s.retainedReleases, func() { reservation.Release(actual) })
	return data, nil
}

func (s *sessionScan) unmarshalRetained(data []byte, value any) error {
	mark := len(s.retainedReleases)
	if err := s.retainCharge(int64(len(data))); err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		s.releaseRetainedAfter(mark)
		return err
	}
	return nil
}

// newSourceBundle reserves decoded native records before conversion. Text and
// supplemental inputs keep their existing ownership; no duplicate text copy is
// charged merely because the bundle aliases immutable filtered text.
func (s *sessionScan) newSourceBundle(reg archive.SessionRegistration, adapter archive.Adapter, filtered archive.FilteredTranscript, at time.Time, supplemental []archive.SupplementalEvidence) (archive.SourceBundle, error) {
	var n int64
	for _, row := range filtered.Records {
		if int64(len(row)) > s.readBudget().Available()-n {
			s.readBudget().Refuse(n + int64(len(row)))
			return archive.SourceBundle{}, errRetainedBudget
		}
		n += int64(len(row))
	}
	mark := len(s.retainedReleases)
	if err := s.retainCharge(n); err != nil {
		return archive.SourceBundle{}, err
	}
	bundle, err := archive.NewSourceBundle(reg, adapter, filtered, at, supplemental)
	if err != nil {
		s.releaseRetainedAfter(mark)
	}
	return bundle, err
}

func (s *sessionScan) jsonEncodingsEqual(a, b any) (bool, error) {
	mark := len(s.retainedReleases)
	defer s.releaseRetainedAfter(mark)
	left, err := s.marshalRetained(a)
	if err != nil {
		return false, err
	}
	right, err := s.marshalRetained(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}

func (s *sessionScan) bundleEvidenceEqual(a, b archive.SourceBundle) (bool, error) {
	return bundleEvidenceEqualWith(a, b, s.jsonEncodingsEqual)
}

// releaseRetainedIndex ends one independent owner without disturbing later
// leases. Its slot remains a no-op so enclosing scope markers stay stable.
func (s *sessionScan) releaseRetainedIndex(index int) {
	release := s.retainedReleases[index]
	s.retainedReleases[index] = func() {}
	release()
}

func (s *sessionScan) chooseRefreshSource(bundle archive.SourceBundle, uploaded archive.SourceReference, known bool) (refreshSource, bool, error) {
	compressed, err := s.compressSource(bundle)
	if err != nil {
		if errors.Is(err, agentapi.ErrReadBudget) {
			return refreshSource{}, false, err
		}
		return refreshSource{ref: uploaded}, known, nil
	}
	key, keyErr := archive.SourceObjectKey(bundle, compressed.SHA256)
	validKey := keyErr == nil
	if !validKey {
		return refreshSource{ref: uploaded}, known, nil
	}
	ref := archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
	return refreshSource{ref: ref, bytes: compressed.Bytes}, true, nil
}

// detachRetainedEnvelope gives a returned filtered bundle its own small envelope
// before an input scope ends. Native maps retain their existing decoded lease.
func (s *sessionScan) detachRetainedEnvelope(bundle archive.SourceBundle) (archive.SourceBundle, error) {
	detached, release, err := agentapi.DetachRetainedEnvelope(s.ctx, bundle, s.readBudget())
	if err == nil {
		s.retainedReleases = append(s.retainedReleases, release)
	}
	return detached, err
}

// nativeFilterLease identifies the one output owner installed by a successful
// injected leased native filter. Unleased adapters install no owner here.
func (s *sessionScan) nativeFilterLease(mark int) *int {
	if len(s.retainedReleases) == mark+1 {
		return &mark
	}
	return nil
}

// finishNativeFilter ends ordinary encoded-row ownership after every build and
// rewrite guard has consumed it. History, ordinal and text aliases retain their
// input lease. The returned ordinary envelope owns its auxiliary observations.
func (s *sessionScan) finishNativeFilter(read *sourceRead, candidate archive.SourceBundle) (archive.SourceBundle, error) {
	if read.filterLease == nil || candidate.History != nil || len(candidate.Ordinals) != 0 || read.filtered.History != nil || len(read.filtered.Ordinals) != 0 || len(read.filtered.Text) != 0 {
		return candidate, nil
	}
	detached, err := s.detachRetainedEnvelope(candidate)
	if err != nil {
		return archive.SourceBundle{}, err
	}
	read.filtered.Records = nil
	if s.filtered != nil && s.filtered.filterLease != nil && *s.filtered.filterLease == *read.filterLease {
		s.filtered = nil
	}
	if s.rewritten != nil {
		rewritten, err := s.detachRetainedEnvelope(*s.rewritten)
		if err != nil {
			return archive.SourceBundle{}, err
		}
		s.rewritten = &rewritten
	}
	s.releaseRetainedIndex(*read.filterLease)
	read.filterLease = nil
	return detached, nil
}

// keepRetainedFrom promotes independently owned output while ending input loans.
func (s *sessionScan) keepRetainedFrom(mark, keep int) {
	for i := mark; i < keep; i++ {
		s.releaseRetainedIndex(i)
	}
}

// finishHistoryFilter ends a history read's encoded-row ownership once build
// has decoded the candidate from it, before the candidate is reconciled and
// published: nothing on that path reads the encoded rows again, and holding
// them beside the candidate's decoded records would charge a long thread
// twice its size. The candidate gets its own envelope (history included);
// its ordinals and text still alias the read's, so they stay charged on
// their own. A read whose rows a rewrite guard still holds is left as it is.
func (s *sessionScan) finishHistoryFilter(read *sourceRead, candidate archive.SourceBundle) (archive.SourceBundle, error) {
	if read.filterLease == nil || candidate.History == nil || read.filtered.History == nil || s.rewritten != nil {
		return candidate, nil
	}
	aliased := 8 * int64(len(read.filtered.Ordinals))
	for _, text := range read.filtered.Text {
		aliased += int64(len(text))
	}
	if err := s.retainCharge(aliased); err != nil {
		return archive.SourceBundle{}, err
	}
	detached, err := s.detachRetainedEnvelope(candidate)
	if err != nil {
		return archive.SourceBundle{}, err
	}
	lease := *read.filterLease
	read.filtered.Records, read.filterLease = nil, nil
	if s.lastRead != nil && s.lastRead.filterLease != nil && *s.lastRead.filterLease == lease {
		s.lastRead.filtered.Records, s.lastRead.filterLease = nil, nil
	}
	if s.filtered != nil && s.filtered.filterLease != nil && *s.filtered.filterLease == lease {
		s.filtered = nil
	}
	s.releaseRetainedIndex(lease)
	return detached, nil
}
