package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"time"

	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// PickerUpgradeState is the coordinator's durable construction checkpoint. All
// candidate roots stay under its seal until configuration acknowledges activation.
// Captured source roots, immutable bodies and mutation receipts never change.
type PickerUpgradeState struct {
	Owner         string             `json:"owner"`
	Destination   destination.Config `json:"destination"`
	Proof         string             `json:"proof"`
	Source        CatalogHead        `json:"source"`
	SourceETag    string             `json:"source_etag"`
	Candidate     CatalogHead        `json:"candidate"`
	Cursor        string             `json:"cursor"`
	Phase         string             `json:"phase"`
	Copied        uint64             `json:"copied"`
	Memberships   uint64             `json:"memberships"`
	ActivatedETag string             `json:"activated_etag,omitempty"`
}

func (u *PickerUpgradeState) validate(s admissions) error {
	if u == nil {
		return nil
	}
	if !exactHex(u.Owner, 24) || s.Seal != u.Owner || s.Hold != "" || s.GCLink != nil || s.Intent != nil || u.Proof == "" || len(u.Proof) > 4096 || len(u.SourceETag) > 4096 || len(u.ActivatedETag) > 4096 || u.Candidate.Receipts != u.Source.Receipts || u.Destination.EffectiveArchiveFormat() != destination.FormatCatalogV4 || len(u.Cursor) > 4096 || u.Source.Protocol != 10 || !u.Source.validFormat() || !u.Candidate.validFormat() || u.Candidate.Protocol != 11 || u.Source.validateReferences() != nil || u.Candidate.validateReferences() != nil {
		return errCoordinatorDescriptor
	}
	switch u.Phase {
	case "draining":
		if u.Copied != 0 || u.Cursor != "" || s.Protocol != 10 {
			return errCoordinatorDescriptor
		}
	case "building", "verified":
		if len(s.Owners) != 0 || s.Protocol != 10 {
			return errCoordinatorDescriptor
		}
	case "active", "rollback":
		if u.ActivatedETag == "" {
			return errCoordinatorDescriptor
		}
		fallthrough
	case "activating":
		if len(s.Owners) != 0 || s.Protocol != 11 {
			return errCoordinatorDescriptor
		}
	default:
		return errCoordinatorDescriptor
	}
	return nil
}

// PickerUpgrade builds complete scoped roots without exposing intermediate rows.
type PickerUpgrade struct {
	writer    *Writer
	authority CutoverAuthority
	State     PickerUpgradeState
}
type pickerUpgradeKey struct{}
type pickerUpgradeAuthority struct {
	writer *Writer
	owner  string
}

func (u *PickerUpgrade) context(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, writeAuthorityKey{}, writeAuthority{writer: u.writer, seal: u.State.Owner})
	return context.WithValue(ctx, pickerUpgradeKey{}, pickerUpgradeAuthority{u.writer, u.State.Owner})
}

