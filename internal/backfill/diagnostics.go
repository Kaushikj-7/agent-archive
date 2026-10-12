package backfill

import (
	"io"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"sort"

	"github.com/wangjohn/agent-archive/internal/sourcefacts"
	"github.com/wangjohn/agent-archive/internal/terminal"
)

// DiagnosticDetail is a fixed content-free classification, never source text.
type DiagnosticDetail string

// DiagnosticAction describes whether retry or a reviewed policy change can help.
type DiagnosticAction string

// Diagnostic actions never grant permission to import a source.
const (
	ActionRetry         DiagnosticAction = "retry"
	ActionReviewProject DiagnosticAction = "review_project"
	ActionReviewMapping DiagnosticAction = "review_mapping"
	ActionAwaitSupport  DiagnosticAction = "await_support"
	ActionReviewSource  DiagnosticAction = "review_source"
)

// Diagnostic is a content-free explanation subordinate to the primary skip code.
// Detail is a fixed code, never a native ID, path, Git stderr or transcript field.
type Diagnostic struct {
	Detail DiagnosticDetail `json:"detail"`
	Action DiagnosticAction `json:"action"`
}

// DiagnosticSummary counts a fixed diagnostic without exposing individual sources.
type DiagnosticSummary struct {
	Skip SkipReason `json:"skip"`
	Diagnostic
	Count int `json:"count"`
}

// Diagnostic details for recovery outcomes and skips. Recovery outcome details
// reuse the sourcefacts outcome codes so JSON output is unchanged.
const (
	DetailRecoveryBudget                               = DiagnosticDetail(sourcefacts.RecoveryBudgetExhausted)
	DetailInventoryUnavailable                         = DiagnosticDetail(sourcefacts.RecoveryInventoryUnavailable)
	DetailRecoveryExcluded                             = DiagnosticDetail(sourcefacts.RecoveryExcluded)
	DetailRecoveryAmbiguous                            = DiagnosticDetail(sourcefacts.RecoveryAmbiguous)
	DetailRepositoryUnavailable                        = DiagnosticDetail(sourcefacts.RecoveryRepositoryUnavailable)
	DetailMappingConflict                              = DiagnosticDetail(sourcefacts.RecoveryMappingConflict)
	DetailSubtreeUnavailable                           = DiagnosticDetail(sourcefacts.RecoverySubtreeUnavailable)
	DetailRecoveryUnavailable                          = DiagnosticDetail(sourcefacts.RecoveryUnavailable)
	DetailWorktreeEvidenceUnavailable DiagnosticDetail = "worktree_evidence_unavailable"
	DetailHistoryLookupPending        DiagnosticDetail = "history_lookup_pending"
	DetailSourceChanged               DiagnosticDetail = "source_changed"
	DetailSourceInspectionUnavailable DiagnosticDetail = "source_inspection_unavailable"
	DetailSourceSizeLimit             DiagnosticDetail = "source_size_limit"
)

