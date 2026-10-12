// Package archive contains the privacy-first, storage-independent foundation
// for an Agent Archive collector. It deliberately has no filesystem, network,
// CLI, or credential dependencies.
package archive

import (
	"errors"
	"path/filepath"
	"strings"
	"time"
)

const (
	// SourceSchemaVersion 2 is the JSONL source bundle (source.<sha256>.jsonl.gz);
	// see source_jsonl.go. Schema 1, a single JSON document, is not read.
	SourceSchemaVersion = 2
	// MetadataSchemaVersion is the schema_version of every Metadata document
	// (schemas/metadata.schema.json). Readers reject any other version; see
	// Metadata.ValidateSourceReference.
	MetadataSchemaVersion = 1
	// FilterVersion 3 keeps tool arguments and tool-result linkage, reports the
	// key names it could not keep, and strips injected instruction blocks from
	// message text. Filter 4 also retains Claude Code's isMeta flag and strips
	// the text of every isMeta record. Filter 5 retains Claude Code's
	// compact_boundary system record (type, subtype, ids, timestamp only) and
	// the isCompactSummary and isVisibleInTranscriptOnly flags, as booleans.
	// Filter 6 retains Claude Code's origin (as {kind}) and promptSource.
	// Filter 7 sanitizes a Cursor text transcript per role section, so the
	// 64 KB cap applies per message instead of truncating the whole
	// transcript, and bounds it by MaxRecordBytes instead of 2 MB. JSONL
	// output is unchanged. Filter 8 adds the cursor-composer format, a chat
	// read from Cursor's database (CursorAdapter.FilterComposer); every other
	// format's output is unchanged. Filter 9 redacts credential assignments
	// by snake_case, SCREAMING_CASE, camelCase, and quoted names (value only),
	// drops base64 image, document, and file blocks, sanitizes structured
	// Cursor tool results before encoding them, keeps skill snapshots inside
	// their skill root, and truncates on a UTF-8 boundary. Filter 10 reads
	// a Cursor text transcript's role headers only at column 0, so an
	// indented "user:" or "system:" in tool output is content: it no longer
	// starts a section, hides the rest of the transcript, or un-hides a
	// hidden one; and a quoted credential value takes along whatever a shell
	// would glue onto it after the closing quote (`PASSWORD="abc"secret`).
	// Filter 13 keeps the name a session was given (Claude Code's
	// custom-title and pr-link records, Cursor's chat name). Filter 14 keeps
	// the description the parent gave a Claude Code subagent's task, read
	// from the sibling .meta.json. Filter 15 redacts aa-pair1 pairing bundles,
	// including truncated payloads. See dev/specs/privacy-filter.md.
	// Filter 18 additionally accepts pinned opt-in API label evidence.
	// Filter 17 retains typed, owning Codex session_labels metadata with
	// prompt redaction and a bounded display name; it never establishes activity.
	FilterVersion = "18"
	// OpenTelemetryGenAIRevision pins the upstream definitions used by the
	// three gen_ai.* attributes emitted by BuildMetadata. The archive is not
	// an OTLP payload; all agent_archive.* attributes are local extensions.
	OpenTelemetryGenAIRevision = "open-telemetry/semantic-conventions@aec6e9d3e86754683dab7c707655d69d953b2768 (v1.37.0)"
)

// ParserStatus reports how completely a metadata derivation covered its
// source. See schemas/metadata.schema.json for the authoritative enum.
type ParserStatus string

// Parser statuses. BuildMetadata reports partial unless its caller claims
// otherwise, and failed when the filtered source could not be parsed.
const (
	// ParserStatusPartial means the metadata was derived, but the parser does
	// not claim to understand every retained record. It is the default.
	ParserStatusPartial ParserStatus = "partial"
	// ParserStatusFailed means the filtered source could not be parsed, so
	// the metadata holds only what needs no parsing (identity, source
	// reference, hook-derived lifecycle); a later parser version may retry.
	ParserStatusFailed ParserStatus = "failed"
	// ParserStatusComplete means the adapter has demonstrated complete
	// coverage of its source. A reader's complete-coverage filter requires it.
	ParserStatusComplete ParserStatus = "complete"
)

// MetadataState is the session lifecycle state recorded on Metadata.
type MetadataState string

// Session lifecycle states, derived from the latest lifecycle hook evidence
// (and, for Cursor, its native end-of-turn record).
const (
	// MetadataStateActive means a session start or prompt was the latest
	// lifecycle event observed: a turn may be in progress.
	MetadataStateActive MetadataState = "active"
	// MetadataStateIdle means the latest turn was observed to end (stop,
	// interrupt, or failure) and the session was not seen to close.
	MetadataStateIdle MetadataState = "idle"
	// MetadataStateClosed means the session was observed to end.
	MetadataStateClosed MetadataState = "closed"
	// MetadataStateUnknown means no lifecycle evidence was observed.
	MetadataStateUnknown MetadataState = "unknown"
)

// TurnOutcome reports how the most recently observed turn ended. Completion
// means only that a supported native event explicitly reported completion; it
// is not a claim that the task succeeded.
type TurnOutcome string

