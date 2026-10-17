package collector

import (
	"context"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// publicationTransactionStore borrows every metadata response under the same
// pass ledger. It neither substitutes native bytes nor fabricates validators.
type publicationTransactionStore struct {
	storage.ObjectStore
	scan    *sessionScan
	pending *state.PendingPublication
}

// publicationAttempt owns the actual provider for one serial attempt. Ambient
// scan fields cannot retarget its reads, writes or closing proof.
type publicationAttempt struct {
	provider        storage.ObjectStore
	catalogID       string
	catalogKey      string
	catalogRevision string
	catalogProtocol uint64
	store           storage.ObjectStore
	resolvedCommit  *state.PublicationCommit
	resolvedKey     string
	resolved        [65]bool
	verifiedCommit  *state.PublicationCommit
	verifiedKey     string
	baselineCommit  *state.PublicationCommit
}

func (s *sessionScan) beginPublicationAttempt() (func(), error) {
	if s.publicationAttempt != nil {
		return func() {}, nil
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	budget := s.readBudget().Reservation()
	if !budget.Reserve(4096) {
		return nil, errRetainedBudget
	}
	frame := &publicationAttempt{provider: s.remote, store: s.remote}
	s.publicationAttempt = frame
	return func() {
		if s.publicationAttempt == frame {
			s.publicationAttempt = nil
			budget.Release(4096)
		}
	}, nil
}

// sourceSetProvider is the immutable provider captured before any scoped view.
func (s *sessionScan) sourceSetProvider() storage.ObjectStore {
	if frame := s.publicationAttempt; frame != nil {
		if frame.provider != nil {
			return frame.provider
		}
		return frame.store
	}
	return s.remote
}

// bindPublicationAttempt derives an admitted catalog view only from the captured
// provider. Nested entries cannot retarget the attempt through ambient fields.
func (s *sessionScan) bindPublicationAttempt(p state.PendingPublication) error {
	frame := s.publicationAttempt
	if frame == nil {
		return state.ErrDurableStorageRecovery
	}
	if p.Catalog == nil {
		if frame.catalogID != "" {
			return state.ErrCatalogJournalFrozen
		}
		return nil
	}
	if frame.catalogID != "" {
		if frame.catalogID != p.Catalog.ID || frame.catalogKey != p.MetadataKey || frame.catalogRevision != p.Catalog.ExpectedRevision || frame.catalogProtocol != p.Catalog.Protocol {
			return state.ErrCatalogJournalFrozen
		}
		return nil
	}
	remote, ok := s.sourceSetProvider().(storage.CatalogPublisher)
	if !ok || !remote.CatalogMetadataAuthority() {
		return state.ErrCatalogJournalFrozen
	}
	frame.store = remote.Publication(p.Catalog.ID, p.MetadataKey, p.Catalog.ExpectedRevision)
	frame.catalogID, frame.catalogKey = p.Catalog.ID, p.MetadataKey
	frame.catalogRevision, frame.catalogProtocol = p.Catalog.ExpectedRevision, p.Catalog.Protocol
	return nil
}

func (s *sessionScan) sourceSetRemote() storage.ObjectStore {
	if s.publicationAttempt != nil {
		return s.publicationAttempt.store
	}
	return s.remote
}

func (s publicationTransactionStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.scan.historyGet(key, limit)
}

func (s publicationTransactionStore) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	return s.scan.historyGetRevision(ctx, key, limit)
}

func (s *sessionScan) historyGetRevision(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	getter, ok := s.sourceSetRemote().(storage.LimitedVersionedGetter)
	if !ok {
		return nil, "", storage.ErrVersionedReadUnavailable
	}
	if limit <= 0 || limit > historyMetadataLimit || !strings.HasSuffix(key, "/metadata.json") {
		return nil, "", storage.ErrObjectTooLarge
	}
	// Exact final length is known at C. The provider checks it before allocation;
	// its opaque validator belongs to this SAME response, not a later HEAD.
	budget := s.readBudget().Reservation()
	if !budget.Reserve(limit + 4096) {
		return nil, "", errRetainedBudget
	}
	body, etag, err := getter.GetLimitedVersioned(ctx, key, limit)
	if err != nil || int64(len(body)) > limit || len(etag) > 4096 {
		budget.Release(limit + 4096)
		if err == nil {
			err = storage.ErrObjectTooLarge
		}
		return nil, "", err
	}
	if err = ctx.Err(); err != nil {
		budget.Release(limit + 4096)
		return nil, "", err
	}
	charge := int64(len(body) + len(etag))
	budget.Release(limit + 4096 - charge)
	s.retainedReleases = append(s.retainedReleases, func() { budget.Release(charge) })
	return body, etag, nil
}

// publicationClosingReadback is the single-use D proof. Only the actual closing
// GET after reference/listing/ledger work can mint it in this session frame.
type publicationClosingReadback struct {
	scan     *sessionScan
	frame    *publicationAttempt
	key      string
	commit   state.PublicationCommit
	consumed bool
}

func (s *sessionScan) closePublicationReadback(p state.PendingPublication) (*publicationClosingReadback, error) {
	if err := s.local.ValidateRetentionRestoration(s.reg, p); err != nil {
		return nil, err
	}
	if s.publicationAttempt == nil || p.Commit == nil || s.publicationAttempt.verifiedCommit == nil || *s.publicationAttempt.verifiedCommit != *p.Commit || s.publicationAttempt.verifiedKey != p.MetadataKey || p.Phase != state.PublicationReady || p.ValidatePublicationBudgeted(s.ctx, s.readBudget()) != nil {
		return nil, state.ErrDurableStorageRecovery
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := s.historyGet(p.MetadataKey, historyMetadataLimit)
	if err != nil {
		return nil, err
	}
	if len(raw) != len(p.MetadataBytes) || storage.SHA256Hex(raw) != p.Commit.MetadataSHA256 {
		return nil, storage.ErrPublicationConflict
	}
	if err = s.checkHistoryPublicationBody(p, raw); err != nil {
		return nil, err
	}
	return &publicationClosingReadback{scan: s, frame: s.publicationAttempt, key: p.MetadataKey, commit: *p.Commit}, nil
}

func (r *publicationClosingReadback) consume(s *sessionScan, p state.PendingPublication) error {
	return r.consumeSelection(s, p, false)
}

func (r *publicationClosingReadback) consumeBaselineOwed(s *sessionScan, p state.PendingPublication) error {
	return r.consumeSelection(s, p, true)
}

func (r *publicationClosingReadback) consumeSelection(s *sessionScan, p state.PendingPublication, baseline bool) error {
	if r == nil || r.consumed || r.scan != s || r.frame == nil || r.frame != s.publicationAttempt || p.Commit == nil || r.key != p.MetadataKey || r.commit != *p.Commit {
		return storage.ErrPublicationConflict
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if err := s.local.ValidateRetentionRestoration(s.reg, p); err != nil {
		return err
	}
	if err := s.checkHistoryPublicationLocal(p); err != nil {
		return err
	}
	// This is already a ready sealed transaction, so sealPending only checks
	// the current adapter/policy/destination/admission and performs no write.
	if baseline {
		if p.History == nil || !p.History.MaintenanceOwed || p.History.Preparing || r.frame.baselineCommit == nil || *r.frame.baselineCommit != *p.Commit || p.Commit.DestinationID != s.reg.DestinationID || p.Commit.AdmissionContext != s.publicationAdmission() || !baselineSkillPoliciesSupported(s.opts.skillEvidence(), pendingSkillMode(p.SkillEvidence)) {
			return state.ErrDurableStorageRecovery
		}
		adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
		if err != nil || adapter.Version() == "" || archive.FilterVersion == "" {
			return state.ErrDurableStorageRecovery
		}
		oldPolicy := state.PublicationPolicy{FilterVersion: p.Bundle.Capture.FilterVersion, AdapterVersion: p.Bundle.Capture.AdapterVersion, SkillEvidence: pendingSkillMode(p.SkillEvidence)}
		if p.Commit.PolicyContext != oldPolicy.Context() {
			return state.ErrDurableStorageRecovery
		}
	} else if err := s.sealPending(&p); err != nil {
		return err
	}
	if s.reg.CaptureFrozen {
		if err := s.local.FrozenGeneration(s.reg); err != nil {
			return err
		}
	} else if err := s.local.GenerationCaptureAllowed(s.reg); err != nil {
		return err
	}
	if err := p.ValidatePublicationBudgeted(s.ctx, s.readBudget()); err != nil {
		return err
	}
	r.consumed = true
	return nil
}

var (
	_ storage.LimitedGetter          = publicationTransactionStore{}
	_ storage.LimitedVersionedGetter = publicationTransactionStore{}
)

func (s publicationTransactionStore) ValidatePublicationPosition(ctx context.Context, key string, raw []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.pending == nil || s.pending.MetadataKey != key {
		return storage.ErrPublicationConflict
	}
	if err := s.scan.local.ValidateRetentionRestoration(s.scan.reg, *s.pending); err != nil {
		return err
	}
	return s.scan.checkHistoryPublicationBody(*s.pending, raw)
}

// Filter/adapter strings have no monotonic ordering. Baseline acknowledgement
// makes no upgrade claim; only the concrete skill ceiling has an order.
// baselineSkillPoliciesSupported validates policy names for acknowledgement of
// exact bytes already remote. Both tightening and loosening may acknowledge
// that baseline; retained transformation still enforces each original ceiling.
func baselineSkillPoliciesSupported(current, frozen config.SkillEvidence) bool {
	known := func(p config.SkillEvidence) bool {
		switch p {
		case config.SkillEvidenceNone, config.SkillEvidenceMetadata, config.SkillEvidenceBody:
			return true
		default:
			return false
		}
	}
	return known(current) && known(frozen)
}
