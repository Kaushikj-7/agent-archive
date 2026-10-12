package archive

import (
	"regexp"
	"strings"
)

// injectedInstructionOpen matches the opening tag of a block a harness injects
// into an otherwise ordinary message: Claude Code wraps CLAUDE.md, hook output,
// and memory in <system-reminder>, and Codex writes AGENTS.md inside
// <user_instructions>, machine details inside <environment_context>, and a
// catalog of uninstalled plugins inside <recommended_plugins>. They are
// instructions to the model, not something the user wrote, so they are
// stripped from string content wherever they appear. Untagged instruction text
// is deliberately not guessed at.
var injectedInstructionOpen = regexp.MustCompile(`<(system-reminder|user_instructions|environment_context|recommended_plugins)\b[^>]*>`)

// injectedInstructionOpenByTag finds a further opening tag of one specific
// kind, so a block nested inside a block of the same kind extends the outer
// block instead of ending it early.
var injectedInstructionOpenByTag = map[string]*regexp.Regexp{
	"external_codex_apps_open_page": regexp.MustCompile(`<external_codex_apps_open_page(?:\s+[^>]*)?>`),
	"system-reminder":               regexp.MustCompile(`<system-reminder\b[^>]*>`),
	"user_instructions":             regexp.MustCompile(`<user_instructions\b[^>]*>`),
	"environment_context":           regexp.MustCompile(`<environment_context\b[^>]*>`),
	"recommended_plugins":           regexp.MustCompile(`<recommended_plugins\b[^>]*>`),
}

// stripInjectedInstructions removes every injected instruction block from one
// string. It reports whether anything was removed and returns the remaining
// text, which is empty when the string held nothing else. A block runs from
// its opening tag to the matching closing tag, counting nested blocks of the
// same kind; an opening tag whose block never closes, which a truncated or
// still-streaming record can produce, drops everything after it rather than
// retaining part of the block.
func stripInjectedInstructions(value string) (bool, string) {
	changed, safe := stripKnownInjectedInstructions(value)
	// Native open-page context is a leading wrapper. A mention in prose or a
	// quoted/code example is user text, not proof of injected page context.
	// Four spaces or a tab starts a Markdown code example.
	for !startsWithIndentedCode(safe) {
		leading := strings.TrimLeft(safe, " \t\r\n")
		open := injectedInstructionOpenByTag["external_codex_apps_open_page"].FindStringIndex(leading)
		if open == nil || open[0] != 0 {
			break
		}
		changed = true
		end := injectedInstructionEnd(leading, open[1], "external_codex_apps_open_page")
		if end < 0 {
			safe = ""
			break
		}
		safe = leading[end:]
	}
	if changed {
		// Keep Markdown indentation through repeated sanitization passes.
		// Removing a separate injected block must not turn a code example
		// into a leading native context wrapper on the next pass.
		if startsWithIndentedCode(safe) {
			safe = strings.TrimRight(safe, " \t\r\n")
		} else {
			safe = strings.TrimSpace(safe)
		}
	}
	return changed, safe
}

// startsWithIndentedCode checks the first nonblank line without removing its
// indentation. Markdown blank lines may themselves contain spaces or tabs.
func startsWithIndentedCode(value string) bool {
	for {
		end := strings.IndexAny(value, "\r\n")
		line := value
		if end >= 0 {
			line = value[:end]
		}
		if strings.Trim(line, " \t") != "" {
			return strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")
		}
		if end < 0 {
			return false
		}
		value = value[end+1:]
	}
}

func stripKnownInjectedInstructions(value string) (bool, string) {
	loc := injectedInstructionOpen.FindStringSubmatchIndex(value)
	if loc == nil {
		return false, value
	}
	var out strings.Builder
	rest := value
	for loc != nil {
		out.WriteString(rest[:loc[0]])
		end := injectedInstructionEnd(rest, loc[1], rest[loc[2]:loc[3]])
		if end < 0 {
			rest = ""
			break
		}
		rest = rest[end:]
		loc = injectedInstructionOpen.FindStringSubmatchIndex(rest)
	}
	out.WriteString(rest)
	return true, out.String()
}

// injectedInstructionEnd returns the index just past the closing tag which
// matches an opening tag of kind tag that ended at from, or -1 when the block
// never closes.
func injectedInstructionEnd(value string, from int, tag string) int {
	closeTag := "</" + tag + ">"
	open := injectedInstructionOpenByTag[tag]
	depth, pos := 1, from
	for depth > 0 {
		closeAt := strings.Index(value[pos:], closeTag)
		if closeAt < 0 {
			return -1
		}
		if openAt := open.FindStringIndex(value[pos:]); openAt != nil && openAt[0] < closeAt {
			depth++
			pos += openAt[1]
			continue
		}
		depth--
		pos += closeAt + len(closeTag)
	}
	return pos
}