// Native refusal diagnostic details retain the public JSON codes.
const (
	// DetailAggregateHistoryByteLimit describes the aggregate history byte limit refusal.
	DetailAggregateHistoryByteLimit DiagnosticDetail = DiagnosticDetail(agentapi.CauseAggregateHistoryByteLimit)
	// DetailCurrentRolloutEvidenceUnavailable describes the current rollout evidence unavailable refusal.
	DetailCurrentRolloutEvidenceUnavailable DiagnosticDetail = DiagnosticDetail(agentapi.CauseCurrentRolloutEvidenceUnavailable)
	// DetailForkOwnershipBoundaryUnavailable describes the fork ownership boundary unavailable refusal.
	DetailForkOwnershipBoundaryUnavailable DiagnosticDetail = DiagnosticDetail(agentapi.CauseForkOwnershipBoundaryUnavailable)
	// DetailHistoricalCandidateLimit describes the historical candidate limit refusal.
	DetailHistoricalCandidateLimit DiagnosticDetail = DiagnosticDetail(agentapi.CauseHistoricalCandidateLimit)
	// DetailHistoryCoverage describes the history coverage refusal.
	DetailHistoryCoverage DiagnosticDetail = DiagnosticDetail(agentapi.CauseHistoryCoverage)
	// DetailHistoryDependencyUnavailable describes the history dependency unavailable refusal.
	DetailHistoryDependencyUnavailable DiagnosticDetail = DiagnosticDetail(agentapi.CauseHistoryDependencyUnavailable)
	// DetailHistoryOrdinalOverflow describes the history ordinal overflow refusal.
	DetailHistoryOrdinalOverflow DiagnosticDetail = DiagnosticDetail(agentapi.CauseHistoryOrdinalOverflow)
	// DetailHistoryRecordLimit describes the history record limit refusal.
	DetailHistoryRecordLimit DiagnosticDetail = DiagnosticDetail(agentapi.CauseHistoryRecordLimit)
	// DetailLegacyCompatibilityRequiresUnboundAdmission describes the legacy compatibility requires unbound admission refusal.
	DetailLegacyCompatibilityRequiresUnboundAdmission DiagnosticDetail = DiagnosticDetail(agentapi.CauseLegacyCompatibilityRequiresUnboundAdmission)
	// DetailNativeAdmissionEvidenceUnavailable describes the native admission evidence unavailable refusal.
	DetailNativeAdmissionEvidenceUnavailable DiagnosticDetail = DiagnosticDetail(agentapi.CauseNativeAdmissionEvidenceUnavailable)
	// DetailNativeBoundaryScratchBudgetExhausted describes the native boundary scratch budget exhausted refusal.
	DetailNativeBoundaryScratchBudgetExhausted DiagnosticDetail = DiagnosticDetail(agentapi.CauseNativeBoundaryScratchBudgetExhausted)
	// DetailNativeCreationEvidenceUnavailable describes the native creation evidence unavailable refusal.
	DetailNativeCreationEvidenceUnavailable DiagnosticDetail = DiagnosticDetail(agentapi.CauseNativeCreationEvidenceUnavailable)
	// DetailNativeRecordWindowBudgetExhausted describes the native record window budget exhausted refusal.
	DetailNativeRecordWindowBudgetExhausted DiagnosticDetail = DiagnosticDetail(agentapi.CauseNativeRecordWindowBudgetExhausted)
	// DetailNativeResourceLimit describes the native resource limit refusal.
	DetailNativeResourceLimit DiagnosticDetail = "native_resource_limit"
	// DetailNativeSourceUnavailable describes the native source unavailable refusal.
	DetailNativeSourceUnavailable DiagnosticDetail = "native_source_unavailable"
	// DetailOrdinaryNativeScratchBudgetExhausted describes the ordinary native scratch budget exhausted refusal.
	DetailOrdinaryNativeScratchBudgetExhausted DiagnosticDetail = DiagnosticDetail(agentapi.CauseOrdinaryNativeScratchBudgetExhausted)
	// DetailOriginalFactCandidateLimit describes the original fact candidate limit refusal.
	DetailOriginalFactCandidateLimit DiagnosticDetail = DiagnosticDetail(agentapi.CauseOriginalFactCandidateLimit)
	// DetailOwnTaskBeforeCreation describes the own task before creation refusal.
	DetailOwnTaskBeforeCreation DiagnosticDetail = DiagnosticDetail(agentapi.CauseOwnTaskBeforeCreation)
	// DetailOwnTaskExecutionUnsupported describes the own task execution unsupported refusal.
	DetailOwnTaskExecutionUnsupported DiagnosticDetail = DiagnosticDetail(agentapi.CauseOwnTaskExecutionUnsupported)
	// DetailOwnTaskInvalid describes the own task invalid refusal.
	DetailOwnTaskInvalid DiagnosticDetail = DiagnosticDetail(agentapi.CauseOwnTaskInvalid)
	// DetailOwnTaskMalformed describes the own task malformed refusal.
	DetailOwnTaskMalformed DiagnosticDetail = "own_task_malformed"
	// DetailOwnTaskMissing describes the own task missing refusal.
	DetailOwnTaskMissing DiagnosticDetail = DiagnosticDetail(agentapi.CauseOwnTaskMissing)
	// DetailOwnTaskRootTurnInvalid describes the own task root turn invalid refusal.
	DetailOwnTaskRootTurnInvalid DiagnosticDetail = "own_task_root_turn_invalid"
	// DetailOwnTaskStartedAtInvalid describes the own task started at invalid refusal.
	DetailOwnTaskStartedAtInvalid DiagnosticDetail = "own_task_started_at_invalid"
	// DetailOwnTaskStartedAtMissing describes the own task started at missing refusal.
	DetailOwnTaskStartedAtMissing DiagnosticDetail = "own_task_started_at_missing"
	// DetailOwnTaskTurnIDInvalid describes the own task turn id invalid refusal.
	DetailOwnTaskTurnIDInvalid DiagnosticDetail = "own_task_turn_id_invalid"
	// DetailRelatedAdmissionRequiresConfinedSourceHome describes the related admission requires confined source home refusal.
	DetailRelatedAdmissionRequiresConfinedSourceHome DiagnosticDetail = DiagnosticDetail(agentapi.CauseRelatedAdmissionRequiresConfinedSourceHome)
	// DetailRelatedHistoryRequiresAdmittedConfinedSourceHome describes the related history requires admitted confined source home refusal.
	DetailRelatedHistoryRequiresAdmittedConfinedSourceHome DiagnosticDetail = DiagnosticDetail(agentapi.CauseRelatedHistoryRequiresAdmittedConfinedSourceHome)
	// DetailRolloutCandidateLimit describes the rollout candidate limit refusal.
	DetailRolloutCandidateLimit DiagnosticDetail = DiagnosticDetail(agentapi.CauseRolloutCandidateLimit)
	// DetailRolloutHistoryDepthLimit describes the rollout history depth limit refusal.
	DetailRolloutHistoryDepthLimit DiagnosticDetail = DiagnosticDetail(agentapi.CauseRolloutHistoryDepthLimit)
	// DetailRolloutLocatorLimit describes the rollout locator limit refusal.
	DetailRolloutLocatorLimit DiagnosticDetail = DiagnosticDetail(agentapi.CauseRolloutLocatorLimit)
	// DetailRolloutMetadataIncomplete describes the rollout metadata incomplete refusal.
	DetailRolloutMetadataIncomplete DiagnosticDetail = DiagnosticDetail(agentapi.CauseRolloutMetadataIncomplete)
	// DetailSessionIndexRecovery describes the session index recovery refusal.
	DetailSessionIndexRecovery DiagnosticDetail = "session_index_recovery"
	// DetailSharedRolloutBudgetExhausted describes the shared rollout budget exhausted refusal.
	DetailSharedRolloutBudgetExhausted DiagnosticDetail = "shared_rollout_budget_exhausted"
	// DetailSharedRolloutFileLimit describes the shared rollout file limit refusal.
	DetailSharedRolloutFileLimit DiagnosticDetail = DiagnosticDetail(agentapi.CauseSharedRolloutFileLimit)
	// DetailSourceAndLookupRequireTheSameChargeLedger describes the source and lookup require the same charge ledger refusal.
	DetailSourceAndLookupRequireTheSameChargeLedger DiagnosticDetail = DiagnosticDetail(agentapi.CauseSourceAndLookupRequireTheSameChargeLedger)
)

