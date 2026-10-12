// Package catalog implements the opt-in immutable catalog writer protocol.
package catalog

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/listingindex"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// HeadKey is the unique mutable catalog commit point.
const HeadKey = "catalog-v4/head.json"

// SnapshotLifetime bounds the validity of a pinned catalog root.
const SnapshotLifetime = 10 * time.Minute

// ErrConflict rejects a mutation against a changed session revision.
var ErrConflict = errors.New("catalog session revision conflict")

// ErrMutationReuse rejects reuse of an ID for different frozen input.
var ErrMutationReuse = errors.New("catalog mutation ID reused")

// ErrCommitUnknown retains the transaction until fresh authority resolves it.
var ErrCommitUnknown = errors.New("catalog commit acknowledgement unknown")

// ImmutableKind selects the content-addressed object namespace.
type ImmutableKind string

// Supported immutable catalog object kinds.
const (
	KindNodes    ImmutableKind = "nodes"
	KindMetadata ImmutableKind = "metadata"
	KindHeads    ImmutableKind = "heads"
)

// ObjectRef identifies exact immutable bytes by key and SHA256.
type ObjectRef struct {
	Key    string `json:"Key"`
	SHA256 string `json:"SHA256"`
}

func (ref ObjectRef) validate() error {
	if ref.Key == "" && ref.SHA256 == "" {
		return nil
	}
	if ref.Key == "" || len(ref.SHA256) != 64 {
		return errors.New("incomplete catalog object reference")
	}
	if _, err := hex.DecodeString(ref.SHA256); err != nil {
		return errors.New("invalid catalog object checksum")
	}
	return nil
}

func (h *CatalogHead) validateReferences() error {
	for _, ref := range []ObjectRef{h.Identity, h.Capture, h.Activity, h.Project, h.Receipts, h.Previous} {
		if err := ref.validate(); err != nil {
			return err
		}
	}
	return nil
}

// CatalogHead atomically commits all indexes and mutation receipts.
//
//revive:disable-next-line:exported -- Keep the accepted protocol API name.
type CatalogHead struct {
	Protocol   uint64    `json:"Protocol"`
	Schema     uint64    `json:"Schema"`
	Generation uint64    `json:"Generation"`
	Epoch      string    `json:"Epoch"`
	Identity   ObjectRef `json:"Identity"`
	Capture    ObjectRef `json:"Capture"`
	Activity   ObjectRef `json:"Activity"`
	Project    ObjectRef `json:"Project"`
	// Receipts are committed with the roots, so a retry can prove its prior
	// commit even when a newer publication replaced the same session.
	Receipts           ObjectRef            `json:"Receipts"`
	Previous           ObjectRef            `json:"Previous"`
	CommittedAt        time.Time            `json:"CommittedAt"`
	GCCoordinator      gcCoordinatorWitness `json:"GCCoordinator"`
	GCLease            string               `json:"GCLease"`
	PublicationEpoch   string               `json:"PublicationEpoch"`
	PublicationWitness HeadWitness          `json:"PublicationWitness"`
	PredecessorWitness HeadWitness          `json:"PredecessorWitness"`
}

// HeadWitness preserves the original provider publication version through leases.
type HeadWitness struct {
	ETag         string        `json:"ETag"`
	LastModified time.Time     `json:"LastModified"`
	Precision    time.Duration `json:"Precision"`
	Epoch        string        `json:"Epoch"`
	RootsSHA256  string        `json:"RootsSHA256"`
}

// CatalogEntry holds immutable metadata and typed query summary fields.
//
//revive:disable-next-line:exported -- Keep the accepted protocol API name.
type CatalogEntry struct {
	// SummaryOverflow marks a lazy index projection resolved through Metadata.
	SummaryOverflow  string           `json:"SummaryOverflow,omitempty"`
	OrdinaryChildren uint64           `json:"OrdinaryChildren"`
	ReplayChildren   uint64           `json:"ReplayChildren"`
	Revision         string           `json:"Revision"`
	Metadata         ObjectRef        `json:"Metadata"`
	Summary          archive.Metadata `json:"Summary"`
}

