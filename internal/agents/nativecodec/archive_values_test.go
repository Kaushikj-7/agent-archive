package nativecodec

import (
	"context"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"io"
	"strings"
	"time"
)

type Adapter interface {
	archive.Adapter
	FilterJSONL(io.Reader) (archive.FilteredTranscript, error)
}

type FilterError = archive.FilterError

var ErrUnsafeSourceFormat = archive.ErrUnsafeSourceFormat

var ErrRecordTooLarge = archive.ErrRecordTooLarge

const MaxRecordBytes = archive.MaxRecordBytes

const DefaultParserVersion = archive.DefaultParserVersion

type PrivacyState = archive.PrivacyState

type Analysis = archive.Analysis

type AvailabilityState = archive.AvailabilityState

const AvailabilityAvailable = archive.AvailabilityAvailable

const AvailabilityUnavailable = archive.AvailabilityUnavailable

const AvailabilityUnknown = archive.AvailabilityUnknown

type AvailabilityReason = archive.AvailabilityReason

const AvailabilityReasonText = archive.AvailabilityReasonText

const AvailabilityReasonNotRecorded = archive.AvailabilityReasonNotRecorded

const AvailabilityReasonHistoricalFilter = archive.AvailabilityReasonHistoricalFilter

const AvailabilityReasonParseFailed = archive.AvailabilityReasonParseFailed

type Availability = archive.Availability

type Observability = archive.Observability

type NativeFacts = archive.NativeFacts

type NativeTurnEnd = archive.NativeTurnEnd

var ReconcileHookFinals = archive.ReconcileHookFinals

var CollapseSessionTitle = archive.CollapseSessionTitle

var ValidBranch = archive.ValidBranch

var SkillNameFromPath = archive.SkillNameFromPath

var ValidateSourceBundle = archive.ValidateSourceBundle

var ResolveSlashCommands = archive.ResolveSlashCommands

type ToolCallCandidate = archive.ToolCallCandidate

var FinalizeToolCalls = archive.FinalizeToolCalls

type TokenAccumulator = archive.TokenAccumulator

type CompressedSource = archive.CompressedSource

var NewSourceBundle = archive.NewSourceBundle

var NewLinkedSessionEvidence = archive.NewLinkedSessionEvidence

var FilterSupplementalEvidence = archive.FilterSupplementalEvidence

var AnnotateSupplementalGaps = archive.AnnotateSupplementalGaps

var MergeSupplementalEvidence = archive.MergeSupplementalEvidence

var SupplementalEvidenceEqual = archive.SupplementalEvidenceEqual

var BuildCompressedSource = archive.BuildCompressedSource

var SourceObjectKey = archive.SourceObjectKey

var ProjectID = archive.ProjectID

var MetadataObjectKey = archive.MetadataObjectKey

var CaptureGapCodes = archive.CaptureGapCodes

type GitEventKind = archive.GitEventKind

const GitEventCommit = archive.GitEventCommit

const GitEventPush = archive.GitEventPush

const GitEventPRCreated = archive.GitEventPRCreated

const GitEventPRMerged = archive.GitEventPRMerged

type GitEventSource = archive.GitEventSource

const GitEventSourceShell = archive.GitEventSourceShell

const GitEventSourceMCP = archive.GitEventSourceMCP

type GitEvent = archive.GitEvent

const MaxGitActivity = archive.MaxGitActivity

const HandoffVersion = archive.HandoffVersion

const DefaultHandoffMaxBytes = archive.DefaultHandoffMaxBytes

type HandoffOptions = archive.HandoffOptions

type HandoffCheckout = archive.HandoffCheckout

type Handoff = archive.Handoff

type HandoffSession = archive.HandoffSession

type HandoffWorkspace = archive.HandoffWorkspace

type HandoffPlanItem = archive.HandoffPlanItem

type HandoffExchange = archive.HandoffExchange

type HandoffStep = archive.HandoffStep

type HandoffStepKind = archive.HandoffStepKind

const HandoffStepText = archive.HandoffStepText

const HandoffStepTool = archive.HandoffStepTool

const HandoffStepShell = archive.HandoffStepShell

const HandoffStepSummary = archive.HandoffStepSummary

const HandoffStepCollapsed = archive.HandoffStepCollapsed

type HandoffToolCall = archive.HandoffToolCall

type HandoffGap = archive.HandoffGap

type HandoffElision = archive.HandoffElision

type HandoffElisionKind = archive.HandoffElisionKind

const HandoffElisionToolOutput = archive.HandoffElisionToolOutput

const HandoffElisionToolCalls = archive.HandoffElisionToolCalls

