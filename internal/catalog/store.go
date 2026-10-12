package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// ErrGCRequired defers physical cleanup until global GC fencing is held.
var ErrGCRequired = errors.New("catalog source deletion requires fenced garbage collection")

// Store is the narrow publication/history authority adapter. General catalog
// query activation belongs to migration; no canonical metadata is dual-written.
type Store struct {
	storage.ObjectStore
	Writer        *Writer
	pendingMu     sync.Mutex
	pending       map[string]string
	running       map[string]bool
	claims        map[string]*publicationClaim
	owners        map[string]string
	readScope     string
	destinationID string
}

// WrapConfigured binds journal recovery to the existing credential-free
// configured namespace identity. An unbound Wrap cannot transfer old owners.
func WrapConfigured(store storage.ObjectStore, cfg destination.Config) (*Store, error) {
	wrapped, err := Wrap(store)
	if err != nil {
		return nil, err
	}
	id := config.DestinationID(cfg)
	wrapped.pendingMu.Lock()
	defer wrapped.pendingMu.Unlock()
	if id == "" || (wrapped.destinationID != "" && wrapped.destinationID != id) {
		return nil, ErrAdmissionClosed
	}
	wrapped.destinationID = id
	return wrapped, nil
}

// Wrap installs catalog metadata authority over a qualified object store.
func Wrap(store storage.ObjectStore) (*Store, error) {
	w, err := New(store)
	if err != nil {
		return nil, err
	}
	if w.readOnly {
		return nil, ErrReadOnly
	}
	if wrapped, ok := store.(*Store); ok {
		return wrapped, nil
	}
	scope, err := NewMutationID()
	if err != nil {
		return nil, err
	}
	return &Store{ObjectStore: store, Writer: w, readScope: scope}, nil
}

func metadataKey(key string) bool {
	return strings.HasPrefix(key, "sessions/") && strings.HasSuffix(key, "/metadata.json")
}

// Get resolves catalog metadata and delegates immutable source reads.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	return s.GetLimited(ctx, key, 64<<20)
}

// GetLimited preserves bounded allocation for metadata and source reads.
func (s *Store) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if !metadataKey(key) {
		if _, ok := ctx.Value(readViewKey{}).(*readView); ok {
			if _, err := OpenSnapshot(ctx, s, nil); err != nil {
				return nil, err
			}
		}
		raw, err := s.Writer.bounded.GetLimited(ctx, key, limit)
		return raw, s.checkReadView(ctx, err)
	}
	entry, _, err := s.findForRead(ctx, key)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, storage.ErrNotFound
	}
	raw, err := s.Writer.readRef(ctx, entry.Metadata, limit)
	return raw, s.checkReadView(ctx, err)
}

// GetVersioned uses a unique catalog revision as metadata validator.
func (s *Store) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	return s.GetLimitedVersioned(ctx, key, 64<<20)
}

// GetLimitedVersioned preserves a bounded body and exact revision.
func (s *Store) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	if !metadataKey(key) {
		if _, ok := ctx.Value(readViewKey{}).(*readView); ok {
			if _, err := OpenSnapshot(ctx, s, nil); err != nil {
				return nil, "", err
			}
		}
		raw, v, err := s.Writer.versioned.GetCatalogVersion(ctx, key, limit)
		return raw, v.ETag, s.checkReadView(ctx, err)
	}
	entry, revision, err := s.findForRead(ctx, key)
	if err != nil {
		return nil, "", err
	}
	if entry == nil {
		return nil, "", storage.ErrNotFound
	}
	raw, err := s.Writer.readRef(ctx, entry.Metadata, limit)
	return raw, revision, s.checkReadView(ctx, err)
}

// Stat describes current catalog metadata or delegates source checksums.
func (s *Store) Stat(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if !metadataKey(key) {
		if statter, ok := s.ObjectStore.(storage.ObjectStatter); ok {
			return statter.Stat(ctx, key)
		}
	}
	raw, etag, err := s.GetVersioned(ctx, key)
	if err != nil {
		return storage.ObjectInfo{}, err
	}
	return storage.ObjectInfo{ETag: etag, Size: int64(len(raw)), SHA256: storage.SHA256Hex(raw)}, nil
}

// ListPage delegates the underlying paged listing capability.
func (s *Store) ListPage(ctx context.Context, prefix, continuation string, limit int32) (storage.ObjectPage, error) {
	if l, ok := s.ObjectStore.(storage.PageLister); ok {
		return l.ListPage(ctx, prefix, continuation, limit)
	}
	return storage.ObjectPage{}, errors.New("underlying store does not support page listing")
}

// ListRange delegates the underlying range listing capability.
func (s *Store) ListRange(ctx context.Context, prefix, after, through string) ([]storage.Object, error) {
	if l, ok := s.ObjectStore.(storage.RangeLister); ok {
		return l.ListRange(ctx, prefix, after, through)
	}
	return nil, errors.New("underlying store does not support range listing")
}

// ObjectKey preserves configured provider key composition.
func (s *Store) ObjectKey(key string) string {
	if k, ok := s.ObjectStore.(storage.ObjectKeyer); ok {
		return k.ObjectKey(key)
	}
	return key
}

