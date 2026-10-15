package sourceio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/trace"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

// Reader bounds input to its observation and checks cancellation on every read.
func Reader(ctx context.Context, f agentapi.FileInput, length int64) *contextReader {
	return &contextReader{ctx: ctx, r: io.NewSectionReader(f, 0, length)}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
	err error
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = errors.Join(r.err, err)
	}
	return n, err
}

// ReadError preserves I/O failures that legacy privacy codecs replace with a safe refusal.
func (r *contextReader) ReadError() error { return Classify(r.err) }

type contextAt struct {
	ctx context.Context
	f   io.ReaderAt
}

func (r contextAt) ReadAt(p []byte, o int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.f.ReadAt(p, o)
}

// FilterJSONL preserves complete-full-source framing independently of preview Records.
func FilterJSONL(ctx context.Context, in agentapi.NativeInput, c agentapi.FilterContext, filter func(io.Reader) (archive.FilteredTranscript, error)) (archive.FilteredTranscript, error) {
	if in.File == nil {
		return archive.FilteredTranscript{}, errors.New("file input required")
	}
	if err := ctx.Err(); err != nil {
		return archive.FilteredTranscript{}, err
	}
	limit := c.Limits.RecordBytes
	if limit <= 0 {
		limit = archive.MaxRecordBytes
	}
	boundary, err := transcriptio.CompleteJSONLBoundary(contextAt{ctx, in.File}, in.File.Length(), limit)
	if err != nil {
		return archive.FilteredTranscript{}, errors.Join(Classify(err), Classify(in.File.Check()), ctx.Err())
	}
	input := Reader(ctx, in.File, boundary)
	measured, finish := trace.CountReads(input, trace.FilterBytes)
	defer finish()
	r := &recordReader{r: measured, limit: limit}
	out, err := filter(r)
	if r.exceeded {
		return archive.FilteredTranscript{}, errors.Join(agentapi.Wrap(agentapi.Limit, archive.ErrRecordTooLarge), input.ReadError(), Classify(in.File.Check()), ctx.Err())
	}
	if err != nil {
		return out, errors.Join(Classify(err), input.ReadError(), Classify(in.File.Check()), ctx.Err())
	}
	return out, errors.Join(input.ReadError(), Classify(in.File.Check()), ctx.Err())
}

type recordReader struct {
	r        io.Reader
	limit    int64
	line     int64
	exceeded bool
}

func (l *recordReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	for data := p[:n]; len(data) > 0; {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			l.line += int64(len(data))
			break
		}
		if l.line+int64(i) > l.limit {
			l.exceeded = true
			return 0, archive.ErrRecordTooLarge
		}
		l.line = 0
		data = data[i+1:]
	}
	if l.line > l.limit {
		l.exceeded = true
		return 0, archive.ErrRecordTooLarge
	}
	return n, err
}

// RefilterJSONL streams existing retained records without another whole bundle.
func RefilterJSONL(ctx context.Context, a interface {
	FilterJSONL(io.Reader) (archive.FilteredTranscript, error)
}, b archive.SourceBundle) (archive.FilteredTranscript, error) {
	if err := ctx.Err(); err != nil {
		return archive.FilteredTranscript{}, err
	}
	r := &retainedReader{ctx: ctx, records: b.NativeRecords}
	out, err := a.FilterJSONL(r)
	return out, errors.Join(err, ctx.Err())
}

type retainedReader struct {
	ctx     context.Context
	records []map[string]any
	next    int
	row     *bytes.Reader
}

func (r *retainedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	for {
		if r.row != nil {
			n, err := r.row.Read(p)
			if n > 0 {
				return n, nil
			}
			if !errors.Is(err, io.EOF) {
				return n, err
			}
		}
		if r.next == len(r.records) {
			return 0, io.EOF
		}
		b, err := json.Marshal(r.records[r.next])
		if err != nil {
			return 0, err
		}
		r.next++
		r.row = bytes.NewReader(append(b, '\n'))
	}
}

// RefilterText checks the retained format before invoking its integration-owned codec.
func RefilterText(ctx context.Context, b archive.SourceBundle, at time.Time, filter func(io.Reader, time.Time) (archive.FilteredTranscript, error)) (archive.FilteredTranscript, error) {
	if len(b.NativeText) != 1 || len(b.NativeRecords) != 0 {
		return archive.FilteredTranscript{}, errors.New("invalid retained text source")
	}
	if err := ctx.Err(); err != nil {
		return archive.FilteredTranscript{}, err
	}
	out, err := filter(&contextReader{ctx: ctx, r: strings.NewReader(b.NativeText[0].Content)}, at)
	return out, errors.Join(err, ctx.Err())
}
