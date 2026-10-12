package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wangjohn/agent-archive/internal/archive"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type qualifiedCLIStore struct{ *storagetest.MemoryStore }

func (*qualifiedCLIStore) CatalogAtomicQualification() error { return nil }

// Scoped legacy fixtures clone metadata identities. Catalog fixtures give every
// reference its canonical owning-session key before comparing both readers.
func normalizePrivateCatalogSources(t *testing.T, source *storagetest.MemoryStore) {
	t.Helper()
	objects, err := source.List(t.Context(), "sessions/")
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		if !strings.HasSuffix(object.Key, "/metadata.json") {
			continue
		}
		raw, err := source.Get(t.Context(), object.Key)
		if err != nil {
			t.Fatal(err)
		}
		var metadata archive.Metadata
		if err = json.Unmarshal(raw, &metadata); err != nil {
			t.Fatal(err)
		}
		refs := []*archive.SourceReference{&metadata.SourceBundle}
		if metadata.History != nil {
			for i := range metadata.History.Preserved {
				refs = append(refs, &metadata.History.Preserved[i].Source)
			}
		}
		for _, ref := range refs {
			body, err := source.Get(t.Context(), ref.Key)
			if err != nil {
				t.Fatal(err)
			}
			if storage.SHA256Hex(body) != ref.SHA256 {
				t.Fatal("fixture source hash mismatch")
			}
			ref.Key = fmt.Sprintf("sessions/%s/%s/source.%s.jsonl.gz", metadata.Harness.Name, metadata.SessionID, ref.SHA256)
			if err = source.Put(t.Context(), ref.Key, body); err != nil {
				t.Fatal(err)
			}
		}
		raw, err = json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err = source.Put(t.Context(), object.Key, raw); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = reader.RebuildIndex(t.Context(), source, "sessions"); err != nil {
		t.Fatal(err)
	}
}