type diagnosticDescription struct {
	action DiagnosticAction
	text   string
}

// The finite vocabulary bounds rendering even when thousands of sources fail.
var diagnosticDescriptions = map[DiagnosticDetail]diagnosticDescription{
	DetailSessionIndexRecovery:                             {ActionRetry, "Local session identity bookkeeping is being recovered or needs repair. These sessions cannot be imported until the background collector completes recovery; retry backfill afterwards."},
	DetailOwnTaskMalformed:                                 {ActionAwaitSupport, "The first own task has malformed event or identity fields."},
	DetailOwnTaskTurnIDInvalid:                             {ActionAwaitSupport, "The first own task has no valid native turn UUID."},
	DetailOwnTaskRootTurnInvalid:                           {ActionAwaitSupport, "The first own task has an invalid root turn UUID."},
	DetailOwnTaskStartedAtMissing:                          {ActionAwaitSupport, "The first own task has no started_at timestamp."},
	DetailOwnTaskStartedAtInvalid:                          {ActionAwaitSupport, "The first own task has an invalid started_at timestamp."},
	DetailOwnTaskExecutionUnsupported:                      {ActionAwaitSupport, "The own task's source is not a supported local execution format."},
	DetailHistoryCoverage:                                  {ActionRetry, "Codex history coverage is incomplete; retry after the native inventory settles."},
	DetailNativeAdmissionEvidenceUnavailable:               {ActionAwaitSupport, "The source reader cannot supply native admission evidence."},
	DetailOwnTaskMissing:                                   {ActionReviewSource, "No first own task was found beyond the inherited history boundary."},
	DetailOwnTaskInvalid:                                   {ActionAwaitSupport, "The first own task lacks valid native identity, execution or timestamp evidence."},
	DetailOwnTaskBeforeCreation:                            {ActionAwaitSupport, "The first own task predates native session creation."},
	DetailNativeResourceLimit:                              {ActionAwaitSupport, "The source reader reached a resource limit without a more specific reason."},
	DetailNativeSourceUnavailable:                          {ActionReviewSource, "The source reader could not obtain required evidence; no more specific reason was supplied."},
	DetailAggregateHistoryByteLimit:                        {ActionAwaitSupport, "Aggregate history byte limit."},
	DetailCurrentRolloutEvidenceUnavailable:                {ActionReviewSource, "Current rollout evidence unavailable."},
	DetailForkOwnershipBoundaryUnavailable:                 {ActionReviewSource, "Fork ownership boundary unavailable."},
	DetailHistoricalCandidateLimit:                         {ActionAwaitSupport, "Historical candidate limit."},
	DetailHistoryDependencyUnavailable:                     {ActionReviewSource, "History dependency unavailable."},
	DetailHistoryOrdinalOverflow:                           {ActionAwaitSupport, "History ordinal overflow."},
	DetailHistoryRecordLimit:                               {ActionAwaitSupport, "History record limit."},
	DetailLegacyCompatibilityRequiresUnboundAdmission:      {ActionReviewSource, "Legacy compatibility requires unbound admission."},
	DetailNativeBoundaryScratchBudgetExhausted:             {ActionAwaitSupport, "Native boundary scratch budget exhausted."},
	DetailNativeCreationEvidenceUnavailable:                {ActionReviewSource, "Native creation evidence unavailable."},
	DetailNativeRecordWindowBudgetExhausted:                {ActionAwaitSupport, "Native record window budget exhausted."},
	DetailOrdinaryNativeScratchBudgetExhausted:             {ActionAwaitSupport, "Ordinary native scratch budget exhausted."},
	DetailOriginalFactCandidateLimit:                       {ActionAwaitSupport, "Original fact candidate limit."},
	DetailRelatedAdmissionRequiresConfinedSourceHome:       {ActionReviewSource, "Related admission requires confined source home."},
	DetailRelatedHistoryRequiresAdmittedConfinedSourceHome: {ActionReviewSource, "Related history requires admitted confined source home."},
	DetailRolloutCandidateLimit:                            {ActionAwaitSupport, "Rollout candidate limit."},
	DetailRolloutHistoryDepthLimit:                         {ActionAwaitSupport, "Rollout history depth limit."},
	DetailRolloutLocatorLimit:                              {ActionAwaitSupport, "Rollout locator limit."},
	DetailRolloutMetadataIncomplete:                        {ActionReviewSource, "Rollout metadata incomplete."},
	DetailSharedRolloutBudgetExhausted:                     {ActionAwaitSupport, "Shared rollout budget exhausted."},
	DetailSharedRolloutFileLimit:                           {ActionAwaitSupport, "Shared rollout file limit."},
	DetailSourceAndLookupRequireTheSameChargeLedger:        {ActionReviewSource, "Source and lookup require the same charge ledger."},
	// Most recovery limits are fixed (configured roots, dependency stamps,
	// Cursor catalog rows), so the same plan reaches them again; retrying
	// clears only a timeout. An exact mapping bypasses the shared inventory.
	DetailRecoveryBudget:              {ActionReviewMapping, "Project recovery reached a fixed work or time limit. Retrying helps only if Git or the disk was slow; otherwise review an exact --map-project assignment."},
	DetailInventoryUnavailable:        {ActionRetry, "Configured repository evidence is unavailable or changed; check repository access and Git, then retry."},
	DetailRecoveryExcluded:            {ActionReviewProject, "Recorded repository identity matches an excluded project; review the exclusion."},
	DetailRecoveryAmbiguous:           {ActionReviewMapping, "Recorded repository identity matches multiple configured repositories; review an exact --map-project assignment."},
	DetailRepositoryUnavailable:       {ActionReviewProject, "Recorded repository identity is missing or has no matching configured repository; review the project and an exact --map-project assignment."},
	DetailMappingConflict:             {ActionReviewMapping, "The mapping conflicts with configured ownership or recorded repository identity; review the assignment."},
	DetailSubtreeUnavailable:          {ActionReviewMapping, "Repository identity cannot prove the original subtree's ownership; review project scope."},
	DetailRecoveryUnavailable:         {ActionReviewProject, "The original project evidence cannot be used safely; review the project."},
	DetailWorktreeEvidenceUnavailable: {ActionReviewProject, "The worktree cannot be followed to its repository; review the checkout and project."},
	DetailHistoryLookupPending:        {ActionAwaitSupport, "Native history remains pending; this refusal has no more specific diagnostic."},
	DetailSourceChanged:               {ActionRetry, "The source changed during planning; retry after it settles."},
	DetailSourceInspectionUnavailable: {ActionReviewSource, "The source could not be safely inspected or filtered; check source access and format support."},
	DetailSourceSizeLimit:             {ActionAwaitSupport, "The source exceeds the supported size limit."},
	// Recovery gap causes replace project_inventory_unavailable when other
	// sessions' evidence, not configured repository evidence, is what could
	// not be observed (see recoveryGaps).
	CauseCursorDatabaseUnavailable:   {ActionRetry, "Recovery was blocked because Cursor's chat database could not be fully read, so a Cursor chat may name another clone of this repository; retry when Cursor is idle."},
	CauseCursorChatFolderUnavailable: {ActionReviewSource, "Recovery was blocked because a Cursor chat's workspace folder could not be determined, so it may be another clone of this repository; check Cursor's workspaceStorage, then retry."},
	CauseNativeInventoryChanged:      {ActionRetry, "Recovery was blocked because session files changed while planning; retry after the agents are idle."},
	CauseNativeStoreUnreadable:       {ActionReviewSource, "Recovery was blocked because an agent's session store could not be fully listed, so an unseen session may name another clone of this repository; check its access, then retry."},
	CauseWitnessLimit:                {ActionReviewMapping, "Recovery was blocked because sessions name more repository checkouts than backfill compares (1,024), so retrying will not help; review an exact --map-project assignment."},
	CauseSessionFolderUnknown:        {ActionReviewSource, "Recovery was blocked because another session's folder could not be determined, so it may be another clone of this repository; review that session's source."},
}