const HandoffElisionAssistantText = archive.HandoffElisionAssistantText

const HandoffElisionPromptText = archive.HandoffElisionPromptText

var TruncateUTF8 = archive.TruncateUTF8

var FitHandoff = archive.FitHandoff

var CutHandoffMarkdown = archive.CutHandoffMarkdown

var BuildHandoffWithAnalysis = archive.BuildHandoffWithAnalysis

type HandoffRenderOptions = archive.HandoffRenderOptions

var RenderHandoffMarkdown = archive.RenderHandoffMarkdown

var DisplayLine = archive.DisplayLine

var DisplayJSON = archive.DisplayJSON

const HarnessClaude = archive.HarnessClaude

const HarnessCodex = archive.HarnessCodex

const HarnessCursor = archive.HarnessCursor

var CanonicalHarness = archive.CanonicalHarness

var KnownHarness = archive.KnownHarness

type ImportBatch = archive.ImportBatch

var NewImportBatch = archive.NewImportBatch

const MaxToolsUsed = archive.MaxToolsUsed

const MaxMCPCalls = archive.MaxMCPCalls

var BuildMetadataWithAnalysis = archive.BuildMetadataWithAnalysis

type RecordPreview = archive.RecordPreview

type PreviewAccumulator = archive.PreviewAccumulator

var SanitizeValue = archive.SanitizeValue

var RedactSensitive = archive.RedactSensitive

var JSONHasDuplicateKeys = archive.JSONHasDuplicateKeys

const MaxSanitizeStringPasses = archive.MaxSanitizeStringPasses

var RedactText = archive.RedactText

var RepoKey = archive.RepoKey

var IsRepoKey = archive.IsRepoKey

var NormalizeRemoteURL = archive.NormalizeRemoteURL

const MaxPullRequests = archive.MaxPullRequests

type PullRequestLink = archive.PullRequestLink

type Labels = archive.Labels

var DisplayTitle = archive.DisplayTitle

var LatestPR = archive.LatestPR

var LabelsFromAnalysis = archive.LabelsFromAnalysis

const SourceLineHeader = archive.SourceLineHeader

const SourceLineNativeRecord = archive.SourceLineNativeRecord

const SourceLineNativeText = archive.SourceLineNativeText

const SourceLineSupplementalEvidence = archive.SourceLineSupplementalEvidence

type SourceLineKind = archive.SourceLineKind

const MaxSourceLineBytes = archive.MaxSourceLineBytes

type SourceCounts = archive.SourceCounts

type SourceHeader = archive.SourceHeader

type SourceLine = archive.SourceLine

var EncodeSource = archive.EncodeSource

type DecodeOptions = archive.DecodeOptions

var ErrSourceTooLarge = archive.ErrSourceTooLarge

var DecodeSource = archive.DecodeSource

var ReadSourceBundle = archive.ReadSourceBundle

type SourceKind = archive.SourceKind

const SourceKindFile = archive.SourceKindFile

const SourceKindCursorSQLite = archive.SourceKindCursorSQLite

const MaxSubagentTypeLength = archive.MaxSubagentTypeLength

var SanitizeSubagentType = archive.SanitizeSubagentType

var NewCaptureGapEvidence = archive.NewCaptureGapEvidence

type Transcript = archive.Transcript

type TranscriptExchangeKind = archive.TranscriptExchangeKind

const TranscriptExchangeLeading = archive.TranscriptExchangeLeading

const TranscriptExchangePrompt = archive.TranscriptExchangePrompt

const TranscriptExchangeNotification = archive.TranscriptExchangeNotification

type TranscriptExchange = archive.TranscriptExchange

type TranscriptStepKind = archive.TranscriptStepKind

const TranscriptStepText = archive.TranscriptStepText

const TranscriptStepTool = archive.TranscriptStepTool

const TranscriptStepShell = archive.TranscriptStepShell

const TranscriptStepCommand = archive.TranscriptStepCommand

const TranscriptStepSummary = archive.TranscriptStepSummary

const TranscriptStepOutput = archive.TranscriptStepOutput

const TranscriptStepCollapsed = archive.TranscriptStepCollapsed

type TranscriptStep = archive.TranscriptStep

var BuildTranscriptWithAnalysis = archive.BuildTranscriptWithAnalysis

var FitTranscript = archive.FitTranscript

type TranscriptElision = archive.TranscriptElision

type TranscriptElisionKind = archive.TranscriptElisionKind

const TranscriptElisionToolOutput = archive.TranscriptElisionToolOutput

const TranscriptElisionToolCalls = archive.TranscriptElisionToolCalls

const TranscriptElisionAssistantText = archive.TranscriptElisionAssistantText

