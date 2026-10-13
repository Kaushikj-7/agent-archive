package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/wangjohn/agent-archive/internal/destination"
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

const settledCursorSelectionWriter configWriter = "settled-cursor-selection-v15"

const stagedSelectionWriter configWriter = "staged-selection-v13"

const publicationCompositionWriter configWriter = "publication-composition-v12"

// scopedPickerWriter reserves the permanent capability; construction is not yet supported.
const scopedPickerWriter configWriter = "scoped-picker-v1"

const catalogWriter configWriter = "catalog-v4-v11"

const historicalProjectWriter configWriter = "historical-projects-v10"

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
	if len(c.HistoricalProjects) > 0 {
		c.HistoricalProjectProtection = true
		if err := prepareDiscoveryConfig(&c); err != nil {
			return nil, err
		}
	}
	plain := configJSON(c)
	if !c.protectedWriterRequired() {
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
	if c.HistoricalProjectProtection {
		version = writerVersion{Version: 10, Writer: historicalProjectWriter}
	}
	if c.Storage.EffectiveArchiveFormat() == destination.FormatCatalogV4 {
		version = writerVersion{Version: 11, Writer: catalogWriter}
	}
	if c.PublicationCompositionProtection {
		version = writerVersion{Version: 12, Writer: publicationCompositionWriter}
	}
	if c.StagedSelectionProtection {
		version = writerVersion{Version: 13, Writer: stagedSelectionWriter}
	}
	if c.SettledCursorSelectionProtection {
		version = writerVersion{Version: 15, Writer: settledCursorSelectionWriter}
	}
	if c.ScopedPickerProtection {
		version = writerVersion{Version: 14, Writer: scopedPickerWriter}
	}
	return json.Marshal(struct {
		SchemaVersion writerVersion `json:"schema_version"`
		*configJSON
	}{SchemaVersion: version, configJSON: &plain})
}

func (c Config) protectedWriterRequired() bool {
	return c.ScopedPickerProtection || c.SettledCursorSelectionProtection || c.StagedSelectionProtection || c.PublicationCompositionProtection || c.Storage.EffectiveArchiveFormat() == destination.FormatCatalogV4 || c.HistoricalProjectProtection || c.Discovery != nil || c.ImportStagingProtection || c.RetentionProtection || c.CodexCapture != nil || c.GenerationProtection || c.CodexHistoryProtection || c.DurableStorageProtection
}

// UnmarshalJSON reads legacy numeric versions and the protected writer fence.
// Numeric discovery configs remain readable for guarded canonical migration.
func (c *Config) UnmarshalJSON(data []byte) error {
	_, err := decodeConfig(data, c)
	return err
}