// OpenPickerUpgrade seals an existing catalog or an empty fresh candidate. Resume
// requires the recorded owner and the same reviewed credential proof. Unknown
// pending owners remain sealed and must settle through their exact owner route.
func OpenPickerUpgrade(ctx context.Context, store storage.ObjectStore, cfg destination.Config, authority CutoverAuthority, owner string) (*PickerUpgrade, error) {
	w, err := New(store)
	if err != nil {
		return nil, err
	}
	if w.readOnly || cfg.EffectiveArchiveFormat() != destination.FormatCatalogV4 || authority == nil {
		return nil, errors.New("scoped upgrade requires catalog destination and reviewed credential authority")
	}
	proof, err := authority.VerifyCatalogCutover(ctx, cfg, cfg)
	if err != nil {
		return nil, err
	}
	if proof.Protocol != 11 || proof.ID == "" || proof.Source != destinationIdentity(cfg) || proof.Destination != destinationIdentity(cfg) {
		return nil, errors.New("scoped upgrade requires exact protocol 11 credential proof")
	}
	state, _, err := w.Coordinator().read(ctx)
	if err != nil {
		return nil, err
	}
	if state.Upgrade != nil || state.PickerUpgradeReceipt != nil || owner != "" {
		return resumePickerUpgrade(w, state, cfg, authority, proof, owner)
	}
	h, etag, err := w.Head(ctx)
	if err != nil {
		return nil, err
	}
	if h.Protocol != 10 || h.GCLease != "" {
		return nil, errors.New("scoped upgrade requires an unleased protocol 10 source")
	}
	if etag == "" {
		if err = validateFreshPickerNamespace(ctx, store); err != nil {
			return nil, err
		}
	}
	if _, err = w.preflightClock(ctx); err != nil {
		return nil, err
	}
	owner, err = NewMutationID()
	if err != nil {
		return nil, err
	}
	candidate := h
	candidate.Protocol = 11
	candidate.Schema = 5
	candidate.Picker = pickerFormat()
	candidate.Identity = ObjectRef{}
	candidate.Capture = ObjectRef{}
	candidate.Activity = ObjectRef{}
	candidate.Project = ObjectRef{}
	u := &PickerUpgrade{writer: w, authority: authority, State: PickerUpgradeState{Owner: owner, Destination: cfg, Proof: proof.ID, Source: h, SourceETag: etag, Candidate: candidate, Phase: "draining"}}
	err = u.save(ctx, nil, func(s *admissions) error {
		if s.Protocol != 10 || s.Seal != "" || s.Hold != "" || s.Intent != nil || s.Upgrade != nil || s.GCLink != nil {
			return ErrAdmissionClosed
		}
		s.Seal = owner
		return nil
	})
	return u, err
}

// Save compares the complete previous checkpoint. Exact readback adjudicates a
// lost coordinator response; concurrent progress never overwrites another page.
func (u *PickerUpgrade) save(ctx context.Context, prior *PickerUpgradeState, before func(*admissions) error) error {
	wanted := u.State
	var expected []byte
	err := u.writer.Coordinator().change(ctx, func(s *admissions) error {
		if !reflect.DeepEqual(s.Upgrade, prior) {
			return ErrConflict
		}
		if before != nil {
			if err := before(s); err != nil {
				return err
			}
		}
		s.Upgrade = &wanted
		next := *s
		next.Generation++
		expected, _ = json.Marshal(next)
		return nil
	})
	if err == nil {
		return nil
	}
	if len(expected) > 0 {
		raw, _, e := u.writer.versioned.GetCatalogVersion(ctx, CoordinatorKey, 4<<20)
		if e == nil && bytes.Equal(raw, expected) {
			return nil
		}
	}
	return err
}

// identityPage traverses finite pages, including tombstones, without retaining
// the complete source universe in memory.
func (w *Writer) identityPage(ctx context.Context, ref ObjectRef, after string, limit int) ([]item, error) {
	out := make([]item, 0, limit)
	var visit func(ObjectRef, int) error
	visit = func(ref ObjectRef, depth int) error {
		if depth >= maxDepth {
			return errors.New("catalog tree depth exceeded")
		}
		n, err := w.readNode(ctx, ref)
		if err != nil {
			return err
		}
		for _, c := range n.Children {
			if c.Max <= after {
				continue
			}
			if err = visit(c.Ref, depth+1); err != nil {
				return err
			}
			if len(out) == limit {
				return nil
			}
		}
		for _, leaf := range n.Leaves {
			if leaf.Key > after {
				out = append(out, leaf)
				if len(out) == limit {
					return nil
				}
			}
		}
		return ctx.Err()
	}
	err := visit(ref, 0)
	return out, err
}

func (u *PickerUpgrade) unchanged(ctx context.Context) error {
	h, etag, err := u.writer.Head(ctx)
	if err != nil {
		return err
	}
	if etag != u.State.SourceETag || !sameObservedHead(h, u.State.Source) {
		return ErrConflict
	}
	return nil
}

func sameObservedHead(a, b CatalogHead) bool { return headHash(a) == headHash(b) }

