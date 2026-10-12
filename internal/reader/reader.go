// Package reader lists private metadata and reads selected source bundles into
// memory. It creates no normalized persistence. The only thing it may keep on
// disk is the disposable metadata cache (see MetadataCache); source bundles
// are never written locally.
package reader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/trace"
)

// ErrRefreshRequired means the source bundle a metadata sidecar points to is
// no longer in the bucket, usually because the session was republished and
// the old source cleaned up. Reread the metadata and try again, as
// RefreshAndLoad does once.
var ErrRefreshRequired = errors.New("source changed or was deleted; refresh metadata and retry")

// ErrInvalidMetadata means a metadata sidecar was read but does not decode
// or validate: it is damaged, or a newer version of agent-archive wrote it.
var ErrInvalidMetadata = errors.New("invalid metadata")

// SkippedSidecar is a listed sidecar a listing left out because it is
// invalid (Err wraps ErrInvalidMetadata).
type SkippedSidecar struct {
	Key string
	Err error
}

// SkillUsage narrows a Filter's Skill/SkillSHA256 match to a specific
// relationship between a session and the named skill. The zero value means
// "the skill was used".
type SkillUsage string

const (
	// SkillUsageUsed is the default (zero-value) behavior: match sessions
	// that actually used the skill.
	SkillUsageUsed SkillUsage = "used"
	// SkillUsageAvailable matches sessions where the skill was available
	// (eligible or discovered coverage), regardless of whether it was used.
	SkillUsageAvailable SkillUsage = "available"
	// SkillUsageEligibleNoUse matches sessions where the skill was eligible
	// but never used. It requires the metadata's SkillDetection to be
	// archive.SkillDetectionObservedNone; anything else (including
	// archive.SkillDetectionUnavailable) is treated as unknown, not "no use".
	//
	// No parser version emits archive.SkillDetectionObservedNone today, so
	// this value cannot match any sidecar yet. The CLI rejects it until a
	// parser can prove non-use; the comparison remains for that future parser.
	SkillUsageEligibleNoUse SkillUsage = "eligible_no_use"
)

// Filter selects metadata sidecars. Every set field must match; a zero field
// matches anything. Harness is compared with the harness name, Model with the
// request or response model, and From and To bound CapturedAt, inclusive.
// Skill and SkillSHA256 match a skill by name and content hash, in the
// relationship SkillUsage names. RequireCompleteCoverage keeps only sessions
// whose parser status is complete and which have no capture gaps. Replays
// says what to do with sessions a replay tool ran; its zero value includes
// them, so only a caller that asks hides them.
type Filter struct {
	Harness                 string       `json:"Harness"`
	Model                   string       `json:"Model"`
	Skill                   string       `json:"Skill"`
	SkillSHA256             string       `json:"SkillSHA256"`
	From                    time.Time    `json:"From"`
	To                      time.Time    `json:"To"`
	RequireCompleteCoverage bool         `json:"RequireCompleteCoverage"`
	SkillUsage              SkillUsage   `json:"SkillUsage"`
	Replays                 ReplayFilter `json:"Replays"`
}

// ReplayFilter selects by whether a session is a replay (archive.Replay).
type ReplayFilter string

const (
	// ReplaysIncluded is the zero value: it matches replays and ordinary
	// sessions alike.
	ReplaysIncluded ReplayFilter = ""
	// ReplaysHidden matches only ordinary sessions: what list and stats
	// show by default.
	ReplaysHidden ReplayFilter = "hide"
	// ReplaysOnly matches only replays.
	ReplaysOnly ReplayFilter = "only"
)

// Limits bounds how much of a source bundle LoadSource reads: the compressed
// object (default 32 MiB) and the decompressed stream (default 128 MiB). A
// zero or negative value means the default.
type Limits struct {
	MaxCompressedBytes   int
	MaxUncompressedBytes int
}

func (l Limits) compressed() int {
	if l.MaxCompressedBytes > 0 {
		return l.MaxCompressedBytes
	}
	return 32 << 20
}