// Turn outcomes.
const (
	// TurnOutcomeCompleted means the harness explicitly reported that the
	// turn completed.
	TurnOutcomeCompleted TurnOutcome = "completed"
	// TurnOutcomeInterrupted means the turn was interrupted, aborted, or
	// cancelled.
	TurnOutcomeInterrupted TurnOutcome = "interrupted"
	// TurnOutcomeError means the harness reported that the turn failed.
	TurnOutcomeError TurnOutcome = "error"
	// TurnOutcomeUnknown means no supported event reported how the turn
	// ended, including while a turn is still in progress.
	TurnOutcomeUnknown TurnOutcome = "unknown"
)

// SkillDetection reports whether skill-usage evidence was observed for a
// session and, if so, whether any skill was actually used.
type SkillDetection string

const (
	// SkillDetectionUnavailable means the harness gave no skill-usage signal.
	SkillDetectionUnavailable SkillDetection = "unavailable"
	// SkillDetectionObserved means at least one skill was used.
	SkillDetectionObserved SkillDetection = "observed"
	// SkillDetectionObservedNone means skills were known to be available but
	// none were used. A reader's "eligible but unused" filter requires this
	// exact value; see reader.SkillUsageEligibleNoUse.
	SkillDetectionObservedNone SkillDetection = "observed_none"
	// SkillDetectionPartial means skill-usage coverage was incomplete.
	SkillDetectionPartial SkillDetection = "partial"
)

// SkillCoverage classifies why a skill appears in Metadata.SkillsAvailable.
// Only SkillCoverageEligible and SkillCoverageDiscovered count as available
// for filtering; SkillCoverageInstalledOnly and any other hook-reported value
// do not.
type SkillCoverage string

// Skill coverage values.
const (
	// SkillCoverageEligible means the skill was reported as eligible for use
	// in the session.
	SkillCoverageEligible SkillCoverage = "eligible"
	// SkillCoverageDiscovered means the harness was reported to have
	// discovered the skill in the session.
	SkillCoverageDiscovered SkillCoverage = "discovered"
	// SkillCoverageInstalledOnly means the skill was only found installed on
	// disk, which does not show the session could use it.
	SkillCoverageInstalledOnly SkillCoverage = "installed_only"
)

// SkillUseEvidence names how a skill use was detected.
type SkillUseEvidence string

// Skill-use evidence values. A native invocation outranks a read inference
// for the same skill.
const (
	// SkillUseEvidenceNativeInvocation means the agent invoked the skill
	// through the harness's skill tool, or a hook reported an invocation.
	SkillUseEvidenceNativeInvocation SkillUseEvidence = "native_invocation"
	// SkillUseEvidenceReadInference means the use is inferred from the agent
	// reading a file inside the skill's directory; it is weaker evidence.
	SkillUseEvidenceReadInference SkillUseEvidence = "skill_read_inference"
)

// SupplementalEvidenceKind names the recognized shapes of hook or verified native metadata evidence.
// It is a named string, not a closed set: a hook may report a kind this
// package does not recognize, and such evidence is still preserved as-is.
type SupplementalEvidenceKind string

// Recognized supplemental evidence kinds. Each names the shape of one hook
// observation's payload: skill inventory, discovery, snapshot, invocation, and
// read; a lifecycle event; a final response; explicit user feedback; a linked
// child session; and a capture gap.
const (
	EvidenceKindSessionLabels    SupplementalEvidenceKind = "session_labels"
	EvidenceKindSkillInventory   SupplementalEvidenceKind = "skill_inventory"
	EvidenceKindSkillDiscovered  SupplementalEvidenceKind = "skill_discovered"
	EvidenceKindSkillSnapshot    SupplementalEvidenceKind = "skill_snapshot"
	EvidenceKindSkillInvocation  SupplementalEvidenceKind = "skill_invocation"
	EvidenceKindSkillRead        SupplementalEvidenceKind = "skill_read"
	EvidenceKindLifecycleHook    SupplementalEvidenceKind = "lifecycle_hook"
	EvidenceKindFinalResponse    SupplementalEvidenceKind = "final_response"
	EvidenceKindExplicitFeedback SupplementalEvidenceKind = "explicit_feedback"
	EvidenceKindLinkedSession    SupplementalEvidenceKind = "linked_session"
	EvidenceKindCaptureGap       SupplementalEvidenceKind = "capture_gap"
)

// LinkedSessionStatus is the durable producer state for a child session.
// Readers resolve it against remote metadata to distinguish an upload still
// pending from a published child whose metadata is now unavailable.
type LinkedSessionStatus string

// Linked session statuses.
const (
	// LinkedSessionPending means the child's transcript is known and its
	// upload has not been published yet.
	LinkedSessionPending LinkedSessionStatus = "pending"
	// LinkedSessionPublished means the child's metadata was published; if a
	// reader cannot find it now, it has been removed or expired.
	LinkedSessionPublished LinkedSessionStatus = "published"
	// LinkedSessionUnavailable means the child will not be archived, for
	// example because the harness gave no transcript for it or the candidate
	// was rejected.
	LinkedSessionUnavailable LinkedSessionStatus = "unavailable"
)

// LinkedSessionReference identifies a separately archived source. It carries
// no transcript content and never contributes to the containing session's
// counts.
type LinkedSessionReference struct {
	SessionID    string              `json:"session_id"`
	Relationship string              `json:"relationship"`
	Status       LinkedSessionStatus `json:"status"`
	ObservedAt   time.Time           `json:"observed_at"`
}

// ModelSummarySource names where a ModelSummary's attribution came from.
type ModelSummarySource string

