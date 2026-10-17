package cli

import (
	"bytes"
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

// Unsupported recovery authority must remain at its original path, including
// through the explicit abandon command's generic undecodable fallback.
func TestMuseAbandonPreservesUnsupportedAuthority(t *testing.T) {
	for _, kind := range []string{"future_admission", "protected_snapshot"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
			var raw []byte
			if kind == "future_admission" {
				raw = []byte(`{"files_only":{"version":1,"writer":"native-authority-v16","value":true,"admission":"future-contract"}}`)
			} else {
				future := []byte(`{"schema_version":{"version":16,"writer":"native-authority-v16","base":1,"admission":"future-contract"}}`)
				_, err := json.Marshal(setupjournal.Journal{FilesOnly: true, Changes: []fileapply.Change{{Path: filepath.Join(home, "config.json"), Before: future, After: []byte(`{}`), Existed: true, Mode: 0600}}})
				if err == nil {
					t.Fatal("serializer accepted unsupported rollback snapshot")
				}
				// Build unknown future wire without the current serializer's guard.
				base, err := json.Marshal(map[string]any{"files_only": true, "changes": []fileapply.Change{{Path: filepath.Join(home, "config.json"), Before: future, After: []byte(`{}`), Existed: true, Mode: 0600}}})
				if err != nil {
					t.Fatal(err)
				}
				raw = base
			}
			path := setupjournal.JournalPath(home)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			if code := Run([]string{"setup", "--abandon-recovery"}, nil, &out, &stderr, env); code == 0 {
				t.Fatalf("unsupported authority abandoned: %s %s", &out, &stderr)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, after) {
				t.Fatalf("unsupported journal moved or changed: %v", err)
			}
			entries, err := filepath.Glob(path + "*")
			if err != nil || len(entries) != 1 {
				t.Fatalf("unexpected moved-aside recovery record: %v %v", entries, err)
			}
		})
	}
}
