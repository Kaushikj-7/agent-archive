package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// nativeAuthorityWriter wraps an exact base fence instead of replacing its
// permission floors or mutually exclusive storage-mode requirements. SchemaVersion
// in memory still describes that base, which can itself be search lifecycle version 16.
const nativeAuthorityWriter configWriter = "native-authority-v16"

type nativeAuthorityVersion struct {
	Version int             `json:"version"`
	Writer  configWriter    `json:"writer"`
	Base    json.RawMessage `json:"base"`
}

func (c Config) validateNativeRoots() error {
	if len(c.NativeRoots) > 16 {
		return errors.New("too many native source roots")
	}
	seen := map[string]bool{}
	for _, root := range c.NativeRoots {
		if err := root.Validate(); err != nil {
			return err
		}
		if root.Agent == "codex" || seen[root.Agent] {
			return errors.New("native roots require one non-Codex root per agent")
		}
		seen[root.Agent] = true
	}
	return nil
}

// MarshalJSON preserves every preexisting writer constraint beneath the sticky
// native-authority fence, including configs with no roots after removal.
func (c Config) MarshalJSON() ([]byte, error) {
	if len(c.NativeRoots) > 0 {
		c.NativeAuthorityProtection = true
	}
	if err := c.validateNativeRoots(); err != nil {
		return nil, err
	}
	raw, err := c.marshalBaseJSON()
	if err != nil || !c.NativeAuthorityProtection {
		return wrapDeletionConfig(raw, c.DeletionProtection), err
	}
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	base := doc["schema_version"]
	if bytes.Equal(base, []byte("0")) {
		base = json.RawMessage("1")
	}
	if err := validateNativeBase(base); err != nil {
		return nil, err
	}
	version, err := json.Marshal(nativeAuthorityVersion{Version: 16, Writer: nativeAuthorityWriter, Base: base})
	if err != nil {
		return nil, err
	}
	doc["schema_version"] = version
	result, err := json.Marshal(doc)
	return wrapDeletionConfig(result, c.DeletionProtection), err
}

func unwrapNativeConfig(raw []byte) ([]byte, bool, error) {
	if err := checkNativeConfigSelectors(raw); err != nil {
		return nil, false, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, err
	}
	versionRaw := bytes.TrimSpace(doc["schema_version"])
	if len(versionRaw) == 0 || versionRaw[0] != '{' {
		if doc["native_roots"] != nil || doc["native_authority_protection"] != nil {
			return nil, false, errors.New("native authority requires its permanent writer fence")
		}
		return raw, false, nil
	}
	var peek struct {
		Version int          `json:"version"`
		Writer  configWriter `json:"writer"`
	}
	if err := json.Unmarshal(versionRaw, &peek); err != nil {
		return nil, false, err
	}
	// Search has its own exact version16 base fence. Let its decoder validate
	// that shape; native protection is still rejected below without a wrapper.
	if peek.Version == 16 && peek.Writer == searchLifecycleWriter {
		if doc["native_roots"] != nil || doc["native_authority_protection"] != nil {
			return nil, false, errors.New("native authority requires its permanent writer fence")
		}
		return raw, false, nil
	}
	if peek.Version != 16 && peek.Writer != nativeAuthorityWriter {
		return raw, false, nil
	}
	if err := exactAuthorityObject(versionRaw, []string{"version", "writer", "base"}); err != nil {
		return nil, false, err
	}
	var version nativeAuthorityVersion
	decoder := json.NewDecoder(bytes.NewReader(versionRaw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&version); err != nil {
		return nil, false, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, false, errors.New("invalid native authority fence")
	}
	if version.Version != 16 || version.Writer != nativeAuthorityWriter || !bytes.Equal(bytes.TrimSpace(doc["native_authority_protection"]), []byte("true")) {
		return nil, false, errors.New("native authority requires its exact permanent writer fence")
	}
	base := bytes.TrimSpace(version.Base)
	if err := validateNativeBase(base); err != nil {
		return nil, false, err
	}
	doc["schema_version"] = base
	unwrapped, err := json.Marshal(doc)
	return unwrapped, true, err
}

// validateNativeBase is shared by encoding and decoding so a writer cannot
// replace readable settings with a native fence its own reader would reject.
func validateNativeBase(base []byte) error {
	base = bytes.TrimSpace(base)
	if bytes.Equal(base, []byte("1")) {
		return nil
	}
	if err := exactAuthorityObject(base, []string{"version", "writer"}); err != nil {
		return err
	}
	var lower writerVersion
	dec := json.NewDecoder(bytes.NewReader(base))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&lower); err != nil || !supportedWriterVersion(lower) {
		return errors.New("native authority requires a supported exact base fence")
	}
	return nil
}

