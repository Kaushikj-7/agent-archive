package cli

import (
	"bytes"
	"context"
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
	env := operatorTestEnv(t, cfg)
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
