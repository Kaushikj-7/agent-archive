package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type bareSelectionInput struct {
	io.Reader
	arm  func()
	once sync.Once
}

func (r *bareSelectionInput) Read(raw []byte) (int, error) {
	r.once.Do(r.arm)
	return r.Reader.Read(raw)
}

type bareSelectionStore struct {
	*storagetest.MeasuredStore
	armed     atomic.Bool
	heads     atomic.Int64
	parentKey string
	mutated   atomic.Bool
	mutate    func()
}

func (s *bareSelectionStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	raw, err := s.MeasuredStore.GetLimited(ctx, key, limit)
	if err == nil && key == s.parentKey && s.armed.Load() && s.mutated.CompareAndSwap(false, true) {
		s.mutate()
	}
	return raw, err
}

func (s *bareSelectionStore) GetCatalogVersion(ctx context.Context, key string, limit int64) ([]byte, storage.CatalogObjectVersion, error) {
	if key == catalog.HeadKey && s.armed.Load() {
		s.heads.Add(1)
	}
	return s.MeasuredStore.GetCatalogVersion(ctx, key, limit)
}

func TestBareShowJSONPinsParentAndLinkedChildAcrossMutation(t *testing.T) {
	a := newScopedArchive(t)
	a.add(t, "private-child", "Child", a.label, subagentOf(a.id))
	parent := a.base
	parent.LinkedSessions = []archive.LinkedSessionReference{{SessionID: "private-child", Relationship: "subagent", Status: archive.LinkedSessionPublished}}
	parentKey, err := archive.MetadataObjectKey("codex", a.id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.mem.Put(t.Context(), parentKey, raw); err != nil {
		t.Fatal(err)
	}
	remote := privateCatalogFromLegacy(t, a.mem)
	entry, _, err := remote.Writer.Find(t.Context(), parentKey)
	if err != nil || entry == nil {
		t.Fatal(err)
	}
	want, err := readSessionView(catalog.NewReadView(t.Context()), remote, "codex", a.id)
	if err != nil || len(want.LinkedAvailability) != 1 || want.LinkedAvailability[0].State != reader.LinkedStateMetadataAvailable {
		t.Fatal("private child oracle", err)
	}
	childKey, err := archive.MetadataObjectKey("codex", "private-child")
	if err != nil {
		t.Fatal(err)
	}
	var mutationErr error
	hook := &bareSelectionStore{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), parentKey: entry.Metadata.Key, mutate: func() { mutationErr = remote.DeleteSession(t.Context(), childKey) }}
	wrapped, err := catalog.Wrap(hook)
	if err != nil {
		t.Fatal(err)
	}
	input := &bareSelectionInput{Reader: strings.NewReader("1\n"), arm: func() { hook.armed.Store(true) }}
	var output, stderr bytes.Buffer
	env := a.env
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return wrapped, nil }
	env.IsTerminal = func(stream any) bool { return stream == any(input) || stream == any(&output) }
	if code := Run([]string{"show", "--json", "--harness", "codex"}, input, &output, &stderr, env); code != 0 {
		t.Fatal(code, stderr.String())
	}
	start := strings.Index(output.String(), "{")
	if start < 0 {
		t.Fatal("selected JSON missing")
	}
	var got sessionView
	if err = json.Unmarshal(output.Bytes()[start:], &got); err != nil {
		t.Fatal(err)
	}
	if mutationErr != nil || len(got.LinkedAvailability) != 1 || got.LinkedAvailability[0] != want.LinkedAvailability[0] || hook.heads.Load() != 1 {
		t.Fatal("selected parent/child switched roots", mutationErr, got.LinkedAvailability, hook.heads.Load())
	}
	fresh, err := readSessionView(catalog.NewReadView(t.Context()), remote, "codex", a.id)
	if err != nil || fresh.LinkedAvailability[0].State != reader.LinkedStateUnavailableOrExpired {
		t.Fatal("mutation did not establish a different current-root oracle", err)
	}
}