// Step consumes at most pageSize source identities, hydrating every live body
// exactly once in that page before deriving indexed facts. Replaying a page
// rewrites only immutable nodes and preserves original revisions and receipts.
func (u *PickerUpgrade) Step(ctx context.Context, pageSize int) (bool, error) {
	if pageSize < 1 || pageSize > 128 {
		return false, errors.New("upgrade page size must be between 1 and 128")
	}
	if u.State.Phase == "verified" || u.State.Phase == "activating" || u.State.Phase == "active" {
		return true, nil
	}
	starting := u.State
	committed := false
	defer func() {
		if !committed {
			u.State = starting
		}
	}()
	prior := u.State
	state, _, err := u.writer.Coordinator().read(ctx)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(state.Upgrade, &prior) || state.Seal != prior.Owner || len(state.Owners) != 0 {
		return false, ErrAdmissionClosed
	}
	if prior.Phase == "draining" {
		h, etag, e := u.writer.Head(ctx)
		if e != nil {
			return false, e
		}
		if h.Protocol != 10 || h.GCLease != "" {
			return false, ErrConflict
		}
		u.State.Source = h
		u.State.SourceETag = etag
		u.State.Candidate.Receipts = h.Receipts
		u.State.Phase = "building"
		if err = u.save(ctx, &prior, nil); err != nil {
			u.State = prior
			return false, err
		}
		prior = u.State
	}
	if err = u.unchanged(ctx); err != nil {
		return false, err
	}
	rows, err := u.writer.identityPage(ctx, u.State.Source.Identity, u.State.Cursor, pageSize)
	if err != nil {
		return false, err
	}
	admitted := u.context(ctx)
	if err = u.copyPage(admitted, rows); err != nil {
		return false, err
	}
	if len(rows) < pageSize {
		if err = u.verify(ctx); err != nil {
			u.State = prior
			return false, err
		}
		u.State.Phase = "verified"
	}
	if err = u.save(ctx, &prior, nil); err != nil {
		u.State = prior
		return false, err
	}
	committed = true
	return u.State.Phase == "verified", nil
}

// Verify independently checks source-to-candidate correspondence and the exact
// auxiliary cardinalities. Each expected key is checked, so equal cardinality
// also proves there are no extra keys. Verification retains only one page.
func (u *PickerUpgrade) verify(ctx context.Context) error {
	cursor := ""
	var live, members uint64
	counts := [3]uint64{}
	for {
		rows, err := u.writer.identityPage(ctx, u.State.Source.Identity, cursor, 64)
		if err != nil {
			return err
		}
		for _, leaf := range rows {
			if err = u.verifyIdentity(ctx, leaf, &live, &members, &counts); err != nil {
				return err
			}
			cursor = leaf.Key
		}
		if len(rows) < 64 {
			break
		}
	}
	src, err := u.writer.readNode(ctx, u.State.Source.Identity)
	if err != nil {
		return err
	}
	dst, err := u.writer.readNode(ctx, u.State.Candidate.Identity)
	if err != nil {
		return err
	}
	if src.Count != dst.Count || live != u.State.Copied || members != u.State.Memberships {
		return errors.New("upgrade identity cardinality failed")
	}
	for i, root := range []ObjectRef{u.State.Candidate.Capture, u.State.Candidate.Activity, u.State.Candidate.Project} {
		n, e := u.writer.readNode(ctx, root)
		if e != nil {
			return e
		}
		if n.Count != counts[i] {
			return errors.New("upgrade auxiliary cardinality failed")
		}
	}
	if u.State.Candidate.Receipts != u.State.Source.Receipts {
		return errors.New("upgrade changed mutation receipts")
	}
	return u.unchanged(ctx)
}