// CatalogMutation replaces or deletes one expected session revision.
//
//revive:disable-next-line:exported -- Keep the accepted protocol API name.
type CatalogMutation struct {
	ID               string        `json:"ID"`
	SessionKey       string        `json:"SessionKey"`
	ExpectedRevision string        `json:"ExpectedRevision"`
	Next             *CatalogEntry `json:"Next"`
}

type record struct {
	Revision string        `json:"Revision"`
	Entry    *CatalogEntry `json:"Entry"`
}

type receipt struct {
	Digest         string `json:"Digest"`
	Revision       string `json:"Revision"`
	SessionKey     string `json:"SessionKey"`
	MetadataSHA256 string `json:"MetadataSHA256"`
}

// Writer publishes immutable catalog objects through a qualified store.
type Writer struct {
	store       storage.ObjectStore
	bounded     storage.LimitedGetter
	versioned   storage.CatalogVersionedGetter
	clock       storage.CatalogClock
	conditional storage.ConditionalPutter
	readOnly    bool
}

// ErrReadOnly refuses mutation through a reader authority wrapper.
var ErrReadOnly = errors.New("catalog authority wrapper is read-only")

// New requires qualified atomic writes and bounded versioned reads.
func New(store storage.ObjectStore) (*Writer, error) {
	if wrapped, ok := store.(*Store); ok {
		if wrapped.Writer == nil || wrapped.Writer.store != wrapped.ObjectStore || wrapped.Writer.conditional == nil {
			return nil, ErrAdmissionClosed
		}
		return wrapped.Writer, nil
	}
	qualified, ok := store.(storage.AtomicCatalogProvider)
	if !ok {
		return nil, storage.ErrAtomicCatalogUnqualified
	}
	if err := qualified.CatalogAtomicQualification(); err != nil {
		return nil, err
	}
	bounded, ok := store.(storage.LimitedGetter)
	if !ok {
		return nil, errors.New("catalog requires bounded reads")
	}
	versioned, ok := store.(storage.CatalogVersionedGetter)
	if !ok {
		return nil, errors.New("catalog requires versioned reads")
	}
	conditional, ok := store.(storage.ConditionalPutter)
	if !ok {
		return nil, errors.New("catalog requires conditional writes")
	}
	clock, ok := store.(storage.CatalogClock)
	if !ok {
		return nil, errors.New("catalog requires qualified provider clock")
	}
	readOnly := false
	if authority, ok := store.(interface{ CatalogMetadataAuthority() bool }); ok && authority.CatalogMetadataAuthority() {
		readOnly = true
		conditional = nil
	}
	return &Writer{store: store, bounded: bounded, versioned: versioned, conditional: conditional, clock: clock, readOnly: readOnly}, nil
}

// NewMutationID creates a fresh publication and revision identity.
func NewMutationID() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (w *Writer) putJSON(ctx context.Context, kind ImmutableKind, value any, limit int) (ObjectRef, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return ObjectRef{}, err
	}
	if len(b) > limit {
		return ObjectRef{}, storage.ErrObjectTooLarge
	}
	return w.putImmutableAdmitted(ctx, kind, b)
}

// PutImmutable creates exact content-addressed objects without replacement.
func (w *Writer) PutImmutable(ctx context.Context, kind ImmutableKind, b []byte) (ObjectRef, error) {
	if ctx.Value(publicationClaimKey{}) != nil {
		return ObjectRef{}, ErrAdmissionClosed
	}
	if w.readOnly {
		return ObjectRef{}, ErrReadOnly
	}
	id, err := NewMutationID()
	if err != nil {
		return ObjectRef{}, err
	}
	digest := storage.SHA256Hex(b)
	owner := "immutable/" + id
	if err = w.Coordinator().Admit(ctx, owner, digest, nil); err != nil {
		return ObjectRef{}, err
	}
	admitted := context.WithValue(ctx, writeAuthorityKey{}, writeAuthority{writer: w, owner: owner, digest: digest})
	ref, err := w.putImmutableAdmitted(admitted, kind, b)
	completeErr := w.Coordinator().Complete(context.WithoutCancel(ctx), owner, digest)
	return ref, errors.Join(err, completeErr)
}

