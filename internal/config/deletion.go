package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/wangjohn/agent-archive/internal/local"
)

const deletionWriter configWriter = "owned-deletion-v18"

// Deletion retains the exact underlying writer fence in memory. Version18 is an
// outer capability, so all ordinary/staged/Cursor/catalog guards keep their base.
func wrapDeletionConfig(raw []byte, protected bool) []byte {
	if !protected || raw == nil {
		return raw
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	base := doc["schema_version"]
	if bytes.Equal(base, []byte("0")) {
		base = []byte("1")
	}
	version, _ := json.Marshal(nativeAuthorityVersion{Version: 18, Writer: deletionWriter, Base: base})
	doc["schema_version"] = version
	result, _ := json.Marshal(doc)
	return result
}

func unwrapDeletionConfig(raw []byte) ([]byte, bool, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, err
	}
	v := bytes.TrimSpace(doc["schema_version"])
	var peek struct {
		Version int          `json:"version"`
		Writer  configWriter `json:"writer"`
	}
	_ = json.Unmarshal(v, &peek)
	flag := bytes.TrimSpace(doc["deletion_protection"])
	if peek.Version != 18 && peek.Writer != deletionWriter {
		if len(flag) > 0 {
			return nil, false, errors.New("deletion protection requires its permanent writer fence")
		}
		return raw, false, nil
	}
	if err := checkDeletionSelectors(raw); err != nil {
		return nil, false, err
	}
	if err := exactAuthorityObject(v, []string{"version", "writer", "base"}); err != nil {
		return nil, false, err
	}
	var wrapper nativeAuthorityVersion
	if err := json.Unmarshal(v, &wrapper); err != nil {
		return nil, false, err
	}
	if wrapper.Version != 18 || wrapper.Writer != deletionWriter || !bytes.Equal(flag, []byte("true")) {
		return nil, false, errors.New("unsupported owned deletion writer fence")
	}
	// The complete base decoder below validates the nested native/base contract.
	var nested struct {
		Version int          `json:"version"`
		Writer  configWriter `json:"writer"`
	}
	_ = json.Unmarshal(wrapper.Base, &nested)
	if nested.Version == 18 || nested.Writer == deletionWriter {
		return nil, false, errors.New("nested deletion fence")
	}
	doc["schema_version"] = wrapper.Base
	result, err := json.Marshal(doc)
	return result, true, err
}

// WithDeletionProtection persists the sticky deletion floor under the existing
// configuration lock before handing a storage capability to its owner.
func WithDeletionProtection(home string, fn func(DurableStorageGuard) error) error {
	err := WithPublicationComposition(home, func(g PublicationCompositionGuard) error {
		storage, err := g.Storage(home)
		if err != nil {
			return err
		}
		held, err := storage.RootedHome(home)
		if err != nil {
			return err
		}
		cfg, found, err := LoadRooted(held)
		if err != nil || !found {
			return errors.Join(errors.New("deletion requires a readable configuration"), err)
		}
		if !cfg.DeletionProtection {
			cfg.DeletionProtection = true
			if err := local.RootedWrite(held.Root, "config.json", cfg); err != nil {
				return err
			}
		}
		// The original guard binds the old config inode; reacquire a verified guard
		// after releasing the config lock rather than using it across replacement.
		return nil
	})
	if err != nil {
		return err
	}
	return WithDurableStorage(home, func(g DurableStorageGuard) error {
		held, err := g.RootedHome(home)
		if err != nil {
			return err
		}
		cfg, found, err := LoadRooted(held)
		if err != nil || !found || !cfg.DeletionProtection {
			return errors.Join(errors.New("deletion writer fence changed before write"), err)
		}
		return fn(g)
	})
}

type deletionSelector string

const (
	deletionSchemaSelector deletionSelector = "schema_version"
	deletionFlagSelector   deletionSelector = "deletion_protection"
)

func checkDeletionSelectors(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("invalid deletion configuration")
	}
	seen := map[deletionSelector]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("invalid configuration selector")
		}
		canonical := deletionSelector(strings.ToLower(key))
		if seen[canonical] || (canonical == deletionSchemaSelector || canonical == deletionFlagSelector) && key != string(canonical) {
			return errors.New("ambiguous deletion configuration selector")
		}
		seen[canonical] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return err
		}
	}
	return nil
}

// DeletionGuardRequired inspects only the selecting control fields retained by
// a bounded projection. It grants no configuration or storage authority: full
// configuration loading still validates every typed setting before use.
func DeletionGuardRequired(projected []byte) (bool, error) {
	if err := checkDeletionSelectors(projected); err != nil {
		return false, err
	}
	raw, protected, err := unwrapDeletionConfig(projected)
	if err != nil || protected {
		return protected, err
	}
	var view struct {
		Schema json.RawMessage `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		return false, err
	}
	return false, validateDeletionGuardVersion(bytes.TrimSpace(view.Schema))
}

func validateDeletionGuardVersion(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] != '{' {
		var version int
		if err := json.Unmarshal(raw, &version); err != nil || bytes.Equal(raw, []byte("null")) || version < 0 || version > 17 {
			return errors.New("unsupported deletion guard configuration version")
		}
		return nil
	}
	var version nativeAuthorityVersion
	if err := json.Unmarshal(raw, &version); err != nil {
		return err
	}
	if version.Writer == nativeAuthorityWriter {
		if version.Version != 16 {
			return errors.New("unsupported native authority configuration version")
		}
		if err := exactAuthorityObject(raw, []string{"version", "writer", "base"}); err != nil {
			return err
		}
		return validateNativeBase(version.Base)
	}
	if err := exactAuthorityObject(raw, []string{"version", "writer"}); err != nil {
		return err
	}
	return validateSupportedWriterVersion(writerVersion{Version: version.Version, Writer: version.Writer})
}
