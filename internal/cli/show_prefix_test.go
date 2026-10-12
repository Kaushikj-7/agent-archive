package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func TestShowShortPrefixDiscoversOnceAndKeepsTitleAmbiguity(t *testing.T) {
	env, mem := statsEnv(t)
	for i, s := range []syntheticSession{
		{id: "abcdef01000000000000000000000001", harness: "codex", project: "p", captured: statsNow},
		{id: "12345678000000000000000000000002", harness: "codex", project: "p", captured: statsNow},
	} {
		m := s.build()
		if i == 1 {
			m.Title = "Compare abcdef01 results"
		}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		key, err := archive.MetadataObjectKey(m.Harness.Name, m.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if err := mem.Put(context.Background(), key, data); err != nil {
			t.Fatal(err)
		}
	}
	store := storagetest.NewMeasuredStore(mem, 0)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return store, nil }
	var out, errOut bytes.Buffer
	if code := Run([]string{"show", "abcdef01", "--json"}, nil, &out, &errOut, env); code != 1 {
		t.Fatalf("ambiguous query exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "matches 2") || !strings.Contains(errOut.String(), "Compare abcdef01 results") {
		t.Fatalf("missing prefix/title ambiguity: %s", errOut.String())
	}
	metrics := store.Metrics()
	if metrics.Lists != 1 || metrics.Gets != 2 {
		t.Fatalf("query performed redundant discovery or exact probes: %+v", metrics)
	}
}
