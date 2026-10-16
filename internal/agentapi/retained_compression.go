package agentapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"sync"
)

// CompressRetainedSource uses the canonical codec with the caller's shared
// ledger; the returned bytes are owned until release.
func CompressRetainedSource(ctx context.Context, bundle archive.SourceBundle, budget *NativeReadBudget) (archive.CompressedSource, func(), error) {
	reservation := budget.Reservation()
	if budget == nil {
		return archive.CompressedSource{}, nil, ErrReadBudget
	}
	if bundle.SchemaVersion != archive.SourceSchemaVersion && bundle.SchemaVersion != archive.HistorySourceSchemaVersion {
		return archive.CompressedSource{}, nil, errors.New("unsupported retained source schema")
	}
	if err := bundle.ValidateHistory(); err != nil {
		return archive.CompressedSource{}, nil, err
	}
	const compressorScratch = 1 << 20
	const sizingScratch = 32 << 10
	if !reservation.Reserve(sizingScratch) {
		return archive.CompressedSource{}, nil, ErrReadBudget
	}
	available := reservation.Available()
	largest, err := archive.SourceEncodingLineBound(ctx, bundle, available)
	if errors.Is(err, agentmeta.ErrJSONWireLimit) {
		reservation.Refuse(available + 1)
	}
	reservation.Release(sizingScratch)
	if err != nil {
		return archive.CompressedSource{}, nil, errors.Join(ErrReadBudget, err)
	}
	if largest > (reservation.Available()-compressorScratch)/2 {
		reservation.Refuse(2*largest + compressorScratch)
		return archive.CompressedSource{}, nil, ErrReadBudget
	}
	scratch := 2*largest + compressorScratch
	if !reservation.Reserve(scratch) {
		return archive.CompressedSource{}, nil, ErrReadBudget
	}
	defer reservation.Release(scratch)
	writer := &retainedCompressedWriter{ctx: ctx, reservation: reservation}
	var once sync.Once
	release := func() { once.Do(func() { reservation.Release(writer.charged) }) }
	if err = archive.CompressSource(writer, bundle); err != nil {
		release()
		return archive.CompressedSource{}, nil, err
	}
	sum := sha256.Sum256(writer.Bytes())
	return archive.CompressedSource{Bytes: writer.Bytes(), SHA256: hex.EncodeToString(sum[:])}, release, nil
}

type retainedCompressedWriter struct {
	bytes.Buffer
	ctx         context.Context
	reservation *NativeReadReservation
	charged     int64
}

func (w *retainedCompressedWriter) Write(raw []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n := int64(len(raw))
	if n > (128<<20)-int64(w.Len()) || !w.reservation.Reserve(n) {
		return 0, ErrReadBudget
	}
	w.charged += n
	return w.Buffer.Write(raw)
}