const TranscriptElisionPromptText = archive.TranscriptElisionPromptText

const TranscriptElisionHookFinals = archive.TranscriptElisionHookFinals

const TranscriptElisionOldestHookFinals = archive.TranscriptElisionOldestHookFinals

const TranscriptElisionOldestExchanges = archive.TranscriptElisionOldestExchanges

const TranscriptElisionOldestSteps = archive.TranscriptElisionOldestSteps

var DescribeTranscriptElisions = archive.DescribeTranscriptElisions

const SourceSchemaVersion = archive.SourceSchemaVersion

const MetadataSchemaVersion = archive.MetadataSchemaVersion

const FilterVersion = archive.FilterVersion

const OpenTelemetryGenAIRevision = archive.OpenTelemetryGenAIRevision

type ParserStatus = archive.ParserStatus

const ParserStatusPartial = archive.ParserStatusPartial

const ParserStatusFailed = archive.ParserStatusFailed

const ParserStatusComplete = archive.ParserStatusComplete

type MetadataState = archive.MetadataState

const MetadataStateActive = archive.MetadataStateActive

const MetadataStateIdle = archive.MetadataStateIdle

const MetadataStateClosed = archive.MetadataStateClosed

const MetadataStateUnknown = archive.MetadataStateUnknown

type TurnOutcome = archive.TurnOutcome

const TurnOutcomeCompleted = archive.TurnOutcomeCompleted

const TurnOutcomeInterrupted = archive.TurnOutcomeInterrupted

const TurnOutcomeError = archive.TurnOutcomeError

const TurnOutcomeUnknown = archive.TurnOutcomeUnknown

type SkillDetection = archive.SkillDetection

const SkillDetectionUnavailable = archive.SkillDetectionUnavailable

const SkillDetectionObserved = archive.SkillDetectionObserved

const SkillDetectionObservedNone = archive.SkillDetectionObservedNone

const SkillDetectionPartial = archive.SkillDetectionPartial

type SkillCoverage = archive.SkillCoverage

const SkillCoverageEligible = archive.SkillCoverageEligible

const SkillCoverageDiscovered = archive.SkillCoverageDiscovered

const SkillCoverageInstalledOnly = archive.SkillCoverageInstalledOnly

type SkillUseEvidence = archive.SkillUseEvidence

const SkillUseEvidenceNativeInvocation = archive.SkillUseEvidenceNativeInvocation

const SkillUseEvidenceReadInference = archive.SkillUseEvidenceReadInference

type SupplementalEvidenceKind = archive.SupplementalEvidenceKind

const EvidenceKindSkillInventory = archive.EvidenceKindSkillInventory

const EvidenceKindSkillDiscovered = archive.EvidenceKindSkillDiscovered

const EvidenceKindSkillSnapshot = archive.EvidenceKindSkillSnapshot

const EvidenceKindSkillInvocation = archive.EvidenceKindSkillInvocation

const EvidenceKindSkillRead = archive.EvidenceKindSkillRead

const EvidenceKindLifecycleHook = archive.EvidenceKindLifecycleHook

const EvidenceKindFinalResponse = archive.EvidenceKindFinalResponse

const EvidenceKindExplicitFeedback = archive.EvidenceKindExplicitFeedback

const EvidenceKindLinkedSession = archive.EvidenceKindLinkedSession

const EvidenceKindCaptureGap = archive.EvidenceKindCaptureGap

type LinkedSessionStatus = archive.LinkedSessionStatus

const LinkedSessionPending = archive.LinkedSessionPending

const LinkedSessionPublished = archive.LinkedSessionPublished

const LinkedSessionUnavailable = archive.LinkedSessionUnavailable

type LinkedSessionReference = archive.LinkedSessionReference

type ModelSummarySource = archive.ModelSummarySource

const ModelSummarySourceNativeTranscript = archive.ModelSummarySourceNativeTranscript

const ModelSummarySourceHook = archive.ModelSummarySourceHook

type ResponseModelStatus = archive.ResponseModelStatus

const ResponseModelStatusNotExposed = archive.ResponseModelStatusNotExposed

const ResponseModelStatusObserved = archive.ResponseModelStatusObserved

type Config = archive.Config

type ProjectActivation = archive.ProjectActivation

type Harness = archive.Harness

type SessionOrigin = archive.SessionOrigin

const SessionOriginHook = archive.SessionOriginHook

const SessionOriginImport = archive.SessionOriginImport

type StartedAtSource = archive.StartedAtSource

const StartedAtSourceHook = archive.StartedAtSourceHook

const StartedAtSourceTranscript = archive.StartedAtSourceTranscript

