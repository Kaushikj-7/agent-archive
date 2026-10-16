package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

// LabelEntry holds only safe current evidence and content-free lookup bookkeeping.
type LabelEntry struct {
	Context        agentapi.LabelContext `json:"context"`
	SourceChecksum string                `json:"source_checksum,omitempty"`
	SourceStamp    string                `json:"source_stamp,omitempty"`
	Label          archive.SessionLabel  `json:"label"`
	ObservedAt     time.Time             `json:"observed_at,omitempty"`
	NextAt         time.Time             `json:"next_at,omitempty"`
	Failures       uint8                 `json:"failures,omitempty"`
	Scope          string                `json:"scope"`
}

// ErrNoLabelPublication is LabelRevision for a session never published: it
// has no revision yet.
var ErrNoLabelPublication = errors.New("label publication unavailable")

// LabelRevision reads only the leading published summary and a content-free stat.
func (s *Store) LabelRevision(id string) (string, string, error) {
	if !safeFileComponent(id) {
		return "", "", errors.New("invalid label state identity")
	}
	summary, found, _, info, err := s.publishedPrefix(id)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", ErrNoLabelPublication
	}
	// A history source set is named too, like an ordinary source
	// (archive.ValidateSessionLabels).
	token := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())))
	checksum := summary.LabelSourceChecksum
	if len(checksum) != 64 {
		checksum = ""
	}
	return checksum, hex.EncodeToString(token[:]), nil
}

// LoadLabelPublication enforces an aggregate caller budget before a one-time context decode.
func (s *Store) LoadLabelPublication(id string, budget int64) (*Published, int64, error) {
	reservation := s.readReservation()
	if !safeFileComponent(id) {
		return nil, 0, errors.New("invalid label state identity")
	}
	info, err := os.Lstat(s.publishedPath(id))
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 || info.Size() > budget {
		return nil, 0, errors.New("label context read unavailable or over budget")
	}
	f, err := os.Open(s.publishedPath(id))
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, 0, errors.New("label context changed before read")
	}
	// Serialized input and decoded state coexist. The decoded charge remains
	// owned by the caller's scope while its Published value is still borrowed.
	keep := false
	if s.resourceBudget != nil {
		if err := s.resourceContext.Err(); err != nil {
			return nil, 0, err
		}
		if !reservation.Reserve(info.Size()) {
			return nil, 0, errStateBudget
		}
		defer reservation.Release(info.Size())
		if !reservation.Reserve(info.Size()) {
			return nil, 0, errStateBudget
		}
		defer func() {
			if !keep {
				reservation.Release(info.Size())
			}
		}()
	}
	// Retained state is plain JSON, including its native records. Limit the
	// actual read as well as the stat so growth cannot defeat the byte cap.
	data, err := io.ReadAll(io.LimitReader(f, info.Size()))
	if err != nil || int64(len(data)) != info.Size() {
		return nil, int64(len(data)), errors.New("label context changed during read")
	}
	after, err := f.Stat()
	if err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, int64(len(data)), errors.New("label context changed during read")
	}
	publishedStateLoads.Add(1)
	p := &Published{store: s, id: id, found: true}
	if err := decodeStateJSON(data, &p.state); err != nil {
		return nil, int64(len(data)), err
	}
	if p.state.PublicationVersion == 3 || p.state.PublicationVersion == 4 {
		if err := p.preserveStagedPublished(s.durableContext(), &p.state); err != nil {
			return nil, int64(len(data)), err
		}
	}
	if p.state.PublicationVersion == 2 {
		if err := p.preserveReadyPublished(s.durableContext(), &p.state); err != nil {
			return nil, int64(len(data)), err
		}
	}
	if s.resourceBudget != nil {
		if err := s.resourceContext.Err(); err != nil {
			return nil, int64(len(data)), err
		}
		keep = true
		*s.resourceReleases = append(*s.resourceReleases, func() { reservation.Release(info.Size()) })
	}
	return p, int64(len(data)), nil
}

func labelHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

var labelProducerVersion = regexp.MustCompile(`^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}([-+][a-zA-Z0-9.-]{1,32})?$`)

func validLabelContext(entry LabelEntry) bool {
	proof := entry.Context
	// Only bounded producer versions and content-free hashes enter durable context.
	if proof.Producer != "" && !labelProducerVersion.MatchString(proof.Producer) {
		return false
	}
	if proof.NativeID != "" {
		_, ok := archive.FilterSessionLabel(archive.SessionLabel{NativeID: proof.NativeID, State: archive.SessionLabelAbsent, Source: archive.SessionLabelIndex, Contract: "label-context"})
		if !ok {
			return false
		}
	}
	return labelHash(entry.Scope) && (entry.SourceChecksum == "" || labelHash(entry.SourceChecksum)) &&
		(entry.SourceStamp == "" || labelHash(entry.SourceStamp)) &&
		(proof.PreviewDigest == "" || labelHash(proof.PreviewDigest)) &&
		labelHash(proof.Contract) && entry.Failures <= 6
}

