package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

type confirmedPolicyCase string

const (
	confirmedPolicyAccepted    confirmedPolicyCase = "accepted"
	confirmedPolicyPaused      confirmedPolicyCase = "paused"
	confirmedPolicyDisabled    confirmedPolicyCase = "disabled"
	confirmedPolicyDestination confirmedPolicyCase = "destination"
	confirmedPolicyHarness     confirmedPolicyCase = "harness"
	confirmedPolicyPrivacy     confirmedPolicyCase = "privacy"
	confirmedPolicyUnreadable  confirmedPolicyCase = "unreadable"
	confirmedPolicyMissing     confirmedPolicyCase = "missing"
)

func TestConfirmedImportPolicyCauseDistinguishesKnownRefusal(t *testing.T) {
	t.Parallel()
	for _, name := range []confirmedPolicyCase{confirmedPolicyAccepted, confirmedPolicyPaused, confirmedPolicyDisabled, confirmedPolicyDestination, confirmedPolicyHarness, confirmedPolicyPrivacy, confirmedPolicyUnreadable, confirmedPolicyMissing} {
		t.Run(string(name), func(t *testing.T) {
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
			case confirmedPolicyPaused:
				cfg.Paused = true
			case confirmedPolicyDisabled:
				cfg.Archive.Enabled = false
			case confirmedPolicyDestination:
				cfg.Storage.Bucket = "synthetic-other-destination"
			case confirmedPolicyHarness:
				cfg.Harnesses = []string{archive.HarnessClaude}
			case confirmedPolicyPrivacy:
				cfg.SkillEvidence = config.SkillEvidenceNone
			}
			if err := config.Save(s.Home(), cfg); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.Home(), "config.json")
			switch name {
			case confirmedPolicyUnreadable:
				if err := os.WriteFile(path, []byte("{synthetic-unreadable"), 0600); err != nil {
					t.Fatal(err)
				}
			case confirmedPolicyMissing:
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			_, err = s.SaveConfirmedImportSourceSet(t.Context(), reg, bundle, nil, mode)
			known := name != confirmedPolicyAccepted && name != confirmedPolicyUnreadable && name != confirmedPolicyMissing
			if errors.Is(err, ErrImportPolicyChanged) != known || (err == nil) != (name == confirmedPolicyAccepted) {
				t.Fatal("confirmed stage confused known policy rejection with unknown state", known, err)
			}
			present, readErr := s.HasImportStage(reg.ArchiveSessionID)
			if readErr != nil || present != (name == confirmedPolicyAccepted) {
				t.Fatal("refused confirmed stage created retained evidence", present, readErr)
			}
		})
	}
}
