package archive

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"io"
)

// Source bundle wire format, schema versions 2 and 3: gzip of newline-delimited JSON.
// Every line is one object with a "kind" discriminator, in this order:
//
//  1. exactly one "header" line, first, carrying the envelope and the number
//     of each following kind of line;
//  2. one "native_record" line per retained native record, in source order;
//  3. one "native_text" line per retained native text transcript;
//  4. one "supplemental_evidence" line per evidence item.
//
// A reader can therefore stream a bundle one record at a time instead of
// holding the whole decompressed document, which schema 1 (one JSON document)
// required.
const (
	SourceLineHeader               SourceLineKind = "header"
	SourceLineNativeRecord         SourceLineKind = "native_record"
	SourceLineNativeText           SourceLineKind = "native_text"
	SourceLineSupplementalEvidence SourceLineKind = "supplemental_evidence"
)

// SourceLineKind is the "kind" of one source bundle line.
type SourceLineKind string

// MaxSourceLineBytes bounds one decoded source line: the largest native
// record the filter reads (MaxRecordBytes, which also bounds the transcripts
// the collector accepts) plus room for the line's own envelope and JSON
// escaping overhead.
const MaxSourceLineBytes = MaxRecordBytes + 1<<20

// SourceCounts records how many lines of each kind follow the header.
type SourceCounts struct {
	NativeRecords        int `json:"native_records"`
	NativeText           int `json:"native_text"`
	SupplementalEvidence int `json:"supplemental_evidence"`
}

// SourceHeader is the first line of a source bundle.
type SourceHeader struct {
	History              *SourceHistory           `json:"history,omitempty"`
	Kind                 SourceLineKind           `json:"kind"`
	SchemaVersion        int                      `json:"schema_version"`
	ArchiveSessionID     string                   `json:"archive_session_id"`
	NativeSessionID      string                   `json:"native_session_id"`
	ProjectID            string                   `json:"project_id"`
	Capture              SourceCapture            `json:"capture"`
	PreviousGenerationID string                   `json:"previous_generation_id,omitempty"`
	NativeChild          bool                     `json:"native_child,omitempty"`
	ParentSessionID      string                   `json:"parent_session_id,omitempty"`
	LinkedSessions       []LinkedSessionReference `json:"linked_sessions,omitempty"`
	Counts               SourceCounts             `json:"counts"`
}

// SourceLine is one decoded line. Kind names which one field is set.
type SourceLine struct {
	Ordinal      *uint64
	Kind         SourceLineKind
	Header       *SourceHeader
	NativeRecord map[string]any
	NativeText   *TextTranscript
	Evidence     *SupplementalEvidence
}

type nativeRecordLine struct {
	Ordinal *uint64        `json:"ordinal,omitempty"`
	Kind    SourceLineKind `json:"kind"`
	Record  map[string]any `json:"record"`
}

type nativeTextLine struct {
	Kind    SourceLineKind `json:"kind"`
	Format  string         `json:"format"`
	Content string         `json:"content"`
}

// Supplemental evidence has its own "kind" field (skill_inventory, ...), so
// it is nested under "evidence" rather than flattened beside the line's kind.
type evidenceLine struct {
	Kind     SourceLineKind       `json:"kind"`
	Evidence SupplementalEvidence `json:"evidence"`
}

// EncodeSource writes bundle as versioned JSONL to w, uncompressed. The output
// is deterministic: struct fields keep their declared order and map keys are
// sorted by encoding/json, so identical evidence always yields identical bytes.
func EncodeSource(w io.Writer, bundle SourceBundle) error {
	if err := validateBundle(bundle); err != nil {
		return err
	}
	header := sourceHeader(bundle)
	write := func(value any) error {
		line, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode source line: %w", err)
		}
		if len(line) > MaxSourceLineBytes {
			return fmt.Errorf("source line of %d bytes exceeds the %d byte line limit", len(line), MaxSourceLineBytes)
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		_, err = w.Write([]byte{'\n'})
		return err
	}
	if err := write(header); err != nil {
		return err
	}
	for i, record := range bundle.NativeRecords {
		var ordinal *uint64
		if bundle.History != nil {
			ordinal = &bundle.Ordinals[i]
		}
		if err := write(nativeRecordLine{Kind: SourceLineNativeRecord, Record: record, Ordinal: ordinal}); err != nil {
			return err
		}
	}
	for _, text := range bundle.NativeText {
		if err := write(nativeTextLine{Kind: SourceLineNativeText, Format: text.Format, Content: text.Content}); err != nil {
			return err
		}
	}
	for _, evidence := range bundle.SupplementalEvidence {
		if err := write(evidenceLine{Kind: SourceLineSupplementalEvidence, Evidence: evidence}); err != nil {
			return err
		}
	}
	return nil
}

