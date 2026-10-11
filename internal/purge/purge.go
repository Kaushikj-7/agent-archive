// Package purge builds and applies conservative plans for unreferenced archive
// sources. The bucket's current metadata, not local collector state, decides
// whether a source is live.
package purge

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Lifetime is the maximum time a saved purge plan remains valid.
const Lifetime = 5 * time.Minute

const maxSourceRead = 32 << 20

// Mode selects which unreferenced source objects an inventory includes.
type Mode string

const (
	// ModeUnreferenced selects all source objects not referenced by current metadata.
	ModeUnreferenced Mode = "unreferenced"
	// ModeOldFilter selects unreferenced sources with an older filter version.
	ModeOldFilter Mode = "old-filter"
)

var sourceName = regexp.MustCompile(`^source\.[0-9a-f]{64}\.jsonl\.gz$`)

// Candidate describes an unreferenced source proposed for deletion.
type Candidate struct {
	Key           string `json:"key"`
	Size          int64  `json:"size"`
	ETag          string `json:"etag"`
	FilterVersion string `json:"filter_version,omitempty"`
}

// CurrentOldSession describes a live session using an older filter version.
type CurrentOldSession struct {
	MetadataKey   string `json:"metadata_key"`
	SourceKey     string `json:"source_key"`
	FilterVersion string `json:"filter_version"`
}

// Plan records the candidate inventory and destination for a short-lived purge.
type Plan struct {
	Version            int                 `json:"version"`
	DestinationID      string              `json:"destination_id"`
	Bucket             string              `json:"bucket"`
	Prefix             string              `json:"prefix"`
	Mode               Mode                `json:"mode"`
	BeforeFilter       string              `json:"before_filter,omitempty"`
	CreatedAt          time.Time           `json:"created_at"`
	ExpiresAt          time.Time           `json:"expires_at"`
	Candidates         []Candidate         `json:"candidates"`
	CurrentOldSessions []CurrentOldSession `json:"current_old_sessions"`
	Digest             string              `json:"digest"`
}

// Inventory reads every current metadata sidecar before it proposes a source.
// An ambiguous or unreadable sidecar makes the entire plan fail closed.
func Inventory(ctx context.Context, store storage.ObjectStore, destinationID, bucket, prefix string, mode Mode, before string, now time.Time) (Plan, error) {
	if mode != ModeUnreferenced && mode != ModeOldFilter {
		return Plan{}, fmt.Errorf("unsupported purge mode %q", mode)
	}
	if mode == ModeOldFilter && !numericBefore(before) {
		return Plan{}, fmt.Errorf("old-filter mode requires a numeric --before-filter version")
	}
	if mode == ModeUnreferenced {
		before = archive.FilterVersion
	}
	objects, err := store.List(ctx, "sessions")
	if err != nil {
		return Plan{}, fmt.Errorf("list sessions: %w", err)
	}
	current := make(map[string]bool)
	listedSources := make(map[string]bool)
	for _, object := range objects {
		if validSourceKey(object.Key) {
			listedSources[object.Key] = true
		}
	}
	var old []CurrentOldSession
	for _, object := range objects {
		if strings.HasPrefix(object.Key, "sessions/") && strings.HasSuffix(object.Key, "/metadata.json") && !validMetadataKey(object.Key) {
			return Plan{}, fmt.Errorf("unexpected metadata key %q", object.Key)
		}
		if !validMetadataKey(object.Key) {
			continue
		}
		meta, err := reader.ReadMetadata(ctx, store, object.Key)
		if err != nil {
			return Plan{}, fmt.Errorf("inspect %q: %w", object.Key, err)
		}
		refs, err := liveReferences(meta, object.Key, listedSources)
		if err != nil {
			return Plan{}, err
		}
		for i, ref := range refs {
			current[ref.Key] = true
			version := meta.FilterVersion
			if i > 0 {
				version = meta.History.Preserved[i-1].FilterVersion
				if version == "" && mode == ModeOldFilter {
					version, err = sourceFilterVersion(ctx, store, storage.Object{Key: ref.Key, Size: int64(ref.CompressedBytes)})
					if err != nil {
						return Plan{}, fmt.Errorf("inspect live source %q: %w", ref.Key, err)
					}
				}
			}
			if numericOlder(version, before) {
				old = append(old, CurrentOldSession{MetadataKey: object.Key, SourceKey: ref.Key, FilterVersion: version})
			}
		}
	}
	var candidates []Candidate
	for _, object := range objects {
		if !validSourceKey(object.Key) || current[object.Key] {
			continue
		}
		var version string
		if mode == ModeOldFilter {
			version, err = sourceFilterVersion(ctx, store, object)
			if err != nil {
				return Plan{}, fmt.Errorf("inspect %q: %w", object.Key, err)
			}
			if !numericOlder(version, before) {
				continue
			}
		}
		candidates = append(candidates, Candidate{Key: object.Key, Size: object.Size, ETag: object.ETag, FilterVersion: version})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Key < candidates[j].Key })
	sort.Slice(old, func(i, j int) bool {
		if old[i].MetadataKey != old[j].MetadataKey {
			return old[i].MetadataKey < old[j].MetadataKey
		}
		return old[i].SourceKey < old[j].SourceKey
	})
	plan := Plan{Version: 1, DestinationID: destinationID, Bucket: bucket, Prefix: prefix, Mode: mode, BeforeFilter: before, CreatedAt: now.UTC(), ExpiresAt: now.Add(Lifetime).UTC(), Candidates: candidates, CurrentOldSessions: old}
	plan.Digest, err = digest(plan)
	return plan, err
}

