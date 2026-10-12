package listingindex

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// V2Prefix contains revision-qualified entries; coverage is checked every read.
const V2Prefix = "listing/v2/"

// V3Prefix stores each immutable revision under its own session for cleanup.
const V3Prefix = "listing/v3/"

const v2Pointers = "listing/by-session-v2/"

// Revision contains only discovery summaries, never transcript or skill content.
type Revision struct {
	Key         string    `json:"-"`
	MetadataKey string    `json:"-"`
	CapturedAt  time.Time `json:"-"`
	Nonce       string    `json:"n,omitempty"`
	ETag        string    `json:"v"`
	Hash        string    `json:"h"`
	Activity    time.Time `json:"a"`
	NativeChild bool      `json:"c,omitempty"`
	Parent      string    `json:"p,omitempty"`
	Replay      bool      `json:"r,omitempty"`
	ProjectID   string    `json:"j"`
	RepoKey     string    `json:"k,omitempty"`
}

// ActivityTime matches the listing's activity-date policy.
func ActivityTime(m archive.Metadata) time.Time {
	if m.EndedAt != nil {
		return *m.EndedAt
	}
	if m.Origin == archive.SessionOriginImport && !m.StartedAt.IsZero() {
		return m.StartedAt
	}
	return m.CapturedAt
}

// NewRevision validates canonical identity and encodes an opaque validator.
// A fresh nonce prevents restoring identical bytes from reactivating a key
// which an older cleanup snapshot may already be deleting.
func NewRevision(key string, data []byte, etag string) (Revision, error) {
	return newRevision(key, data, etag, rand.Text())
}

func newRevision(key string, data []byte, etag, nonce string) (Revision, error) {
	r, err := revisionSummary(key, data, etag, nonce)
	if err != nil {
		return Revision{}, err
	}
	r.Key, err = revisionKey(r)
	if err != nil {
		return Revision{}, err
	}
	return r, nil
}

func revisionSummary(key string, data []byte, etag, nonce string) (Revision, error) {
	legacy, err := New(key, data)
	if err != nil {
		return Revision{}, err
	}
	if etag == "" {
		return Revision{}, errors.New("store returned no metadata revision validator")
	}
	var m archive.Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return Revision{}, err
	}
	return Revision{Nonce: nonce, MetadataKey: key, CapturedAt: m.CapturedAt, ETag: etag, Hash: legacy.Hash, Activity: ActivityTime(m), NativeChild: m.NativeChild, Parent: m.ParentSessionID, Replay: m.Replay != nil, ProjectID: m.ProjectID, RepoKey: m.RepoKey}, nil
}

const revisionComponentLimit = 255

// revisionKey preserves session ownership and timestamp order while splitting
// only the base64url summary into filesystem-compatible path components.
func revisionKey(r Revision) (string, error) {
	encoded, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	parts := strings.Split(r.MetadataKey, "/")
	reverse := fmt.Sprintf("%019d", maxTime-uint64(r.CapturedAt.UnixNano()))
	key := V3Prefix + parts[1] + "/" + parts[2] + "/" + reverse + "/" + splitSummary(base64.RawURLEncoding.EncodeToString(encoded))
	if len(key) > 1024 {
		return "", errors.New("listing entry exceeds object key limit")
	}
	for component := range strings.SplitSeq(key, "/") {
		if len(component) > revisionComponentLimit {
			return "", errors.New("listing entry exceeds object component limit")
		}
	}
	return key, nil
}

func splitSummary(summary string) string {
	var chunks []string
	for len(summary) > revisionComponentLimit {
		chunks = append(chunks, summary[:revisionComponentLimit])
		summary = summary[revisionComponentLimit:]
	}
	return strings.Join(append(chunks, summary), "/")
}

// ParseRevision rejects unsupported and noncanonical encodings.
func ParseRevision(key string) (Revision, error) {
	if (!strings.HasPrefix(key, V2Prefix) && !strings.HasPrefix(key, V3Prefix)) || len(key) > 1024 {
		return Revision{}, errors.New("unsupported listing entry")
	}
	parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(key, V2Prefix), V3Prefix), "/")
	if strings.HasPrefix(key, V3Prefix) && len(parts) >= 4 {
		if len(parts) > 4 {
			summary := strings.Join(parts[3:], "")
			if strings.Join(parts[3:], "/") != splitSummary(summary) {
				return Revision{}, errors.New("noncanonical listing summary components")
			}
			parts = append(parts[:3], summary)
		}
		parts[0], parts[1], parts[2] = parts[2], parts[0], parts[1]
	}
	if len(parts) != 4 || len(parts[0]) != 19 {
		return Revision{}, errors.New("invalid listing entry")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return Revision{}, err
	}
	var r Revision
	if err = json.Unmarshal(encoded, &r); err != nil {
		return Revision{}, err
	}
	legacy, err := Parse(Prefix + strings.Join(parts[:3], "/") + "/" + r.Hash + ".json")
	if err != nil {
		return Revision{}, err
	}
	if r.ETag == "" || r.Activity.IsZero() || r.Nonce != "" && (len(r.Nonce) != 26 || strings.Trim(r.Nonce, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "") {
		return Revision{}, errors.New("invalid listing summary")
	}
	canonical, _ := json.Marshal(r)
	if base64.RawURLEncoding.EncodeToString(canonical) != parts[3] {
		return Revision{}, errors.New("noncanonical listing summary")
	}
	r.Key = key
	r.MetadataKey = legacy.MetadataKey
	r.CapturedAt = legacy.CapturedAt
	return r, nil
}