func (w *Writer) putImmutableAdmitted(ctx context.Context, kind ImmutableKind, b []byte) (ObjectRef, error) {
	if err := w.checkWriteAuthority(ctx); err != nil {
		return ObjectRef{}, err
	}
	sum := storage.SHA256Hex(b)
	key := "catalog-v4/" + string(kind) + "/" + sum + ".json"
	if kind != KindNodes && kind != KindMetadata && kind != KindHeads {
		return ObjectRef{}, errors.New("invalid immutable object kind")
	}
	_, err := w.conditional.PutConditional(ctx, key, b, storage.PutCondition{CreateOnly: true})
	if err != nil {
		raw, readErr := w.readRef(ctx, ObjectRef{key, sum}, int64(len(b)))
		if readErr != nil {
			return ObjectRef{}, errors.Join(err, readErr)
		}
		if string(raw) != string(b) {
			return ObjectRef{}, storage.ErrChecksumMismatch
		}
	}
	return ObjectRef{key, sum}, nil
}

// Head reads the current commit point and its exact provider validator.
func (w *Writer) Head(ctx context.Context) (CatalogHead, string, error) {
	h, v, err := w.readHead(ctx)
	return h, v.ETag, err
}

func (w *Writer) readHead(ctx context.Context) (CatalogHead, storage.CatalogObjectVersion, error) {
	raw, version, err := w.versioned.GetCatalogVersion(ctx, HeadKey, 16<<10)
	etag := version.ETag
	if errors.Is(err, storage.ErrNotFound) {
		return CatalogHead{Schema: 4, Protocol: 10}, storage.CatalogObjectVersion{}, nil
	}
	if err != nil {
		return CatalogHead{}, storage.CatalogObjectVersion{}, err
	}
	if len(raw) > 16<<10 {
		return CatalogHead{}, storage.CatalogObjectVersion{}, storage.ErrObjectTooLarge
	}
	var h CatalogHead
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&h); err != nil {
		return h, storage.CatalogObjectVersion{}, err
	}
	if h.Schema != 4 || h.Protocol != 10 || h.Generation == 0 || h.Epoch == "" || h.PublicationEpoch == "" || etag == "" {
		return h, storage.CatalogObjectVersion{}, errors.New("invalid catalog head")
	}
	if err = h.validateReferences(); err != nil {
		return h, storage.CatalogObjectVersion{}, err
	}
	if err = h.observeVersion(version); err != nil {
		return h, storage.CatalogObjectVersion{}, err
	}
	return h, version, nil
}

// Find resolves a canonical session key through the current identity root.
func (w *Writer) Find(ctx context.Context, key string) (*CatalogEntry, string, error) {
	h, _, err := w.Head(ctx)
	if err != nil {
		return nil, "", err
	}
	r, err := w.find(ctx, h.Identity, key)
	return r.Entry, r.Revision, err
}

func (w *Writer) find(ctx context.Context, root ObjectRef, key string) (record, error) {
	b, err := w.lookup(ctx, root, key)
	var r record
	if err == nil && b != nil {
		err = json.Unmarshal(b, &r)
	}
	return r, err
}

// OrderPrefix names a precise auxiliary root range. All sessions keep the
// unprefixed range; root prefixes distinguish ordinary and replay sessions.
func OrderPrefix(replay bool) string {
	if replay {
		return "!root/replay/"
	}
	return "!root/ordinary/"
}

// ChildPrefix addresses one parent's children in the project tree.
func ChildPrefix(harness, parent string, replay bool) string {
	kind := "ordinary/"
	if replay {
		kind = "replay/"
	}
	return "!child/" + hex.EncodeToString([]byte(harness+"/"+parent)) + "/" + kind
}

func descendingIdentity(key string) string {
	b := []byte(key)
	for i := range b {
		b[i] = ^b[i]
	}
	return hex.EncodeToString(b)
}

func orderKeys(key string, e *CatalogEntry) []string {
	capture := e.Summary.CapturedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
	activity := listingindex.ActivityTime(e.Summary).UTC().Format("2006-01-02T15:04:05.000000000Z")
	identity := descendingIdentity(key)
	return []string{capture + "/" + identity, activity + "/" + capture + "/" + identity, "project/" + hex.EncodeToString([]byte(e.Summary.ProjectID)) + "/" + key}
}

