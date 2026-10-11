package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/local"
)

const packedOverlaySentinel = ".packed-epoch"

const packedSessionIndexDir = "sessions-packed-v1"

const packedSessionIndexThreshold = 4096

const packedSessionIndexShards = 256

const packedSessionIndexMaxBytes = 2 * 1024 * 1024

const packedSessionIndexMaxEntries = 1024

type packedSessionIndex struct {
	Version   int                                   `json:"version"`
	Epoch     string                                `json:"epoch"`
	Revision  string                                `json:"revision"`
	Inventory string                                `json:"inventory"`
	Entries   map[string]qualifiedSessionIndexEntry `json:"entries"`
	Fallback  bool                                  `json:"fallback,omitempty"`
}

func recoveryMarkerVersion(version int) bool { return version == 1 || version == 2 }

func packedIndexHash(key agentmeta.SessionKey) string {
	sum := sha256.Sum256(key.Encoding())
	return hex.EncodeToString(sum[:])
}

func packedIndexPath(home, shard string) string {
	return filepath.Join(home, packedSessionIndexDir, shard+".idx")
}

func (s *Store) packedSessionIndexEntry(key agentmeta.SessionKey) (qualifiedSessionIndexEntry, bool, error) {
	var marker sessionIndexMarker
	err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker)
	if errors.Is(err, os.ErrNotExist) {
		return qualifiedSessionIndexEntry{}, false, nil
	}
	if err != nil {
		return qualifiedSessionIndexEntry{}, false, ErrSessionIndexRecoveryRequired
	}
	if marker.Version == 1 {
		return qualifiedSessionIndexEntry{}, false, nil
	}
	if marker.Version != 2 || !safeFileComponent(marker.PackedEpoch) || marker.PackedRevision == "" || marker.PackedInventory == "" {
		return qualifiedSessionIndexEntry{}, false, ErrSessionIndexRecoveryRequired
	}
	hash := packedIndexHash(key)
	entry, found, err := s.readPackedEntry(hash, marker)
	if err != nil {
		return qualifiedSessionIndexEntry{}, false, err
	}
	if !found {
		if err := s.packedMissAllowed(key, marker); err != nil {
			return entry, false, err
		}
		return entry, false, nil
	}
	if err := entry.validate(key); err != nil {
		return entry, false, err
	}
	valid, err := s.matchingRegistration(key, entry.ArchiveSessionID)
	if err != nil {
		return entry, false, err
	}
	if valid {
		entry.Reservation = ""
		return entry, true, nil
	}
	if entry.Reservation == "" {
		if err := s.packedMissAllowed(key, marker); err != nil {
			return entry, false, err
		}
		return qualifiedSessionIndexEntry{}, false, nil
	}
	// A packed reservation is usable only while its directly referenced durable
	// candidate and parent still prove it. Removing either cannot revive a child.
	candidate, present, err := readJSON[SubagentCandidate](s.subagentCandidatePath(entry.ArchiveSessionID))
	if err != nil || !present {
		return entry, false, ErrSessionIndexRecoveryRequired
	}
	actual, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID)
	if err != nil || actual != key || candidate.ArchiveSessionID != entry.ArchiveSessionID {
		return entry, false, ErrSessionIndexRecoveryRequired
	}
	parentKey, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.ParentNativeSessionID)
	if err != nil {
		return entry, false, ErrSessionIndexRecoveryRequired
	}
	valid, err = s.matchingCandidateParent(parentKey, candidate.ParentArchiveSessionID)
	if err != nil || !valid {
		return entry, false, ErrSessionIndexRecoveryRequired
	}
	return entry, true, nil
}

const packedRecordIndexBytes = 76

const packedIndexMagic = "AASIDX01"

type packedIndexHeader struct {
	Version       int    `json:"version"`
	Epoch         string `json:"epoch"`
	Revision      string `json:"revision"`
	Inventory     string `json:"inventory"`
	Count         int    `json:"count"`
	Bytes         int    `json:"bytes"`
	IndexChecksum string `json:"index_checksum"`
	Checksum      string `json:"checksum"`
	Fallback      bool   `json:"fallback,omitempty"`
}

func (header packedIndexHeader) checksum() string {
	header.Checksum = ""
	return phaseFingerprint(header)
}

func (header packedIndexHeader) valid(marker sessionIndexMarker) bool {
	return header.Version == 1 && header.Epoch == marker.PackedEpoch && header.Revision == marker.PackedRevision && header.Inventory == marker.PackedInventory && header.Count >= 0 && header.Count <= packedSessionIndexMaxEntries && header.Bytes >= 0 && header.Bytes <= packedSessionIndexMaxBytes && header.Checksum != "" && header.Checksum == header.checksum()
}

type packedIndexReader struct {
	file      *os.File
	table     []byte
	dataStart int64
	dataBytes int
	data      []byte
	fallback  bool
	epoch     string
	revision  string
	inventory string
}

