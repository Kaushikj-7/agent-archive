package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// configJSON prevents recursive calls through Config's JSON methods.
type configJSON Config

type configWriter string

const (
	legacyDiscoveryWriter configWriter = "discovery-v2"
	discoveryWriter       configWriter = "discovery-floor-v2"
)

const legacyCodexWriter configWriter = "codex-scope-v3"

const codexWriter configWriter = "codex-scope-floor-v3"

const importStagingWriter configWriter = "import-staging-v9"

const retentionWriter configWriter = "retention-forever-v8"

const durableStorageWriter configWriter = "durable-storage-v7"

const codexHistoryWriter configWriter = "codex-history-v5"

const generationWriter configWriter = "archive-generations-v4"

type writerVersion struct {
	Version int          `json:"version"`
	Writer  configWriter `json:"writer"`
}

// MarshalJSON keeps legacy configuration numeric and fences protected writers
// through the known schema_version field. Published older integer decoders
// reject this object before they can discard unknown authorization fields.
// Normalize a value copy so edited nested setup drafts retain that fence.
func (c Config) MarshalJSON() ([]byte, error) {
	if c.RetentionDays == 0 {
		c.RetentionProtection = true
	}
	if err := prepareDiscoveryConfig(&c); err != nil {
		return nil, err
	}
	plain := configJSON(c)
	if c.Discovery == nil && !c.ImportStagingProtection && !c.RetentionProtection && c.CodexCapture == nil && !c.GenerationProtection && !c.CodexHistoryProtection && !c.DurableStorageProtection {
		return json.Marshal(plain)
	}
	if err := validateDiscoveryConfig(c); err != nil {
		return nil, err
	}
	version := writerVersion{Version: 2, Writer: discoveryWriter}
	if c.CodexCapture != nil {
		version = writerVersion{Version: 3, Writer: codexWriter}
	}
	if c.GenerationProtection {
		version = writerVersion{Version: 4, Writer: generationWriter}
	}
	if c.CodexHistoryProtection {
		version = writerVersion{Version: 5, Writer: codexHistoryWriter}
	}
	if c.DurableStorageProtection {
		version = writerVersion{Version: 7, Writer: durableStorageWriter}
	}
	if c.RetentionProtection {
		version = writerVersion{Version: 8, Writer: retentionWriter}
	}
	if c.ImportStagingProtection {
		version = writerVersion{Version: 9, Writer: importStagingWriter}
	}
	return json.Marshal(struct {
		SchemaVersion writerVersion `json:"schema_version"`
		*configJSON
	}{SchemaVersion: version, configJSON: &plain})
}

// UnmarshalJSON reads legacy numeric versions and the protected writer fence.
// Numeric discovery configs remain readable for guarded canonical migration.
func (c *Config) UnmarshalJSON(data []byte) error {
	_, err := decodeConfig(data, c)
	return err
}

func decodeConfig(data []byte, c *Config) (bool, error) {
	plain := configJSON(*c)
	wire := struct {
		SchemaVersion json.RawMessage `json:"schema_version"`
		RetentionDays json.RawMessage `json:"retention_days"`
		*configJSON
	}{configJSON: &plain}
	if err := json.Unmarshal(data, &wire); err != nil {
		return false, err
	}
	if len(wire.RetentionDays) == 0 {
		plain.RetentionDays = DefaultRetentionDays
	} else {
		if bytes.Equal(bytes.TrimSpace(wire.RetentionDays), []byte("null")) {
			return false, errors.New("retention_days must be an integer; omit the field for the legacy default")
		}
		if err := json.Unmarshal(wire.RetentionDays, &plain.RetentionDays); err != nil {
			return false, err
		}
	}
	if err := ValidateRetentionDays(plain.RetentionDays); err != nil {
		return false, err
	}
	raw := bytes.TrimSpace(wire.SchemaVersion)
	fenced := len(raw) > 0 && raw[0] == '{'
	if fenced {
		var version writerVersion
		if err := json.Unmarshal(raw, &version); err != nil {
			return false, err
		}
		if err := validateWriterVersion(Config(plain), version); err != nil {
			return false, err
		}
		// Prior protected writers also need canonical migration before identity
		// mutation: they do not understand immutable generation floors.
		fenced = version.Writer == discoveryWriter || version.Writer == codexWriter || version.Writer == generationWriter || version.Writer == codexHistoryWriter || version.Writer == durableStorageWriter || version.Writer == retentionWriter || version.Writer == importStagingWriter
		plain.SchemaVersion = version.Version
	} else if len(raw) > 0 {
		if err := json.Unmarshal(raw, &plain.SchemaVersion); err != nil {
			return false, err
		}
	}
	if plain.ImportStagingProtection || plain.SchemaVersion >= 9 {
		if err := validateDiscoveryConfig(Config(plain)); err != nil {
			return false, err
		}
	}
	*c = Config(plain)
	if c.RetentionDays == 0 {
		c.RetentionProtection = true
		if err := prepareDiscoveryConfig(c); err != nil {
			return false, err
		}
	}
	return fenced, nil
}

