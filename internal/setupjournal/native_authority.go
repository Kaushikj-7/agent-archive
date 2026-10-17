package setupjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-archive/internal/config"
)

// ErrUnsupportedRecoveryAuthority preserves protected future journals before
// legacy garbage-record quarantine or raw rollback can mutate their paths.
var ErrUnsupportedRecoveryAuthority = errors.New("unsupported setup recovery authority; use a compatible agent-archive")

type journalJSON Journal

type nativeJournalFence struct {
	Version   int    `json:"version"`
	Writer    string `json:"writer"`
	Value     bool   `json:"value"`
	Admission string `json:"admission,omitempty"`
	Import    string `json:"import,omitempty"`
}

// MarshalJSON fences recovery before older setup binaries can apply raw rollback
// bytes. Older Journal decoders know files_only as bool and reject this object.
func (j Journal) MarshalJSON() ([]byte, error) {
	protected, err := nativeJournalProtected(j)
	if err != nil {
		return nil, err
	}
	admission, err := nativeJournalAdmissionProtected(j)
	if err != nil {
		return nil, err
	}
	replay, err := nativeJournalReplayProtected(j)
	if err != nil {
		return nil, err
	}
	j.NativeReplayProtection = replay
	admission = admission || replay
	j.NativeAdmissionProtection = admission
	importProtected, err := nativeJournalImportProtected(j)
	if err != nil {
		return nil, err
	}
	j.NativeImportProtection = importProtected
	protected = protected || admission || importProtected
	j.NativeAuthorityProtection = protected
	raw, err := marshalBaseJSON(j)
	if err != nil || !protected {
		return raw, err
	}
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var capability string
	if admission {
		capability = config.MuseAdmissionWriterSelector(replay)
	}
	fence := nativeJournalFence{Version: 1, Writer: "native-authority-v16", Value: j.FilesOnly, Admission: capability}
	if importProtected {
		fence.Import = "muse-historical-import"
	}
	doc["files_only"], err = json.Marshal(fence)
	if err != nil {
		return nil, err
	}
	return json.Marshal(doc)
}

// UnmarshalJSON validates the recovery fence before interpreting any changes.
func (j *Journal) UnmarshalJSON(raw []byte) (decodeErr error) {
	defer func() {
		if decodeErr == nil {
			return
		}
		if nativeJournalAuthorityPresent(raw) {
			decodeErr = errors.Join(ErrUnsupportedRecoveryAuthority, decodeErr)
		}
	}()
	if err := checkNativeJournalKeys(raw, false); err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	filesOnly := bytes.TrimSpace(doc["files_only"])
	replayProtected, importProtected, err := nativeJournalStickyCapabilities(doc)
	if err != nil {
		return err
	}
	fenced := len(filesOnly) > 0 && filesOnly[0] == '{'
	if fenced {
		normalized, err := normalizeNativeJournalFence(raw, doc, filesOnly, replayProtected, importProtected)
		if err != nil {
			return err
		}
		raw = normalized
	}
	plain, err := unmarshalBaseJSON(raw)
	if err != nil {
		return err
	}
	protected, err := nativeJournalProtected(plain)
	if err != nil {
		return errors.Join(ErrUnsupportedRecoveryAuthority, err)
	}
	if protected != fenced || doc["native_authority_protection"] != nil && !fenced {
		return errors.New("native setup recovery requires its exact writer fence")
	}
	admission, err := nativeJournalAdmissionProtected(plain)
	if err != nil {
		return errors.Join(ErrUnsupportedRecoveryAuthority, err)
	}
	if admission != plain.NativeAdmissionProtection || admission && !fenced {
		return errors.New("native admission recovery requires exact fence")
	}
	importProtected, err = nativeJournalImportProtected(plain)
	if err != nil {
		return errors.Join(ErrUnsupportedRecoveryAuthority, err)
	}
	if importProtected != plain.NativeImportProtection || importProtected && !fenced {
		return errors.New("native import recovery requires exact fence")
	}
	replay, err := nativeJournalReplayProtected(plain)
	if err != nil {
		return errors.Join(ErrUnsupportedRecoveryAuthority, err)
	}
	if replay != plain.NativeReplayProtection || replay && !fenced {
		return errors.New("native replay recovery requires exact fence")
	}
	*j = plain
	return nil
}

// nativeJournalStickyCapabilities checks wire-level flags before interpreting
// the recovery fence or its snapshots, in the original refusal order.
func nativeJournalStickyCapabilities(doc map[string]json.RawMessage) (bool, bool, error) {
	replayProtected := bytes.Equal(bytes.TrimSpace(doc["native_replay_protection"]), []byte("true"))
	if doc["native_replay_protection"] != nil && !replayProtected {
		return false, false, errors.New("native replay recovery requires sticky protection")
	}
	if replayProtected && !bytes.Equal(bytes.TrimSpace(doc["native_admission_protection"]), []byte("true")) {
		return false, false, errors.New("native replay recovery requires admission protection")
	}
	importProtected := bytes.Equal(bytes.TrimSpace(doc["native_import_protection"]), []byte("true"))
	if doc["native_import_protection"] != nil && !importProtected {
		return false, false, errors.New("native import recovery requires sticky protection")
	}
	return replayProtected, importProtected, nil
}

