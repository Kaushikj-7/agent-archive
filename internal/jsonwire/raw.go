package jsonwire

import "context"

// Raw JSON is validated without decoding or allocating a scalar-sized buffer.
type rawScan struct {
	ctx  context.Context
	data []byte
	at   int
}

func validateRaw(ctx context.Context, data []byte) error {
	s := rawScan{ctx: ctx, data: data}
	if err := s.value(0); err != nil {
		return err
	}
	if err := s.space(); err != nil {
		return err
	}
	if s.at != len(data) {
		return ErrUnsupported
	}
	return ctx.Err()
}

func (s *rawScan) space() error {
	for s.at < len(s.data) {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		b := s.data[s.at]
		if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			return nil
		}
		s.at++
	}
	return nil
}

func (s *rawScan) take(b byte) bool {
	if s.at < len(s.data) && s.data[s.at] == b {
		s.at++
		return true
	}
	return false
}

func (s *rawScan) value(depth int) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if depth > 128 {
		return ErrUnsupported
	}
	if err := s.space(); err != nil {
		return err
	}
	if s.at == len(s.data) {
		return ErrUnsupported
	}
	switch s.data[s.at] {
	case '"':
		return s.text()
	case '{':
		return s.object(depth)
	case '[':
		return s.array(depth)
	case 't':
		return s.word("true")
	case 'f':
		return s.word("false")
	case 'n':
		return s.word("null")
	default:
		return s.number()
	}
}

func (s *rawScan) word(word string) error {
	for i := range len(word) {
		if !s.take(word[i]) {
			return ErrUnsupported
		}
	}
	return nil
}

func (s *rawScan) text() error {
	if !s.take('"') {
		return ErrUnsupported
	}
	for s.at < len(s.data) {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		b := s.data[s.at]
		s.at++
		if b == '"' {
			return nil
		}
		if b < 0x20 {
			return ErrUnsupported
		}
		if b != '\\' {
			continue
		}
		if s.at == len(s.data) {
			return ErrUnsupported
		}
		b = s.data[s.at]
		s.at++
		if b == 'u' {
			for range 4 {
				if s.at == len(s.data) {
					return ErrUnsupported
				}
				b = s.data[s.at]
				s.at++
				if (b < '0' || b > '9') && (b < 'a' || b > 'f') && (b < 'A' || b > 'F') {
					return ErrUnsupported
				}
			}
			continue
		}
		if b != '"' && b != '\\' && b != '/' && b != 'b' && b != 'f' && b != 'n' && b != 'r' && b != 't' {
			return ErrUnsupported
		}
	}
	return ErrUnsupported
}

func (s *rawScan) object(depth int) error {
	s.at++
	if err := s.space(); err != nil {
		return err
	}
	if s.take('}') {
		return nil
	}
	for {
		if err := s.text(); err != nil {
			return err
		}
		if err := s.space(); err != nil {
			return err
		}
		if !s.take(':') {
			return ErrUnsupported
		}
		if err := s.value(depth + 1); err != nil {
			return err
		}
		if err := s.space(); err != nil {
			return err
		}
		if s.take('}') {
			return nil
		}
		if !s.take(',') {
			return ErrUnsupported
		}
		if err := s.space(); err != nil {
			return err
		}
	}
}

func (s *rawScan) array(depth int) error {
	s.at++
	if err := s.space(); err != nil {
		return err
	}
	if s.take(']') {
		return nil
	}
	for {
		if err := s.value(depth + 1); err != nil {
			return err
		}
		if err := s.space(); err != nil {
			return err
		}
		if s.take(']') {
			return nil
		}
		if !s.take(',') {
			return ErrUnsupported
		}
	}
}

func (s *rawScan) digits() error {
	start := s.at
	for s.at < len(s.data) && s.data[s.at] >= '0' && s.data[s.at] <= '9' {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		s.at++
	}
	if s.at == start {
		return ErrUnsupported
	}
	return nil
}

func (s *rawScan) number() error {
	s.take('-')
	if s.at == len(s.data) {
		return ErrUnsupported
	}
	if !s.take('0') {
		if s.data[s.at] < '1' || s.data[s.at] > '9' {
			return ErrUnsupported
		}
		if err := s.digits(); err != nil {
			return err
		}
	}
	if s.take('.') {
		if err := s.digits(); err != nil {
			return err
		}
	}
	if s.take('e') || s.take('E') {
		if !s.take('+') {
			s.take('-')
		}
		if err := s.digits(); err != nil {
			return err
		}
	}
	return nil
}

func validNumber(ctx context.Context, text string) bool {
	// The number string is already caller-owned; index without a byte-sized copy.
	i := 0
	if i < len(text) && text[i] == '-' {
		i++
	}
	if i == len(text) {
		return false
	}
	digits := func() bool {
		start := i
		for i < len(text) && text[i] >= '0' && text[i] <= '9' {
			if ctx.Err() != nil {
				return false
			}
			i++
		}
		return i > start
	}
	if text[i] == '0' {
		i++
	} else {
		if text[i] < '1' || text[i] > '9' || !digits() {
			return false
		}
	}
	if i < len(text) && text[i] == '.' {
		i++
		if !digits() {
			return false
		}
	}
	if i < len(text) && (text[i] == 'e' || text[i] == 'E') {
		i++
		if i < len(text) && (text[i] == '+' || text[i] == '-') {
			i++
		}
		if !digits() {
			return false
		}
	}
	return i == len(text) && ctx.Err() == nil
}
