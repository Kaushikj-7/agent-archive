package backfill

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

// ConfirmNativeImportBatch freezes one explicit reviewed scope in the existing
// ordinary control. The caller establishes durable configuration first and may
// already hold hooks.lock; this path never reacquires hooks or setup locks.
// Confirmation grants intent only, never source or registration authority.
func ConfirmNativeImportBatch(ctx context.Context, store *state.Store, batch Batch, expectedOrdinaryControlSHA256 string, scope archive.NativeImportBatchScope) (Batch, error) {
	if store == nil || !archive.ValidImportBatchID(batch.ID) || !nativeImportControlDigest(expectedOrdinaryControlSHA256) || batch.NativeImport != nil || batch.CompletedAt != nil || batch.UndoneAt != nil {
		return Batch{}, archive.ErrUnsupportedNativeImport
	}
	frozen, err := freezeNativeImportBatch(ctx, batch, scope)
	if err != nil {
		return Batch{}, err
	}
	if err := store.CheckNativeImportConfirmationScope(ctx, frozen.NativeImport.Scope); err != nil {
		return Batch{}, err
	}
	instance, err := local.ID()
	if err != nil {
		return Batch{}, err
	}
	frozen.NativeImport.Instance = instance
	if err := frozen.nativeImportControl().Validate(ctx); err != nil {
		return Batch{}, err
	}
	if _, err := agentmeta.JSONWireBound(ctx, frozen, archive.MaxImportBatchControlBytes); err != nil {
		return Batch{}, err
	}
	payload, err := json.Marshal(frozen)
	if err != nil {
		return Batch{}, err
	}
	err = state.ReplaceImportBatchControl(ctx, store.Home(), batch.ID, func(w io.Writer) error {
		n, err := w.Write(payload)
		if err == nil && n != len(payload) {
			err = io.ErrShortWrite
		}
		return err
	}, func(raw []byte, found bool) error {
		if err := compareNativeImportConfirmation(ctx, raw, found, expectedOrdinaryControlSHA256, frozen, payload); err != nil {
			return err
		}
		return store.CheckNativeImportConfirmationScope(ctx, frozen.NativeImport.Scope)
	})
	if err != nil {
		return Batch{}, err
	}
	return frozen, nil
}

func nativeImportControlDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func freezeNativeImportBatch(ctx context.Context, batch Batch, scope archive.NativeImportBatchScope) (Batch, error) {
	digest, err := archive.NativeImportBatchScopeDigest(ctx, scope)
	if err != nil {
		return Batch{}, err
	}
	if _, err := agentmeta.JSONWireBound(ctx, batch, archive.MaxImportBatchControlBytes); err != nil {
		return Batch{}, err
	}
	// Bounded serialization owns every nested slice and map before a caller
	// can later edit the plan. No raw native source is part of this control.
	raw, err := json.Marshal(batch)
	if err != nil {
		return Batch{}, err
	}
	var frozen Batch
	if err := json.Unmarshal(raw, &frozen); err != nil {
		return Batch{}, err
	}
	scopeRaw, err := json.Marshal(scope)
	if err != nil {
		return Batch{}, err
	}
	var copied archive.NativeImportBatchScope
	if err := json.Unmarshal(scopeRaw, &copied); err != nil {
		return Batch{}, err
	}
	frozen.NativeImport = &archive.NativeImportBatchConfirmation{Contract: "muse-historical-import-v1", Instance: strings.Repeat("0", 32), ScopeSHA256: digest, Scope: copied}
	return frozen, frozen.nativeImportControl().Validate(ctx)
}

func compareNativeImportConfirmation(ctx context.Context, raw []byte, found bool, expected string, frozen Batch, payload []byte) error {
	sum := sha256.Sum256(raw)
	if !found || hex.EncodeToString(sum[:]) != expected {
		return archive.ErrUnsupportedNativeImport
	}
	var current Batch
	if err := decodeBatch(raw, &current); err != nil {
		return err
	}
	if current.NativeImport != nil || current.CompletedAt != nil || current.UndoneAt != nil {
		return archive.ErrUnsupportedNativeImport
	}
	// Validate the actual old document plus this one new section, retaining
	// unknown old keys during validation so they cannot silently disappear.
	if err := validateAugmentedNativeImportControl(ctx, raw, frozen.NativeImport); err != nil {
		return err
	}
	current.NativeImport = frozen.NativeImport
	encoded, err := json.Marshal(current)
	if err != nil || !bytes.Equal(encoded, payload) {
		return errors.Join(archive.ErrUnsupportedNativeImport, err)
	}
	return ctx.Err()
}

func validateAugmentedNativeImportControl(ctx context.Context, raw []byte, confirmation *archive.NativeImportBatchConfirmation) error {
	if _, err := agentmeta.JSONWireBound(ctx, confirmation, archive.MaxNativeImportBatchBytes); err != nil {
		return err
	}
	encoded, err := json.Marshal(confirmation)
	if err != nil {
		return err
	}
	original := bytes.TrimSpace(raw)
	if len(original) < 2 || original[0] != '{' || original[len(original)-1] != '}' {
		return archive.ErrUnsupportedNativeImport
	}
	// Append lexically so duplicate, aliased and unknown existing keys remain
	// visible to the closed decoder rather than collapsing through a map.
	suffix := []byte(`"native_import":`)
	comma := len(bytes.TrimSpace(original[1:len(original)-1])) != 0
	size := len(original) + len(suffix) + len(encoded)
	if comma {
		size++
	}
	if size > archive.MaxImportBatchControlBytes {
		return archive.ErrUnsupportedNativeImport
	}
	augmented := make([]byte, 0, size)
	augmented = append(augmented, original[:len(original)-1]...)
	if comma {
		augmented = append(augmented, ',')
	}
	augmented = append(augmented, suffix...)
	augmented = append(augmented, encoded...)
	augmented = append(augmented, '}')
	var control archive.NativeImportBatchControl
	if err := archive.DecodeNativeImportBatchControl(augmented, &control); err != nil {
		return err
	}
	return control.Validate(ctx)
}
