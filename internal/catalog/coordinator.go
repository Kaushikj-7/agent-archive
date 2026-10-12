package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// CoordinatorKey is the destination-wide durable admission authority. Catalog
// garbage collection must preserve it independently from tree reachability.
const CoordinatorKey = "catalog-v4/coordinator.json"

// ErrAdmissionClosed keeps all unacknowledged owners and the seal durable.
var ErrAdmissionClosed = errors.New("catalog admission sealed or owners not drained")

type admission struct {
	Digest  string                `json:"digest"`
	Refs    []ObjectRef           `json:"refs"`
	Journal *local.CatalogJournal `json:"journal,omitempty"`
}

type admissionMode string

const (
	admissionCandidate admissionMode = "candidate"
	admissionActive    admissionMode = "active"
	admissionRollback  admissionMode = "rollback"
)

type admissions struct {
	Intent     *migrationIntent     `json:"migration_intent,omitempty"`
	Protocol   uint64               `json:"protocol"`
	Mode       admissionMode        `json:"mode"`
	Proof      string               `json:"proof"`
	Generation uint64               `json:"generation"`
	Seal       string               `json:"seal"`
	Hold       string               `json:"hold"`
	GCReceipt  *gcCompletion        `json:"gc_receipt,omitempty"`
	GCLink     *gcLink              `json:"gc_link,omitempty"`
	Owners     map[string]admission `json:"owners"`
	Completed  map[string]admission `json:"completed,omitempty"`
}

// Coordinator uses the qualified provider's single CAS admission authority.
// Every mutation registers before writing and acknowledges only after its
// durable lifecycle settles. No timeout releases an unfinished owner or seal.
type Coordinator struct{ writer *Writer }

// Coordinator returns the writer's destination-wide admission authority.
func (w *Writer) Coordinator() *Coordinator { return &Coordinator{writer: w} }

func (c *Coordinator) read(ctx context.Context) (admissions, string, error) {
	raw, v, err := c.writer.versioned.GetCatalogVersion(ctx, CoordinatorKey, 4<<20)
	if errors.Is(err, storage.ErrNotFound) {
		return admissions{Protocol: 10, Mode: admissionCandidate, Owners: map[string]admission{}}, "", nil
	}
	if err != nil {
		return admissions{}, "", err
	}
	var state admissions
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 4<<20 || decoder.Decode(&state) != nil || decoder.Decode(new(any)) != io.EOF || state.validate() != nil || v.ETag == "" {
		return state, "", errors.New("invalid catalog coordinator")
	}
	return state, v.ETag, nil
}

func (c *Coordinator) change(ctx context.Context, fn func(*admissions) error) error {
	if c.writer.readOnly {
		return ErrReadOnly
	}
	for range 64 {
		state, etag, err := c.read(ctx)
		if err != nil {
			return err
		}
		if state.Intent != nil {
			authority, ok := ctx.Value(migrationIntentKey{}).(migrationIntentAuthority)
			if !ok || authority.writer != c.writer || authority.digest != intentDigest(state.Intent) {
				return ErrAdmissionClosed
			}
		}
		if err = fn(&state); err != nil {
			return err
		}
		state.Generation++
		if err = state.validate(); err != nil {
			return err
		}
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		if len(raw) > 4<<20 {
			return storage.ErrObjectTooLarge
		}
		_, err = c.writer.conditional.PutConditional(ctx, CoordinatorKey, raw, storage.PutCondition{MatchETag: etag, CreateOnly: etag == ""})
		if err == nil {
			return nil
		}
		if !errors.Is(err, storage.ErrPreconditionFailed) {
			return errors.Join(ErrCommitUnknown, err)
		}
	}
	return storage.ErrPreconditionFailed
}

// Admit registers exact frozen lifecycle input before any mutation. Identical
// owners are refused to prevent concurrent acknowledgement of another caller.
// A new owner cannot cross a seal, including after process restart.
func (c *Coordinator) Admit(ctx context.Context, owner, digest string, refs []ObjectRef) error {
	if owner == "" || len(owner) > 512 || digest == "" || len(refs) > 256 {
		return errors.New("invalid catalog admission")
	}
	for _, ref := range refs {
		if ref.Key == "" || ref.SHA256 == "" {
			return errors.New("incomplete catalog admission refs")
		}
	}
	return c.change(ctx, func(state *admissions) error {
		if _, ok := state.Owners[owner]; ok {
			return ErrAdmissionClosed
		}
		if state.Seal != "" {
			return ErrAdmissionClosed
		}
		if len(state.Owners) >= 256 {
			return errors.New("catalog admission owner capacity exceeded")
		}
		state.Owners[owner] = admission{Digest: digest, Refs: refs}
		return nil
	})
}