// redactSensitive applies every value-level credential pattern to one string
// and reports whether anything was replaced. The narrow structural patterns
// run before the broad assignment patterns so that, for example, a
// `x-access-token:…@host` URL keeps its host instead of losing everything
// after the word "token", and a YAML block value is taken whole before its
// key line is read as an assignment. The patterns and the vocabulary they
// share are in credential_shapes.go and credential_vocabulary.go.
//
// A pass reads quote context from the text around a value, and a
// replacement earlier in the same pass can remove a quote that context
// depended on, so the passes repeat until the text is stable; redacting
// twice then changes nothing. Only text that was redacted takes a second
// pass.
func redactSensitive(value string) (string, bool) {
	redacted := false
	for range maxRedactPasses {
		next, hit := redactSensitiveOnce(value)
		if !hit {
			break
		}
		redacted = true
		if next == value {
			break
		}
		value = next
	}
	return value, redacted
}

// RedactText returns text with every credential the filter recognizes in a
// string replaced by [REDACTED], and nothing else changed: there is no
// length cap, so a digest of the result covers all of the text. A producer
// that records a digest beside something it uploads filtered hashes this,
// never the original, so the digest cannot confirm a guess at a redacted
// secret.
func RedactText(text string) string {
	out, _ := redactSensitive(text)
	return out
}

// maxRedactPasses bounds redactSensitive's passes. Every pass is complete,
// so the result is redacted however many ran.
const maxRedactPasses = 4

// redactSensitiveOnce is one pass of every pattern (see redactSensitive).
// The line-based patterns read the text through a needleText (see
// linePattern), prepared again only when a step changed the text.
func redactSensitiveOnce(value string) (string, bool) {
	return redactOnce(value, newNeedleText)
}

// redactOnce is redactSensitiveOnce with the text prepared by prepare.
func redactOnce(value string, prepare func(string) needleText) (string, bool) {
	redacted := false
	text := prepare(value)
	apply := func(out string, hit bool) {
		if hit {
			value, redacted = out, true
			if text.s != value {
				text = prepare(value)
			}
		}
	}
	apply(redactPrivateKeyBlocks(value))
	apply(redactURLUserinfo(value))
	apply(redactMatches(credentialShape, text, false))
	for _, pattern := range credentialContextPatterns {
		apply(redactMatches(pattern, text, true))
	}
	apply(redactPgpassLines(text))
	apply(redactYAMLBlockValues(text))
	apply(redactCredentialEntryValues(text))
	apply(redactCredentialStructures(text))
	apply(redactAssignments(text))
	apply(redactMatches(linePattern{credentialFlag, vocabularyNeedles, "-"}, text, true))
	return value, redacted
}

// valueSpan is the byte range of one value to redact.
type valueSpan struct {
	start int
	end   int
}

// redactMatches replaces, in t.s, the "value" group of every match of
// pattern with [REDACTED], keeping a quoted value's quotes, or with values
// false the whole match, and reports whether anything was replaced. A
// pattern may name several alternative groups "value"; the one that matched
// is replaced.
func redactMatches(pattern linePattern, t needleText, values bool) (string, bool) {
	matches := lineMatches(pattern, t)
	if matches == nil {
		return t.s, false
	}
	if !values {
		var out strings.Builder
		last := 0
		for _, match := range matches {
			out.WriteString(t.s[last:match[0]])
			out.WriteString(redactedMarker)
			last = match[1]
		}
		out.WriteString(t.s[last:])
		return out.String(), true
	}
	var groups []int
	for i, name := range pattern.re.SubexpNames() {
		if name == "value" {
			groups = append(groups, i)
		}
	}
	spans := make([]valueSpan, 0, len(matches))
	for _, match := range matches {
		for _, group := range groups {
			if match[2*group] >= 0 {
				spans = append(spans, valueSpan{match[2*group], match[2*group+1]})
				break
			}
		}
	}
	return redactSpans(t.s, spans), len(spans) > 0
}