// Model summary sources.
const (
	// ModelSummarySourceNativeTranscript means the model was read from the
	// retained transcript records.
	ModelSummarySourceNativeTranscript ModelSummarySource = "native_transcript"
	// ModelSummarySourceHook means a hook reported the model.
	ModelSummarySourceHook ModelSummarySource = "hook"
)

// ResponseModelStatus reports whether a model attribution came from the
// request side (not exposed to the archive) or was directly observed on a
// response.
type ResponseModelStatus string

// Response model statuses.
const (
	// ResponseModelStatusNotExposed means only the requested model is known
	// (gen_ai.request.model); the model that answered was not exposed.
	ResponseModelStatusNotExposed ResponseModelStatus = "not_exposed"
	// ResponseModelStatusObserved means the model was read from a response
	// (gen_ai.response.model).
	ResponseModelStatusObserved ResponseModelStatus = "observed"
)

// Config is the durable, non-secret configuration required to decide whether a
// session is eligible for capture. Credentials and runtime state do not belong
// in this type.
type Config struct {
	SchemaVersion int                 `json:"schema_version"`
	MachineID     string              `json:"machine_id"`
	Enabled       bool                `json:"enabled"`
	Projects      []ProjectActivation `json:"projects"`
}

// ProjectActivation explicitly opts a project into capture. Sessions which
// started before ActivatedAt are intentionally ineligible.
type ProjectActivation struct {
	ProjectID   string    `json:"project_id"`
	Root        string    `json:"root"`
	ActivatedAt time.Time `json:"activated_at"`
	Included    bool      `json:"included"`
}

// Eligible reports whether a project is explicitly included and the session
// began at or after its activation time. It performs lexical path matching; a
// caller is responsible for resolving symlinks before constructing Config.
func (c Config) Eligible(projectRoot string, sessionStartedAt time.Time) bool {
	if !c.Enabled || sessionStartedAt.IsZero() {
		return false
	}
	clean := filepath.Clean(projectRoot)
	for _, project := range c.Projects {
		if project.Included && filepath.Clean(project.Root) == clean && !project.ActivatedAt.IsZero() && !sessionStartedAt.Before(project.ActivatedAt) {
			return true
		}
	}
	return false
}

// Harness identifies the application which owns the native transcript.
type Harness struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	//lint:ignore LV1001 an external harness-reported value, set from plain strings in other packages
	Mode string `json:"mode,omitempty"`
}

// SessionOrigin is how a session entered the archive. The empty value means
// SessionOriginHook, so registrations written before the field decode as hook
// registrations.
type SessionOrigin string

// Session origins.
const (
	// SessionOriginHook means an application hook registered the session as
	// it ran.
	SessionOriginHook SessionOrigin = "hook"
	// SessionOriginImport means `agent-archive backfill` imported the session
	// after the fact, so it has no hook evidence.
	SessionOriginImport SessionOrigin = "import"
	// SessionOriginDiscovery means bounded local source discovery admitted the session.
	SessionOriginDiscovery SessionOrigin = "discovery"
)

// StartedAtSource says where a registration's SessionStartedAt came from. The
// empty value means StartedAtSourceHook.
type StartedAtSource string

// Sources of SessionStartedAt.
const (
	StartedAtSourceHook           StartedAtSource = "hook"            // when the hook fired
	StartedAtSourceTranscript     StartedAtSource = "transcript"      // earliest native record
	StartedAtSourceFileCreated    StartedAtSource = "file_created"    // file birth time; format has no timestamps
	StartedAtSourceCursorComposer StartedAtSource = "cursor_composer" // phase 2: composerData.createdAt
)

// CodexAdmissionProof is immutable evidence of fresh blanket admission.
type CodexAdmissionProof struct {
	Cwd        string `json:"cwd"`
	Generation string `json:"generation"`
	Revision   string `json:"revision"`
}

// ProjectResolution is private, bounded evidence retained with local ownership.
// Root is the selected configured spelling; Context is a content-free policy digest.
type ProjectResolution struct {
	OriginalCwd      string `json:"original_cwd"`
	CanonicalCwd     string `json:"canonical_cwd,omitempty"`
	Root             string `json:"root"`
	Method           string `json:"method"`
	RecordedRepoKey  string `json:"recorded_repo_key,omitempty"`
	Context          string `json:"context"`
	PolicyContext    string `json:"policy_context"`
	InventoryDigest  string `json:"inventory_digest,omitempty"`
	ValidationMethod string `json:"validation_method,omitempty"`
}