func codexMarkerMatches(mode SkillEvidence, writer configWriter) bool {
	return strings.HasSuffix(string(mode), codexWriterMarker) || (writer == legacyCodexWriter && strings.HasSuffix(string(mode), legacyCodexWriterMarker))
}

func validateWriterVersion(c Config, version writerVersion) error {
	if !supportedWriterVersion(version) {
		return errors.New("configuration requires a supported writer fence")
	}
	if version.Version == 9 {
		if !c.ImportStagingProtection {
			return errors.New("import staging writer fence requires protection")
		}
	} else if version.Version == 8 {
		if !c.RetentionProtection {
			return errors.New("retention writer fence requires protection")
		}
	} else if version.Version == 7 {
		if !c.DurableStorageProtection {
			return errors.New("durable storage writer fence requires protection")
		}
	} else if version.Version == 5 {
		if !c.CodexHistoryProtection {
			return errors.New("history writer fence requires history protection")
		}
	} else if version.Version == 4 {
		if !c.GenerationProtection {
			return errors.New("generation writer fence requires generation protection")
		}
	} else if version.Version == 3 {
		if c.CodexCapture == nil || !codexMarkerMatches(c.SkillEvidence, version.Writer) {
			return errors.New("writer fence requires Codex policy")
		}
	} else if c.Discovery == nil || !strings.HasSuffix(string(c.SkillEvidence), discoveryWriterMarker) {
		return errors.New("writer fence requires protected discovery configuration")
	}
	return validateWriterFloors(c, version)
}

func supportedWriterVersion(v writerVersion) bool {
	return (v.Version == 2 && (v.Writer == discoveryWriter || v.Writer == legacyDiscoveryWriter)) ||
		(v.Version == 3 && (v.Writer == codexWriter || v.Writer == legacyCodexWriter)) ||
		(v.Version == 4 && v.Writer == generationWriter) ||
		(v.Version == 5 && v.Writer == codexHistoryWriter) ||
		(v.Version == 7 && v.Writer == durableStorageWriter) ||
		(v.Version == 8 && v.Writer == retentionWriter) ||
		(v.Version == 9 && v.Writer == importStagingWriter)
}

func validateWriterFloors(c Config, version writerVersion) error {
	if version.Writer == discoveryWriter || ((version.Writer == generationWriter || version.Writer == codexHistoryWriter || version.Writer == durableStorageWriter || version.Writer == retentionWriter || version.Writer == importStagingWriter) && c.Discovery != nil) {
		for _, a := range c.Discovery.Authorizations {
			if len(a.Intervals) > 0 && a.NativeStartFloor.IsZero() {
				return errors.New("discovery permission history requires its native start floor")
			}
		}
	}
	if version.Writer == codexWriter || ((version.Writer == generationWriter || version.Writer == codexHistoryWriter || version.Writer == durableStorageWriter || version.Writer == retentionWriter || version.Writer == importStagingWriter) && c.CodexCapture != nil) {
		var scopes []*DiscoveryAuthorization
		scopes = append(scopes, c.CodexCapture.Authorization, c.CodexCapture.SourceAuthorization)
		if c.Discovery != nil {
			for i := range c.Discovery.Authorizations {
				scopes = append(scopes, &c.Discovery.Authorizations[i])
			}
		}
		for _, a := range scopes {
			if a != nil && len(a.Intervals) > 0 && a.NativeStartFloor.IsZero() {
				return errors.New("codex permission history requires its native start floor")
			}
		}
	}
	return nil
}
