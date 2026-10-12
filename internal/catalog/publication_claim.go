package catalog

import (
	"context"
	"errors"
	"slices"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type publicationClaimKey struct{}

type publicationClaim struct {
	store      *Store
	id         string
	owner      string
	digest     string
	refs       []ObjectRef
	journal    *local.CatalogJournal
	guard      *local.CollectorGuard
	release    func()
	replayOnly bool
}

// Caller holds pendingMu through its complete subordinate operation so End
// cannot retire authority while source/history/publication bytes are staged.
func (s *Store) publicationClaim(ctx context.Context) (*publicationClaim, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	claim, ok := ctx.Value(publicationClaimKey{}).(*publicationClaim)
	if !ok || claim.replayOnly || claim.store != s || !s.running[claim.id] || s.claims[claim.id] != claim {
		return nil, ErrAdmissionClosed
	}
	if claim.journal != nil {
		origin, err := claim.guard.Origin()
		if err != nil || origin != claim.journal.Origin {
			return nil, ErrAdmissionClosed
		}
	}
	state, _, err := s.Writer.Coordinator().read(ctx)
	if err != nil {
		return nil, err
	}
	owner, ok := state.Owners[claim.owner]
	if !ok || owner.Digest != claim.digest || state.Hold != "" || len(owner.Refs) != len(claim.refs) {
		return nil, ErrAdmissionClosed
	}
	if claim.journal != nil && (owner.Journal == nil || *owner.Journal != *claim.journal) {
		return nil, ErrAdmissionClosed
	}
	for i, ref := range owner.Refs {
		if ref != claim.refs[i] {
			return nil, ErrAdmissionClosed
		}
	}
	return claim, nil
}

func (s *Store) putClaimedSource(ctx context.Context, key string, raw []byte) error {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	claim, err := s.publicationClaim(ctx)
	if err != nil {
		return err
	}
	ref := ObjectRef{key, storage.SHA256Hex(raw)}
	if !slices.Contains(claim.refs, ref) {
		return errors.New("source is outside frozen publication admission")
	}
	_, err = s.Writer.conditional.PutConditional(ctx, key, raw, storage.PutCondition{CreateOnly: true})
	if err == nil {
		return nil
	}
	existing, readErr := s.Writer.bounded.GetLimited(ctx, key, int64(len(raw)))
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	if string(existing) != string(raw) {
		return storage.ErrChecksumMismatch
	}
	return ctx.Err()
}

func (s publicationStore) putClaimedMetadata(ctx context.Context, key string, raw []byte, entry *CatalogEntry) error {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	claim, err := s.publicationClaim(ctx)
	if err != nil {
		return err
	}
	if claim.id != s.id || storage.SHA256Hex(raw) != claim.digest {
		return ErrMutationReuse
	}
	ctx = context.WithValue(ctx, writeAuthorityKey{}, writeAuthority{writer: s.Writer, owner: claim.owner, digest: claim.digest})
	entry.Metadata, err = s.Writer.putImmutableAdmitted(ctx, KindMetadata, raw)
	if err != nil {
		return err
	}
	_, err = s.Writer.commitAdmitted(ctx, CatalogMutation{ID: s.id, SessionKey: key, ExpectedRevision: s.expected, Next: entry})
	return err
}
