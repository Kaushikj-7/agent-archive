package state

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// CacheStatus distinguishes why a bundle sits in the local published cache,
// since only some of those reasons should be auto-retried once time passes.
type CacheStatus string

const (
	// CacheStatusPublished means this exact bundle was actually published.
	CacheStatusPublished CacheStatus = "published"
	// CacheStatusRateLimited means this bundle was built and differs from
	// what's published, but was withheld by the minimum upload interval; it
	// is eligible to auto-publish once that interval elapses.
	CacheStatusRateLimited CacheStatus = "rate_limited"
	// CacheStatusDeclined means this bundle was deliberately not published
	// by policy (see collector.Options.RequireSkillUse), not by cadence.
	// Unlike CacheStatusRateLimited, it must never auto-publish just because
	// time passed — only a genuine further content change reconsiders it.
	CacheStatusDeclined CacheStatus = "declined"
	// CacheStatusBlocked means the session's current transcript can no longer
	// be captured safely (see BlockedReason) and, unlike a transient failure,
	// the condition cannot clear by retrying: it is a recorded capture gap,
	// not an error. The last published snapshot stays retained and any
	// outstanding request is acknowledged, so later passes are no-ops until
	// the transcript changes again.
	CacheStatusBlocked CacheStatus = "blocked"
)

// BlockedReason says why a session sits in CacheStatusBlocked.
type BlockedReason string

const (
	// BlockedReasonTranscriptRewritten means the transcript was truncated,
	// compacted, or rewritten so it no longer extends the retained evidence.
	BlockedReasonTranscriptRewritten BlockedReason = "transcript_rewritten"
	// BlockedReasonTranscriptTooLarge means the transcript exceeds the
	// collection size limit (collector.Options.MaxTranscriptBytes).
	BlockedReasonTranscriptTooLarge BlockedReason = "transcript_too_large"
	// BlockedReasonRecordTooLarge means one record of the transcript is longer
	// than archive.MaxRecordBytes, so the transcript cannot be read whole.
	BlockedReasonRecordTooLarge BlockedReason = "record_size_limit"
	// BlockedReasonTranscriptMissing means the native transcript is no longer
	// on disk. Every supported application deletes its own transcripts on its
	// own schedule (Claude Code after cleanupPeriodDays, 30 by default) while
	// this archive retains sessions for far longer, so a session outliving its
	// transcript is the steady state, not a failure. Unlike the other reasons
	// this one can end: if the file comes back, the next scan clears the block.
	BlockedReasonTranscriptMissing BlockedReason = "transcript_missing"
)

// Recoverable reports whether a block can end without the session changing:
// only a missing file can reappear. A rewritten or oversize transcript stays
// rewritten or oversize until its content changes, which clears the block
// through the normal comparison instead.
func (r BlockedReason) Recoverable() bool { return r == BlockedReasonTranscriptMissing }

// publishedState is the small local cache of what was last built for a
// session: the exact source bundle (so a later scan can detect "no
// meaningful change" without redownloading or reparsing published history),
// when that happened, and why the bundle is in the state it's in. It is the
// JSON of published/<id>.json.
type publishedState struct {
	PublicationVersion int `json:"publication_version,omitempty"`
	// Summary restates, ahead of everything else in the file, the few facts
	// a caller that is not scanning the session needs (see
	// LoadPublishedSummary), so they can be read without decoding the source
	// bundles that follow. write keeps it in step; state written before it
	// existed has none and is decoded in full instead.
	Summary       *PublishedSummary    `json:"summary,omitempty"`
	MetadataBytes []byte               `json:"metadata_bytes,omitempty"`
	Bundle        archive.SourceBundle `json:"bundle"`
	PublishedAt   time.Time            `json:"published_at"`
	Status        CacheStatus          `json:"status"`
	// BlockedReason is set only while Status is CacheStatusBlocked.
	BlockedReason BlockedReason `json:"blocked_reason,omitempty"`
	// PreBlockStatus is the status a recoverable block replaced, so clearing
	// that block restores what was true before it rather than guessing.
	PreBlockStatus CacheStatus `json:"pre_block_status,omitempty"`
	// DeferredHookEvidence is hook evidence (a final response, a link) whose
	// request was acknowledged while a recoverable block was in force. The
	// transcript could not be read, so nothing could be built to carry it;
	// rather than drop it, the block holds it and hands it back as request
	// evidence the moment the transcript is readable again, so it publishes
	// with the recovery. Set only while Status is CacheStatusBlocked with a
	// recoverable reason.
	DeferredHookEvidence []archive.SupplementalEvidence `json:"deferred_hook_evidence,omitempty"`
	// LastPublished survives a newer rate-limited or declined candidate so
	// compaction checks and retention always have the actual remote baseline.
	LastPublished *publishedSnapshot `json:"last_published,omitempty"`
	// AgeFrom, when set, is when retention counts the cached bundle as
	// captured instead of its own CapturedAt, which was stamped by a clock
	// that was ahead (see Published.ClampAgeFrom).
	AgeFrom *ageClamp `json:"age_from,omitempty"`
}