// Activate persists the stronger coordinator fence first, then commits one
// complete head CAS. Mixed states stay sealed, resumable and refuse writes.
func (u *PickerUpgrade) Activate(ctx context.Context) error {
	if err := u.checkProof(ctx); err != nil {
		return err
	}
	if _, err := u.writer.preflightClock(ctx); err != nil {
		return err
	}
	if u.State.Phase == "active" {
		return nil
	}
	if u.State.Phase == "verified" {
		if err := u.verify(ctx); err != nil {
			return err
		}
		prior := u.State
		id, err := NewMutationID()
		if err != nil {
			return err
		}
		candidate := u.State.Candidate
		if u.State.SourceETag != "" {
			candidate.PredecessorWitness = u.State.Source.PublicationWitness
			candidate.Previous, err = u.writer.putJSON(u.context(ctx), KindHeads, u.State.Source, 16<<10)
			if err != nil {
				return err
			}
		}
		candidate.Generation = u.State.Source.Generation + 1
		candidate.Epoch = id
		candidate.PublicationEpoch = id
		candidate.PublicationWitness = HeadWitness{}
		candidate.CommittedAt = time.Time{}
		u.State.Candidate = candidate
		u.State.Phase = "activating"
		if err = u.save(ctx, &prior, func(s *admissions) error { s.Protocol = 11; s.Picker = pickerFormat(); return nil }); err != nil {
			u.State = prior
			return err
		}
	}
	if u.State.Phase != "activating" {
		return errors.New("upgrade must be completely verified before activation")
	}
	h, etag, err := u.writer.Head(ctx)
	if err != nil {
		return err
	}
	candidate := u.State.Candidate
	// Version observation fills publication witnesses; compare the publication
	// identity and immutable roots when adjudicating an already successful CAS.
	observed := candidate
	observed.PublicationWitness = h.PublicationWitness
	observed.CommittedAt = h.CommittedAt
	same := headHash(observed) == headHash(h)

	if !same {
		if etag != u.State.SourceETag || !sameObservedHead(h, u.State.Source) {
			return ErrConflict
		}
		raw, e := json.Marshal(candidate)
		if e != nil {
			return e
		}
		_, e = u.writer.conditional.PutConditional(ctx, HeadKey, raw, storage.PutCondition{MatchETag: etag, CreateOnly: etag == ""})
		if e != nil {
			return errors.Join(ErrCommitUnknown, e)
		}
		return u.Activate(ctx)
	}
	prior := u.State
	u.State.Phase = "active"
	u.State.ActivatedETag = etag
	err = u.save(ctx, &prior, func(s *admissions) error { s.Mode = admissionActive; s.Proof = u.State.Proof; return nil })
	if err != nil {
		u.State = prior
	}
	return err
}

// FinishActivation acknowledges the protected local configuration save. Until
// this explicit acknowledgement all writers and GC remain durably fenced.
func (u *PickerUpgrade) FinishActivation(ctx context.Context) error {
	if u.State.Phase != "active" {
		return ErrAdmissionClosed
	}
	state, _, err := u.writer.Coordinator().read(ctx)
	if err != nil {
		return err
	}
	if state.Upgrade == nil && state.Seal == "" && state.Mode == admissionActive && state.Protocol == 11 && state.Proof == u.State.Proof && reflect.DeepEqual(state.PickerUpgradeReceipt, &u.State) {
		return nil
	}
	h, etag, err := u.writer.Head(ctx)
	if err != nil {
		return err
	}
	if etag != u.State.ActivatedETag || h.Identity != u.State.Candidate.Identity || h.Protocol != 11 {
		return ErrConflict
	}
	return u.writer.Coordinator().change(ctx, func(s *admissions) error {
		if s.Upgrade == nil && reflect.DeepEqual(s.PickerUpgradeReceipt, &u.State) && s.Seal == "" {
			return nil
		}
		if !reflect.DeepEqual(s.Upgrade, &u.State) || s.Seal != u.State.Owner || len(s.Owners) != 0 {
			return ErrAdmissionClosed
		}
		s.PickerUpgradeReceipt = &u.State
		s.Upgrade = nil
		s.Seal = ""
		return nil
	})
}