// ValidateMetadata checks identity, schema, digest and every discovery field
// against canonical bytes, preserving this immutable publication identity.
func (r Revision) ValidateMetadata(data []byte) error {
	check, err := revisionSummary(r.MetadataKey, data, r.ETag, r.Nonce)
	if err != nil {
		return err
	}
	if !check.SameSummary(r) {
		return errors.New("metadata does not match listing revision")
	}
	return nil
}

// SameSummary permits concurrent publications of the same canonical revision
// while rejecting different discovery claims for one provider validator.
func (r Revision) SameSummary(other Revision) bool {
	if r.MetadataKey != other.MetadataKey || !r.CapturedAt.Equal(other.CapturedAt) {
		return false
	}
	r.Nonce, other.Nonce = "", ""
	a, _ := json.Marshal(r)
	b, _ := json.Marshal(other)
	return string(a) == string(b)
}

func revisionPointerPrefix(r Revision) string {
	parts := strings.Split(r.MetadataKey, "/")
	return v2Pointers + parts[1] + "/" + parts[2] + "/"
}

func revisionPointer(r Revision) string {
	return revisionPointerPrefix(r) + storage.SHA256Hex([]byte(r.Key))
}

// PutRevision writes one immutable session-addressed v3 hint.
func PutRevision(ctx context.Context, store storage.ObjectStore, r Revision) error {
	parsed, err := ParseRevision(r.Key)
	if err != nil {
		return err
	}
	r = parsed
	if !strings.HasPrefix(r.Key, V3Prefix) {
		return errors.New("legacy listing revisions are read-only")
	}
	canonicalKey, err := revisionKey(r)
	if err != nil {
		return err
	}
	if r.Key != canonicalKey {
		return errors.New("legacy listing encodings are read-only; rebuild the index")
	}
	fullKey := r.Key
	if keyer, ok := store.(interface{ ObjectKey(string) string }); ok {
		fullKey = keyer.ObjectKey(r.Key)
	}
	if len(fullKey) > 1024 {
		return errors.New("listing entry exceeds full object key limit")
	}
	for component := range strings.SplitSeq(fullKey, "/") {
		if len(component) > revisionComponentLimit {
			return errors.New("listing entry exceeds full object component limit")
		}
	}
	return store.Put(ctx, r.Key, nil)
}

// PublishRevision confirms the exact canonical bytes using a response validator.
// A concurrent replacement creates harmless stale hints, detected by coverage.
func PublishRevision(ctx context.Context, store storage.ObjectStore, key string, data []byte) error {
	getter, ok := store.(storage.VersionedGetter)
	if !ok {
		return errors.New("store cannot return metadata revision validators")
	}
	stored, etag, err := getter.GetVersioned(ctx, key)
	if err != nil {
		return err
	}
	if storage.SHA256Hex(stored) != storage.SHA256Hex(data) {
		return errors.New("canonical metadata changed during index publication")
	}
	r, err := NewRevision(key, stored, etag)
	if err != nil {
		return err
	}
	return RepairRevision(ctx, store, r)
}

// RepairRevision publishes a fresh session-addressed hint and retires at most
// 32 earlier entries. Its snapshot cannot include a later writer's identity.
func RepairRevision(ctx context.Context, store storage.ObjectStore, r Revision) error {
	return repairRevision(ctx, store, r, nil)
}

func repairRevision(ctx context.Context, store storage.ObjectStore, r Revision, legacy []storage.Object) error {
	parsed, err := ParseRevision(r.Key)
	if err != nil {
		return err
	}
	r = parsed
	r.Nonce = rand.Text()
	r.Key, err = revisionKey(r)
	if err != nil {
		return err
	}
	candidates, err := store.List(ctx, revisionSessionPrefix(r.MetadataKey))
	if err != nil {
		return err
	}
	candidates = append(candidates, legacy...)
	if err := PutRevision(ctx, store, r); err != nil {
		return err
	}
	if statter, ok := store.(storage.ObjectStatter); ok {
		info, err := statter.Stat(ctx, r.MetadataKey)
		if err != nil {
			return err
		}
		if info.ETag != r.ETag {
			return errors.New("metadata changed before listing maintenance")
		}
	} else if len(candidates) != 0 {
		return errors.New("store cannot confirm listing maintenance validator")
	}
	return retireCandidates(ctx, store, r.MetadataKey, candidates)
}

func revisionSessionPrefix(metadataKey string) string {
	parts := strings.Split(metadataKey, "/")
	return V3Prefix + parts[1] + "/" + parts[2] + "/"
}

// DeleteRevision removes one validated auxiliary revision and its exact pointer.
func DeleteRevision(ctx context.Context, store storage.ObjectStore, r Revision) error {
	parsed, err := ParseRevision(r.Key)
	if err != nil {
		return err
	}
	r = parsed
	if err := store.Delete(ctx, r.Key); err != nil {
		return err
	}
	if strings.HasPrefix(r.Key, V3Prefix) {
		return nil
	}
	return store.Delete(ctx, revisionPointer(r))
}
