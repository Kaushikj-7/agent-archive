package backfill

import (
	"io"
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

type diagnosticDescription struct {
	action DiagnosticAction
	text   string
}

// The finite vocabulary bounds rendering even when thousands of sources fail.
var diagnosticDescriptions = map[DiagnosticDetail]diagnosticDescription{
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
	DetailHistoryLookupPending:        {ActionAwaitSupport, "Related history remains pending; current selection and dependencies have not been inspected by backfill."},
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
	case SkipRegisteredNotAdmitted, SkipProjectUnknown, SkipIdentityMismatch,
		SkipRelatedHistory, SkipSourceChanged, SkipEmpty, SkipUnsafeFormat,
		SkipTooLarge, SkipStartUnknown, SkipStartInFuture:
		return "unresolved"
	default:
		return "unresolved"
	}
	return "unresolved"
}