// liveReferences applies the same complete-set checks at inventory and deletion.
func liveReferences(meta archive.Metadata, metadataKey string, listed map[string]bool) ([]archive.SourceReference, error) {
	refs, err := meta.SourceReferences()
	if err != nil {
		return nil, fmt.Errorf("metadata %q has invalid source references: %w", metadataKey, err)
	}
	for _, ref := range refs {
		if path.Dir(ref.Key) != path.Dir(metadataKey) || !validSourceKey(ref.Key) {
			return nil, fmt.Errorf("metadata %q has an invalid source reference", metadataKey)
		}
		if !listed[ref.Key] {
			return nil, fmt.Errorf("metadata %q references a missing source", metadataKey)
		}
	}
	return refs, nil
}

func validMetadataKey(key string) bool {
	parts := strings.Split(key, "/")
	return len(parts) == 4 && parts[0] == "sessions" && validHarness(parts[1]) && validSession(parts[2]) && parts[3] == "metadata.json"
}

func validSourceKey(key string) bool {
	parts := strings.Split(key, "/")
	return len(parts) == 4 && parts[0] == "sessions" && validHarness(parts[1]) && validSession(parts[2]) && sourceName.MatchString(parts[3])
}

func validHarness(value string) bool {
	canonical, known := archive.KnownHarness(value)
	return known && canonical == value
}