// redactAssignments redacts the value of every credential assignment
// (credentialAssignment). A quoted value ends at its closing quote; an
// unquoted one runs to the end of its line (see unquotedValueEnd), and one
// in a URL query to the next parameter. A value that is only a boolean or
// null, or a YAML block indicator, is not a secret and is left alone.
//
// An assignment and its value never cross a line break, so the search runs
// line by line, on the lines holding a credential word (see linePattern).
func redactAssignments(t needleText) (string, bool) {
	s := t.s
	if t.whole {
		spans := appendAssignmentSpans(nil, s, 0, len(s))
		if len(spans) == 0 {
			return s, false
		}
		return redactSpans(s, spans), true
	}
	present, gated := t.presentNeedles(vocabularyNeedles)
	if gated && len(present) == 0 {
		return s, false
	}
	var spans []valueSpan
	for lineStart := 0; lineStart <= len(s); {
		lineEnd := strings.IndexByte(s[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(s)
		} else {
			lineEnd += lineStart
		}
		if lineHasNeedle(t.lower[lineStart:lineEnd], present, gated) && strings.ContainsAny(s[lineStart:lineEnd], assignmentSeparators) {
			spans = appendAssignmentSpans(spans, s, lineStart, lineEnd)
		}
		lineStart = lineEnd + 1
	}
	if len(spans) == 0 {
		return s, false
	}
	return redactSpans(s, spans), true
}

// appendAssignmentSpans appends the value span of every credential
// assignment in the line s[lineStart:lineEnd].
func appendAssignmentSpans(spans []valueSpan, s string, lineStart, lineEnd int) []valueSpan {
	nameGroup := credentialAssignment.SubexpIndex("name")
	valueGroup := credentialAssignment.SubexpIndex("value")
	quotes := &quoteScanner{s: s, pos: lineStart}
	for pos := lineStart; pos < lineEnd; {
		match := credentialAssignment.FindStringSubmatchIndex(s[pos:lineEnd])
		if match == nil {
			break
		}
		if match[0] == 0 && pos > lineStart && isNameByte(s[pos-1]) {
			// The slice's start stood in for credentialLead's `^`, in the
			// middle of a name.
			pos++
			continue
		}
		nameStart, nameEnd := pos+match[2*nameGroup], pos+match[2*nameGroup+1]
		start, end := pos+match[2*valueGroup], pos+match[2*valueGroup+1]
		end = assignmentValueEnd(s, quotes, nameStart, nameEnd, start, end)
		if end < 0 {
			pos += match[1]
			continue
		}
		spans = append(spans, valueSpan{start, end})
		// A value cut short (a URL parameter) resumes the search right after
		// it, so a credential in the next parameter is still found.
		pos = end
	}
	return spans
}

// assignmentValueEnd returns where the value of one credential assignment
// ends, or -1 when the value is not a secret. start and end are the value
// as credentialAssignment matched it; quotes scans s.
func assignmentValueEnd(s string, quotes *quoteScanner, nameStart, nameEnd, start, end int) int {
	bare := strings.TrimLeft(s[start:end], "=")
	valueStart := end - len(bare)
	urlParam := nameStart > 0 && (s[nameStart-1] == '?' || s[nameStart-1] == '&')
	switch {
	case bare == "":
		return -1
	case isQuotedStart(bare):
		return end
	case bare[0] == '[' || bare[0] == '{':
		if urlParam {
			end = min(end, urlParamEnd(s, start))
		}
		// A glued tail stops at the closing quote of a string the
		// assignment sits in, as an unquoted value does.
		if quote := quotes.context(nameStart, nameEnd); quote != 0 {
			if at := strings.IndexByte(s[start:end], quote); at >= 0 {
				end = start + at
			}
		}
		return backOffEscapes(s, start, end)
	}
	if urlParam {
		end = urlParamEnd(s, start)
	} else {
		end = unquotedValueEnd(s, start, quotes.context(nameStart, nameEnd), strings.ToLower(s[nameStart:nameEnd]))
	}
	end = backOffEscapes(s, start, end)
	if end <= valueStart || isNonSecretValue(s[valueStart:end]) {
		return -1
	}
	return end
}

// unquotedValueEnd returns where an unquoted credential value that starts at
// start ends: at the end of its line, so `password: correct horse battery
// staple` and `DB_PASSWORD=Xk9;mP2vQ7zR` are redacted whole (filter 10
// stopped at the first space, `,`, `;`, or quote and kept the rest). It ends
// earlier at:
//   - the closing quote of a quoted string the assignment sits in
//     (`-H 'x-api-key: abc'`, `["TOKEN=abc"]`, an escaped JSON string);
//   - whitespace followed by shell punctuation (`&&`, `||`, `|`, `;`, a
//     redirection), a comment (`#`), a flag (`-`), a line continuation, or
//     another assignment (`user=bob`), so `A=x B=y` and `TOKEN=x && make`
//     keep what follows;
//   - `,` or `;` followed by whitespace, which ends a list item or a
//     statement (`f(password=abc, user=x)`, `password = x; next()`).
//
// A Cookie or Set-Cookie value runs to the end of the line whatever it
// holds, since `; ` separates its cookies. quote is the assignment's quote
// context (assignmentQuoteContext).
func unquotedValueEnd(s string, start int, quote byte, name string) int {
	cookie := strings.HasSuffix(name, "cookie") || strings.HasSuffix(name, "cookies")
	i := start
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\n' || c == '\r':
			return i
		case quote != 0 && c == quote:
			return i
		case c == ' ' || c == '\t':
			j := i
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			if j == len(s) || s[j] == '\n' || s[j] == '\r' || (quote != 0 && s[j] == quote) || (!cookie && valueBoundary(s[j:])) {
				return i
			}
			i = j
		case !cookie && (c == ',' || c == ';') && (i+1 == len(s) || strings.IndexByte(" \t\r\n", s[i+1]) >= 0):
			return i
		default:
			i++
		}
	}
	return i
}

