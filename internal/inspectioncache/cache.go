// Package inspectioncache stores disposable, content-qualified import facts.
package inspectioncache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
)

const (
	// MaxEntries bounds the number of persisted summaries.
	MaxEntries = 10000
	// MaxBytes bounds total persisted summary bytes.
	MaxBytes      int64 = 16 << 20
	maxEntryBytes       = 2048
	formatVersion       = 1
)

// Key qualifies intrinsic facts by source content and the effective inspection policy.
type Key struct {
	Agent, SourceIdentity, AdapterVersion, FilterVersion string
	InspectionVersion                                    uint8
	Proof                                                agentapi.SourceContentProof
	RawBytes, RecordBytes, FilteredBytes                 int64
	Records                                              int
}

// Observation is a fully verified successful inspection ready for caller persistence.
type Observation struct {
	Key     Key
	Summary agentapi.ImportSummary
}

// Cache performs bounded lookup and serialized, best-effort persistence.
// Construction and Lookup do not create or modify files.
type Cache struct {
	dir        string
	maxEntries int
	maxBytes   int64
}

// New describes a cache at dir without touching the filesystem.
func New(dir string) *Cache { return &Cache{dir: dir, maxEntries: MaxEntries, maxBytes: MaxBytes} }

func (k Key) valid() bool {
	return k.Agent == "codex" && len(k.SourceIdentity) > 0 && len(k.SourceIdentity) <= 16384 && len(k.AdapterVersion) > 0 && len(k.AdapterVersion) <= 128 && len(k.FilterVersion) > 0 && len(k.FilterVersion) <= 128 && k.InspectionVersion == 1 && k.Proof.Version == 1 && k.Proof.Provider == "codex-ordinary" && k.Proof.SHA256 != [32]byte{} && k.Proof.Boundary >= 0 && k.RawBytes > 0 && k.RecordBytes > 0 && k.FilteredBytes > 0 && k.Records >= 0
}
func (k Key) token() string {
	data, _ := json.Marshal(k)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
func (c *Cache) path(key Key) string { return filepath.Join(c.dir, key.token()+".json") }

type record struct {
	Version   int                    `json:"version"`
	Key       string                 `json:"key"`
	Summary   agentapi.ImportSummary `json:"summary"`
	Integrity string                 `json:"integrity"`
}

func summaryDigest(token string, summary agentapi.ImportSummary) string {
	data, _ := json.Marshal(summary)
	digest := sha256.Sum256(append([]byte(token), data...))
	return hex.EncodeToString(digest[:])
}
func validSummary(summary agentapi.ImportSummary, k Key) bool {
	return summary.Conversation && summary.RetainedBytes > 0 && summary.RetainedBytes <= k.FilteredBytes && summary.RetainedRecords > 0 && int64(summary.RetainedRecords) <= summary.RetainedBytes/2 && (k.Records == 0 || summary.RetainedRecords <= k.Records) && (summary.NativeStartAt.IsZero() || summary.NativeStartAt.Year() >= 1 && summary.NativeStartAt.Year() <= 9999)
}

// Lookup reads one small entry. Any unavailable or invalid entry is a miss.
func (c *Cache) Lookup(ctx context.Context, key Key) (agentapi.ImportSummary, bool) {
	var zero agentapi.ImportSummary
	cacheLookups.Add(1)
	if c == nil || ctx.Err() != nil || !key.valid() {
		return zero, false
	}
	info, err := os.Lstat(c.dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return zero, false
	}
	path := c.path(key)
	info, err = os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > maxEntryBytes {
		return zero, false
	}
	file, err := os.Open(path)
	if err != nil {
		return zero, false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return zero, false
	}
	data, err := io.ReadAll(io.LimitReader(file, maxEntryBytes+1))
	if err != nil || len(data) > maxEntryBytes || ctx.Err() != nil {
		return zero, false
	}
	var entry record
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&entry) != nil || decoder.Decode(&struct{}{}) != io.EOF || entry.Version != formatVersion || entry.Key != key.token() || entry.Integrity != summaryDigest(entry.Key, entry.Summary) || !validSummary(entry.Summary, key) {
		return zero, false
	}
	cacheHits.Add(1)
	return entry.Summary, true
}

type diskEntry struct {
	name     string
	bytes    int64
	modified int64
}

