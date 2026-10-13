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
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestRealCLIArchiveBrowsersStartWithoutFullArchiveReads(t *testing.T) {
	for _, command := range []string{"list", "show"} {
		t.Run(command, func(t *testing.T) {
			a := newScopedArchive(t)
			for i := range 105 {
				a.add(t, fmt.Sprintf("browse-%03d", i), fmt.Sprintf("Title %03d", i), a.label)
			}
			remote := scopedPickerCatalog(t, a.mem)
			counts := &catalogListReads{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), paths: map[string]int{}}
			wrapped, err := catalog.Wrap(counts)
			if err != nil {
				t.Fatal(err)
			}
			env := a.env
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return wrapped, nil }
			input := strings.NewReader("q\n")
			var out, stderr bytes.Buffer
			env.IsTerminal = func(stream any) bool { return stream == any(input) || stream == any(&out) }
			if code := Run([]string{command}, input, &out, &stderr, env); code != 0 {
				t.Fatal(code, stderr.String())
			}
			if !strings.Contains(out.String(), "Older sessions") {
				t.Fatal("no remote Older action", out.String())
			}
			if counts.Metrics().Lists != 0 || counts.paths["body"] != 0 || counts.paths["other"] != 0 {
				t.Fatalf("first screen initialized complete archive: %+v %v", counts.Metrics(), counts.paths)
			}
		})
	}
}

func TestBoundedArchiveScopeContinuationsAreIndependentAndSelectionPinned(t *testing.T) {
	a := newScopedArchive(t)
	for i := range 130 {
		project := a.label
		if i >= 60 {
			project = "elsewhere"
		}
		a.add(t, fmt.Sprintf("scope-%03d", i), fmt.Sprintf("Title %03d", i), project)
	}
	remote := scopedPickerCatalog(t, a.mem)
	scope := sessionScope{Label: a.label, Dir: a.dir, ProjectIDs: []string{archive.ProjectID(a.dir)}}
	choices, used, err := boundedArchiveChoices(catalog.WithReadView(t.Context()), a.env, remote, listOptions{filter: reader.Filter{Replays: reader.ReplaysHidden}}, scope, listFormatOptions{Numbered: true}, io.Discard, "show")
	if err != nil || !used {
		t.Fatal(used, err)
	}
	scoped := append([]listRow(nil), choices.shown().rows...)
	if len(scoped) != 50 {
		t.Fatal("scope prefix", len(scoped))
	}
	if _, err = choices.loadOlder.LoadContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(choices.shown().rows) != 61 {
		t.Fatal("scope continuation", len(choices.shown().rows))
	}
	choices.toggle()
	if len(choices.shown().rows) != 50 {
		t.Fatal("all scope inherited another frontier", len(choices.shown().rows))
	}
	if _, err = choices.loadOlder.LoadContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(choices.shown().rows) != 100 {
		t.Fatal("all continuation", len(choices.shown().rows))
	}
	choices.toggle()
	if len(choices.shown().rows) != 61 {
		t.Fatal("scope frontier lost")
	}
	for i, r := range scoped {
		if choices.shown().rows[i].SessionID != r.SessionID || choices.shown().rows[i].Index != r.Index {
			t.Fatal("prefix renumbered")
		}
	}
	row := choices.shown().rows[0]
	key, _ := archive.MetadataObjectKey(row.HarnessKey, row.SessionID)
	if err = remote.DeleteSession(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, err = readPickerSessionView(t.Context(), remote, row); err == nil {
		t.Fatal("pinned selected row silently substituted latest authority")
	}
}
