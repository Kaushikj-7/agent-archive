package setupjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/config"
)

type journalJSON Journal

type nativeJournalFence struct {
	Version int    `json:"version"`
	Writer  string `json:"writer"`
	Value   bool   `json:"value"`
}

// MarshalJSON fences recovery before older setup binaries can apply raw rollback
// bytes. Older Journal decoders know files_only as bool and reject this object.
func (j Journal) MarshalJSON() ([]byte, error) {
	protected, err := nativeJournalProtected(j)
	if err != nil {
		return nil, err
	}
	j.NativeAuthorityProtection = protected
	raw, err := j.marshalBaseJSON()
	if err != nil || !protected {
		return raw, err
	}
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	doc["files_only"], err = json.Marshal(nativeJournalFence{Version: 1, Writer: "native-authority-v16", Value: j.FilesOnly})
	if err != nil {
		return nil, err
	}
	return json.Marshal(doc)
}

// UnmarshalJSON validates the recovery fence before interpreting any changes.
func (j *Journal) UnmarshalJSON(raw []byte) error {
	if err := checkNativeJournalKeys(raw, false); err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	filesOnly := bytes.TrimSpace(doc["files_only"])
	fenced := len(filesOnly) > 0 && filesOnly[0] == '{'
	if fenced {
		// Validate the original envelope before normalizing its fence: a map
		// would otherwise collapse duplicate managed authority fields.
		if err := checkJournalKeys(json.NewDecoder(bytes.NewReader(raw)), 0); err != nil {
			return err
		}
		if err := checkNativeJournalKeys(filesOnly, true); err != nil {
			return err
		}
		var fence nativeJournalFence
		d := json.NewDecoder(bytes.NewReader(filesOnly))
		d.DisallowUnknownFields()
		if err := d.Decode(&fence); err != nil {
			return err
		}
		if fence.Version != 1 || fence.Writer != "native-authority-v16" || !bytes.Equal(bytes.TrimSpace(doc["native_authority_protection"]), []byte("true")) {
			return errors.New("unsupported native setup recovery fence")
		}
		doc["files_only"], _ = json.Marshal(fence.Value)
		var err error
		raw, err = json.Marshal(doc)
		if err != nil {
			return err
		}
	}
	var plain Journal
	if err := plain.unmarshalBaseJSON(raw); err != nil {
		return err
	}
	protected, err := nativeJournalProtected(plain)
	if err != nil {
		return err
	}
	if protected != fenced || doc["native_authority_protection"] != nil && !fenced {
		return errors.New("native setup recovery requires its exact writer fence")
	}
	*j = plain
	return nil
}

func nativeJournalProtected(j Journal) (bool, error) {
	protected := j.NativeAuthorityProtection
	for _, change := range j.Changes {
		if filepath.Base(change.Path) != "config.json" {
			continue
		}
		for _, raw := range [][]byte{change.Before, change.After} {
			var doc map[string]json.RawMessage
			if json.Unmarshal(raw, &doc) != nil {
				continue
			}
			if doc["native_authority_protection"] == nil && doc["native_roots"] == nil {
				continue
			}
			var cfg config.Config
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return false, err
			}
			protected = protected || cfg.NativeAuthorityProtection
		}
	}
	return protected, nil
}

func checkNativeJournalKeys(raw []byte, fence bool) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("invalid native setup recovery object")
	}
	names := []string{"files_only", "native_authority_protection"}
	if fence {
		names = []string{"version", "writer", "value"}
	}
	seen := map[string]int{}
	exact := true
	native := fence
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("invalid journal key")
		}
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return err
		}
		known := false
		for _, name := range names {
			if strings.EqualFold(name, key) {
				known = true
				seen[name]++
				exact = exact && key == name && seen[name] == 1 && !bytes.Equal(bytes.TrimSpace(value), []byte("null"))
				if name == "native_authority_protection" {
					native = true
				}
			}
		}
		if fence && !known {
			return errors.New("unknown native recovery fence field")
		}
	}
	if native && (!exact || len(seen) != len(names)) {
		return errors.New("ambiguous native setup recovery fence")
	}
	return nil
}