func (l Limits) uncompressed() int {
	if l.MaxUncompressedBytes > 0 {
		return l.MaxUncompressedBytes
	}
	return 128 << 20
}

var defaultFinder = NewMetadataFinder(agentmeta.Builtins())

// MetadataFinder caches known-agent probes once, preserving unknown-agent fallback.
type MetadataFinder struct{ harnesses []string }

// NewMetadataFinder builds known-agent probes from an injected catalog.
func NewMetadataFinder(c agentmeta.Catalog) *MetadataFinder {
	return &MetadataFinder{harnesses: agentmeta.Names(c)}
}

// listConcurrency bounds how many sidecars a listing downloads at once. The
// sidecars are small and independent, so a sequential read spends almost all
// of its time waiting on round trips; eight keeps a slow link busy without
// flooding a provider.
const listConcurrency = 8

// ListOptions tunes ListMetadataWithOptions. The zero value reads every
// matching sidecar from the store.
type ListOptions struct {
	includeRootChildren bool

	// ScopeMatch tests only identity summaries before body selection.
	ScopeMatch func(archive.Metadata) bool

	// ActivityOrder selects the text listing's activity ordering.
	ActivityOrder bool
	// TopLevelOnly excludes subagents before the limit and returns their counts.
	TopLevelOnly bool
	// CompatibilityScan explains why the exhaustive reader was required.
	CompatibilityScan func(string)
	// BodyRead observes successful selected metadata or cache body reads.
	// Indexed reads invoke it serially in selection order after workers join.
	BodyRead func(key string, cached bool)

	// Cache, when set, serves a sidecar whose listed ETag is unchanged from
	// local disk instead of downloading it, and forgets sidecars which are
	// no longer listed. It holds metadata only.
	Cache *MetadataCache
	// Skipped, when set, is called once for each invalid sidecar the listing
	// left out, in key order, after every sidecar has been read, so a caller
	// can warn about it. The listing itself still succeeds.
	Skipped func(SkippedSidecar)
	// Progress, when set, is called as each listed sidecar has been read
	// (served from the cache or downloaded), with how many are done of how
	// many the listing has. It is called from several goroutines at once, so
	// it must be safe for that, and it must not block.
	// Indexed selection invokes it serially on the caller as reads finish.
	Progress func(done, total int)
}

// ListMetadataWithOptions reads only metadata sidecars and applies filters
// without downloading transcript bundles, with an optional local cache. A
// harness filter narrows the listing to that harness's own prefix, a listing
// large enough to split is listed as key ranges in parallel (listObjects),
// keys which are not sidecars are skipped before any download, and sidecars
// are read with bounded concurrency. Results are ordered newest capture first.
//
// Another machine's retention or undo can delete a session at any time, so a
// sidecar that is listed and then not found is left out: it no longer
// exists. One that does not decode or validate (damaged, or written by a
// newer version) is left out and reported through options.Skipped, so one
// bad sidecar does not hide the rest of the archive. Any other sidecar read
// error (network, credentials) fails the listing: the first in key order, as
// a sequential read would. A listing split into ranges fails with the error
// of the first range to fail, which need not be the lowest in key order.
func ListMetadataWithOptions(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter, options ListOptions) ([]archive.Metadata, error) {
	options.Cache.maintain(ctx)
	if CatalogAuthority(store) {
		return catalogMetadata(ctx, store, prefix, filter, options)
	}
	span := trace.Start("list metadata")
	defer span.End()
	listPrefix := listPrefixFor(prefix, filter.Harness)
	scan := span.Child("scan cache")
	known := options.Cache.keys(listPrefix)
	scan.Count("cached keys", len(known))
	scan.End()
	objects, err := listObjects(ctx, store, listPrefix, known)
	if err != nil {
		return nil, err
	}
	return listMetadataFromHeaders(ctx, store, filter, options, objects, known)
}