func (s *Store) openPackedIndex(shard string, marker sessionIndexMarker) (*packedIndexReader, error) {
	file, err := os.Open(packedIndexPath(s.home, shard))
	if err != nil {
		return nil, ErrSessionIndexRecoveryRequired
	}
	good := false
	defer func() {
		if !good {
			_ = file.Close()
		}
	}()
	var prefix [12]byte
	if _, err := io.ReadFull(file, prefix[:]); err != nil || string(prefix[:8]) != packedIndexMagic {
		return nil, ErrSessionIndexRecoveryRequired
	}
	length := int(binary.BigEndian.Uint32(prefix[8:]))
	if length <= 0 || length > 4096 {
		return nil, ErrSessionIndexRecoveryRequired
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(file, data); err != nil {
		return nil, ErrSessionIndexRecoveryRequired
	}
	var header packedIndexHeader
	if json.Unmarshal(data, &header) != nil || !header.valid(marker) {
		return nil, ErrSessionIndexRecoveryRequired
	}
	table := make([]byte, header.Count*packedRecordIndexBytes)
	if _, err := io.ReadFull(file, table); err != nil {
		return nil, ErrSessionIndexRecoveryRequired
	}
	sum := sha256.Sum256(table)
	if hex.EncodeToString(sum[:]) != header.IndexChecksum {
		return nil, ErrSessionIndexRecoveryRequired
	}
	start := int64(12 + length + len(table))
	info, err := file.Stat()
	if err != nil || info.Size() != start+int64(header.Bytes) || info.Size() > packedSessionIndexMaxBytes {
		return nil, ErrSessionIndexRecoveryRequired
	}
	if err := validatePackedTable(table, shard, header.Count, header.Bytes); err != nil {
		return nil, err
	}
	good = true
	return &packedIndexReader{file: file, table: table, dataStart: start, dataBytes: header.Bytes, fallback: header.Fallback, epoch: header.Epoch, revision: header.Revision, inventory: header.Inventory}, nil
}

func validatePackedTable(table []byte, shard string, count, dataBytes int) error {
	if dataBytes < 0 || dataBytes > math.MaxInt32 {
		return ErrSessionIndexRecoveryRequired
	}
	shardBytes, decodeErr := hex.DecodeString(shard)
	if decodeErr != nil || len(shardBytes) != 1 {
		return ErrSessionIndexRecoveryRequired
	}
	var offset uint64
	for i := range count {
		row := table[i*packedRecordIndexBytes : (i+1)*packedRecordIndexBytes]
		if row[0] != shardBytes[0] || binary.BigEndian.Uint64(row[32:40]) != offset {
			return ErrSessionIndexRecoveryRequired
		}
		if i > 0 && bytes.Compare(table[(i-1)*packedRecordIndexBytes:(i-1)*packedRecordIndexBytes+32], row[:32]) >= 0 {
			return ErrSessionIndexRecoveryRequired
		}
		size := uint64(binary.BigEndian.Uint32(row[40:44]))
		if size == 0 || size > uint64(dataBytes) || offset+size > uint64(dataBytes) {
			return ErrSessionIndexRecoveryRequired
		}
		offset += size
	}
	if offset != uint64(dataBytes) {
		return ErrSessionIndexRecoveryRequired
	}
	return nil
}

func (reader *packedIndexReader) entry(index int) (qualifiedSessionIndexEntry, error) {
	row := reader.table[index*packedRecordIndexBytes : (index+1)*packedRecordIndexBytes]
	length := int(binary.BigEndian.Uint32(row[40:44]))
	data := make([]byte, length)

	rawOffset := binary.BigEndian.Uint64(row[32:40])
	if rawOffset > math.MaxInt32 {
		return qualifiedSessionIndexEntry{}, ErrSessionIndexRecoveryRequired
	}
	if reader.data != nil {
		offset := int(rawOffset)
		copy(data, reader.data[offset:offset+length])
	} else if _, err := reader.file.ReadAt(data, reader.dataStart+int64(rawOffset)); err != nil {
		return qualifiedSessionIndexEntry{}, ErrSessionIndexRecoveryRequired
	}

	sum := sha256.Sum256(data)
	if !bytes.Equal(sum[:], row[44:76]) {
		return qualifiedSessionIndexEntry{}, ErrSessionIndexRecoveryRequired
	}
	var entry qualifiedSessionIndexEntry
	if json.Unmarshal(data, &entry) != nil {
		return entry, ErrSessionIndexRecoveryRequired
	}
	key := agentmeta.SessionKey{Agent: entry.Agent, NativeID: entry.NativeID}
	if key.Validate() != nil || packedIndexHash(key) != hex.EncodeToString(row[:32]) || entry.Version != 1 || entry.Recovery || entry.Absent {
		return entry, ErrSessionIndexRecoveryRequired
	}
	if !entry.Conflict && entry.validate(key) != nil {
		return entry, ErrSessionIndexRecoveryRequired
	}
	return entry, nil
}

func (s *Store) readPackedEntry(hash string, marker sessionIndexMarker) (qualifiedSessionIndexEntry, bool, error) {
	reader, err := s.openPackedIndex(hash[:2], marker)
	if err != nil {
		return qualifiedSessionIndexEntry{}, false, err
	}
	defer func() { _ = reader.file.Close() }()
	key, err := hex.DecodeString(hash)
	if err != nil || len(key) != 32 {
		return qualifiedSessionIndexEntry{}, false, ErrSessionIndexRecoveryRequired
	}
	count := len(reader.table) / packedRecordIndexBytes
	index := sort.Search(count, func(i int) bool {
		return bytes.Compare(reader.table[i*packedRecordIndexBytes:i*packedRecordIndexBytes+32], key) >= 0
	})
	if index == count || !bytes.Equal(reader.table[index*packedRecordIndexBytes:index*packedRecordIndexBytes+32], key) {
		return qualifiedSessionIndexEntry{}, false, nil
	}
	entry, err := reader.entry(index)
	return entry, err == nil, err
}

func (s *Store) readPackedSessionIndex(shard string, marker sessionIndexMarker) (packedSessionIndex, error) {
	index := packedSessionIndex{Entries: make(map[string]qualifiedSessionIndexEntry)}
	reader, err := s.openPackedIndex(shard, marker)
	if err != nil {
		return index, err
	}
	defer func() { _ = reader.file.Close() }()
	index.Fallback, index.Epoch, index.Revision, index.Inventory = reader.fallback, reader.epoch, reader.revision, reader.inventory
	reader.data, err = io.ReadAll(io.LimitReader(reader.file, int64(reader.dataBytes)+1))
	if err != nil || len(reader.data) != reader.dataBytes {
		return index, ErrSessionIndexRecoveryRequired
	}
	for i := range len(reader.table) / packedRecordIndexBytes {
		entry, err := reader.entry(i)
		if err != nil {
			return index, err
		}
		hash := hex.EncodeToString(reader.table[i*packedRecordIndexBytes : i*packedRecordIndexBytes+32])
		index.Entries[hash] = entry
	}
	return index, nil
}

func encodePackedIndex(index packedSessionIndex) ([]byte, error) {
	keys := make([]string, 0, len(index.Entries))
	for key := range index.Entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	table := make([]byte, len(keys)*packedRecordIndexBytes)
	var data bytes.Buffer
	for i, key := range keys {
		hash, err := hex.DecodeString(key)
		if err != nil || len(hash) != 32 {
			return nil, ErrSessionIndexRecoveryRequired
		}
		record, err := json.Marshal(index.Entries[key])
		if err != nil {
			return nil, err
		}
		offset, length := data.Len(), len(record)
		if offset < 0 || offset > math.MaxInt32 || length < 0 || length > math.MaxInt32 {
			return nil, ErrSessionIndexRecoveryRequired
		}
		row := table[i*packedRecordIndexBytes : (i+1)*packedRecordIndexBytes]
		copy(row[:32], hash)
		binary.BigEndian.PutUint64(row[32:40], uint64(offset))
		binary.BigEndian.PutUint32(row[40:44], uint32(length))
		sum := sha256.Sum256(record)
		copy(row[44:76], sum[:])
		_, _ = data.Write(record)
	}
	sum := sha256.Sum256(table)
	header := packedIndexHeader{Version: 1, Epoch: index.Epoch, Revision: index.Revision, Inventory: index.Inventory, Count: len(keys), Bytes: data.Len(), Fallback: index.Fallback, IndexChecksum: hex.EncodeToString(sum[:])}
	header.Checksum = header.checksum()
	metadata, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	metadataLength := len(metadata)
	if metadataLength < 0 || metadataLength > math.MaxInt32 || metadataLength > 4096 {
		return nil, ErrSessionIndexRecoveryRequired
	}
	result := make([]byte, 12+len(metadata)+len(table)+data.Len())
	copy(result, packedIndexMagic)
	binary.BigEndian.PutUint32(result[8:12], uint32(metadataLength))
	offset := 12
	copy(result[offset:], metadata)
	offset += len(metadata)
	copy(result[offset:], table)
	offset += len(table)
	copy(result[offset:], data.Bytes())
	return result, nil
}

// Physical per-key files dominate packed authority, including damage. The same
// logical snapshot is compared before staging and again under the request lock.
func (s *Store) readIndexSnapshot(path string) (fileSnapshot, error) {
	return s.readQualifiedIndexSnapshot(path, true)
}

func (s *Store) readQualifiedIndexSnapshot(path string, validateOwner bool) (fileSnapshot, error) {
	before, err := readSnapshot(path)
	if err != nil || before.found {
		return before, err
	}
	if filepath.Dir(path) != filepath.Join(s.home, "sessions-v1") {
		return before, nil
	}
	name := filepath.Base(path)
	if len(name) != 69 || filepath.Ext(name) != ".json" {
		return before, ErrSessionIndexRecoveryRequired
	}
	var marker sessionIndexMarker
	err = local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker)
	if errors.Is(err, os.ErrNotExist) || (err == nil && marker.Version == 1) {
		return before, nil
	}
	if err != nil || marker.Version != 2 {
		return before, ErrSessionIndexRecoveryRequired
	}
	entry, found, err := s.readPackedEntry(name[:64], marker)
	if err != nil {
		if !validateOwner && !marker.Complete {
			// A new census epoch invalidates old aggregate bytes. Generation
			// callers already prove routing from their journal or complete
			// lineage census; an invalid derived shard is not another owner.
			return before, nil
		}
		if !marker.Complete {
			if _, statErr := os.Stat(packedIndexPath(s.home, name[:2])); errors.Is(statErr, os.ErrNotExist) {
				return before, nil
			}
		}
		return before, err
	}
	if !found {
		return before, nil
	}
	if validateOwner {
		key := agentmeta.SessionKey{Agent: entry.Agent, NativeID: entry.NativeID}
		entry, found, err = s.packedSessionIndexEntry(key)
		if err != nil || !found {
			return fileSnapshot{}, err
		}
	}
	data, err := json.Marshal(entry)
	return fileSnapshot{data: data, found: true}, err
}