// nativeJournalAuthorityPresent classifies recovery errors without granting
// authority from a malformed or aliased envelope.
func nativeJournalAuthorityPresent(raw []byte) bool {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	protected := false
	for key, value := range fields {
		protected = protected || strings.EqualFold(key, "native_authority_protection") || strings.EqualFold(key, "native_admission_protection") || strings.EqualFold(key, "native_import_protection") || strings.EqualFold(key, "native_replay_protection") || strings.EqualFold(key, "files_only") && bytes.HasPrefix(bytes.TrimSpace(value), []byte("{"))
	}
	return protected
}

func normalizeNativeJournalFence(raw []byte, doc map[string]json.RawMessage, filesOnly []byte, replayProtected, importProtected bool) ([]byte, error) {
	// Validate the original envelope before normalizing its fence: a map
	// would otherwise collapse duplicate managed authority fields.
	if err := checkJournalKeys(json.NewDecoder(bytes.NewReader(raw)), 0); err != nil {
		return nil, err
	}
	admissionProtected := bytes.Equal(bytes.TrimSpace(doc["native_admission_protection"]), []byte("true"))
	if err := checkNativeJournalKeys(filesOnly, true, admissionProtected, importProtected); err != nil {
		return nil, err
	}
	var fence nativeJournalFence
	d := json.NewDecoder(bytes.NewReader(filesOnly))
	d.DisallowUnknownFields()
	if err := d.Decode(&fence); err != nil {
		return nil, err
	}
	if fence.Version != 1 || fence.Writer != "native-authority-v16" || !bytes.Equal(bytes.TrimSpace(doc["native_authority_protection"]), []byte("true")) {
		return nil, errors.New("unsupported native setup recovery fence")
	}
	if admissionProtected && fence.Admission != config.MuseAdmissionWriterSelector(replayProtected) {
		return nil, errors.New("unsupported native admission recovery capability")
	}
	if importProtected && fence.Import != "muse-historical-import" {
		return nil, errors.New("unsupported native import recovery capability")
	}
	doc["files_only"], _ = json.Marshal(fence.Value)
	return json.Marshal(doc)
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
			if !nativeSnapshotAuthority(raw) {
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

func checkNativeJournalKeys(raw []byte, fence bool, admission ...bool) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("invalid native setup recovery object")
	}
	names := []string{"files_only", "native_authority_protection"}
	if !fence {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) == nil {
			for _, name := range []string{"native_admission_protection", "native_import_protection", "native_replay_protection"} {
				for key := range fields {
					if strings.EqualFold(key, name) {
						names = append(names, name)
						break
					}
				}
			}
		}
	}
	if fence {
		names = []string{"version", "writer", "value"}
		if len(admission) > 0 && admission[0] {
			names = append(names, "admission")
		}
		if len(admission) > 1 && admission[1] {
			names = append(names, "import")
		}
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
				if strings.HasSuffix(name, "_protection") {
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

func nativeJournalAdmissionProtected(j Journal) (bool, error) {
	protected := j.NativeAdmissionProtection
	for _, change := range j.Changes {
		if filepath.Base(change.Path) != "config.json" {
			continue
		}
		for _, raw := range [][]byte{change.Before, change.After} {
			var doc map[string]json.RawMessage
			if json.Unmarshal(raw, &doc) != nil {
				continue
			}
			if !nativeSnapshotAuthority(raw) {
				continue
			}
			var cfg config.Config
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return false, err
			}
			protected = protected || cfg.NativeAdmissionProtection
		}
	}
	return protected, nil
}

func nativeJournalImportProtected(j Journal) (bool, error) {
	protected := j.NativeImportProtection
	for _, change := range j.Changes {
		if filepath.Base(change.Path) != "config.json" {
			continue
		}
		for _, raw := range [][]byte{change.Before, change.After} {
			if !nativeSnapshotAuthority(raw) {
				continue
			}
			var cfg config.Config
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return false, err
			}
			protected = protected || cfg.NativeImportProtection
		}
	}
	return protected, nil
}

func nativeJournalReplayProtected(j Journal) (bool, error) {
	protected := j.NativeReplayProtection
	for _, change := range j.Changes {
		if filepath.Base(change.Path) != "config.json" {
			continue
		}
		for _, raw := range [][]byte{change.Before, change.After} {
			if !nativeSnapshotAuthority(raw) {
				continue
			}
			var cfg config.Config
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return false, err
			}
			protected = protected || cfg.NativeReplayProtection
		}
	}
	return protected, nil
}

// A structural schema fence is authority even when a future writer no longer
// emits today's companion booleans. Decode it before permitting raw rollback.

func nativeSnapshotAuthority(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		key, ok := token.(string)
		if !ok {
			return false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
		if nativeSnapshotSelector(key, value) {
			return true
		}
	}
	return false
}

func nativeSnapshotSelector(key string, value json.RawMessage) bool {
	switch strings.ToLower(key) {
	case "native_authority_protection", "native_roots", "native_admission_protection", "native_import_protection", "native_replay_protection", "muse_capture", "muse_capture_last_issued_at":
		return true
	case "schema_version":
		return bytes.HasPrefix(bytes.TrimSpace(value), []byte("{"))
	default:
		return false
	}
}
