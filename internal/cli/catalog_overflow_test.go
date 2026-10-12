package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func overflowCLIArchive(t *testing.T) (Env, Env, string, *catalogListReads) {
	t.Helper()
	env, mem, id := publishedFixture(t)
	key := "sessions/codex/" + id + "/metadata.json"
	raw, err := mem.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	var metadata archive.Metadata
	if err = json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.Title = "Unique overflow authority"
	metadata.PullRequests = []archive.PullRequestLink{{Number: 88123}}
	for i := range 2000 {
		metadata.LinkedSessions = append(metadata.LinkedSessions, archive.LinkedSessionReference{SessionID: fmt.Sprintf("linked-%04d", i), Relationship: "subagent", Status: archive.LinkedSessionPublished, ObservedAt: metadata.CapturedAt})
	}
	raw, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err = mem.Put(t.Context(), key, raw); err != nil {
		t.Fatal(err)
	}
	remote := privateCatalogFromLegacy(t, mem)
	counts := &catalogListReads{MeasuredStore: storagetest.NewMeasuredStore(remote.ObjectStore, 0), paths: map[string]int{}}
	measured, err := catalog.Wrap(counts)
	if err != nil {
		t.Fatal(err)
	}
	oracle := env
	oracle.OpenStore = func(config.Config) (storage.ObjectStore, error) { return mem, nil }
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return measured, nil }
	return env, oracle, id, counts
}

func TestRealCLIOverflowShowKeepsFullBodyAndSourceAuthority(t *testing.T) {
	env, oracle, id, counts := overflowCLIArchive(t)
	for _, flags := range [][]string{{"--json"}, {"--transcript", "--no-pager"}} {
		var want, wantErr bytes.Buffer
		if code := Run(append([]string{"show", id}, flags...), nil, &want, &wantErr, oracle); code != 0 {
			t.Fatal(code, wantErr.String())
		}
		for _, query := range []string{"overflow", id[:8], "#88123"} {
			counts.Reset()
			counts.paths = map[string]int{}
			var got, errs bytes.Buffer
			if code := Run(append([]string{"show", query}, flags...), nil, &got, &errs, env); code != 0 || got.String() != want.String() || errs.String() != wantErr.String() {
				t.Fatalf("full overflow show %s %v code=%d error=%s", query, flags, code, errs.String())
			}
			if counts.paths["body"] != 1 || counts.Metrics().Lists != 0 {
				t.Fatal("selected overflow body repeated", counts.paths, counts.Metrics())
			}
		}
	}
}

func TestRealCLIOverflowUncachedListMatchesFullAuthority(t *testing.T) {
	env, oracle, _, counts := overflowCLIArchive(t)
	for _, query := range []string{"overflow", "#88123"} {
		counts.Reset()
		counts.paths = map[string]int{}
		args := []string{"list", "--all-projects", "--json", "--no-cache", query}
		var got, errs, want, wantErr bytes.Buffer
		if code := Run(args, nil, &got, &errs, env); code != 0 {
			t.Fatal("uncached list", code, errs.String())
		}
		code := Run(args, nil, &want, &wantErr, oracle)
		t.Logf("uncached full JSON equal=%t actual SHA=%s oracle SHA=%s", got.String() == want.String(), storage.SHA256Hex(got.Bytes()), storage.SHA256Hex(want.Bytes()))
		if code != 0 || got.String() != want.String() {
			t.Fatal("uncached overflow list authority differs", code, wantErr.String())
		}
		if errs.Len() != 0 || wantErr.String() != "agent-archive: list: query requires an exhaustive metadata scan.\n" {
			t.Fatal("unexpected compatibility warning", errs.String(), wantErr.String())
		}
		t.Logf("uncached full-summary list body GET=%d canonical LIST=%d", counts.paths["body"], counts.Metrics().Lists)
		if counts.paths["body"] != 1 || counts.Metrics().Lists != 0 {
			t.Fatal("uncached full-summary list cost", counts.paths, counts.Metrics())
		}
	}
}