// assignmentStart matches another `name=` assignment.
var assignmentStart = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*=`)

// valueBoundary reports whether text after whitespace ends an unquoted
// value (see unquotedValueEnd).
func valueBoundary(t string) bool {
	for _, prefix := range []string{"&&", "||", "|", ";", ">", "<", "2>", "#", "-"} {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	if t[0] == '\\' && (len(t) == 1 || t[1] == '\n' || t[1] == '\r') {
		return true
	}
	return assignmentStart.MatchString(t)
}

// quoteScanner tracks which quote, if any, is open at a position in s, on
// that position's line. Assignments on a line are checked in order, so it
// resumes where the last check stopped rather than rescanning the line: a
// long line with many assignments (minified code) stays linear.
type quoteScanner struct {
	s     string
	pos   int  // a position whose state is known: the start of a line, or where the last scan stopped
	quote byte // the quote open at pos, or 0
}

// context returns the quote character of a string the assignment whose name
// spans s[nameStart:nameEnd] sits in, or 0. A quoted name's own opening
// quote (`"password": …`) is not such a string; a quote before an unquoted
// name (`["TOKEN=abc"]`, a backticked “ `PASS=x` “) is.
func (q *quoteScanner) context(nameStart, nameEnd int) byte {
	end := nameStart
	if nameEnd < len(q.s) && strings.IndexByte("\"'\\", q.s[nameEnd]) >= 0 {
		for end > 0 && strings.IndexByte("\"'\\", q.s[end-1]) >= 0 {
			end--
		}
	}
	return q.openAt(end)
}

// openAt returns the quote character of a string left open at end, within
// end's line, or 0 when none is.
func (q *quoteScanner) openAt(end int) byte {
	if end < q.pos {
		q.pos, q.quote = strings.LastIndexAny(q.s[:end], "\n\r")+1, 0
	}
	for i := q.pos; i < end; i++ {
		c := q.s[i]
		switch {
		case c == '\n' || c == '\r':
			q.quote = 0
		case q.quote == 0 && (c == '"' || c == '\'' || c == '`'):
			q.quote = c
		case q.quote != 0 && c == q.quote:
			q.quote = 0
		}
	}
	q.pos = end
	return q.quote
}

// urlParamEnd returns where a URL query parameter's value starting at start
// ends: at the next `&`, a fragment, whitespace, a quote, or an escape.
func urlParamEnd(s string, start int) int {
	for i := start; i < len(s); i++ {
		if strings.IndexByte("&# \t\r\n\"'<>`\\", s[i]) >= 0 {
			return i
		}
	}
	return len(s)
}

// backOffEscapes moves a value's end before the backslashes that escape the
// quote after it (`TOKEN=abc\"` in escaped JSON keeps its `\"`), so the
// marker is never glued to a backslash a second pass would take along.
func backOffEscapes(s string, start, end int) int {
	if end < len(s) && (s[end] == '"' || s[end] == '\'' || s[end] == '`') {
		for end > start && s[end-1] == '\\' {
			end--
		}
	}
	return end
}

