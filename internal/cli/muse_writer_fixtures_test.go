package cli

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/fileapply"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

var museWriterFixtureDirectory = flag.String("muse-writer-fixture-directory", "", "export synthetic Muse protected writer fixtures to an explicit acceptance directory")

// This serializer export is opt-in; ordinary tests retain everything in TempDir.
// The actual old executable is run independently against copied isolated homes.
func TestMuseProtectedWriterFixtures(t *testing.T) {
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cfg := config.Config{MachineID: "00000000000000000000000000000001", RetentionDays: 90, Harnesses: []string{"muse"}, Storage: destination.Config{Provider: destination.ProviderS3, Bucket: "synthetic-old-writer", Prefix: "fixture", AWSProfile: "synthetic"}, DestinationSince: at.Add(-time.Hour), NativeRoots: []archive.NativeRoot{{Agent: "muse", Configured: "/synthetic/muse", Canonical: "/synthetic/muse/sessions", Revision: "fixture-root"}}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: "/synthetic/project", ProjectID: archive.ProjectID("/synthetic/project"), Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	active, err := config.ReconcileMuseCapture(cfg, config.Config{}, at)
	if err != nil {
		t.Fatal(err)
	}
	sticky := active
	sticky.Archive.Enabled = false
	sticky, err = config.ReconcileMuseCapture(sticky, active, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	scalar, err := json.Marshal(config.Config{MachineID: cfg.MachineID, RetentionDays: 90, Storage: cfg.Storage, Archive: archive.Config{Enabled: false}})
	if err != nil {
		t.Fatal(err)
	}
	fixtures := map[string][]byte{"control-config.json": scalar}
	for name, value := range map[string]config.Config{"active": active, "sticky": sticky} {
		raw, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		var decoded config.Config
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if !decoded.NativeAdmissionProtection || !decoded.MuseCaptureLastIssuedAt.Equal(at) {
			t.Fatal("serializer lost admission protection")
		}
		fixtures["config-"+name+".json"] = append(raw, '\n')
	}
	for name, before := range map[string][]byte{"before": fixtures["config-sticky.json"], "after": scalar} {
		after := fixtures["config-active.json"]
		journal := setupjournal.Journal{FilesOnly: true, Changes: []fileapply.Change{{Path: "/synthetic/archive/config.json", Before: before, After: after, Existed: true, Mode: 0600}}}
		raw, err := json.MarshalIndent(journal, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		var decoded setupjournal.Journal
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if !decoded.NativeAdmissionProtection {
			t.Fatal("journal lost admission fence")
		}
		fixtures["journal-"+name+".json"] = append(raw, '\n')
	}
	directory := *museWriterFixtureDirectory
	if directory == "" {
		directory = t.TempDir()
	} else if !filepath.IsAbs(directory) {
		t.Fatal("fixture export requires absolute directory")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	for name, raw := range fixtures {
		if err := os.WriteFile(filepath.Join(directory, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
