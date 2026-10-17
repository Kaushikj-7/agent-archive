package agentapi

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"time"
)

// HookInput carries local native JSON only during decoding.
type HookInput struct {
	Environment map[string]string
	Payload     map[string]any
	ObservedAt  time.Time
}

// NativeHookContract declares a closed, bounded environment observation contract.
// Environment values are read once and remain read-only for every decode path.
type NativeHookContract struct {
	Contract                 string
	EnvironmentKeys          []string
	MaxEnvironmentValueBytes int
}

// NativeHookContractProvider optionally declares hook observation inputs.
type NativeHookContractProvider interface {
	NativeHookContract() NativeHookContract
}

// NativeSession contains exact native identity and observed native version facts.
type NativeSession struct {
	Agent    agentmeta.ID
	NativeID string
	Version  string
	Mode     NativeMode
}

// EventKind is an archive-relevant lifecycle action, independent of native names.
type EventKind uint8

// The following values define the supported typed observations.
const (
	EventStart EventKind = iota + 1
	EventTurnStart
	EventResponse
	EventStop
	EventSubagent
)

// Freshness is native evidence interpreted by shared admission policy.
type Freshness uint8

// The following values define the supported typed observations.
const (
	FreshUnknown Freshness = iota
	FreshExplicit
	FreshContinuation
	FreshStat
)

// NativeMode retains the exact observed native mode, including future values.
type NativeMode string

// NativeModeUnspecified is the absence of a native mode observation.
const NativeModeUnspecified NativeMode = ""

// FreshnessReason is a fixed explanation of native freshness evidence.
type FreshnessReason string

// These reason codes describe proof without retaining raw native source values.
const (
	FreshnessEmptyPath     FreshnessReason = "empty_native_path"
	FreshnessInvalidPath   FreshnessReason = "invalid_native_path"
	FreshnessInspectPath   FreshnessReason = "inspect_native_path"
	FreshnessExplicitStart FreshnessReason = "explicit_start"
	FreshnessContinuation  FreshnessReason = "explicit_continuation"
	FreshnessUnknownSource FreshnessReason = "unknown_native_source"
	FreshnessRetainedProof FreshnessReason = "retained_hook_proof"
)

// StartEvidence requests at most one bounded stat and never a source read.
type StartEvidence struct {
	Kind   Freshness
	Reason FreshnessReason
	Path   string
}

// LocatorUpdate declares a validated file locator's permitted update semantics.
type LocatorUpdate uint8

// The following values define the supported typed observations.
const (
	LocatorNone LocatorUpdate = iota
	LocatorReplaceFile
	LocatorFillFile
)

// DeferredKind limits which absent-parent effects may be retained for replay.
type DeferredKind uint8

// The following values define the supported typed observations.
const (
	DeferredNone DeferredKind = iota
	DeferredStart
	DeferredFollowup
)

// ChildObservation contains native child facts, without archive admission policy.
type ChildObservation struct {
	ID                string
	Path              string
	Type              string
	MissingDetail     string
	CaptureTranscript bool
}

// LifecycleEvent is a typed local result. Raw native payloads are never retained.
type LifecycleEvent struct {
	NativeHookContract string                         `json:",omitempty"`
	CodexProjectRoot   string                         `json:"CodexProjectRoot"`
	CodexCwd           string                         `json:"CodexCwd"`
	CodexPolicyToken   string                         `json:"CodexPolicyToken"`
	Kind               EventKind                      `json:"Kind"`
	Session            NativeSession                  `json:"Session"`
	ProjectRoot        string                         `json:"ProjectRoot"`
	Source             SourceRef                      `json:"Source"`
	Locator            LocatorUpdate                  `json:"Locator"`
	Start              StartEvidence                  `json:"Start"`
	NewOnly            bool                           `json:"NewOnly"`
	Deferred           DeferredKind                   `json:"Deferred"`
	Reason             string                         `json:"Reason"`
	NativeEvent        string                         `json:"NativeEvent"`
	Evidence           []archive.SupplementalEvidence `json:"Evidence"`
	Child              *ChildObservation              `json:"Child"`
}

// HookDecoder interprets native JSON; it performs no host operations.
type HookDecoder interface {
	Decode(context.Context, HookInput) ([]LifecycleEvent, error)
}

// HookDiagnosticDecoder supplies only a project root for panic diagnostics.
// It is pure, optional, and conveys no admission or lifecycle authority.
type HookDiagnosticDecoder interface {
	DiagnosticProject(HookInput) string
}

// DecodersLookup is the narrow port consumed by capture and replay.
type DecodersLookup interface {
	LookupDecoder(string) (HookDecoder, bool)
}

// LegacyAdmission retains the exact previous private intent wire fields.
// Only native decoders interpret their meaning; no raw hook content is present.
type LegacyAdmission struct {
	Harness         string    `json:"harness"`
	Event           string    `json:"event"`
	NativeSessionID string    `json:"native_session_id"`
	ProjectRoot     string    `json:"project_root"`
	DestinationID   string    `json:"destination_id"`
	PauseGeneration string    `json:"pause_generation,omitempty"`
	TranscriptPath  string    `json:"transcript_path,omitempty"`
	CursorVersion   string    `json:"cursor_version,omitempty"`
	ComposerMode    string    `json:"composer_mode,omitempty"`
	ObservedAt      time.Time `json:"observed_at"`
}

// LegacyHookDecoder translates old private records without rechecking fresh proof.
type LegacyHookDecoder interface {
	DecodeLegacy(AdmissionIntent) ([]LifecycleEvent, error)
}

// ReplayEffect is a content-free, versioned effect with original proof and identity.
type ReplayEffect struct {
	Event LifecycleEvent `json:"Event"`
}

// NativeIntentMode selects a closed native admission operation.
type NativeIntentMode string

const (
	// NativeStartupCandidate holds observation-only root startup work.
	NativeStartupCandidate NativeIntentMode = "startup_candidate"
	// NativeOwnerContinuation requires an exact validated existing owner.
	NativeOwnerContinuation NativeIntentMode = "owner_continuation"
)

// AdmissionIntent is the private replay wire envelope, retaining legacy fields.
type AdmissionIntent struct {
	NativeWindowGeneration string                   `json:"native_window_generation,omitempty"`
	NativeMode             NativeIntentMode         `json:"native_mode,omitempty"`
	NativeAdmission        *archive.NativeAdmission `json:"native_admission,omitempty"`
	NativeOwnerID          string                   `json:"native_owner_id,omitempty"`
	ObservedCwd            string                   `json:"observed_cwd,omitempty"`
	CodexPolicyToken       string                   `json:"codex_policy_token,omitempty"`
	Harness                string                   `json:"harness"`
	Event                  string                   `json:"event"`
	NativeSessionID        string                   `json:"native_session_id"`
	ProjectRoot            string                   `json:"project_root"`
	DestinationID          string                   `json:"destination_id"`
	PauseGeneration        string                   `json:"pause_generation,omitempty"`
	TranscriptPath         string                   `json:"transcript_path,omitempty"`
	CursorVersion          string                   `json:"cursor_version,omitempty"`
	ComposerMode           string                   `json:"composer_mode,omitempty"`
	ObservedAt             time.Time                `json:"observed_at"`
	Version                int                      `json:"version,omitempty"`
	Effects                []ReplayEffect           `json:"effects,omitempty"`
	// Replay is the hook process marker, retained for queued new admissions.
	Replay *archive.Replay `json:"replay,omitempty"`
	// LastHead is the commit the contended hook saw for a stop it queued.
	LastHead *archive.GitHead `json:"last_head,omitempty"`
}