// listMetadataFromHeaders consumes one complete canonical listing, including
// when an indexed query must fall back after discovery.
func listMetadataFromHeaders(ctx context.Context, store storage.ObjectStore, filter Filter, options ListOptions, objects []storage.Object, known []string) ([]archive.Metadata, error) {
	span := trace.Start("read metadata headers")
	defer span.End()
	sidecars := make([]storage.Object, 0, len(objects))
	for _, object := range objects {
		if strings.HasSuffix(object.Key, "/metadata.json") {
			sidecars = append(sidecars, object)
		}
	}
	span.Count("keys", len(objects))
	span.Count("sidecars", len(sidecars))
	// Complete fresh headers establish deletion independently of whether the
	// remaining bodies can be read. A later read error must not retain an
	// absent session's cached metadata.
	options.Cache.evictUnlisted(known, sidecars)
	loaded, skipped, err := readSidecars(ctx, store, sidecars, options.Cache, options.Progress)
	if err != nil {
		return nil, err
	}
	if options.Skipped != nil {
		for _, s := range skipped {
			options.Skipped(s)
		}
	}
	var results []archive.Metadata
	for _, metadata := range loaded {
		if matches(metadata, filter) {
			results = append(results, metadata)
		}
	}
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if !a.CapturedAt.Equal(b.CapturedAt) {
			return a.CapturedAt.After(b.CapturedAt)
		}
		if a.Harness.Name != b.Harness.Name {
			return a.Harness.Name < b.Harness.Name
		}
		return a.SessionID < b.SessionID
	})
	return results, nil
}

// listPrefixFor narrows a listing to one harness's sessions. A harness value
// that cannot be a key segment lists the whole prefix instead, which is what
// every harness filter did before, so it still matches nothing rather than
// becoming an error.
func listPrefixFor(prefix, harness string) string {
	if harness == "" {
		return prefix
	}
	if _, err := archive.MetadataObjectKey(harness, "probe"); err != nil {
		return prefix
	}
	if trimmed := strings.TrimSuffix(prefix, "/"); trimmed != "" {
		return trimmed + "/" + harness + "/"
	}
	return harness + "/"
}

// readSidecars downloads and validates each listed sidecar with at most
// listConcurrency requests in flight. Work is dispatched in key order and no
// new work starts after a failure or once ctx is done, so every sidecar before
// the first failing one has been read and the lowest-index error is the one a
// sequential read would have returned. It returns only after every read it
// started has finished, so no goroutine outlives the call, and a cancelled
// ctx is reported even when the store itself ignores it. A sidecar deleted
// since the listing is left out, and one that is invalid is left out and
// returned in skipped, in key order; neither stops the others.
func readSidecars(ctx context.Context, store storage.ObjectStore, objects []storage.Object, cache *MetadataCache, progress func(done, total int)) (loaded []archive.Metadata, skipped []SkippedSidecar, err error) {
	span := trace.Start("read sidecars")
	var cached, downloaded atomic.Int64
	defer func() {
		span.Count("from cache", int(cached.Load()))
		span.Count("downloaded", int(downloaded.Load()))
		span.End()
	}()
	out := make([]archive.Metadata, len(objects))
	present := make([]bool, len(objects))
	errs := make([]error, len(objects))
	var failed atomic.Bool
	var finished atomic.Int64
	slots := make(chan struct{}, listConcurrency)
	var wg sync.WaitGroup
	dispatched := 0
	for index, object := range objects {
		if failed.Load() || ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		// A failure may have landed while waiting for the slot.
		if failed.Load() || ctx.Err() != nil {
			<-slots
			break
		}
		dispatched++
		wg.Add(1)
		go func(index int, object storage.Object) {
			defer wg.Done()
			defer func() { <-slots }()
			defer func() {
				if progress != nil {
					progress(int(finished.Add(1)), len(objects))
				}
			}()
			metadata, fromCache, err := readListedSidecar(ctx, store, object, cache)
			switch {
			case err == nil && fromCache:
				cached.Add(1)
			case err == nil:
				downloaded.Add(1)
			}
			switch {
			case errors.Is(err, storage.ErrNotFound):
				// Deleted since the listing.
			case errors.Is(err, ErrInvalidMetadata):
				errs[index] = err
			case err != nil:
				errs[index] = err
				failed.Store(true)
			default:
				out[index], present[index] = metadata, true
			}
		}(index, object)
	}
	wg.Wait()
	for index, err := range errs {
		switch {
		case errors.Is(err, ErrInvalidMetadata):
			skipped = append(skipped, SkippedSidecar{Key: objects[index].Key, Err: err})
		case err != nil:
			return nil, nil, err
		}
	}
	if dispatched < len(objects) {
		// Dispatch stopped early without a read failing: ctx is done.
		return nil, nil, fmt.Errorf("read metadata: %w", ctx.Err())
	}
	for index := range out {
		if present[index] {
			loaded = append(loaded, out[index])
		}
	}
	return loaded, skipped, nil
}