// Recovery gap causes name which non-configured witness evidence was missing.
// The set is finite and never carries a path, an agent name or a native ID.
const (
	CauseCursorDatabaseUnavailable   DiagnosticDetail = "cursor_database_unavailable"
	CauseCursorChatFolderUnavailable DiagnosticDetail = "cursor_chat_folder_unavailable"
	CauseNativeInventoryChanged      DiagnosticDetail = "native_inventory_changed"
	CauseNativeStoreUnreadable       DiagnosticDetail = "native_store_unreadable"
	CauseSessionFolderUnknown        DiagnosticDetail = "session_folder_unknown"
	// CauseWitnessLimit is the fixed limit on distinct witness roots. It is not
	// a budget: retrying observes the same roots.
	CauseWitnessLimit DiagnosticDetail = "project_witness_limit"
	// CauseRecoveryBudget reuses the budget outcome's detail and text.
	CauseRecoveryBudget = DetailRecoveryBudget
)

// candidateDiagnostic explains skip. cause, when set, names the missing
// witness evidence behind an inventory-unavailable recovery outcome.
func candidateDiagnostic(skip SkipReason, outcome sourcefacts.RecoveryOutcome, cause DiagnosticDetail) *Diagnostic {
	var detail DiagnosticDetail
	switch skip {
	case SkipWorktreeUnresolved:
		detail = DiagnosticDetail(outcome)
		if outcome == sourcefacts.RecoveryInventoryUnavailable && cause != "" {
			detail = cause
		}
		if detail == "" {
			detail = DetailWorktreeEvidenceUnavailable
		}
	case SkipIndexRecovery:
		detail = DetailSessionIndexRecovery
	case SkipRelatedHistory:
		detail = DetailHistoryLookupPending
	case SkipSourceChanged:
		detail = DetailSourceChanged
	case SkipUnsafeFormat:
		detail = DetailSourceInspectionUnavailable
	case SkipTooLarge:
		detail = DetailSourceSizeLimit
	case "", SkipAlreadyArchived, SkipDuplicateSession, SkipRegisteredNotAdmitted,
		SkipRemovedByUndo, SkipRemovedByRetention, SkipFilteredOut, SkipExcludedProject, SkipExcludedHistory,
		SkipHomeDirectory, SkipAboveHome, SkipTemporaryDirectory, SkipProjectUnknown,
		SkipIdentityMismatch, SkipEmpty, SkipStartUnknown, SkipStartInFuture:
		return nil
	default:
		return nil
	}
	description, ok := diagnosticDescriptions[detail]
	if !ok {
		return nil
	}
	return &Diagnostic{Detail: detail, Action: description.action}
}