// Complete acknowledges a settled frozen owner. Callers must hold its durable
// pending authority until all history/preservation and publication work ends.
// A crash before acknowledgement intentionally blocks collection and cutover.
func (c *Coordinator) Complete(ctx context.Context, owner, digest string) error {
	return c.change(ctx, func(state *admissions) error {
		if state.Hold != "" {
			return ErrAdmissionClosed
		}
		prior, ok := state.Owners[owner]
		if !ok {
			return nil
		}
		if prior.Digest != digest {
			return ErrMutationReuse
		}
		if prior.Journal != nil {
			return ErrAdmissionClosed
		}
		delete(state.Owners, owner)
		return nil
	})
}

// Seal closes new admissions durably. Existing owners must acknowledge their
// frozen lifecycle before inventory is complete; references alone never prove
// stopped writers. The returned owner is required for explicit crash recovery.
func (c *Coordinator) Seal(ctx context.Context) (string, error) {
	owner, err := NewMutationID()
	if err != nil {
		return "", err
	}
	err = c.change(ctx, func(state *admissions) error {
		if state.Seal != "" {
			return ErrAdmissionClosed
		}
		state.Seal = owner
		return nil
	})
	return owner, err
}

// HeldBarrier binds collection/migration to the exact durable seal owner.
// Hold verifies all lifecycle owners drained and returns a release that checks
// that same owner. An error leaves admission closed for explicit recovery.
func (c *Coordinator) HeldBarrier(owner string) Barrier { return heldCoordinator{c, owner} }

type heldCoordinator struct {
	coordinator *Coordinator
	owner       string
}