func sourceHeader(bundle SourceBundle) SourceHeader {
	return SourceHeader{
		History: bundle.History,
		Kind:    SourceLineHeader, SchemaVersion: bundle.SchemaVersion,
		ArchiveSessionID: bundle.ArchiveSessionID, NativeSessionID: bundle.NativeSessionID, ProjectID: bundle.ProjectID,
		Capture: bundle.Capture, PreviousGenerationID: bundle.PreviousGenerationID, NativeChild: bundle.NativeChild, ParentSessionID: bundle.ParentSessionID, LinkedSessions: bundle.LinkedSessions,
		Counts: SourceCounts{NativeRecords: len(bundle.NativeRecords), NativeText: len(bundle.NativeText), SupplementalEvidence: len(bundle.SupplementalEvidence)},
	}
}

// SourceEncodingLineBound preflights the largest concrete JSONL envelope. It
// does not encode records or hold an uncompressed document while counting.
func SourceEncodingLineBound(ctx context.Context, bundle SourceBundle, limit int64) (int64, error) {
	if err := validateBundle(bundle); err != nil {
		return 0, err
	}
	largest, err := agentmeta.JSONWireBound(ctx, sourceHeader(bundle), limit)
	if err != nil {
		return 0, err
	}
	check := func(value any) error {
		n, err := agentmeta.JSONWireBound(ctx, value, limit)
		largest = max(largest, n)
		return err
	}
	for i, record := range bundle.NativeRecords {
		var ordinal *uint64
		if bundle.History != nil {
			ordinal = &bundle.Ordinals[i]
		}
		if err := check(nativeRecordLine{Kind: SourceLineNativeRecord, Record: record, Ordinal: ordinal}); err != nil {
			return 0, err
		}
	}
	for _, text := range bundle.NativeText {
		if err := check(nativeTextLine{Kind: SourceLineNativeText, Format: text.Format, Content: text.Content}); err != nil {
			return 0, err
		}
	}
	for _, evidence := range bundle.SupplementalEvidence {
		if err := check(evidenceLine{Kind: SourceLineSupplementalEvidence, Evidence: evidence}); err != nil {
			return 0, err
		}
	}
	return largest, nil
}

// DecodeOptions bounds a streaming decode. Zero values use the defaults.
type DecodeOptions struct {
	// MaxLineBytes caps one decompressed line (default MaxSourceLineBytes).
	MaxLineBytes int
	// MaxUncompressedBytes caps the whole decompressed stream (default: no
	// cap beyond the per-line one).
	MaxUncompressedBytes int64
}

// ErrSourceTooLarge is returned when a bundle's decompressed size exceeds
// DecodeOptions.MaxUncompressedBytes.
var ErrSourceTooLarge = errors.New("source exceeds uncompressed read limit")

