package state

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type projectedCancellationReader struct {
	reader    io.Reader
	cancel    context.CancelFunc
	threshold int64
	read      int64
}

func (r *projectedCancellationReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += int64(n)
	if r.read >= r.threshold {
		r.cancel()
	}
	return n, err
}

func TestGenerationWriterProjectionNilBudgetCancelsDuringScan(t *testing.T) {
	t.Parallel()
	for _, where := range []string{"skipped-payload", "trailing-whitespace"} {
		t.Run(where, func(t *testing.T) {
			t.Parallel()
			raw := append([]byte(`{"version":1,"pending":{"source_bytes":"`), bytes.Repeat([]byte("x"), 4<<20)...)
			raw = append(raw, []byte(`"}}`)...)
			if where == "trailing-whitespace" {
				raw = append([]byte(`{"version":1}`), bytes.Repeat([]byte(" "), 4<<20)...)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader := &projectedCancellationReader{reader: bytes.NewReader(raw), cancel: cancel, threshold: 2 * pendingFenceBuffer}
			s := newTestStore(t)
			read, end := s.WithReadBudget(ctx, nil)
			defer end()
			var marker generationWriterMarker
			err := read.readProjectedObserved(ctx, reader, int64(len(raw)), jsonProjection{"version": nil, "prior_metadata_sha256": nil, "prior_source_set_sha256": nil}, &marker)
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal("actual midscan cancellation did not occur")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("nil-budget marker ignored cancellation: err=%v read=%d total=%d", err, reader.read, len(raw))
			}
			t.Logf("where=%s read=%d cancel_after=%d total=%d result=%v", where, reader.read, reader.threshold, len(raw), err)
			if reader.read > reader.threshold+pendingFenceBuffer {
				t.Fatalf("cancellation drained more than one additional buffer: read=%d threshold=%d", reader.read, reader.threshold)
			}
		})
	}
}

func TestGenerationWriterProjectionBoundsObservedGrowth(t *testing.T) {
	t.Parallel()
	header := []byte(`{"version":1}`)
	raw := append(bytes.Clone(header), bytes.Repeat([]byte(" "), 4<<20)...)
	s := newTestStore(t)
	var marker generationWriterMarker
	// The source reports a larger stream than the already observed regular file.
	reader := &projectedCancellationReader{reader: bytes.NewReader(raw), cancel: func() {}, threshold: int64(len(raw))}
	err := s.readProjectedObserved(t.Context(), reader, int64(len(header)), jsonProjection{"version": nil}, &marker)
	if !errors.Is(err, errStateFileUnstable) {
		t.Fatalf("growth was not refused: %v", err)
	}
	t.Logf("growth read=%d observed=%d result=%v", reader.read, len(header), err)
	if reader.read > int64(len(header))+1 {
		t.Fatalf("projection followed growing tail: read=%d observed=%d", reader.read, len(header))
	}
}

type projectedEOFCancellationReader struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r *projectedEOFCancellationReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if errors.Is(err, io.EOF) {
		r.cancel()
	}
	return n, err
}

func TestGenerationWriterProjectionPreservesCancellationAtEOF(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"version":1}`, `{"version":1`} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := newTestStore(t)
			scoped, end := s.WithReadBudget(ctx, nil)
			defer end()
			reader := &projectedEOFCancellationReader{reader: bytes.NewBufferString(raw), cancel: cancel}
			var marker generationWriterMarker
			err := scoped.readProjectedObserved(ctx, reader, int64(len(raw)), jsonProjection{"version": nil}, &marker)
			if !errors.Is(ctx.Err(), context.Canceled) || !errors.Is(err, context.Canceled) {
				t.Fatalf("EOF discarded original cancellation: context=%v result=%v", ctx.Err(), err)
			}
		})
	}
}