// Rollback leaves the fully indexed original data readable under the permanent
// stronger fence and durably pauses publication. Any later head or unresolved
// owner refuses; an older unindexed root is never installed as protocol 11.
func (u *PickerUpgrade) Rollback(ctx context.Context) error {
	if err := u.checkProof(ctx); err != nil {
		return err
	}
	if u.State.Phase != "active" && u.State.Phase != "rollback" {
		return errors.New("only activated scoped upgrade can roll back")
	}
	h, etag, err := u.writer.Head(ctx)
	if err != nil {
		return err
	}
	if etag != u.State.ActivatedETag || h.Protocol != 11 || h.Identity != u.State.Candidate.Identity || h.Project != u.State.Candidate.Project {
		return errors.New("catalog changed since scoped activation; rollback would forget publications")
	}
	prior := u.State
	u.State.Phase = "rollback"
	err = u.writer.Coordinator().change(ctx, func(s *admissions) error {
		if len(s.Owners) != 0 || s.Hold != "" || s.GCLink != nil || s.Intent != nil {
			return ErrAdmissionClosed
		}
		if s.Upgrade != nil {
			if !reflect.DeepEqual(s.Upgrade, &prior) {
				return ErrConflict
			}
		} else if !reflect.DeepEqual(s.PickerUpgradeReceipt, &prior) || s.Seal != "" {
			return ErrConflict
		}
		s.Seal = u.State.Owner
		s.Upgrade = &u.State
		return nil
	})
	if err != nil {
		u.State = prior
	}
	return err
}

// PickerUpgradeCheckpoint exposes only the validated coordinator checkpoint for
// operator resumption. It does not grant write authority or settle pending work.
func PickerUpgradeCheckpoint(ctx context.Context, store storage.ObjectStore) (PickerUpgradeState, error) {
	w, err := New(store)
	if err != nil {
		return PickerUpgradeState{}, err
	}
	state, _, err := w.Coordinator().read(ctx)
	if err != nil {
		return PickerUpgradeState{}, err
	}
	if state.Upgrade != nil {
		return *state.Upgrade, nil
	}
	if state.PickerUpgradeReceipt != nil {
		return *state.PickerUpgradeReceipt, nil
	}
	return PickerUpgradeState{}, storage.ErrNotFound
}

func (u *PickerUpgrade) checkProof(ctx context.Context) error {
	if u.authority == nil {
		return ErrAdmissionClosed
	}
	cfg := u.State.Destination
	proof, err := u.authority.VerifyCatalogCutover(ctx, cfg, cfg)
	if err != nil {
		return err
	}
	if proof.ID != u.State.Proof || proof.Protocol != 11 || proof.Source != destinationIdentity(cfg) || proof.Destination != destinationIdentity(cfg) {
		return ErrAdmissionClosed
	}
	return nil
}

func resumePickerUpgrade(w *Writer, state admissions, cfg destination.Config, authority CutoverAuthority, proof CutoverProof, owner string) (*PickerUpgrade, error) {
	if state.Upgrade != nil {
		saved := state.Upgrade
		if owner == "" || owner != saved.Owner || saved.Proof != proof.ID || !reflect.DeepEqual(saved.Destination, cfg) {
			return nil, ErrAdmissionClosed
		}
		return &PickerUpgrade{writer: w, authority: authority, State: *saved}, nil
	}
	if saved := state.PickerUpgradeReceipt; saved != nil {
		if owner == "" || saved.Owner != owner || saved.Proof != proof.ID || !reflect.DeepEqual(saved.Destination, cfg) {
			return nil, ErrAdmissionClosed
		}
		return &PickerUpgrade{writer: w, authority: authority, State: *saved}, nil
	}
	if owner != "" {
		return nil, ErrAdmissionClosed
	}

	return nil, ErrAdmissionClosed
}

func (u *PickerUpgrade) copyPage(admitted context.Context, rows []item) error {
	var err error
	for _, leaf := range rows {
		var r record
		if err = json.Unmarshal(leaf.Value, &r); err != nil {
			return err
		}
		if r.Revision == "" {
			return errors.New("source identity has no revision")
		}
		if r.Entry != nil {
			e := r.Entry
			if e.Revision != r.Revision || e.Picker != nil {
				return errors.New("source identity revision or format mismatch")
			}
			if err = u.writer.verifyEntry(admitted, leaf.Key, e); err != nil {
				return err
			}
			e.Picker, err = BuildPickerRow(leaf.Key, *e)
			if err != nil {
				return err
			}
			keys, eerr := PickerMembershipKeys(leaf.Key, *e.Picker)
			if eerr != nil {
				return eerr
			}
			u.State.Memberships += uint64(len(keys))
			if err = u.writer.updateOrders(admitted, &u.State.Candidate, leaf.Key, nil, e); err != nil {
				return err
			}
			u.State.Copied++
		}
		u.State.Candidate.Identity, err = u.writer.update(admitted, u.State.Candidate.Identity, leaf.Key, r)
		if err != nil {
			return err
		}
		u.State.Cursor = leaf.Key
	}

	return nil
}

