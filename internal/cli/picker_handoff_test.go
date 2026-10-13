package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/destination"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func scopedPickerCatalog(t *testing.T, mem *storagetest.MemoryStore) *catalog.Store {
	t.Helper()
	legacy := privateCatalogFromLegacy(t, mem)
	raw := &scopedMigrationCLIStore{legacy.ObjectStore.(*qualifiedCLIStore)}
	dest := destination.Config{Provider: "s3", Bucket: "private-picker-test", ArchiveFormat: destination.FormatCatalogV4}
	upgrade, err := catalog.OpenPickerUpgrade(t.Context(), raw, dest, raw, "")
	if err != nil {
		t.Fatal(err)
	}
	for {
		done, e := upgrade.Step(t.Context(), 64)
		if e != nil {
			t.Fatal(e)
		}
		if done {
			break
		}
	}
	if err = upgrade.Activate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = upgrade.FinishActivation(t.Context()); err != nil {
		t.Fatal(err)
	}
	remote, err := catalog.Wrap(raw)
	if err != nil {
		t.Fatal(err)
	}
	return remote
}

func TestRealCLIBoundedHandoffStartupAndOlderReadNoBodies(t *testing.T) {
	a := newScopedArchive(t)
	for i := 0; i < 105; i++ {
		a.add(t, fmt.Sprintf("picker-%03d", i), fmt.Sprintf("Title %03d", i), a.label, func(m *archive.Metadata) { m.Counts.Turns = nil })
	}
	remote := scopedPickerCatalog(t, a.mem)
	counts := &catalogListReads{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), paths: map[string]int{}}
	wrapped, err := catalog.Wrap(counts)
	if err != nil {
		t.Fatal(err)
	}
	env := a.env
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return wrapped, nil }
	out, stderr, code := runPicker(t, env, "q\n", "--all-projects", "--source", "archive")
	if code != 0 {
		t.Fatal(code, stderr)
	}
	if !strings.Contains(out, "Older sessions") {
		t.Fatal("no Older action", out)
	}
	if counts.Metrics().Lists != 0 || counts.paths["body"] != 0 || counts.paths["other"] != 0 {
		t.Fatalf("startup enumerated archive/bodies: %+v paths=%v", counts.Metrics(), counts.paths)
	}
	home, err := env.readHome()
	if err != nil {
		t.Fatal(err)
	}
	choices, model, _, used, err := boundedHandoffChoices(catalog.WithReadView(t.Context()), env, wrapped, home, handoffOptions{source: "archive"}, sessionScope{All: true}, nil, listFormatOptions{Now: env.now(), Numbered: true}, io.Discard)
	if err != nil || !used {
		t.Fatal(used, err)
	}
	if len(choices.shown().rows) != 50 {
		t.Fatal(len(choices.shown().rows))
	}
	if _, err = choices.loadOlder.Load(); err != nil {
		t.Fatal(err)
	}
	if len(choices.shown().rows) != 100 {
		t.Fatal(len(choices.shown().rows))
	}
	if model.searchError() != nil || counts.paths["body"] != 0 || counts.Metrics().Lists != 0 {
		t.Fatal("Older read complete archive")
	}
}

func TestRealCLIBoundedHandoffSelectionRejectsHeadRewriteBeforeSources(t *testing.T) {
	f := newPickerFixture(t)
	remote := scopedPickerCatalog(t, f.mem)
	env := f.env
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	input := &bareSelectionInput{Reader: strings.NewReader("1\n"), arm: func() {
		key, _ := archive.MetadataObjectKey("codex", f.archiveOnly)
		if err := remote.DeleteSession(context.Background(), key); err != nil {
			t.Error(err)
		}
	}}
	var out, stderr bytes.Buffer
	env.IsTerminal = func(stream any) bool { return stream == any(input) || stream == any(&out) }
	code := Run([]string{"handoff", "--all-projects", "--source", "archive"}, input, &out, &stderr, env)
	if code == 0 {
		t.Fatalf("selected a stale rewrite: %s %s", out.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "refresh") {
		t.Fatalf("missing refresh diagnostic: %s", stderr.String())
	}
}