// SessionRegistration is the small hook-produced observation a later collector
// needs. TranscriptPath is local operational data and is never placed in a
// SourceBundle or Metadata document.
//
// SessionStartedAt is when the conversation began. It is not the capture
// boundary: project activation and the storage destination compare Admitted().
type SessionRegistration struct {
	HistoricalProject  *HistoricalProjectProof `json:"historical_project,omitempty"`
	ProjectResolution  *ProjectResolution      `json:"project_resolution,omitempty"`
	CodexBinding       *CodexSourceBinding     `json:"codex_binding,omitempty"`
	CodexCandidatePath string                  `json:"codex_candidate_path,omitempty"`
	CodexAdmission     *CodexAdmissionProof    `json:"codex_admission,omitempty"`
	ArchiveSessionID   string                  `json:"archive_session_id"`
	// PreviousGenerationID links recovery generations independently of subagents.
	PreviousGenerationID string `json:"previous_generation_id,omitempty"`
	// CaptureFrozen forbids further native capture; retained privacy maintenance remains.
	CaptureFrozen    bool      `json:"capture_frozen,omitempty"`
	NativeSessionID  string    `json:"native_session_id"`
	ProjectID        string    `json:"project_id"`
	ProjectRoot      string    `json:"project_root"`
	Harness          Harness   `json:"harness"`
	TranscriptPath   string    `json:"transcript_path"`
	SessionStartedAt time.Time `json:"session_started_at"`
	RegisteredAt     time.Time `json:"registered_at"`
	// RepoKey is RepoKey of the project's origin remote when the session
	// registered: a hash, never the URL. Empty when there was no portable
	// origin, or on older registrations; the collector then derives it from
	// ProjectRoot when it publishes.
	RepoKey string `json:"repo_key,omitempty"`
	// StartHead is the commit the session's working directory had checked
	// out, and whether its tree was dirty, when the hook registered it. Set
	// once; nil when git could not tell in time, and on registrations made
	// before the field, by backfill, or for a subagent.
	StartHead *GitHead `json:"start_head,omitempty"`
	// LastHead is HEAD at the latest stop hook that could read it, replaced
	// only when the commit changes (see GitHead.ObservedAt). Nil when no
	// stop hook has.
	LastHead *GitHead `json:"last_head,omitempty"`
	// LastHeadSeenAt fences delayed stop observations without changing the
	// first-seen time published in LastHead. Local only; absent on older state.
	LastHeadSeenAt *time.Time `json:"last_head_seen_at,omitempty"`
	// NativeChild identifies independently admitted native Codex children, including unresolved links.
	NativeChild         bool   `json:"native_child,omitempty"`
	NativeRootSessionID string `json:"native_root_session_id,omitempty"`
	NativeSourceHome    string `json:"native_source_home,omitempty"`
	// NativeLinkVersion records bounded native relationship/legacy-link reconciliation.
	NativeLinkVersion     int       `json:"native_link_version,omitempty"`
	ParentSessionID       string    `json:"parent_session_id,omitempty"`
	ParentNativeSessionID string    `json:"parent_native_session_id,omitempty"`
	SubagentID            string    `json:"subagent_id,omitempty"`
	SubagentObservedAt    time.Time `json:"subagent_observed_at,omitempty"`
	// AdmittedAt is when this machine took ownership of the session: the
	// boundary for project activation and storage destination. Hooks set it at
	// registration and backfill sets it to the import time. Empty on older
	// registrations.
	AdmittedAt time.Time `json:"admitted_at,omitempty"`
	// Origin is how the session entered the archive. Set once.
	Origin SessionOrigin `json:"origin,omitempty"`
	// HookObservedAt records actual lifecycle hook execution independently of origin.
	HookObservedAt time.Time `json:"hook_observed_at,omitzero"`
	// DiscoveryRoot confines reopening a discovered source to its approved root.
	DiscoveryRoot       string `json:"discovery_root,omitempty"`
	DiscoveryCwd        string `json:"discovery_cwd,omitempty"`
	DiscoveryGeneration string `json:"discovery_generation,omitempty"`
	// DiscoveryProducerOriginator and DiscoveryProducerSource retain admitted
	// producer facts for publication revalidation. They remain private state.
	DiscoveryProducerOriginator string `json:"discovery_producer_originator,omitempty"`
	DiscoveryProducerSource     string `json:"discovery_producer_source,omitempty"`
	// DiscoverySourcePriority is an adapter scheduling hint for source preference.
	DiscoverySourcePriority int `json:"discovery_source_priority,omitempty"`
	// StartedAtSource says where SessionStartedAt came from.
	StartedAtSource StartedAtSource `json:"started_at_source,omitempty"`
	// ImportBatch is the backfill run that registered the session. Whether
	// a registration belongs to an import is InBatch's to say.
	ImportBatch ImportBatch `json:"import_batch,omitzero"`
	// StagedImportSHA256 selects durably filtered pre-admission evidence.
	StagedImportSHA256 string `json:"staged_import_sha256,omitempty"`
	// DestinationID is config.DestinationID of the storage destination the
	// session was admitted into. Set once, at registration, by hooks and
	// backfill; a subagent copies its parent's. Empty on older
	// registrations, which fall back to comparing Admitted() with the
	// destination's start.
	DestinationID string `json:"destination_id,omitempty"`
	// Replay marks a session a replay tool ran: ReplayEnv was set in the
	// hook that registered it. Set once, at registration; a subagent copies
	// its parent's. Nil for an ordinary session.
	Replay *Replay `json:"replay,omitempty"`
	// SourceKind is where the collector reads the session from, fixed at
	// registration: a transcript file ("") or a Cursor database chat.
	SourceKind SourceKind `json:"source_kind,omitempty"`
	// SourceKey names the session within its source: for
	// SourceKindCursorSQLite, the chat's composer ID.
	SourceKey string `json:"source_key,omitempty"`
}

// Admitted is the boundary time. Registrations older than AdmittedAt were
// all hook-registered, so their start is their admission.
func (r SessionRegistration) Admitted() time.Time {
	if !r.AdmittedAt.IsZero() {
		return r.AdmittedAt
	}
	return r.SessionStartedAt
}

// Imported reports whether backfill, not a hook, registered the session.
func (r SessionRegistration) Imported() bool {
	return r.Origin == SessionOriginImport
}

// IsChild reports child ownership independently of whether the parent link resolved.
func (r SessionRegistration) IsChild() bool { return r.NativeChild || r.ParentSessionID != "" }