// Diagnostics aggregates only known diagnostics belonging to the winning skip.
func (p Plan) Diagnostics() []DiagnosticSummary {
	counts := map[string]DiagnosticSummary{}
	for _, c := range p.Candidates {
		if c.Skip == "" || c.Diagnostic == nil {
			continue
		}
		d, ok := diagnosticDescriptions[c.Diagnostic.Detail]
		if !ok {
			continue
		}
		key := string(c.Skip) + "\x00" + string(c.Diagnostic.Detail)
		summary := counts[key]
		summary.Skip, summary.Detail, summary.Action = c.Skip, c.Diagnostic.Detail, d.action
		summary.Count++
		counts[key] = summary
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]DiagnosticSummary, 0, len(keys))
	for _, key := range keys {
		out = append(out, counts[key])
	}
	return out
}

func renderDiagnostics(w io.Writer, p Plan) {
	for _, d := range p.Diagnostics() {
		terminal.Printf(w, "      %d: %s\n", d.Count, diagnosticDescriptions[d.Detail].text)
	}
}

// InventoryAccounting reconciles unique physical candidate files, separately
// from database-only candidates. It does not claim logical-history accounting:
// dependencies, identical copies and retained revisions need the shared lookup.
type InventoryAccounting struct {
	UniqueCandidateFiles  int            `json:"unique_candidate_files"`
	DatabaseCandidates    int            `json:"database_candidates"`
	Dispositions          map[string]int `json:"dispositions"`
	LogicalHistoryPending bool           `json:"logical_history_pending"`
}

