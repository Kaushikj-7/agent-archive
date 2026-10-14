package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/reader"
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
	if code, out, errOut := run("--refresh", "--session", id, "--harness", "codex"); code != 0 || !strings.Contains(out, "Observed "+id) {
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
	env, objects, originalID := publishedFixture(t)
	_, seed, err := reader.RefreshAndLoad(t.Context(), objects, "sessions/codex/"+originalID+"/metadata.json", reader.Limits{})
	must(t, err)
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
		m.CapturedAt = env.now().AddDate(0, 0, i)
		bundle := seed
		bundle.ArchiveSessionID = m.SessionID
		bundle.ProjectID = m.ProjectID
		bundle.Capture.CapturedAt = m.CapturedAt
		packed, e := archive.BuildCompressedSource(bundle)
		must(t, e)
		key, e := archive.SourceObjectKey(bundle, packed.SHA256)
		must(t, e)
		must(t, objects.Put(t.Context(), key, packed.Bytes))
		parser, ok := parsersFor(env).LookupParser("codex")
		if !ok {
			t.Fatal("missing parser")
		}
		analysis, parseErr := agentapi.Analyze(t.Context(), parser, bundle)
		verified, e := archive.BuildMetadataWithAnalysis(bundle, analysis, parseErr, "synthetic", m.CapturedAt, m.CapturedAt, archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}, archive.ParserInfo{Version: parser.Version()})
		must(t, e)
		verified.Name = m.Name
		verified.ProjectName = m.ProjectName
		verified.RepoKey = m.RepoKey
		must(t, index.Observe(t.Context(), verified, parsersFor(env), env.now(), 0))
		progress, e := index.Run(t.Context(), objects, parsersFor(env), env.now(), search.Budget{})
		if e != nil || progress.Committed != 1 {
			t.Fatalf("%+v %v", progress, e)
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
	for _, args := range [][]string{{}, {"--session", "id"}, {"--refresh", "--session", "id"}, {"query", "--read", "ref"}, {"query", "--project", "x", "--all-projects"}, {"query", "--limit", "0"}, {"query", "--replays", "invalid"}, {"query", "--children", "invalid"}, {"query", "--offset", "1"}, {"query", "--max-bytes", "1"}, {"query", "--since", "invalid"}, {"query", "--until", "invalid"}, {"query", "--since", "2026-02-01", "--until", "2026-01-01"}, {"\"unterminated"}} {
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
	// Namespace assembly must retain the exact persisted encoding and isolate
	// installation and activation changes as well as storage destination changes.
	want := cfg.DestinationID() + "/" + cfg.MachineID + "/" + cfg.DestinationSince.UTC().Format(time.RFC3339Nano) + "/filter-" + archive.FilterVersion
	if a != want {
		t.Fatalf("namespace encoding changed: %q, want %q", a, want)
	}
	for _, change := range []func(*config.Config){
		func(c *config.Config) { c.MachineID += "-other" },
		func(c *config.Config) { c.DestinationSince = c.DestinationSince.Add(time.Second) },
	} {
		changed := cfg
		change(&changed)
		namespace, dir := contentSearchLocation(home, changed)
		if namespace == a || dir == dirA {
			t.Fatal("installation or activation contexts collide")
		}
	}
	cfg.Storage.Prefix += "-other"
	b, dirB := contentSearchLocation(home, cfg)
	if a == b || dirA == dirB {
		t.Fatal("destination contexts collide")
	}
	if filepath.Dir(dirA) != filepath.Join(home, "search") {
		t.Fatal(dirA)
	}
}

type searchRefreshStore struct {
	storage.ObjectStore
	before    func()
	inspect   func(context.Context)
	reads     int
	interrupt chan<- os.Signal
}

func (s *searchRefreshStore) Get(ctx context.Context, key string) ([]byte, error) {
	s.reads++
	if s.inspect != nil {
		s.inspect(ctx)
	}
	return s.ObjectStore.Get(ctx, key)
}

func (s *searchRefreshStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	s.reads++
	if s.inspect != nil {
		s.inspect(ctx)
	}
	if s.before != nil {
		callback := s.before
		s.before = nil
		callback()
	}
	if s.interrupt != nil {
		signal := s.interrupt
		s.interrupt = nil
		signal <- os.Interrupt
		<-ctx.Done()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.ObjectStore.(storage.LimitedGetter).GetLimited(ctx, key, limit)
}

func TestContentSearchConcurrentRefreshDoesNotReadMetadata(t *testing.T) {
	t.Parallel()
	env, objects, id := publishedFixture(t)
	wrapped := &searchRefreshStore{ObjectStore: objects}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return wrapped, nil }
	wrapped.before = func() {
		before := wrapped.reads
		var out, errOut bytes.Buffer
		if code := Run([]string{"search", "--refresh", "--session", id, "--harness", "codex"}, nil, &out, &errOut, env); code != 1 || !strings.Contains(errOut.String(), "another search refresh is running") {
			t.Fatalf("overlap code=%d %s %s", code, &out, &errOut)
		}
		if wrapped.reads != before {
			t.Fatal("overlapping refresh read archive metadata or source")
		}
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"search", "--refresh", "--session", id, "--harness", "codex"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("%d %s %s", code, &out, &errOut)
	}
	// A later independent invocation acquires the released work lock.
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"search", "--refresh", "--session", id, "--harness", "codex"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("lock retained %d %s %s", code, &out, &errOut)
	}
}

func TestContentSearchInterruptedRefreshReleasesWorkLock(t *testing.T) {
	t.Parallel()
	env, objects, id := publishedFixture(t)
	signals := make(chan os.Signal, 1)
	env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() {} }
	wrapped := &searchRefreshStore{ObjectStore: objects, interrupt: signals}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return wrapped, nil }
	var out, errOut bytes.Buffer
	if code := Run([]string{"search", "--refresh", "--session", id, "--harness", "codex"}, nil, &out, &errOut, env); code != 1 || !strings.Contains(errOut.String(), "context canceled") {
		t.Fatalf("%d %s %s", code, &out, &errOut)
	}
	env.Interrupts = noInterrupts
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"search", "--refresh", "--session", id, "--harness", "codex"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("lock retained after cancellation %d %s %s", code, &out, &errOut)
	}
}

func TestContentSearchRefreshBoundsWholePass(t *testing.T) {
	t.Parallel()
	env, objects, id := publishedFixture(t)
	wrapped := &searchRefreshStore{ObjectStore: objects, inspect: func(ctx context.Context) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Second {
			t.Fatalf("refresh archive read has no finite pass deadline: %v %v", deadline, ok)
		}
	}}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return wrapped, nil }
	var out, errOut bytes.Buffer
	if code := Run([]string{"search", "--refresh", "--session", id, "--harness", "codex"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("%d %s %s", code, &out, &errOut)
	}
	if wrapped.reads < 2 {
		t.Fatalf("did not exercise metadata and source reads: %d", wrapped.reads)
	}
}

func TestContentSearchBoundedArchiveRefreshAndOfflineQuery(t *testing.T) {
	env, objects, id := publishedFixture(t)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects, nil }
	var out, errOut bytes.Buffer
	if code := Run([]string{"search", "--refresh"}, nil, &out, &errOut, env); code != 0 || !strings.Contains(out.String(), "Observed archive slice") {
		t.Fatalf("%d %s %s", code, &out, &errOut)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Error("ordinary query opened remote archive")
		return nil, errors.New("offline")
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"search", "visible", "--all-projects"}, nil, &out, &errOut, env); code != 0 || !strings.Contains(out.String(), id) {
		t.Fatalf("%d %s %s", code, &out, &errOut)
	}
}
