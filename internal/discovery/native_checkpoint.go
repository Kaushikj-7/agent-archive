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
	"reflect"
	"strings"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

var errNativeCatalogChanged = errors.New("native inventory checkpoint changed; retry next pass")

// Catalog writers cooperate under collector ownership. This additional short
// lock protects expected bytes, including callers testing concurrent ownership.
// It authenticates neither the writer nor the historical observations.
func mergeNativeCatalog(ctx context.Context, path string, expected, replacement []byte, nativeOnly bool, budget *agentapi.NativeReadBudget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// An ordinary writer rebuilds an over-cap catalog without reading it. There
	// can be no supported optional inventory to preserve in that file.
	if !nativeOnly {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > maxCatalogBytes {
			return replaceOversizedCatalog(ctx, path, info, replacement)
		}
	}
	release, inputLimit, err := nativeCatalogScratch(path, int64(len(replacement)), budget)
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := boundedCatalogBytes(path, inputLimit)
	if err != nil {
		return err
	}
	object, valid := nativeCatalogMembers(before)
	if len(before) > 0 && !valid && nativeOnly {
		return errNativeCatalogChanged
	}
	if !nativeOnly && (!valid || string(object["version"]) != "6") {
		object = nil
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
		next, ok := nativeCatalogMembers(replacement)
		if !ok {
			return errNativeCatalogChanged
		}
		delete(object, "native_coverage")
		// Missing optional fields mean deletion for the ordinary writer, while
		// future members and the separate native inventory remain preserved.
		fields := reflect.TypeFor[catalog]()
		for i := range fields.NumField() {
			delete(object, strings.Split(fields.Field(i).Tag.Get("json"), ",")[0])
		}
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
	current, err := boundedCatalogBytes(path, inputLimit)
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

func boundedCatalogBytes(path string, inputLimit int64) ([]byte, error) {
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
	if info.Size() > inputLimit {
		return nil, errCatalogChanged
	}
	raw, err := io.ReadAll(io.LimitReader(f, inputLimit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > inputLimit || int64(len(raw)) != info.Size() {
		return nil, errCatalogChanged
	}
	return raw, nil
}

// Reserve before reading/decoding/merging the existing whole catalog. Refusal
// preserves the previous atomic bytes. The member's retained charge is separate.
func nativeCatalogScratch(path string, replacement int64, budget *agentapi.NativeReadBudget) (func(), int64, error) {
	n := int64(0)
	// #nosec G703 -- caller-owned local discovery-catalog.json; native locators never form this path.
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() || info.Size() > maxCatalogBytes {
			return nil, 0, errCatalogTooLarge
		}
		n = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	// Top-level member values borrow slices of the bounded input rather than
	// decoding/copying their nested values. Three input lengths cover ReadAll
	// growth, the later comparison read and member keys; three replacement
	// lengths cover the encoder/output. Fixed scratch bounds scanner/key maps.
	charge := n*3 + replacement*3 + (256 << 10)
	reservation := budget.Reservation()
	if !reservation.Reserve(charge) {
		return nil, 0, errCatalogBudget
	}
	return func() { reservation.Release(charge) }, n, nil
}

// nativeCatalogMembers indexes validated top-level values without copying their
// potentially large nested documents. The input owns every returned value.
func nativeCatalogMembers(raw []byte) (map[string]json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '{' || !json.Valid(raw) {
		return nil, false
	}
	members := map[string]json.RawMessage{}
	i := 1
	for {
		for i < len(raw) && (raw[i] == ' ' || raw[i] == '\n' || raw[i] == '\r' || raw[i] == '\t' || raw[i] == ',') {
			i++
		}
		if i >= len(raw) {
			return nil, false
		}
		if raw[i] == '}' {
			return members, true
		}
		start := i
		i = nativeJSONStringEnd(raw, i)
		var key string
		if i < 0 || i-start > 4096 || json.Unmarshal(raw[start:i], &key) != nil || len(members) >= 1024 {
			return nil, false
		}
		if _, duplicate := members[key]; duplicate {
			return nil, false
		}
		for i < len(raw) && raw[i] != ':' {
			i++
		}
		i++
		start = i
		depth := 0
		for i < len(raw) {
			c := raw[i]
			if c == '"' {
				i = nativeJSONStringEnd(raw, i)
				if i < 0 {
					return nil, false
				}
				continue
			}
			if c == '[' || c == '{' {
				depth++
			}
			if c == ']' || c == '}' {
				if depth == 0 {
					break
				}
				depth--
			}
			if c == ',' && depth == 0 {
				break
			}
			i++
		}
		members[key] = bytes.TrimSpace(raw[start:i])
	}
}

func nativeJSONStringEnd(raw []byte, i int) int {
	if i >= len(raw) || raw[i] != '"' {
		return -1
	}
	for i++; i < len(raw); i++ {
		if raw[i] == '\\' {
			i++
			continue
		}
		if raw[i] == '"' {
			return i + 1
		}
	}
	return -1
}

func replaceOversizedCatalog(ctx context.Context, path string, before os.FileInfo, replacement []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(replacement) > maxCatalogBytes {
		return errCatalogTooLarge
	}
	staged, err := local.StageBytes(path, replacement)
	if err != nil {
		return err
	}
	defer staged.Discard()
	unlock, err := local.NamedLock(filepath.Dir(path), "discovery-catalog.lock")
	if err != nil {
		return err
	}
	current, err := os.Stat(path)
	if err == nil && !transcriptio.SameObservation(before, current) {
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
