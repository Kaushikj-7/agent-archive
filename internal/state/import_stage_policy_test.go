package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

func TestConfirmedImportPolicyCauseDistinguishesKnownRefusal(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"accepted", "paused", "disabled", "destination", "harness", "privacy", "unreadable", "missing"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			cfg, _, err := config.Load(s.Home())
			if err != nil {
				t.Fatal(err)
			}
			cfg.Archive.Enabled = true
			reg := registration(t)
			reg.Origin, reg.ImportBatch, reg.DestinationID = archive.SessionOriginImport, archive.NewImportBatch("synthetic-policy"), cfg.DestinationID()
			bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "test", AdapterVersion: "test-v1", FilterVersion: archive.FilterVersion, CapturedAt: reg.RegisteredAt}, NativeRecords: []map[string]any{{"synthetic": "policy control"}}}
			mode := cfg.EffectiveSkillEvidence()
			switch name {
			case "paused":
				cfg.Paused = true
			case "disabled":
				cfg.Archive.Enabled = false
			case "destination":
				cfg.Storage.Bucket = "synthetic-other-destination"
			case "harness":
				cfg.Harnesses = []string{archive.HarnessClaude}
			case "privacy":
				cfg.SkillEvidence = config.SkillEvidenceNone
			}
			if err := config.Save(s.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.Home(), "config.json")
			if name == "unreadable" {
				if err := os.WriteFile(path, []byte("{synthetic-unreadable"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if name == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			_, err = s.SaveConfirmedImportSourceSet(t.Context(), reg, bundle, nil, mode)
			known := name != "accepted" && name != "unreadable" && name != "missing"
			if errors.Is(err, ErrImportPolicyChanged) != known || (err == nil) != (name == "accepted") {
				t.Fatal("confirmed stage confused known policy rejection with unknown state", known, err)
			}
			present, readErr := s.HasImportStage(reg.ArchiveSessionID)
			if readErr != nil || present != (name == "accepted") {
				t.Fatal("refused confirmed stage created retained evidence", present, readErr)
			}
		})
	}
}