func allOrderKeys(key string, e *CatalogEntry) [][]string {
	keys := orderKeys(key, e)
	result := [][]string{{keys[0]}, {keys[1]}, {keys[2]}}
	if !e.Summary.IsChild() {
		prefix := OrderPrefix(e.Summary.Replay != nil)
		for i, k := range keys {
			result[i] = append(result[i], prefix+k)
		}
	} else {
		kind := "ordinary/"
		if e.Summary.Replay != nil {
			kind = "replay/"
		}
		for i, k := range keys {
			result[i] = append(result[i], "!children/"+kind+k)
		}
		if e.Summary.ParentSessionID != "" {
			prefix := ChildPrefix(e.Summary.Harness.Name, e.Summary.ParentSessionID, e.Summary.Replay != nil)
			result[2] = append(result[2], prefix+keys[0])
		}
	}
	return result
}

// Commit rebases only across other sessions. Every acknowledged mutation has
// a durable receipt; an ambiguous response never licenses a blind overwrite.
func (w *Writer) Commit(ctx context.Context, m CatalogMutation) (string, error) {
	if ctx.Value(publicationClaimKey{}) != nil {
		return "", ErrAdmissionClosed
	}
	if w.readOnly {
		return "", ErrReadOnly
	}
	if m.Next != nil && m.Next.SummaryOverflow != "" {
		return "", errors.New("catalog mutation requires complete metadata summary")
	}
	frozen, digest, err := freezeMutation(m)
	if err != nil {
		return "", err
	}
	var refs []ObjectRef
	if frozen.Next != nil {
		refs = append(refs, frozen.Next.Metadata)
		sources, e := canonicalSourceReferences(frozen.Next.Summary)
		if e != nil {
			return "", e
		}
		for _, source := range sources {
			refs = append(refs, ObjectRef{source.Key, source.SHA256})
		}
	}
	c := w.Coordinator()
	invocation, e := NewMutationID()
	if e != nil {
		return "", e
	}
	owner := "commit/" + m.ID + "/" + invocation
	if err = c.Admit(ctx, owner, digest, refs); err != nil {
		return "", err
	}
	admitted := context.WithValue(ctx, writeAuthorityKey{}, writeAuthority{writer: w, owner: owner, digest: digest})
	revision, err := w.commitAdmitted(admitted, frozen)
	if errors.Is(err, ErrCommitUnknown) {
		return revision, err
	}
	completeErr := c.Complete(context.WithoutCancel(ctx), owner, digest)
	return revision, errors.Join(err, completeErr)
}

func (w *Writer) commitAdmitted(ctx context.Context, m CatalogMutation) (string, error) {
	if err := w.checkWriteAuthority(ctx); err != nil {
		return "", err
	}
	m, digest, err := freezeMutation(m)
	if err != nil {
		return "", err
	}
	revision, err := NewMutationID()
	if err != nil {
		return "", err
	}
	for range 64 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		h, etag, err := w.Head(ctx)
		if err != nil {
			return "", err
		}
		if h.GCLease != "" {
			return "", errors.New("catalog garbage collection is fenced")
		}
		committed, err := w.receipt(ctx, h, m.ID, digest)
		if err != nil || committed != "" {
			return committed, err
		}
		next, err := w.prepareCommit(ctx, h, m, revision, digest, etag != "")
		if err != nil {
			return "", err
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return "", err
		}
		if err := w.checkWriteAuthority(ctx); err != nil {
			return "", err
		}
		_, putErr := w.conditional.PutConditional(ctx, HeadKey, raw, storage.PutCondition{MatchETag: etag, CreateOnly: etag == ""})
		if putErr == nil {
			return revision, nil
		}
		// A definite CAS conflict may rebase. Every other failed response requires
		// fresh receipt authority, or remains unknown without blindly retrying.
		fresh, _, readErr := w.Head(ctx)
		if readErr != nil {
			return "", errors.Join(ErrCommitUnknown, putErr, readErr)
		}
		committed, readErr = w.receipt(ctx, fresh, m.ID, digest)
		if readErr != nil {
			return "", errors.Join(ErrCommitUnknown, putErr, readErr)
		}
		if committed != "" {
			return committed, nil
		}
		if !errors.Is(putErr, storage.ErrPreconditionFailed) {
			return "", errors.Join(ErrCommitUnknown, putErr)
		}
	}
	return "", fmt.Errorf("catalog contention exceeded retry bound: %w", storage.ErrPreconditionFailed)
}

