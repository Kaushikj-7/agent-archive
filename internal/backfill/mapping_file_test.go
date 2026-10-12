package backfill

import (
	"encoding/json"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBulkMappingsCanonicalIdentityAndConflicts(t *testing.T) {
	t.Parallel()
	const id = "11111111111111111111111111111111"
	entries := make([]mappingEntry, 256)
	for i := range entries {
		entries[i] = mappingEntry{Cwd: fmt.Sprintf("/gone/%03d", i), HistoricalProjectID: id}
	}
	file := mappingFile{SchemaVersion: 1, HistoricalProjects: []HistoricalProjectDefinition{{ID: id, Label: "Service"}}, Mappings: entries}
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	var first Filters
	if err := ReadMappingFile(strings.NewReader(string(raw)), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.HistoricalMappings) != 256 {
		t.Fatal("bulk mappings retained interactive cap")
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	file.HistoricalProjects[0].Label = "Renamed service"
	raw, err = json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	var reversed Filters
	if err := ReadMappingFile(strings.NewReader(string(raw)), &reversed); err != nil {
		t.Fatal(err)
	}
	if first.mappingDigest() != reversed.mappingDigest() {
		t.Fatal("order or label changed ownership digest")
	}
	entries[0].HistoricalProjectID = strings.Repeat("2", 32)
	raw, err = json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	var reassigned Filters
	if err := ReadMappingFile(strings.NewReader(string(raw)), &reassigned); err != nil {
		t.Fatal(err)
	}
	if first.mappingDigest() == reassigned.mappingDigest() {
		t.Fatal("ownership reassignment resumed old batch")
	}
	for _, value := range []string{
		`{"schema_version":1,"schema_version":1,"mappings":[]}`,
		`{"schema_version":2,"mappings":[]}`,
		`{"schema_version":1,"unknown":1,"mappings":[]}`,
		`{"schema_version":1,"mappings":null}`,
		`{"schema_version":1,"mappings":[]} {}`,
		`{"schema_version":1,"mappings":[{"cwd":"/gone/a","configured_root":"/live","historical_project_id":"` + id + `"}]}`,
		`{"schema_version":1,"mappings":[{"cwd":"/gone/a","configured_root":"","historical_project_id":"` + id + `"}]}`,
		`{"schema_version":1,"mappings":[{"cwd":"/gone/a","configured_root":"/live","historical_project_id":""}]}`,
		`{"schema_version":1,"mappings":[{"cwd":"/reviewed","CWD":"/different","configured_root":"/live"}]}`,
		`{"SCHEMA_VERSION":1,"mappings":[]}`,
		`{"schema_version":1,"historical_projects":[{"id":"` + id + `","label":"Reviewed","Label":"Different"}],"mappings":[]}`,
		`{"schema_version":1,"mappings":[{"cwd":"/gone/a","configured_root":"/live"},{"cwd":"/gone/./a","historical_project_id":"` + id + `"}]}`,
	} {
		var invalid Filters
		if err := ReadMappingFile(strings.NewReader(value), &invalid); err == nil {
			t.Errorf("ambiguous mapping accepted: %s", value)
		}
	}
	var tooLarge Filters
	if ReadMappingFile(strings.NewReader(strings.Repeat(" ", MappingFileLimit+1)), &tooLarge) == nil {
		t.Fatal("unbounded mapping file")
	}
}

func TestHistoricalTemporaryOriginalCwdCannotEscapeThroughSymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	temporary := filepath.Join(root, "temporary")
	service := filepath.Join(home, "service")
	for _, path := range []string{temporary, service} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(temporary, "service")
	if err := os.Symlink(service, alias); err != nil {
		t.Fatal(err)
	}
	env := Environment{Home: home, TempDirs: []string{temporary}}
	if NewHistoricalSourcePolicy(env, config.Config{}, Filters{})(alias) {
		t.Fatal("temporary original cwd gained permission through its canonical target")
	}
	if !NewHistoricalSourcePolicy(env, config.Config{}, Filters{IncludeTemp: true})(alias) {
		t.Fatal("explicit temporary-source permission refused")
	}
}

func TestExcludedHistoricalDependencyCountsAsExcluded(t *testing.T) {
	t.Parallel()
	if got := candidateDisposition(Candidate{Skip: SkipExcludedHistory}); got != "excluded" {
		t.Fatalf("excluded dependency counted as %q", got)
	}
}

func TestHistoricalSourceRestrictionsCannotBeBypassedByAssignment(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	env := Environment{Home: filepath.Join(root, "home"), TempDirs: []string{filepath.Join(root, "temporary")}}
	mustMkdir := func(path string) {
		t.Helper()
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir(env.Home)
	mustMkdir(env.TempDirs[0])
	cfg := config.Config{Archive: archive.Config{Projects: []archive.ProjectActivation{{Root: filepath.Join(env.Home, "private"), Included: false}}}}
	sourceAllowed := NewHistoricalSourcePolicy(env, cfg, Filters{})
	for _, cwd := range []string{env.Home, root, env.TempDirs[0], filepath.Join(env.Home, "private", "gone")} {
		if sourceAllowed(cwd) {
			t.Fatalf("restricted dependency permitted: %s", cwd)
		}
	}
	if !sourceAllowed(filepath.Join(env.Home, "gone-service")) {
		t.Fatal("ordinary vanished cwd refused")
	}
	overridden := NewHistoricalSourcePolicy(env, cfg, Filters{IncludeHome: true, IncludeTemp: true})
	if !overridden(env.Home) || !overridden(env.TempDirs[0]) || overridden(root) || overridden(filepath.Join(env.Home, "private")) {
		t.Fatal("override bypassed above-home or explicit exclusion")
	}
}