func (s *Store) preparePackedSessionIndex(ctx context.Context, revision, inventory string) (sessionIndexMarker, error) {
	var marker sessionIndexMarker
	if err := local.Read(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil {
		return marker, err
	}
	if marker.Version == 2 && marker.PackedRevision == revision && marker.PackedInventory == inventory {
		return marker, s.ensurePackedOverlayDirectory(marker)
	}
	epoch, err := local.ID()
	if err != nil {
		return marker, err
	}
	err = s.writeUnderLock(lockedWrite{
		lock: func() (func(), error) { return s.namedLockWait("hooks.lock", time.Second) }, path: filepath.Join(s.home, sessionIndexMarkerFile), check: ctx.Err,
		change: func(current fileSnapshot) (any, bool, error) {
			if json.Unmarshal(current.data, &marker) != nil || !recoveryMarkerVersion(marker.Version) || marker.Complete {
				return nil, false, ErrSessionIndexRecoveryRequired
			}
			marker.Version = 2
			marker.PackedEpoch = epoch
			marker.PackedRevision = revision
			marker.PackedInventory = inventory
			return marker, true, nil
		},
	})
	if err == nil {
		err = s.ensurePackedOverlayDirectory(marker)
	}
	return marker, err
}

var errPackedSlicePending = errors.New("packed application slice pending")

func (s *Store) preparePackedShardSlice(ctx context.Context, marker sessionIndexMarker, owners map[agentmeta.SessionKey][]string, candidates []SubagentCandidate, deadline time.Time) ([]byte, error) {

	sizing, oversized, err := packedShardSizing(marker, owners, candidates)
	if err != nil {
		return nil, err
	}

	if oversized {
		sizing.Fallback = true
		empty, err := encodePackedIndex(sizing)
		if err != nil {
			return nil, err
		}
		return empty, nil
	}
	entries := make(map[string]qualifiedSessionIndexEntry, len(owners)+len(candidates))
	if err := s.packedShardOwners(ctx, owners, entries, deadline); err != nil {
		return nil, fmt.Errorf("owners: %w", err)
	}
	if err := s.packedShardCandidates(ctx, candidates, entries, marker, deadline); err != nil {
		return nil, err
	}
	index := packedSessionIndex{Version: 1, Epoch: marker.PackedEpoch, Revision: marker.PackedRevision, Inventory: marker.PackedInventory, Entries: entries}
	data, err := encodePackedIndex(index)
	if err != nil {
		return nil, err
	}
	if len(entries) > packedSessionIndexMaxEntries || len(data) > packedSessionIndexMaxBytes {
		// Large/skewed existing identities remain supported by the ordinary durable
		// namespace. Bounds select storage, never reject otherwise valid input.

		// The marker is incomplete. Publish an empty current-epoch shard first so
		// ordinary locked repairs see logical absence, not an obsolete aggregate.
		// Failure leaves the certificate incomplete and the cursor on this shard.
		index.Entries = map[string]qualifiedSessionIndexEntry{}
		index.Fallback = true
		empty, err := encodePackedIndex(index)
		if err != nil {
			return nil, err
		}
		return empty, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func (s *Store) packedShardOwners(ctx context.Context, owners map[agentmeta.SessionKey][]string, entries map[string]qualifiedSessionIndexEntry, deadline time.Time) error {
	ownerKeys, _ := inventoryFingerprint(owners)
	for _, key := range ownerKeys {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return errPackedSlicePending
		}
		ids := owners[key]
		if err := ctx.Err(); err != nil {
			return err
		}
		hash := packedIndexHash(key)
		entry := indexEntry(key, ids[0])
		active, protected, err := s.generationOwner(key, ids)
		if err != nil {
			return err
		}
		if protected {
			if active == "" {
				// Packed files encode owners, not absence certificates. Preserve
				// the explicit retired-head absence as a fenced per-key overlay.
				if _, err := s.recoverGenerationOwners(key, ids); err != nil {
					return err
				}
				continue
			}
			entry.ArchiveSessionID = active
		} else if len(ids) > 1 {
			entry.ArchiveSessionID = ""
			entry.Conflict = true
		}
		// Existing overrides keep their authority; requests are repaired by the
		// original locked path, never hidden by an aggregate replacement.
		physical, err := readSnapshot(qualifiedSessionIndexPath(s.home, key))
		if err != nil {
			return err
		}
		if physical.found {
			var prior qualifiedSessionIndexEntry
			if json.Unmarshal(physical.data, &prior) != nil || prior.validate(key) != nil || prior.Reservation != "" || prior != entry {
				if err := s.recoverRegistrationOwners(key, ids); err != nil {
					return err
				}
			}
		}
		entries[hash] = entry
	}
	return nil
}

func (s *Store) packedShardCandidates(ctx context.Context, candidates []SubagentCandidate, entries map[string]qualifiedSessionIndexEntry, marker sessionIndexMarker, deadline time.Time) error {

	for _, candidate := range candidates {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return errPackedSlicePending
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		key, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID)
		if err != nil || !safeFileComponent(candidate.ArchiveSessionID) {
			return ErrSessionIndexRecoveryRequired
		}
		hash := packedIndexHash(key)
		if existing, found := entries[hash]; found {
			if existing.Conflict || existing.ArchiveSessionID != candidate.ArchiveSessionID {
				return ErrSessionIdentityConflict
			}
			continue
		}
		parentKey, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.ParentNativeSessionID)
		if err != nil {
			return err
		}
		valid, err := s.matchingCandidateParent(parentKey, candidate.ParentArchiveSessionID)
		if err != nil {
			return err
		}
		if !valid {
			continue
		}
		physical, err := readSnapshot(qualifiedSessionIndexPath(s.home, key))
		if err != nil {
			return err
		}
		if physical.found {
			var prior qualifiedSessionIndexEntry
			if json.Unmarshal(physical.data, &prior) == nil && prior.validate(key) == nil && !prior.Absent {
				if prior.ArchiveSessionID != candidate.ArchiveSessionID {
					return ErrSessionIdentityConflict
				}
			} else if err := s.recoverCandidateIndex(ctx, candidate); err != nil {
				return err
			}
		}
		entry := indexEntry(key, candidate.ArchiveSessionID)
		entry.Reservation = marker.PackedEpoch
		entries[hash] = entry
	}
	return nil
}

