package backfill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
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

// EncodeMappingFile exports exact reviewed assignments in the existing schema.
// It validates through the strict reader before returning bounded private bytes.
func EncodeMappingFile(filters Filters) ([]byte, error) {
	if len(filters.ProjectMappings)+len(filters.HistoricalMappings) > MappingCountLimit || len(filters.HistoricalProjects) > MappingCountLimit {
		return nil, errors.New("too many mappings or historical project definitions")
	}
	if err := validateExportSize(filters); err != nil {
		return nil, err
	}
	file := mappingFile{SchemaVersion: 1, HistoricalProjects: append([]HistoricalProjectDefinition{}, filters.HistoricalProjects...), Mappings: []mappingEntry{}}
	for cwd, root := range filters.ProjectMappings {
		file.Mappings = append(file.Mappings, mappingEntry{Cwd: cwd, ConfiguredRoot: root})
	}
	for cwd, id := range filters.HistoricalMappings {
		file.Mappings = append(file.Mappings, mappingEntry{Cwd: cwd, HistoricalProjectID: id})
	}
	slices.SortFunc(file.Mappings, func(a, b mappingEntry) int { return strings.Compare(a.Cwd, b.Cwd) })
	slices.SortFunc(file.HistoricalProjects, func(a, b HistoricalProjectDefinition) int { return strings.Compare(a.ID, b.ID) })
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	var checked Filters
	if err := ReadMappingFile(bytes.NewReader(data), &checked); err != nil {
		return nil, err
	}
	return data, nil
}

// validateExportSize bounds raw string payload before JSON escaping/indentation.
// The strict reader separately enforces the exact encoded 4 MiB file limit.
func validateExportSize(filters Filters) error {
	payload := 0
	for cwd, root := range filters.ProjectMappings {
		if !validMappingPath(cwd) || !validMappingPath(root) {
			return errors.New("mapping export needs bounded absolute paths")
		}
		payload += len(cwd) + len(root)
	}
	for cwd, id := range filters.HistoricalMappings {
		if !validMappingPath(cwd) || !archive.IsHistoricalProjectID(id) {
			return errors.New("mapping export needs bounded absolute paths and historical identities")
		}
		payload += len(cwd) + len(id)
	}
	for _, project := range filters.HistoricalProjects {
		if !archive.IsHistoricalProjectID(project.ID) || !archive.ValidHistoricalProjectLabel(project.Label) {
			return errors.New("invalid historical project definition")
		}
		payload += len(project.ID) + len(project.Label)
	}
	if payload > MappingFileLimit {
		return errors.New("mapping file exceeds 4 MiB")
	}
	return nil
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
	maps.Copy(live, filters.ProjectMappings)
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
	fields, err := strictMappingFields(data, "cwd", "configured_root", "historical_project_id")
	if err != nil {
		return err
	}
	_, live := fields["configured_root"]
	_, historical := fields["historical_project_id"]
	if live == historical {
		return errors.New("mapping requires exactly one configured_root or historical_project_id field")
	}
	type plain mappingEntry
	var decoded plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*entry = mappingEntry(decoded)
	return nil
}

func (file *mappingFile) UnmarshalJSON(data []byte) error {
	if _, err := strictMappingFields(data, "schema_version", "historical_projects", "mappings"); err != nil {
		return err
	}
	type plain mappingFile
	var decoded plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	*file = mappingFile(decoded)
	return nil
}

// UnmarshalJSON requires schema-exact names, rather than Go's case-insensitive aliases.
func (definition *HistoricalProjectDefinition) UnmarshalJSON(data []byte) error {
	if _, err := strictMappingFields(data, "id", "label"); err != nil {
		return err
	}
	type plain HistoricalProjectDefinition
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*definition = HistoricalProjectDefinition(decoded)
	return nil
}

func strictMappingFields(data []byte, allowed ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("mapping document entries must be objects")
	}
	for name, raw := range fields {
		known := false
		for _, field := range allowed {
			known = known || name == field
		}
		if !known {
			return nil, fmt.Errorf("unknown mapping field %q", name)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, errors.New("mapping fields cannot be null")
		}
	}
	return fields, nil
}