func exactAuthorityObject(raw []byte, names []string) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("invalid authority object")
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("invalid authority key")
		}
		known := false
		for _, name := range names {
			known = known || key == name
		}
		if !known || seen[key] {
			return errors.New("ambiguous authority object")
		}
		seen[key] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("invalid authority value")
		}
	}
	if len(seen) != len(names) {
		return errors.New("incomplete authority object")
	}
	return nil
}

type nativeConfigSelector string

const (
	nativeSchemaSelector     nativeConfigSelector = "schema_version"
	nativeProtectionSelector nativeConfigSelector = "native_authority_protection"
	nativeRootsSelector      nativeConfigSelector = "native_roots"
	nativeStagedSelector     nativeConfigSelector = "staged_selection_protection"
	nativeSettledSelector    nativeConfigSelector = "settled_cursor_selection_protection"
	nativeScopedSelector     nativeConfigSelector = "scoped_picker_protection"
	nativeSearchSelector     nativeConfigSelector = "search_lifecycle_protection"
)

func checkNativeConfigSelectors(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("invalid configuration object")
	}
	seen := map[nativeConfigSelector]int{}
	exact := true
	native := false
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("invalid configuration key")
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return err
		}
		for _, name := range []nativeConfigSelector{nativeSchemaSelector, nativeProtectionSelector, nativeRootsSelector, nativeStagedSelector, nativeSettledSelector, nativeScopedSelector, nativeSearchSelector} {
			if strings.EqualFold(key, string(name)) {
				seen[name]++
				exact = exact && key == string(name) && seen[name] == 1
				if name == nativeProtectionSelector || name == nativeRootsSelector {
					native = true
					exact = exact && !bytes.Equal(bytes.TrimSpace(value), []byte("null"))
				}
			}
		}
	}
	if native && (!exact || seen[nativeSchemaSelector] != 1) {
		return errors.New("ambiguous native configuration authority")
	}
	return nil
}

func validateNativeRootTransition(next, previous Config) error {
	for _, root := range next.NativeRoots {
		for _, old := range previous.NativeRoots {
			if root.Agent == old.Agent && root.Revision == old.Revision && (root.Canonical != old.Canonical || root.Configured != old.Configured) {
				return errors.New("native root changes require a new reconciliation revision")
			}
		}
	}
	return nil
}

// NativeValidationInCurrentDestination checks the fresh native validation window,
// independently of the ID-first ownership rule used for retained publication.
// The candidate supplies actual native birth; hook admission cannot replace it.
func (c Config) NativeValidationInCurrentDestination(owner archive.SessionRegistration, candidate *archive.NativeSourceBinding) bool {
	if candidate == nil || candidate.Agent != "muse" || candidate.Version != 2 || candidate.Validate() != nil || candidate.Agent != owner.Harness.Name || candidate.NativeID != owner.NativeSessionID || owner.Imported() || owner.NativeChild || candidate.ParentID != "" || candidate.RootID != "" && candidate.RootID != candidate.NativeID {
		return false
	}
	return owner.DestinationID != "" && owner.DestinationID == c.DestinationID() && c.InCurrentDestination(owner) && (c.DestinationSince.IsZero() || !candidate.NativeCreatedAt.Before(c.DestinationSince))
}
