package state

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// pendingFenceViewLimit bounds what a fence view keeps of a pending
// publication: its proofs and catalog commit, each a few KiB at most.
const pendingFenceViewLimit = 1 << 20

// pendingFenceBuffer bounds the read buffer a fence view scans the file with.
const pendingFenceBuffer = 32 << 10

// errPendingFenceView refuses a pending publication whose fence view cannot be
// read: malformed, or its kept fields past pendingFenceViewLimit.
var errPendingFenceView = errors.Join(ErrDurableStorageRecovery, errors.New("pending publication fence fields unreadable"))

// pendingFenceFields are the fields of a pending publication its fences read:
// the proofs and the catalog commit it carries and, inside the bundle, the
// session it belongs to, decoded by PendingPublication's own rules. The
// fields a future writer's publication carries (the legacy decoder's refusals)
// are kept too, so the view refuses such a publication as a whole decode
// does. Everything else (the bundle's records, the source and metadata
// bytes, the history journal) is scanned past, not decoded.
var pendingFenceFields = jsonProjection{
	"cursor_commit4":   nil,
	"journal_version":  nil,
	"staged_commit3":   nil,
	"selection_commit": nil,
	"commit":           nil,
	"bundle":           {"archive_session_id": nil},
	"sources":          nil,
	"phase":            nil,
	"preparation":      nil,
	"progress":         nil,
	"cleanup":          nil,
	"admission_stage":  nil,
	// The staged-selection shape check compares the covered request with it.
	"request_token": nil,
}

// pendingFenceView reads only the fence fields (pendingFenceFields) of a
// session's pending publication. The rest of the returned publication is
// empty. Its charge is the scan's read buffer and the kept fields, released
// before it returns, never the file's size: a pending publication carries
// the whole compressed source and the bundle's records, which a fence never
// looks at. A staged import's proof is decoded closed, refusing any field
// it does not know anywhere in the publication, so a publication carrying
// one is read whole (catalogPendingForFence), as before; it is bounded by
// the import's own stage. The view validates JSON syntax in fields it scans
// past, but only a whole read (LoadPending) checks their typed values.
func (s *Store) pendingFenceView(id string) (PendingPublication, bool, error) {
	return s.pendingFenceViewContext(s.durableContext(), id)
}

func (s *Store) pendingFenceViewContext(ctx context.Context, id string) (PendingPublication, bool, error) {
	read, ctx, closeContext := s.withDurableContext(ctx)
	defer closeContext()
	s = read
	var pending PendingPublication
	// Source-set publication is a closed protocol: its full preparation and
	// source graph must be validated, including fields legacy fences refuse.
	// Probe only its discriminator with bounded memory before the legacy view.
	var marker struct {
		Version int `json:"journal_version"`
	}
	if err := s.readProjectedContext(ctx, s.pendingPath(id), jsonProjection{"journal_version": nil}, &marker); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pending, false, nil
		}
		return pending, false, err
	}
	if marker.Version == 5 {
		return s.catalogPendingForFence(id)
	}
	err := s.readProjectedContext(ctx, s.pendingPath(id), pendingFenceFields, &pending)
	if errors.Is(err, os.ErrNotExist) {
		return pending, false, nil
	}
	if err == nil && pending.StagedProof() != nil {
		return s.catalogPendingForFence(id)
	}
	if err == nil {
		err = pending.ValidateReadySlot(id)
	}
	return pending, err == nil, err
}

// readProjected decodes into value only the fields of the JSON object at path
// that projection names, scanning past the others.
func (s *Store) readProjected(path string, projection jsonProjection, value any) error {
	return s.readProjectedContext(s.durableContext(), path, projection, value)
}