func validSession(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

func numericBefore(version string) bool { n, err := strconv.Atoi(version); return err == nil && n > 0 }

func numericOlder(version, before string) bool {
	if !numericBefore(before) {
		return false
	}
	a, errA := strconv.Atoi(version)
	b, _ := strconv.Atoi(before)
	return errA == nil && a > 0 && a < b
}

func sourceFilterVersion(ctx context.Context, store storage.ObjectStore, object storage.Object) (string, error) {
	if object.Size > maxSourceRead {
		return "", fmt.Errorf("source exceeds %d-byte inspection limit", maxSourceRead)
	}
	data, err := store.Get(ctx, object.Key)
	if err != nil {
		return "", err
	}
	if len(data) > maxSourceRead {
		return "", fmt.Errorf("source exceeds %d-byte inspection limit", maxSourceRead)
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	line, readErr := io.ReadAll(io.LimitReader(reader, 1<<20))
	if closeErr := reader.Close(); closeErr != nil {
		return "", closeErr
	}
	if readErr != nil {
		return "", readErr
	}
	first, _, ok := bytes.Cut(line, []byte{'\n'})
	if !ok {
		return "", errors.New("source header exceeds inspection limit")
	}
	var header archive.SourceHeader
	if err := json.Unmarshal(first, &header); err != nil {
		return "", err
	}
	if header.Kind != archive.SourceLineHeader || header.Capture.FilterVersion == "" {
		return "", errors.New("source has no supported filter header")
	}
	return header.Capture.FilterVersion, nil
}

func digest(plan Plan) (string, error) {
	plan.Digest = ""
	data, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Validate checks the plan destination, expiry, and content digest.
func (p Plan) Validate(destinationID string, now time.Time) error {
	if p.Version != 1 || p.DestinationID != destinationID {
		return errors.New("purge plan destination changed or version unsupported")
	}
	if now.Before(p.CreatedAt) || !now.Before(p.ExpiresAt) {
		return errors.New("purge plan expired; create a new plan")
	}
	got, err := digest(p)
	if err != nil {
		return err
	}
	if got != p.Digest {
		return errors.New("purge plan digest mismatch")
	}
	return nil
}

// Report records completed and remaining deletions for a resumable purge.
type Report struct {
	PlanDigest string   `json:"plan_digest"`
	Deleted    []string `json:"deleted"`
	Remaining  []string `json:"remaining"`
	Error      string   `json:"error,omitempty"`
}

// Apply deletes one candidate at a time after a fresh full metadata scan.
// The caller durably saves the report after each successful delete. All machines
// writing to this prefix must be paused; S3 offers no atomic conditional
// delete against another writer's metadata update.
func Apply(ctx context.Context, store storage.ObjectStore, plan Plan, report *Report, save func(Report) error) error {
	if err := validateReport(plan, report); err != nil {
		return err
	}
	for len(report.Remaining) > 0 {
		if err := applyNext(ctx, store, plan, report, save); err != nil {
			return err
		}
	}
	return nil
}

func validateReport(plan Plan, report *Report) error {
	if report.PlanDigest != plan.Digest {
		return errors.New("report does not match plan")
	}
	remaining := make(map[string]bool, len(plan.Candidates))
	for _, c := range plan.Candidates {
		remaining[c.Key] = true
	}
	for _, key := range report.Deleted {
		if !remaining[key] {
			return errors.New("report contains an unknown or repeated deleted key")
		}
		delete(remaining, key)
	}
	for _, key := range report.Remaining {
		if !remaining[key] {
			return errors.New("report contains an unknown or repeated remaining key")
		}
		delete(remaining, key)
	}
	if len(remaining) != 0 {
		return errors.New("report omits planned keys")
	}
	return nil
}

func applyNext(ctx context.Context, store storage.ObjectStore, plan Plan, report *Report, save func(Report) error) error {
	key := report.Remaining[0]
	candidate, ok := findCandidate(plan.Candidates, key)
	if !ok || !validSourceKey(key) {
		return fmt.Errorf("invalid planned key %q", key)
	}
	objects, err := store.List(ctx, "sessions")
	if err != nil {
		return fmt.Errorf("relist before %q: %w", key, err)
	}
	var listed *storage.Object
	listedSources := make(map[string]bool)
	for _, object := range objects {
		if validSourceKey(object.Key) {
			listedSources[object.Key] = true
		}
	}
	for i := range objects {
		if objects[i].Key == key {
			listed = &objects[i]
		}
		if strings.HasPrefix(objects[i].Key, "sessions/") && strings.HasSuffix(objects[i].Key, "/metadata.json") && !validMetadataKey(objects[i].Key) {
			return fmt.Errorf("unexpected metadata key %q", objects[i].Key)
		}
		if !validMetadataKey(objects[i].Key) {
			continue
		}
		meta, err := reader.ReadMetadata(ctx, store, objects[i].Key)
		if err != nil {
			return fmt.Errorf("recheck %q: %w", objects[i].Key, err)
		}
		refs, err := liveReferences(meta, objects[i].Key, listedSources)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			if ref.Key == key {
				return fmt.Errorf("%q is now a current source", key)
			}
		}
	}
	if listed != nil && (listed.Size != candidate.Size || listed.ETag != candidate.ETag) {
		return fmt.Errorf("%q changed since plan", key)
	}
	if listed != nil {
		if err := store.Delete(ctx, key); err != nil {
			return fmt.Errorf("delete %q: %w", key, err)
		}
	}
	report.Deleted = append(report.Deleted, key)
	report.Remaining = report.Remaining[1:]
	report.Error = ""
	if err := save(*report); err != nil {
		return fmt.Errorf("save deletion report: %w", err)
	}
	return nil
}

func findCandidate(candidates []Candidate, key string) (Candidate, bool) {
	for _, item := range candidates {
		if item.Key == key {
			return item, true
		}
	}
	return Candidate{}, false
}
