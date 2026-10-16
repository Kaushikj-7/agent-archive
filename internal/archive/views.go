package archive

import (
	"errors"

	"time"
)

// ParseError means a parser could not derive a complete summary. The filtered
// source remains valid and should be retained with minimal failed metadata.
type ParseError struct{ Reason string }

func (e *ParseError) Error() string { return "metadata parse failed: " + e.Reason }

// NormalizedView is an in-memory analysis aid. It must never be uploaded as a
// second transcript representation.
type NormalizedView struct {
	Turns       []NormalizedTurn
	ToolCalls   []NormalizedToolCall
	ToolResults []NormalizedToolResult
	HookFinals  []HookFinalReconciliation
	// NativeSkillUses is collected in the same per-record walk as ToolCalls
	// (see toolActivity), rather than a second, separate traversal, so
	// deriveSkills only has to fold this in with supplemental evidence.
	NativeSkillUses []SkillUse
	// Tokens holds whatever token accounting the harness exposed in the
	// retained records. Absent accounting stays nil rather than zero.
	Tokens TokenUsage
	// ModelTokens is Tokens split by the model each record's usage belongs
	// to, sorted by model; empty when Tokens is.
	ModelTokens []ModelTokens
	// CompactBoundaries and CompactSummaries count Claude Code's
	// compact_boundary records and isCompactSummary records.
	CompactBoundaries int
	CompactSummaries  int
	// LatestRecordAt is the latest top-level timestamp (or created_at) of
	// any record the view read, whatever its kind: records carry no ordering
	// promise, so this is the maximum, not the last record's. Zero when no
	// record carried one.
	LatestRecordAt time.Time
}

// TokenUsage sums the token accounting a harness exposed. Each field is nil
// when no retained record reported it.
type TokenUsage struct {
	Input      *int `json:"input_tokens,omitempty"`
	Output     *int `json:"output_tokens,omitempty"`
	CacheRead  *int `json:"cache_read_tokens,omitempty"`
	CacheWrite *int `json:"cache_write_tokens,omitempty"`
	Reasoning  *int `json:"reasoning_tokens,omitempty"`
}

// TurnKind classifies a visible record for counting. A harness writes tool
// results in the user role, so the role alone cannot say whether a human
// spoke.
type TurnKind string

const (
	// TurnKindHumanPrompt is a user record carrying text or any other
	// non-tool-result content: what a person actually sent.
	TurnKindHumanPrompt TurnKind = "human_prompt"
	// TurnKindAssistant is a record in the assistant role.
	TurnKindAssistant TurnKind = "assistant"
	// TurnKindToolResult is a record whose content is only tool results.
	TurnKindToolResult TurnKind = "tool_result"
	// TurnKindHarnessMeta is a user record the harness wrote and marked
	// isMeta (an expanded skill or command). Filter 4 strips its text, so it
	// usually does not appear at all; a filter-3 bundle cannot tell it apart.
	TurnKindHarnessMeta TurnKind = "harness_meta"
	// TurnKindCommandOutput is the output of a local command or a `!` shell
	// command (<local-command-stdout>, <local-command-stderr>,
	// <local-command-caveat>, <bash-stdout>, <bash-stderr>).
	TurnKindCommandOutput TurnKind = "command_output"
	// TurnKindShellCommand is a `!` shell command the person ran directly
	// (<bash-input>). It counts as counts.user_shell_commands.
	TurnKindShellCommand TurnKind = "shell_command"
	// TurnKindLocalCommand is a typed slash command (<command-name>) which no
	// assistant answered before the next prompt, such as /model or /clear. A
	// slash command the assistant answered is a TurnKindHumanPrompt.
	TurnKindLocalCommand TurnKind = "local_command"
	// TurnKindCompactSummary is the user record Claude Code writes after
	// /compact or auto-compaction, marked isCompactSummary: a model-written
	// summary of the earlier conversation. Its text is kept, but it is
	// neither a prompt nor a message. Filters before 5 dropped the flag, so
	// in their bundles it still reads as a prompt.
	TurnKindCompactSummary TurnKind = "compact_summary"
	// TurnKindHarnessNotification is a user record the harness produced on
	// its own, such as Claude Code's background-task completion notice
	// (origin.kind "task-notification"). Filter 6 retains origin; an older
	// bundle cannot tell such a record from a prompt.
	TurnKindHarnessNotification TurnKind = "harness_notification"
)

// TurnModelSource names where a NormalizedTurn's model attribution came from.
type TurnModelSource string

// Turn model sources, one per harness.
const (
	// TurnModelSourceTurnContext is Codex: the model and reasoning level of
	// the latest turn_context record, as requested.
	TurnModelSourceTurnContext TurnModelSource = "turn_context"
	// TurnModelSourceNativeResponse is Claude Code: the model named on the
	// response message itself (NormalizedTurn.ResponseModel).
	TurnModelSourceNativeResponse TurnModelSource = "native_response"
	// TurnModelSourceNativeTranscript is any other harness: a model field on
	// the record, treated as the requested model.
	TurnModelSourceNativeTranscript TurnModelSource = "native_transcript"
)