func (s *Store) readProjectedContext(ctx context.Context, path string, projection jsonProjection, value any) error {
	read, ctx, closeContext := s.withDurableContext(ctx)
	defer closeContext()
	s = read
	if err := s.durableContext().Err(); err != nil {
		return err
	}
	owner, err := os.OpenRoot(s.home)
	if err != nil {
		return err
	}
	defer func() { _ = owner.Close() }()
	rel, err := filepath.Rel(s.home, path)
	if err != nil {
		return err
	}
	if !filepath.IsLocal(rel) {
		return errStateFileUnstable
	}
	f, err := owner.Open(rel)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	n := info.Size()
	if !info.Mode().IsRegular() || n < 0 {
		return errStateFileUnstable
	}
	return s.readProjectedObserved(ctx, f, n, projection, value)
}

func (s *Store) readProjectedObserved(ctx context.Context, reader io.Reader, n int64, projection jsonProjection, value any) error {
	read, ctx, closeContext := s.withDurableContext(ctx)
	defer closeContext()
	s = read
	if err := ctx.Err(); err != nil {
		return err
	}
	if n < 0 {
		return errStateFileUnstable
	}
	reservation := s.readReservation()
	buffer := min(max(n, 16), pendingFenceBuffer)
	if !reservation.Reserve(buffer) {
		return errStateBudget
	}
	defer reservation.Release(buffer)
	// One extra byte detects growth without following an unbounded appended tail.
	limit := n
	if limit < math.MaxInt64 {
		limit++
	}
	counted := &countingReader{r: &projectedContextReader{ctx: ctx, reader: io.LimitReader(reader, limit)}}
	p := projector{in: bufio.NewReaderSize(counted, int(buffer)), reserve: reservation.Reserve, limit: pendingFenceViewLimit, ctx: ctx.Err}
	defer func() { reservation.Release(p.charged) }()
	if err := p.object(projection, true); err != nil {
		return err
	}
	if err := p.trailing(); err != nil {
		return err
	}
	if counted.n != n {
		return errStateFileUnstable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := json.Unmarshal(p.out, value); err != nil {
		return errors.Join(errPendingFenceView, err)
	}
	return ctx.Err()
}

// Every buffer refill observes the original read scope, including while the
// parser is consuming trailing whitespace rather than an object value.
type projectedContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *projectedContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// jsonProjection names the members of a JSON object to keep: a nil value
// keeps the member's whole value, a non-nil one keeps only the named members
// of the object it holds.
type jsonProjection map[string]jsonProjection

// member reports whether projection keeps the member name, matched as
// encoding/json matches a field (exactly, else ignoring case), so the view
// keeps every member a full decode would read into a kept field.
func (projection jsonProjection) member(name string, long bool) (jsonProjection, bool) {
	if long {
		return nil, false
	}
	if nested, ok := projection[name]; ok {
		return nested, true
	}
	for field, nested := range projection {
		if strings.EqualFold(field, name) {
			return nested, true
		}
	}
	return nil, false
}

// projector streams one JSON value, copying to out only what a projection
// keeps and charging out as it grows. Skipped values are checked for their
// structure, not decoded.
type projector struct {
	in      *bufio.Reader
	out     []byte
	charged int64
	limit   int
	reserve func(int64) bool
	ctx     func() error
	scanned int
	depth   int
}

func (p *projector) emit(b ...byte) error {
	if len(p.out)+len(b) > p.limit {
		return errPendingFenceView
	}
	if need := int64(len(p.out) + len(b)); need > p.charged {
		if !p.reserve(need - p.charged) {
			return errStateBudget
		}
		p.charged = need
	}
	p.out = append(p.out, b...)
	return nil
}

func (p *projector) next() (byte, error) {
	c, err := p.in.ReadByte()
	if errors.Is(err, io.EOF) {
		return 0, errors.Join(errPendingFenceView, io.ErrUnexpectedEOF)
	}
	if err != nil {
		return 0, err
	}
	if p.scanned++; p.scanned >= 1<<20 {
		p.scanned = 0
		if err := p.ctx(); err != nil {
			return 0, err
		}
	}
	return c, nil
}

func (p *projector) nonSpace() (byte, error) {
	for {
		c, err := p.next()
		if err != nil {
			return 0, err
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return c, nil
		}
	}
}

// trailing accepts only whitespace after the value.
func (p *projector) trailing() error {
	for {
		c, err := p.in.ReadByte()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return errPendingFenceView
		}
	}
}