// ageClamp is a retention age recorded for one capture: At stands in for the
// capture time For, and for no other.
type ageClamp struct {
	At  time.Time `json:"at"`
	For time.Time `json:"for"`
}

// captureAge keeps an unacknowledged meaningful candidate and the complete
// acknowledged manifest independent. Neither can age out the other's evidence.
func (p publishedState) captureAge(bundle archive.SourceBundle, raw []byte) time.Time {
	var m archive.Metadata
	if json.Unmarshal(raw, &m) != nil {
		m = archive.Metadata{}
	}
	return captureAgeFromMetadata(bundle, m)
}

func captureAgeFromMetadata(bundle archive.SourceBundle, m archive.Metadata) time.Time {
	at := bundle.Capture.CapturedAt
	if m.SessionID == bundle.ArchiveSessionID && m.NativeSessionID == bundle.NativeSessionID && m.ProjectID == bundle.ProjectID && m.Harness.Name == bundle.Capture.Harness.Name {
		if retained, err := m.MeaningfulCapturedAt(); err == nil && retained.After(at) {
			at = retained
		}
	}
	return at
}

// PublishedSummary is what a session's published state says about the
// session without its source bundles: enough for retention, for a subagent
// looking for its link in its parent, and for status.
type PublishedSummary struct {
	// LabelSourceChecksum invalidates narrow naming context without decoding source records.
	LabelSourceChecksum string `json:"label_source_checksum,omitempty"`
	// Complete source-set facts are derived only when acknowledged state changes.
	SourceSetDigest       string    `json:"source_set_digest,omitempty"`
	CurrentRevision       string    `json:"current_revision,omitempty"`
	SourceSchemaVersion   int       `json:"source_schema_version,omitempty"`
	MetadataSchemaVersion int       `json:"metadata_schema_version,omitempty"`
	SourceSetComplete     bool      `json:"source_set_complete,omitempty"`
	MeaningfulCapturedAt  time.Time `json:"meaningful_captured_at,omitzero"`

	// Harness is the cached bundle's harness: the one its objects are under.
	Harness string `json:"harness,omitempty"`
	// Status is the cached bundle's status, and BlockedReason why it is
	// blocked when it is.
	Status        CacheStatus   `json:"status"`
	BlockedReason BlockedReason `json:"blocked_reason,omitempty"`
	// CapturedAt is the cached bundle's capture time. AgeFrom, when set, is
	// the time retention ages it from instead (see Published.ClampAgeFrom).
	CapturedAt time.Time `json:"captured_at,omitzero"`
	AgeFrom    time.Time `json:"age_from,omitzero"`
	// Published reports that a publication was ever recorded, and
	// LastPublishedAt when the last one was.
	Published       bool      `json:"published"`
	LastPublishedAt time.Time `json:"last_published_at,omitzero"`
	// LinkedPublished lists the sessions the cached bundle links to as
	// published: a parent's published subagents.
	LinkedPublished []string `json:"linked_published,omitempty"`
}

// RetentionAge is the time retention ages the session from: AgeFrom when a
// clamp is recorded, otherwise the capture time.
func (s PublishedSummary) RetentionAge() time.Time {
	if !s.AgeFrom.IsZero() {
		return s.AgeFrom
	}
	if !s.MeaningfulCapturedAt.IsZero() {
		return s.MeaningfulCapturedAt
	}
	return s.CapturedAt
}

// LinksPublished reports whether the cached bundle links child as published.
func (s PublishedSummary) LinksPublished(child string) bool {
	return slices.Contains(s.LinkedPublished, child)
}

