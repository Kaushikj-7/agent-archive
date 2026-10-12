package codex

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

// lineReaderScratch is the read buffer every chargedLineReader holds. A
// record that fits in it is returned from that buffer without a copy.
const lineReaderScratch int64 = 4096

// chargedLineReader streams newline-delimited records. Memory follows the
// longest record read so far, never the file's length: one line buffer grows
// (to at most limit+1 bytes) only for a record longer than the scratch
// buffer, and each growth is reserved from the pass ledger before it is
// allocated, the old and the new storage both while the copy runs. A
// refusal is a shared budget refusal (agentapi.ErrReadBudget); a record
// longer than limit is archive.ErrRecordTooLarge. Parsed values stay outside
// this charge.
type chargedLineReader struct {
	p          *relatedSourcePass
	in         *bufio.Reader
	limit      int64
	line       []byte
	lineCharge int64
	charged    bool
	done       bool
	// err is the failure that ended the records: it is returned again,
	// never a later record past a refused or oversized one.
	err error
}

func (p *relatedSourcePass) newChargedLineReader(r io.Reader, limit int64) (*chargedLineReader, error) {
	if !p.reserve(lineReaderScratch) {
		return nil, agentapi.ReadBudgetLimit(errors.New("native scanner scratch budget exhausted"))
	}
	return &chargedLineReader{p: p, in: bufio.NewReaderSize(r, int(lineReaderScratch)), limit: max(limit, 0), charged: true}, nil
}

// next returns the next record without its line ending, and false at the
// end of input. The record is valid until the following call. A failure
// ends the records: every later call returns it again.
func (c *chargedLineReader) next(ctx context.Context) ([]byte, bool, error) {
	if c.err != nil {
		return nil, false, c.err
	}
	record, more, err := c.read(ctx)
	if err != nil {
		c.err = err
	}
	return record, more, err
}

func (c *chargedLineReader) read(ctx context.Context) ([]byte, bool, error) {
	if c.done || !c.charged {
		return nil, false, nil
	}
	c.line = c.line[:0]
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		part, err := c.in.ReadSlice('\n')
		full := errors.Is(err, bufio.ErrBufferFull)
		if err != nil && !full && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		size := int64(len(c.line) + len(part))
		if bytes.HasSuffix(part, []byte{'\n'}) {
			size--
		}
		if size > c.limit {
			return nil, false, agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge)
		}
		record := part
		if len(c.line) > 0 || full {
			if err := c.grow(len(c.line) + len(part)); err != nil {
				return nil, false, err
			}
			c.line = append(c.line, part...)
			if full {
				continue
			}
			record = c.line
		}
		if err != nil {
			c.done = true
			if len(record) == 0 {
				return nil, false, nil
			}
		}
		record = bytes.TrimSuffix(record, []byte{'\n'})
		return bytes.TrimSuffix(record, []byte{'\r'}), true, nil
	}
}

func (c *chargedLineReader) grow(needed int) error {
	if needed <= cap(c.line) {
		return nil
	}
	capacity := min(c.limit+1, max(int64(needed), 2*int64(cap(c.line))))
	if !c.p.reserve(capacity) {
		return agentapi.ReadBudgetLimit(errors.New("native record buffer budget exhausted"))
	}
	grown := make([]byte, len(c.line), capacity)
	copy(grown, c.line)
	c.p.release(c.lineCharge)
	c.lineCharge = capacity
	c.line = grown
	return nil
}

// close releases every charge; the reader returns nothing afterwards.
func (c *chargedLineReader) close() {
	if c == nil || !c.charged {
		return
	}
	c.charged = false
	c.p.release(c.lineCharge + lineReaderScratch)
	c.lineCharge = 0
	c.line = nil
	c.in = nil
}

// scanChargedLines visits each record of reader until visit reports true.
func (p *relatedSourcePass) scanChargedLines(ctx context.Context, reader io.Reader, limit int64, visit func([]byte) bool) error {
	lines, err := p.newChargedLineReader(reader, limit)
	if err != nil {
		return err
	}
	defer lines.close()
	for {
		line, more, err := lines.next(ctx)
		if err != nil || !more || visit(line) {
			return err
		}
	}
}