// readListedSidecar serves an unchanged sidecar from the cache and otherwise
// downloads it, saying which it did. Only a sidecar which decoded and
// validated is cached, and a cache entry which no longer decodes is treated
// as a miss.
func readListedSidecar(ctx context.Context, store storage.ObjectStore, object storage.Object, cache *MetadataCache) (metadata archive.Metadata, fromCache bool, err error) {
	if data, ok := cache.get(object.Key, object.ETag); ok {
		if metadata, err := decodeMetadata(object.Key, data); err == nil {
			return metadata, true, nil
		}
	}
	data, err := store.Get(ctx, object.Key)
	if err != nil {
		return archive.Metadata{}, false, fmt.Errorf("read metadata %q: %w", object.Key, err)
	}
	metadata, err = decodeMetadata(object.Key, data)
	if err != nil {
		return archive.Metadata{}, false, err
	}
	cache.put(object.Key, object.ETag, data)
	return metadata, false, nil
}

// ReadMetadata reads and validates one metadata sidecar by its object key.
// Like ListMetadataWithOptions it downloads no transcript bundle.
func ReadMetadata(ctx context.Context, store storage.ObjectStore, key string) (archive.Metadata, error) {
	data, err := store.Get(ctx, key)
	if err != nil {
		return archive.Metadata{}, fmt.Errorf("read metadata %q: %w", key, err)
	}
	return decodeMetadata(key, data)
}

func decodeMetadata(key string, data []byte) (archive.Metadata, error) {
	var metadata archive.Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return archive.Metadata{}, fmt.Errorf("%w %q: decode: %w", ErrInvalidMetadata, key, err)
	}
	if err := metadata.ValidateSourceReference(); err != nil {
		return archive.Metadata{}, fmt.Errorf("%w %q: %w", ErrInvalidMetadata, key, err)
	}
	return metadata, nil
}

// MetadataLookup retains the decoded metadata alongside its object key.
type MetadataLookup struct {
	Key      string
	Metadata archive.Metadata
}

type metadataProbe struct {
	key     string
	data    []byte
	fetched bool
	err     error
}

// FindMetadata returns matching sidecars in key order, retaining probe reads.
// Several results mean an ambiguous ID. Known harnesses are probed before the
// listing fallback for unknown archived harnesses. Decode failures return the
// discovered keys alongside the error, so callers can still report ambiguity.
func (f *MetadataFinder) FindMetadata(ctx context.Context, store storage.ObjectStore, prefix, archiveSessionID string) ([]MetadataLookup, error) {
	probes, err := f.findMetadata(ctx, store, prefix, archiveSessionID)
	if err != nil {
		return nil, err
	}
	result := make([]MetadataLookup, len(probes))
	var firstErr error
	for i, probe := range probes {
		result[i].Key = probe.key
		data := probe.data
		if !probe.fetched {
			data, err = store.Get(ctx, probe.key)
			if err != nil {
				err = fmt.Errorf("read metadata %q: %w", probe.key, err)
			}
		}
		if err == nil {
			result[i].Metadata, err = decodeMetadata(probe.key, data)
		}
		if firstErr == nil {
			firstErr = err
		}
		err = nil
	}
	return result, firstErr
}