// DecodeSource streams a gzip-compressed source bundle, calling fn
// once per line in order. It holds at most one line in memory, and enforces
// the format's structure: the header is first and appears once, line kinds
// arrive in their fixed order, every line is well formed and within the line
// limit, and the counts in the header match the lines that follow. Any
// violation, a truncated stream, or an error from fn stops the decode. The
// caller is responsible for verifying the compressed bytes' size and hash
// before decoding.
func DecodeSource(compressed io.Reader, options DecodeOptions, fn func(SourceLine) error) error {
	maxLine := options.MaxLineBytes
	if maxLine <= 0 {
		maxLine = MaxSourceLineBytes
	}
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return fmt.Errorf("open source gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	var plain io.Reader = gz
	counter := &countingReader{r: gz, limit: options.MaxUncompressedBytes}
	if options.MaxUncompressedBytes > 0 {
		plain = counter
	}
	scanner, unterminated := sourceScanner(plain, maxLine)
	state := sourceDecodeState{}
	for scanner.Scan() {
		line, err := state.decodeLine(scanner.Bytes(), *unterminated, scanner.Err())
		if err != nil {
			return err
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	if err := sourceScanError(scanner.Err(), state.lineNo+1, maxLine); err != nil {
		return err
	}
	if state.header == nil {
		return errors.New("source is empty: no header line")
	}
	if state.seen != state.header.Counts {
		return fmt.Errorf("source line counts %+v do not match its header's %+v", state.seen, state.header.Counts)
	}
	return nil
}

// sourceScanner keeps the wire format's required newline visible to the
// decoder while limiting the buffered line to the configured size.
func sourceScanner(plain io.Reader, maxLine int) (*bufio.Scanner, *bool) {
	scanner := bufio.NewScanner(plain)
	// Every line the encoder writes ends with a newline. A final line
	// without one is still handed over, flagged, so that a schema-1 bundle
	// (one JSON document, no trailing newline) is reported as the
	// unsupported version it is, and anything else as a truncated stream.
	unterminated := new(bool)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			*unterminated = true
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	initial := min(64<<10, maxLine)
	// The scanner must buffer a line and its newline together, so a line of
	// exactly maxLine bytes (the largest the encoder writes) needs one byte
	// more than the cap; one of maxLine+1 bytes still fails.
	scanner.Buffer(make([]byte, 0, initial), maxLine+1)
	return scanner, unterminated
}

type sourceProbe struct {
	Kind          SourceLineKind `json:"kind"`
	SchemaVersion *int           `json:"schema_version"`
}

type sourceDecodeState struct {
	header  *SourceHeader
	seen    SourceCounts
	stage   int
	lineNo  int
	ordinal uint64
}

func (s *sourceDecodeState) decodeLine(raw []byte, unterminated bool, scanErr error) (SourceLine, error) {
	s.lineNo++
	if unterminated {
		return SourceLine{}, sourceUnterminatedError(raw, s.lineNo, scanErr)
	}
	probe, err := sourceLineProbe(raw, s.lineNo)
	if err != nil {
		return SourceLine{}, err
	}
	if s.lineNo == 1 {
		return s.decodeHeader(raw, probe)
	}
	return s.decodeBody(raw, probe.Kind)
}

func sourceUnterminatedError(raw []byte, lineNo int, scanErr error) error {
	var probe sourceProbe
	if lineNo == 1 && json.Unmarshal(raw, &probe) == nil && probe.Kind == "" && probe.SchemaVersion != nil {
		return fmt.Errorf("unsupported source schema version %d; this build reads schema %d only", *probe.SchemaVersion, SourceSchemaVersion)
	}
	switch {
	case errors.Is(scanErr, ErrSourceTooLarge):
		return scanErr
	case scanErr != nil:
		return fmt.Errorf("source is truncated: %w", scanErr)
	}
	return fmt.Errorf("source is truncated: %w", errUnterminatedLine)
}

func sourceLineProbe(raw []byte, lineNo int) (sourceProbe, error) {
	var probe sourceProbe
	if trimmed := bytes.TrimLeft(raw, " \t\r"); len(trimmed) == 0 || trimmed[0] != '{' {
		return probe, fmt.Errorf("source line %d is not a JSON object", lineNo)
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		if lineNo == 1 {
			return probe, fmt.Errorf("source line 1 is not a JSONL header (a schema 1 bundle is a single JSON document and is not supported): %w", err)
		}
		return probe, fmt.Errorf("source line %d is not valid JSON: %w", lineNo, err)
	}
	return probe, nil
}

func (s *sourceDecodeState) decodeHeader(raw []byte, probe sourceProbe) (SourceLine, error) {
	if probe.Kind != SourceLineHeader {
		if probe.Kind == "" && probe.SchemaVersion != nil {
			return SourceLine{}, fmt.Errorf("unsupported source schema version %d; this build reads schema %d only", *probe.SchemaVersion, SourceSchemaVersion)
		}
		return SourceLine{}, fmt.Errorf("source line 1 is %q, but the header must come first", probe.Kind)
	}
	var decoded SourceHeader
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return SourceLine{}, fmt.Errorf("decode source header: %w", err)
	}
	if decoded.SchemaVersion != SourceSchemaVersion && decoded.SchemaVersion != HistorySourceSchemaVersion {
		return SourceLine{}, fmt.Errorf("unsupported source schema version %d; this build reads schema %d only", decoded.SchemaVersion, SourceSchemaVersion)
	}
	if decoded.Counts.NativeRecords < 0 || decoded.Counts.NativeText < 0 || decoded.Counts.SupplementalEvidence < 0 {
		return SourceLine{}, errors.New("source header has negative counts")
	}
	if decoded.SchemaVersion == HistorySourceSchemaVersion {
		if decoded.Capture.Harness.Name != "codex" || decoded.Counts.NativeText != 0 {
			return SourceLine{}, errors.New("invalid history source shape")
		}
		if err := decoded.History.Validate(decoded.NativeSessionID, decoded.Counts.NativeRecords); err != nil {
			return SourceLine{}, err
		}
	} else if decoded.History != nil {
		return SourceLine{}, errors.New("history requires source schema 3")
	}
	s.header = &decoded
	return SourceLine{Kind: SourceLineHeader, Header: s.header}, nil
}

func (s *sourceDecodeState) decodeBody(raw []byte, kind SourceLineKind) (SourceLine, error) {
	if kind == SourceLineHeader {
		return SourceLine{}, fmt.Errorf("source line %d is a second header", s.lineNo)
	}
	position := sourceKindPosition(kind)
	if position == 0 {
		return SourceLine{}, fmt.Errorf("source line %d has unknown kind %q", s.lineNo, kind)
	}
	if position < s.stage {
		return SourceLine{}, fmt.Errorf("source line %d (%s) is out of order", s.lineNo, kind)
	}
	s.stage = position
	switch kind {
	case SourceLineHeader:
		return SourceLine{}, fmt.Errorf("source line %d is a second header", s.lineNo)
	case SourceLineNativeRecord:
		return s.decodeNativeRecord(raw)
	case SourceLineNativeText:
		return s.decodeNativeText(raw)
	case SourceLineSupplementalEvidence:
		return s.decodeEvidence(raw)
	}
	return SourceLine{}, fmt.Errorf("source line %d has unknown kind %q", s.lineNo, kind)
}

func sourceKindPosition(kind SourceLineKind) int {
	switch kind {
	case SourceLineHeader:
		return 0
	case SourceLineNativeRecord:
		return 1
	case SourceLineNativeText:
		return 2
	case SourceLineSupplementalEvidence:
		return 3
	}
	return 0
}

func (s *sourceDecodeState) decodeNativeRecord(raw []byte) (SourceLine, error) {
	var decoded nativeRecordLine
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return SourceLine{}, fmt.Errorf("decode source line %d: %w", s.lineNo, err)
	}
	if len(decoded.Record) == 0 {
		return SourceLine{}, fmt.Errorf("source line %d has an empty native record", s.lineNo)
	}
	if s.header.History != nil {
		span, ok := s.header.History.SpanAt(s.seen.NativeRecords)
		if !ok || decoded.Ordinal == nil || *decoded.Ordinal < span.StartOrdinal || *decoded.Ordinal >= span.EndOrdinal || (s.seen.NativeRecords > span.FirstRecord && *decoded.Ordinal <= s.ordinal) {
			return SourceLine{}, errors.New("invalid history record ordinal")
		}
		s.ordinal = *decoded.Ordinal
	} else if decoded.Ordinal != nil {
		return SourceLine{}, errors.New("ordinal requires history source")
	}
	s.seen.NativeRecords++
	if s.seen.NativeRecords > s.header.Counts.NativeRecords {
		return SourceLine{}, fmt.Errorf("source has more native records than its header's %d", s.header.Counts.NativeRecords)
	}
	return SourceLine{Kind: SourceLineNativeRecord, NativeRecord: decoded.Record, Ordinal: decoded.Ordinal}, nil
}

func (s *sourceDecodeState) decodeNativeText(raw []byte) (SourceLine, error) {
	var decoded nativeTextLine
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return SourceLine{}, fmt.Errorf("decode source line %d: %w", s.lineNo, err)
	}
	s.seen.NativeText++
	if s.seen.NativeText > s.header.Counts.NativeText {
		return SourceLine{}, fmt.Errorf("source has more native text lines than its header's %d", s.header.Counts.NativeText)
	}
	return SourceLine{Kind: SourceLineNativeText, NativeText: &TextTranscript{Format: decoded.Format, Content: decoded.Content}}, nil
}