// object reads an object whose opening brace is next (or, when open is
// false, already read) and copies the members projection keeps.
func (p *projector) object(projection jsonProjection, open bool) error {
	if err := p.startObject(open); err != nil {
		return err
	}
	defer func() { p.depth-- }()
	kept := 0
	c, err := p.nonSpace()
	if err != nil {
		return err
	}
	if c == '}' {
		return p.emit('}')
	}
	for {
		if c != '"' {
			return errPendingFenceView
		}
		key, long := []byte{'"'}, false
		if err := p.str(func(b byte) error {
			// No kept name is this long; such a member is scanned past.
			if long = long || len(key) > 256; !long {
				key = append(key, b)
			}
			return nil
		}); err != nil {
			return err
		}
		var name string
		if !long {
			if err := json.Unmarshal(append(key, '"'), &name); err != nil {
				return errors.Join(errPendingFenceView, err)
			}
		}
		if c, err = p.nonSpace(); err != nil {
			return err
		}
		if c != ':' {
			return errPendingFenceView
		}
		nested, keep := projection.member(name, long)
		if keep {
			if kept > 0 {
				if err := p.emit(','); err != nil {
					return err
				}
			}
			kept++
			encoded, _ := json.Marshal(name)
			if err := p.emit(append(encoded, ':')...); err != nil {
				return err
			}
		}
		if c, err = p.nonSpace(); err != nil {
			return err
		}
		switch {
		case keep && nested != nil && c == '{':
			err = p.object(nested, false)
		case keep:
			err = p.value(c, p.emit)
		default:
			err = p.value(c, func(...byte) error { return nil })
		}
		if err != nil {
			return err
		}
		if c, err = p.nonSpace(); err != nil {
			return err
		}
		switch c {
		case ',':
			if c, err = p.nonSpace(); err != nil {
				return err
			}
		case '}':
			return p.emit('}')
		default:
			return errPendingFenceView
		}
	}
}

func (p *projector) startObject(open bool) error {
	if p.depth++; p.depth > 10000 {
		return errPendingFenceView
	}
	if open {
		c, err := p.nonSpace()
		if err != nil {
			return err
		}
		if c != '{' {
			return errPendingFenceView
		}
	}
	return p.emit('{')
}

// value reads one value whose first byte is first, passing its bytes to
// copyOut.
func (p *projector) value(first byte, copyOut func(...byte) error) error {
	switch first {
	case '"':
		if err := copyOut('"'); err != nil {
			return err
		}
		if err := p.str(func(b byte) error { return copyOut(b) }); err != nil {
			return err
		}
		return copyOut('"')
	case '{', '[':
		return p.container(first, copyOut)
	default:
		return p.scalar(first, copyOut)
	}
}

// container reads an object or array whose opening byte (first) was read,
// passing its bytes to copyOut. Nesting is bounded as encoding/json bounds
// it.
func (p *projector) container(first byte, copyOut func(...byte) error) error {
	if p.depth++; p.depth > 10000 {
		return errPendingFenceView
	}
	defer func() { p.depth-- }()
	closing := byte('}')
	if first == '[' {
		closing = ']'
	}
	if err := copyOut(first); err != nil {
		return err
	}
	c, err := p.nonSpace()
	if err != nil {
		return err
	}
	if c == closing {
		return copyOut(c)
	}
	for {
		if first == '{' {
			if c, err = p.member(c, copyOut); err != nil {
				return err
			}
		}
		if err := p.value(c, copyOut); err != nil {
			return err
		}
		if c, err = p.nonSpace(); err != nil {
			return err
		}
		if err := copyOut(c); err != nil {
			return err
		}
		if c == closing {
			return nil
		}
		if c != ',' {
			return errPendingFenceView
		}
		if c, err = p.nonSpace(); err != nil {
			return err
		}
	}
}

