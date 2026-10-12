package nativecodec

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"io"
	"slices"
	"sort"
	"strings"
	"time"
)

// CodexAdapter supports the conservative JSONL shapes observed by this
// foundation. Unsupported Codex record types are gaps, never pass-through.
type CodexAdapter struct{}

// Name returns the canonical harness name, "codex".
func (CodexAdapter) Name() string { return "codex" }

// Version returns the adapter version shared by every adapter.
func (CodexAdapter) Version() string { return adapterVersion }

// FilterJSONL keeps only the Codex record types this adapter recognizes,
// through the shared privacy filter, and labels the result codex-jsonl.
func (CodexAdapter) FilterJSONL(r io.Reader) (archive.FilteredTranscript, error) {
	return filterJSONL(r, "codex-jsonl", map[string]bool{
		"session_meta": true, "turn_context": true, "response_item": true,
		"event_msg": true, "message": true, "token_usage_record": true,
	}, nil)
}

// ClaudeAdapter handles a small, explicit subset of Claude Code JSONL event
// types. It does not claim schema coverage for every installed version.
type ClaudeAdapter struct{}

// Name returns the canonical harness name, "claude".
func (ClaudeAdapter) Name() string { return "claude" }

// Version returns the adapter version shared by every adapter.
func (ClaudeAdapter) Version() string { return adapterVersion }

// FilterJSONL keeps only the Claude Code record types this adapter recognizes,
// through the shared privacy filter, and labels the result claude-jsonl.
func (ClaudeAdapter) FilterJSONL(r io.Reader) (archive.FilteredTranscript, error) {
	return filterClaudeJSONL(r, nil)
}

// FilterClaudeRetainedBounded applies the ordinary privacy codec while refusing
// retained output beyond the caller's allowance before accumulating it.
func FilterClaudeRetainedBounded(r io.Reader, boundary archive.CaptureBoundary) (archive.FilteredTranscript, error) {
	return filterJSONL(r, "claude-jsonl", map[string]bool{"user": true, "assistant": true, "tool_use": true, "tool_result": true, "message": true, "summary": true}, nil, boundary)
}

// FilterCursorRetainedBounded is the equivalent ordinary Cursor record codec.
func FilterCursorRetainedBounded(r io.Reader, boundary archive.CaptureBoundary) (archive.FilteredTranscript, error) {
	return filterJSONL(r, "cursor-jsonl", map[string]bool{"session": true, "message": true, "tool_call": true, "tool_result": true, "event": true, "turn_ended": true}, nil, boundary)
}

// FilterSubagentJSONL is FilterJSONL for a Claude Code subagent's transcript
// with the contents of its sibling agent-<id>.meta.json (see
// archive.SubagentMetaPath). The description in it, redacted and bounded as prompt
// text is, becomes one subagent-meta record at the front of the filtered
// records; nothing else of the file is kept. metaJSON that is empty,
// oversized, not a JSON object, or without a non-blank string description
// changes nothing and records no gap: the file is optional. The record is
// written only when the transcript has records of its own, so a transcript
// that is still empty stays empty, and it never makes an unrecognized
// transcript acceptable.
func (ClaudeAdapter) FilterSubagentJSONL(r io.Reader, metaJSON []byte) (archive.FilteredTranscript, error) {
	return filterClaudeJSONL(r, subagentMetaLead(metaJSON))
}

// filterClaudeJSONL is the Claude Code filter, with lead, when not nil, a
// subagent-meta record to write first.
func filterClaudeJSONL(r io.Reader, lead map[string]any) (archive.FilteredTranscript, error) {
	return filterJSONL(r, "claude-jsonl", map[string]bool{
		"user": true, "assistant": true, "tool_use": true, "tool_result": true,
		"message": true, "summary": true,
	}, lead)
}

// CursorAdapter filters hook-provided JSONL records, a hook-provided text
// transcript (FilterText), and a chat read from Cursor's database
// (FilterComposer, in cursor_composer.go). Undocumented formats remain
// unsupported capture gaps upstream.
type CursorAdapter struct{}

// Name returns the canonical harness name, "cursor".
func (CursorAdapter) Name() string { return "cursor" }