// FindMetadataKeys returns matching sidecar keys in key order. It preserves
// existence-only lookup for callers that do not need decoded metadata.
func FindMetadataKeys(ctx context.Context, store storage.ObjectStore, prefix, archiveSessionID string) ([]string, error) {
	return defaultFinder.FindMetadataKeys(ctx, store, prefix, archiveSessionID)
}

// FindMetadataKeys is the compatibility key-only view of metadata discovery.
func (f *MetadataFinder) FindMetadataKeys(ctx context.Context, store storage.ObjectStore, prefix, archiveSessionID string) ([]string, error) {
	probes, err := f.findMetadata(ctx, store, prefix, archiveSessionID)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, probe := range probes {
		keys = append(keys, probe.key)
	}
	return keys, nil
}

func (f *MetadataFinder) findMetadata(ctx context.Context, store storage.ObjectStore, prefix, archiveSessionID string) ([]metadataProbe, error) {
	if archiveSessionID == "" || strings.Contains(archiveSessionID, "/") {
		return nil, fmt.Errorf("invalid archive session ID %q", archiveSessionID)
	}
	if _, err := archive.MetadataObjectKey("probe", archiveSessionID); err != nil {
		return nil, fmt.Errorf("invalid archive session ID %q", archiveSessionID)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	probes := make([]metadataProbe, len(f.harnesses))
	var next atomic.Int64
	var workers sync.WaitGroup
	for range min(8, len(probes)) {
		workers.Go(func() {
			for {
				i := int(next.Add(1) - 1)
				if i >= len(probes) {
					return
				}
				key := strings.TrimPrefix(listPrefixFor(prefix, f.harnesses[i])+archiveSessionID+"/metadata.json", "/")
				probes[i].key = key
				if err := ctx.Err(); err != nil {
					probes[i].err = err
					continue
				}
				probes[i].data, probes[i].err = store.Get(ctx, key)
				probes[i].fetched = true
			}
		})
	}
	workers.Wait()
	var found []metadataProbe
	for _, probe := range probes {
		if probe.err == nil {
			found = append(found, probe)
		} else if !errors.Is(probe.err, storage.ErrNotFound) {
			return nil, fmt.Errorf("read metadata %q: %w", probe.key, probe.err)
		}
	}
	if len(found) == 0 && CatalogAuthority(store) {
		summaries, err := CatalogSummaries(ctx, store, Filter{})
		if err != nil {
			return nil, err
		}
		for _, summary := range summaries {
			if summary.SessionID == archiveSessionID {
				key, err := archive.MetadataObjectKey(summary.Harness.Name, summary.SessionID)
				if err != nil {
					return nil, err
				}
				found = append(found, metadataProbe{key: key})
			}
		}
		sort.Slice(found, func(i, j int) bool { return found[i].key < found[j].key })
		return found, nil
	}
	if len(found) == 0 {
		objects, err := store.List(ctx, prefix)
		if err != nil {
			return nil, err
		}
		suffix := "/" + archiveSessionID + "/metadata.json"
		for _, object := range objects {
			if strings.HasSuffix(object.Key, suffix) {
				found = append(found, metadataProbe{key: object.Key})
			}
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].key < found[j].key })
	return found, nil
}

// eligibilityParserVersion is the lowest parser version whose observed_none
// could rest on complete eligibility and use-observation coverage rather than
// on inference from availability alone.
var eligibilityParserVersion = [3]int{0, 4, 0}

