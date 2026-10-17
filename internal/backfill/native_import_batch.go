package backfill

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

// decodeBatch preserves malformed private authority before generic JSON syntax
// checks; an unreadable confirmation is never treated as a resumable legacy run.
func decodeBatch(raw []byte, b *Batch) error {
	var control archive.NativeImportBatchControl
	if err := archive.DecodeNativeImportBatchControl(raw, &control); err != nil {
		return err
	}
	type plain Batch
	var next plain
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	*b = Batch(next)
	return nil
}

func (b *Batch) UnmarshalJSON(raw []byte) error { return decodeBatch(raw, b) }

func readBatch(path string, b *Batch) (err error) {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	raw, err := io.ReadAll(io.LimitReader(f, archive.MaxImportBatchControlBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > archive.MaxImportBatchControlBytes {
		return archive.ErrUnsupportedNativeImport
	}
	return decodeBatch(raw, b)
}

func (b Batch) nativeImportControl() archive.NativeImportBatchControl {
	return archive.NativeImportBatchControl{ID: b.ID, StartedAt: b.StartedAt, CompletedAt: b.CompletedAt, Filters: archive.ImportBatchFilters(b.Filters), DestinationID: b.DestinationID, UndoneAt: b.UndoneAt, NativeImport: b.NativeImport}
}

func validateBatchReplacement(ctx context.Context, before *Batch, after Batch) error {
	if before == nil || before.NativeImport == nil {
		if after.NativeImport != nil {
			return archive.ErrUnsupportedNativeImport
		}
		return nil
	}
	old := before.nativeImportControl()
	next := after.nativeImportControl()
	if err := old.Validate(ctx); err != nil {
		return err
	}
	if err := next.Validate(ctx); err != nil {
		return err
	}
	oldRaw, err := json.Marshal(old.NativeImport)
	if err != nil {
		return err
	}
	nextRaw, err := json.Marshal(next.NativeImport)
	if err != nil || string(oldRaw) != string(nextRaw) {
		return errors.Join(archive.ErrUnsupportedNativeImport, err)
	}
	if old.UndoneAt != nil && (next.UndoneAt == nil || !old.UndoneAt.Equal(*next.UndoneAt)) {
		return archive.ErrUnsupportedNativeImport
	}
	return nil
}

// Generic saves cannot create confirmation. A final reread under the same lock
// as undo/publication prevents a stale writer from clearing an undo marker.
func saveBatchControl(ctx context.Context, home string, b Batch) error {
	var before Batch
	err := readBatch(batchPath(home, b.ID), &before)
	found := !errors.Is(err, os.ErrNotExist)
	if err != nil && found {
		return err
	}
	var previous *Batch
	if found {
		previous = &before
	}
	if err := validateBatchReplacement(ctx, previous, b); err != nil {
		return err
	}
	if _, err := agentmeta.JSONWireBound(ctx, b, archive.MaxImportBatchControlBytes-1); err != nil {
		return err
	}
	return state.ReplaceImportBatchControl(ctx, home, b.ID, func(w io.Writer) error {
		limited := &batchControlWriter{writer: w, remaining: archive.MaxImportBatchControlBytes}
		encoder := json.NewEncoder(limited)
		encoder.SetIndent("", "  ")
		return encoder.Encode(b)
	}, func(raw []byte, currentFound bool) error {
		var current Batch
		if currentFound {
			if err := decodeBatch(raw, &current); err != nil {
				return err
			}
		}
		if err := validateBatchReplacement(ctx, batchIfFound(current, currentFound), b); err != nil {
			return err
		}
		if current.NativeImport != nil || b.NativeImport != nil {
			if found != currentFound {
				return archive.ErrUnsupportedNativeImport
			}
			beforeWire, err := json.Marshal(before)
			if err != nil {
				return err
			}
			currentWire, err := json.Marshal(current)
			if err != nil || !bytes.Equal(beforeWire, currentWire) {
				return errors.Join(archive.ErrUnsupportedNativeImport, err)
			}
		}
		return nil
	})
}

func batchIfFound(b Batch, found bool) *Batch {
	if !found {
		return nil
	}
	return &b
}

type batchControlWriter struct {
	writer    io.Writer
	remaining int
}

func (w *batchControlWriter) Write(raw []byte) (int, error) {
	if len(raw) > w.remaining {
		return 0, fmt.Errorf("import control exceeds %d bytes", archive.MaxImportBatchControlBytes)
	}
	n, err := w.writer.Write(raw)
	w.remaining -= n
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	return n, err
}