// Validate reports the first problem that makes the registration unusable:
// a missing session ID, project, harness name, start time or incompatible source.
func (r SessionRegistration) Validate() error {
	if err := r.validateNativeChild(); err != nil {
		return err
	}

	if r.CodexBinding != nil {
		if r.Harness.Name != "codex" || r.CodexBinding.NativeThreadID != r.NativeSessionID {
			return errors.New("codex binding requires matching codex registration")
		}
		if err := r.CodexBinding.Validate(); err != nil {
			return err
		}
	}
	if r.CodexCandidatePath != "" && (r.Harness.Name != "codex" || !filepath.IsAbs(r.CodexCandidatePath) || len(r.CodexCandidatePath) > 4096) {
		return errors.New("invalid Codex candidate locator")
	}
	if r.PreviousGenerationID == r.ArchiveSessionID && r.PreviousGenerationID != "" {
		return errors.New("generation cannot precede itself")
	}
	if r.CodexAdmission != nil && (r.Harness.Name != "codex" || r.Imported() || (r.Origin != SessionOriginHook && r.Origin != SessionOriginDiscovery) || r.CodexAdmission.Generation == "" || r.CodexAdmission.Revision == "" || !filepath.IsAbs(r.CodexAdmission.Cwd) || r.ProjectID != ProjectID(r.ProjectRoot) || !filepath.IsAbs(r.ProjectRoot)) {
		return errors.New("invalid Codex admission proof")
	}
	if strings.TrimSpace(r.ArchiveSessionID) == "" || strings.TrimSpace(r.NativeSessionID) == "" {
		return errors.New("archive and native session IDs are required")
	}
	if err := r.validateProjectOwnership(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Harness.Name) == "" {
		return errors.New("harness name is required")
	}
	if r.SessionStartedAt.IsZero() {
		return errors.New("session start time is required; older sessions cannot be inferred safely")
	}
	return r.validateSource()
}