// parserVersionAtLeast reports whether version parses as a plain numeric
// major.minor.patch triple at or above minimum. Anything unparsable — the empty
// string, a custom collector.Options.ParserVersion override, a pre-release
// suffix — is not at least minimum, because only a version this function can
// order says which parser wrote the sidecar.
func parserVersionAtLeast(version string, minimum [3]int) bool {
	fields := strings.Split(version, ".")
	if len(fields) != len(minimum) {
		return false
	}
	var parsed [3]int
	for i, field := range fields {
		if field == "" || len(field) > 1 && field[0] == '0' {
			return false
		}
		for _, r := range field {
			if r < '0' || r > '9' {
				return false
			}
		}
		n, err := strconv.Atoi(field)
		if err != nil {
			return false
		}
		parsed[i] = n
	}
	for i := range parsed {
		if parsed[i] != minimum[i] {
			return parsed[i] > minimum[i]
		}
	}
	return true
}

func matches(m archive.Metadata, f Filter) bool {
	return matchesCapture(m, f) && matchesCoverage(m, f) && matchesModel(m, f.Model) && matchesSkill(m, f)
}

func matchesCapture(m archive.Metadata, f Filter) bool {
	if f.Harness != "" && m.Harness.Name != f.Harness {
		return false
	}
	if !f.From.IsZero() && m.CapturedAt.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && m.CapturedAt.After(f.To) {
		return false
	}
	switch f.Replays {
	case ReplaysHidden:
		return !m.IsReplay()
	case ReplaysOnly:
		return m.IsReplay()
	case ReplaysIncluded:
	}
	return true
}

func matchesCoverage(m archive.Metadata, f Filter) bool {
	if f.RequireCompleteCoverage && (m.Parser.Status != archive.ParserStatusComplete || len(m.CaptureGaps) != 0) {
		return false
	}
	return true
}

func matchesModel(m archive.Metadata, model string) bool {
	if model == "" {
		return true
	}
	for _, x := range m.Models {
		if x.Attributes["gen_ai.request.model"] == model || x.Attributes["gen_ai.response.model"] == model {
			return true
		}
	}
	return false
}

func matchesSkill(m archive.Metadata, f Filter) bool {
	if f.Skill == "" && f.SkillSHA256 == "" {
		return true
	}
	used := matchesUsedSkill(m, f)
	available := matchesAvailableSkill(m, f)
	switch f.SkillUsage {
	case SkillUsageAvailable:
		return available
	case SkillUsageEligibleNoUse:
		// Older parsers inferred non-use from availability alone. This is a
		// whitelist, not a blacklist of known-legacy strings: a pre-0.4.0
		// build could have written observed_none with a custom version.
		return available && !used && parserVersionAtLeast(m.Parser.Version, eligibilityParserVersion) && m.SkillDetection == archive.SkillDetectionObservedNone
	case SkillUsageUsed:
		fallthrough
	default: // the zero value
		return used
	}
}

func matchesUsedSkill(m archive.Metadata, f Filter) bool {
	for _, x := range m.SkillsUsed {
		if matchesSkillIdentity(x.Name, x.SHA256, f) {
			return true
		}
	}
	return false
}

func matchesAvailableSkill(m archive.Metadata, f Filter) bool {
	for _, x := range m.SkillsAvailable {
		if matchesSkillIdentity(x.Name, x.SHA256, f) && (x.Coverage == archive.SkillCoverageEligible || x.Coverage == archive.SkillCoverageDiscovered) {
			return true
		}
	}
	return false
}

func matchesSkillIdentity(name, hash string, f Filter) bool {
	return (f.Skill == "" || name == f.Skill) && (f.SkillSHA256 == "" || hash == f.SkillSHA256)
}

