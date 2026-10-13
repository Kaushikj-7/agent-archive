package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type scopedMigrationCLIStore struct{ *qualifiedCLIStore }

func (*scopedMigrationCLIStore) VerifyCatalogCutover(_ context.Context, source, target destination.Config) (catalog.CutoverProof, error) {
	return catalog.CutoverProof{ID: "private-scoped-credential-proof", Source: config.DestinationID(source), Destination: config.DestinationID(target), Protocol: 11}, nil
}

func TestRunScopedUpgradeResumesProtectedConfigurationAndKeepsRollbackFenced(t *testing.T) {
	t.Parallel()
	store := &scopedMigrationCLIStore{&qualifiedCLIStore{storagetest.NewMemoryStore()}}
	cfg := config.Config{SchemaVersion: 11, RetentionDays: 90, Storage: destination.Config{Provider: "s3", Bucket: "private-picker", Prefix: "synthetic", Region: "us-east-1", ArchiveFormat: destination.FormatCatalogV4}}
	env := pickerOperatorEnv(t, cfg)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	run := func(extra ...string) int {
		var out, stderr bytes.Buffer
		code := Run(append([]string{"migrate", "--scoped-picker"}, extra...), nil, &out, &stderr, env)
		if code != 0 {
			t.Log(stderr.String())
		}
		return code
	}
	if code := run(); code != 0 {
		t.Fatal(code)
	}
	// A repeated invocation acknowledges an already saved configuration without
	// rebuilding or changing the stronger head, even after process restart.
	if code := run(); code != 0 {
		t.Fatal(code)
	}
	home, err := env.readHome()
	if err != nil {
		t.Fatal(err)
	}
	saved, _, err := config.Load(home)
	if err != nil || saved.SchemaVersion != 14 || !saved.ScopedPickerProtection {
		t.Fatal(saved, err)
	}
	if code := run("--rollback"); code != 0 {
		t.Fatal(code)
	}
	saved, _, err = config.Load(home)
	if err != nil || !saved.Paused || saved.SchemaVersion != 14 {
		t.Fatal(saved, err)
	}
}

func TestRunScopedUpgradePreservesUnknownPendingEvidence(t *testing.T) {
	t.Parallel()
	store := &scopedMigrationCLIStore{&qualifiedCLIStore{storagetest.NewMemoryStore()}}
	cfg := config.Config{SchemaVersion: 11, RetentionDays: 90, Storage: destination.Config{Provider: "s3", Bucket: "private-picker", Prefix: "synthetic", Region: "us-east-1", ArchiveFormat: destination.FormatCatalogV4}}
	env := pickerOperatorEnv(t, cfg)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	home, err := env.readHome()
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(home, "pending")
	if err = os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "foreign.json")
	original := []byte(`{"unknown_future_pending":true}`)
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := Run([]string{"migrate", "--scoped-picker"}, nil, &out, &stderr, env); code != 1 {
		t.Fatal(code)
	}
	if !strings.Contains(stderr.String(), "exact original publication") {
		t.Fatal(stderr.String())
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatal("changed unknown evidence", err)
	}
	if _, err = catalog.PickerUpgradeCheckpoint(t.Context(), store); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("claimed upgrade with unknown local work", err)
	}
}

func pickerOperatorEnv(t *testing.T, cfg config.Config) Env {
	t.Helper()
	env := operatorTestEnv(t, cfg)
	home, err := env.readHome()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	return env
}

type scopedConfigurationFaultStore struct {
	*scopedMigrationCLIStore
	beforeConfiguration func() error
}

func (s *scopedConfigurationFaultStore) PutConditional(ctx context.Context, key string, raw []byte, condition storage.PutCondition) (string, error) {
	etag, err := s.scopedMigrationCLIStore.PutConditional(ctx, key, raw, condition)
	if err != nil {
		return etag, err
	}
	if key == catalog.CoordinatorKey && s.beforeConfiguration != nil {
		var state struct {
			Upgrade *catalog.PickerUpgradeState `json:"picker_upgrade"`
		}
		if err = json.Unmarshal(raw, &state); err != nil {
			return etag, err
		}
		if state.Upgrade != nil && state.Upgrade.Phase == catalog.PickerUpgradeActive {
			fn := s.beforeConfiguration
			s.beforeConfiguration = nil
			if err = fn(); err != nil {
				return etag, err
			}
		}
	}
	return etag, nil
}