const StartedAtSourceFileCreated = archive.StartedAtSourceFileCreated

const StartedAtSourceCursorComposer = archive.StartedAtSourceCursorComposer

type SessionRegistration = archive.SessionRegistration

type CaptureGap = archive.CaptureGap

type CaptureBoundary = archive.CaptureBoundary

type FilteredTranscript = archive.FilteredTranscript

type SupplementalEvidence = archive.SupplementalEvidence

type SourceCapture = archive.SourceCapture

type SourceBundle = archive.SourceBundle

type TextTranscript = archive.TextTranscript

type SourceReference = archive.SourceReference

type AdapterInfo = archive.AdapterInfo

type ParserInfo = archive.ParserInfo

type SemanticConventionsInfo = archive.SemanticConventionsInfo

type Counts = archive.Counts

const UnknownModel = archive.UnknownModel

const MaxModelTokens = archive.MaxModelTokens

const OtherModels = archive.OtherModels

type ModelTokens = archive.ModelTokens

type ModelSummary = archive.ModelSummary

type SkillSnapshot = archive.SkillSnapshot

type SkillUse = archive.SkillUse

type ToolUsage = archive.ToolUsage

type Metadata = archive.Metadata

const CaptureGapImportedWithoutHookEvidence = archive.CaptureGapImportedWithoutHookEvidence

type ParseError = archive.ParseError

type NormalizedView = archive.NormalizedView

type TokenUsage = archive.TokenUsage

type TurnKind = archive.TurnKind

const TurnKindHumanPrompt = archive.TurnKindHumanPrompt

const TurnKindAssistant = archive.TurnKindAssistant

const TurnKindToolResult = archive.TurnKindToolResult

const TurnKindHarnessMeta = archive.TurnKindHarnessMeta

const TurnKindCommandOutput = archive.TurnKindCommandOutput

const TurnKindShellCommand = archive.TurnKindShellCommand

const TurnKindLocalCommand = archive.TurnKindLocalCommand

const TurnKindCompactSummary = archive.TurnKindCompactSummary

const TurnKindHarnessNotification = archive.TurnKindHarnessNotification

type TurnModelSource = archive.TurnModelSource

const TurnModelSourceTurnContext = archive.TurnModelSourceTurnContext

const TurnModelSourceNativeResponse = archive.TurnModelSourceNativeResponse

const TurnModelSourceNativeTranscript = archive.TurnModelSourceNativeTranscript

type NormalizedTurn = archive.NormalizedTurn

type HookFinalStatus = archive.HookFinalStatus

const HookFinalStatusUnreconciledIdentity = archive.HookFinalStatusUnreconciledIdentity

const HookFinalStatusSeparateSubagent = archive.HookFinalStatusSeparateSubagent

const HookFinalStatusMatchedMessageID = archive.HookFinalStatusMatchedMessageID

const HookFinalStatusMatchedTurnID = archive.HookFinalStatusMatchedTurnID

type HookFinalReconciliation = archive.HookFinalReconciliation

type NormalizedToolCall = archive.NormalizedToolCall

type NormalizedToolResult = archive.NormalizedToolResult

var IsParseError = archive.IsParseError

func ParseNormalized(bundle archive.SourceBundle) (archive.NormalizedView, error) {
	a, err := Parse(context.Background(), bundle)
	return a.View, err
}

func BuildMetadata(bundle archive.SourceBundle, machine string, start, derived time.Time, ref archive.SourceReference, info archive.ParserInfo) (archive.Metadata, error) {
	a, err := Parse(context.Background(), bundle)
	return archive.BuildMetadataWithAnalysis(bundle, a, err, machine, start, derived, ref, info)
}

func BuildHandoff(bundle archive.SourceBundle, m *archive.Metadata, opts archive.HandoffOptions) (archive.Handoff, error) {
	a, err := Parse(context.Background(), bundle)
	if err != nil {
		return archive.Handoff{}, err
	}
	return archive.BuildHandoffWithAnalysis(bundle, a, m, opts)
}

func BuildTranscript(bundle archive.SourceBundle, opts archive.HandoffOptions) (archive.Transcript, error) {
	a, err := Parse(context.Background(), bundle)
	if err != nil {
		return archive.Transcript{}, err
	}
	return archive.BuildTranscriptWithAnalysis(bundle, a, opts)
}

func SessionLabels(bundle archive.SourceBundle) (archive.Labels, bool) {
	a, err := Parse(context.Background(), bundle)
	if err != nil {
		return archive.Labels{}, false
	}
	return archive.LabelsFromAnalysis(a)
}

