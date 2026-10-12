package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

// BeginJournalPublication transfers only the exact origin-bound frozen owner.
// The journal descriptor must already be durable before this admission CAS.
// The completed result authorizes replay of local acknowledgement, never writes.
func (s *Store) BeginJournalPublication(ctx context.Context, id string, raw []byte, journal local.CatalogJournal, guard *local.CollectorGuard) (context.Context, bool, error) {
	if journal.Validate() != nil || journal.MutationID != id {
		return nil, false, ErrAdmissionClosed
	}
	release, err := guard.ClaimJournal(journal.Origin, journal.Owner)
	if err != nil {
		return nil, false, err
	}
	claimed := false
	defer func() {
		if !claimed {
			release()
		}
	}()
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.destinationID == "" || s.destinationID != journal.Destination || s.running[id] {
		return nil, false, ErrAdmissionClosed
	}
	if err = ctx.Err(); err != nil {
		return nil, false, err
	}
	home, err := guard.Home()
	if err != nil {
		return nil, false, err
	}
	if err = state.OpenReadOnly(home).VerifyPersistedCatalogJournal(journal, raw, guard, s.destinationID); err != nil {
		return nil, false, err
	}
	if err = ctx.Err(); err != nil {
		return nil, false, err
	}
	refs, digest, err := publicationAdmission(raw)
	if err != nil {
		return nil, false, err
	}
	invocation, err := NewMutationID()
	if err != nil {
		return nil, false, err
	}
	owner := "pending/" + id + "/" + invocation
	completed := false
	err = s.Writer.Coordinator().change(ctx, func(state *admissions) error {
		completed = false
		if state.Hold != "" || state.GCLink != nil {
			return ErrAdmissionClosed
		}
		if receipt, ok := state.Completed[journal.Owner]; ok {
			if !sameJournalAdmission(receipt, journal, digest, refs) {
				return ErrMutationReuse
			}
			completed = true
			return nil
		}
		priorOwner := ""
		for active, admission := range state.Owners {
			if strings.HasPrefix(active, "pending/"+id+"/") {
				if priorOwner != "" || !sameJournalAdmission(admission, journal, digest, refs) {
					return ErrAdmissionClosed
				}
				priorOwner = active
			}
		}
		if priorOwner == "" && (state.Seal != "" || len(state.Owners) >= 256) {
			return ErrAdmissionClosed
		}
		// A pre-seal exact owner retains the identical complete protection set
		// throughout transfer. A sealed destination never gains a new owner.
		delete(state.Owners, priorOwner)
		state.Owners[owner] = admission{Digest: digest, Refs: refs, Journal: &journal}
		return nil
	})
	if err != nil {
		return ctx, completed, err
	}
	if s.pending == nil {
		s.pending = map[string]string{}
		s.running = map[string]bool{}
		s.claims = map[string]*publicationClaim{}
		s.owners = map[string]string{}
	}
	claim := &publicationClaim{replayOnly: completed, store: s, id: id, owner: owner, digest: digest, refs: refs, journal: &journal, guard: guard, release: release}
	s.pending[id], s.running[id], s.claims[id], s.owners[id] = digest, true, claim, owner
	claimed = true
	return context.WithValue(ctx, publicationClaimKey{}, claim), completed, nil
}

func sameJournalAdmission(admitted admission, journal local.CatalogJournal, digest string, refs []ObjectRef) bool {
	return admitted.Journal != nil && *admitted.Journal == journal && admitted.Digest == digest && slices.Equal(admitted.Refs, refs)
}

func (s *Store) completeJournal(ctx context.Context, claim *publicationClaim) error {
	if err := s.verifyJournalCommit(ctx, claim); err != nil {
		return err
	}
	return s.Writer.Coordinator().change(ctx, func(state *admissions) error {
		if state.Hold != "" || state.GCLink != nil {
			return ErrAdmissionClosed
		}
		admitted, ok := state.Owners[claim.owner]
		if !ok || !sameJournalAdmission(admitted, *claim.journal, claim.digest, claim.refs) {
			return ErrAdmissionClosed
		}
		if len(state.Completed) >= 256 {
			return errors.New("catalog completed journal capacity exceeded")
		}
		if state.Completed == nil {
			state.Completed = map[string]admission{}
		}
		state.Completed[claim.journal.Owner] = admitted
		delete(state.Owners, claim.owner)
		return nil
	})
}

func (s *Store) verifyJournalCommit(ctx context.Context, claim *publicationClaim) error {
	head, _, err := s.Writer.Head(ctx)
	if err != nil {
		return err
	}
	raw, err := s.Writer.lookup(ctx, head.Receipts, claim.id)
	if err != nil {
		return err
	}
	var committed receipt
	if len(raw) == 0 || json.Unmarshal(raw, &committed) != nil || committed.MetadataSHA256 != claim.digest || committed.Revision == "" || !strings.HasSuffix(committed.SessionKey, "/"+claim.journal.SessionID+"/metadata.json") {
		return errors.New("catalog journal has no exact verified final commit")
	}
	return nil
}

// AcknowledgeJournalRemoval removes only the exact receipt with positive local
// tombstone/unlink proof. Missing journals and matching digests are insufficient.
func (s *Store) AcknowledgeJournalRemoval(ctx context.Context, proof *local.JournalRemoval, guard *local.CollectorGuard) error {
	journal, err := proof.Journal(guard)
	if err != nil {
		return err
	}
	done, err := guard.BeginOperation(journal.Origin)
	if err != nil {
		return err
	}
	defer done()
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.destinationID == "" || journal.Destination != s.destinationID {
		return ErrAdmissionClosed
	}
	return s.Writer.Coordinator().change(ctx, func(state *admissions) error {
		if state.Hold != "" || state.GCLink != nil {
			return ErrAdmissionClosed
		}
		receipt, ok := state.Completed[journal.Owner]
		if !ok {
			return nil // An exact durable removal proof also covers lost acknowledgment.
		}
		if receipt.Journal == nil || *receipt.Journal != journal {
			return ErrMutationReuse
		}
		delete(state.Completed, journal.Owner)
		return nil
	})
}

func completedReferences(state admissions) []ObjectRef {
	var refs []ObjectRef
	for _, receipt := range state.Completed {
		refs = append(refs, receipt.Refs...)
		refs = append(refs, ObjectRef{Key: "catalog-v4/metadata/" + receipt.Digest + ".json", SHA256: receipt.Digest})
	}
	slices.SortFunc(refs, func(a, b ObjectRef) int {
		if a.Key == b.Key {
			return strings.Compare(a.SHA256, b.SHA256)
		}
		return strings.Compare(a.Key, b.Key)
	})
	return slices.Compact(refs)
}
