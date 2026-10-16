package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type operatorCatalogFixture struct {
	*storagetest.MemoryStore
	barrier catalog.Barrier
	writes  int
}

func (*operatorCatalogFixture) CatalogAtomicQualification() error { return nil }

func (s *operatorCatalogFixture) CatalogBarrier(context.Context) (catalog.Barrier, error) {
	return s.barrier, nil
}

func (s *operatorCatalogFixture) PutConditional(ctx context.Context, key string, raw []byte, condition storage.PutCondition) (string, error) {
	s.writes++
	return s.MemoryStore.PutConditional(ctx, key, raw, condition)
}

type operatorBarrier struct{ refs []catalog.ObjectRef }

func (b operatorBarrier) Hold(context.Context) ([]catalog.ObjectRef, func(), error) {
	return b.refs, func() {}, nil
}

func TestConfiguredCatalogRoutesPublicationThroughAuthority(t *testing.T) {
	raw := &operatorCatalogFixture{MemoryStore: storagetest.NewMemoryStore()}
	legacy, err := configuredArchiveStore(destination.FormatLegacy, raw)
	if err != nil || legacy != raw {
		t.Fatal("legacy provider changed", err)
	}
	if _, err = configuredArchiveStore(destination.FormatCatalogV4, storagetest.NewMemoryStore()); !errors.Is(err, storage.ErrAtomicCatalogUnqualified) {
		t.Fatal("unqualified provider routed", err)
	}
	remote, err := configuredArchiveStore(destination.FormatCatalogV4, raw)
	if err != nil {
		t.Fatal(err)
	}
	authority, ok := remote.(storage.CatalogPublisher)
	if !ok || !authority.CatalogMetadataAuthority() {
		t.Fatal("catalog routing lost publication authority")
	}
	source := []byte("synthetic routing fixture")
	key := "sessions/claude/routing/source." + storage.SHA256Hex(source) + ".jsonl.gz"
	metadata := archive.Metadata{SchemaVersion: 1, SessionID: "routing", Harness: archive.Harness{Name: "claude"}, CapturedAt: time.Now(), SourceBundle: archive.SourceReference{Key: key, SHA256: storage.SHA256Hex(source), CompressedBytes: len(source)}}
	body, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	metadataKey := "sessions/claude/routing/metadata.json"
	id, expected, err := authority.FreezeCatalogMutation(t.Context(), metadataKey)
	if err != nil {
		t.Fatal(err)
	}
	if err = storage.PutSourceThenMetadataIndexed(t.Context(), authority.Publication(id, metadataKey, expected), key, metadataKey, source, body, storage.RetryPolicy{MaxAttempts: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = raw.Get(t.Context(), metadataKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("routing wrote canonical metadata")
	}
	if got, err := remote.Get(t.Context(), metadataKey); err != nil || !bytes.Equal(got, body) {
		t.Fatal("catalog authority readback", err)
	}
}

func TestConfiguredCatalogRefusesBeforeCredentialStore(t *testing.T) {
	for _, provider := range []string{"s3", "r2"} {
		cfg := config.Config{Storage: credentials.Config{Provider: provider, ArchiveFormat: destination.FormatCatalogV4}}
		_, err := openConfiguredStoreContext(t.Context(), cfg, func() (credentials.CredentialStore, error) {
			t.Fatal("unqualified catalog opened credentials")
			return nil, nil
		})
		if !errors.Is(err, storage.ErrAtomicCatalogUnqualified) {
			t.Fatal(err)
		}
	}
}

func operatorTestEnv(t *testing.T, cfg config.Config) Env {
	t.Helper()
	home := t.TempDir()
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	return testEnv(t, home, time.Now())
}

func TestCatalogProbeRequiresDisjointExplicitTarget(t *testing.T) {
	for _, active := range []string{"", ".catalog-qualification/", ".catalog-qualification/test/child"} {
		cfg := config.Config{SchemaVersion: 1, Storage: credentials.Config{Bucket: "archive", Prefix: active}}
		env := operatorTestEnv(t, cfg)
		env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
			t.Fatal("overlapping probe opened remote")
			return nil, nil
		}
		for _, args := range [][]string{
			{"probe"},
			{"probe", "--bucket", "archive", "--prefix", ".catalog-qualification/test/", "--isolated"},
			{"probe", "--bucket", "different", "--prefix", ".catalog-qualification/../", "--isolated"},
			{"probe", "--bucket", "different", "--prefix", "production/", "--isolated"},
			{"probe", "--bucket", "different", "--prefix", ".catalog-qualification/test/"},
		} {
			var out, errOut bytes.Buffer
			if code := runCatalogOperator(args, &out, &errOut, env); code == 0 {
				t.Fatal("unsafe probe admitted", args)
			}
		}
	}
}

func TestCatalogProbeReportsObservationWithoutActivation(t *testing.T) {
	cfg := config.Config{SchemaVersion: 1, Storage: credentials.Config{Bucket: "archive", ArchiveFormat: destination.FormatCatalogV4}}
	env := operatorTestEnv(t, cfg)
	raw := storagetest.NewMemoryStore()
	env.OpenStore = func(target config.Config) (storage.ObjectStore, error) {
		if target.Storage.Bucket != "isolated" || target.Storage.Prefix != ".catalog-qualification/test/" || target.Storage.EffectiveArchiveFormat() != destination.FormatLegacy {
			t.Fatal("probe did not select explicit raw isolated target")
		}
		return raw, nil
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"_catalog", "probe", "--bucket", "isolated", "--prefix", ".catalog-qualification/test/", "--isolated"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatal(code, errOut.String())
	}
	if !strings.Contains(out.String(), "qualification remain unproven") || !errors.Is(raw.CatalogAtomicQualification(), storage.ErrAtomicCatalogUnqualified) {
		t.Fatal("probe claimed qualification", out.String())
	}
	objects, err := raw.List(t.Context(), "")
	if err != nil || len(objects) != 0 {
		t.Fatal("probe fixture was not cleaned up", err)
	}
	home, err := env.readHome()
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := config.Load(home)
	if err != nil || after.Storage != cfg.Storage {
		t.Fatal("probe changed configured destination", err)
	}
}

func TestCatalogMaintenanceRequiresCoordinatorAndExactRecoveryOwner(t *testing.T) {
	cfg := config.Config{SchemaVersion: 1, Storage: credentials.Config{ArchiveFormat: destination.FormatCatalogV4}}
	env := operatorTestEnv(t, cfg)
	raw := &operatorCatalogFixture{MemoryStore: storagetest.NewMemoryStore()}
	remote, err := catalog.Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	run := func(args ...string) int {
		var out, errOut bytes.Buffer
		return runCatalogOperator(args, &out, &errOut, env)
	}
	if run("collect") == 0 || run("recover", "--owner", "unobserved") == 0 || raw.writes != 0 {
		t.Fatal("missing barrier permitted maintenance writes")
	}
	// A complete synthetic barrier allows the real durable lease transitions.
	raw.barrier = operatorBarrier{}
	if run("collect") != 0 {
		t.Fatal("coordinated collection refused")
	}
	raw.barrier = operatorBarrier{refs: []catalog.ObjectRef{{Key: "missing", SHA256: strings.Repeat("0", 64)}}}
	if run("collect") == 0 {
		t.Fatal("incomplete inventory released lease")
	}
	h, _, err := remote.Writer.Head(t.Context())
	if err != nil || h.GCLease == "" {
		t.Fatal("failed inventory did not preserve lease", err)
	}
	raw.barrier = operatorBarrier{}
	if run("recover", "--owner", "wrong") == 0 {
		t.Fatal("wrong recovery owner admitted")
	}
	if run("recover", "--owner", h.GCLease) == 0 {
		t.Fatal("exact owner discarded incomplete protected inventory")
	}
	after, _, err := remote.Writer.Head(t.Context())
	if err != nil || after.GCLease != h.GCLease || after.Epoch != h.Epoch {
		t.Fatal("recovery released incomplete inventory lease", err)
	}
}