func privateCatalogFromLegacy(t *testing.T, source *storagetest.MemoryStore) *catalog.Store {
	t.Helper()
	normalizePrivateCatalogSources(t, source)
	raw := &qualifiedCLIStore{storagetest.NewMemoryStore()}
	objects, err := source.List(t.Context(), "sessions/")
	if err != nil {
		t.Fatal(err)
	}
	w, err := catalog.New(raw)
	if err != nil {
		t.Fatal(err)
	}
	var metadata []storage.Object
	for _, obj := range objects {
		bytes, err := source.Get(t.Context(), obj.Key)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(obj.Key, "/metadata.json") {
			metadata = append(metadata, obj)
			continue
		}
		if err = raw.Put(t.Context(), obj.Key, bytes); err != nil {
			t.Fatal(err)
		}
	}
	for _, obj := range metadata {
		bytes, err := source.Get(t.Context(), obj.Key)
		if err != nil {
			t.Fatal(err)
		}
		var summary archive.Metadata
		if err = json.Unmarshal(bytes, &summary); err != nil {
			t.Fatal(err)
		}
		ref, err := w.PutImmutable(t.Context(), catalog.KindMetadata, bytes)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Commit(t.Context(), catalog.CatalogMutation{ID: "private/" + storage.SHA256Hex([]byte(obj.Key)), SessionKey: obj.Key, Next: &catalog.CatalogEntry{Metadata: ref, Summary: summary}}); err != nil {
			t.Fatal(err)
		}
	}
	c := w.Coordinator()
	owner, err := c.Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Activate(t.Context(), owner, "private fixture credential proof"); err != nil {
		t.Fatal(err)
	}
	if err = c.Release(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	remote, err := catalog.Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	return remote
}

func TestRealCLICatalogCommandsEqualPrivateExhaustiveOracle(t *testing.T) {
	a := newScopedArchive(t)
	a.add(t, "alpha001", "Résumé query", a.label, linked(212))
	a.add(t, "alpha002", "Child detail", a.label, subagentOf("alpha001"))
	a.add(t, "beta0001", "Unrelated", "other")
	remote := privateCatalogFromLegacy(t, a.mem)
	measured := storagetest.NewMeasuredStore(remote, 0)
	for _, args := range [][]string{{"list", "--all-projects", "--json", "--limit", "2"}, {"list", "--all-projects", "résumé"}, {"show", "alpha"}, {"show", "#212"}, {"stats", "--json"}} {
		var wantOut, wantErr, gotOut, gotErr bytes.Buffer
		oracleEnv := a.env
		oracleEnv.OpenStore = func(config.Config) (storage.ObjectStore, error) { return a.mem, nil }
		wantCode := Run(args, bytes.NewReader(nil), &wantOut, &wantErr, oracleEnv)
		catalogEnv := a.env
		catalogEnv.OpenStore = func(config.Config) (storage.ObjectStore, error) { return measured, nil }
		gotCode := Run(args, bytes.NewReader(nil), &gotOut, &gotErr, catalogEnv)
		if gotCode != wantCode || gotOut.String() != wantOut.String() || gotErr.String() != wantErr.String() {
			t.Fatalf("%v catalog(%d,%q,%q) oracle(%d,%q,%q)", args, gotCode, gotOut.String(), gotErr.String(), wantCode, wantOut.String(), wantErr.String())
		}
	}
	if measured.Metrics().Lists != 0 {
		t.Fatalf("catalog canonical LIST: %+v", measured.Metrics())
	}
}

func TestMigrateHelpAndNoForceBypass(t *testing.T) {
	var out, stderr bytes.Buffer
	env := Env{Home: func() (string, error) { t.Fatal("help opened user home"); return "", nil }}
	if code := Run([]string{"migrate", "--help"}, nil, &out, &stderr, env); code != 0 {
		t.Fatal(code, stderr.String())
	}
	out.Reset()
	stderr.Reset()
	forceEnv := Env{Home: func() (string, error) { return t.TempDir(), nil }}
	if code := Run([]string{"migrate", "--format", "catalog-v4", "--prefix", "new", "--force"}, nil, &out, &stderr, forceEnv); code != 2 {
		t.Fatal(code, stderr.String())
	}
	if !reflect.DeepEqual(commandHelp["migrate"][:6], "Usage:") {
		t.Fatal("missing help")
	}
}

type migrationCLIStore struct{ *qualifiedCLIStore }

func (*migrationCLIStore) VerifyCatalogCutover(_ context.Context, source, target destination.Config) (catalog.CutoverProof, error) {
	return catalog.CutoverProof{ID: "private-reviewed-cutoff", Source: config.DestinationID(source), Destination: config.DestinationID(target), Protocol: 10}, nil
}

func TestRunMigrateResumesActivatesAndRollsBackPrivateDestination(t *testing.T) {
	source := storagetest.NewMemoryStore()
	target := &migrationCLIStore{&qualifiedCLIStore{storagetest.NewMemoryStore()}}
	original := config.Config{SchemaVersion: 1, Storage: destination.Config{Provider: "s3", Bucket: "private-source", Prefix: "old", Region: "us-east-1", AWSProfile: "private-synthetic"}}
	env := operatorTestEnv(t, original)
	env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
		if cfg.Storage.Bucket == "private-target" {
			return target, nil
		}
		if cfg.Storage.Bucket == original.Storage.Bucket {
			return source, nil
		}
		return nil, errors.New("fixture refuses unknown destination")
	}
	captured := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for i := range 80 {
		id := fmt.Sprintf("private-%03d", i)
		body := []byte("private source " + id)
		sourceKey := "sessions/claude/" + id + "/source." + storage.SHA256Hex(body) + ".jsonl.gz"
		metadata := archive.Metadata{SchemaVersion: 1, SessionID: id, Harness: archive.Harness{Name: "claude"}, CapturedAt: captured, SourceBundle: archive.SourceReference{Key: sourceKey, SHA256: storage.SHA256Hex(body), CompressedBytes: len(body)}}
		raw, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err = source.Put(t.Context(), sourceKey, body); err != nil {
			t.Fatal(err)
		}
		key, err := archive.MetadataObjectKey("claude", id)
		if err != nil {
			t.Fatal(err)
		}
		if err = source.Put(t.Context(), key, raw); err != nil {
			t.Fatal(err)
		}
	}
	destinationConfig := original.Storage
	destinationConfig.Bucket = "private-target"
	destinationConfig.Prefix = "new"
	destinationConfig.ArchiveFormat = destination.FormatCatalogV4
	migration, err := catalog.OpenMigration(t.Context(), source, target, original.Storage, destinationConfig, target)
	if err != nil {
		t.Fatal(err)
	}
	done, err := migration.Step(t.Context())
	if err != nil || done || migration.State.Copied == 0 || migration.State.Copied >= 80 {
		t.Fatal("first bounded checkpoint", done, migration.State.Copied, err)
	}
	if _, err = catalog.OpenSnapshot(t.Context(), target, nil); err == nil {
		t.Fatal("candidate read activation")
	}
	run := func(extra ...string) int {
		args := append([]string{"migrate", "--format", "catalog-v4", "--bucket", "private-target", "--prefix", "new"}, extra...)
		var out, stderr bytes.Buffer
		code := Run(args, bytes.NewReader(nil), &out, &stderr, env)
		if code != 0 {
			t.Log(out.String(), stderr.String())
		}
		return code
	}
	if code := run(); code != 0 {
		t.Fatal("resume/activation", code)
	}
	home, err := env.readHome()
	if err != nil {
		t.Fatal(err)
	}
	activated, found, err := config.Load(home)
	if err != nil || !found || activated.Storage.EffectiveArchiveFormat() != destination.FormatCatalogV4 {
		t.Fatal("config cutover", err)
	}
	snapshot, err := catalog.OpenSnapshot(t.Context(), target, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := snapshot.Query(t.Context(), catalog.Query{Index: catalog.IdentityIndex}, "", 1000)
	if err != nil || len(page.Rows) != 80 {
		t.Fatal("catalog oracle", len(page.Rows), err)
	}
	for _, row := range page.Rows {
		body, err := snapshot.ReadMetadata(t.Context(), row.Entry)
		if err != nil {
			t.Fatal(err)
		}
		originalBody, err := source.Get(t.Context(), row.Key)
		if err != nil || !bytes.Equal(body, originalBody) || !row.Entry.Summary.CapturedAt.Equal(captured) {
			t.Fatal("body/retention changed", err)
		}
	}
	if code := run(); code != 0 {
		t.Fatal("activation retry", code)
	}
	if code := run("--rollback"); code != 0 {
		t.Fatal("rollback", code)
	}
	rolled, found, err := config.Load(home)
	if err != nil || !found || !rolled.Paused || rolled.Storage != original.Storage {
		t.Fatal("read-only rollback config", err)
	}
	if _, err = catalog.OpenSnapshot(t.Context(), target, nil); err == nil {
		t.Fatal("rollback remained activated")
	}
}
