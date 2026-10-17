// Package jsonedit preserves unrelated bytes while editing ordered JSON members.
package jsonedit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
)

// A Document keeps original bytes and replaces only caller-selected top-level
// members. Ordered values preserve key order, literal numbers, and indentation
// without HTML escaping. Unrelated preferences retain their exact bytes.

// Object is a JSON Object that keeps its members in file order.
type Object struct{ Members []Member }

// Member is an ordered JSON key and value.
type Member struct {
	Key   string
	Value any
}

// Get returns the value of key. JSON parsers conventionally keep the last of
// duplicate keys, and so does Get.
func (o *Object) Get(key string) (any, bool) {
	for i := len(o.Members) - 1; i >= 0; i-- {
		if o.Members[i].Key == key {
			return o.Members[i].Value, true
		}
	}
	return nil, false
}

// Set replaces every Member named key with one holding value, in the place
// of the first, or appends it.
func (o *Object) Set(key string, value any) {
	for i := range o.Members {
		if o.Members[i].Key == key {
			o.Members[i].Value = value
			o.Remove(key, i+1)
			return
		}
	}
	o.Members = append(o.Members, Member{key, value})
}

// Remove deletes every Member named key at or after index from.
func (o *Object) Remove(key string, from int) {
	kept := o.Members[:from]
	for _, m := range o.Members[from:] {
		if m.Key != key {
			kept = append(kept, m)
		}
	}
	o.Members = kept
}

// decodeValue reads one JSON value from dec, which must have UseNumber set:
// objects become *Object, arrays []any, numbers json.Number.
func decodeValue(dec *json.Decoder) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		o := &Object{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid object key")
			}
			value, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			o.Members = append(o.Members, Member{key, value})
		}
		_, err = dec.Token()
		return o, err
	case '[':
		list := []any{}
		for dec.More() {
			value, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			list = append(list, value)
		}
		_, err = dec.Token()
		return list, err
	}
	return nil, errors.New("unexpected JSON delimiter")
}

