package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestStagingWriterRefusesFutureCapabilityBeforeHookObservation(t *testing.T) {
	s := newTestStore(t)
	if err := config.Save(s.Home(), config.Config{RetentionDays: 90}); err != nil {
		t.Fatal(err)
	}
	r := registration(t)
	if err := s.SaveRegistration(r); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.registrationPath(r.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["staged_import_sha256"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := local.Write(s.registrationPath(r.ArchiveSessionID), fields); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.registrationPath(r.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(s.Home(), "config.json")
	raw, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	fields = map[string]any{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["schema_version"] = map[string]any{"version": 10, "writer": "future-staging-v10"}
	fields["import_staging_protection"] = true
	if err := local.Write(configPath, fields); err != nil {
		t.Fatal(err)
	}
	if err := config.WithDurableStorage(s.Home(), func(config.DurableStorageGuard) error {
		return s.RecordHookObservation(r.ArchiveSessionID, time.Now())
	}); err == nil {
		t.Fatal("unsupported staging writer mutated a registration")
	}
	after, err := os.ReadFile(s.registrationPath(r.ArchiveSessionID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refusal lost staged ownership evidence")
	}
}
