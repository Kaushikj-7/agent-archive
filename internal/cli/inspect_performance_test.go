package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// recordingStore counts the List and Get calls `list` and `show` make.
type recordingStore struct {
	storage.ObjectStore
	mu    sync.Mutex
	lists []string
	gets  []string
}

func (s *recordingStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	s.mu.Lock()
	s.lists = append(s.lists, prefix)
	s.mu.Unlock()
	return s.ObjectStore.List(ctx, prefix)
}

func (s *recordingStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	s.mu.Lock()
	s.gets = append(s.gets, key)
	s.mu.Unlock()
	bounded, ok := s.ObjectStore.(storage.LimitedGetter)
	if !ok {
		return nil, errors.New("underlying store has no bounded read capability")
	}
	return bounded.GetLimited(ctx, key, limit)
}

func (s *recordingStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	s.gets = append(s.gets, key)
	s.mu.Unlock()
	return s.ObjectStore.Get(ctx, key)
}

func (s *recordingStore) take() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lists, gets := s.lists, s.gets
	s.lists, s.gets = nil, nil
	return lists, gets
}

func TestShowReadsDirectlyAndListReusesTheMetadataCache(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	recorder := &recordingStore{ObjectStore: mem}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return recorder, nil }
	run := func(args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("%v: code=%d err=%s", args, code, errOut.String())
		}
		return out.String()
	}

	// A known harness derives the key: one read, no listing.
	run("show", id, "--harness", "codex")
	lists, gets := recorder.take()
	if len(lists) != 0 || len(gets) != 1 {
		t.Fatalf("show --harness: lists=%q gets=%q", lists, gets)
	}

	// Without a harness, the known harness keys are read directly; the
	// archive is not listed.
	if out := run("show", id); !strings.Contains(out, id) {
		t.Fatalf("show output = %s", out)
	}
	if lists, _ := recorder.take(); len(lists) != 0 {
		t.Fatalf("show without --harness listed %q", lists)
	}

	// list: the first run downloads the sidecar, the second reuses it, and
	// --no-cache downloads it again. No run downloads a source bundle.
	for _, step := range []struct {
		args  []string
		reads int
	}{
		{[]string{"list"}, 1},
		{[]string{"list"}, 0},
		{[]string{"list", "--no-cache"}, 1},
		{[]string{"list", "--harness", "codex"}, 0},
	} {
		if out := run(step.args...); !strings.Contains(out, id[:min(8, len(id))]) {
			t.Fatalf("%v output = %s", step.args, out)
		}
		lists, gets := recorder.take()
		if len(gets) != step.reads {
			t.Fatalf("%v: gets=%q, want %d", step.args, gets, step.reads)
		}
		for _, key := range gets {
			if !strings.HasSuffix(key, "/metadata.json") {
				t.Fatalf("%v downloaded %q", step.args, key)
			}
		}
		if len(lists) != 1 {
			t.Fatalf("%v: lists=%q", step.args, lists)
		}
	}
}