// EncodeValue writes value as compact JSON without HTML escaping.
// EncodeValue encodes an ordered JSON tree without HTML escaping.
func EncodeValue(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.RawMessage:
		if !json.Valid(v) {
			return errors.New("invalid preserved JSON value")
		}
		buf.Write(v)
	case json.Number:
		buf.WriteString(v.String())
	case string:
		return encodeString(buf, v)
	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := EncodeValue(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case *Object:
		buf.WriteByte('{')
		for i, m := range v.Members {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeString(buf, m.Key); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := EncodeValue(buf, m.Value); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return errors.New("unsupported JSON value")
	}
	return nil
}

func encodeString(buf *bytes.Buffer, s string) error {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.Write(bytes.TrimSuffix(out.Bytes(), []byte("\n")))
	return nil
}

// Document is a JSON file whose top level is an Object, with the byte span
// of each top-level Member so single members can be replaced, added, or
// removed without touching the rest of the file.
type Document struct {
	src     []byte
	Root    *Object
	spans   []memberSpan // one per top-level Member, in file order
	open    int          // offset of the root '{'
	close   int          // offset of the root '}'
	indent  string       // whitespace before each top-level key; "" when compact
	newline string       // the file's line ending, "\r\n" or "\n"
	created bool         // the file was empty, so the whole Document is new
	edits   []edit
}

type memberSpan struct {
	Key      string
	keyStart int
	valStart int
	valEnd   int
}

type edit struct {
	start int
	end   int
	text  string
}

// ErrInvalidConfiguration identifies a refused JSON Document.
var ErrInvalidConfiguration = errors.New("invalid existing hook configuration")

var errInvalidConfiguration = ErrInvalidConfiguration

// configError is errInvalidConfiguration for one file. Its message is what
// setup shows; it also keeps apart the parts that message runs together,
// where in the file the problem is and what it is, for Validate.
type configError struct {
	message string
	line    int // 1-based; 0 when the problem is not at one place
	column  int // 1-based, in bytes, like the message's
	reason  string
	cause   error
}

func (e *configError) Error() string { return e.message }

func (e *configError) Unwrap() []error {
	if e.cause == nil {
		return []error{errInvalidConfiguration}
	}
	return []error{errInvalidConfiguration, e.cause}
}

// refused is a configError whose message is reason alone: at is where in
// src the problem is (offset of its first byte), or -1 when it is not at
// one place.
func refused(src []byte, at int, reason string) error {
	line, column := position(src, at)
	return &configError{message: errInvalidConfiguration.Error() + ": " + reason, line: line, column: column, reason: reason}
}

// position is the line and column (in bytes) of offset in src, both
// 1-based, or 0, 0 for an offset outside src.
func position(src []byte, offset int) (line, column int) {
	if offset < 0 || offset > len(src) {
		return 0, 0
	}
	return 1 + bytes.Count(src[:offset], []byte("\n")), offset - bytes.LastIndexByte(src[:offset], '\n')
}

// invalid is errInvalidConfiguration for src, saying where and, for the
// forms people most often put in these files by accident, what: a
// byte-order mark, a comment (JSONC), or a trailing comma, none of which is
// JSON. The applications themselves read the files as plain JSON.
func invalid(src []byte, err error) error {
	if bytes.HasPrefix(src, []byte("\xef\xbb\xbf")) {
		return refused(src, 0, "the file starts with a byte-order mark (BOM), which JSON does not allow; save it as UTF-8 without one")
	}
	var syntaxErr *json.SyntaxError
	offset := -1
	switch {
	case errors.As(err, &syntaxErr):
		offset = int(syntaxErr.Offset) - 1
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		offset = len(src)
	}
	if offset < 0 || offset > len(src) {
		return &configError{message: fmt.Sprintf("%s: %s", errInvalidConfiguration, err), reason: err.Error(), cause: err}
	}
	// A file that stops before its JSON is complete is reported at its end,
	// whatever it last held: a value, a comma, or whitespace.
	truncated := offset == len(src) || syntaxErr != nil && int(syntaxErr.Offset) == len(src) && strings.Contains(syntaxErr.Error(), "unexpected end of JSON input")
	if truncated {
		offset = len(src)
	}
	// Offset counts the bytes read, which ends just after the one that
	// failed; step back over whitespace to the character itself.
	for !truncated && offset > 0 && offset < len(src) && (src[offset] == ' ' || src[offset] == '\n' || src[offset] == '\r' || src[offset] == '\t') {
		offset--
	}
	line, column := position(src, offset)
	// The reason names the plain cause; the parser's own words are kept
	// only for a mistake not named here.
	reason := "this is not valid JSON (" + err.Error() + ")"
	rest := src[offset:]
	after := bytes.TrimLeft(bytes.TrimPrefix(rest, []byte(",")), " \t\r\n")
	// A comma where a value belongs, as in "a": , is not a trailing one.
	before := bytes.TrimRight(src[:offset], " \t\r\n")
	afterValue := len(before) > 0 && !bytes.ContainsAny(before[len(before)-1:], ":[{,")
	switch {
	case truncated:
		reason = "the file ends before its JSON is complete"
	case bytes.HasPrefix(rest, []byte("//")) || bytes.HasPrefix(rest, []byte("/*")):
		reason = "this is a comment (JSONC); comments are not JSON, so remove it"
	case bytes.HasPrefix(rest, []byte(",")) && afterValue && (len(after) == 0 || bytes.HasPrefix(after, []byte("}")) || bytes.HasPrefix(after, []byte("]"))):
		reason = "this comma comes before a closing brace or bracket (a trailing comma), which is not JSON; remove it"
	}
	return &configError{
		message: fmt.Sprintf("%s: line %d, column %d: %s", errInvalidConfiguration, line, column, reason),
		line:    line,
		column:  column,
		reason:  reason,
		cause:   err,
	}
}

// Parse reads src, which must be empty or a single JSON Object.
func Parse(src []byte, ownedKeys ...string) (*Document, error) {
	newline := "\n"
	if bytes.Contains(src, []byte("\r\n")) {
		newline = "\r\n"
	}
	created := len(bytes.TrimSpace(src)) == 0
	var indent string
	if created {
		src, indent = []byte("{}"), "  "
	}
	d := &Document{
		src:     src,
		newline: newline,
		created: created,
		indent:  indent,
	}
	dec := json.NewDecoder(bytes.NewReader(d.src))
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil {
		return nil, invalid(d.src, err)
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, refused(d.src, skipSpace(d.src, 0), "the file must hold one JSON object")
	}
	d.open = int(dec.InputOffset()) - 1
	d.Root = &Object{}
	for dec.More() {
		before := int(dec.InputOffset())
		keyToken, err := dec.Token()
		if err != nil {
			return nil, invalid(d.src, err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, refused(d.src, -1, "an object key is not a string")
		}
		keyStart := skipUntil(d.src, before, '"')
		// Setup would edit one of two members that tools resolve
		// differently (the last wins in Go and JavaScript, not everywhere),
		// so a duplicate of a Member it owns is refused.
		if _, dup := d.span(key); dup && slices.Contains(ownedKeys, key) {
			return nil, refused(d.src, keyStart, fmt.Sprintf("more than one top-level %q key; remove the duplicate", key))
		}
		valStart := skipSpace(d.src, skipSpace(d.src, int(dec.InputOffset()))+1)
		value, err := decodeValue(dec)
		if err != nil {
			return nil, invalid(d.src, err)
		}
		d.Root.Members = append(d.Root.Members, Member{key, value})
		d.spans = append(d.spans, memberSpan{key, keyStart, valStart, int(dec.InputOffset())})
	}
	if token, err = dec.Token(); err != nil {
		return nil, invalid(d.src, err)
	} else if token != json.Delim('}') {
		return nil, refused(d.src, -1, "the file must hold one JSON object")
	}
	d.close = int(dec.InputOffset()) - 1
	// After the Object, only the end of the file: text the decoder cannot
	// read there, such as a comment or a comma, is reported as what it is.
	var syntaxErr *json.SyntaxError
	if _, err = dec.Token(); errors.As(err, &syntaxErr) {
		return nil, invalid(d.src, err)
	} else if !errors.Is(err, io.EOF) {
		return nil, refused(d.src, skipSpace(d.src, d.close+1), "more than one JSON value in the file")
	}
	if !d.created {
		d.indent = d.memberIndent()
	}
	return d, nil
}

// memberIndent is the whitespace a top-level key is indented with: that of
// the first Member that starts a line of its own, so a file whose first key
// shares the opening brace's line ({"a": 1,\n  "b": 2}) still gets its
// new members on lines of their own. It is "" for a compact file (no Member
// starts a line), and two spaces for an empty Object.
func (d *Document) memberIndent() string {
	if len(d.spans) == 0 {
		return "  "
	}
	start := d.open + 1
	for _, s := range d.spans {
		lead := string(d.src[start:s.keyStart])
		// Only a key that starts its line counts: "\n  ," puts the
		// separator there, which is no indentation.
		if i := strings.LastIndexByte(lead, '\n'); i >= 0 && strings.Trim(lead[i+1:], " \t") == "" {
			return lead[i+1:]
		}
		start = s.valEnd
	}
	return ""
}

func skipSpace(src []byte, i int) int {
	for i < len(src) && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
		i++
	}
	return i
}

func skipUntil(src []byte, i int, b byte) int {
	for i < len(src) && src[i] != b {
		i++
	}
	return i
}

// render encodes value as it would appear as a top-level Member's value.
func (d *Document) render(value any) (string, error) {
	var compact bytes.Buffer
	if err := EncodeValue(&compact, value); err != nil {
		return "", err
	}
	if d.indent == "" || containsPreservedJSON(value) {
		return compact.String(), nil
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact.Bytes(), d.indent, d.indent); err != nil {
		return "", err
	}
	// JSON strings cannot hold a raw newline, so every one here is layout.
	return strings.ReplaceAll(out.String(), "\n", d.newline), nil
}

func (d *Document) span(key string) (int, bool) {
	for i := len(d.spans) - 1; i >= 0; i-- {
		if d.spans[i].Key == key {
			return i, true
		}
	}
	return 0, false
}

// Set replaces the value of a top-level Member in place, or adds the Member
// after the last one.
// Set replaces or appends a top-level member while retaining unrelated bytes.
func (d *Document) Set(key string, value any) error {
	text, err := d.render(value)
	if err != nil {
		return err
	}
	d.Root.Set(key, value)
	if i, ok := d.span(key); ok {
		d.edits = append(d.edits, edit{d.spans[i].valStart, d.spans[i].valEnd, text})
		return nil
	}
	var name bytes.Buffer
	if err := encodeString(&name, key); err != nil {
		return err
	}
	if d.indent == "" {
		entry := name.String() + ":" + text
		if len(d.spans) == 0 {
			d.edits = append(d.edits, edit{d.close, d.close, entry})
		} else {
			d.edits = append(d.edits, edit{d.spans[len(d.spans)-1].valEnd, d.spans[len(d.spans)-1].valEnd, "," + entry})
		}
		return nil
	}
	entry := d.newline + d.indent + name.String() + ": " + text
	if len(d.spans) > 0 {
		end := d.spans[len(d.spans)-1].valEnd
		d.edits = append(d.edits, edit{end, end, "," + entry})
		return nil
	}
	// An empty root Object: lay it out afresh, keeping any members added
	// before this one in the same edit.
	if n := len(d.edits); n > 0 && d.edits[n-1].start == d.open+1 && d.edits[n-1].end == d.close {
		d.edits[n-1].text = strings.TrimSuffix(d.edits[n-1].text, d.newline) + "," + entry + d.newline
		return nil
	}
	d.edits = append(d.edits, edit{d.open + 1, d.close, entry + d.newline})
	return nil
}

// SetBefore adds the Member key just before the existing Member before, or
// like set when there is no such Member. key must not exist yet.
func (d *Document) SetBefore(before, key string, value any) error {
	i, ok := d.span(before)
	if !ok {
		return d.Set(key, value)
	}
	text, err := d.render(value)
	if err != nil {
		return err
	}
	var name bytes.Buffer
	if err := encodeString(&name, key); err != nil {
		return err
	}
	for j, m := range d.Root.Members {
		if m.Key == before {
			d.Root.Members = append(d.Root.Members[:j], append([]Member{{key, value}}, d.Root.Members[j:]...)...)
			break
		}
	}
	entry := name.String() + ":" + text + ","
	if d.indent != "" {
		entry = name.String() + ": " + text + "," + d.newline + d.indent
	}
	d.edits = append(d.edits, edit{d.spans[i].keyStart, d.spans[i].keyStart, entry})
	return nil
}

// Remove deletes a top-level Member together with the separator before it
// (or, for the first Member, the one after it), so a Member set added is
// removed without a trace.
// Remove deletes one top-level member while retaining unrelated bytes.
func (d *Document) Remove(key string) {
	i, ok := d.span(key)
	if !ok {
		return
	}
	d.Root.Remove(key, 0)
	s := d.spans[i]
	switch {
	case i > 0:
		d.edits = append(d.edits, edit{d.spans[i-1].valEnd, s.valEnd, ""})
	case len(d.spans) > 1:
		d.edits = append(d.edits, edit{s.keyStart, d.spans[1].keyStart, ""})
	default:
		d.edits = append(d.edits, edit{d.open + 1, d.close, ""})
	}
}

// Clear empties the root Object, replacing every edit made so far.
func (d *Document) Clear() {
	d.Root.Members = nil
	d.edits = []edit{{d.open + 1, d.close, ""}}
}

// Bytes applies the edits. Edits never overlap: each touches one Member.
// Two insertions at one offset keep the order they were made in: the later
// is applied first, so it ends up after the earlier.
func (d *Document) Bytes() []byte {
	edits := make([]edit, 0, len(d.edits))
	for i := len(d.edits) - 1; i >= 0; i-- {
		edits = append(edits, d.edits[i])
	}
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := append([]byte(nil), d.src...)
	for _, e := range edits {
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
	}
	if d.created {
		out = append(out, '\n')
	}
	return out
}

// Problem extracts structured refusal details from an editor error.
func Problem(err error) (line, column int, reason string) {
	var e *configError
	if errors.As(err, &e) {
		return e.line, e.column, e.reason
	}
	return 0, 0, ""
}

// Preserved native settings nodes keep their original internal whitespace.
// Indenting the containing value would rewrite those bytes.
func containsPreservedJSON(value any) bool {
	switch v := value.(type) {
	case json.RawMessage:
		return true
	case []any:
		return slices.ContainsFunc(v, containsPreservedJSON)
	case *Object:
		for _, member := range v.Members {
			if containsPreservedJSON(member.Value) {
				return true
			}
		}
	}
	return false
}