func packedShardSizing(marker sessionIndexMarker, owners map[agentmeta.SessionKey][]string, candidates []SubagentCandidate) (packedSessionIndex, bool, error) {
	// Decide fallback before any per-key native repairs. Its ordinary writes
	// are applied independently in the cursor's next phase.
	sizing := packedSessionIndex{Epoch: marker.PackedEpoch, Revision: marker.PackedRevision, Inventory: marker.PackedInventory, Entries: map[string]qualifiedSessionIndexEntry{}}
	oversized := len(owners)+len(candidates) > packedSessionIndexMaxEntries
	bytes := 4096 + (len(owners)+len(candidates))*packedRecordIndexBytes
	measure := func(entry qualifiedSessionIndexEntry) error {
		if oversized {
			return nil
		}
		if len(entry.NativeID)+len(entry.ArchiveSessionID)+len(entry.Reservation) > packedSessionIndexMaxBytes {
			oversized = true
			return nil
		}
		data, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		bytes += len(data)
		oversized = bytes > packedSessionIndexMaxBytes
		return nil
	}
	for key, ids := range owners {
		if err := measure(indexEntry(key, ids[0])); err != nil {
			return sizing, false, err
		}
	}
	for _, candidate := range candidates {
		key, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID)
		if err != nil {
			return sizing, false, err
		}
		entry := indexEntry(key, candidate.ArchiveSessionID)
		entry.Reservation = marker.PackedEpoch
		if err := measure(entry); err != nil {
			return sizing, false, err
		}
	}
	return sizing, oversized, nil
}