// summary derives the state's PublishedSummary.
func (p publishedState) summary() PublishedSummary {
	var m archive.Metadata
	if json.Unmarshal(p.MetadataBytes, &m) != nil {
		m = archive.Metadata{}
	}
	return p.summaryFromMetadata(m)
}

func (p publishedState) summaryFromMetadata(m archive.Metadata) PublishedSummary {
	source, _ := p.lastPublishedSource()
	_, lastPublishedAt, published := p.resolveLastPublished()
	var ageFrom time.Time
	if clamp := p.AgeFrom; clamp != nil && clamp.For.Equal(captureAgeFromMetadata(p.Bundle, m)) {
		ageFrom = clamp.At
	}
	var linked []string
	for _, link := range p.Bundle.LinkedSessions {
		if link.Status == archive.LinkedSessionPublished && !slices.Contains(linked, link.SessionID) {
			linked = append(linked, link.SessionID)
		}
	}
	digest, revision := "", ""
	complete := false
	sourceSchema, metadataSchema := 0, 0
	if m.SessionID == p.Bundle.ArchiveSessionID && m.NativeSessionID == p.Bundle.NativeSessionID && m.ProjectID == p.Bundle.ProjectID && m.Harness.Name == p.Bundle.Capture.Harness.Name {
		if observedDigest, err := m.SourceSetDigest(); err == nil {
			digest, complete = observedDigest, true
			acknowledged, _, _ := p.resolveLastPublished()
			sourceSchema, metadataSchema = acknowledged.SchemaVersion, m.SchemaVersion
			if m.History != nil {
				revision = m.History.CurrentRevision
			}
		}
	}
	return PublishedSummary{
		LabelSourceChecksum: source.SHA256,
		SourceSetDigest:     digest, CurrentRevision: revision,
		SourceSetComplete: complete, SourceSchemaVersion: sourceSchema, MetadataSchemaVersion: metadataSchema,
		MeaningfulCapturedAt: captureAgeFromMetadata(p.Bundle, m),
		Harness:              p.Bundle.Capture.Harness.Name,
		Status:               p.Status, BlockedReason: p.BlockedReason, CapturedAt: p.Bundle.Capture.CapturedAt, AgeFrom: ageFrom,
		Published: published, LastPublishedAt: lastPublishedAt, LinkedPublished: linked,
	}
}

type publishedSnapshot struct {
	Selection   *ReadySelection      `json:"selection_commit,omitempty"`
	Bundle      archive.SourceBundle `json:"bundle"`
	PublishedAt time.Time            `json:"published_at"`
	// Source is the source object this publication's metadata points at:
	// exactly the key, digest, and size that were uploaded. The next
	// publication names the object it supersedes from it. Rebuilding that
	// reference from Bundle is not reliable: a later build may serialize or
	// compress an old bundle differently, or refuse it outright after a
	// source schema bump. State written before this field existed is read
	// through its cached metadata instead (see lastPublishedSource).
	Source *archive.SourceReference `json:"source,omitempty"`
	// SameAsBundle means the last published bundle is the one in Bundle, so
	// this snapshot carries only its time. While a session sits in its normal
	// published state the two are always identical, and a source bundle is by
	// far the largest thing in this file: storing it once halves the file and
	// the cost of every decode of it. Bundle is materialized here again the
	// moment a different candidate (rate limited, declined, blocked) takes
	// over publishedState.Bundle. State written before this field existed
	// always carries its own copy, so it keeps working unchanged.
	SameAsBundle bool `json:"same_as_bundle,omitempty"`
}

// resolveLastPublished returns the bundle actually made discoverable remotely,
// expanding the shared-copy marker.
func (p publishedState) resolveLastPublished() (archive.SourceBundle, time.Time, bool) {
	if p.LastPublished == nil {
		// Backward compatibility with state written before the separate ledger.
		if p.Status == CacheStatusPublished {
			return p.Bundle, p.PublishedAt, true
		}
		return archive.SourceBundle{}, time.Time{}, false
	}
	if p.LastPublished.SameAsBundle {
		return p.Bundle, p.LastPublished.PublishedAt, true
	}
	return p.LastPublished.Bundle, p.LastPublished.PublishedAt, true
}

