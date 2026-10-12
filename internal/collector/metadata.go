package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// parserFor resolves one immutable capability per agent during a collector pass.
func (o Options) parserFor(name string) agentapi.TranscriptParser {
	if parser, found := o.parserCache[name]; found {
		return parser
	}
	var parser agentapi.TranscriptParser
	if o.Parsers != nil {
		var found bool
		parser, found = o.Parsers.LookupParser(name)
		if !found {
			parser = nil
		}
	}
	if o.parserCache != nil {
		o.parserCache[name] = parser
	}
	return parser
}

func (o Options) parserVersionFor(name string) string {
	if o.ParserVersion != "" {
		return o.ParserVersion
	}
	if parser := o.parserFor(name); parser != nil {
		return parser.Version()
	}
	return "unavailable"
}

// regenerateMetadata is the scan's refresh step: it re-derives the last
// publication's metadata from durable filtered evidence before opening the
// live transcript, so a parser upgrade still works after the application
// rotates its local log. handled reports that the scan ends here.
//
// Regeneration is best effort and must never stand between a session and
// normal capture: whatever makes the retained publication unusable here (no
// cached metadata and no readable remote copy, or metadata that does not
// describe this machine's retained source) is a reason to skip, not to fail
// the session, because the next content change publishes current-parser
// metadata anyway. Only the publish attempt itself reports errors.
func regenerateMetadata(s *sessionScan) (outcome sessionOutcome, handled bool, err error) {
	key, err := archive.MetadataObjectKey(s.reg.Harness.Name, s.id())
	if err != nil {
		return outcomeSkipped, false, err
	}
	last, ok, lastErr := s.lastPublication(key)
	if lastErr != nil {
		return outcomeSkipped, true, lastErr
	}
	if !ok {
		return outcomeSkipped, false, nil
	}
	if !sourceEvidenceWithinPolicy(last.bundle.SupplementalEvidence, s.opts.skillEvidence()) {
		return outcomeSkipped, false, nil
	}
	// A commit the hooks recorded since is published first, from the
	// retained metadata: it needs no derivation, so neither a refresh this
	// parser cannot make nor one it already skipped holds it back.
	if outcome, handled, err := s.publishRecordedGitHead(last, key); handled || err != nil {
		return outcome, handled, err
	}
	prior := last.metadata
	sameParser := prior.Parser.Version == s.parserVersion()
	projectLabel := prior.ProjectName
	if s.reg.HistoricalProject != nil {
		preview := prior
		applyProjectName(&preview, s.reg, s.opts)
		projectLabel = preview.ProjectName
	}
	if sameParser && !last.legacy && projectLabel == prior.ProjectName {
		return outcomeSkipped, false, nil
	}
	// The metadata must describe the source actually uploaded last, as
	// recorded at upload. (For state older than that record,
	// LastPublishedSource reads it from the cached metadata itself, so this
	// holds trivially.)
	uploaded, known := s.published.LastPublishedSource()
	if known && prior.SourceBundle != uploaded {
		return outcomeSkipped, false, nil
	}
	// A refresh this parser already found impossible for this publication is
	// not attempted again: each attempt costs a full decode and recompression
	// of the retained bundle, and possibly a download. A new publication
	// clears the record.
	if skipped, found, err := s.local.LoadRefreshSkip(s.id()); err != nil {
		return outcomeSkipped, false, err
	} else if found && skipped.ParserVersion == s.parserVersion() && skipped.SourceKey == prior.SourceBundle.Key {
		return outcomeSkipped, false, nil
	}
	source, ok, sourceErr := s.chooseRefreshSource(last.bundle, uploaded, known)
	if sourceErr != nil {
		return outcomeSkipped, true, sourceErr
	}
	if !ok {
		// Neither the bytes nor a recorded reference: nothing to publish
		// against. The next content change publishes current metadata.
		return outcomeSkipped, false, nil
	}
	// Cache before any early return below, so a legacy publication is
	// migrated exactly once rather than re-read on every scan.
	if err := s.published.CacheMetadata(last.encoded); err != nil {
		return outcomeSkipped, false, err
	}
	if sameParser && projectLabel == prior.ProjectName {
		// The same parser over the same retained source yields the same
		// result, including a failed parse: nothing to rebuild.
		return outcomeSkipped, false, nil
	}
	if s.liveTranscriptChanged(last.bundle) {
		// Normal capture is about to publish current-parser metadata with
		// the new content; a metadata-only publication first would only be
		// wasted work that also starts the upload interval early.
		return outcomeSkipped, false, nil
	}
	metadataBytes, changed, err := s.refreshedMetadata(last, source.ref)
	if err != nil || !changed {
		return outcomeSkipped, false, err
	}
	pending := state.PendingPublication{ImportExpectedMetadataSHA256: func() string {
		if s.reg.StagedImportSHA256 != "" {
			return metadataSHA(last.encoded)
		}
		return ""
	}(), SkillEvidence: string(s.opts.skillEvidence()), MetadataOnly: true, Bundle: last.bundle, SourceKey: source.ref.Key, MetadataKey: key, SourceSHA256: source.ref.SHA256, SourceBytes: source.bytes, SourceSize: source.ref.CompressedBytes, MetadataBytes: metadataBytes, ReadyAt: s.now, Attempted: true}
	if err := s.local.SavePending(s.id(), pending); err != nil {
		return outcomeSkipped, false, err
	}
	outcome, err = s.publishPending(pending)
	return outcome, true, err
}