func packedRecoverySources(inventory map[agentmeta.SessionKey][]string, candidates []SubagentCandidate) ([packedSessionIndexShards]map[agentmeta.SessionKey][]string, [packedSessionIndexShards][]SubagentCandidate, error) {
	var owners [packedSessionIndexShards]map[agentmeta.SessionKey][]string
	var children [packedSessionIndexShards][]SubagentCandidate
	for key, ids := range inventory {
		hash := packedIndexHash(key)
		bytes, _ := hex.DecodeString(hash[:2])
		i := int(bytes[0])
		if owners[i] == nil {
			owners[i] = make(map[agentmeta.SessionKey][]string)
		}
		owners[i][key] = ids
	}
	for _, candidate := range candidates {
		key, err := agentmeta.NewSessionKey(candidate.Harness.Name, candidate.NativeSessionID)
		if err != nil {
			return owners, children, err
		}
		hash := packedIndexHash(key)
		bytes, _ := hex.DecodeString(hash[:2])
		i := int(bytes[0])
		children[i] = append(children[i], candidate)
	}
	return owners, children, nil
}

func packedShardName(index int) string { return fmt.Sprintf("%02x", index) }

func (s *Store) validatePackedPrefix(marker sessionIndexMarker, count int) (int, error) {
	for i := range count {
		if _, err := s.readPackedSessionIndex(packedShardName(i), marker); err != nil {
			return i, err
		}
	}
	return count, nil
}

// The sentinel names a single physical index, never a cached registration.
// Hook health checks use direct reads and never enumerate the namespace.
func packedOverlayAnchor(value string, marker sessionIndexMarker) (string, bool) {
	if value == marker.PackedEpoch+":empty" {
		return "", true
	}
	prefix := marker.PackedEpoch + ":overrides:"
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	hash := strings.TrimPrefix(value, prefix)
	decoded, err := hex.DecodeString(hash)
	return hash, err == nil && len(decoded) == sha256.Size && hash == strings.ToLower(hash)
}

func (s *Store) packedAnchorHealthy(hash string) bool {
	var entry qualifiedSessionIndexEntry
	if local.Read(filepath.Join(s.home, "sessions-v1", hash+".json"), &entry) != nil {
		return false
	}
	key := agentmeta.SessionKey{Agent: entry.Agent, NativeID: entry.NativeID}
	if key.Validate() != nil || packedIndexHash(key) != hash {
		return false
	}
	err := entry.validate(key)
	return err == nil || errors.Is(err, ErrSessionIdentityConflict)
}

func (s *Store) ensurePackedOverlayDirectory(marker sessionIndexMarker) error {
	before, err := readSnapshot(filepath.Join(s.home, "sessions-v1", packedOverlaySentinel))
	if err != nil {
		return err
	}
	revision, err := s.sessionMembershipRevision()
	if err != nil {
		return err
	}
	anchor, err := s.selectPackedOverlayAnchor()
	if err != nil {
		return err
	}
	value := marker.PackedEpoch + ":empty"
	if anchor != "" {
		value = marker.PackedEpoch + ":overrides:" + anchor
	}
	if before.found && string(before.data) == value {
		return nil
	}
	return s.writePackedOverlayState(marker, value, before, revision)
}

// An intentional removal can retire exactly its own missing anchor. Unrelated
// missing/corrupt anchors continue to fail closed until a complete census.
func (s *Store) retirePackedOverlayAnchor(marker sessionIndexMarker, key agentmeta.SessionKey) error {
	before, err := readSnapshot(filepath.Join(s.home, "sessions-v1", packedOverlaySentinel))
	if err != nil {
		return err
	}
	anchor, valid := packedOverlayAnchor(string(before.data), marker)
	if !valid {
		return ErrSessionIndexRecoveryRequired
	}
	if anchor == "" || anchor != packedIndexHash(key) {
		return nil
	}
	if exists(qualifiedSessionIndexPath(s.home, key)) {
		return nil
	}
	revision, err := s.sessionMembershipRevision()
	if err != nil {
		return err
	}
	next, err := s.selectPackedOverlayAnchor()
	if err != nil {
		return err
	}
	value := marker.PackedEpoch + ":empty"
	if next != "" {
		value = marker.PackedEpoch + ":overrides:" + next
	}
	return s.writePackedOverlayState(marker, value, before, revision)
}

// Collector/retention only. Results are fenced against hooks and membership
// changes before publication, with native staging and sync outside both locks.
func (s *Store) selectPackedOverlayAnchor() (string, error) {
	if s.onPackedEnumeration != nil {
		s.onPackedEnumeration()
	}
	dir, err := os.Open(filepath.Join(s.home, "sessions-v1"))
	if err != nil {
		return "", err
	}
	defer func() { _ = dir.Close() }()
	for {
		names, readErr := dir.Readdirnames(64)
		for _, name := range names {
			if !strings.HasSuffix(name, ".json") {
				continue
			}
			hash := strings.TrimSuffix(name, ".json")
			decoded, err := hex.DecodeString(hash)
			if err != nil || len(decoded) != sha256.Size || hash != strings.ToLower(hash) {
				return "", ErrSessionIndexRecoveryRequired
			}
			return hash, nil
		}
		if errors.Is(readErr, io.EOF) {
			return "", nil
		}
		if readErr != nil {
			return "", readErr
		}
	}
}