func decodeConfig(data []byte, c *Config) (bool, error) {
	if err := checkStagedConfigAuthority(data); err != nil {
		return false, err
	}
	plain := configJSON(*c)
	wire := struct {
		SchemaVersion          json.RawMessage `json:"schema_version"`
		RetentionDays          json.RawMessage `json:"retention_days"`
		ScopedPickerProtection json.RawMessage `json:"scoped_picker_protection"`
		*configJSON
	}{configJSON: &plain}
	if err := json.Unmarshal(data, &wire); err != nil {
		return false, err
	}
	if len(wire.ScopedPickerProtection) != 0 {
		if !bytes.Equal(bytes.TrimSpace(wire.ScopedPickerProtection), []byte("true")) {
			return false, errors.New("scoped picker requires true exact protection")
		}
		plain.ScopedPickerProtection = true
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
	if len(plain.HistoricalProjects) > 0 && !plain.HistoricalProjectProtection {
		return false, errors.New("historical project configuration requires permanent writer protection")
	}
	fenced, err := decodeConfigVersion(&plain, wire.SchemaVersion)
	if err != nil {
		return false, err
	}
	if err := validateCompositionConfig(plain, fenced); err != nil {
		return false, err
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

func validateCompositionConfig(plain configJSON, fenced bool) error {
	scoped := plain.SchemaVersion == 14 && plain.ScopedPickerProtection && plain.Storage.EffectiveArchiveFormat() == destination.FormatCatalogV4
	staged := (plain.SchemaVersion == 13 || plain.SchemaVersion == 15 && plain.SettledCursorSelectionProtection) && plain.Storage.EffectiveArchiveFormat() == destination.FormatLegacy
	if plain.ScopedPickerProtection && (!fenced || !scoped) {
		return errors.New("scoped picker requires its exact permanent catalog fence")
	}
	if err := validateRememberedSettledComposition(plain, fenced, scoped); err != nil {
		return err
	}
	if plain.StagedSelectionProtection && (!fenced || !staged && !scoped) {
		return errors.New("staged selection requires its exact permanent writer fence")
	}
	if plain.PublicationCompositionProtection && (!fenced || plain.SchemaVersion != 12 && !(staged && plain.StagedSelectionProtection) && !scoped || !plain.DurableStorageProtection) {
		return errors.New("publication composition requires its exact protected writer fence")
	}
	if plain.Storage.EffectiveArchiveFormat() == destination.FormatCatalogV4 && (!fenced || (plain.SchemaVersion != 14 && plain.SchemaVersion != 11 && (plain.SchemaVersion != 12 || !plain.PublicationCompositionProtection || !plain.DurableStorageProtection))) {
		return errors.New("catalog format requires its supported forward writer fence")
	}
	return nil
}

func validateRememberedSettledComposition(c configJSON, fenced, scoped bool) error {
	if !c.SettledCursorSelectionProtection {
		return nil
	}
	legacy := c.SchemaVersion == 15 && c.Storage.EffectiveArchiveFormat() == destination.FormatLegacy
	if !fenced || !legacy && !scoped || !c.StagedSelectionProtection || !c.ImportStagingProtection || !c.PublicationCompositionProtection || !c.DurableStorageProtection {
		return errors.New("settled Cursor protection requires its exact permanent writer fence and every lower capability")
	}
	return nil
}

func decodeConfigVersion(plain *configJSON, rawVersion json.RawMessage) (bool, error) {
	raw := bytes.TrimSpace(rawVersion)
	fenced := len(raw) > 0 && raw[0] == '{'
	if fenced {
		var version writerVersion
		if err := json.Unmarshal(raw, &version); err != nil {
			return false, err
		}
		if err := validateWriterVersion(Config(*plain), version); err != nil {
			return false, err
		}
		// Prior protected writers also need canonical migration before identity
		// mutation: they do not understand immutable generation floors.
		fenced = version.Writer == discoveryWriter || version.Writer == codexWriter || version.Writer == generationWriter || version.Writer == codexHistoryWriter || version.Writer == durableStorageWriter || version.Writer == retentionWriter || version.Writer == importStagingWriter || version.Writer == historicalProjectWriter || version.Writer == catalogWriter || (version.Writer == publicationCompositionWriter || version.Writer == stagedSelectionWriter || version.Writer == scopedPickerWriter || version.Writer == settledCursorSelectionWriter)
		plain.SchemaVersion = version.Version
	} else if len(raw) > 0 {
		if err := json.Unmarshal(raw, &plain.SchemaVersion); err != nil {
			return false, err
		}
		if plain.SchemaVersion == 14 {
			return false, errors.New("scoped picker configuration requires its permanent protected writer fence and complete scoped catalog support")
		}
	}
	return fenced, nil
}

func codexMarkerMatches(mode SkillEvidence, writer configWriter) bool {
	return strings.HasSuffix(string(mode), codexWriterMarker) || (writer == legacyCodexWriter && strings.HasSuffix(string(mode), legacyCodexWriterMarker))
}

func validateWriterVersion(c Config, version writerVersion) error {
	if err := validateScopedWriterMarker(c, version); err != nil {
		return err
	}
	if err := validateSupportedWriterVersion(version); err != nil {
		return err
	}
	if version.Version == 14 {
		if err := validateScopedWriterConfig(c); err != nil {
			return err
		}
	} else if version.Version == 15 || version.Version == 13 {
		if err := validateStagedWriterConfiguration(c, version); err != nil {
			return err
		}

	} else if version.Version == 12 {
		if !c.PublicationCompositionProtection || !c.DurableStorageProtection {
			return errors.New("publication composition requires durable storage protection")
		}
	} else if version.Version == 11 {
		if c.Storage.EffectiveArchiveFormat() != destination.FormatCatalogV4 {
			return errors.New("catalog writer fence requires catalog format")
		}
	} else if version.Version == 10 {
		if !c.HistoricalProjectProtection {
			return errors.New("historical project writer fence requires protection")
		}
	} else if version.Version == 9 {
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

func validateSupportedWriterVersion(version writerVersion) error {
	if !supportedWriterVersion(version) {
		return errors.New("configuration requires a supported writer fence")
	}
	return nil
}

func supportedWriterVersion(v writerVersion) bool {
	return (v.Version == 2 && (v.Writer == discoveryWriter || v.Writer == legacyDiscoveryWriter)) ||
		(v.Version == 3 && (v.Writer == codexWriter || v.Writer == legacyCodexWriter)) ||
		(v.Version == 4 && v.Writer == generationWriter) ||
		(v.Version == 5 && v.Writer == codexHistoryWriter) ||
		(v.Version == 7 && v.Writer == durableStorageWriter) ||
		(v.Version == 8 && v.Writer == retentionWriter) ||
		(v.Version == 9 && v.Writer == importStagingWriter) ||
		(v.Version == 10 && v.Writer == historicalProjectWriter) ||
		(v.Version == 11 && v.Writer == catalogWriter) ||
		(v.Version == 12 && v.Writer == publicationCompositionWriter) ||
		(v.Version == 15 && v.Writer == settledCursorSelectionWriter) ||
		(v.Version == 13 && v.Writer == stagedSelectionWriter) || (v.Version == 14 && v.Writer == scopedPickerWriter)
}

func validateWriterFloors(c Config, version writerVersion) error {
	if version.Writer == discoveryWriter || (writerHasPermissionFloors(version.Writer) && c.Discovery != nil) {
		for _, a := range c.Discovery.Authorizations {
			if len(a.Intervals) > 0 && a.NativeStartFloor.IsZero() {
				return errors.New("discovery permission history requires its native start floor")
			}
		}
	}
	if version.Writer == codexWriter || (writerHasPermissionFloors(version.Writer) && c.CodexCapture != nil) {
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

func writerHasPermissionFloors(writer configWriter) bool {
	switch writer {
	case generationWriter, codexHistoryWriter, durableStorageWriter, retentionWriter, importStagingWriter, historicalProjectWriter, catalogWriter, publicationCompositionWriter, stagedSelectionWriter, settledCursorSelectionWriter, scopedPickerWriter:
		return true
	case legacyDiscoveryWriter, discoveryWriter, legacyCodexWriter, codexWriter:
		return false
	default:
		return false
	}
}

func validateScopedWriterMarker(c Config, version writerVersion) error {
	if c.ScopedPickerProtection && (version.Version != 14 || version.Writer != scopedPickerWriter) {
		return errors.New("scoped picker requires its exact permanent fence")
	}

	return nil
}

func validateScopedWriterConfig(c Config) error {
	if !c.ScopedPickerProtection || c.Storage.EffectiveArchiveFormat() != destination.FormatCatalogV4 {
		return errors.New("scoped picker requires catalog format and permanent protection")
	}
	return nil
}

func validateStagedWriterConfiguration(c Config, version writerVersion) error {
	switch version.Version {
	case 15:
		if !c.SettledCursorSelectionProtection || !c.StagedSelectionProtection || !c.PublicationCompositionProtection || !c.DurableStorageProtection || !c.ImportStagingProtection || c.Storage.EffectiveArchiveFormat() != destination.FormatLegacy {
			return errors.New("unsupported settled Cursor writer")
		}
	case 13:
		if !c.StagedSelectionProtection || !c.PublicationCompositionProtection || !c.DurableStorageProtection || !c.ImportStagingProtection || c.Storage.EffectiveArchiveFormat() != destination.FormatLegacy {
			return errors.New("staged selection requires protected legacy import configuration")
		}
	}
	return nil
}