// InventoryAccounting assigns each observed physical candidate exactly one
// disposition. Duplicate candidates are not claimed to be identical copies.
func (p Plan) InventoryAccounting() InventoryAccounting {
	out := InventoryAccounting{Dispositions: map[string]int{}, LogicalHistoryPending: true}
	files := map[string]map[string]bool{}
	for _, c := range p.Candidates {
		if c.TranscriptPath == "" {
			out.DatabaseCandidates++
			continue
		}
		if files[c.TranscriptPath] == nil {
			files[c.TranscriptPath] = map[string]bool{}
		}
		files[c.TranscriptPath][candidateDisposition(c)] = true
	}
	out.UniqueCandidateFiles = len(files)
	for _, dispositions := range files {
		// A repeated observation discarded as a duplicate does not make the
		// selected physical file a discarded copy. Prefer actual selection;
		// conflicting nonselected observations remain unresolved.
		disposition := "unresolved"
		switch {
		case dispositions["planned_import"]:
			disposition = "planned_import"
		case dispositions["already_archived"]:
			disposition = "already_archived"
		default:
			if len(dispositions) > 1 {
				delete(dispositions, "duplicate_candidate")
			}
			if len(dispositions) == 1 {
				for value := range dispositions {
					disposition = value
				}
			}
		}
		out.Dispositions[disposition]++
	}
	return out
}