func (s *Store) writePackedOverlayState(marker sessionIndexMarker, value string, before fileSnapshot, revision string) error {
	path := filepath.Join(s.home, "sessions-v1", packedOverlaySentinel)
	staged, err := local.StageBytes(path, []byte(value))
	if err != nil {
		return err
	}
	defer staged.Discard()
	s.writeSynced()
	committed := false
	err = func() error {
		hooks, err := s.namedLockWait("hooks.lock", time.Second)
		if err != nil {
			return err
		}
		defer hooks()
		membership, err := local.NamedLockWait(s.home, sessionMembershipLock, time.Second)
		if err != nil {
			return err
		}
		defer membership()
		current, err := s.sessionMembershipRevision()
		if err != nil || current != revision {
			return ErrSessionIndexRecoveryRequired
		}
		var now sessionIndexMarker
		if readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &now) != nil || now.Version != 2 || now.PackedEpoch != marker.PackedEpoch {
			return ErrSessionIndexRecoveryRequired
		}
		snapshot, err := readSnapshot(path)
		if err != nil {
			return err
		}
		if !snapshot.equal(before) {
			return errIndexMoved
		}
		anchor, valid := packedOverlayAnchor(value, marker)
		if !valid || (anchor != "" && !exists(filepath.Join(s.home, "sessions-v1", anchor+".json"))) {
			return ErrSessionIndexRecoveryRequired
		}
		err = staged.Commit()
		committed = err == nil
		return err
	}()
	if !committed {
		return err
	}
	s.writeSynced()
	return errors.Join(err, staged.SyncDir())
}

// Stage and directory durability happen outside all locks. The membership
// lock serializes shard renames with expiry and fences stale census publication.
func (s *Store) commitPackedIndexGuarded(ctx context.Context, path string, marker sessionIndexMarker, before fileSnapshot, data []byte, census bool, guard func() (func() error, error)) error {
	staged, err := local.StageBytes(path, data)
	if err != nil {
		return err
	}
	defer staged.Discard()
	s.writeSynced()
	committed, err := s.commitStagedPackedIndex(ctx, path, marker, before, staged, census, guard)
	if !committed {
		return err
	}
	s.writeSynced()
	return errors.Join(err, staged.SyncDir())
}

// commitStagedPackedIndex applies the original publication fences to a synced
// temporary file. Its caller must sync the directory after a successful rename.
func (s *Store) commitStagedPackedIndex(ctx context.Context, path string, marker sessionIndexMarker, before fileSnapshot, staged *local.Staged, census bool, guard func() (func() error, error)) (bool, error) {
	committed := false
	err := func() (err error) {
		if guard != nil {
			unlock, err := guard()
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, unlock()) }()
		}
		unlock, err := local.NamedLockWait(s.home, sessionMembershipLock, time.Second)
		if err != nil {
			return err
		}
		defer unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		var current sessionIndexMarker
		if readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &current) != nil || current.Version != 2 || current.PackedEpoch != marker.PackedEpoch {
			return ErrSessionIndexRecoveryRequired
		}
		if census {
			revision, err := s.sessionMembershipRevision()
			if err != nil || revision != marker.PackedRevision {
				return ErrSessionIndexRecoveryRequired
			}
		}
		snapshot, err := readSnapshot(path)
		if err != nil {
			return err
		}
		if !snapshot.equal(before) {
			return errIndexMoved
		}
		err = staged.Commit()
		committed = err == nil
		return err
	}()
	return committed, err
}

// Capture linked archive IDs before candidate deletion; cleanup does not need
// to enumerate registrations or retain the whole packed inventory.
func (s *Store) packedRemovalIDs(id string) (map[string]bool, error) {
	var marker sessionIndexMarker
	if err := readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if marker.Version != 2 {
		return nil, nil
	}
	ids, err := s.subagentCandidatesForSession(id)
	if err != nil {
		return nil, err
	}
	removed := map[string]bool{id: true}
	for _, child := range ids {
		removed[child] = true
	}
	return removed, nil
}

func (s *Store) removePackedIdentities(ids map[string]bool) error {
	return s.removePackedIdentitiesContext(context.Background(), ids)
}