func (u *PickerUpgrade) verifyIdentity(ctx context.Context, leaf item, live, members *uint64, counts *[3]uint64) error {
	var err error

	var source record
	if err = json.Unmarshal(leaf.Value, &source); err != nil {
		return err
	}
	actual, err := u.writer.find(ctx, u.State.Candidate.Identity, leaf.Key)
	if err != nil {
		return err
	}
	if actual.Revision != source.Revision || (source.Entry == nil) != (actual.Entry == nil) {
		return errors.New("upgrade identity correspondence failed")
	}
	if source.Entry != nil {
		raw, e := u.writer.readRef(ctx, source.Entry.Metadata, 32<<20)
		if e != nil {
			return e
		}
		full, e := decodeEntryBody(*source.Entry, raw)
		if e != nil {
			return e
		}
		expected := *source.Entry
		expected.Summary = full
		expected.SummaryOverflow = ""
		expected.Picker, e = BuildPickerRow(leaf.Key, expected)
		if e != nil {
			return e
		}
		if actual.Entry.Picker == nil || !samePickerRow(*expected.Picker, *actual.Entry.Picker) || actual.Entry.MatchMetadata(full) != nil {
			return errors.New("upgrade picker correspondence failed")
		}
		*live++
		keys, e := PickerMembershipKeys(leaf.Key, *expected.Picker)
		if e != nil {
			return e
		}
		*members += uint64(len(keys))
		for i, keys := range allOrderKeys(leaf.Key, &expected) {
			roots := []ObjectRef{u.State.Candidate.Capture, u.State.Candidate.Activity, u.State.Candidate.Project}
			counts[i] += uint64(len(keys))
			for _, key := range keys {
				raw, e = u.writer.lookup(ctx, roots[i], key)
				if e != nil {
					return e
				}
				var entry CatalogEntry
				if len(raw) == 0 || json.Unmarshal(raw, &entry) != nil || entry.MatchMetadata(full) != nil || entry.Picker == nil || !samePickerRow(*entry.Picker, *expected.Picker) {
					return errors.New("upgrade membership correspondence failed")
				}
			}
		}
	}
	return nil
}

func validateFreshPickerNamespace(ctx context.Context, store storage.ObjectStore) error {
	lister, ok := store.(storage.PageLister)
	if !ok {
		return errors.New("fresh scoped catalog requires bounded namespace verification")
	}
	cursor := ""
	for {
		page, err := lister.ListPage(ctx, "sessions/", cursor, 64)
		if err != nil {
			return err
		}
		for _, obj := range page.Objects {
			if metadataKey(obj.Key) {
				return errors.New("fresh scoped destination contains legacy metadata; use isolated legacy migration")
			}
		}
		if page.Next == "" {
			return nil
		}
		if page.Next == cursor {
			return errors.New("namespace verification did not advance")
		}
		cursor = page.Next
	}
}

// UnmarshalJSON refuses unknown, missing and null checkpoint fields without
// modifying the receiver. Head, coordinator and checkpoint contracts remain
// distinct even though this checkpoint contains exact copies of both heads.
func (u *PickerUpgradeState) UnmarshalJSON(raw []byte) error {
	type plain PickerUpgradeState
	var next plain
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&next) != nil || d.Decode(new(any)) != io.EOF {
		return errCoordinatorDescriptor
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return errCoordinatorDescriptor
	}
	for _, field := range []string{"owner", "destination", "proof", "source", "source_etag", "candidate", "cursor", "phase", "copied", "memberships"} {
		value, present := fields[field]
		if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errCoordinatorDescriptor
		}
	}
	*u = PickerUpgradeState(next)
	return nil
}