func candidateDisposition(c Candidate) string {
	switch c.Skip {
	case "":
		return "planned_import"
	case SkipDuplicateSession:
		return "duplicate_candidate"
	case SkipAlreadyArchived:
		return "already_archived"
	case SkipExcludedProject, SkipExcludedHistory, SkipFilteredOut, SkipHomeDirectory, SkipAboveHome, SkipTemporaryDirectory, SkipRemovedByUndo, SkipRemovedByRetention:
		return "excluded"
	case SkipWorktreeUnresolved:
		if c.Diagnostic != nil && c.Diagnostic.Detail == DetailRecoveryExcluded {
			return "excluded"
		}
	case SkipRegisteredNotAdmitted, SkipProjectUnknown, SkipIndexRecovery, SkipIdentityMismatch,
		SkipRelatedHistory, SkipSourceChanged, SkipEmpty, SkipUnsafeFormat,
		SkipTooLarge, SkipStartUnknown, SkipStartInFuture:
		return "unresolved"
	default:
		return "unresolved"
	}
	return "unresolved"
}

// sourceRefusalDetail walks classified errors without rendering native error text.
// Unknown causes fall back to a fixed kind-level reason.
func sourceRefusalDetail(err error) DiagnosticDetail {
	if err == nil {
		return ""
	}
	if classified, ok := err.(agentapi.Classified); ok {
		_, cause := classified.Classification()
		detail := DiagnosticDetail(cause)
		if _, known := diagnosticDescriptions[detail]; known && detail != "" {
			return detail
		}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if detail := specificRefusalDetail(child); detail != "" {
				return detail
			}
		}
	} else if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if detail := specificRefusalDetail(wrapped.Unwrap()); detail != "" {
			return detail
		}
	}
	if agentapi.HasFailure(err, agentapi.Limit) {
		return DetailNativeResourceLimit
	}
	if agentapi.HasFailure(err, agentapi.Unavailable) {
		return DetailNativeSourceUnavailable
	}
	return ""
}

func specificRefusalDetail(err error) DiagnosticDetail {
	detail := sourceRefusalDetail(err)
	if detail == DetailNativeResourceLimit || detail == DetailNativeSourceUnavailable {
		return ""
	}
	return detail
}