// Version returns the adapter version shared by every adapter.
func (CursorAdapter) Version() string { return adapterVersion }

// FilterJSONL keeps only the Cursor record types this adapter recognizes,
// through the shared privacy filter, and labels the result cursor-jsonl.
func (CursorAdapter) FilterJSONL(r io.Reader) (archive.FilteredTranscript, error) {
	return filterJSONL(r, "cursor-jsonl", map[string]bool{
		"session": true, "message": true, "tool_call": true, "tool_result": true,
		"event": true, "turn_ended": true,
	}, nil)
}

// textRole is the lower-case role name of a Cursor text transcript section
// header, such as "user" in "user: fix the build".
type textRole string

// The visible textRole values: the sections FilterText retains.
const (
	textRoleUser      textRole = "user"
	textRoleAssistant textRole = "assistant"
	textRoleTool      textRole = "tool"
)

// The hidden textRole values: the sections FilterText omits.
const (
	textRoleSystem    textRole = "system"
	textRoleDeveloper textRole = "developer"
	textRoleThinking  textRole = "thinking"
	textRoleAnalysis  textRole = "analysis"
)

// visibleTextRoles and hiddenTextRoles are the role headers of a Cursor
// text transcript: a visible section is retained, a hidden one omitted.
var (
	visibleTextRoles = map[textRole]bool{textRoleUser: true, textRoleAssistant: true, textRoleTool: true}
	hiddenTextRoles  = map[textRole]bool{textRoleSystem: true, textRoleDeveloper: true, textRoleThinking: true, textRoleAnalysis: true}
)

// textHeaderCase is how a Cursor text transcript writes its role headers:
// `user:` or `User:`. The real format is not pinned by a fixture, so both
// are read, but one transcript uses one: the case of its first header (see
// textHeaderCaseOf) is the only one that starts a section anywhere in it.
type textHeaderCase int

const (
	// textHeaderLower is a lower-case header: `user:`, `assistant:`.
	textHeaderLower textHeaderCase = iota
	// textHeaderTitle is a capitalized header: `User:`, `Assistant:`.
	textHeaderTitle
)

// textRoleHeader reports whether line has the shape of a role header of a
// Cursor text transcript written in headerCase, and returns the role and
// the text after the header. A header is a role name written exactly in
// that case and a colon at column 0, then a space or the end of the line.
// An indented "user:" is content (a YAML key in tool output), and so, since
// filter 11, is a header in the other case: in a lower-case transcript,
// prose such as "archive.Analysis: the bug is …" or "System: linux" at the start of
// a line no longer hides what follows it, and in a capitalized one a YAML
// `user:` line no longer starts a Person turn. Whether a line with this
// shape really starts a section also depends on the lines around it; see
// parseTextSections.
func textRoleHeader(line string, headerCase textHeaderCase) (role textRole, rest string, ok bool) {
	line = strings.TrimSuffix(line, "\r")
	colon := strings.IndexByte(line, ':')
	if colon <= 0 {
		return "", "", false
	}
	name := line[:colon]
	role, rest = textRole(strings.ToLower(name)), line[colon+1:]
	if !visibleTextRoles[role] && !hiddenTextRoles[role] {
		return "", "", false
	}
	want := string(role)
	if headerCase == textHeaderTitle {
		want = strings.ToUpper(want[:1]) + want[1:]
	}
	if name != want {
		return "", "", false
	}
	if rest != "" && rest[0] != ' ' {
		return "", "", false
	}
	return role, strings.TrimPrefix(rest, " "), true
}

// textHeaderCaseOf returns the header case of a Cursor text transcript: the
// case of its first non-blank line when that is a header in either case. A
// transcript whose first non-blank line is no header is refused by
// parseTextSections whatever the case.
func textHeaderCaseOf(content string) textHeaderCase {
	for line := range strings.SplitSeq(content, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if _, _, ok := textRoleHeader(line, textHeaderTitle); ok {
			return textHeaderTitle
		}
		break
	}
	return textHeaderLower
}