func (s *sourceDecodeState) decodeEvidence(raw []byte) (SourceLine, error) {
	var decoded evidenceLine
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return SourceLine{}, fmt.Errorf("decode source line %d: %w", s.lineNo, err)
	}
	s.seen.SupplementalEvidence++
	if s.seen.SupplementalEvidence > s.header.Counts.SupplementalEvidence {
		return SourceLine{}, fmt.Errorf("source has more supplemental evidence than its header's %d", s.header.Counts.SupplementalEvidence)
	}
	return SourceLine{Kind: SourceLineSupplementalEvidence, Evidence: &decoded.Evidence}, nil
}

func sourceScanError(err error, lineNo, maxLine int) error {
	if err != nil {
		switch {
		case errors.Is(err, bufio.ErrTooLong):
			return fmt.Errorf("source line %d exceeds the %d byte line limit", lineNo, maxLine)
		case errors.Is(err, ErrSourceTooLarge):
			return err
		case errors.Is(err, io.ErrUnexpectedEOF):
			return fmt.Errorf("source is truncated: %w", err)
		case errors.Is(err, gzip.ErrHeader):
			// NewReader already accepted the first gzip header, so a header
			// error here means bytes followed the end of the gzip stream.
			return fmt.Errorf("source has trailing bytes after its gzip stream: %w", err)
		}
		return fmt.Errorf("read source: %w", err)
	}
	return nil
}