func (c *Cache) inventory(ctx context.Context) ([]diskEntry, int64, error) {
	directory, err := os.Open(c.dir)
	if err != nil {
		return nil, 0, err
	}
	defer directory.Close()
	var entries []diskEntry
	var bytes int64
	var seen int
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		page, err := directory.ReadDir(256)
		if err != nil && err != io.EOF {
			return nil, 0, err
		}
		for _, entry := range page {
			seen++
			if seen > 2*MaxEntries+1 {
				return nil, 0, errors.New("inspection cache directory exceeds scan bound")
			}
			if entry.Name() == "writer.lock" {
				info, statErr := entry.Info()
				if statErr != nil {
					return nil, 0, statErr
				}
				if !info.Mode().IsRegular() || info.Size() != 0 {
					return nil, 0, errors.New("invalid inspection cache lock")
				}
				continue
			}
			info, statErr := entry.Info()
			if statErr != nil {
				return nil, 0, statErr
			}
			if !info.Mode().IsRegular() {
				return nil, 0, errors.New("inspection cache contains nonregular entry")
			}
			entries = append(entries, diskEntry{entry.Name(), info.Size(), info.ModTime().UnixNano()})
			bytes += info.Size()
		}
		if err == io.EOF {
			break
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].modified != entries[j].modified {
			return entries[i].modified < entries[j].modified
		}
		return entries[i].name < entries[j].name
	})
	return entries, bytes, nil
}

// StoreBatch persists observations under a nonblocking process-shared lock.
// Busy, unavailable or corrupt caches may fail; callers never alter decisions.
func (c *Cache) StoreBatch(ctx context.Context, observations []Observation) error {
	if c == nil {
		return nil
	}
	if len(observations) > 256 {
		return errors.New("inspection cache batch exceeds bound")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(c.dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(c.dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("inspection cache directory is not private")
	}
	lockInfo, lockErr := os.Lstat(filepath.Join(c.dir, "writer.lock"))
	if lockErr != nil && !os.IsNotExist(lockErr) {
		return lockErr
	}
	if lockErr == nil && (!lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0600 || lockInfo.Size() != 0) {
		return errors.New("inspection cache lock is not private and empty")
	}
	unlock, err := local.NamedLock(c.dir, "writer.lock")
	if err != nil {
		return err
	}
	defer unlock()
	entries, bytes, err := c.inventory(ctx)
	if err != nil {
		return err
	}
	// Discard invalid entries and enforce caps even if an older writer exceeded them.
	for len(entries) > 0 && (len(entries) > c.maxEntries || bytes > c.maxBytes) {
		victim := entries[0]
		if err := os.Remove(filepath.Join(c.dir, victim.name)); err != nil {
			return err
		}
		entries, bytes = entries[1:], bytes-victim.bytes
	}
	for _, observed := range observations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !observed.Key.valid() || !validSummary(observed.Summary, observed.Key) {
			continue
		}
		token := observed.Key.token()
		name := token + ".json"
		data, err := json.Marshal(record{formatVersion, token, observed.Summary, summaryDigest(token, observed.Summary)})
		if err != nil || len(data) > maxEntryBytes || int64(len(data)) > c.maxBytes {
			continue
		}
		// Reserve the incoming temporary bytes as well as its final directory entry.
		for len(entries) > 0 && (len(entries)+1 > c.maxEntries || bytes+int64(len(data)) > c.maxBytes) {
			victim := entries[0]
			if err := os.Remove(filepath.Join(c.dir, victim.name)); err != nil {
				return err
			}
			entries, bytes = entries[1:], bytes-victim.bytes
		}
		if err := writeAtomic(c.dir, name, data); err != nil {
			return err
		}
		for i := range entries {
			if entries[i].name == name {
				bytes -= entries[i].bytes
				entries = append(entries[:i], entries[i+1:]...)
				break
			}
		}
		bytes += int64(len(data))
		entries = append(entries, diskEntry{name, int64(len(data)), 0})
	}
	return nil
}

func writeAtomic(dir, name string, data []byte) (err error) {
	file, err := os.CreateTemp(dir, ".entry-")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer func() { _ = file.Close(); _ = os.Remove(temp) }()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temp, filepath.Join(dir, name))
}