// indentHeaderShapedLines indents by one space every line of a sanitized
// section, after its first, that has a role header's shape in the
// transcript's header case. Such a line was content in the transcript
// (parseTextSections decided so), or sanitizing brought it to the start of
// a line (stripping an injected block can); the handoff reads the retained
// text back with the same parser, and the indent keeps the line content
// there too, so a line of a tool's output can never become a Person turn or
// hide the rest.
func indentHeaderShapedLines(section string, headerCase textHeaderCase) string {
	lines := strings.Split(section, "\n")
	changed := false
	for i := 1; i < len(lines); i++ {
		if _, _, header := textRoleHeader(lines[i], headerCase); header {
			lines[i], changed = " "+lines[i], true
		}
	}
	if !changed {
		return section
	}
	return strings.Join(lines, "\n")
}

// textSection is one role section of a Cursor text transcript: its role,
// the text after its header, and its lines, the header line first. Blank
// lines inside the section are kept; blank lines after its last line are
// not.
type textSection struct {
	role   textRole
	header string
	lines  []string
}

// textSections is a Cursor text transcript split into its role sections
// (see parseTextSections).
type textSections struct {
	sections []textSection
	// blankSeparated reports whether the transcript separates its sections
	// with a blank line.
	blankSeparated bool
	// headerCase is the transcript's header case (textHeaderCaseOf).
	headerCase textHeaderCase
}

// parseTextSections splits a Cursor text transcript into its role sections.
// FilterText and the handoff reader both use it, so the retained text is
// read back exactly as it was filtered. It returns ok false when non-blank
// text comes before the first header; that text is in no section.
//
// A line with a header's shape (textRoleHeader) at column 0, in the case of
// the transcript's first header (textHeaderCaseOf), starts a section, with
// one refinement. Cursor separates its sections with a blank
// line; when the transcript does (its second header follows a blank line),
// a visible header that does not follow a blank line is content, so a
// `user: …` line in the middle of a tool's output cannot start a Person
// turn. A hidden header (`system:`, `thinking:`, …) always starts a
// section, so text that might be a hidden section is never retained. A
// transcript whose second header does not follow a blank line is read as
// filter 10 read it, one header per line. blankSeparated reports which
// reading applied.
func parseTextSections(content string) (parsed textSections, ok bool) {
	headerCase := textHeaderCaseOf(content)
	var sections []textSection
	decided, blankSeparated, previousBlank, leading := false, false, false, false
	for line := range strings.SplitSeq(content, "\n") {
		blank := strings.TrimSpace(line) == ""
		role, rest, header := textRoleHeader(line, headerCase)
		if header && len(sections) > 0 {
			switch {
			case !decided:
				blankSeparated, decided = previousBlank, true
			case blankSeparated && !previousBlank && visibleTextRoles[role]:
				header = false
			}
		}
		previousBlank = blank
		switch {
		case header:
			sections = append(sections, textSection{role: role, header: rest, lines: []string{line}})
		case len(sections) > 0:
			sections[len(sections)-1].lines = append(sections[len(sections)-1].lines, line)
		case !blank:
			// Text before the first header belongs to no section.
			leading = true
		}
	}
	for i := range sections {
		lines := sections[i].lines
		for len(lines) > 1 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		sections[i].lines = lines
	}
	return textSections{sections: sections, blankSeparated: blankSeparated, headerCase: headerCase}, !leading
}