func (b heldCoordinator) Hold(ctx context.Context) ([]ObjectRef, func(), error) {
	hold, err := NewMutationID()
	if err != nil {
		return nil, nil, err
	}
	var protected []ObjectRef
	err = b.coordinator.change(ctx, func(state *admissions) error {
		if b.owner == "" || state.Seal != b.owner || len(state.Owners) != 0 || state.Hold != "" {
			return ErrAdmissionClosed
		}
		state.Hold = hold
		protected = completedReferences(*state)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return protected, func() { _ = b.coordinator.releaseHeld(context.WithoutCancel(ctx), b.owner, hold) }, nil
}

// Release is explicit owner-checked recovery. Unfinished owners always refuse.
func (c *Coordinator) Release(ctx context.Context, owner string) error {
	if owner == "" {
		return ErrAdmissionClosed
	}
	return c.change(ctx, func(state *admissions) error {
		if state.Seal != owner || len(state.Owners) != 0 || state.Hold != "" || state.GCLink != nil {
			return ErrAdmissionClosed
		}
		state.Seal = ""
		return nil
	})
}

// BeginPublication holds global pending/history admission through durable local
// acknowledgement. Frozen metadata describes all active and preserved sources.
func (s *Store) BeginPublication(ctx context.Context, id string, metadata []byte) (context.Context, error) {
	if id == "" || len(id) > 455 {
		return ctx, errors.New("invalid publication identifier")
	}
	refs, digest, err := publicationAdmission(metadata)
	if err != nil {
		return ctx, err
	}
	invocation, err := NewMutationID()
	if err != nil {
		return ctx, err
	}
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.running[id] {
		return ctx, ErrAdmissionClosed
	}
	owner := "pending/" + id + "/" + invocation
	if prior, ok := s.pending[id]; ok {
		if prior != digest {
			return ctx, ErrMutationReuse
		}
		oldOwner := s.owners[id]
		err = s.Writer.Coordinator().change(ctx, func(state *admissions) error {
			previous, ok := state.Owners[oldOwner]
			if !ok || previous.Digest != digest || previous.Journal != nil || state.Seal != "" || state.Hold != "" {
				return ErrAdmissionClosed
			}
			delete(state.Owners, oldOwner)
			state.Owners[owner] = admission{Digest: digest, Refs: refs}
			return nil
		})
	} else {
		err = s.Writer.Coordinator().change(ctx, func(state *admissions) error {
			if state.Seal != "" || state.Hold != "" || len(state.Owners) >= 256 {
				return ErrAdmissionClosed
			}
			for active := range state.Owners {
				if strings.HasPrefix(active, "pending/"+id+"/") {
					return ErrAdmissionClosed
				}
			}
			state.Owners[owner] = admission{Digest: digest, Refs: refs}
			return nil
		})
	}
	if err != nil {
		return ctx, err
	}
	if s.pending == nil {
		s.pending = map[string]string{}
		s.running = map[string]bool{}
		s.claims = map[string]*publicationClaim{}
		s.owners = map[string]string{}
	}
	claim := &publicationClaim{store: s, id: id, owner: owner, digest: digest, refs: refs}
	s.pending[id], s.running[id], s.claims[id], s.owners[id] = digest, true, claim, owner
	return context.WithValue(ctx, publicationClaimKey{}, claim), nil
}

// CompletePublication settles the exact admitted invocation after its durable
// publication and local/history acknowledgement. It cannot acknowledge another
// concurrent attempt or a context retained after EndPublicationAttempt.
func (s *Store) CompletePublication(ctx context.Context, id string, metadata []byte) error {
	_, digest, err := publicationAdmission(metadata)
	if err != nil {
		return err
	}
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	claim, err := s.publicationClaim(ctx)
	if err != nil || claim.id != id || claim.digest != digest {
		return ErrAdmissionClosed
	}
	if claim.journal != nil {
		err = s.completeJournal(ctx, claim)
	} else {
		err = s.Writer.Coordinator().Complete(ctx, claim.owner, digest)
	}
	if err != nil {
		return err
	}
	if claim.journal != nil {
		// The remote receipt is settled, but local removal still belongs to this
		// exact invocation and must join before its collector lock is released.
		claim.replayOnly = true
		delete(s.pending, id)
		delete(s.owners, id)
		return nil
	}
	if claim.release != nil {
		claim.release()
	}
	delete(s.pending, id)
	delete(s.claims, id)
	delete(s.owners, id)
	delete(s.running, id)
	return nil
}

// EndPublicationAttempt releases only local execution ownership. Its durable
// unresolved admission survives cancellation, process restart and failure.
func (s *Store) EndPublicationAttempt(id string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if claim := s.claims[id]; claim != nil && claim.release != nil {
		claim.release()
	}
	delete(s.running, id)
	delete(s.claims, id)
}

func publicationAdmission(raw []byte) ([]ObjectRef, string, error) {
	if len(raw) == 0 || len(raw) > 32<<20 {
		return nil, "", storage.ErrObjectTooLarge
	}
	var entry CatalogEntry
	if err := json.Unmarshal(raw, &entry.Summary); err != nil {
		return nil, "", err
	}
	sources, err := canonicalSourceReferences(entry.Summary)
	if err != nil {
		return nil, "", err
	}
	if len(sources) > 256 {
		return nil, "", errors.New("publication source capacity exceeded")
	}
	refs := make([]ObjectRef, 0, len(sources))
	for _, source := range sources {
		if len(source.Key) > 4096 {
			return nil, "", errors.New("publication source key too large")
		}
		refs = append(refs, ObjectRef{source.Key, source.SHA256})
	}
	return refs, storage.SHA256Hex(raw), nil
}

func coordinatorObject(key string) bool {
	return key == CoordinatorKey || strings.HasPrefix(key, "catalog-v4/migrations/")
}

// Activate makes the fully verified candidate visible to general readers.
// Only the exact held migration owner can transition a drained destination.
func (c *Coordinator) Activate(ctx context.Context, owner, proof string) error {
	if proof == "" {
		return errors.New("cutover proof required")
	}
	return c.change(ctx, func(state *admissions) error {
		if state.Seal != owner || len(state.Owners) != 0 || state.Hold != "" {
			return ErrAdmissionClosed
		}
		state.Mode = admissionActive
		state.Proof = proof
		return nil
	})
}

// Active refuses candidate, interrupted and rollback destinations as complete
// archive discovery authority. Reader activation is independent from presence
// of a head and can never be inferred from a nonempty prefix.
func (c *Coordinator) Active(ctx context.Context) error {
	state, _, err := c.read(ctx)
	if err != nil {
		return err
	}
	if state.Mode != admissionActive || state.Proof == "" {
		return errors.New("catalog migration is not activated")
	}
	return nil
}

// Deactivate closes reader activation while the migration owner holds writers.
func (c *Coordinator) Deactivate(ctx context.Context, owner string) error {
	transition := func(state *admissions) error {
		if state.Seal != owner || len(state.Owners) != 0 || state.Hold != "" {
			return ErrAdmissionClosed
		}
		state.Mode = admissionRollback
		return nil
	}
	if authority, ok := ctx.Value(migrationIntentKey{}).(migrationIntentAuthority); ok {
		state, _, err := c.read(ctx)
		if err != nil {
			return err
		}
		if authority.writer != c.writer || state.Intent == nil || authority.digest != intentDigest(state.Intent) {
			return ErrAdmissionClosed
		}
		return c.changeIntent(ctx, state.Intent, transition)
	}
	return c.change(ctx, transition)
}

func (c *Coordinator) releaseHeld(ctx context.Context, owner, hold string) error {
	return c.change(ctx, func(state *admissions) error {
		if state.Seal != owner || state.Hold != hold || hold == "" || len(state.Owners) != 0 || state.GCLink != nil {
			return ErrAdmissionClosed
		}
		state.Hold = ""
		state.Seal = ""
		return nil
	})
}