func IsFilterError(err error) bool { var target *FilterError; return errors.As(err, &target) }

type tokenTotals struct{ value archive.TokenAccumulator }

func (t *tokenTotals) observe(raw map[string]any, id, model string) {
	t.value.Observe(tokenObservation(raw), id, model)
}

func (t *tokenTotals) usage() (archive.TokenUsage, []archive.ModelTokens) { return t.value.Usage() }

var redactSensitive = archive.RedactSensitive

type sanitizeState = archive.PrivacyState

const maxTextBytes = 64 * 1024

const sessionTitleLimit = 128

func Parse(ctx context.Context, b archive.SourceBundle) (archive.Analysis, error) {
	return parse(ctx, b, nativeProfile(archive.CanonicalHarness(b.Capture.Harness.Name)))
}

func deriveGitActivity(calls []NormalizedToolCall) []GitEvent {
	return callMetadata(calls).GitActivity
}

func textTranscriptExchanges(texts []TextTranscript) ([]HandoffExchange, string) {
	a := archive.Analysis{Facts: archive.NativeFacts{Text: true, TextOnly: true}}
	b := archive.SourceBundle{NativeText: texts}
	parseText(&a, b)
	h, err := archive.BuildHandoffWithAnalysis(b, a, nil, HandoffOptions{})
	if err != nil {
		panic(err)
	}
	return h.Exchanges, h.LeftOff
}

func deriveSessionTitle(view NormalizedView, texts []TextTranscript) string {
	a := archive.Analysis{View: view}
	if len(texts) > 0 {
		parseText(&a, SourceBundle{Capture: SourceCapture{Harness: Harness{Name: "cursor"}}, NativeText: texts})
	}
	labels, _ := archive.LabelsFromAnalysis(a)
	return labels.Title
}

const maxModelNameRunes = 128

const toolNameLimit = 128

func callMetadata(calls []NormalizedToolCall) Metadata {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := SourceBundle{SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", Capture: SourceCapture{Harness: Harness{Name: "synthetic"}, AdapterName: "synthetic", AdapterVersion: "1", SourceFormat: "jsonl", FilterVersion: FilterVersion, CapturedAt: now}, NativeRecords: []map[string]any{{"safe": "retained"}}}
	analysis := archive.Analysis{View: NormalizedView{ToolCalls: calls}, Facts: archive.NativeFacts{}, Observability: archive.Observability{StructuredCounts: archive.Availability{State: archive.AvailabilityAvailable}}}
	m, err := archive.BuildMetadataWithAnalysis(b, analysis, nil, "m", now, now, SourceReference{Key: "synthetic", SHA256: strings.Repeat("a", 64)}, ParserInfo{})
	if err != nil {
		panic(err)
	}
	return m
}

func deriveToolsUsed(calls []NormalizedToolCall) []ToolUsage {
	return callMetadata(calls).ToolsUsed
}

func deriveMCPCalls(calls []NormalizedToolCall) []ToolUsage {
	return callMetadata(calls).MCPCalls
}

func metadataToolName(name string) string {
	tools := deriveToolsUsed([]NormalizedToolCall{{Name: name}})
	if len(tools) == 0 {
		return ""
	}
	return tools[0].Name
}

func toolSummary(name string, input, raw map[string]any, root string) string {
	call := observedTool(archive.NormalizedToolCall{Name: name, Input: input}, raw)
	h, err := archive.BuildHandoffWithAnalysis(SourceBundle{}, archive.Analysis{View: NormalizedView{ToolCalls: []NormalizedToolCall{call}}, Facts: archive.NativeFacts{WorkspaceRoot: root}}, nil, HandoffOptions{})
	if err != nil {
		panic(err)
	}
	if len(h.Exchanges) == 0 || len(h.Exchanges[0].Steps) == 0 {
		return ""
	}
	return h.Exchanges[0].Steps[0].Tool.Summary
}

func PreviewRecord(agent string, record []byte) (RecordPreview, error) {
	adapter, err := NewAdapter(agent)
	if err != nil {
		return RecordPreview{}, err
	}
	return Preview(context.Background(), adapter, record)
}

// NewAdapter returns a privacy-first adapter by canonical harness name.
func NewAdapter(name string) (Adapter, error) {
	switch archive.CanonicalHarness(name) {
	case archive.HarnessCodex:
		return CodexAdapter{}, nil
	case archive.HarnessClaude:
		return ClaudeAdapter{}, nil
	case archive.HarnessCursor:
		return CursorAdapter{}, nil
	default:
		return nil, fmt.Errorf("unsupported archive adapter %q", name)
	}
}

type GitHead = archive.GitHead

type Replay = archive.Replay