// FilterText retains a hook-provided Cursor text transcript only when the hook
// has established that this is a fresh eligible session. It labels the source
// as text rather than fabricating message events from unstructured content.
//
// The transcript as a whole is bounded by the record size limit (a text
// transcript is one unit, like one JSONL record); a longer one fails with
// archive.ErrRecordTooLarge, which the collector records as a capture gap. Filter 6
// and earlier stopped at 2 MB.
//
// Each visible role section — a `user:`, `assistant:`, or `tool:` line and the
// continuation lines under it — is sanitized on its own, so redaction and the
// 64 KB string cap apply per message, as they do to JSONL records. Filter 6
// sanitized the whole joined text as one string, which truncated any text
// transcript over 64 KB to its first 64 KB. The sanitized sections are joined
// again in their original order, one line per original line, so the result is
// read back by the same section prefixes (see textSectionPrefixes). Hidden
// sections (`system:`, `developer:`, `thinking:`, `analysis:`) and their
// continuation lines are omitted. Each gap is recorded once.
//
// The collector's rewrite guard compares the retained text by prefix across
// passes. Per-section sanitizing keeps every completed section's bytes
// stable, but the last section, if it is still being written, can change
// bytes it already produced once more of it lands (a credential that only
// matches when complete, or an injected block whose stripping trims the
// section's edges). That is a property of sanitizing a growing string, not
// of this function, and a text transcript offers no record boundary to stop
// short of.
func (CursorAdapter) FilterText(r io.Reader, freshStartedAt time.Time) (archive.FilteredTranscript, error) {
	if freshStartedAt.IsZero() {
		return archive.FilteredTranscript{}, &archive.FilterError{Reason: "cursor text transcript has no reliable fresh-session start"}
	}
	content, err := io.ReadAll(io.LimitReader(r, int64(maxRecordBytes)+1))
	if err != nil {
		return archive.FilteredTranscript{}, &archive.FilterError{Reason: "cursor text transcript cannot be read"}
	}
	if len(content) > maxRecordBytes {
		return archive.FilteredTranscript{}, archive.ErrRecordTooLarge
	}
	result := archive.FilteredTranscript{Format: "cursor-text", FirstEventAt: freshStartedAt.UTC()}
	gapSet := map[archive.CaptureGap]bool{}
	addGap := func(code, detail string) {
		gap := archive.CaptureGap{Code: code, Detail: detail}
		if !gapSet[gap] {
			gapSet[gap] = true
			result.Gaps = append(result.Gaps, gap)
		}
	}
	addGap("text_structure_partial", "Cursor role sections retained without manufactured events")

	parsed, ok := parseTextSections(string(content))
	if !ok {
		return archive.FilteredTranscript{}, &archive.FilterError{Reason: "cursor text transcript has unrecognized role section"}
	}
	// Keep each visible section's lines as they were, blank lines inside it
	// included (filter 10 dropped them). Sections are joined with a blank
	// line when the transcript separated them with one, so the retained
	// text is read back with the same rule.
	var sections [][]string
	hiddenSections, hiddenLines := 0, 0
	for _, section := range parsed.sections {
		if hiddenTextRoles[section.role] {
			hiddenSections++
			for _, line := range section.lines {
				if strings.TrimSpace(line) != "" {
					hiddenLines++
				}
			}
			continue
		}
		sections = append(sections, section.lines)
	}
	if hiddenSections > 0 {
		addGap("hidden_instruction_omitted", fmt.Sprintf("%d text sections omitted (%d lines)", hiddenSections, hiddenLines))
	}
	if len(sections) == 0 {
		return archive.FilteredTranscript{}, &archive.FilterError{Reason: "cursor text transcript has no retainable visible sections"}
	}
	state := archive.PrivacyState{AddGap: func(code string, _ int, detail string) { addGap(code, detail) }}
	retained := make([]string, 0, len(sections))
	for _, section := range sections {
		safe, keep := sanitizeValue(strings.Join(section, "\n"), &state)
		if !keep {
			continue
		}
		text, ok := safe.(string)
		if !ok {
			return archive.FilteredTranscript{}, &archive.FilterError{Reason: "cursor text transcript is not text"}
		}
		retained = append(retained, indentHeaderShapedLines(text, parsed.headerCase))
	}
	if len(retained) == 0 {
		return archive.FilteredTranscript{}, &archive.FilterError{Reason: "cursor text transcript has no retainable content"}
	}
	separator := "\n"
	if parsed.blankSeparated {
		separator = "\n\n"
	}
	text := strings.Join(retained, separator)
	result.Text = []string{text}
	result.Boundary.RetainedBytes = len(text)
	return result, nil
}

func filterJSONL(r io.Reader, format string, knownTypes map[string]bool, lead map[string]any, bounds ...archive.CaptureBoundary) (archive.FilteredTranscript, error) {
	return filterJSONLEncoded(r, format, knownTypes, lead, nil, nil, nil, bounds...)
}

