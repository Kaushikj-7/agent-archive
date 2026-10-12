// Package codexmeta interprets bounded Codex identity and execution metadata without I/O.
package codexmeta

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// HistoryMode names the native JSONL history representation.
type HistoryMode string

const (
	// CodexHistoryLegacy identifies the original JSONL history representation.
	CodexHistoryLegacy HistoryMode = "legacy"
	// CodexHistoryPaginated identifies the paginated JSONL history representation.
	CodexHistoryPaginated HistoryMode = "paginated"
)

// UnmarshalJSON rejects explicit null, empty, and non-string modes. An absent
// mode is legacy; upstream's recorder rejects these explicit representations.
func (m *HistoryMode) UnmarshalJSON(raw []byte) error {
	var mode string
	if err := json.Unmarshal(raw, &mode); err != nil {
		return err
	}
	if mode == "" {
		return errors.New("invalid history mode")
	}
	*m = HistoryMode(mode)
	return nil
}

type codexExecutionSource string

const (
	executionCLI    codexExecutionSource = "cli"
	executionExec   codexExecutionSource = "exec"
	executionVSCode codexExecutionSource = "vscode"
)

// codexThreadSource names the native thread_source values capture understands.
// Codex serializes any other feature-defined source as its own string; those
// stay unsupported until their ownership is known.
type codexThreadSource string

const (
	threadUser     codexThreadSource = "user"
	threadSubagent codexThreadSource = "subagent"
	// threadAgentCreated is a top-level thread the Codex app opens for a task
	// another thread's agent handed off. It carries no parent, spawn edge or
	// agent markers, Codex lists and names it like a user thread, and the user
	// continues it there, so it is captured as one.
	threadAgentCreated codexThreadSource = "agent_created_thread"
)

type codexTaskEvent string

const (
	taskStarted codexTaskEvent = "task_started"
	turnStarted codexTaskEvent = "turn_started"
)

// CodexMeta is identity and execution metadata, never transcript body.
type CodexMeta struct {
	Git             GitInfo         `json:"git"`
	ThreadSource    json.RawMessage `json:"thread_source"`
	AgentPath       json.RawMessage `json:"agent_path"`
	AgentRole       json.RawMessage `json:"agent_role"`
	AgentType       json.RawMessage `json:"agent_type"`
	AgentNickname   json.RawMessage `json:"agent_nickname"`
	ID              string          `json:"id"`
	SessionID       string          `json:"session_id"`
	Timestamp       string          `json:"timestamp"`
	Cwd             string          `json:"cwd"`
	Source          json.RawMessage `json:"source"`
	Originator      string          `json:"originator"`
	Version         string          `json:"cli_version"`
	ForkedFrom      json.RawMessage `json:"forked_from_id"`
	ForkOrdinal     json.RawMessage `json:"forked_from_ordinal_exclusive"`
	Parent          json.RawMessage `json:"parent_thread_id"`
	HistoryBase     json.RawMessage `json:"history_base"`
	HistoryMode     HistoryMode     `json:"history_mode"`
	SubagentOrdinal json.RawMessage `json:"subagent_history_start_ordinal"`
}

var uuid = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var codexRolloutUUID = regexp.MustCompile(`(?i)([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// RolloutID returns the physical history segment ID, which can differ from the thread ID after revert.
func RolloutID(path string) string {
	m := codexRolloutUUID.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return ""
	}
	return m[1]
}

// ParseCodexMeta decodes a session_meta record. Other records are ignored.
func ParseCodexMeta(line []byte) (CodexMeta, time.Time, bool, error) {
	var r struct {
		Type      string    `json:"type"`
		Timestamp string    `json:"timestamp"`
		Payload   CodexMeta `json:"payload"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return CodexMeta{}, time.Time{}, false, err
	}
	if r.Type != "session_meta" {
		return CodexMeta{}, time.Time{}, false, nil
	}
	for _, ts := range []string{r.Payload.Timestamp, r.Timestamp} {
		if start, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			return r.Payload, start.UTC(), true, nil
		}
	}
	return r.Payload, time.Time{}, true, errors.New("invalid native start")
}

// ValidateIdentity validates thread and locator shapes, never equating their IDs.
// A different rollout ID requires related-history support before capture.
func (m CodexMeta) ValidateIdentity(path string) bool {
	return uuid.MatchString(m.ID) && RolloutID(path) != "" && (m.SessionID == "" || uuid.MatchString(m.SessionID)) && filepath.IsAbs(m.Cwd) && len(m.Cwd) <= 4096
}

func present(v json.RawMessage) bool { return len(v) > 0 && string(v) != "null" }

// CaptureOutcome classifies a physical source without granting capture consent.
func (m CodexMeta) CaptureOutcome(path string) Outcome {
	_, outcome := m.Identity(path)
	if outcome != "" {
		return outcome
	}
	if outcome := m.Classification(); outcome != NativeFormat {
		return outcome
	}
	if !strings.EqualFold(RolloutID(path), m.ID) {
		return RelatedHistoryPending
	}
	return NativeFormat
}

// Classification distinguishes understood relationships awaiting complete capture
// from invalid metadata and unknown execution/history shapes.
func (m CodexMeta) Classification() Outcome {
	facts, outcome := m.Relationships()
	if outcome != "" {
		return outcome
	}
	if facts.Child {
		return ChildHistoryPending
	}
	if facts.ForkID != "" {
		return ForkHistoryPending
	}
	if facts.HistoryBase != nil {
		return RelatedHistoryPending
	}
	return m.FormatOutcome()
}