// NormalizedTurn is one visible message record of a filtered source in a
// harness-independent shape: its role and TurnKind, retained
// text, model attribution, and the IDs and timestamp the record carried.
// RecordIndex is its position in SourceBundle.NativeRecords.
type NormalizedTurn struct {
	PresentationText  string   `json:"-"`
	PresentationKnown bool     `json:"-"`
	RecordIndex       int      `json:"record_index"`
	Role              string   `json:"role"`
	Kind              TurnKind `json:"kind,omitempty"`
	// MessageID is the id of the API message a record belongs to (Claude's
	// message.id). Several streamed records can share one.
	MessageID     string          `json:"message_id,omitempty"`
	Text          string          `json:"text,omitempty"`
	Model         string          `json:"model,omitempty"`
	ResponseModel string          `json:"response_model,omitempty"`
	ModelSource   TurnModelSource `json:"model_source,omitempty"`
	Provider      string          `json:"provider,omitempty"`
	Reasoning     string          `json:"reasoning_level,omitempty"`
	ID            string          `json:"id,omitempty"`
	ParentID      string          `json:"parent_id,omitempty"`
	TurnID        string          `json:"turn_id,omitempty"`
	Timestamp     string          `json:"timestamp,omitempty"`
}

// HookFinalStatus reports how a hook-reported final response was reconciled
// against the native transcript's turns.
type HookFinalStatus string

// Hook final statuses.
const (
	// HookFinalStatusUnreconciledIdentity means no transcript turn carried
	// the final response's message or turn ID.
	HookFinalStatusUnreconciledIdentity HookFinalStatus = "unreconciled_identity"
	// HookFinalStatusSeparateSubagent means the final response came from a
	// subagent (it names an agent ID), so it is not matched to this session's
	// turns.
	HookFinalStatusSeparateSubagent HookFinalStatus = "separate_subagent"
	// HookFinalStatusMatchedMessageID means a turn has the final response's
	// message ID.
	HookFinalStatusMatchedMessageID HookFinalStatus = "matched_message_id"
	// HookFinalStatusMatchedTurnID means an assistant turn has the final
	// response's turn ID.
	HookFinalStatusMatchedTurnID HookFinalStatus = "matched_turn_id"
)

// HookFinalReconciliation keeps hook-only finals separate from native source.
// It never uses identical text as a deduplication signal.
type HookFinalReconciliation struct {
	EvidenceIndex int             `json:"evidence_index"`
	Status        HookFinalStatus `json:"status"`
	MessageID     string          `json:"message_id,omitempty"`
	TurnID        string          `json:"turn_id,omitempty"`
	AgentID       string          `json:"agent_id,omitempty"`
}

// NormalizedToolCall is one tool call found in a filtered source, with the
// result linked to it when one was observed. RecordIndex is the position of
// the record that holds it in SourceBundle.NativeRecords.
type NormalizedToolCall struct {
	RecordIndex int    `json:"record_index"`
	CallID      string `json:"call_id,omitempty"`
	ParentID    string `json:"parent_id,omitempty"`
	Model       string `json:"model,omitempty"`
	Reasoning   string `json:"reasoning_level,omitempty"`
	Name        string `json:"name,omitempty"`
	// Input is the retained argument object exactly as the privacy filter
	// kept it. A harness which encodes its arguments as a JSON string (Codex)
	// contributes the decoded object; anything that does not decode to an
	// object stays nil.
	Input map[string]any `json:"input,omitempty"`
	// ResultRecordIndex, IsError, and OutputBytes describe the tool result
	// this call was linked to. Each is nil when its individual fact is unavailable;
	// a known occurrence need not establish error status or retained output size.
	// OutputBytes measures the retained output, which the filter may have
	// redacted or capped.
	ResultRecordIndex *int  `json:"result_record_index,omitempty"`
	IsError           *bool `json:"is_error,omitempty"`
	OutputBytes       *int  `json:"output_bytes,omitempty"`

	// Attribution and presentation facts come from the native parser and are
	// excluded from the published normalized view. No raw record is retained.
	RecordedBranch string     `json:"-"`
	RecordedAt     time.Time  `json:"-"`
	ResultAt       *time.Time `json:"-"`

	ObservedName string     `json:"-"`
	Invocation   bool       `json:"-"`
	ShellCommand string     `json:"-"`
	Action       ToolAction `json:"-"`
	ResultText   string     `json:"-"`
}

// NormalizedToolResult is a tool result observed in the source, before it is
// linked to the call it answers.
type NormalizedToolResult struct {
	RecordIndex int    `json:"record_index"`
	CallID      string `json:"call_id,omitempty"`
	IsError     bool   `json:"is_error,omitempty"`
	OutputBytes int    `json:"output_bytes"`

	// Text is the retained output OutputBytes measures; see
	// NormalizedToolCall.ResultText.
	RecordedAt time.Time `json:"-"`
	Text       string    `json:"-"`
}

func reconcileHookFinals(bundle SourceBundle, turns []NormalizedTurn) []HookFinalReconciliation {
	var out []HookFinalReconciliation
	for index, evidence := range bundle.SupplementalEvidence {
		if evidence.Kind != EvidenceKindFinalResponse {
			continue
		}
		final := HookFinalReconciliation{EvidenceIndex: index, MessageID: firstString(evidence.Payload, "message_id"), TurnID: firstString(evidence.Payload, "turn_id"), AgentID: firstString(evidence.Payload, "agent_id"), Status: HookFinalStatusUnreconciledIdentity}
		if final.AgentID != "" {
			final.Status = HookFinalStatusSeparateSubagent
		} else {
			for _, turn := range turns {
				if final.MessageID != "" && final.MessageID == turn.ID {
					final.Status = HookFinalStatusMatchedMessageID
					break
				}
				if final.TurnID != "" && turn.Role == "assistant" && final.TurnID == turn.TurnID {
					final.Status = HookFinalStatusMatchedTurnID
					break
				}
			}
		}
		out = append(out, final)
	}
	return out
}

func firstString(record map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, _ := record[key].(string); value != "" {
			return value
		}
	}
	return ""
}

// IsParseError supports the source-first publication flow.
func IsParseError(err error) bool {
	var target *ParseError
	return errors.As(err, &target)
}