func filterJSONLEncoded(r io.Reader, format string, knownTypes map[string]bool, lead map[string]any, validateMeta func([]byte) error, encoder func(map[string]any) ([]byte, error), beforeRecord func(int) (func(), error), bounds ...archive.CaptureBoundary) (archive.FilteredTranscript, error) {
	scanner := bufio.NewScanner(r)
	// Individual native JSONL records can contain tool output. A hard limit keeps
	// filtering bounded; exceeding it is refused rather than silently
	// truncated. The buffer holds a record plus its newline, so a record of
	// exactly maxRecordBytes is still read. bufio.Scanner allows the larger of
	// its maximum and the initial buffer's capacity, so the initial buffer
	// must not exceed the limit either.
	scanner.Buffer(make([]byte, min(64*1024, maxRecordBytes+1)), maxRecordBytes+1)
	return filterRecordsObserved(format, knownTypes, lead, func() ([]byte, bool) {
		if scanner.Scan() {
			return scanner.Bytes(), true
		}
		return nil, false
	}, scanner.Err, validateMeta, nil, nil, encoder, beforeRecord, bounds...)
}

func filterRecordsObserved(format string, knownTypes map[string]bool, lead map[string]any, next func() ([]byte, bool), readError func() error, validateMeta func([]byte) error, retained func(int), own func(string) bool, encoder func(map[string]any) ([]byte, error), beforeRecord func(int) (func(), error), bounds ...archive.CaptureBoundary) (archive.FilteredTranscript, error) {
	next, readError = boundRecordSource(next, readError, bounds)
	result := archive.FilteredTranscript{Format: format, NativeStartComplete: true}
	records := recordReservations{before: beforeRecord}
	defer records.end()
	lineNo, recognized := 0, 0
	gapSet := map[string]bool{}
	// Filter 2 collapsed every omission into one content-free gap, so a reader
	// could not see what this filter version was unable to keep. Collect the
	// distinct key names — names only, never values — and report them once.
	// Tool arguments dropped by the deny list are reported the same way under
	// their own code.
	var omittedKeys, deniedKeys keyNameSet
	addGap := func(code string, record int, detail string) {
		key := fmt.Sprintf("%s:%s", code, detail)
		if !gapSet[key] {
			gapSet[key] = true
			// Do not retain a source line number: an excluded preceding line must
			// not alter an otherwise identical retained snapshot.
			result.Gaps = append(result.Gaps, archive.CaptureGap{Code: code, Detail: detail})
		}
	}
	// A transcript holds one subagent-meta record: the lead when there is one,
	// else the first the transcript itself holds (a retained snapshot filtered
	// again).
	var meta subagentMetaSlot
	var filteredLead subagentLead
	if lead != nil && format == "claude-jsonl" {
		var err error
		if filteredLead, err = meta.lead(lead); err != nil {
			return archive.FilteredTranscript{}, err
		}
	}
	for {
		records.end()
		observeRetained(retained, len(result.Records))
		line, more := next()
		if !more {
			break
		}
		lineNo++
		// bytes.TrimSpace, not strings.TrimSpace(string(line)): the same test
		// without copying a record that can be tens of megabytes.
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := records.borrow(len(line)); err != nil {
			return archive.FilteredTranscript{}, err
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			result.NativeStartComplete = false
			addGap("incomplete_or_invalid_record", lineNo, "jsonl record omitted")
			continue
		}
		kind, _ := raw["type"].(string)
		noteOwnedNativeIdentity(&result, raw, kind, own)
		if err := validateMetadataReserved(kind, line, validateMeta, beforeRecord); err != nil {
			return archive.FilteredTranscript{}, err
		}
		if format == "claude-jsonl" && isCompactBoundary(raw) {
			recognized++
			if err := retainSafeIdentityRecord(&result, compactBoundaryRecord(raw, omittedKeys.add)); err != nil {
				return archive.FilteredTranscript{}, err
			}
			continue
		}
		if format == "claude-jsonl" && isClaudeLabelType(kind) {
			recognized++
			safe := filterClaudeLabel(raw, lineNo, addGap, omittedKeys.add)
			if err := retainSafeIdentityRecord(&result, safe); err != nil {
				return archive.FilteredTranscript{}, err
			}
			continue
		}
		if format == "claude-jsonl" && kind == subagentMetaType {
			recognized++
			encoded, err := meta.filter(raw, lineNo, addGap, omittedKeys.add)
			if err != nil {
				return archive.FilteredTranscript{}, err
			}
			retain(&result, encoded)
			continue
		}
		cursorRoleContent := cursorRoleRecord(format, kind, raw)
		if !recordTypeAllowed(knownTypes, kind, cursorRoleContent) {
			addGap("unknown_record_type", lineNo, "record omitted")
			continue
		}
		recognized++
		if stripMetaRecordText(raw) {
			addGap("hidden_instruction_omitted", lineNo, "meta record text omitted")
		}
		state := archive.PrivacyState{Record: lineNo, AddGap: addGap, OmittedKey: omittedKeys.add, DeniedKey: deniedKeys.add, ExtraAllowed: codexLabelMetadataKeys(format, kind)}
		safe, keep := sanitizeObject(raw, &state)
		if !keep {
			continue
		}
		observeHarness(&result, safe)
		noteSafeIdentity(&result, safe)
		if err := retainEncodedRecord(&result, safe, encoder, bounds); err != nil {
			return archive.FilteredTranscript{}, err
		}
	}
	if err := filteredReadComplete(readError(), lineNo, recognized); err != nil {
		return archive.FilteredTranscript{}, err
	}
	filteredLead.writeTo(&result, addGap, omittedKeys.add)
	if detail := omittedKeys.detail("omitted keys: "); detail != "" {
		addGap("unknown_field_omitted", 0, detail)
	}
	if detail := deniedKeys.detail(deniedToolArgumentIntro); detail != "" {
		addGap("sensitive_or_hidden_field_omitted", 0, detail)
	}
	sort.SliceStable(result.Gaps, func(i, j int) bool { return result.Gaps[i].Code < result.Gaps[j].Code })
	return result, nil
}