// detachedLastPublished gives the last published snapshot its own copy of the
// bundle, for use when publishedState.Bundle is about to become a different
// candidate. Called on every save that is not itself a publication.
func (p publishedState) detachedLastPublished() *publishedSnapshot {
	if p.LastPublished == nil || !p.LastPublished.SameAsBundle {
		return p.LastPublished
	}
	return &publishedSnapshot{Bundle: p.Bundle, PublishedAt: p.LastPublished.PublishedAt, Source: p.LastPublished.Source, Selection: p.LastPublished.Selection}
}

// lastPublishedSource returns the source reference of the last publication,
// as it was uploaded. found is false when nothing was published, or when
// state written before publishedSnapshot.Source existed has no readable
// cached metadata either: the reference is then unknown. It is never rebuilt.
func (p publishedState) lastPublishedSource() (archive.SourceReference, bool) {
	if _, _, published := p.resolveLastPublished(); !published {
		return archive.SourceReference{}, false
	}
	if p.LastPublished != nil && p.LastPublished.Source != nil {
		return *p.LastPublished.Source, true
	}
	// Older state: MetadataBytes is only ever replaced by a publication, so it
	// is the metadata document that went out with the last published source.
	if len(p.MetadataBytes) == 0 {
		return archive.SourceReference{}, false
	}
	// Only the reference is read, so metadata written under an older (or
	// newer) metadata schema still yields it.
	var metadata struct {
		SourceBundle archive.SourceReference `json:"source_bundle"`
	}
	if err := json.Unmarshal(p.MetadataBytes, &metadata); err != nil || !validSourceReference(metadata.SourceBundle) {
		return archive.SourceReference{}, false
	}
	return metadata.SourceBundle, true
}

// validSourceReference reports whether ref names an object and carries a
// SHA-256 digest in hex.
func validSourceReference(ref archive.SourceReference) bool {
	if ref.Key == "" || len(ref.SHA256) != 64 {
		return false
	}
	_, err := hex.DecodeString(ref.SHA256)
	return err == nil
}

// next returns the state that saving bundle with status makes of p: the
// transition every save goes through. publishedAt is the last actual publish
// time. metadata, when non-empty, replaces the cached metadata (only a
// publication supplies it). reason and deferred apply to CacheStatusBlocked,
// source to CacheStatusPublished (the uploaded source, when known).
//
// The last published snapshot survives any status but a publication, which
// replaces it; a recoverable block remembers the status it replaced and
// accumulates the hook evidence it held back while it lasts.
func (p publishedState) next(bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, reason BlockedReason, metadata []byte, deferred []archive.SupplementalEvidence, source *archive.SourceReference) publishedState {
	last := p.detachedLastPublished()
	if last == nil && p.Status == CacheStatusPublished {
		snapshot := publishedSnapshot{Bundle: p.Bundle, PublishedAt: p.PublishedAt}
		last = &snapshot
	}
	if status == CacheStatusPublished {
		// The candidate becoming the current bundle is exactly what was just
		// published, so the snapshot records only when, not a second copy.
		last = &publishedSnapshot{PublishedAt: publishedAt, SameAsBundle: true, Source: source}
	}
	preBlock := p.PreBlockStatus
	switch {
	case status != CacheStatusBlocked:
		preBlock = ""
	case p.Status != CacheStatusBlocked:
		preBlock = p.Status
	}
	var held []archive.SupplementalEvidence
	if status == CacheStatusBlocked && reason.Recoverable() {
		// The same block continuing keeps what it already holds; a block
		// that replaces a settled state starts with only what arrived now.
		if p.Status == CacheStatusBlocked && p.BlockedReason == reason {
			held = p.DeferredHookEvidence
		}
		held = archive.MergeSupplementalEvidence(held, deferred)
	}
	if len(metadata) == 0 {
		metadata = p.MetadataBytes
	}
	next := publishedState{Bundle: bundle, PublishedAt: publishedAt, Status: status, BlockedReason: reason, PreBlockStatus: preBlock, DeferredHookEvidence: held, LastPublished: last, MetadataBytes: metadata}
	if p.AgeFrom != nil && p.AgeFrom.For.Equal(next.captureAge(bundle, metadata)) {
		next.AgeFrom = p.AgeFrom
	}
	return next
}

// Published is one session's published state, read from published/<id>.json
// once and kept in step with every change made through it. The file holds a
// whole source bundle, often megabytes, so a collector scan loads one per
// session and passes it through its steps instead of decoding the file at
// each of them. It assumes it is that file's only writer while in use, which
// the collector lock guarantees for a pass.
type Published struct {
	store *Store
	id    string
	state publishedState
	found bool
}

