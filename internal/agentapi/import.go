package agentapi

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/archive"
	"time"
)

// ImportStartPolicy declares evidence needed before filtering historical input.
type ImportStartPolicy uint8

// ImportNativeStart requires a native start observation; ImportFileCreatedStart permits the caller to use file creation evidence.
const (
	ImportNativeStart ImportStartPolicy = iota
	ImportFileCreatedStart
)

// ImportPolicy declares native historical filtering requirements, not admission.
type ImportPolicy struct{ Start ImportStartPolicy }

// ImportInspectionRequest supplies retained filter observations and compact
// discovery evidence. Native identity rules remain separate from opaque keys.
type ImportInspectionRequest struct {
	Session  NativeSession
	Source   SourceRef
	Header   NativeHeader
	Filtered archive.FilteredTranscript
}

// ImportInspection contains native observations. Shared core chooses refusal,
// date/project filters, duplicate conflicts, registration and publication.
type ImportInspection struct {
	Conversation     bool
	IdentityMismatch bool
	StartedAt        time.Time
}

// ImportInspector owns native conversation, identity and start interpretation.
type ImportInspector interface {
	ImportPolicy(SourceRef) ImportPolicy
	InspectImport(context.Context, ImportInspectionRequest) (ImportInspection, error)
}

// ImportsLookup projects native historical observation owners to backfill.
type ImportsLookup interface {
	LookupImport(string) (ImportInspector, bool)
}

// ImportSummary contains bounded intrinsic observations from complete filtering.
// It neither grants native admission nor supplies current header preference.
type ImportSummary struct {
	Conversation    bool      `json:"Conversation"`
	NativeStartAt   time.Time `json:"NativeStartAt"`
	RetainedBytes   int64     `json:"RetainedBytes"`
	RetainedRecords int       `json:"RetainedRecords"`
}

// StreamingImportFilter optionally inspects safe rows without retaining them.
// Both attestation and input support must hold on the actual injected filter.
type StreamingImportFilter interface {
	InspectionFor(archive.Adapter) bool
	SupportsImportInspection(NativeInput) bool
	InspectFilteredImport(context.Context, NativeInput, FilterContext) (ImportSummary, error)
}

// SummaryImportInspector interprets compact facts under its native policy.
// Membership-sensitive inspectors must decline unsupported intrinsic summaries.
type SummaryImportInspector interface {
	SummaryFor(ImportInspector) bool
	SupportsImportSummary() bool
	InspectImportSummary(context.Context, ImportInspectionRequest, ImportSummary) (ImportInspection, error)
}