func cursorRoleRecord(format, kind string, raw map[string]any) bool {
	return format == "cursor-jsonl" && kind == "" && firstString(raw, "role") != ""
}

func validateMetadata(kind string, line []byte, validate func([]byte) error) error {
	if kind == "session_meta" && validate != nil {
		return validate(line)
	}
	return nil
}

func retain(t *archive.FilteredTranscript, encoded []byte) {
	if encoded == nil {
		return
	}
	t.Records = append(t.Records, encoded)
	t.Boundary.RetainedRecords++
	t.Boundary.RetainedBytes += len(encoded)
}

func retainFirst(t *archive.FilteredTranscript, encoded []byte) {
	if encoded == nil || len(t.Records) == 0 {
		return
	}
	t.Records = slices.Insert(t.Records, 0, encoded)
	t.Boundary.RetainedRecords++
	t.Boundary.RetainedBytes += len(encoded)
}

func noteRecordTime(t *archive.FilteredTranscript, raw map[string]any) {
	observed := parseNativeTimestamp(raw)
	if t.FirstEventAt.IsZero() {
		t.FirstEventAt = observed
	}
	if observed.IsZero() {
		if recordCarriesConversation(raw) {
			t.NativeStartComplete = false
		}
	} else if t.NativeStartAt.IsZero() || observed.Before(t.NativeStartAt) {
		t.NativeStartAt = observed
	}
	if !observed.IsZero() && (t.NativeEndAt.IsZero() || observed.After(t.NativeEndAt)) {
		t.NativeEndAt = observed
	}
}

func compactBoundaryRecord(raw map[string]any, omit func(string)) map[string]any {
	out := map[string]any{"type": "system", "subtype": "compact_boundary"}
	for key, value := range raw {
		switch {
		//lint:ignore LV1001 keys of an external JSON record are an open domain
		case key == "type" || key == "subtype":
		case key == "timestamp":
			if stamp, ok := value.(string); ok && !parseNativeTimestamp(map[string]any{"timestamp": stamp}).IsZero() {
				out[key] = stamp
			} else {
				omit(key)
			}
		case compactBoundaryIDKeys[key]:
			// An id is kept only when it looks like one (see
			// looksLikeRecordID). A null parent is kept as null.
			if value == nil {
				out[key] = nil
			} else if id, ok := value.(string); ok && looksLikeRecordID(id) {
				out[key] = id
			} else {
				omit(key)
			}
		case key == "isSidechain":
			if flag, ok := value.(bool); ok {
				out[key] = flag
			} else {
				omit(key)
			}
		default:
			omit(key)
		}
	}
	return out
}

