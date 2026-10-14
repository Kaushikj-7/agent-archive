package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/storage"
)

func TestContentSearchExplicitRefreshAndOfflineQuery(t *testing.T) {
	env, objects, id := publishedFixture(t)
	run := func(args ...string) (int, string, string) {
		var out, errOut bytes.Buffer
		code := Run(append([]string{"search"}, args...), nil, &out, &errOut, env)
		return code, out.String(), errOut.String()
	}
	home, _ := env.readHome()
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	_, dir := contentSearchLocation(home, cfg)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Error("cached query opened storage")
		return nil, errors.New("offline")
	}
	if code, out, errOut := run("visible", "--all-projects"); code != 0 || !strings.Contains(out, "no local content index") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	if _, err = os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("query created index: %v", err)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects, nil }
	if code, out, errOut := run("--refresh", "--session", id, "--harness", "codex"); code != 0 || !strings.Contains(out, "Indexed "+id) {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Error("cached query opened storage")
		return nil, errors.New("offline")
	}
	code, out, errOut := run("visible", "--all-projects")
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "coverage is incomplete") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	// The synthetic fixture's assistant text supplies a body-only reference.
	code, out, errOut = run("visible", "--all-projects")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	var token string
	for line := range strings.SplitSeq(out, "\n") {
		if value, ok := strings.CutPrefix(line, "  ref: "); ok {
			token = value
			break
		}
	}
	if token == "" {
		t.Fatalf("no body reference: %s", out)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects, nil }
	code, out, errOut = run("--read", token, "--max-bytes", "4")
	if code != 0 || out != "\"visi\"\n" || !strings.Contains(errOut, "next offset 4") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(token)
	var ref search.Reference
	if err = json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	ref.Source.Destination = "other"
	raw, _ = json.Marshal(ref)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Error("foreign reference opened storage")
		return nil, errors.New("offline")
	}
	code, _, errOut = run("--read", base64.RawURLEncoding.EncodeToString(raw))
	if code != 1 || !strings.Contains(errOut, "another destination") {
		t.Fatalf("%d %s", code, errOut)
	}
}

func TestContentSearchScopeBeforeLimit(t *testing.T) {
	env, _, _ := publishedFixture(t)
	home, _ := env.readHome()
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	namespace, dir := contentSearchLocation(home, cfg)
	index, err := search.Open(t.Context(), dir, namespace)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range []archive.Metadata{{SessionID: "outside", ProjectID: "outside", ProjectName: "outside", RepoKey: "outside-repo"}, {SessionID: "inside", ProjectID: "inside", ProjectName: "inside", RepoKey: "inside-repo"}} {
		m.Name = "checksum"
		m.Harness = archive.Harness{Name: "codex"}
		m.CapturedAt = env.now().AddDate(0, 0, i)
		source := search.Source{Destination: namespace, SessionID: m.SessionID, ExtractorVersion: search.ExtractorVersion}
		if _, err = index.Replace(t.Context(), m, []search.Projection{{Source: source}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = index.Close(); err != nil {
		t.Fatal(err)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Error("scope query opened storage")
		return nil, errors.New("offline")
	}
	for _, args := range [][]string{{"checksum", "--project", "inside", "--limit", "1"}, {"checksum", "--all-projects", "--limit", "1"}} {
		var out, errOut bytes.Buffer
		code := Run(append([]string{"search"}, args...), nil, &out, &errOut, env)
		if code != 0 || !strings.Contains(out.String(), "inside") {
			t.Fatalf("%v: %d %s %s", args, code, &out, &errOut)
		}
	}
}

func TestContentSearchRejectsInvalidFlagsWithoutIO(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	env.Home = func() (string, error) { t.Error("invalid flags read home"); return "", errors.New("no") }
	for _, args := range [][]string{{}, {"--refresh"}, {"--session", "id"}, {"--refresh", "--session", "id"}, {"query", "--read", "ref"}, {"query", "--project", "x", "--all-projects"}, {"query", "--limit", "0"}, {"query", "--replays", "invalid"}, {"query", "--children", "invalid"}, {"query", "--offset", "1"}, {"query", "--max-bytes", "1"}, {"query", "--since", "invalid"}, {"query", "--until", "invalid"}, {"query", "--since", "2026-02-01", "--until", "2026-01-01"}, {"\"unterminated"}} {
		var out, errOut bytes.Buffer
		if code := Run(append([]string{"search"}, args...), nil, &out, &errOut, env); code != 2 {
			t.Fatalf("%v: %d %s", args, code, &errOut)
		}
	}
}

func TestContentSearchDestinationIsolation(t *testing.T) {
	env, _, _ := publishedFixture(t)
	home, _ := env.readHome()
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	a, dirA := contentSearchLocation(home, cfg)
	cfg.Storage.Prefix += "-other"
	b, dirB := contentSearchLocation(home, cfg)
	if a == b || dirA == dirB {
		t.Fatal("destination contexts collide")
	}
	if filepath.Dir(dirA) != filepath.Join(home, "search") {
		t.Fatal(dirA)
	}
}