func (s *Store) removePackedIdentitiesContext(ctx context.Context, ids map[string]bool) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.MarkSessionIndexRecoveryNeeded())
		}
	}()
	var inventory map[agentmeta.SessionKey][]string
	var inventoryRevision string
	for i := range packedSessionIndexShards {
		shard := packedShardName(i)
	attemptLoop:
		for attempt := range unlockedWriteAttempts {
			var marker sessionIndexMarker
			if err := readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil {
				return err
			}
			if marker.Version != 2 {
				return nil
			}
			// Capture before reading authority: a registration added during
			// staging must prevent pruning even if the shard itself is unchanged.
			revision, err := s.sessionMembershipRevision()
			if err != nil {
				return err
			}
			path := packedIndexPath(s.home, shard)
			before, err := readSnapshot(path)
			if err != nil {
				return err
			}
			if !before.found {
				break
			}
			index, err := s.readPackedSessionIndex(shard, marker)
			if err != nil {
				return err
			}
			changed := false
			var candidateIDs []string
			for hash, entry := range index.Entries {
				remove := ids[entry.ArchiveSessionID] && !exists(s.registrationPath(entry.ArchiveSessionID)) && !exists(s.subagentCandidatePath(entry.ArchiveSessionID))
				if entry.Conflict {
					// Conflicts intentionally name no single archive owner. Only
					// a complete validated census can prove all owners expired.
					// It runs outside locks and its revision is fenced at commit.
					if inventory == nil || inventoryRevision != revision {
						inventory, err = s.sessionRegistrationInventory(ctx)
						if err != nil {
							return err
						}
						inventoryRevision = revision
					}
					key := agentmeta.SessionKey{Agent: entry.Agent, NativeID: entry.NativeID}
					remove = len(inventory[key]) == 0
				}
				if remove {
					if entry.Conflict {
						if err := s.removePackedConflictOverride(entry, revision); err != nil {
							if errors.Is(err, errIndexMoved) && attempt+1 < unlockedWriteAttempts {
								continue attemptLoop
							}
							return err
						}
					}
					if !entry.Conflict {
						candidateIDs = append(candidateIDs, entry.ArchiveSessionID)
					}
					delete(index.Entries, hash)
					changed = true
				}
			}
			if !changed {
				break
			}
			index.Epoch, index.Revision, index.Inventory = marker.PackedEpoch, marker.PackedRevision, marker.PackedInventory
			data, err := encodePackedIndex(index)
			if err != nil {
				return err
			}
			// The bytes retain the packed provenance; only the commit fence
			// uses the current membership snapshot that authorized deletion.
			fence := marker
			fence.PackedRevision = revision
			err = s.commitPackedIndexGuarded(ctx, path, fence, before, data, true, func() (func() error, error) {
				return s.lockPackedRemovedCandidates(candidateIDs)
			})
			if errors.Is(err, errIndexMoved) {
				if attempt+1 == unlockedWriteAttempts {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// Only the mixed packed/physical conflict case reaches this helper. The full
// census proved no registrations remain; a newer physical owner or reservation
// must still win. Rename/delete checks hold hooks then membership, with the
// native directory sync after both release.
func (s *Store) removePackedConflictOverride(entry qualifiedSessionIndexEntry, revision string) error {
	key := agentmeta.SessionKey{Agent: entry.Agent, NativeID: entry.NativeID}
	path := qualifiedSessionIndexPath(s.home, key)
	before, err := readSnapshot(path)
	if err != nil || !before.found {
		return err
	}
	var physical qualifiedSessionIndexEntry
	if json.Unmarshal(before.data, &physical) != nil || physical.Version != 1 || physical.Agent != key.Agent || physical.NativeID != key.NativeID || !physical.Conflict || physical.Recovery || physical.Absent || physical.ArchiveSessionID != "" || physical.Reservation != "" {
		return nil
	}
	removed, err := func() (bool, error) {
		hooks, err := s.namedLockWait("hooks.lock", time.Second)
		if err != nil {
			return false, err
		}
		defer hooks()
		membership, err := local.NamedLockWait(s.home, sessionMembershipLock, time.Second)
		if err != nil {
			return false, err
		}
		defer membership()
		currentRevision, err := s.sessionMembershipRevision()
		if err != nil || currentRevision != revision {
			return false, ErrSessionIndexRecoveryRequired
		}
		current, err := readSnapshot(path)
		if err != nil {
			return false, err
		}
		if !current.equal(before) {
			return false, errIndexMoved
		}
		err = os.Remove(path)
		return err == nil, err
	}()
	if err != nil || !removed {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

// Candidate recreation does not change registration membership. Serialize the
// final absence checks with candidate writers, before taking membership.lock.
// Native file and directory sync remain outside both sets of locks.
func (s *Store) lockPackedRemovedCandidates(ids []string) (func() error, error) {
	sort.Strings(ids)
	var unlocks []func()
	unlockAll := func() error {
		var err error
		for i := len(unlocks) - 1; i >= 0; i-- {
			id := ids[i]
			if !exists(s.subagentCandidatePath(id)) && !exists(s.registrationPath(id)) {
				removeErr := os.Remove(filepath.Join(s.home, subagentLockName(id)))
				if !errors.Is(removeErr, os.ErrNotExist) {
					err = errors.Join(err, removeErr)
				}
			}
			unlocks[i]()
		}
		return err
	}
	for _, id := range ids {
		unlock, err := s.lockSubagentCandidate(id)
		if err != nil {
			return nil, errors.Join(err, unlockAll())
		}
		unlocks = append(unlocks, unlock)
		if exists(s.registrationPath(id)) || exists(s.subagentCandidatePath(id)) {
			return nil, errors.Join(errIndexMoved, unlockAll())
		}
	}
	return unlockAll, nil
}

type packedExpiryProof struct {
	Epoch    string `json:"epoch"`
	Revision string `json:"revision"`
}

func packedExpiryProofPath(home string, key agentmeta.SessionKey) string {
	return filepath.Join(home, "sessions-v1", packedIndexHash(key)+".removed")
}

func (s *Store) packedMissAllowed(_ agentmeta.SessionKey, marker sessionIndexMarker) error {
	if !s.packedOverlaysHealthy(marker) {
		return ErrSessionIndexRecoveryRequired
	}
	return nil
}

// A content-free hashed proof permits explicit fresh reuse after expiry until
// membership changes or a durable replacement removes it. No identity strings
// remain in this derived proof; it is not an absent-census certificate.
func (s *Store) recordPackedExpiry(key agentmeta.SessionKey, id string) error {
	if key.NativeID == "" || exists(qualifiedSessionIndexPath(s.home, key)) || exists(s.registrationPath(id)) {
		return nil
	}
	var marker sessionIndexMarker
	if err := readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if marker.Version != 2 {
		return nil
	}
	if err := s.retirePackedOverlayAnchor(marker, key); err != nil {
		return errors.Join(err, s.MarkSessionIndexRecoveryNeeded())
	}
	revision, err := s.sessionMembershipRevision()
	if err != nil {
		return err
	}
	return s.writeUnderLock(lockedWrite{
		lock: func() (func(), error) {
			hooks, err := local.NamedLockWait(s.home, "hooks.lock", time.Second)
			if err != nil {
				return nil, err
			}
			membership, err := local.NamedLockWait(s.home, sessionMembershipLock, time.Second)
			if err != nil {
				hooks()
				return nil, err
			}
			return func() { membership(); hooks() }, nil
		},
		path: packedExpiryProofPath(s.home, key),
		check: func() error {
			if exists(s.registrationPath(id)) || exists(qualifiedSessionIndexPath(s.home, key)) {
				return errIndexMoved
			}
			current, err := s.sessionMembershipRevision()
			if err != nil || current != revision {
				return ErrSessionIndexRecoveryRequired
			}
			var currentMarker sessionIndexMarker
			if readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &currentMarker) != nil || currentMarker.Version != 2 || currentMarker.PackedEpoch != marker.PackedEpoch {
				return ErrSessionIndexRecoveryRequired
			}
			return nil
		}, change: func(fileSnapshot) (any, bool, error) {
			return packedExpiryProof{Epoch: marker.PackedEpoch, Revision: revision}, true, nil
		},
	})
}

func removePackedExpiryFile(path string) error {
	proof := strings.TrimSuffix(path, ".json") + ".removed"
	if err := os.Remove(proof); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) clearPackedExpiryProofs() error {
	entries, err := os.ReadDir(filepath.Join(s.home, "sessions-v1"))
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".removed") {
			if err := os.Remove(filepath.Join(s.home, "sessions-v1", entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	dir, err := os.Open(filepath.Join(s.home, "sessions-v1"))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func (s *Store) packedOverridesExpected(marker sessionIndexMarker) bool {
	data, err := os.ReadFile(filepath.Join(s.home, "sessions-v1", packedOverlaySentinel))
	if err != nil {
		return false
	}
	anchor, valid := packedOverlayAnchor(string(data), marker)
	return valid && anchor != ""
}

func (s *Store) stagePackedExpectation(path string) (*local.Staged, string, error) {
	if !s.indexSnapshots || filepath.Dir(path) != filepath.Join(s.home, "sessions-v1") {
		return nil, "", nil
	}
	var marker sessionIndexMarker
	if err := readRecoveryJSON(filepath.Join(s.home, sessionIndexMarkerFile), &marker); errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	} else if err != nil {
		return nil, "", err
	}
	if marker.Version != 2 {
		return nil, "", nil
	}
	if s.packedOverridesExpected(marker) && exists(path) {
		return nil, marker.PackedEpoch, nil
	}
	staged, err := local.StageBytes(filepath.Join(s.home, "sessions-v1", packedOverlaySentinel), []byte(marker.PackedEpoch+":overrides:"+strings.TrimSuffix(filepath.Base(path), ".json")))
	return staged, marker.PackedEpoch, err
}

func (w lockedWrite) packedEpochValid() error {
	if w.packedEpoch == "" {
		return nil
	}
	var marker sessionIndexMarker
	if readRecoveryJSON(filepath.Join(w.packedHome, sessionIndexMarkerFile), &marker) != nil || marker.Version != 2 || marker.PackedEpoch != w.packedEpoch {
		return ErrSessionIndexRecoveryRequired
	}
	return nil
}

func (s *Store) packedOverlaysHealthy(marker sessionIndexMarker) bool {
	data, err := os.ReadFile(filepath.Join(s.home, "sessions-v1", packedOverlaySentinel))
	if err != nil {
		return false
	}
	anchor, valid := packedOverlayAnchor(string(data), marker)
	return valid && (anchor == "" || s.packedAnchorHealthy(anchor))
}

func (s *Store) reconcilePackedExpiryProofs(marker sessionIndexMarker) error {
	revision, err := s.sessionMembershipRevision()
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Join(s.home, "sessions-v1"))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	changed := false
	for {
		names, readErr := dir.Readdirnames(64)
		for _, name := range names {
			if !strings.HasSuffix(name, ".removed") {
				continue
			}
			path := filepath.Join(s.home, "sessions-v1", name)
			var proof packedExpiryProof
			if readRecoveryJSON(path, &proof) == nil && proof.Epoch == marker.PackedEpoch && proof.Revision == revision {
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			changed = true
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if changed {
		return dir.Sync()
	}
	return nil
}

// packedPublication holds one shard's bounded bytes and pre-staging snapshot.
type packedPublication struct {
	path   string
	before fileSnapshot
	data   []byte
}

// publishPackedBatch overlaps only disk durability for at most two shards.
// Authority preparation, commit fences, and test seams stay on the caller.
// Every started operation is joined before returning, including failures.
func (s *Store) publishPackedBatch(ctx context.Context, marker sessionIndexMarker, batch []packedPublication) []error {
	if len(batch) > 2 {
		panic("packed publication batch exceeds two shards")
	}
	type stagedResult struct {
		staged *local.Staged
		err    error
	}
	stages := make([]chan stagedResult, len(batch))
	for i, publication := range batch {
		stages[i] = make(chan stagedResult, 1)
		go func() {
			staged, err := local.StageBytes(publication.path, publication.data)
			stages[i] <- stagedResult{staged, err}
		}()
	}
	results := make([]stagedResult, len(batch))
	for i := range batch {
		results[i] = <-stages[i]
		defer results[i].staged.Discard()
	}
	errs := make([]error, len(batch))
	committedStages := make([]*local.Staged, len(batch))
	for i, publication := range batch {
		result := results[i]
		if result.err != nil {
			errs[i] = result.err
			continue
		}
		s.writeSynced()
		committed, err := s.commitStagedPackedIndex(ctx, publication.path, marker, publication.before, result.staged, true, nil)
		errs[i] = err
		if !committed {
			continue
		}
		s.writeSynced()
		committedStages[i] = result.staged
	}
	syncPackedParents(batch, committedStages, errs, (*local.Staged).SyncDir)
	return errs
}

// syncPackedParents runs native directory durability after every guarded rename.
// One sync covers all committed members of the same parent; its error belongs
// to every such member, so none can advance the durable checkpoint prefix.
// Mixed parents remain independent and every started sync is joined.
func syncPackedParents(batch []packedPublication, committed []*local.Staged, errs []error, syncDir func(*local.Staged) error) {
	type parentSync struct {
		parent  string
		members []int
		result  chan error
	}
	groups := make([]parentSync, 0, len(batch))
	for i, staged := range committed {
		if staged == nil {
			continue
		}
		parent := filepath.Dir(batch[i].path)
		group := -1
		for j := range groups {
			if groups[j].parent == parent {
				group = j
				break
			}
		}
		if group < 0 {
			groups = append(groups, parentSync{parent: parent, result: make(chan error, 1)})
			group = len(groups) - 1
		}
		groups[group].members = append(groups[group].members, i)
	}
	for _, group := range groups {
		go func() { group.result <- syncDir(committed[group.members[0]]) }()
	}
	for _, group := range groups {
		err := <-group.result
		for _, i := range group.members {
			errs[i] = errors.Join(errs[i], err)
		}
	}
}