// Put refuses unfrozen metadata and creates immutable source bytes.
func (s *Store) Put(ctx context.Context, key string, raw []byte) error {
	if strings.HasPrefix(key, "catalog-v4/") {
		return ErrAdmissionClosed
	}
	if metadataKey(key) {
		return errors.New("catalog metadata requires a frozen mutation")
	}
	if strings.HasPrefix(key, "sessions/") {
		if _, ok := ctx.Value(publicationClaimKey{}).(*publicationClaim); ok {
			return s.putClaimedSource(ctx, key, raw)
		}
		digest := storage.SHA256Hex(raw)
		invocation, e := NewMutationID()
		if e != nil {
			return e
		}
		owner := "source/" + invocation
		c := s.Writer.Coordinator()
		if err := c.Admit(ctx, owner, digest, []ObjectRef{{key, digest}}); err != nil {
			return err
		}
		_, err := s.Writer.conditional.PutConditional(ctx, key, raw, storage.PutCondition{CreateOnly: true})
		if err != nil {
			existing, e := s.Writer.bounded.GetLimited(ctx, key, int64(len(raw)))
			if e != nil {
				return errors.Join(err, e)
			}
			if string(existing) != string(raw) {
				return storage.ErrChecksumMismatch
			}
		}
		return c.Complete(ctx, owner, digest)
	}
	return s.ObjectStore.Put(ctx, key, raw)
}

// Delete refuses unfenced catalog and session object removal.
func (s *Store) Delete(ctx context.Context, key string) error {
	if strings.HasPrefix(key, "sessions/") || strings.HasPrefix(key, "catalog-v4/") {
		return ErrGCRequired
	}
	return s.ObjectStore.Delete(ctx, key)
}

// Publication binds the already persisted mutation to the metadata write made
// by the existing source-first machinery. Other metadata keys are refused.
func (s *Store) Publication(id, key, expected string) storage.ObjectStore {
	return publicationStore{s, id, key, expected}
}

type publicationStore struct {
	*Store
	id       string
	key      string
	expected string
}

func (s publicationStore) Put(ctx context.Context, key string, raw []byte) error {
	if !metadataKey(key) {
		return s.Store.Put(ctx, key, raw)
	}
	if key != s.key {
		return errors.New("publication metadata key changed")
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return err
	}
	canonical, err := archive.MetadataObjectKey(metadata.Harness.Name, metadata.SessionID)
	if err != nil || canonical != key {
		return errors.New("catalog metadata identity mismatch")
	}
	if _, err = metadata.SourceReferences(); err != nil {
		return err
	}
	if _, ok := ctx.Value(publicationClaimKey{}).(*publicationClaim); ok {
		return s.putClaimedMetadata(ctx, key, raw, &CatalogEntry{Summary: metadata})
	}
	ref, err := s.Writer.PutImmutable(ctx, KindMetadata, raw)
	if err != nil {
		return err
	}
	_, err = s.Writer.Commit(ctx, CatalogMutation{ID: s.id, SessionKey: key, ExpectedRevision: s.expected, Next: &CatalogEntry{Metadata: ref, Summary: metadata}})
	return err
}

// DeleteSession commits a tombstone before any physical source cleanup.
func (s *Store) DeleteSession(ctx context.Context, key string) error {
	entry, revision, err := s.Writer.Find(ctx, key)
	if err != nil {
		return err
	}
	if entry == nil {
		return nil
	}
	id, err := NewMutationID()
	if err != nil {
		return err
	}
	_, err = s.Writer.Commit(ctx, CatalogMutation{ID: id, SessionKey: key, ExpectedRevision: revision})
	return err
}

// CatalogMetadataAuthority reports that canonical metadata resolves via head.
func (s *Store) CatalogMetadataAuthority() bool { return true }

// CatalogBarrier obtains the underlying destination coordinator's complete
// global writer/history/pending barrier. A missing coordinator never grants GC.
func (s *Store) CatalogBarrier(ctx context.Context) (Barrier, error) {
	if _, err := s.Writer.preflightClock(ctx); err != nil {
		return nil, err
	}
	provider, ok := s.ObjectStore.(interface {
		CatalogBarrier(context.Context) (Barrier, error)
	})
	if !ok {
		owner, err := s.Writer.Coordinator().Seal(ctx)
		if err != nil {
			return nil, err
		}
		return s.Writer.Coordinator().HeldBarrier(owner), nil
	}
	barrier, err := provider.CatalogBarrier(ctx)
	if err != nil {
		return nil, err
	}
	if barrier == nil {
		return nil, errors.New("complete catalog destination coordinator is required")
	}
	return barrier, nil
}

// FreezeCatalogMutation observes the current revision before durable journaling.
func (s *Store) FreezeCatalogMutation(ctx context.Context, key string) (string, string, error) {
	_, revision, err := s.Writer.Find(ctx, key)
	if err != nil {
		return "", "", err
	}
	id, err := NewMutationID()
	return id, revision, err
}

// GetCatalogVersion preserves same-response head version authority for readers.
func (s *Store) GetCatalogVersion(ctx context.Context, key string, limit int64) ([]byte, storage.CatalogObjectVersion, error) {
	return s.Writer.versioned.GetCatalogVersion(ctx, key, limit)
}

// CatalogServerClock forwards the qualified provider clock.
func (s *Store) CatalogServerClock(ctx context.Context) (storage.CatalogTime, error) {
	return s.Writer.clock.CatalogServerClock(ctx)
}

// CatalogAtomicQualification forwards live provider qualification.
func (s *Store) CatalogAtomicQualification() error {
	return s.ObjectStore.(storage.AtomicCatalogProvider).CatalogAtomicQualification()
}

// PutConditional refuses adapter writes that bypass publication admission.
// Protocol internals hold exact ownership on the qualified raw writer.
func (*Store) PutConditional(context.Context, string, []byte, storage.PutCondition) (string, error) {
	return "", ErrAdmissionClosed
}

// CatalogReadScope identifies this destination authority without exposing
// credentials or object paths. Wrappers forward the same opaque instance key.
func (s *Store) CatalogReadScope() string { return s.readScope }