// refreshedMetadata derives current-parser metadata for the last
// publication, pointing at source. changed is false when it equals what was
// published apart from its derivation time, or when this build cannot
// derive it at all; the latter is recorded (a refresh skip) so it is not
// retried on every pass and status can count it.
func (s *sessionScan) refreshedMetadata(last lastPublication, source archive.SourceReference) (encoded []byte, changed bool, err error) {
	prior := last.metadata
	analysis, parseErr := agentapi.Analyze(s.ctx, s.resolveParser(), last.bundle)
	if errors.Is(parseErr, context.Canceled) || errors.Is(parseErr, context.DeadlineExceeded) {
		return nil, false, parseErr
	}
	next, buildErr := archive.BuildMetadataWithAnalysis(last.bundle, analysis, parseErr, s.opts.MachineID, s.reg.SessionStartedAt, s.now, source, archive.ParserInfo{Version: s.parserVersion()})
	next.ApplyRegistrationProvenance(s.reg)
	applyProjectName(&next, s.reg, s.opts)
	next.ApplyRepoKey(s.opts.repoKeyOr(s.reg, func() string { return prior.RepoKey }))
	next.ApplyGitHead(s.reg)
	next.ApplyReplay(s.reg)
	if buildErr != nil && !archive.IsParseError(buildErr) {
		// This build cannot derive metadata from the retained bundle at all
		// (one cached under an older source schema, say). That is not a
		// failure of the session.
		skip := state.RefreshSkip{ParserVersion: s.parserVersion(), SourceKey: prior.SourceBundle.Key, Reason: state.RefreshSkipUnderivable}
		return nil, false, s.local.SaveRefreshSkip(s.id(), skip)
	}
	// Derivation/representation maintenance keeps every alternative and its age.
	if prior.History != nil {
		if err := s.validateAuthorityIdentity(prior); err != nil {
			return nil, false, err
		}
		next.SchemaVersion = prior.SchemaVersion
		next.History = prior.History
		next.CapturedAt = prior.CapturedAt
		if _, err := next.SourceReferences(); err != nil {
			return nil, false, err
		}
	}
	// Unchanged metadata over the same source needs no publication.
	comparison := next
	comparison.MetadataDerivedAt = prior.MetadataDerivedAt
	oldBytes, err := s.marshalRetained(prior)
	if err != nil {
		return nil, false, err
	}
	comparisonBytes, err := s.marshalRetained(comparison)
	if err != nil {
		return nil, false, err
	}
	if bytes.Equal(oldBytes, comparisonBytes) {
		return nil, false, nil
	}
	encoded, err = s.marshalRetained(next)
	return encoded, err == nil, err
}

// lastPublication is what regenerateMetadata refreshes: the last published
// bundle and the metadata document published with it.
type lastPublication struct {
	bundle   archive.SourceBundle
	metadata archive.Metadata
	encoded  []byte
	// legacy means no metadata was cached locally and encoded is the copy
	// read from storage (publications made before metadata was cached).
	legacy bool
}

// lastPublication returns the session's last publication for a refresh. ok
// is false when there is nothing usable to refresh: nothing was ever
// published (a blocked, declined, or rate-limited candidate cached alongside
// it was never made discoverable), or the metadata is unreadable, missing
// from storage, or describes another session, machine, or source.
func (s *sessionScan) lastPublication(metadataKey string) (lastPublication, bool, error) {
	bundle, _, found := s.published.LastPublished()
	if !found {
		return lastPublication{}, false, nil
	}
	encoded := s.published.Metadata()
	legacy := len(encoded) == 0
	if legacy {
		// One-time migration for publications made before metadata was cached.
		// A missing or unreachable copy is not fatal: nothing can be refreshed
		// from it, and normal capture keeps working without it.
		var err error
		if encoded, err = s.historyGet(metadataKey, historyMetadataLimit); err != nil {
			if errors.Is(err, agentapi.ErrReadBudget) {
				return lastPublication{}, false, err
			}
			return lastPublication{}, false, nil
		}
	}
	var metadata archive.Metadata
	decodeErr := s.unmarshalRetained(encoded, &metadata)
	if errors.Is(decodeErr, agentapi.ErrReadBudget) {
		return lastPublication{}, false, decodeErr
	}
	decodeValid := decodeErr == nil
	if !decodeValid {
		return lastPublication{}, false, nil
	}
	referenceValid := metadata.ValidateSourceReference() == nil
	if metadata.SessionID != s.id() || metadata.MachineID != s.opts.MachineID || !referenceValid {
		return lastPublication{}, false, nil
	}
	return lastPublication{bundle: bundle, metadata: metadata, encoded: encoded, legacy: legacy}, true, nil
}

