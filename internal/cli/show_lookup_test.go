package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestShowReusesSelectedMetadataForExactShortAndTranscript(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	recorder := &recordingStore{ObjectStore: mem}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return recorder, nil }
	key := "sessions/codex/" + id + "/metadata.json"
	home, err := env.Home()
	if err != nil {
		t.Fatal(err)
	}
	var expected string
	for _, args := range [][]string{
		{"show", id, "--json", "--harness", "codex"},
		{"show", id, "--json"},
		{"show", id[:8], "--json"},
		{"show", id, "--transcript", "--no-pager"},
		{"show", id[:8], "--transcript", "--no-pager"},
	} {
		if err := os.RemoveAll(filepath.Join(home, "cache")); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("%v: code=%d error=%s", args, code, errOut.String())
		}
		_, gets := recorder.take()
		parentReads := 0
		for _, got := range gets {
			if got == key {
				parentReads++
			}
		}
		if parentReads != 1 {
			t.Fatalf("%v selected parent reads=%d gets=%v", args, parentReads, gets)
		}
		if strings.Contains(strings.Join(args, " "), "--json") {
			if expected == "" {
				expected = out.String()
			} else if out.String() != expected {
				t.Fatalf("exact/short lookup output changed: %v", args)
			}
		}
	}
}

func TestShowAmbiguousHarnessesWinOverMalformedMetadata(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	recorder := &recordingStore{ObjectStore: mem}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return recorder, nil }
	if err := mem.Put(context.Background(), "sessions/claude/"+id+"/metadata.json", []byte("malformed")); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"show", id, "--json"}, nil, &out, &errOut, env); code != 1 || !strings.Contains(errOut.String(), "exists under more than one harness (claude, codex); pass --harness") || out.Len() != 0 {
		t.Fatalf("ambiguous show: code=%d out=%s err=%s", code, out.String(), errOut.String())
	}
	lists, gets := recorder.take()
	if len(lists) != 0 || len(gets) != len(agentmeta.Names(agentmeta.Builtins())) {
		t.Fatalf("ambiguity reread/listed: %v %v", lists, gets)
	}
}

func TestWarmShowProjectionLoadsAuthoritativeJSONAndTranscript(t *testing.T) {
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
	metadata.Title = "Unique provenance ÉCOLE"
	metadata.PullRequests = []archive.PullRequestLink{{Number: 9123}}
	raw, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err = mem.Put(t.Context(), key, raw); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingStore{ObjectStore: mem}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return recorder, nil }
	var warm, stderr bytes.Buffer
	if code := Run([]string{"list", "--all-projects", "--limit", "0"}, nil, &warm, &stderr, env); code != 0 {
		t.Fatal(code, stderr.String())
	}
	recorder.take()
	for _, flags := range [][]string{{"--json"}, {"--transcript", "--no-pager"}} {
		var want, wantErr bytes.Buffer
		if code := Run(append([]string{"show", id}, flags...), nil, &want, &wantErr, env); code != 0 {
			t.Fatal(code, wantErr.String())
		}
		recorder.take()
		for _, query := range []string{id[:8], "école", "#9123"} {
			var got, gotErr bytes.Buffer
			if code := Run(append([]string{"show", query}, flags...), nil, &got, &gotErr, env); code != 0 || got.String() != want.String() || gotErr.String() != wantErr.String() {
				t.Fatalf("%s %v: code=%d got=%s error=%s want=%s", query, flags, code, got.String(), gotErr.String(), want.String())
			}
			_, gets := recorder.take()
			parentReads := 0
			for _, gotKey := range gets {
				if gotKey == key {
					parentReads++
				}
			}
			if parentReads != 1 {
				t.Fatalf("projection selected body reads=%d gets=%v", parentReads, gets)
			}
			if flags[0] == "--json" {
				var authoritative archive.Metadata
				if err = json.Unmarshal(got.Bytes(), &authoritative); err != nil {
					t.Fatal(err)
				}
				if authoritative.SourceBundle != metadata.SourceBundle || !reflect.DeepEqual(authoritative.Counts, metadata.Counts) {
					t.Fatal("projection lost body-only source/count fields")
				}
			}
		}
	}
}

func TestColdAndWarmShowCandidatesReadSelectedBodyOnce(t *testing.T) {
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
	metadata.Title = "Unique body provenance"
	metadata.PullRequests = []archive.PullRequestLink{{Number: 9133}}
	raw, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err = mem.Put(t.Context(), key, raw); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingStore{ObjectStore: mem}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return recorder, nil }
	home, err := env.Home()
	if err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{{"--json"}, {"--transcript", "--no-pager"}} {
		var want, wantErr bytes.Buffer
		if code := Run(append([]string{"show", id}, flags...), nil, &want, &wantErr, env); code != 0 {
			t.Fatal(code, wantErr.String())
		}
		for _, query := range []string{id[:8], "unique body provenance", "#9133"} {
			if err = os.RemoveAll(filepath.Join(home, "cache")); err != nil {
				t.Fatal(err)
			}
			for _, temperature := range []string{"cold", "warm"} {
				recorder.take()
				var got, stderr bytes.Buffer
				if code := Run(append([]string{"show", query}, flags...), nil, &got, &stderr, env); code != 0 || got.String() != want.String() || stderr.String() != wantErr.String() {
					t.Fatalf("%s %s %v: code=%d got=%s error=%s", temperature, query, flags, code, got.String(), stderr.String())
				}
				_, gets := recorder.take()
				selectedReads := 0
				for _, gotKey := range gets {
					if gotKey == key {
						selectedReads++
					}
				}
				if selectedReads != 1 {
					t.Fatalf("%s %s %v selected=%d gets=%v", temperature, query, flags, selectedReads, gets)
				}
			}
		}
	}
}
