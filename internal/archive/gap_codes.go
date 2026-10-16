package archive

// CaptureGapCodes lists every capture gap code this version writes, into a
// source bundle's capture.gaps and so into metadata capture_gaps. Both
// schemas enumerate the same list (schema_test.go checks it, and that every
// code the filter writes is listed). A reader must still accept a code not
// listed here: an older or newer writer may use others. Codes are
// archive-authored and never carry transcript content; see
// dev/specs/privacy-filter.md for what each one means.
var CaptureGapCodes = []string{
	CaptureGapDiscoveredWithoutHookEvidence,
	"recovered_generation",
	"history_cumulative_tokens_unavailable",
	"own_usage_observations_partial",
	"cache_reasoning_cost_unqualified",
	// Source filter (every format).
	"binary_content_omitted",
	"content_truncated",
	"hidden_instruction_omitted",
	"hidden_or_unknown_nested_content_omitted",
	"incomplete_or_invalid_record",
	"record_without_allowed_fields_omitted",
	"sensitive_content_redacted",
	"sensitive_or_hidden_field_omitted",
	"unknown_field_omitted",
	"unknown_record_type",
	"unsupported_value_omitted",
	// Cursor plain-text transcripts.
	"text_structure_partial",
	// Cursor chats read from Cursor's database (cursor-composer).
	"cursor_blob_content_unavailable",
	"cursor_bubble_id_mismatch",
	"cursor_bubble_missing",
	"cursor_context_omitted",
	"cursor_incomplete_tail_omitted",
	"cursor_inline_conversation_omitted",
	"cursor_message_type_unknown",
	"cursor_tool_argument_omitted",
	// Supplemental evidence, filtered by FilterSupplementalEvidence.
	"subagent_final_not_reconciled",
	"native_parent_link_pending",
	"native_child_link_unverified",
	"supplemental_evidence_omitted",
	// Capture-gap evidence recorded outside the filter: the collector's
	// rewritten Cursor database chat, the hook's unidentifiable subagent, and
	// the collector's subagent whose transcript Claude Code never wrote.
	"cursor_chat_rewritten",
	"subagent_identity_unavailable",
	"subagent_transcript_never_written",
	// Added to metadata for a session backfill imported.
	CaptureGapImportedWithoutHookEvidence,
}
