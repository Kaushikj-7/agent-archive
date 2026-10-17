package agentapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// DecodeReferencedSourceLeased charges decompressed wire data before assembling
// records. The returned release owns that charge for the bundle's lifetime.
// It is a logical data bound: encoding/json map overhead and process RSS are
// not proportional to wire size and are not claimed to be bounded by it.
func DecodeReferencedSourceLeased(ctx context.Context, metadata archive.Metadata, data []byte, limits SourceReadLimits, budget *NativeReadBudget) (archive.SourceBundle, func(), error) {
	return decodeSourceLeased(ctx, data, limits, budget, func() (archive.SourceBundle, error) {
		return DecodeReferencedSource(ctx, metadata, data, limits)
	})
}

// DecodeRevisionSourceLeased is the same ownership contract for an alternative.
func DecodeRevisionSourceLeased(ctx context.Context, metadata archive.Metadata, revision string, data []byte, limits SourceReadLimits, budget *NativeReadBudget) (archive.SourceBundle, func(), error) {
	return decodeSourceLeased(ctx, data, limits, budget, func() (archive.SourceBundle, error) {
		return DecodeRevisionSource(ctx, metadata, revision, data, limits)
	})
}

var errDecodeBudget = ReadBudgetLimit(errors.New("retained source exceeds shared data budget"))

func decodeSourceLeased(ctx context.Context, data []byte, limits SourceReadLimits, budget *NativeReadBudget, decode func() (archive.SourceBundle, error)) (archive.SourceBundle, func(), error) {
	reservation := budget.Reservation()
	noop := func() {}
	if err := ctx.Err(); err != nil {
		return archive.SourceBundle{}, noop, err
	}
	// Reserve the bounded gzip/counting and scanner scratch before opening
	// the stream. Preflight reads wire bytes only, never JSON objects.
	const preflightScratch = 64 << 10
	if !reservation.Reserve(preflightScratch) {
		return archive.SourceBundle{}, noop, errDecodeBudget
	}
	wire, longest, err := sourceWireSize(ctx, data, int64(limits.UncompressedLimit()))
	reservation.Release(preflightScratch)
	if err != nil {
		return archive.SourceBundle{}, noop, err
	}
	// Match the scanner's geometric growth, capped by the format line limit.
	lineCapacity := int64(64 << 10)
	for lineCapacity < longest+1 && lineCapacity < int64(archive.MaxSourceLineBytes+1) {
		lineCapacity = min(lineCapacity*2, int64(archive.MaxSourceLineBytes+1))
	}
	// The scanner and the decoded raw record line coexist before its map
	// is adopted by the returned bundle. Charge both independent byte owners.
	scratch := lineCapacity + longest + preflightScratch
	if !reservation.Reserve(wire + scratch) {
		return archive.SourceBundle{}, noop, errDecodeBudget
	}
	bundle, err := decode()
	reservation.Release(scratch)
	if err != nil {
		reservation.Release(wire)
		return archive.SourceBundle{}, noop, err
	}
	var once sync.Once
	release := func() { once.Do(func() { reservation.Release(wire) }) }
	return bundle, release, nil
}

func sourceWireSize(ctx context.Context, data []byte, limit int64) (int64, int64, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = gz.Close() }()
	var scratch [32 << 10]byte
	var total, longest, line int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		n, err := gz.Read(scratch[:])
		total += int64(n)
		for _, b := range scratch[:n] {
			if b == '\n' {
				longest = max(longest, line)
				line = 0
			} else {
				line++
			}
		}
		if max(longest, line) > int64(archive.MaxSourceLineBytes) {
			return 0, 0, archive.ErrSourceTooLarge
		}
		if total > limit {
			return 0, 0, archive.ErrSourceTooLarge
		}
		if errors.Is(err, io.EOF) {
			return total, max(longest, line), nil
		}
		if err != nil {
			return 0, 0, err
		}
	}
}