// LabelCache is collector-owned, bounded, and independent of publication retries.
type LabelCache struct {
	// TargetCursors retain only hashed group identities and admitted archive IDs.
	TargetCursors map[string]string `json:"target_cursors,omitempty"`
	Version       int               `json:"version"`
	// PriorityCursor rotates the first provider work group separately from batch coverage.
	PriorityCursor string                `json:"priority_cursor,omitempty"`
	Cursor         string                `json:"cursor,omitempty"`
	Entries        map[string]LabelEntry `json:"entries"`
}

// MaxLabelCacheEntries bounds persisted observations independently of admission.
const MaxLabelCacheEntries = 2048

// MaxLabelTargetCursors bounds independent within-group scheduling progress.
const MaxLabelTargetCursors = 64

func validLabelTargetCursors(cursors map[string]string) bool {
	if len(cursors) > MaxLabelTargetCursors {
		return false
	}
	for key, id := range cursors {
		if !labelHash(key) || !safeFileComponent(id) || len(id) > 256 {
			return false
		}
	}
	return true
}

// LoadLabels reads safe lookup state without native source access.
func (s *Store) LoadLabels() (LabelCache, error) {
	cache := LabelCache{Version: 1, Entries: map[string]LabelEntry{}}
	f, err := os.Open(filepath.Join(s.home, "session-labels.json"))
	if errors.Is(err, os.ErrNotExist) {
		return cache, nil
	}
	if err != nil {
		return cache, errors.New("read session label cache")
	}
	defer func() { _ = f.Close() }()
	reader := io.LimitReader(f, 4<<20)
	if json.NewDecoder(reader).Decode(&cache) != nil || cache.Version != 1 || !validLabelTargetCursors(cache.TargetCursors) || len(cache.Entries) > MaxLabelCacheEntries || (len(cache.Cursor) > 256 || len(cache.PriorityCursor) > 256) {
		return LabelCache{}, errors.New("invalid session label cache")
	}
	if cache.Entries == nil {
		cache.Entries = map[string]LabelEntry{}
	}
	for id, entry := range cache.Entries {
		if !safeFileComponent(id) || len(id) > 256 || !validLabelContext(entry) {
			delete(cache.Entries, id)
			continue
		}
		if entry.Label.State == "" && !reflect.DeepEqual(entry.Label, archive.SessionLabel{}) {
			delete(cache.Entries, id)
			continue
		}
		if entry.Label.State != "" {
			label, ok := archive.FilterSessionLabel(entry.Label)
			if !ok || !reflect.DeepEqual(label, entry.Label) || entry.ObservedAt.IsZero() {
				delete(cache.Entries, id)
			}
		}
	}
	return cache, nil
}

// SaveLabels atomically persists already-filtered observations under the collector lock.
func (s *Store) SaveLabels(cache LabelCache) error {
	if cache.Version != 1 || !validLabelTargetCursors(cache.TargetCursors) || len(cache.Entries) > MaxLabelCacheEntries || (len(cache.Cursor) > 256 || len(cache.PriorityCursor) > 256) {
		return errors.New("invalid session label cache")
	}
	for id, entry := range cache.Entries {
		if !safeFileComponent(id) || len(id) > 256 || !validLabelContext(entry) {
			return errors.New("invalid session label cache identity")
		}
		if entry.Label.State == "" && !reflect.DeepEqual(entry.Label, archive.SessionLabel{}) {
			return errors.New("unavailable cache entry contains label data")
		}
		if entry.Label.State != "" {
			label, ok := archive.FilterSessionLabel(entry.Label)
			if !ok || !reflect.DeepEqual(label, entry.Label) || entry.ObservedAt.IsZero() {
				return errors.New("session label cache requires filtered evidence")
			}
		}
	}
	prior, err := s.LoadLabels()
	if err == nil && reflect.DeepEqual(prior, cache) {
		return nil
	}
	encoded, err := json.Marshal(cache)
	if err != nil || len(encoded)+1 > 4<<20 {
		return errors.New("session label cache exceeds byte budget")
	}
	return local.WriteCompact(filepath.Join(s.home, "session-labels.json"), cache)
}
