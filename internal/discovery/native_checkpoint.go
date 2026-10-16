package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
)

var errNativeCatalogChanged = errors.New("native inventory checkpoint changed; retry next pass")

// Catalog writers cooperate under collector ownership. This additional short
// lock protects expected bytes, including callers testing concurrent ownership.
// It authenticates neither the writer nor the historical observations.
func mergeNativeCatalog(ctx context.Context, path string, expected, replacement []byte, nativeOnly bool, budget *agentapi.NativeReadBudget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	release, err := nativeCatalogScratch(path, int64(len(replacement)), budget)
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := boundedCatalogBytes(path)
	if err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if len(before) > 0 && json.Unmarshal(before, &object) != nil {
		return errNativeCatalogChanged
	}
	if object == nil {
		object = map[string]json.RawMessage{"version": json.RawMessage("6")}
	}
	if nativeOnly {
		if string(object["version"]) != "6" {
			return errNativeCatalogChanged
		}
		if !bytes.Equal(object["native_inventory"], expected) {
			return errNativeCatalogChanged
		}
		object["native_inventory"] = replacement
	} else {
		var next map[string]json.RawMessage
		if json.Unmarshal(replacement, &next) != nil {
			return errNativeCatalogChanged
		}
		delete(object, "native_coverage")
		maps.Copy(object, next)
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return err
	}
	if len(raw) > maxCatalogBytes {
		return errCatalogTooLarge
	}
	staged, err := local.StageBytes(path, raw)
	if err != nil {
		return err
	}
	defer staged.Discard()
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock, err := local.NamedLock(filepath.Dir(path), "discovery-catalog.lock")
	if err != nil {
		return err
	}
	current, err := boundedCatalogBytes(path)
	if err == nil && !bytes.Equal(current, before) {
		err = errNativeCatalogChanged
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = staged.Commit()
	}
	unlock()
	if err != nil {
		return err
	}
	return staged.SyncDir()
}

func boundedCatalogBytes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxCatalogBytes || info.Size() < 0 {
		return nil, errCatalogTooLarge
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCatalogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxCatalogBytes || int64(len(raw)) != info.Size() {
		return nil, errCatalogChanged
	}
	return raw, nil
}

// Reserve before reading/decoding/merging the existing whole catalog. Refusal
// preserves the previous atomic bytes. The member's retained charge is separate.
func nativeCatalogScratch(path string, replacement int64, budget *agentapi.NativeReadBudget) (func(), error) {
	n := int64(0)
	// #nosec G703 -- caller-owned local discovery-catalog.json; native locators never form this path.
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() || info.Size() > maxCatalogBytes {
			return nil, errCatalogTooLarge
		}
		n = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	charge := n*12 + replacement*3 + (32 << 10)
	reservation := budget.Reservation()
	if !reservation.Reserve(charge) {
		return nil, errCatalogBudget
	}
	return func() { reservation.Release(charge) }, nil
}
