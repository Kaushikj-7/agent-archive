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
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

var errNativeCatalogChanged = errors.New("native inventory checkpoint changed; retry next pass")

const (
	maxNativeCatalogMembers  = 1024
	maxNativeCatalogKeyBytes = 4096
)

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
		if info := oversizedOrdinaryCatalog(path); info != nil {
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
	outputSize, err := nativeCatalogOutputSize(object)
	if err != nil {
		return err
	}
	// Quoted keys may expand under JSON's HTML escaping. Pay any exact output
	// excess before allocation, independently of the baseline input scratch.
	outputCharge := max(int64(outputSize)-inputLimit-int64(len(replacement)), 0)
	outputReservation := budget.Reservation()
	if !outputReservation.Reserve(outputCharge) {
		return errCatalogBudget
	}
	defer outputReservation.Release(outputCharge)
	raw, err := encodeNativeCatalogMembers(object)
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
	err = compareNativeCatalogBytes(path, before)
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
	// #nosec G703 -- caller-owned discovery-catalog.json, never formed from a native locator.
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
	// The reserved stat size bounds the exact backing allocation; avoid
	// geometric ReadAll growth while other catalog buffers are retained.
	raw := make([]byte, info.Size())
	if _, err := io.ReadFull(f, raw); err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !transcriptio.SameObservation(info, after) {
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
	// decoding/copying their nested values. Three input lengths cover the exact
	// input, borrowed member keys/maps and single exact output buffer.
	// UTF-8 refusal prevents replacement-rune expansion of decoded keys.
	// Comparison uses fixed streaming scratch instead of a second whole read;
	// output assembly never invokes a whole-map encoder. Three replacement
	// lengths cover its member/output overhead. Fixed scratch bounds scanner,
	// temporary encoded keys, sorted key headers and comparison buffers.
	charge := n*3 + replacement*3 + (512 << 10)
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
	if len(raw) < 2 || raw[0] != '{' || !utf8.Valid(raw) || !json.Valid(raw) {
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
		if i < 0 || i-start > maxNativeCatalogKeyBytes || json.Unmarshal(raw[start:i], &key) != nil || len(members) >= maxNativeCatalogMembers {
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
		i = nativeJSONValueEnd(raw, i)
		if i < 0 {
			return nil, false
		}
		members[key] = bytes.TrimSpace(raw[start:i])
	}
}

func nativeJSONStringEnd(raw []byte, i int) int {
	if i >= len(raw) || raw[i] != '"' {
		return -1
	}
	for i++; i < len(raw); {
		// Skip ordinary string bodies with the standard vectorized byte search;
		// catalog health/error strings can legitimately approach the wire cap.
		next := bytes.IndexAny(raw[i:], "\\\"")
		if next < 0 {
			return -1
		}
		i += next
		if raw[i] == '"' {
			return i + 1
		}
		i += 2
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
	// #nosec G703 -- same caller-owned local catalog whose staged replacement was prepared above.
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

// encodeNativeCatalogMembers copies already validated member values once. Exact
// sizing refuses over-cap output before allocation and avoids encoder copies of
// large unrelated catalog members.
func encodeNativeCatalogMembers(members map[string]json.RawMessage) ([]byte, error) {
	n, err := nativeCatalogOutputSize(members)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(members))
	for key := range members {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	raw := make([]byte, 0, n)
	raw = append(raw, '{')
	for i, key := range keys {
		if i > 0 {
			raw = append(raw, ',')
		}
		encoded, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		raw = append(raw, encoded...)
		raw = append(raw, ':')
		raw = append(raw, members[key]...)
	}
	return append(raw, '}'), nil
}

func nativeCatalogOutputSize(members map[string]json.RawMessage) (int, error) {
	if len(members) > maxNativeCatalogMembers {
		return 0, errCatalogTooLarge
	}
	n := 2
	if len(members) > 0 {
		n += len(members) - 1
	}
	for key, value := range members {
		encoded, err := json.Marshal(key)
		if err != nil {
			return 0, err
		}
		if len(encoded) > maxNativeCatalogKeyBytes {
			return 0, errCatalogTooLarge
		}
		n += len(encoded) + 1 + len(value)
		if n > maxCatalogBytes {
			return 0, errCatalogTooLarge
		}
	}
	return n, nil
}

// compareNativeCatalogBytes checks expected ownership without allocating a
// second whole-catalog copy while the staged output and original are retained.
func compareNativeCatalogBytes(path string, expected []byte) error {
	// #nosec G703 -- the same caller-owned local catalog staged above; no native locator supplies this path.
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && len(expected) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(expected)) {
		return errNativeCatalogChanged
	}
	var scratch [32 << 10]byte
	offset := 0
	for offset < len(expected) {
		n := min(len(scratch), len(expected)-offset)
		if _, err := io.ReadFull(f, scratch[:n]); err != nil {
			return err
		}
		if !bytes.Equal(scratch[:n], expected[offset:offset+n]) {
			return errNativeCatalogChanged
		}
		offset += n
	}
	after, err := f.Stat()
	if err == nil && !transcriptio.SameObservation(info, after) {
		err = errNativeCatalogChanged
	}
	return err
}

func nativeJSONValueEnd(raw []byte, i int) int {
	depth := 0
	for i < len(raw) {
		c := raw[i]
		if c == '"' {
			i = nativeJSONStringEnd(raw, i)
			if i < 0 {
				return -1
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
	return i
}

func oversizedOrdinaryCatalog(path string) os.FileInfo {
	// #nosec G703 -- caller-owned local discovery-catalog.json; native locators cannot form this path.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= maxCatalogBytes {
		return nil
	}
	return info
}
