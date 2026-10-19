package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
)

// The complete production census crosses its former shared root-entry window.
// These are physical synthetic files/directories, not a virtual entry count.
func TestDurableCensusCompletesCombinedRoots(t *testing.T) {
	t.Log("begin physical synthetic combined-root fixture construction")
	s := newTestStore(t)
	reg := registration(t)
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: reg.ArchiveSessionID, NativeSessionID: reg.NativeSessionID, ProjectID: reg.ProjectID, Capture: archive.SourceCapture{Harness: reg.Harness, AdapterName: "test", AdapterVersion: "test-v1", FilterVersion: archive.FilterVersion, CapturedAt: reg.RegisteredAt}}
	published, err := s.LoadPublishedState("prototype")
	if err != nil {
		t.Fatal(err)
	}
	if err = published.SaveBlocked(bundle, reg.RegisteredAt, BlockedReasonTranscriptMissing); err != nil {
		t.Fatal(err)
	}
	// Both controls use the identical producer-shaped supported v0 peer
	// representation, registration, bundle and actual source-set writer.
	seedPeer := func(store *Store, id string) int64 {
		value := published.state
		value.Bundle.ArchiveSessionID, value.Bundle.NativeSessionID = id, id
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(store.publishedPath(id), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.MkdirAll(filepath.Join(store.home, "sessions", id), 0700); err != nil {
			t.Fatal(err)
		}
		return int64(len(raw))
	}
	reg.Origin = archive.SessionOriginImport
	reg.ImportBatch = archive.NewImportBatch("synthetic-census")
	small := newTestStore(t)
	seedPeer(small, "small-peer")
	smallDigest, err := small.SaveImportSourceSet(t.Context(), reg, bundle, nil, config.SkillEvidenceNone)
	if err != nil {
		t.Fatal("small identical producer positive control", err)
	}
	smallReg := reg
	smallReg.StagedImportSHA256 = smallDigest
	selected, selectedFound, err := small.LoadImportStage(t.Context(), smallReg)
	if err != nil || !selectedFound || selected.SHA256 != smallDigest || !reflect.DeepEqual(selected.Bundle, bundle) {
		t.Fatal("small producer exact stage/digest readback", selectedFound, err)
	}
	t.Log("small identical SaveImportSourceSet -> LoadImportStage positive control PASS; exact selected bundle/digest")
	settledBytes := int64(0)
	for i := range durableEntryLimit / 2 {
		settledBytes += seedPeer(s, fmt.Sprintf("settled-%05d", i))
	}
	physicalPublished, err := os.ReadDir(filepath.Join(s.home, "published"))
	if err != nil {
		t.Fatal("physical fixture published enumeration", err)
	}
	physicalSessions, err := os.ReadDir(filepath.Join(s.home, "sessions"))
	if err != nil {
		t.Fatal("physical fixture session enumeration", err)
	}
	if len(physicalPublished) != durableEntryLimit/2+1 || len(physicalSessions) != durableEntryLimit/2 {
		t.Fatal("physical fixture did not cross combined entry window", len(physicalPublished), len(physicalSessions))
	}
	t.Logf("physical fixture complete: published=%d sessions=%d combined=%d; begin actual SaveImportSourceSet", len(physicalPublished), len(physicalSessions), len(physicalPublished)+len(physicalSessions))
	started := time.Now()
	digest, err := s.SaveImportSourceSet(t.Context(), reg, bundle, nil, config.SkillEvidenceNone)
	if err != nil {
		t.Fatal("supported stage beyond combined entry window", err)
	}
	t.Logf("physical combined inventory: published=%d session_directories=%d supported_save_elapsed=%s legacy_wire_bytes=%d", durableEntryLimit/2+1, durableEntryLimit/2+1, time.Since(started), settledBytes)
	reg.StagedImportSHA256 = digest
	stagePath := filepath.Join(s.home, importStageRelative(reg.ArchiveSessionID))
	info, err := os.Stat(stagePath)
	if err != nil {
		t.Fatal(err)
	}
	expectedPhysical := info.Size()
	check := func() error {
		return config.WithDurableStorage(s.home, func(g config.DurableStorageGuard) (err error) {
			// This callback already holds hooks; the entire census below
			// remains in that span. These measurements are lower bounds on
			// the outer lock lifetime, not a short-hold qualification.
			hooksCallback := time.Now()
			q, err := s.openDurableQuota(g)
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, q.Close()) }()
			quotaAcquired := time.Now()
			usage, err := q.usageContext(t.Context())
			t.Logf("locked census: quota_acquisition_elapsed=%s census_inside_hooks_and_quota=%s hooks_callback_elapsed=%s", quotaAcquired.Sub(hooksCallback), time.Since(quotaAcquired), time.Since(hooksCallback))
			if err == nil && (usage.physical != expectedPhysical || usage.charged != 2*expectedPhysical+durableControlBytes) {
				t.Fatalf("incomplete accounting: %+v physical=%d", usage, expectedPhysical)
			}
			return err
		})
	}
	if err = check(); err != nil {
		t.Fatal(err)
	}
	obligations, err := s.DurableStorageObligations()
	if err != nil || len(obligations) != 1 || obligations[0].SessionID != reg.ArchiveSessionID || obligations[0].Namespace != ImportAdmissionStorage {
		t.Fatalf("late supported obligation: count=%d err=%v", len(obligations), err)
	}
	// A second observation must see obligations created after recovery.
	bad := filepath.Join(s.home, "temporary-reservations", "unavailable")
	if err = os.MkdirAll(filepath.Dir(bad), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(bad, []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	obligations, err = s.DurableStorageObligations()
	if err != nil || len(obligations) != 2 || obligations[1].Namespace != UnavailableStorage {
		t.Fatalf("late unavailable owner lost: %+v %v", obligations, err)
	}
	if _, err = s.StagePendingSource("new-owner", durableRef([]byte("new")), []byte("new")); !errors.Is(err, ErrDurableStorageRecovery) {
		t.Fatal("unknown owner granted capacity", err)
	}
	if err = os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.SaveImportSourceSet(canceled, reg, bundle, nil, config.SkillEvidenceNone); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled census/write completed", err)
	}
	if err = check(); err != nil {
		t.Fatal("cancellation changed complete accounting", err)
	}
}