func freezeMutation(m CatalogMutation) (CatalogMutation, string, error) {
	if m.Next != nil && m.Next.SummaryOverflow != "" {
		return CatalogMutation{}, "", errors.New("catalog mutation requires complete metadata summary")
	}

	if m.ID == "" || len(m.ID) > 128 || m.SessionKey == "" || len(m.SessionKey) > 1024 {
		return m, "", errors.New("invalid catalog mutation")
	}
	if m.Next != nil && (m.Next.Metadata.Key == "" || m.Next.Metadata.SHA256 == "") {
		return m, "", errors.New("metadata reference required")
	}
	b, err := json.Marshal(m) // #nosec G117 -- SessionKey is an archive object identity, never credential material.
	if err != nil {
		return m, "", err
	}
	// Freeze caller-owned slices, maps and pointers for all retries.
	var frozen CatalogMutation
	if err = json.Unmarshal(b, &frozen); err != nil {
		return m, "", err
	}
	return frozen, storage.SHA256Hex(b), nil
}

func (w *Writer) receipt(ctx context.Context, h CatalogHead, id, digest string) (string, error) {
	b, err := w.lookup(ctx, h.Receipts, id)
	if err != nil || b == nil {
		return "", err
	}
	var r receipt
	if err = json.Unmarshal(b, &r); err != nil {
		return "", err
	}
	if r.Digest != digest || r.Revision == "" || r.SessionKey == "" {
		return "", ErrMutationReuse
	}
	return r.Revision, nil
}

func (w *Writer) prepareCommit(ctx context.Context, h CatalogHead, m CatalogMutation, revision, digest string, existing bool) (CatalogHead, error) {
	base := h
	if m.Next != nil {
		if err := w.verifyEntry(ctx, m.SessionKey, m.Next); err != nil {
			return h, err
		}
	}
	old, err := w.find(ctx, h.Identity, m.SessionKey)
	if err != nil {
		return h, err
	}
	if old.Revision != m.ExpectedRevision {
		return h, ErrConflict
	}
	next := m.Next
	if next != nil {
		next.Revision = revision
	}
	if err = w.updateOrders(ctx, &h, m.SessionKey, old.Entry, next); err != nil {
		return h, err
	}
	h.Identity, err = w.update(ctx, h.Identity, m.SessionKey, record{revision, next})
	if err != nil {
		return h, err
	}
	if err = w.updateChildCounters(ctx, &h, m.SessionKey, old.Entry, next); err != nil {
		return h, err
	}
	metadataHash := ""
	if next != nil {
		metadataHash = next.Metadata.SHA256
	}
	h.Receipts, err = w.update(ctx, h.Receipts, m.ID, receipt{Digest: digest, Revision: revision, SessionKey: m.SessionKey, MetadataSHA256: metadataHash})
	if err != nil {
		return h, err
	}
	if existing {
		h.PredecessorWitness = base.PublicationWitness
		h.Previous, err = w.putJSON(ctx, KindHeads, base, 16<<10)
		if err != nil {
			return h, err
		}
	}
	h.Generation++
	h.Epoch = revision
	h.CommittedAt = time.Time{}
	h.PublicationEpoch = revision
	h.PublicationWitness = HeadWitness{}
	return h, nil
}

func (w *Writer) updateOrders(ctx context.Context, h *CatalogHead, key string, old, next *CatalogEntry) error {
	roots := []*ObjectRef{&h.Capture, &h.Activity, &h.Project}
	for _, entry := range []*CatalogEntry{old, next} {
		if entry == nil {
			continue
		}
		var value any
		if entry == next {
			value = entry
		}
		for i, keys := range allOrderKeys(key, entry) {
			for _, orderKey := range keys {
				ref, err := w.update(ctx, *roots[i], orderKey, value)
				if err != nil {
					return err
				}
				*roots[i] = ref
			}
		}
	}
	return nil
}

