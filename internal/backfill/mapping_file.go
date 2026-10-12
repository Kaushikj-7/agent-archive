package backfill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"unicode"
	"unicode/utf8"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// MappingFileLimit bounds a reviewable bulk assignment file before discovery.
const MappingFileLimit = 4 << 20

// MappingCountLimit bounds the number of exact assignments in a plan.
const MappingCountLimit = 16384

// HistoricalProjectDefinition introduces an explicit stable import-only identity.
type HistoricalProjectDefinition struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type mappingEntry struct {
	Cwd                 string `json:"cwd"`
	ConfiguredRoot      string `json:"configured_root,omitempty"`
	HistoricalProjectID string `json:"historical_project_id,omitempty"`
}

type mappingFile struct {
	SchemaVersion      int                           `json:"schema_version"`
	HistoricalProjects []HistoricalProjectDefinition `json:"historical_projects"`
	Mappings           []mappingEntry                `json:"mappings"`
}

// ReadMappingFile decodes bounded strict JSON and merges it with inline mappings.
// Its semantic assignments, rather than the filename or bytes, identify resume.
func ReadMappingFile(input io.Reader, filters *Filters) error {
	data, err := io.ReadAll(io.LimitReader(input, MappingFileLimit+1))
	if err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return errors.New("mapping file must be valid UTF-8")
	}
	if len(data) > MappingFileLimit {
		return errors.New("mapping file exceeds 4 MiB")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file mappingFile
	if err := decoder.Decode(&file); err != nil {
		return fmt.Errorf("mapping file: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("mapping file must contain exactly one JSON value")
	}
	if file.SchemaVersion != 1 || file.Mappings == nil || len(file.Mappings) > MappingCountLimit || len(file.HistoricalProjects) > MappingCountLimit {
		return errors.New("mapping file requires schema_version 1 and at most 16384 assignments and projects")
	}
	definitions := map[string]bool{}
	for _, p := range file.HistoricalProjects {
		if !archive.IsHistoricalProjectID(p.ID) || !archive.ValidHistoricalProjectLabel(p.Label) || definitions[p.ID] {
			return errors.New("invalid or duplicate historical project definition")
		}
		definitions[p.ID] = true
	}
	live := map[string]string{}
	for cwd, root := range filters.ProjectMappings {
		live[cwd] = root
	}
	historical := map[string]string{}
	for _, m := range file.Mappings {
		if !validMappingPath(m.Cwd) || (m.ConfiguredRoot == "") == (m.HistoricalProjectID == "") {
			return errors.New("mapping requires absolute cwd and exactly one configured_root or historical_project_id")
		}
		cwd := filepath.Clean(m.Cwd)
		if m.ConfiguredRoot != "" {
			if !validMappingPath(m.ConfiguredRoot) || historical[cwd] != "" || live[cwd] != "" && live[cwd] != filepath.Clean(m.ConfiguredRoot) {
				return errors.New("invalid or conflicting cwd mapping")
			}
			live[cwd] = filepath.Clean(m.ConfiguredRoot)
		} else {
			if !archive.IsHistoricalProjectID(m.HistoricalProjectID) || live[cwd] != "" || historical[cwd] != "" && historical[cwd] != m.HistoricalProjectID {
				return errors.New("invalid or conflicting cwd mapping")
			}
			historical[cwd] = m.HistoricalProjectID
		}
	}
	if len(live)+len(historical) > MappingCountLimit {
		return errors.New("too many mappings")
	}
	filters.ProjectMappings = live
	filters.HistoricalMappings = historical
	filters.HistoricalProjects = file.HistoricalProjects
	filters.BulkMappings = true
	return nil
}

func validMappingPath(value string) bool {
	if !filepath.IsAbs(value) || len(value) > 4096 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// rejectDuplicateJSONKeys prevents ambiguous authorization documents.
func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, container := token.(json.Delim)
		if !container {
			return nil
		}
		switch delimiter {
		case '{':
			keys := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || keys[name] {
					return errors.New("mapping file has duplicate JSON fields")
				}
				keys[name] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid mapping file JSON")
		}
		_, err = decoder.Token()
		return err
	}
	return value()
}

func historicalConfig(cfg config.Config, filters Filters) (config.Config, error) {
	cfg.HistoricalProjects = append([]config.HistoricalProject(nil), cfg.HistoricalProjects...)
	for _, definition := range filters.HistoricalProjects {
		current, found := cfg.HistoricalProject(definition.ID)
		if !found {
			current = config.HistoricalProject{ID: definition.ID, Label: definition.Label}
		}
		current.Permissions = append([]config.HistoricalProjectPermission(nil), current.Permissions...)
		if _, recorded := current.HistoricalPermission(cfg.DestinationID()); !recorded {
			current.Permissions = append(current.Permissions, config.HistoricalProjectPermission{DestinationID: cfg.DestinationID(), Enabled: true})
		}
		if found {
			// Renames are explicit policy operations, not assignment side effects.
			for i := range cfg.HistoricalProjects {
				if cfg.HistoricalProjects[i].ID == current.ID {
					cfg.HistoricalProjects[i] = current
				}
			}
		} else {
			cfg.HistoricalProjects = append(cfg.HistoricalProjects, current)
		}
	}
	for _, id := range filters.HistoricalMappings {
		p, found := cfg.HistoricalProject(id)
		enabled, recorded := p.HistoricalPermission(cfg.DestinationID())
		if !found || !recorded || !enabled {
			return cfg, errors.New("historical mapping needs an enabled project permission for this destination; disabled decisions require explicit enable")
		}
	}
	if len(filters.HistoricalMappings) > 0 {
		cfg.HistoricalProjectProtection = true
	}
	return cfg, cfg.ValidateHistoricalProjects()
}

func (f Filters) mappingDigest() string {
	if len(f.HistoricalMappings) == 0 {
		return mappingDigest(f.ProjectMappings)
	}
	identities := map[string]string{}
	for cwd, root := range f.ProjectMappings {
		identities[cwd] = "configured:" + root
	}
	for cwd, id := range f.HistoricalMappings {
		identities[cwd] = "historical:" + id
	}
	return mappingDigest(identities)
}

func (entry *mappingEntry) UnmarshalJSON(data []byte) error {
	type plain mappingEntry
	var decoded plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("mapping fields cannot be null")
		}
	}
	*entry = mappingEntry(decoded)
	return nil
}

func (file *mappingFile) UnmarshalJSON(data []byte) error {
	type plain mappingFile
	var decoded plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, raw := range fields {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("mapping file fields cannot be null")
		}
	}
	*file = mappingFile(decoded)
	return nil
}