// ReadSourceBundle decodes a whole compressed source bundle into a
// SourceBundle, streaming it line by line. It is DecodeSource for callers
// that want the assembled bundle.
func ReadSourceBundle(compressed io.Reader, options DecodeOptions) (SourceBundle, error) {
	var bundle SourceBundle
	err := DecodeSource(compressed, options, func(line SourceLine) error {
		switch line.Kind {
		case SourceLineHeader:
			h := line.Header
			bundle = SourceBundle{
				History: h.History, SchemaVersion: h.SchemaVersion, ArchiveSessionID: h.ArchiveSessionID, NativeSessionID: h.NativeSessionID,
				ProjectID: h.ProjectID, Capture: h.Capture, PreviousGenerationID: h.PreviousGenerationID, NativeChild: h.NativeChild, ParentSessionID: h.ParentSessionID, LinkedSessions: h.LinkedSessions,
				NativeRecords: make([]map[string]any, 0, min(h.Counts.NativeRecords, 1<<16)),
			}
		case SourceLineNativeRecord:
			bundle.NativeRecords = append(bundle.NativeRecords, line.NativeRecord)
			if line.Ordinal != nil {
				bundle.Ordinals = append(bundle.Ordinals, *line.Ordinal)
			}
		case SourceLineNativeText:
			bundle.NativeText = append(bundle.NativeText, *line.NativeText)
		case SourceLineSupplementalEvidence:
			bundle.SupplementalEvidence = append(bundle.SupplementalEvidence, *line.Evidence)
		}
		return nil
	})
	if err != nil {
		return SourceBundle{}, err
	}
	if err := ValidateSessionLabels(bundle); err != nil {
		return SourceBundle{}, err
	}
	return bundle, nil
}

// errUnterminatedLine reports a final line with no newline. The encoder ends
// every line with one, so such a line can only be the cut-off end of a
// truncated stream (or a schema-1 document), and it is refused, not parsed.
var errUnterminatedLine = errors.New("final line has no newline")

// countingReader fails once more than limit bytes have been read.
type countingReader struct {
	r     io.Reader
	n     int64
	limit int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.limit > 0 && c.n > c.limit {
		return n, ErrSourceTooLarge
	}
	return n, err
}