// FormatOutcome validates a native execution format independently of relationships
// and consent. Complete readers still validate all physical dependencies.
func (m CodexMeta) FormatOutcome() Outcome {
	facts, outcome := m.Relationships()
	if outcome != "" {
		return outcome
	}
	if present(m.ThreadSource) {
		var thread codexThreadSource
		_ = json.Unmarshal(m.ThreadSource, &thread)
		if !supportedThreadSource(thread, facts.Child) {
			return UnsupportedExecution
		}
	}
	if m.HistoryMode != "" && m.HistoryMode != CodexHistoryLegacy && m.HistoryMode != CodexHistoryPaginated {
		return UnsupportedHistory
	}
	if !m.LocalExecutionSource() {
		return UnsupportedExecution
	}
	if !ValidCodexVersion(m.Version) || strings.TrimSpace(m.Originator) == "" || len(m.Originator) > 256 || strings.ContainsFunc(m.Originator, unicode.IsControl) {
		return UnsupportedProducer
	}
	return NativeFormat
}

// supportedThreadSource admits a subagent source only for a child thread and
// an agent-created one only for a top-level thread, whose identity the
// relationship checks validate as they do a user thread's.
func supportedThreadSource(thread codexThreadSource, child bool) bool {
	switch thread {
	case threadUser:
		return true
	case threadSubagent:
		return child
	case threadAgentCreated:
		return !child
	}
	return false
}

// ValidCodexVersion bounds the recorded diagnostic token without interpreting
// release order. Prereleases, development builds and unknown versions qualify.
func ValidCodexVersion(version string) bool {
	if version == "" || len(version) > 128 {
		return false
	}
	for _, ch := range version {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '.' && ch != '-' && ch != '+' && ch != '_' {
			return false
		}
	}
	return true
}

// LocalExecutionSource recognizes supported local source format tags only;
// it does not establish local originating execution or producer support.
func (m CodexMeta) LocalExecutionSource() bool {
	var source string
	if json.Unmarshal(m.Source, &source) == nil {
		return ValidCodexExecutionSource(source)
	}
	facts, outcome := m.Relationships()
	if outcome != "" || !facts.Child {
		return false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(m.Source, &object) != nil {
		return false
	}
	sub, ok := object["subagent"]
	if !ok {
		return false
	}
	var kind codexSubagentKind
	if json.Unmarshal(sub, &kind) == nil {
		return kind == subagentReview || kind == subagentCompact || kind == subagentMemory
	}
	var child map[string]json.RawMessage
	if json.Unmarshal(sub, &child) != nil {
		return false
	}
	_, ok = child["thread_spawn"]
	return ok
}

// ValidCodexExecutionSource recognizes supported local source format tags.
func ValidCodexExecutionSource(value string) bool {
	source := codexExecutionSource(value)
	return source == executionCLI || source == executionExec || source == executionVSCode
}

// NativeFirstTask checks the first task_started, distinguishing Codex's built
// in migrated-history events. Callers must not skip a rejected first event.
func NativeFirstTask(line []byte) (seen, native bool) {
	var envelope struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(line, &envelope) != nil {
		return true, false
	}
	if envelope.Type != "event_msg" {
		return false, false
	}
	var kind struct {
		Type codexTaskEvent `json:"type"`
	}
	if json.Unmarshal(envelope.Payload, &kind) != nil || kind.Type == "" {
		return true, false
	}
	if kind.Type != taskStarted && kind.Type != turnStarted {
		return false, false
	}
	// This IS the first start even when its identity fields have invalid JSON
	// types. A later native-looking resume must never repair inherited history.
	var task struct {
		TurnID     string          `json:"turn_id"`
		RootTurnID *string         `json:"root_turn_id"`
		StartedAt  json.RawMessage `json:"started_at"`
	}
	if json.Unmarshal(envelope.Payload, &task) != nil {
		return true, false
	}
	return true, uuid.MatchString(task.TurnID) && (task.RootTurnID == nil || (uuid.MatchString(*task.RootTurnID) && strings.EqualFold(*task.RootTurnID, task.TurnID))) && !taskStartedAt(task.StartedAt).IsZero()
}

func taskStartedAt(raw json.RawMessage) time.Time {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		at, _ := time.Parse(time.RFC3339Nano, text)
		return at
	}
	seconds, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || seconds <= 0 || seconds > 253402300799 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

// FirstTaskAt returns only the first start's timestamp, without keeping body.
func FirstTaskAt(line []byte) time.Time {
	var r struct {
		Payload struct {
			StartedAt json.RawMessage `json:"started_at"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &r) != nil {
		return time.Time{}
	}
	return taskStartedAt(r.Payload.StartedAt)
}

// ExecutionSourceFacts projects only known execution-format fields. Unknown
// additive metadata never enters a catalog or durable producer-source binding.
// The result describes a recorded native shape, never originating permission.
func (m CodexMeta) ExecutionSourceFacts() (json.RawMessage, bool) {
	if !m.LocalExecutionSource() {
		return nil, false
	}
	var scalar string
	if json.Unmarshal(m.Source, &scalar) == nil {
		raw, err := json.Marshal(scalar)
		return raw, err == nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(m.Source, &object) != nil {
		return nil, false
	}
	sub := object["subagent"]
	var kind string
	if json.Unmarshal(sub, &kind) == nil {
		raw, err := json.Marshal(map[string]string{"subagent": kind})
		return raw, err == nil
	}
	var child struct {
		Spawn struct {
			Parent string `json:"parent_thread_id"`
			Depth  int32  `json:"depth"`
		} `json:"thread_spawn"`
	}
	if json.Unmarshal(sub, &child) != nil {
		return nil, false
	}
	raw, err := json.Marshal(map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": child.Spawn.Parent, "depth": child.Spawn.Depth}}})
	return raw, err == nil
}