func isQuotedStart(value string) bool {
	rest := strings.TrimLeft(value, `\`)
	return rest != "" && (rest[0] == '"' || rest[0] == '\'')
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-'
}

// nonSecretValue matches an unquoted value that cannot be a secret: a
// boolean or null (`use_password: false`, `token: null`), or a YAML block
// scalar indicator (`password: |`), whose value is on the lines below (see
// redactYAMLBlockValues).
var nonSecretValue = regexp.MustCompile(`(?i)^(?:true|false|null|nil|none|undefined|[|>][0-9+-]*)$`)

func isNonSecretValue(value string) bool {
	return nonSecretValue.MatchString(strings.TrimSpace(value))
}

// quotedPrefix matches the quoted part at the start of a quoted value,
// without anything glued on after its closing quote.
var quotedPrefix = regexp.MustCompile(`^(?:` + credentialQuoted + `|` + credentialPlainQuoted + `)`)

// redactSpans replaces each span of value (in order, not overlapping) with
// [REDACTED] (see writeRedacted).
func redactSpans(value string, spans []valueSpan) string {
	var out strings.Builder
	last := 0
	for _, span := range spans {
		out.WriteString(value[last:span.start])
		writeRedacted(&out, value[span.start:span.end])
		last = span.end
	}
	out.WriteString(value[last:])
	return out.String()
}

// writeRedacted writes the redacted form of one value. Extra `=` signs
// before the value stay (`PASSWORD==[REDACTED]`), and a quoted value keeps
// its quotes, with any backslashes escaping them, so the marker is never
// glued to text a second pass would take as part of the value.
func writeRedacted(out *strings.Builder, secret string) {
	equals := len(secret) - len(strings.TrimLeft(secret, "="))
	out.WriteString(secret[:equals])
	secret, quote := secret[equals:], ""
	if escapes := len(secret) - len(strings.TrimLeft(secret, `\`)); escapes < len(secret) && (secret[escapes] == '"' || secret[escapes] == '\'') {
		quote = secret[:escapes+1]
	}
	out.WriteString(quote + redactedMarker)
	// Text glued on after the closing quote is part of the value and goes
	// with it; only the quoted part's closing quote is kept.
	if quote != "" {
		if quoted := quotedPrefix.FindString(secret); quoted != "" {
			secret = quoted
		}
	}
	// The closing quote, with whatever backslashes escape it (they can
	// differ from the opening's in malformed input), stays too.
	if quote != "" && len(secret) > len(quote) && secret[len(secret)-1] == quote[len(quote)-1] {
		body := strings.TrimRight(secret[len(quote):len(secret)-1], `\`)
		out.WriteString(secret[len(quote)+len(body):])
	}
}

// base64DataURL matches a base64 `data:` URL, the form Codex and browser
// tools use to inline an image or file in a string. The media type is kept;
// the payload is replaced (see sanitizeValue). Short payloads are left alone.
var base64DataURL = regexp.MustCompile(`(?i)\bdata:([a-z0-9.+-]+/[a-z0-9.+-]+)?((?:;[a-z0-9.+-]+=[a-z0-9.+-]+)*);base64,[A-Za-z0-9+/_-]{16,}=*`)

// Every base64DataURL match needs this ASCII case-insensitive prefix. Probe
// colons without allocating or running the full regexp over ordinary text.
// The regexp still decides boundaries, media types, parameters and payloads.
func hasDataURLPrefix(text string) bool {
	for start := 0; start < len(text); {
		colon := strings.IndexByte(text[start:], ':')
		if colon < 0 {
			return false
		}
		colon += start
		if colon >= 4 && text[colon-4]|0x20 == 'd' && text[colon-3]|0x20 == 'a' && text[colon-2]|0x20 == 't' && text[colon-1]|0x20 == 'a' {
			return true
		}
		start = colon + 1
	}
	return false
}

// binaryBlockTypes are content-block types that carry an image, a document,
// or audio rather than text: Claude's `image` and `document` blocks (their
// `source` is base64 data, a URL, or a file ID) and the OpenAI Responses and
// Chat Completions input shapes Codex writes. A block of one of these types
// is dropped whole at any depth, with a binary_content_omitted gap.
var binaryBlockTypes = map[string]bool{
	"image": true, "document": true, "input_image": true, "input_file": true,
	"input_audio": true, "image_url": true,
}

// binaryContentBlock reports whether an object is a content block carrying
// binary content: one of binaryBlockTypes, a `file` block with inline data,
// or any object whose `source` is `{"type": "base64", …}`. The returned
// detail is a fixed string, never taken from the input.
func binaryContentBlock(in map[string]any) (string, bool) {
	kind := strings.ToLower(strings.TrimSpace(firstString(in, "type")))
	if binaryBlockTypes[kind] {
		return kind + " block omitted", true
	}
	if source, ok := in["source"].(map[string]any); ok && strings.EqualFold(strings.TrimSpace(firstString(source, "type")), "base64") {
		return "base64 source block omitted", true
	}
	if kind == "file" {
		for _, key := range []string{"source", "data", "file_data"} {
			if _, present := in[key]; present {
				return "file block omitted", true
			}
		}
	}
	return "", false
}