// LoadRevision validates the complete parent set, then reads one self-contained
// preserved revision with its original capture time and native identity.
func LoadRevision(ctx context.Context, store storage.ObjectStore, metadata archive.Metadata, revisionID string, limits Limits) (archive.SourceBundle, error) {
	if _, err := metadata.SourceReferences(); err != nil {
		return archive.SourceBundle{}, err
	}
	if metadata.History == nil {
		return archive.SourceBundle{}, errors.New("session has no native revision history")
	}
	selected, _, _, err := revisionSourceMetadata(metadata, revisionID)
	if err != nil {
		return archive.SourceBundle{}, err
	}
	if selected.SourceBundle.CompressedBytes <= 0 || selected.SourceBundle.CompressedBytes > limits.compressed() {
		return archive.SourceBundle{}, storage.ErrObjectTooLarge
	}
	bounded, ok := store.(storage.LimitedGetter)
	if !ok {
		return archive.SourceBundle{}, errors.New("revision reads require allocation-bounded object reads")
	}
	data, err := bounded.GetLimited(ctx, selected.SourceBundle.Key, int64(selected.SourceBundle.CompressedBytes))
	if errors.Is(err, storage.ErrNotFound) {
		return archive.SourceBundle{}, ErrRefreshRequired
	}
	if err != nil {
		return archive.SourceBundle{}, err
	}
	return DecodeRevisionSource(ctx, metadata, revisionID, data, limits)
}

// LoadSource verifies the compressed SHA-256 before bounded, streaming
// decompression and validates that source identity matches the selected
// metadata pointer. A schema-1 bundle (a single JSON document) is refused with
// an error naming its schema version.
func LoadSource(ctx context.Context, store storage.ObjectStore, metadata archive.Metadata, limits Limits) (archive.SourceBundle, error) {
	defer trace.Start("load source").End()
	if err := metadata.ValidateSourceReference(); err != nil {
		return archive.SourceBundle{}, err
	}
	data, err := store.Get(ctx, metadata.SourceBundle.Key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return archive.SourceBundle{}, ErrRefreshRequired
		}
		return archive.SourceBundle{}, err
	}
	return DecodeReferencedSource(ctx, metadata, data, limits)
}

// DecodeReferencedSource verifies bounded immutable compressed bytes against a
// selected metadata pointer before decoding. Private publication stages reuse
// this reader without pretending to be a second object-store implementation.
func DecodeReferencedSource(ctx context.Context, metadata archive.Metadata, data []byte, limits Limits) (archive.SourceBundle, error) {
	return decodeReferencedSource(ctx, metadata, data, limits, &metadata.FilterVersion, 0, false)
}

// DecodeRevisionSource verifies a retained revision using its own provenance.
// Absent legacy fields are derived from bounded decoded bytes, never the active filter.
func DecodeRevisionSource(ctx context.Context, metadata archive.Metadata, revisionID string, data []byte, limits Limits) (archive.SourceBundle, error) {
	selected, filter, schema, err := revisionSourceMetadata(metadata, revisionID)
	if err != nil {
		return archive.SourceBundle{}, err
	}
	return decodeReferencedSource(ctx, selected, data, limits, filter, schema, revisionID != metadata.History.CurrentRevision)
}

func revisionSourceMetadata(metadata archive.Metadata, revisionID string) (archive.Metadata, *string, int, error) {
	if _, err := metadata.SourceReferences(); err != nil {
		return archive.Metadata{}, nil, 0, err
	}
	if metadata.History == nil {
		return archive.Metadata{}, nil, 0, errors.New("session has no native revision history")
	}
	if metadata.History.CurrentRevision == revisionID {
		return metadata, &metadata.FilterVersion, 0, nil
	}
	for _, revision := range metadata.History.Preserved {
		if revision.RevisionID != revisionID {
			continue
		}
		selected := metadata
		selected.SourceBundle = revision.Source
		selected.CapturedAt = revision.CapturedAt
		selected.History = &archive.RevisionHistory{CurrentRevision: revision.RevisionID}
		if revision.FilterVersion == "" {
			return selected, nil, revision.SourceSchemaVersion, nil
		}
		return selected, &revision.FilterVersion, revision.SourceSchemaVersion, nil
	}
	return archive.Metadata{}, nil, 0, errors.New("revision is not referenced by this session")
}