func (w *Writer) verifyEntry(ctx context.Context, key string, entry *CatalogEntry) error {
	if err := validateEntryReference(*entry); err != nil {
		return err
	}
	raw, err := w.readRef(ctx, entry.Metadata, 32<<20)
	if err != nil {
		return err
	}
	metadata, err := decodeEntryBody(*entry, raw)
	if err != nil {
		return err
	}
	canonical, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	if err != nil || canonical != key {
		return errors.New("catalog metadata identity mismatch")
	}
	entry.Summary = metadata
	entry.SummaryOverflow = ""
	refs, err := canonicalSourceReferences(metadata)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if statter, ok := w.store.(storage.ObjectStatter); ok {
			info, e := statter.Stat(ctx, ref.Key)
			if e != nil {
				return e
			}
			if info.Size != int64(ref.CompressedBytes) {
				return storage.ErrChecksumMismatch
			}
			if info.SHA256 != "" {
				if info.SHA256 != ref.SHA256 {
					return storage.ErrChecksumMismatch
				}
				continue
			}
		}
		var source []byte
		if source, err = w.readRef(ctx, ObjectRef{ref.Key, ref.SHA256}, int64(ref.CompressedBytes)); err != nil {
			return err
		}
		if len(source) != ref.CompressedBytes {
			return storage.ErrChecksumMismatch
		}
	}
	return nil
}

func (h *CatalogHead) rootsSHA256() string {
	b, _ := json.Marshal(struct {
		Identity    ObjectRef   `json:"Identity"`
		Capture     ObjectRef   `json:"Capture"`
		Activity    ObjectRef   `json:"Activity"`
		Project     ObjectRef   `json:"Project"`
		Receipts    ObjectRef   `json:"Receipts"`
		Previous    ObjectRef   `json:"Previous"`
		Epoch       string      `json:"Epoch"`
		Predecessor HeadWitness `json:"Predecessor"`
	}{h.Identity, h.Capture, h.Activity, h.Project, h.Receipts, h.Previous, h.PublicationEpoch, h.PredecessorWitness})
	return storage.SHA256Hex(b)
}

func (h *CatalogHead) observeVersion(v storage.CatalogObjectVersion) error {
	if v.LastModified.IsZero() || v.Precision <= 0 || v.Precision > time.Minute {
		return errors.New("catalog version timestamp precision is unqualified")
	}
	witness := h.PublicationWitness
	if witness.ETag == "" {
		if h.Epoch != h.PublicationEpoch || h.GCLease != "" {
			return errors.New("catalog lease lost publication witness")
		}
		witness = HeadWitness{ETag: v.ETag, LastModified: v.LastModified, Precision: v.Precision, Epoch: h.PublicationEpoch, RootsSHA256: h.rootsSHA256()}
	}
	if witness.ETag == "" || witness.LastModified.IsZero() || witness.Precision <= 0 || witness.Precision > time.Minute || witness.Precision != v.Precision || witness.Epoch != h.PublicationEpoch || witness.RootsSHA256 != h.rootsSHA256() || v.LastModified.Add(v.Precision).Before(witness.LastModified) {
		return errors.New("catalog publication witness differs from observed version")
	}
	if h.Previous.Key != "" {
		prior := h.PredecessorWitness
		if prior.ETag == "" || prior.LastModified.IsZero() || prior.Precision <= 0 || prior.Precision > time.Minute || prior.LastModified.After(witness.LastModified.Add(witness.Precision)) {
			return errors.New("catalog predecessor publication clock regressed or is unqualified")
		}
	}
	h.PublicationWitness = witness
	h.CommittedAt = witness.LastModified
	return nil
}

// Validate the entire frozen reference set before source reads or admission.
// Ordinary legacy reference permissiveness never grants catalog authority.
func canonicalSourceReferences(metadata archive.Metadata) ([]archive.SourceReference, error) {
	canonical, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	if err != nil {
		return nil, err
	}
	refs, err := metadata.SourceReferences()
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		digest, e := hex.DecodeString(ref.SHA256)
		if e != nil || len(digest) != 32 || ref.SHA256 != hex.EncodeToString(digest) || ref.Key != strings.TrimSuffix(canonical, "metadata.json")+"source."+ref.SHA256+".jsonl.gz" {
			return nil, errors.New("catalog source reference does not belong to canonical session")
		}
	}
	return refs, nil
}