func (s *Store) publishedPath(archiveSessionID string) string {
	return filepath.Join(s.home, "published", archiveSessionID+".json")
}

// publishedStateLoads counts LoadPublishedState calls; see PublishedStateLoads.
var publishedStateLoads atomic.Int64

// PublishedStateLoads reports how many times this process has read a
// session's published state (LoadPublishedState, and every Store method
// built on it). The file holds whole source bundles, so a collector scan
// reads it once per session. It exists for tests, which use the count to
// keep it that way; production code has no use for it.
func PublishedStateLoads() int64 { return publishedStateLoads.Load() }

// LoadPublishedState reads a session's published state. A session with none
// yet yields an empty one (Found reports false) that saves create.
func (s *Store) LoadPublishedState(archiveSessionID string) (*Published, error) {
	if !safeFileComponent(archiveSessionID) {
		return nil, errors.New("archive session ID is not a safe file name component")
	}
	publishedStateLoads.Add(1)
	p := &Published{store: s, id: archiveSessionID}
	found, err := s.readOwned(s.publishedPath(archiveSessionID), &p.state)
	if err != nil {
		// In a collector pass a corrupt file was moved aside: reported once,
		// then the session is one that never published (quarantineInPass).
		return nil, fmt.Errorf("read published state %q: %w", archiveSessionID, s.afterLoss(archiveSessionID, err))
	}
	if found && p.state.PublicationVersion == 2 {
		if err := p.preserveReadyPublished(&p.state); err != nil {
			return nil, err
		}
	}
	p.found = found
	// Derive from the decoded source authority once per work scan, including
	// older compact summaries missing the newer source-set/age fields.
	if found {
		summary, err := s.summaryBudgeted(p.state)
		if err != nil {
			return nil, err
		}
		p.state.Summary = &summary
	}
	if !found {
		p.state = publishedState{}
	}
	return p, nil
}

func (p *Published) write(next publishedState) error {
	if err := p.preserveReadyPublished(&next); err != nil {
		return err
	}
	summary, err := p.store.summaryBudgeted(next)
	if err != nil {
		return err
	}
	next.Summary = &summary
	if err := p.store.writeCompact(p.store.publishedPath(p.id), next); err != nil {
		return err
	}
	p.state, p.found = next, true
	return nil
}

// Found reports whether anything has been saved for the session.
func (p *Published) Found() bool { return p.found }

// Summary returns the state's PublishedSummary (see LoadPublishedSummary).
func (p *Published) Summary() PublishedSummary {
	if p.state.Summary != nil {
		return *p.state.Summary
	}
	return p.state.summary()
}

// ClampAgeFrom records at as the time retention ages the cached bundle from,
// in place of a capture time that was stamped by a clock running ahead. Only
// an earlier time is recorded, and only for the capture cached now: the next
// capture, stamped by a clock that is right again, needs none.
func (p *Published) ClampAgeFrom(at time.Time) error {
	if !p.found {
		return fmt.Errorf("read published state %q: %w", p.id, os.ErrNotExist)
	}
	capturedAt := p.Summary().MeaningfulCapturedAt
	if capturedAt.IsZero() {
		capturedAt = p.state.Bundle.Capture.CapturedAt
	}
	if current := p.state.AgeFrom; (current != nil && current.For.Equal(capturedAt) && !at.Before(current.At)) || !at.Before(capturedAt) {
		return nil
	}
	next := p.state
	next.AgeFrom = &ageClamp{At: at.UTC(), For: capturedAt}
	return p.write(next)
}

// LoadPublishedSummary reads a session's PublishedSummary. found is false
// when nothing has been saved for the session. It reads only the summary at
// the head of the file, not the source bundles after it, so it costs about as
// much as a stat; state written before the summary existed is decoded in
// full.
func (s *Store) LoadPublishedSummary(archiveSessionID string) (summary PublishedSummary, found bool, err error) {
	if !safeFileComponent(archiveSessionID) {
		return PublishedSummary{}, false, errors.New("archive session ID is not a safe file name component")
	}
	summary, exists, head, _, err := s.publishedPrefix(archiveSessionID)
	if err != nil {
		return PublishedSummary{}, exists, err
	}
	if !exists {
		return PublishedSummary{}, false, nil
	}
	if head {
		return summary, true, nil
	}
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil || !p.found {
		return PublishedSummary{}, errors.Is(err, ErrDurableStorageRecovery), err
	}
	// State from before the summary existed is rewritten with one, once, by
	// the collector pass that owns it; until then every read would decode it
	// in full. A failed rewrite only leaves that cost for the next read.
	if s.collectorPass {
		_ = p.write(p.state)
	}
	return p.Summary(), true, nil
}