func TestRunScopedUpgradeRecoversConfigurationSaveAfterHeadActivation(t *testing.T) {
	t.Parallel()
	store := &scopedConfigurationFaultStore{scopedMigrationCLIStore: &scopedMigrationCLIStore{&qualifiedCLIStore{storagetest.NewMemoryStore()}}}
	cfg := config.Config{SchemaVersion: 11, RetentionDays: 90, Storage: destination.Config{Provider: "s3", Bucket: "private-picker", Prefix: "synthetic", Region: "us-east-1", ArchiveFormat: destination.FormatCatalogV4}}
	env := pickerOperatorEnv(t, cfg)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	home, err := env.readHome()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	saved := filepath.Join(home, "synthetic-config-save-fault.json")
	store.beforeConfiguration = func() error {
		if err := os.Rename(path, saved); err != nil {
			return err
		}
		return os.Mkdir(path, 0700)
	}
	run := func() int {
		var out, stderr bytes.Buffer
		return Run([]string{"migrate", "--scoped-picker"}, nil, &out, &stderr, env)
	}
	if code := run(); code != 1 {
		t.Fatal("configuration fault missing", code)
	}
	checkpoint, err := catalog.PickerUpgradeCheckpoint(t.Context(), store)
	if err != nil || checkpoint.Phase != catalog.PickerUpgradeActive {
		t.Fatal(checkpoint, err)
	}
	w, err := catalog.New(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.PutImmutable(t.Context(), catalog.KindMetadata, []byte(`{}`)); !errors.Is(err, catalog.ErrAdmissionClosed) {
		t.Fatal("released seal before local save", err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(saved, path); err != nil {
		t.Fatal(err)
	}
	if code := run(); code != 0 {
		t.Fatal("configuration resumption failed", code)
	}
	final, _, err := config.Load(home)
	if err != nil || !final.ScopedPickerProtection || final.SchemaVersion != 14 {
		t.Fatal(final, err)
	}
	if _, err = w.PutImmutable(t.Context(), catalog.KindMetadata, []byte(`{}`)); err != nil {
		t.Fatal("exact activation acknowledgement did not release", err)
	}
}

func TestRunScopedMigrationAndRollbackRetainSettledCursorProtection(t *testing.T) {
	t.Parallel()
	source := storagetest.NewMemoryStore()
	target := &scopedMigrationCLIStore{&qualifiedCLIStore{storagetest.NewMemoryStore()}}
	cfg := config.Config{RetentionDays: 90, SettledCursorSelectionProtection: true, StagedSelectionProtection: true, ImportStagingProtection: true, PublicationCompositionProtection: true, DurableStorageProtection: true, Storage: destination.Config{Provider: "s3", Bucket: "private-legacy", Prefix: "old", Region: "us-east-1"}}
	env := pickerOperatorEnv(t, cfg)
	env.OpenStore = func(c config.Config) (storage.ObjectStore, error) {
		if c.Storage.Bucket == "private-scoped" {
			return target, nil
		}
		return source, nil
	}
	for _, rollback := range []bool{false, true} {
		args := []string{"migrate", "--format", "catalog-v4", "--bucket", "private-scoped", "--prefix", "new", "--scoped-picker"}
		if rollback {
			args = append(args, "--rollback")
		}
		var out, stderr bytes.Buffer
		if code := Run(args, nil, &out, &stderr, env); code != 0 {
			t.Fatal(code, stderr.String())
		}
		home, err := env.readHome()
		if err != nil {
			t.Fatal(err)
		}
		saved, _, err := config.Load(home)
		if err != nil || saved.SchemaVersion != 14 || !saved.ScopedPickerProtection || !saved.SettledCursorSelectionProtection || !saved.StagedSelectionProtection || !saved.ImportStagingProtection || !saved.PublicationCompositionProtection || !saved.DurableStorageProtection || saved.SupportsStagedSelection() || saved.SupportsSettledCursorSelection() || saved.Paused != rollback {
			t.Fatal("migration or rollback lost settled protections", saved, err)
		}
	}
}
