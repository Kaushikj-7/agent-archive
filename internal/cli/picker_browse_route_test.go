package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type browseRouteReads struct {
	*catalogListReads
	keys map[string]int
}

func (s *browseRouteReads) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	s.mu.Lock()
	s.keys[key]++
	s.mu.Unlock()
	return s.catalogListReads.GetLimited(ctx, key, limit)
}

func (s *browseRouteReads) snapshot() browseRoutePhase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return browseRoutePhase{paths: maps.Clone(s.paths), keys: maps.Clone(s.keys)}
}

type browseRoutePhase struct {
	paths map[string]int
	keys  map[string]int
}

type browseRouteOutput struct {
	bytes.Buffer
	reads      *browseRouteReads
	firstTitle string
	olderTitle string
	first      *browseRoutePhase
	older      *browseRoutePhase
}

func (w *browseRouteOutput) Write(p []byte) (int, error) {
	if w.first == nil && bytes.Contains(p, []byte(w.firstTitle)) {
		phase := w.reads.snapshot()
		w.first = &phase
	} else if w.first != nil && w.older == nil && bytes.Contains(p, []byte(w.olderTitle)) {
		phase := w.reads.snapshot()
		w.older = &phase
	}
	return w.Buffer.Write(p)
}

func TestBareListUsesOneBoundedPageAndOlderReadsRemote(t *testing.T) {
	t.Parallel()
	a := newScopedArchive(t)
	for i := range 130 {
		a.addMetadata(t, fmt.Sprintf("route-%03d", i), fmt.Sprintf("RouteTitle%03d", i), a.label, func(m *archive.Metadata) {
			m.Name = ""
			m.EndedAt = nil
			m.CapturedAt = a.base.CapturedAt.Add(-time.Duration(i+1) * time.Minute)
		})
	}
	remote := scopedPickerCatalog(t, a.mem)
	head, _, err := remote.Writer.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	oracleReads := &catalogListReads{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), paths: map[string]int{}}
	oracleStore, err := catalog.Wrap(oracleReads)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := scopeFor(a.env, "", false)
	if err != nil {
		t.Fatal(err)
	}
	query, err := pickerScopeQuery(scope, "", defaultListLimit)
	if err != nil {
		t.Fatal(err)
	}
	query.Membership = catalog.PickerBrowse
	snapshot, err := catalog.OpenSnapshot(t.Context(), oracleStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := snapshot.PickerPage(t.Context(), query, "")
	if err != nil || len(first.Rows) != defaultListLimit || !first.More {
		t.Fatal("first typed page", err, len(first.Rows), first.More)
	}
	nodeCeiling := oracleReads.paths["tree"]
	older, err := snapshot.PickerPage(t.Context(), query, first.Next)
	if err != nil || len(older.Rows) != defaultListLimit {
		t.Fatal("second typed page", err, len(older.Rows))
	}
	reads := &browseRouteReads{catalogListReads: &catalogListReads{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), paths: map[string]int{}}, keys: map[string]int{}}
	wrapped, err := catalog.Wrap(reads)
	if err != nil {
		t.Fatal(err)
	}
	out := &browseRouteOutput{reads: reads, firstTitle: first.Rows[defaultListLimit-1].Row.Title, olderTitle: older.Rows[defaultListLimit-1].Row.Title}
	input := strings.NewReader("")
	keys := newFakeKeys("oq")
	env := a.env
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return wrapped, nil }
	env.IsTerminal = func(stream any) bool { return stream == any(input) || stream == any(out) }
	env.TerminalSize = func(io.Writer) (int, int, bool) { return 160, 160, true }
	env.openKeys = func(io.Reader) (keyTerminal, bool) { return keys, true }
	var stderr bytes.Buffer
	if code := Run([]string{"list"}, input, out, &stderr, env); code != 0 {
		t.Fatal(code, stderr.String())
	}
	if out.first == nil || out.older == nil {
		t.Fatal("both usable frames were not rendered", out.String())
	}
	if out.first.paths["tree"] > nodeCeiling || out.first.paths["body"] != 0 || reads.Metrics().Lists != 0 {
		t.Fatalf("bare list scanned beyond one typed page: nodes=%d ceiling=%d paths=%v", out.first.paths["tree"], nodeCeiling, out.first.paths)
	}
	for _, root := range []catalog.ObjectRef{head.Identity, head.Capture, head.Activity} {
		if out.first.keys[root.Key] != 0 {
			t.Fatal("first frame initialized a complete non-Project universe", root.Key, out.first.keys[root.Key])
		}
	}
	t.Logf("cold node ceiling=%d first=%v older=%v", nodeCeiling, out.first.paths, out.older.paths)
	if out.older.paths["head"] <= out.first.paths["head"] || out.older.paths["coordinator"] <= out.first.paths["coordinator"] || out.older.paths["tree"] <= out.first.paths["tree"] {
		t.Fatalf("Older expanded an eager in-memory universe: first=%v older=%v", out.first.paths, out.older.paths)
	}
}