// readLeadingSummary decodes the summary that write puts first in a
// published state file. ok is false when there is none to trust: no file,
// an older file without one, or anything unexpected, all of which the
// caller answers with a full decode.
var errSelectingPublishedFullDecode = errors.New("selecting publication requires full validated decode")

func readLeadingSummary(reader io.Reader) (summary PublishedSummary, ok bool, err error) {
	decoder := json.NewDecoder(bufio.NewReaderSize(reader, 4096))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return summary, false, errors.Join(io.ErrUnexpectedEOF, err)
	}
	key, err := decoder.Token()
	if err != nil {
		return summary, false, err
	}
	if name, ok := key.(string); ok && strings.EqualFold(name, "publication_version") {
		var version int
		if decoder.Decode(&version) != nil || version != 2 {
			return summary, false, ErrDurableStorageRecovery
		}
		return summary, false, errSelectingPublishedFullDecode
	}
	if key != "summary" {
		return summary, false, nil
	}
	var head *PublishedSummary
	if err := decoder.Decode(&head); err != nil || head == nil {
		return summary, false, errors.Join(io.ErrUnexpectedEOF, err)
	}
	return *head, true, nil
}

// Cached returns the bundle a scan compares against and why it is there:
// the last publication, or a newer candidate (rate limited, declined,
// blocked). found is false when nothing has been saved.
func (p *Published) Cached() (bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, found bool) {
	return p.state.Bundle, p.state.PublishedAt, p.state.Status, p.found
}

// LastPublished returns the most recent bundle actually made discoverable
// by remote metadata. It deliberately ignores a newer local-only candidate.
func (p *Published) LastPublished() (bundle archive.SourceBundle, publishedAt time.Time, found bool) {
	return p.state.resolveLastPublished()
}

// LastPublishedSource returns the source reference of the last publication,
// exactly as uploaded: the object its live metadata points at. found is
// false when nothing was published, or when state from an old version
// records no reference (see publishedState.lastPublishedSource); callers
// must then not assume one, least of all by rebuilding it.
func (p *Published) LastPublishedSource() (archive.SourceReference, bool) {
	return p.state.lastPublishedSource()
}

// LastPublishedMetadata returns the supported complete acknowledged reference set.
// Unlike LastPublishedSource, it never extracts a pointer from an unknown schema.
func (p *Published) LastPublishedMetadata() (archive.Metadata, bool, error) {
	if len(p.state.MetadataBytes) == 0 {
		return archive.Metadata{}, false, nil
	}
	var metadata archive.Metadata
	if err := p.store.unmarshalOwned(p.state.MetadataBytes, &metadata); err != nil {
		return archive.Metadata{}, false, err
	}
	if _, err := metadata.SourceReferences(); err != nil {
		return archive.Metadata{}, false, err
	}
	bundle, _, found := p.LastPublished()
	if !found {
		return archive.Metadata{}, false, nil
	}
	if metadata.SessionID != p.id || metadata.NativeSessionID != bundle.NativeSessionID || metadata.ProjectID != bundle.ProjectID || metadata.Harness != bundle.Capture.Harness || metadata.ParentSessionID != bundle.ParentSessionID || metadata.FilterVersion != bundle.Capture.FilterVersion || !metadata.CapturedAt.Equal(bundle.Capture.CapturedAt) {
		return archive.Metadata{}, false, errors.New("acknowledged metadata disagrees with published snapshot")
	}
	if bundle.History != nil && (metadata.History == nil || metadata.History.CurrentRevision != bundle.History.ActiveRolloutID) {
		return archive.Metadata{}, false, errors.New("acknowledged metadata disagrees with published revision")
	}
	if ref, ok := p.LastPublishedSource(); ok && metadata.SourceBundle != ref {
		return archive.Metadata{}, false, errors.New("acknowledged metadata disagrees with published reference")
	}
	return metadata, true, nil
}

