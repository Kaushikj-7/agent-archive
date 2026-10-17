package backfill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/state"
)

func nativeImportConfirmationControl(t *testing.T) (*state.Store, Batch, archive.NativeImportBatchScope) {
	t.Helper()
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	project := t.TempDir()
	cfg := config.Config{MachineID: "synthetic", RetentionDays: 90, ImportedHarnesses: []string{"muse"}, NativeImportProtection: true, NativeAuthorityProtection: true, DurableStorageProtection: true, PublicationCompositionProtection: true, ImportStagingProtection: true, StagedSelectionProtection: true, Storage: destination.Config{Provider: destination.ProviderS3, Bucket: "synthetic-import", Prefix: "synthetic"}, NativeRoots: []archive.NativeRoot{{Agent: "muse", Configured: root, Canonical: root, Revision: "one"}}, Archive: archive.Config{Enabled: true, SchemaVersion: 1, Projects: []archive.ProjectActivation{{Root: project, ProjectID: archive.ProjectID(project), Included: true}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	b := confirmedNativeBatchFixture(t)
	scope := b.NativeImport.Scope
	b.NativeImport = nil
	b.DestinationID = cfg.DestinationID()
	scope.DestinationID = b.DestinationID
	scope.ProjectIDs = []string{archive.ProjectID(project)}
	scope.Selected[0].ProjectID = scope.ProjectIDs[0]
	fingerprint, err := archive.NativeRootFingerprint(cfg.NativeRoots[0])
	if err != nil {
		t.Fatal(err)
	}
	scope.Selected[0].RootFingerprint = fingerprint
	if err := SaveBatch(home, b); err != nil {
		t.Fatal(err)
	}
	return store, b, scope
}

func TestNativeImportConfirmationMintsOnlyExactCurrentOrdinaryControl(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"valid", "stale", "unknown", "duplicate", "caller-progress"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			store, b, scope := nativeImportConfirmationControl(t)
			path := batchPath(store.Home(), b.ID)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unknown":
				raw = bytes.Replace(raw, []byte("{"), []byte(`{"future":true,`), 1)
			case "duplicate":
				raw = bytes.Replace(raw, []byte("{"), []byte(`{"id":"duplicate",`), 1)
			case "caller-progress":
				b.Sessions = []string{"unrecorded"}
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(raw)
			expected := hex.EncodeToString(sum[:])
			if mode == "stale" {
				expected = hex.EncodeToString(make([]byte, 32))
			}
			confirmed, err := ConfirmNativeImportBatch(t.Context(), store, b, expected, scope)
			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode != "valid" {
				if err == nil {
					t.Fatal("unreviewed control confirmed")
				}
				if !bytes.Equal(raw, got) {
					t.Fatal("refusal mutated ordinary control")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if confirmed.NativeImport == nil || confirmed.NativeImport.Instance == "" {
				t.Fatal("no fresh instance")
			}
			if err := confirmed.nativeImportControl().Validate(t.Context()); err != nil {
				t.Fatal(err)
			}
			scope.Selected[0].NativeID = "caller-mutated"
			if confirmed.NativeImport.Scope.Selected[0].NativeID == scope.Selected[0].NativeID {
				t.Fatal("caller retained mutable scope")
			}
			h, err := store.OpenConfirmedNativeImportBatch(t.Context(), b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := ConfirmNativeImportBatch(t.Context(), store, b, expected, confirmed.NativeImport.Scope); err == nil {
				t.Fatal("existing confirmation replaced")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, after) {
				t.Fatal("repeat changed instance", err)
			}
			for _, name := range []string{"registrations", "sessions", "pending", "published"} {
				entries, err := os.ReadDir(filepath.Join(store.Home(), name))
				if err != nil || len(entries) != 0 {
					t.Fatal("intent created session effect", name, err)
				}
			}
		})
	}
}

func TestNativeImportConfirmationComparisonPreservesExactHeaderAndCompletion(t *testing.T) {
	t.Parallel()
	b := confirmedNativeBatchFixture(t)
	ordinary := b
	ordinary.NativeImport = nil
	raw, err := json.Marshal(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	payload, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := compareNativeImportConfirmation(t.Context(), raw, true, hex.EncodeToString(sum[:]), b, payload); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	ordinary.CompletedAt = &at
	completed, err := json.Marshal(ordinary)
	if err != nil {
		t.Fatal(err)
	}
	sum = sha256.Sum256(completed)
	if err := compareNativeImportConfirmation(t.Context(), completed, true, hex.EncodeToString(sum[:]), b, payload); err == nil {
		t.Fatal("completed batch reused")
	}
}

func TestNativeImportConfirmationConcurrentCASHasOneInstance(t *testing.T) {
	t.Parallel()
	store, b, scope := nativeImportConfirmationControl(t)
	raw, err := os.ReadFile(batchPath(store.Home(), b.ID))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	expected := hex.EncodeToString(sum[:])
	type outcome struct {
		batch Batch
		err   error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			confirmed, err := ConfirmNativeImportBatch(t.Context(), store, b, expected, scope)
			results <- outcome{confirmed, err}
		}()
	}
	close(start)
	successes := 0
	instance := ""
	for range 2 {
		result := <-results
		if result.err == nil {
			successes++
			instance = result.batch.NativeImport.Instance
		}
	}
	if successes != 1 {
		t.Fatal("confirmation CAS winner count", successes)
	}
	current, err := os.ReadFile(batchPath(store.Home(), b.ID))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Batch
	if err := decodeBatch(current, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.NativeImport == nil || persisted.NativeImport.Instance != instance {
		t.Fatal("loser replaced exact winning instance")
	}
}
