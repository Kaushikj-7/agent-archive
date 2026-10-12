package archive

import (
	"cmp"
	"encoding/json"
	"slices"
	"time"
)

// EvalExportSchemaVersion is the schema_version of every eval export record
// (schemas/eval-export.schema.json). Adding an optional field keeps it; a
// field removed, renamed, or given a new meaning bumps it.
const EvalExportSchemaVersion = 1

// EvalExportDetail is how much of a session an export record carries.
type EvalExportDetail string

const (
	// EvalExportDetailMetadata is identity, commits, counts, tools, and
	// outcome: no conversation text. For an archived session it is read from
	// the metadata sidecar alone.
	EvalExportDetailMetadata EvalExportDetail = "metadata"
	// EvalExportDetailFull adds the human prompts, the final response, the
	// edited files, and explicit feedback, read from the filtered source.
	EvalExportDetailFull EvalExportDetail = "full"
)

// EvalExportSource is where an export record's session was read from.
type EvalExportSource string

const (
	// EvalExportSourceArchive is the bucket: a metadata sidecar and, for
	// full detail, its verified source bundle.
	EvalExportSourceArchive EvalExportSource = "archive"
	// EvalExportSourceLocal is a transcript file on this machine, filtered
	// as the collector filters it, with no setup.
	EvalExportSourceLocal EvalExportSource = "local"
)

// Record kinds: every line of an export is one of these.
const (
	evalRecordSession = "session"
	evalRecordError   = "error"
)