func decodeReferencedSource(ctx context.Context, metadata archive.Metadata, data []byte, limits Limits, filter *string, schema int, preserved bool) (archive.SourceBundle, error) {
	if err := ctx.Err(); err != nil {
		return archive.SourceBundle{}, err
	}
	if err := metadata.ValidateSourceReference(); err != nil {
		return archive.SourceBundle{}, err
	}
	if len(data) > limits.compressed() {
		return archive.SourceBundle{}, errors.New("source exceeds compressed read limit")
	}
	if len(data) != metadata.SourceBundle.CompressedBytes {
		return archive.SourceBundle{}, errors.New("source compressed size does not match metadata")
	}
	if !storage.VerifySHA256(data, metadata.SourceBundle.SHA256) {
		return archive.SourceBundle{}, errors.New("source checksum mismatch")
	}
	// The verified compressed bytes are decoded as a stream, one JSONL line at
	// a time: the decompressed document is never held whole, only the
	// assembled records the caller asked for.
	bundle, err := archive.ReadSourceBundle(bytes.NewReader(data), archive.DecodeOptions{MaxUncompressedBytes: int64(limits.uncompressed())})
	if err != nil {
		return archive.SourceBundle{}, fmt.Errorf("decode source: %w", err)
	}
	if bundle.History != nil && (metadata.History == nil || metadata.History.CurrentRevision != bundle.History.ActiveRolloutID) {
		return archive.SourceBundle{}, errors.New("source revision disagrees with metadata")
	}
	if (bundle.SchemaVersion != archive.SourceSchemaVersion && bundle.SchemaVersion != archive.HistorySourceSchemaVersion) || bundle.ArchiveSessionID != metadata.SessionID || bundle.NativeSessionID != metadata.NativeSessionID || bundle.ProjectID != metadata.ProjectID || bundle.ParentSessionID != metadata.ParentSessionID || !sourceHarnessMatches(bundle.Capture.Harness, metadata.Harness, preserved) || !bundle.Capture.CapturedAt.Equal(metadata.CapturedAt) || (filter != nil && bundle.Capture.FilterVersion != *filter) || (schema != 0 && bundle.SchemaVersion != schema) {
		return archive.SourceBundle{}, errors.New("source identity does not match metadata")
	}
	key, err := archive.SourceObjectKey(bundle, metadata.SourceBundle.SHA256)
	if err != nil || key != metadata.SourceBundle.Key {
		return archive.SourceBundle{}, errors.New("source key does not match metadata identity")
	}
	return bundle, nil
}

// A preserved physical revision has its own producer version and mode, bound
// by the immutable source checksum. The active sidecar cannot supply those
// observations, while the harness name remains part of stable source identity.
func sourceHarnessMatches(source, active archive.Harness, preserved bool) bool {
	if preserved {
		return source.Name == active.Name
	}
	return source == active
}

// RefreshAndLoad retries once after rereading metadata, covering the normal
// metadata-pointer refresh race after old source cleanup.
func RefreshAndLoad(ctx context.Context, store storage.ObjectStore, metadataKey string, limits Limits) (archive.Metadata, archive.SourceBundle, error) {
	metadata, err := ReadMetadata(ctx, store, metadataKey)
	if err != nil {
		return archive.Metadata{}, archive.SourceBundle{}, err
	}
	return RefreshAndLoadMetadata(ctx, store, metadataKey, metadata, limits)
}

// RefreshAndLoadMetadata verifies source bytes using already-read metadata and
// retries once with fresh metadata when the referenced source has disappeared.
func RefreshAndLoadMetadata(ctx context.Context, store storage.ObjectStore, metadataKey string, metadata archive.Metadata, limits Limits) (archive.Metadata, archive.SourceBundle, error) {
	bundle, err := LoadSource(ctx, store, metadata, limits)
	if !errors.Is(err, ErrRefreshRequired) {
		return metadata, bundle, err
	}
	metadata, err = ReadMetadata(ctx, store, metadataKey)
	if err != nil {
		return archive.Metadata{}, archive.SourceBundle{}, err
	}
	bundle, err = LoadSource(ctx, store, metadata, limits)
	return metadata, bundle, err
}