// CaptureGap tells readers why source coverage is incomplete without including
// an omitted value or transcript content.
type CaptureGap struct {
	Code   string `json:"code"`
	Record int    `json:"record,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// CaptureBoundary describes retained evidence represented by a source snapshot.
// Excluded native records deliberately do not affect it or a source hash.
type CaptureBoundary struct {
	RetainedRecords int `json:"retained_records"`
	RetainedBytes   int `json:"retained_bytes"`
}

// NativeSessionIdentity is sanitized retained identity evidence supplied by an
// integration. ID is authoritative when set; Candidates preserve retained order.
// These observations are transient and are never persisted in source bundles.
type NativeSessionIdentity struct {
	ID         string
	Candidates []string
}

// FilteredTranscript is the only adapter output accepted by NewSourceBundle.
// Records retain their allowed native JSON shape and source ordering.
type FilteredTranscript struct {
	History  *SourceHistory `json:"history,omitempty"`
	Ordinals []uint64       `json:"-"`
	// LocalIdentity is safe retained identity evidence, never raw ownership IDs.
	LocalIdentity NativeSessionIdentity `json:"-"`
	// ObservedHarness is sanitized retained version/mode evidence for source assembly.
	ObservedHarness     Harness         `json:"-"`
	Format              string          `json:"format"`
	Records             [][]byte        `json:"-"`
	Boundary            CaptureBoundary `json:"boundary"`
	Gaps                []CaptureGap    `json:"gaps,omitempty"`
	FirstEventAt        time.Time       `json:"-"`
	Text                []string        `json:"-"`
	SessionIDs          []string        `json:"-"`
	AgentIDs            []string        `json:"-"`
	NativeStartAt       time.Time       `json:"-"`
	NativeEndAt         time.Time       `json:"-"`
	NativeStartComplete bool            `json:"-"`
}

// SupplementalEvidence is hook or verified native metadata evidence. Payload must already be
// privacy-filtered by the producer; this package preserves it separately from
// native records and does not reconcile it into a transcript.
type SupplementalEvidence struct {
	Kind       SupplementalEvidenceKind `json:"kind"`
	ObservedAt time.Time                `json:"observed_at"`
	Provenance string                   `json:"provenance"`
	Payload    map[string]any           `json:"payload"`
}

// SourceCapture describes the provenance shared by all records in a snapshot.
type SourceCapture struct {
	// Origin is emitted for discovery sources without inventing hook evidence.
	Origin         SessionOrigin   `json:"origin,omitempty"`
	Harness        Harness         `json:"harness"`
	AdapterName    string          `json:"adapter_name"`
	AdapterVersion string          `json:"adapter_version"`
	SourceFormat   string          `json:"source_format"`
	Boundary       CaptureBoundary `json:"boundary"`
	FilterVersion  string          `json:"filter_version"`
	CapturedAt     time.Time       `json:"captured_at"`
	Gaps           []CaptureGap    `json:"gaps,omitempty"`
}

// SourceBundle is the durable filtered source envelope. It intentionally has
// no normalized transcript or local filesystem path.
type SourceBundle struct {
	History              *SourceHistory           `json:"history,omitempty"`
	Ordinals             []uint64                 `json:"ordinals,omitempty"`
	SchemaVersion        int                      `json:"schema_version"`
	ArchiveSessionID     string                   `json:"archive_session_id"`
	NativeSessionID      string                   `json:"native_session_id"`
	ProjectID            string                   `json:"project_id"`
	Capture              SourceCapture            `json:"capture"`
	NativeRecords        []map[string]any         `json:"native_records"`
	NativeText           []TextTranscript         `json:"native_text,omitempty"`
	SupplementalEvidence []SupplementalEvidence   `json:"supplemental_evidence,omitempty"`
	PreviousGenerationID string                   `json:"previous_generation_id,omitempty"`
	NativeChild          bool                     `json:"native_child,omitempty"`
	ParentSessionID      string                   `json:"parent_session_id,omitempty"`
	LinkedSessions       []LinkedSessionReference `json:"linked_sessions,omitempty"`
}

// TextTranscript preserves a safely filtered native text transcript without
// inventing JSONL records or universal message events.
type TextTranscript struct {
	Format  string `json:"format"`
	Content string `json:"content"`
}

// SourceReference is the content-addressed pointer carried by metadata.
type SourceReference struct {
	Key             string `json:"key"`
	SHA256          string `json:"sha256"`
	CompressedBytes int    `json:"compressed_bytes"`
}

// AdapterInfo records which code filtered and summarized the source.
type AdapterInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ParserInfo identifies a metadata derivation. Status is partial, failed, or
// complete only when the particular adapter has demonstrated complete coverage.
type ParserInfo struct {
	Name    string       `json:"name"`
	Version string       `json:"version"`
	Status  ParserStatus `json:"status"`
}

// SemanticConventionsInfo identifies the exact upstream vocabulary revision
// used for matching attribute names. Local extensions use agent_archive.*.
type SemanticConventionsInfo struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
}

// Counts intentionally uses pointers: nil means unavailable, rather than an
// invented zero after a partial or failed parse.
//
// Turns counts human prompts only: a user record which carries text or any
// non-tool-result content and is not harness-written (an isMeta record, local
// command output, a `!` shell command, or a slash command no assistant
// answered). Messages counts those prompts plus distinct assistant messages:
// Claude Code streams one response as several records sharing message.id,
// which count once; a record without an id counts on its own. Assistant
// records whose only content is a tool call are included.
// ToolResults counts observed tool results, which are the records a harness
// writes in the user role but no human sent. The token counts are summed from
// whatever the harness exposed (Claude `message.usage`, Codex
// `turn_token_usage`) and stay nil when it exposed nothing; usage repeated
// across the streamed records of one Claude `message.id` is counted once.
type Counts struct {
	Turns       *int `json:"turns,omitempty"`
	Messages    *int `json:"messages,omitempty"`
	ToolCalls   *int `json:"tool_calls,omitempty"`
	ToolResults *int `json:"tool_results,omitempty"`
	// UserShellCommands counts shell commands the person ran directly (Claude
	// Code's `!` commands, recorded as <bash-input>). They are neither prompts
	// nor model tool calls.
	UserShellCommands *int `json:"user_shell_commands,omitempty"`
	// Compactions counts how many times Claude Code compacted the session
	// (/compact or auto-compaction), from its compact_boundary records, or
	// from its compaction summaries when no boundary was retained. It is
	// known only for Claude Code bundles from filter 5 on.
	Compactions      *int `json:"compactions,omitempty"`
	ExplicitFeedback *int `json:"explicit_feedback,omitempty"`
	// FilesTouched counts the distinct files the session's editing calls
	// named: the same files handoff lists (see sessionFilesTouched). A path
	// under the workspace root and the same path relative to it count once.
	// It is known wherever tool_calls is, from parser 0.13.0 on. Only the
	// count is published, never a path.
	FilesTouched *int `json:"files_touched,omitempty"`
	// ToolErrors counts the tool results the harness marked as errors
	// (is_error true), out of ToolResults. It is known from parser 0.14.0,
	// where tool_results is, for the harnesses that write the flag (Claude
	// Code and Cursor). Codex writes no such flag, so its count is unknown,
	// not zero.
	ToolErrors *int `json:"tool_errors,omitempty"`
	// The token counts keep each harness's own meaning. Claude Code's
	// InputTokens excludes what was read from or written to the prompt cache;
	// Codex's InputTokens includes its cached input and its cache-write input,
	// which CacheReadTokens and CacheWriteTokens repeat. ReasoningTokens (from parser 0.14.0: Claude Code's
	// output_tokens_details.thinking_tokens, Codex's reasoning_output_tokens)
	// is the part of OutputTokens spent on reasoning, not an addition to it.
	InputTokens      *int `json:"input_tokens,omitempty"`
	OutputTokens     *int `json:"output_tokens,omitempty"`
	CacheReadTokens  *int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  *int `json:"reasoning_tokens,omitempty"`
	// Commits, Pushes, PRsCreated, and PRsMerged count the git and pull
	// request work the session's own tool calls confirmed (see
	// Metadata.GitActivity), uncapped. They are known from parser 0.15.0
	// wherever tool_calls is.
	Commits    *int `json:"commits,omitempty"`
	Pushes     *int `json:"pushes,omitempty"`
	PRsCreated *int `json:"prs_created,omitempty"`
	PRsMerged  *int `json:"prs_merged,omitempty"`
}

// UnknownModel is the ModelTokens.Model of token accounting no model name
// could be attached to: a record carrying usage that names no model, and
// follows no Codex turn_context that does. (A placeholder model such as
// Claude Code's "<synthetic>" produced no usage, and its records are not
// counted at all.)
const UnknownModel = "unknown"

// MaxModelTokens is the most entries Metadata.ModelTokens holds, and
// OtherModels is the ModelTokens.Model that the models beyond it are added
// together under, so the split still sums to the session's counts. The schema's
// model_tokens maxItems matches MaxModelTokens. A session names a handful of
// models, so only a hostile transcript reaches it.
const (
	MaxModelTokens = 32
	OtherModels    = "other"
)

// maxModelNameRunes is the longest ModelTokens.Model, ellipsis included; the
// schema's maxLength matches it.
const maxModelNameRunes = 128

// ModelTokens is the token accounting attributed to one model, with the
// meaning Counts gives each field. Model is the model id a ModelSummary
// carries (gen_ai.request.model, or gen_ai.response.model when a record names
// only that), cut to maxModelNameRunes, or UnknownModel when a record
// carrying usage names no model. Beyond MaxModelTokens models the
// least-used are added together under OtherModels. A field is nil when no
// record of the model reported it. Below 2^53 tokens, each Counts token field
// is the sum of that field over Metadata.ModelTokens.
type ModelTokens struct {
	Model            string `json:"model"`
	InputTokens      *int   `json:"input_tokens,omitempty"`
	OutputTokens     *int   `json:"output_tokens,omitempty"`
	CacheReadTokens  *int   `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int   `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  *int   `json:"reasoning_tokens,omitempty"`
}

// ModelSummary is one model a session used, as OpenTelemetry GenAI attributes
// (gen_ai.request.model or gen_ai.response.model, and gen_ai.provider.name and
// agent_archive.request.reasoning_level when known). TurnCount is how many
// transcript turns were attributed to it, and nil for a hook-reported model.
type ModelSummary struct {
	Attributes          map[string]string   `json:"attributes"`
	Source              ModelSummarySource  `json:"source"`
	ResponseModelStatus ResponseModelStatus `json:"response_model_status"`
	TurnCount           *int                `json:"turn_count,omitempty"`
}

// SkillSnapshot is a skill reported available to a session, with the SHA-256
// of its content when known and why it counts as available.
type SkillSnapshot struct {
	Name     string        `json:"name"`
	SHA256   string        `json:"sha256,omitempty"`
	Coverage SkillCoverage `json:"coverage,omitempty"`
}

// SkillUse is a skill the session used and the evidence for that use.
type SkillUse struct {
	Name      string           `json:"name"`
	SHA256    string           `json:"sha256,omitempty"`
	TurnCount *int             `json:"turn_count,omitempty"`
	Evidence  SkillUseEvidence `json:"evidence"`
}

// ToolUsage is one tool name and how many calls the session made to it. See
// Metadata.ToolsUsed.
type ToolUsage struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Metadata is the replaceable, source-first reader index. It contains no
// tool payloads and no full transcript. Title is an optional short preview of
// the first filtered human prompt, derived for browsing.
type Metadata struct {
	History              *RevisionHistory `json:"history,omitempty"`
	SchemaVersion        int              `json:"schema_version"`
	SessionID            string           `json:"session_id"`
	PreviousGenerationID string           `json:"previous_generation_id,omitempty"`
	NativeSessionID      string           `json:"native_session_id"`
	MachineID            string           `json:"machine_id"`
	ProjectID            string           `json:"project_id"`
	// Title is a one-line, truncated preview of the first human prompt after
	// filtering. Omitted when no prompt text was available.
	Title string `json:"title,omitempty"`
	// Name is the title the harness gave the session (Claude Code's custom
	// title, the last one if it was renamed; a Cursor chat's name), collapsed
	// to one line like Title. Omitted when there is none.
	Name string `json:"name,omitempty"`
	// Branch is the last git branch the transcript recorded. Omitted when
	// none was recorded or it is HEAD.
	Branch string `json:"branch,omitempty"`
	// PullRequests are the pull requests the session was linked to, in the
	// order first linked, at most MaxPullRequests. Omitted when none.
	PullRequests []PullRequestLink `json:"pull_requests,omitempty"`
	// ProjectName is the basename of the session's project root at publish
	// time, so list can label the project without local config. Omitted when
	// unknown.
	ProjectName string `json:"project_name,omitempty"`
	// RepoKey identifies the git repository the session ran in, independent
	// of where it is checked out: a hash of the normalized origin URL (see
	// RepoKey), never the URL. Omitted when the project had no portable
	// origin remote.
	RepoKey string `json:"repo_key,omitempty"`
	// GitHead is the commit the session's working directory had checked out
	// when it started and when a stop hook last looked, as its hooks
	// recorded them (see ApplyGitHead). Omitted when no hook could tell.
	GitHead   *SessionGitHead `json:"git_head,omitempty"`
	StartedAt time.Time       `json:"started_at"`
	// EndedAt is the latest timestamp any retained record of the session
	// carries, never earlier than StartedAt. Omitted when no record carries
	// a timestamp (a Cursor transcript) or the source could not be parsed.
	EndedAt             *time.Time               `json:"ended_at,omitempty"`
	CapturedAt          time.Time                `json:"captured_at"`
	MetadataDerivedAt   time.Time                `json:"metadata_derived_at"`
	Harness             Harness                  `json:"harness"`
	Adapter             AdapterInfo              `json:"adapter"`
	Parser              ParserInfo               `json:"parser"`
	FilterVersion       string                   `json:"filter_version"`
	State               MetadataState            `json:"state"`
	TurnOutcome         TurnOutcome              `json:"turn_outcome,omitempty"`
	SemanticConventions *SemanticConventionsInfo `json:"semantic_conventions,omitempty"`
	Models              []ModelSummary           `json:"models,omitempty"`
	SkillsAvailable     []SkillSnapshot          `json:"skills_available,omitempty"`
	SkillsUsed          []SkillUse               `json:"skills_used,omitempty"`
	SkillDetection      SkillDetection           `json:"skill_detection"`
	Counts              Counts                   `json:"counts"`
	// ToolsUsed is the session's most-called tools by call count (descending,
	// then name ascending), at most MaxToolsUsed of them. Names are the
	// harness's own, MCP tools included, redacted and length-bounded (see
	// metadataToolName). Omitted when no named tool call was observed or the
	// counts are unknown; counts.tool_calls tells the two apart.
	ToolsUsed []ToolUsage `json:"tools_used,omitempty"`
	// ModelTokens splits the session's token counts by model, sorted by
	// model id. Omitted when the harness exposed no token accounting.
	ModelTokens []ModelTokens `json:"model_tokens,omitempty"`
	// MCPCalls counts the session's MCP tool calls by server (the server in
	// an mcp__<server>__<tool> name), by count descending and then name
	// ascending, at most MaxMCPCalls of them. Omitted when no MCP call was
	// observed or the counts are unknown.
	MCPCalls []ToolUsage `json:"mcp_calls,omitempty"`
	// GitActivity lists, in transcript order, the commits, pushes, and pull
	// requests created or merged that the session's tool calls confirmed,
	// at most MaxGitActivity of them (the counts are exact). Omitted when
	// none was observed or the counts are unknown.
	GitActivity     []GitEvent               `json:"git_activity,omitempty"`
	CaptureGaps     []CaptureGap             `json:"capture_gaps,omitempty"`
	SourceBundle    SourceReference          `json:"source_bundle"`
	NativeChild     bool                     `json:"native_child,omitempty"`
	ParentSessionID string                   `json:"parent_session_id,omitempty"`
	LinkedSessions  []LinkedSessionReference `json:"linked_sessions,omitempty"`
	// Origin, ImportedAt, and StartedAtSource describe a session backfill
	// imported. All three are omitted for hook-captured sessions, so their
	// metadata is unchanged. See ApplyRegistrationProvenance.
	Origin          SessionOrigin   `json:"origin,omitempty"`
	ImportedAt      *time.Time      `json:"imported_at,omitempty"`
	StartedAtSource StartedAtSource `json:"started_at_source,omitempty"`
	// Replay marks a session a replay tool ran (see ApplyReplay). Omitted
	// for an ordinary session.
	Replay *Replay `json:"replay,omitempty"`
}

// CaptureGapDiscoveredWithoutHookEvidence marks admitted background capture
// without hook lifecycle events or a contemporaneous skill inventory.
const CaptureGapDiscoveredWithoutHookEvidence = "discovered_without_hook_evidence"

// CaptureGapImportedWithoutHookEvidence marks a historical import without
// contemporaneous hook lifecycle events or a skill inventory.
const CaptureGapImportedWithoutHookEvidence = "imported_without_hook_evidence"

// ApplyRegistrationProvenance records how the session entered the archive.
// It changes nothing for a hook registration. Callers apply it to every
// metadata document BuildMetadata returns, including a failed parse's.
func (m *Metadata) ApplyRegistrationProvenance(r SessionRegistration) {
	m.PreviousGenerationID = r.PreviousGenerationID
	if !r.Imported() && r.Origin != SessionOriginDiscovery {
		return
	}
	m.Origin = r.Origin
	if r.Imported() && !r.AdmittedAt.IsZero() {
		importedAt := r.AdmittedAt.UTC()
		m.ImportedAt = &importedAt
	}
	m.StartedAtSource = r.StartedAtSource
	if r.Origin == SessionOriginDiscovery {
		for _, gap := range m.CaptureGaps {
			if gap.Code == CaptureGapDiscoveredWithoutHookEvidence {
				return
			}
		}
		m.CaptureGaps = append(m.CaptureGaps, CaptureGap{Code: CaptureGapDiscoveredWithoutHookEvidence, Detail: "Discovered locally; hooks did not establish complete lifecycle coverage"})
		return
	}
	for _, gap := range m.CaptureGaps {
		if gap.Code == CaptureGapImportedWithoutHookEvidence {
			return
		}
	}
	m.CaptureGaps = append(m.CaptureGaps, CaptureGap{Code: CaptureGapImportedWithoutHookEvidence, Detail: importedGapDetail})
}

// importedGapDetail is about the time before the import only: a hook that
// resumes an imported session records its lifecycle from then on.
const importedGapDetail = "No hook observed this session before it was imported (imported_at): activity before then has no hook lifecycle events, final-response text, or skill inventory."

func (r SessionRegistration) validateNativeChild() error {
	if !r.NativeChild {
		return nil
	}
	if r.Harness.Name != "codex" || r.NativeSourceHome != "" && !filepath.IsAbs(r.NativeSourceHome) {
		return errors.New("invalid native child ownership")
	}
	return nil
}

// IsChild reports source child ownership even when the archive parent is unresolved.
func (b SourceBundle) IsChild() bool { return b.NativeChild || b.ParentSessionID != "" }

// IsChild reports metadata child ownership independently of parent-link availability.
func (m *Metadata) IsChild() bool { return m.NativeChild || m.ParentSessionID != "" }