// EvalExport is one session as an external evaluation tool reads it: what
// the metadata says about it and, at full detail, what the person asked, in
// order, and how the agent finished. It is built only from filtered data (a
// metadata sidecar, a filtered source bundle), so it carries nothing the
// archive does not already hold. See dev/specs/eval-export.md.
type EvalExport struct {
	RevisionID    string           `json:"revision_id,omitempty"`
	SchemaVersion int              `json:"schema_version"`
	Record        string           `json:"record"`
	Source        EvalExportSource `json:"source"`
	Detail        EvalExportDetail `json:"detail"`
	// SessionID is the archive session ID for an archived session, and the
	// app's own session ID for a local transcript.
	SessionID       string `json:"session_id"`
	NativeSessionID string `json:"native_session_id,omitempty"`
	// TranscriptPath is the local transcript's absolute path; absent for
	// an archived session.
	TranscriptPath  string          `json:"transcript_path,omitempty"`
	MachineID       string          `json:"machine_id,omitempty"`
	ParentSessionID string          `json:"parent_session_id,omitempty"`
	Harness         Harness         `json:"harness"`
	Project         EvalProject     `json:"project"`
	Branch          string          `json:"branch,omitempty"`
	GitHead         *SessionGitHead `json:"git_head,omitempty"`
	Replay          *Replay         `json:"replay,omitempty"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	EndedAt         *time.Time      `json:"ended_at,omitempty"`
	CapturedAt      *time.Time      `json:"captured_at,omitempty"`
	State           MetadataState   `json:"state,omitempty"`
	TurnOutcome     TurnOutcome     `json:"turn_outcome,omitempty"`
	Parser          ParserInfo      `json:"parser"`
	FilterVersion   string          `json:"filter_version"`
	Models          []ModelSummary  `json:"models,omitempty"`
	Counts          Counts          `json:"counts"`
	ModelTokens     []ModelTokens   `json:"model_tokens,omitempty"`
	ToolsUsed       []ToolUsage     `json:"tools_used,omitempty"`
	MCPCalls        []ToolUsage     `json:"mcp_calls,omitempty"`
	SkillsUsed      []SkillUse      `json:"skills_used,omitempty"`
	GitActivity     []GitEvent      `json:"git_activity,omitempty"`
	CaptureGaps     []CaptureGap    `json:"capture_gaps,omitempty"`

	// Full detail only. Prompts and FilesEdited are present (possibly
	// empty) at full detail and absent at metadata detail.
	Prompts       *[]EvalPrompt      `json:"prompts,omitempty"`
	FinalResponse *EvalFinalResponse `json:"final_response,omitempty"`
	FilesEdited   *[]string          `json:"files_edited,omitempty"`
	Feedback      []EvalFeedback     `json:"feedback,omitempty"`
	Trimmed       *EvalTrimmed       `json:"trimmed,omitempty"`
}

// EvalProject names the project without its path: the project root's base
// name and the repository key. Root is set only for a local transcript,
// whose path never leaves the machine.
type EvalProject struct {
	Name    string `json:"name,omitempty"`
	RepoKey string `json:"repo_key,omitempty"`
	Root    string `json:"root,omitempty"`
}

// EvalPrompt is one human prompt, as filtered, in session order.
type EvalPrompt struct {
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
	// Truncated is set when the record's size bound cut Text.
	Truncated bool `json:"truncated,omitempty"`
}

// EvalFinalResponse is the agent's last reply: the last assistant text in
// the transcript or, when the transcript has none, the last final message a
// stop hook reported.
type EvalFinalResponse struct {
	Text      string `json:"text"`
	Source    string `json:"source"`
	Truncated bool   `json:"truncated,omitempty"`
}

// EvalFeedback is one explicit feedback note (`agent-archive feedback`), as
// filtered and archived with the session.
type EvalFeedback struct {
	Text       string    `json:"text"`
	ObservedAt time.Time `json:"observed_at"`
	Truncated  bool      `json:"truncated,omitempty"`
}

// EvalTrimmed says what FitEvalExport cut to bring the record under its
// bound. ExceedsMaxBytes is set when even the cut record is larger: prompts
// are shortened, never dropped.
type EvalTrimmed struct {
	MaxBytes int `json:"max_bytes"`
	// TextsTruncated counts the prompts, final response, and feedback notes
	// that were cut (each is also marked truncated).
	TextsTruncated int `json:"texts_truncated"`
	// FilesEditedOmitted counts the edited files dropped from the end of
	// files_edited.
	FilesEditedOmitted int  `json:"files_edited_omitted,omitempty"`
	ExceedsMaxBytes    bool `json:"exceeds_max_bytes,omitempty"`
}

// EvalExportError is the record written in place of a session that could
// not be exported, so one bad session never fails a batch.
type EvalExportError struct {
	SchemaVersion  int              `json:"schema_version"`
	Record         string           `json:"record"`
	Source         EvalExportSource `json:"source,omitempty"`
	SessionID      string           `json:"session_id,omitempty"`
	TranscriptPath string           `json:"transcript_path,omitempty"`
	// Input is what was asked for, as given: a session ID or a path.
	Input string        `json:"input"`
	Error EvalErrorInfo `json:"error"`
}

// EvalErrorCode says why a session could not be exported.
type EvalErrorCode string

// EvalErrorCode values. A reader must accept others, which a later release
// may add.
const (
	// EvalErrorNotFound: no session with that ID (or no such file).
	EvalErrorNotFound EvalErrorCode = "not_found"
	// EvalErrorAmbiguous: the ID prefix matches more than one session.
	EvalErrorAmbiguous EvalErrorCode = "ambiguous"
	// EvalErrorReadFailed: the metadata, source, or file could not be read
	// or verified.
	EvalErrorReadFailed EvalErrorCode = "read_failed"
	// EvalErrorParseFailed: the filtered source could not be parsed into
	// prompts and a final response.
	EvalErrorParseFailed EvalErrorCode = "parse_failed"
	// EvalErrorUnknownHarness: a transcript path is not in a folder any app
	// keeps its transcripts in, and no --harness said which app wrote it.
	EvalErrorUnknownHarness EvalErrorCode = "unknown_harness"
)

// EvalErrorInfo is an error record's reason: a stable code and a message for
// people. The message names no transcript content.
type EvalErrorInfo struct {
	Code    EvalErrorCode `json:"code"`
	Message string        `json:"message"`
}

// NewEvalExportError builds an error record.
func NewEvalExportError(source EvalExportSource, input string, code EvalErrorCode, message string) EvalExportError {
	return EvalExportError{SchemaVersion: EvalExportSchemaVersion, Record: evalRecordError, Source: source, Input: input, Error: EvalErrorInfo{Code: code, Message: message}}
}

// EvalExportFromMetadata is the metadata-detail record of a session: every
// field comes from m, and nothing from its source. transcriptPath and
// projectRoot are set for a local transcript only.
func EvalExportFromMetadata(m Metadata, source EvalExportSource) EvalExport {
	var startedAt, capturedAt *time.Time
	var machineID string
	if !m.StartedAt.IsZero() {
		startedAt = new(m.StartedAt.UTC())
	}
	// Where a session was captured, and when, describe the archive's copy.
	if source == EvalExportSourceArchive {
		machineID = m.MachineID
		if !m.CapturedAt.IsZero() {
			capturedAt = new(m.CapturedAt.UTC())
		}
	}
	return EvalExport{
		SchemaVersion: EvalExportSchemaVersion, Record: evalRecordSession, Source: source, Detail: EvalExportDetailMetadata,
		SessionID: m.SessionID, NativeSessionID: m.NativeSessionID, ParentSessionID: m.ParentSessionID,
		Harness: m.Harness, Project: EvalProject{Name: m.ProjectName, RepoKey: m.RepoKey},
		GitHead: m.GitHead, Replay: m.Replay, EndedAt: m.EndedAt,
		State: m.State, TurnOutcome: m.TurnOutcome, Parser: m.Parser, FilterVersion: m.FilterVersion,
		Models: m.Models, Counts: m.Counts, ModelTokens: m.ModelTokens, ToolsUsed: m.ToolsUsed, MCPCalls: m.MCPCalls,
		SkillsUsed: m.SkillsUsed, GitActivity: m.GitActivity, CaptureGaps: m.CaptureGaps,
		StartedAt: startedAt, CapturedAt: capturedAt, MachineID: machineID,
	}
}

// BuildEvalExportWithAnalysis is the record of a session at detail, from its metadata and
// its filtered source bundle. At metadata detail the bundle adds only the
// branch; at full detail it adds the prompts, the final response, the edited
// files, and explicit feedback. The caller supplies successful analysis of
// the filtered source; this builder never resolves a native parser.
func BuildEvalExportWithAnalysis(bundle SourceBundle, analysis Analysis, m Metadata, source EvalExportSource, detail EvalExportDetail, parser ParserInfo) (EvalExport, error) {
	e := EvalExportFromMetadata(m, source)
	e.Detail = detail
	e.Branch = validBranch(analysis.Facts.FirstBranch)
	if e.Branch == "HEAD" {
		e.Branch = ""
	}
	if detail != EvalExportDetailFull {
		return e, nil
	}
	if err := validateBundle(bundle); err != nil {
		return EvalExport{}, &ParseError{Reason: err.Error()}
	}
	if err := e.rederiveWithAnalysis(bundle, analysis, m, parser); err != nil {
		return EvalExport{}, err
	}
	view := analysis.View
	prompts := []EvalPrompt{}
	var last *EvalFinalResponse
	for _, turn := range view.Turns {
		// Every other kind (tool results, harness-written records) is
		// neither a prompt nor a reply.
		if turn.Kind == TurnKindHumanPrompt {
			// As the person typed it: without Cursor's wrapper, and a
			// Claude Code slash command as its command line.
			text := turnDisplayText(turn)
			if !analysis.Facts.TextOnly || text != "" {
				prompts = append(prompts, EvalPrompt{Text: text, Timestamp: turn.Timestamp})
			}
		} else if turn.Kind == TurnKindAssistant && turn.Text != "" {
			last = &EvalFinalResponse{Text: turn.Text, Source: "transcript"}
		}
	}
	if last == nil {
		last = lastHookFinal(bundle)
	}
	files := []string{}
	if len(bundle.NativeRecords) > 0 {
		files = append(files, sessionFilesTouched(view.ToolCalls, analysis.Facts.WorkspaceRoot)...)
	}
	e.Prompts, e.FinalResponse, e.FilesEdited = &prompts, last, &files
	e.Feedback = explicitFeedback(bundle.SupplementalEvidence)
	return e, nil
}

// rederive replaces what a parser derives with this build's parse of the
// bundle when the sidecar was written by another parser version, so the
// record's counts, tools, and parser describe the same parse as its
// prompts and edited files. What the registration contributed (identity,
// project, commits, replay, admission gaps) stays the sidecar's.
func (e *EvalExport) rederiveWithAnalysis(bundle SourceBundle, analysis Analysis, m Metadata, parser ParserInfo) error {
	parser = defaultMetadataParser(bundle, parser)
	if m.Parser.Version == parser.Version {
		return nil
	}
	derived, err := BuildMetadataWithAnalysis(bundle, analysis, nil, m.MachineID, m.StartedAt, m.MetadataDerivedAt, m.SourceBundle, parser)
	if err != nil && !IsParseError(err) {
		// Inputs an old sidecar lacks (a derivation time, say): keep
		// what it says rather than fail a record whose source parses.
		return nil
	}
	if err != nil {
		return err
	}
	e.EndedAt, e.State, e.TurnOutcome, e.Parser = derived.EndedAt, derived.State, derived.TurnOutcome, derived.Parser
	e.Models, e.Counts, e.ModelTokens, e.ToolsUsed, e.MCPCalls = derived.Models, derived.Counts, derived.ModelTokens, derived.ToolsUsed, derived.MCPCalls
	e.SkillsUsed, e.GitActivity, e.CaptureGaps = derived.SkillsUsed, derived.GitActivity, derived.CaptureGaps
	for _, gap := range m.CaptureGaps {
		if slices.Contains([]string{CaptureGapImportedWithoutHookEvidence, CaptureGapDiscoveredWithoutHookEvidence}, gap.Code) && !slices.ContainsFunc(e.CaptureGaps, func(g CaptureGap) bool { return g.Code == gap.Code }) {
			e.CaptureGaps = append(e.CaptureGaps, gap)
		}
	}
	return nil
}

// lastHookFinal is the text of the last final message a stop hook reported,
// or nil. In a parent session's bundle, a final carrying an agent_id is a
// subagent's (HookFinalStatusSeparateSubagent), not the session's answer.
func lastHookFinal(bundle SourceBundle) *EvalFinalResponse {
	evidence := bundle.SupplementalEvidence
	for i := len(evidence) - 1; i >= 0; i-- {
		if evidence[i].Kind != EvidenceKindFinalResponse {
			continue
		}
		if bundle.ParentSessionID == "" && firstString(evidence[i].Payload, "agent_id") != "" {
			continue
		}
		if text, _ := evidence[i].Payload["text"].(string); text != "" {
			return &EvalFinalResponse{Text: text, Source: "hook"}
		}
	}
	return nil
}

// explicitFeedback lists the session's explicit feedback notes in order.
func explicitFeedback(evidence []SupplementalEvidence) []EvalFeedback {
	var notes []EvalFeedback
	for _, e := range evidence {
		if e.Kind != EvidenceKindExplicitFeedback {
			continue
		}
		if text, _ := e.Payload["text"].(string); text != "" {
			notes = append(notes, EvalFeedback{Text: text, ObservedAt: e.ObservedAt.UTC()})
		}
	}
	return notes
}

// minEvalText is the least FitEvalExport cuts a text to.
const minEvalText = 256

// evalTrimmedHeadroom is what FitEvalExport keeps free for the digits of the
// trimmed counts, which grow as it cuts.
const evalTrimmedHeadroom = 32

// FitEvalExport brings e's JSON encoding to at most maxBytes (0: no bound).
// It cuts the longest text among the prompts, the final response, and the
// feedback in half, repeatedly, never below minEvalText, and then drops edited
// files from the end of the list. Prompts are never dropped: when that is
// still not enough, the record is returned over its bound with
// Trimmed.ExceedsMaxBytes set. Trimmed is set whenever anything was cut.
func FitEvalExport(e EvalExport, maxBytes int) EvalExport {
	if maxBytes <= 0 || evalSize(e) <= maxBytes {
		return e
	}
	e = cloneEvalTexts(e)
	trimmed := &EvalTrimmed{MaxBytes: maxBytes}
	e.Trimmed = trimmed
	// The size is kept up to date from each change's own encoding rather
	// than by encoding the whole record again. Only the trimmed counts'
	// own digits can drift, which the headroom covers; the record is
	// measured again between stages and at the end.
	target := max(maxBytes-evalTrimmedHeadroom, 0)
	size := evalSize(e)
	for size > target {
		text, truncated := longestEvalText(&e)
		if text == nil || len(*text) <= minEvalText {
			break
		}
		before := jsonLen(*text)
		cut := max(minEvalText, len(*text)/2)
		// Round the floor up to a whole rune, so the 256-byte promise holds.
		for cut < len(*text) && !isRuneStart((*text)[cut]) {
			cut++
		}
		if cut == len(*text) {
			break
		}
		*text = (*text)[:cut]
		size -= before - jsonLen(*text)
		if !*truncated {
			*truncated = true
			trimmed.TextsTruncated++
			size += len(`,"truncated":true`)
		}
	}
	size = evalSize(e)
	if e.FilesEdited != nil {
		files := *e.FilesEdited
		for size > target && len(files) > 0 {
			size -= jsonLen(files[len(files)-1]) + 1
			files = files[:len(files)-1]
			trimmed.FilesEditedOmitted++
		}
		e.FilesEdited = &files
	}
	trimmed.ExceedsMaxBytes = evalSize(e) > maxBytes
	return e
}

// cloneEvalTexts copies the slices FitEvalExport changes, so the caller's
// record is left alone.
func cloneEvalTexts(e EvalExport) EvalExport {
	if e.Prompts != nil {
		e.Prompts = new(slices.Clone(*e.Prompts))
	}
	if e.FilesEdited != nil {
		e.FilesEdited = new(slices.Clone(*e.FilesEdited))
	}
	if e.FinalResponse != nil {
		e.FinalResponse = new(*e.FinalResponse)
	}
	e.Feedback = slices.Clone(e.Feedback)
	return e
}

// longestEvalText is the longest text FitEvalExport may cut, with its
// truncated flag; nil when there is none.
func longestEvalText(e *EvalExport) (text *string, truncated *bool) {
	consider := func(t *string, flag *bool) {
		// A floor ending inside the last rune cannot be shortened. Skip it
		// so another field can still be fitted.
		floor := minEvalText
		for floor < len(*t) && !isRuneStart((*t)[floor]) {
			floor++
		}
		if len(*t) <= floor {
			return
		}
		if text == nil || len(*t) > len(*text) {
			text, truncated = t, flag
		}
	}
	if e.Prompts != nil {
		for i := range *e.Prompts {
			p := &(*e.Prompts)[i]
			consider(&p.Text, &p.Truncated)
		}
	}
	if e.FinalResponse != nil {
		consider(&e.FinalResponse.Text, &e.FinalResponse.Truncated)
	}
	for i := range e.Feedback {
		consider(&e.Feedback[i].Text, &e.Feedback[i].Truncated)
	}
	return text, truncated
}

// evalSize is the length of e's display-safe JSON encoding.
func evalSize(e EvalExport) int {
	encoded, err := json.Marshal(e)
	if err != nil {
		return 0
	}
	return len(DisplayJSON(encoded))
}

// jsonLen is the length of s encoded as a display-safe JSON string.
func jsonLen(s string) int {
	encoded, err := json.Marshal(s)
	if err != nil {
		return 0
	}
	return len(DisplayJSON(encoded))
}

// LocalTranscript describes a transcript file on this machine for
// BuildLocalEvalExportWithAnalysis.
type LocalTranscript struct {
	// Path is the transcript's absolute path.
	Path string
	// ProjectRoot is the project folder the session belongs to, as backfill
	// resolved it; "" means the first working directory the transcript
	// records.
	ProjectRoot string
	// StartedAt is when the session started, when something other than its
	// records says (backfill's start for a Cursor transcript, whose records
	// carry no time); zero when nothing does.
	StartedAt time.Time
	// Now is the time the export is made.
	Now time.Time
}

// localMachineID stands in for the machine ID metadata derivation requires; a local
// record carries no machine ID.
const localMachineID = "local"

// BuildLocalEvalExportWithAnalysis is the record of a transcript file, filtered as the
// collector filters it (bundle), with no setup: its metadata is derived with
// the running parser, exactly as a first publication would derive it. Where a
// local record differs from an archived one, it says less rather than guess:
// no git_head, replay, feedback, machine ID, or capture time, and no start
// time unless a record or local.StartedAt gives one.
func BuildLocalEvalExportWithAnalysis(bundle SourceBundle, analysis Analysis, parseErr error, local LocalTranscript, detail EvalExportDetail, parser ParserInfo) (EvalExport, error) {
	bundle.SupplementalEvidence = nil
	startedAt := local.StartedAt
	if !analysis.Facts.NativeStartedAt.IsZero() {
		startedAt = analysis.Facts.NativeStartedAt
	}
	if startedAt.IsZero() {
		startedAt = analysis.Facts.EarliestRecordAt
	}
	// Metadata derivation needs a start and a reference to the bundle it
	// summarizes. The reference is the bundle's own content address; it is
	// never uploaded or exported.
	derivationStart := startedAt
	if derivationStart.IsZero() {
		derivationStart = local.Now
	}
	compressed, err := BuildCompressedSource(bundle)
	if err != nil {
		return EvalExport{}, err
	}
	key, err := SourceObjectKey(bundle, compressed.SHA256)
	if err != nil {
		return EvalExport{}, err
	}
	reference := SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
	metadata, err := BuildMetadataWithAnalysis(bundle, analysis, parseErr, localMachineID, derivationStart, local.Now, reference, parser)
	if err != nil {
		return EvalExport{}, err
	}
	root := local.ProjectRoot
	if root == "" {
		root = analysis.Facts.WorkspaceRoot
	}
	metadata.ApplyProjectName(root)
	record, err := BuildEvalExportWithAnalysis(bundle, analysis, metadata, EvalExportSourceLocal, detail, parser)
	if err != nil {
		return EvalExport{}, err
	}
	record.SessionID = cmp.Or(bundle.NativeSessionID, metadata.SessionID)
	record.TranscriptPath = local.Path
	record.Project.Root = root
	if startedAt.IsZero() {
		record.StartedAt = nil
	}
	return record, nil
}
