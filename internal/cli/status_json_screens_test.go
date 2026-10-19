package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// recordStatusJSON, set to 1, rewrites testdata/status-json from the current
// build. It is not -update on purpose: the files are status --json as main
// printed it for the status screens' fixtures, recorded on main, so that a
// change to how status looks can be shown to leave its JSON unchanged. Only
// a change meant to change status --json records them again, on its own.
const recordStatusJSON = "AGENT_ARCHIVE_RECORD_STATUS_JSON"

// statusJSONIDs matches the IDs hashed from the fixture's temporary paths,
// which differ from run to run.
var statusJSONIDs = regexp.MustCompile(`("configuration_id": "|"project_id": "project-)[0-9a-f]+`)

// status --json prints, byte for byte, what it printed before the text
// status was redesigned, for every status screen's fixture.
func TestStatusJSONUnchangedForStatusScreens(t *testing.T) {
	t.Parallel()
	seen := 0
	for _, sc := range screens {
		if !strings.HasPrefix(sc.name, "status-") {
			continue
		}
		seen++
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			f := newScreenFixture(t)
			if sc.arrange != nil {
				sc.arrange(t, f)
			}
			args := append(append([]string{}, sc.args...), "--json")
			got := statusJSONIDs.ReplaceAll(f.run(t, args, nil, false, 0), []byte("${1}ID"))
			path := filepath.Join("testdata", "status-json", sc.name+".json.txt")
			if os.Getenv(recordStatusJSON) == "1" {
				must(t, os.MkdirAll(filepath.Dir(path), 0o755))
				must(t, os.WriteFile(path, got, 0o644))
			}
			want, err := os.ReadFile(path)
			must(t, err)
			// Text fixtures may move between overview and verbose views. The
			// command-line heading is not part of the versioned JSON document.
			_, got, _ = bytes.Cut(got, []byte("\n"))
			_, want, _ = bytes.Cut(want, []byte("\n"))
			if !bytes.Equal(got, want) {
				t.Fatalf("status --json changed for %s:\ngot:\n%s\nwant:\n%s", sc.name, got, want)
			}
		})
	}
	if seen < 5 {
		t.Fatalf("found %d status screens, want ready, waiting, needs attention, paused and not set up", seen)
	}
}