// refreshSource is the source a refreshed metadata document points at, and
// the bytes to publish it with, if this build has them.
type refreshSource struct {
	ref   archive.SourceReference
	bytes []byte
}

// chooseRefreshSource picks the source a metadata refresh points at:
//   - This build reproduces the uploaded source's exact bytes: they are
//     carried, so the source is re-uploaded if it has gone missing.
//   - It builds the retained bundle, but to other bytes (a compressor
//     change): those bytes are published as a new source, which supersedes
//     the old one. The remote is repaired whatever state the old object is
//     in, and from then on the recorded source is one this build reproduces.
//   - It cannot build the bundle at all (a source schema bump): only the
//     recorded reference (uploaded, when known) is carried, and the source is
//     checked in storage against it (see sessionScan.upload).
//
// ok is false when it has neither bytes nor a recorded reference.
// liveTranscriptChanged reports whether normal capture will publish this
// scan: the source (a transcript on disk, or a Cursor database chat) carries
// evidence the cached comparison bundle does not, and it still extends what
// capture guards against (the same rules sessionScan.guard applies), so a
// rewrite that capture will only record as a gap does not count. A Cursor
// database chat that was rewritten does count: capture publishes it. It
// compares against the cached candidate rather than only the last
// publication so a declined candidate the transcript still matches leaves
// regeneration free to proceed. A transcript that cannot be compared
// (rotated, oversize, unsafe) reports no change: regeneration is then the
// only way the summary can move.
func (s *sessionScan) liveTranscriptChanged(lastPublished archive.SourceBundle) bool {
	if s.reg.CaptureFrozen || s.reg.HistoricalProject != nil {
		return false
	}
	source, ok := newSourceReader(s.reg, s.opts)
	if !ok {
		return false
	}
	cached, _, status, found := s.published.Cached()
	if !found {
		return false
	}
	if s.sourceSettled(source) {
		// Read at this very state by a scan that settled, through the same
		// filter and adapter: it would filter to what that scan cached.
		// Only the parser moved, and filtering every historical session's
		// transcript to find that out made the first pass after an upgrade
		// cost a full read of each.
		return false
	}
	adapter, err := sourceAdapter(s.opts.Sources, s.reg.Harness.Name)
	if err != nil {
		return false
	}
	filterMark := len(s.retainedReleases)
	filtered, observed, err := source.Filter(s.ctx, adapter, s.opts.maxTranscriptBytes())
	if err != nil {
		// Native input is optional for a retained-source refresh, but an owned
		// resource that could not be released must remain visible to the caller.
		if agentapi.HasFailure(err, agentapi.Cleanup) {
			s.warn(err)
		}
		return false
	}
	// Normal capture reads this same source next unless the refresh ends the
	// scan, so it keeps what was read here.
	s.filtered = &filteredSource{filterLease: s.nativeFilterLease(filterMark), adapter: adapter, transcript: filtered, observed: observed}
	candidateOwner := len(s.retainedReleases)
	candidate, err := s.newSourceBundle(s.reg, adapter, filtered, s.now, cached.SupplementalEvidence)
	if err != nil {
		return false
	}
	// This probe returns only a decision; its independent decoded rows end here.
	defer s.releaseRetainedIndex(candidateOwner)
	same, err := s.bundleEvidenceEqual(cached, candidate)
	if err != nil || same {
		return false
	}
	semantics, err := sourceSemantics(s.opts.Sources, s.reg)
	if err != nil {
		return false
	}
	if semantics.Mutation == agentapi.ReplaceableSnapshot {
		return true
	}
	guard := cached
	if status == state.CacheStatusBlocked {
		guard = lastPublished
	}
	return nativeEvidenceExtends(adapter, guard, candidate)
}

func (s *sessionScan) resolveParser() agentapi.TranscriptParser {
	if !s.parserResolved {
		s.parserResolved = true
		s.parser = s.opts.parserFor(s.reg.Harness.Name)
	}
	return s.parser
}

func (s *sessionScan) parserVersion() string {
	if s.opts.ParserVersion != "" {
		return s.opts.ParserVersion
	}
	if parser := s.resolveParser(); parser != nil {
		return parser.Version()
	}
	return "unavailable"
}

