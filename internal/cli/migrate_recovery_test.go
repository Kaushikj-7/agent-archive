package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type migrationRecoveryCLIStore struct {
	*migrationCLIStore
	failSeal           bool
	blocked            bool
	beforeRollbackSave func() error
}

func (s *migrationRecoveryCLIStore) PutConditional(ctx context.Context, key string, raw []byte, condition storage.PutCondition) (string, error) {
	if key == catalog.CoordinatorKey && s.failSeal {
		var state struct {
			Seal   string          `json:"seal"`
			Intent json.RawMessage `json:"migration_intent"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			return "", err
		}
		if state.Seal != "" && len(state.Intent) > 0 {
			s.failSeal = false
			if _, err := s.migrationCLIStore.PutConditional(ctx, key, raw, condition); err != nil {
				return "", err
			}
			s.blocked = true
			return "", errors.New("private crash after initial seal")
		}
	}
	if key == catalog.MigrationKey && s.beforeRollbackSave != nil {
		var checkpoint catalog.CatalogMigration
		if err := json.Unmarshal(raw, &checkpoint); err != nil {
			return "", err
		}
		if checkpoint.Phase == catalog.MigrationRollback {
			fn := s.beforeRollbackSave
			s.beforeRollbackSave = nil
			if err := fn(); err != nil {
				return "", err
			}
		}
	}
	return s.migrationCLIStore.PutConditional(ctx, key, raw, condition)
}

func (s *migrationRecoveryCLIStore) GetCatalogVersion(ctx context.Context, key string, limit int64) ([]byte, storage.CatalogObjectVersion, error) {
	if s.blocked {
		return nil, storage.CatalogObjectVersion{}, errors.New("private interrupted migration readback")
	}
	return s.migrationCLIStore.GetCatalogVersion(ctx, key, limit)
}

func TestRunMigrateRecoversInitialSealAndCompletedRollbackConfigFailure(t *testing.T) {
	source := storagetest.NewMemoryStore()
	target := &migrationRecoveryCLIStore{migrationCLIStore: &migrationCLIStore{&qualifiedCLIStore{storagetest.NewMemoryStore()}}, failSeal: true}
	original := config.Config{SchemaVersion: 1, Storage: destination.Config{Provider: "s3", Bucket: "private-source", Prefix: "old", Region: "us-east-1", AWSProfile: "private-synthetic"}}
	env := operatorTestEnv(t, original)
	env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		if cfg.Storage.Bucket == "private-target" {
			return target, nil
		}
		if cfg.Storage.Bucket == original.Storage.Bucket {
			return source, nil
		}
		return nil, errors.New("unknown private destination")
	}
	run := func(extra ...string) int {
		args := append([]string{"migrate", "--format", "catalog-v4", "--bucket", "private-target", "--prefix", "new"}, extra...)
		var out, errs bytes.Buffer
		code := Run(args, bytes.NewReader(nil), &out, &errs, env)
		t.Logf("private migration code=%d output=%s errors=%s", code, out.String(), errs.String())
		return code
	}
	if code := run(); code == 0 || !target.blocked {
		t.Fatal("initial seal interruption missing", code)
	}
	target.blocked = false
	if code := run(); code != 0 {
		t.Fatal("actual initial rerun", code)
	}
	home, err := env.readHome()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	saved := filepath.Join(home, "private-config-save-fault.json")
	target.beforeRollbackSave = func() error {
		if err := os.Rename(path, saved); err != nil {
			return err
		}
		return os.Mkdir(path, 0700)
	}
	if code := run("--rollback"); code == 0 {
		t.Fatal("local configuration failure missing")
	}
	checkpoint, err := catalog.MigrationCheckpoint(t.Context(), target)
	if err != nil || checkpoint.Phase != catalog.MigrationRollback {
		t.Fatal("rollback was not durable before config failure", err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(saved, path); err != nil {
		t.Fatal(err)
	}
	if code := run("--rollback"); code != 0 {
		t.Fatal("completed rollback local config rerun", code)
	}
	rolled, found, err := config.Load(home)
	if err != nil || !found || !rolled.Paused || rolled.Storage != original.Storage {
		t.Fatal("source config restoration", err)
	}
}