func looksLikeRecordID(value string) bool {
	if value == "" || len(value) > 256 || strings.ContainsAny(value, " \t\r\n<>") {
		return false
	}
	_, sensitive := archive.RedactSensitive(value)
	return !sensitive
}

func stripMetaRecordText(record map[string]any) bool {
	if !isMetaRecord(record) {
		return false
	}
	stripped := stripContentText(record)
	if message, ok := record["message"].(map[string]any); ok {
		if stripContentText(message) {
			stripped = true
		}
		if len(message) == 0 {
			delete(record, "message")
		}
	}
	return stripped
}

func stripContentText(holder map[string]any) bool {
	content, present := holder["content"]
	if !present {
		return false
	}
	switch value := content.(type) {
	case string:
		delete(holder, "content")
		return true
	case map[string]any:
		return stripBlockText(value)
	case []any:
		kept := make([]any, 0, len(value))
		stripped := false
		for _, raw := range value {
			switch block := raw.(type) {
			case string:
				stripped = true
			case map[string]any:
				if textBlockTypes[strings.ToLower(strings.TrimSpace(firstString(block, "type")))] {
					stripped = true
					continue
				}
				if stripBlockText(block) {
					stripped = true
				}
				kept = append(kept, block)
			default:
				kept = append(kept, raw)
			}
		}
		if len(kept) == 0 {
			delete(holder, "content")
		} else {
			holder["content"] = kept
		}
		return stripped
	}
	return false
}

func stripBlockText(block map[string]any) bool {
	stripped := false
	if _, isText := block["text"].(string); isText {
		delete(block, "text")
		stripped = true
	}
	if stripContentText(block) {
		stripped = true
	}
	return stripped
}

func appendUniqueString(values []string, candidate string) []string {
	if candidate == "" {
		return values
	}
	if slices.Contains(values, candidate) {
		return values
	}
	return append(values, candidate)
}

func recordCarriesConversation(record map[string]any) bool {
	if _, present := record["message"]; present {
		return true
	}
	if firstString(record, "role") != "" {
		return true
	}
	kind, _ := record["type"].(string)
	return conversationRecordTypes[strings.ToLower(strings.TrimSpace(kind))]
}

const adapterVersion = "0.18.0"

var maxRecordBytes = archive.MaxRecordBytes

func observeHarness(t *archive.FilteredTranscript, record map[string]any) {
	if t.Format == "claude-jsonl" {
		if version := strings.TrimSpace(firstString(record, "version")); version != "" {
			t.ObservedHarness.Version = version
		}
		return
	}
	if firstString(record, "type") != "session_meta" {
		return
	}
	if version := firstStringDeep(record, "cli_version"); version != "" {
		t.ObservedHarness.Version = version
	}
	if mode := firstStringDeep(record, "source"); mode != "" {
		t.ObservedHarness.Mode = mode
	}
}

func noteNativeIdentity(result *archive.FilteredTranscript, raw map[string]any) {
	if isClaudeTitleRecord(raw) {
		return
	}
	noteRecordTime(result, raw)
	result.SessionIDs = appendUniqueString(result.SessionIDs, firstString(raw, "session_id", "sessionId"))
	result.AgentIDs = appendUniqueString(result.AgentIDs, firstString(raw, "agent_id", "agentId"))
}