// RestorePublication records an independently verified remote publication after
// local state loss. A newer local-only candidate survives this recovery.
func (p *Published) RestorePublication(bundle archive.SourceBundle, source archive.SourceReference, metadata []byte, at time.Time) error {
	if !p.found {
		return p.SavePublication(bundle, at, source, metadata)
	}
	next := p.state
	next.MetadataBytes = metadata
	next.PublishedAt = at
	next.LastPublished = &publishedSnapshot{Bundle: bundle, PublishedAt: at, Source: &source}
	if next.Status == CacheStatusPublished {
		next.Bundle = bundle
		next.LastPublished.SameAsBundle = true
		next.LastPublished.Bundle = archive.SourceBundle{}
	}
	return p.write(next)
}

// Metadata returns the metadata document published with the last
// publication, or nil when none is cached (publications from before it was).
func (p *Published) Metadata() []byte { return p.state.MetadataBytes }

// Blocked reports whether the session is in CacheStatusBlocked, and why.
func (p *Published) Blocked() (BlockedReason, bool) {
	if p.state.Status != CacheStatusBlocked {
		return "", false
	}
	return p.state.BlockedReason, true
}

// Save records the outcome of a build/publish decision, so the next scan can
// compare against it instead of rebuilding from scratch. See CacheStatus for
// what each status means for retry. metadata, when given, replaces the
// cached metadata document.
func (p *Published) Save(bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, metadata ...[]byte) error {
	var doc []byte
	if len(metadata) > 0 {
		doc = metadata[0]
	}
	next, err := p.nextBudgeted(bundle, publishedAt, status, "", doc, nil, nil)
	if err != nil {
		return err
	}
	return p.write(next)
}

// SavePublication records a completed publication: bundle becomes both the
// comparison baseline and the last published snapshot, source is the object
// its metadata points at, and metadata is the document that was uploaded.
func (p *Published) SavePublication(bundle archive.SourceBundle, publishedAt time.Time, source archive.SourceReference, metadata []byte) error {
	next, err := p.nextBudgeted(bundle, publishedAt, CacheStatusPublished, "", metadata, nil, &source)
	if err != nil {
		return err
	}
	return p.write(next)
}

// SaveBlocked records a terminal capture gap (see CacheStatusBlocked).
// bundle is what the next scan compares against and publishedAt is the last
// actual publish time, if any; the last published snapshot itself is
// preserved exactly as Save preserves it. For a recoverable reason, deferred
// is hook evidence the block acknowledged and must hand back when it clears;
// it accumulates across saves of the same block and is dropped, having been
// handed back, by any other status.
func (p *Published) SaveBlocked(bundle archive.SourceBundle, publishedAt time.Time, reason BlockedReason, deferred ...archive.SupplementalEvidence) error {
	if reason == "" {
		return errors.New("blocked reason is required")
	}
	next, err := p.nextBudgeted(bundle, publishedAt, CacheStatusBlocked, reason, nil, deferred, nil)
	if err != nil {
		return err
	}
	return p.write(next)
}

// ClearRecoverableBlock ends a block whose condition has passed — today only a
// transcript that came back — by restoring the status the block replaced. It
// exists because a returning transcript whose content is byte-identical to the
// cached bundle produces no change for the normal comparison to act on, so
// without this the gap would be reported forever.
//
// Hook evidence the block held on to (see publishedState.DeferredHookEvidence)
// is handed back first, as an urgent request for the session, before the
// state is rewritten: the request is the durable carrier the collector
// already retries, so a crash at any point leaves the evidence pending
// rather than lost, and a replay adds nothing the request already holds.
// replayed reports that such a request was written, so the caller must
// reload the session's request rather than act on the one it loaded before.
//
// A block with no recorded previous status is not rewritten: there was no
// cached evidence before it, so the first real candidate replaces the whole
// state anyway, and anything it held is replayed (idempotently) until then.
func (p *Published) ClearRecoverableBlock(now time.Time) (restored CacheStatus, replayed, cleared bool, err error) {
	if !p.found {
		return "", false, false, nil
	}
	state := p.state
	if state.Status != CacheStatusBlocked || !state.BlockedReason.Recoverable() {
		return state.Status, false, false, nil
	}
	if len(state.DeferredHookEvidence) > 0 {
		if err := p.store.SaveRequest(p.id, "transcript-returned", now, state.DeferredHookEvidence...); err != nil {
			return "", false, false, fmt.Errorf("replay evidence held while blocked %q: %w", p.id, err)
		}
		replayed = true
	}
	if state.PreBlockStatus == "" || state.PreBlockStatus == CacheStatusBlocked {
		return state.Status, replayed, false, nil
	}
	restored = state.PreBlockStatus
	state.Status, state.BlockedReason, state.PreBlockStatus, state.DeferredHookEvidence = restored, "", "", nil
	if err := p.write(state); err != nil {
		return "", replayed, false, fmt.Errorf("clear block %q: %w", p.id, err)
	}
	return restored, replayed, true, nil
}