// publishRecordedGitHead updates hook observations even when a stop brought
// no new transcript bytes. It reuses retained metadata and source, preserving
// capture time and parser output; it never runs git or re-derives metadata.
// It needs no request: the scan signature's PublishedLastHead brings a
// session whose registration has moved past it back for a scan.
func (s *sessionScan) publishRecordedGitHead(last lastPublication, key string) (sessionOutcome, bool, error) {
	next := last.metadata
	next.ApplyGitHead(s.reg)
	oldHead, err := s.marshalRetained(last.metadata.GitHead)
	if err != nil {
		return outcomeSkipped, false, err
	}
	newHead, err := s.marshalRetained(next.GitHead)
	if err != nil {
		return outcomeSkipped, false, err
	}
	if bytes.Equal(oldHead, newHead) {
		return outcomeSkipped, false, nil
	}
	// Normal capture will carry these observations when content grew. Avoid an
	// extra publication and preserve the request's new lifecycle evidence.
	if s.liveTranscriptChanged(last.bundle) || s.requestAddsEvidence(last.bundle) {
		return outcomeSkipped, false, nil
	}
	uploaded, known := s.published.LastPublishedSource()
	if known && uploaded != next.SourceBundle {
		return outcomeSkipped, false, nil
	}
	// Carry the retained source's bytes when this build can rebuild them, as
	// a metadata refresh does, so a source missing from storage is repaired
	// instead of failing this publication on every pass.
	source := refreshSource{ref: next.SourceBundle}
	rebuilt, ok, rebuildErr := s.chooseRefreshSource(last.bundle, uploaded, known)
	if rebuildErr != nil {
		return outcomeSkipped, true, rebuildErr
	}
	if ok && rebuilt.bytes != nil {
		source = rebuilt
	}
	next.SourceBundle = source.ref
	next.MetadataDerivedAt = s.now
	encoded, err := s.marshalRetained(next)
	if err != nil {
		return outcomeSkipped, false, err
	}
	pending := state.PendingPublication{SkillEvidence: string(s.opts.skillEvidence()), MetadataOnly: true, Bundle: last.bundle, SourceKey: source.ref.Key, MetadataKey: key, SourceSHA256: source.ref.SHA256, SourceBytes: source.bytes, SourceSize: source.ref.CompressedBytes, MetadataBytes: encoded, ReadyAt: s.now, Attempted: true}
	if err := s.local.SavePending(s.id(), pending); err != nil {
		return outcomeSkipped, false, err
	}
	outcome, err := s.publishPending(pending)
	if err != nil {
		return outcome, true, err
	}
	// The scan ends here, before it would record a signature: note the
	// published commit on the one standing, so the next pass can skip.
	if signature, found, err := s.local.LoadScanSignature(s.id()); err != nil || !found {
		return outcome, true, err
	} else if next.GitHead != nil {
		signature.PublishedLastHead = headFingerprint(next.GitHead.Last)
		return outcome, true, s.local.SaveScanSignature(s.id(), signature)
	}
	return outcome, true, nil
}

// headFingerprint identifies a last-HEAD observation for the scan signature:
// its commit and first-seen time, so the same commit seen again after
// another is told apart. "" for none.
func headFingerprint(h *archive.GitHead) string {
	if !h.Valid() {
		return ""
	}
	return h.SHA + "@" + h.ObservedAt.UTC().Format(time.RFC3339Nano)
}

// publishedLastHead is the last-HEAD observation the session's published
// metadata holds (headFingerprint), for the scan signature. With nothing
// published yet there is nothing to update, and the registration's own is
// taken as settled: the first publication carries it. With a publication
// but no metadata cached (an older install's), it is unknown, "", so a
// moved HEAD keeps the session scanned until its metadata is read.
func (s *sessionScan) publishedLastHead() string {
	encoded := s.published.Metadata()
	if len(encoded) == 0 {
		if _, _, published := s.published.LastPublished(); published {
			return ""
		}
		return headFingerprint(s.reg.LastHead)
	}
	var published struct {
		GitHead *archive.SessionGitHead `json:"git_head"`
	}
	if err := json.Unmarshal(encoded, &published); err != nil || published.GitHead == nil {
		return ""
	}
	return headFingerprint(published.GitHead.Last)
}

// requestAddsEvidence reports whether the request carries hook evidence the
// published bundle lacks. Normal capture folds that evidence and the HEAD
// observation into one publication and completes the request; a
// metadata-only update would leave the request for a second upload.
func (s *sessionScan) requestAddsEvidence(published archive.SourceBundle) bool {
	if len(s.req.HookEvidence) == 0 {
		return false
	}
	base := limitSkillEvidence(published.SupplementalEvidence, s.opts.skillEvidence())
	merged := limitSkillEvidence(mergeSupplementalEvidence(base, s.req.HookEvidence), s.opts.skillEvidence())
	return len(merged) != len(base)
}