// member reads an object member's name, whose first byte (c) was read, and
// its colon, passing them to copyOut, and returns the first byte of its
// value.
func (p *projector) member(c byte, copyOut func(...byte) error) (byte, error) {
	if c != '"' {
		return 0, errPendingFenceView
	}
	if err := p.value(c, copyOut); err != nil {
		return 0, err
	}
	c, err := p.nonSpace()
	if err != nil {
		return 0, err
	}
	if c != ':' {
		return 0, errPendingFenceView
	}
	if err := copyOut(':'); err != nil {
		return 0, err
	}
	return p.nonSpace()
}

// scalar reads a number or a literal whose first byte (first) was read: its
// bytes run to the next delimiter, which is left unread.
func (p *projector) scalar(first byte, copyOut func(...byte) error) error {
	// Literals have fixed spellings. Numbers follow JSON's grammar without
	// buffering their potentially unbounded digits.
	literal := ""
	switch first {
	case 't':
		literal = "true"
	case 'f':
		literal = "false"
	case 'n':
		literal = "null"
	}
	// States mark the start, sign, zero, integer, decimal point, fraction,
	// exponent marker, exponent sign, and exponent digits respectively.
	state, position := byte('s'), 0
	c := first
	for {
		if literal != "" {
			if position >= len(literal) || c != literal[position] {
				return errPendingFenceView
			}
			position++
		} else {
			var err error
			state, err = numberState(state, c)
			if err != nil {
				return err
			}
		}
		if err := copyOut(c); err != nil {
			return err
		}
		var err error
		c, err = p.next()
		if err != nil {
			return err
		}
		if c == ',' || c == '}' || c == ']' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if literal != "" && position != len(literal) || literal == "" && state != '0' && state != 'i' && state != 'f' && state != 'x' {
				return errPendingFenceView
			}
			return p.in.UnreadByte()
		}
	}
}

// numberState advances JSON number grammar without retaining digits.
func numberState(state, c byte) (byte, error) {
	switch state {
	case 's', '-':
		switch {
		case c == '-' && state == 's':
			return '-', nil
		case c == '0':
			return '0', nil
		case c >= '1' && c <= '9':
			return 'i', nil
		}
	case '0', 'i', 'f':
		return numberMantissaState(state, c)
	case '.', 'e', '+':
		if (c == '+' || c == '-') && state == 'e' {
			return '+', nil
		}
		if c >= '0' && c <= '9' {
			if state == '.' {
				return 'f', nil
			}
			return 'x', nil
		}
	case 'x':
		if c >= '0' && c <= '9' {
			return 'x', nil
		}
	}
	return 0, errPendingFenceView
}

func numberMantissaState(state, c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9' && state != '0':
		return state, nil
	case c == '.' && state != 'f':
		return '.', nil
	case c == 'e' || c == 'E':
		return 'e', nil
	default:
		return 0, errPendingFenceView
	}
}

// str reads a string's contents after its opening quote through its closing
// quote, passing each content byte (escapes as written) to each.
func (p *projector) str(each func(byte) error) error {
	escaped := false
	hexLeft := 0
	for {
		c, err := p.next()
		if err != nil {
			return err
		}
		switch {
		case hexLeft > 0:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return errPendingFenceView
			}
			hexLeft--
		case escaped:
			escaped = false
			switch c {
			case 'u':
				hexLeft = 4
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			default:
				return errPendingFenceView
			}
		case c == '\\':
			escaped = true
		case c == '"':
			return nil
		case c < 0x20:
			return errPendingFenceView
		}
		if err := each(c); err != nil {
			return err
		}
	}
}