// CacheMetadata stores the metadata document of the last publication, for
// state written before it was cached. An unchanged document is not rewritten.
func (p *Published) CacheMetadata(metadata []byte) error {
	if !p.found {
		return fmt.Errorf("read published state %q: %w", p.id, os.ErrNotExist)
	}
	if bytes.Equal(p.state.MetadataBytes, metadata) {
		return nil
	}
	next := p.state
	next.MetadataBytes = metadata
	return p.write(next)
}

// SaveRepublishedMetadata records a metadata-only publication. Updating a
// summary must not discard a richer local-only candidate, so a rate-limited,
// declined, or blocked bundle stays the comparison baseline.
func (p *Published) SaveRepublishedMetadata(pending PendingPublication, at time.Time) error {
	if !p.found {
		return fmt.Errorf("read published state %q: %w", p.id, os.ErrNotExist)
	}
	next := p.state
	next.MetadataBytes = pending.MetadataBytes
	next.PublishedAt = at
	source := pending.SourceReference()
	if next.Status == CacheStatusPublished {
		// The current bundle is the republished one, so the snapshot shares it.
		next.Bundle = pending.Bundle
		next.LastPublished = &publishedSnapshot{PublishedAt: at, SameAsBundle: true, Source: &source}
	} else {
		next.LastPublished = &publishedSnapshot{Bundle: pending.Bundle, PublishedAt: at, Source: &source}
	}
	return p.write(next)
}

// The Store methods below read the published state and report on it: one
// decode per call, for callers outside a collector scan. A scan uses one
// Published for the whole session instead. Whole-file saves that bypass a
// scan's Published live in package statetest, for tests only.

// LoadBlocked reports whether a session is in CacheStatusBlocked and why.
func (s *Store) LoadBlocked(archiveSessionID string) (BlockedReason, bool, error) {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return "", false, err
	}
	reason, blocked := p.Blocked()
	return reason, blocked, nil
}

// LoadPublished returns the last cached bundle for a session, if any.
func (s *Store) LoadPublished(archiveSessionID string) (bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, found bool, err error) {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return archive.SourceBundle{}, time.Time{}, "", errors.Is(err, ErrDurableStorageRecovery), err
	}
	bundle, publishedAt, status, found = p.Cached()
	return bundle, publishedAt, status, found, nil
}

// LoadLastPublished returns the most recent bundle actually made discoverable
// by remote metadata. It deliberately ignores a newer local-only candidate.
func (s *Store) LoadLastPublished(archiveSessionID string) (bundle archive.SourceBundle, publishedAt time.Time, found bool, err error) {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return archive.SourceBundle{}, time.Time{}, errors.Is(err, ErrDurableStorageRecovery), err
	}
	bundle, publishedAt, found = p.LastPublished()
	return bundle, publishedAt, found, nil
}

// LoadLastPublishedSource is Published.LastPublishedSource for one session.
func (s *Store) LoadLastPublishedSource(archiveSessionID string) (archive.SourceReference, bool, error) {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return archive.SourceReference{}, false, err
	}
	source, found := p.LastPublishedSource()
	return source, found, nil
}

// PublishedMetadata is Published.Metadata for one session. It reports
// os.ErrNotExist when nothing has been saved for the session.
func (s *Store) PublishedMetadata(archiveSessionID string) ([]byte, error) {
	p, err := s.LoadPublishedState(archiveSessionID)
	if err != nil {
		return nil, err
	}
	if !p.found {
		return nil, fmt.Errorf("read published state %q: %w", archiveSessionID, os.ErrNotExist)
	}
	return p.Metadata(), nil
}