// noteSafeIdentity observes only records that survived the privacy filter.
func noteSafeIdentity(result *archive.FilteredTranscript, safe map[string]any) {
	if isClaudeTitleRecord(safe) {
		return
	}
	if result.Format == "codex-jsonl" && result.LocalIdentity.ID == "" && firstString(safe, "type") == "session_meta" {
		if payload, ok := safe["payload"].(map[string]any); ok {
			result.LocalIdentity.ID = firstString(payload, "id")
		}
	}
	for _, key := range []string{"session_id", "sessionId"} {
		result.LocalIdentity.Candidates = appendUniqueString(result.LocalIdentity.Candidates, firstString(safe, key))
	}
}

func retainSafeIdentityRecord(result *archive.FilteredTranscript, safe map[string]any) error {
	if safe == nil {
		return nil
	}
	noteSafeIdentity(result, safe)
	encoded, err := json.Marshal(safe)
	if err != nil {
		return &archive.FilterError{Reason: "safe record cannot be encoded"}
	}
	retain(result, encoded)
	return nil
}

func observeRetained(observer func(int), count int) {
	if observer != nil {
		observer(count)
	}
}

func boundRecordSource(next func() ([]byte, bool), readError func() error, bounds []archive.CaptureBoundary) (func() ([]byte, bool), func() error) {
	if len(bounds) == 0 || bounds[0].RetainedRecords <= 0 {
		return next, readError
	}
	left := bounds[0].RetainedRecords
	exceeded := false
	return func() ([]byte, bool) {
			raw, more := next()
			if !more {
				return raw, more
			}
			if left <= 0 {
				exceeded = true
				return nil, false
			}
			left--
			return raw, true
		}, func() error {
			if exceeded {
				return errors.Join(bufio.ErrTooLong, readError())
			}
			return readError()
		}
}

func bytesBoundReached(bounds []archive.CaptureBoundary, used, next int) bool {
	return len(bounds) > 0 && bounds[0].RetainedBytes > 0 && next > bounds[0].RetainedBytes-used
}

func filteredReadComplete(err error, lines, recognized int) error {
	if errors.Is(err, bufio.ErrTooLong) {
		return archive.ErrRecordTooLarge
	}
	if err != nil {
		return &archive.FilterError{Reason: "transcript cannot be read"}
	}
	if lines > 0 && recognized == 0 {
		return archive.ErrUnsafeSourceFormat
	}
	return nil
}

func noteOwnedNativeIdentity(result *archive.FilteredTranscript, raw map[string]any, kind string, own func(string) bool) {
	if own == nil || own(kind) {
		noteNativeIdentity(result, raw)
	}
}

func codexLabelMetadataKeys(format, kind string) map[string]bool {
	if format == "codex-jsonl" && kind == "session_meta" {
		return map[string]bool{"history_mode": true}
	}
	return nil
}

type recordReservations struct {
	before  func(int) (func(), error)
	release func()
}

func (r *recordReservations) end() {
	if r.release != nil {
		r.release()
		r.release = nil
	}
}

func (r *recordReservations) borrow(n int) error {
	if r.before == nil {
		return nil
	}
	release, err := r.before(n)
	r.release = release
	return err
}

func validateMetadataReserved(kind string, line []byte, validate func([]byte) error, before func(int) (func(), error)) error {
	if kind == "session_meta" && validate != nil && before != nil {
		release, err := before(len(line))
		if err != nil {
			return err
		}
		defer release()
	}
	return validateMetadata(kind, line, validate)
}

func encodeSafeRecord(value map[string]any, encoder func(map[string]any) ([]byte, error)) ([]byte, error) {
	if encoder != nil {
		return encoder(value)
	}
	return json.Marshal(value)
}

func retainEncodedRecord(result *archive.FilteredTranscript, safe map[string]any, encoder func(map[string]any) ([]byte, error), bounds []archive.CaptureBoundary) error {
	encoded, err := encodeSafeRecord(safe, encoder)
	if errors.Is(err, agentapi.ErrReadBudget) {
		// The caller's memory ledger refused the record, not its content:
		// no claim that the source format is unsafe.
		return err
	}
	if err != nil {
		return errors.Join(&archive.FilterError{Reason: "safe record cannot be encoded"}, err)
	}
	if bytesBoundReached(bounds, result.Boundary.RetainedBytes, len(encoded)) {
		return archive.ErrRecordTooLarge
	}
	retain(result, encoded)
	return nil
}
