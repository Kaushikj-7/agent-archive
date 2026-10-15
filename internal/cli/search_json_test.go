package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/reader"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func searchJSONRun(t *testing.T, env Env, args ...string) (int, contentSearchDocument, string) {
	t.Helper()
	var out, diagnostics bytes.Buffer
	code := Run(append([]string{"search", "--json"}, args...), nil, &out, &diagnostics, env)
	var doc contentSearchDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("invalid document code=%d %q %s: %v", code, out.String(), diagnostics.String(), err)
	}
	data, err := os.ReadFile("../../schemas/search-output.schema.json")
	must(t, err)
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	must(t, err)
	compiler := jsonschema.NewCompiler()
	const id = "https://raw.githubusercontent.com/wangjohn/agent-archive/main/schemas/search-output.schema.json"
	must(t, compiler.AddResource(id, value))
	schema, err := compiler.Compile(id)
	must(t, err)
	value, err = jsonschema.UnmarshalJSON(bytes.NewReader(out.Bytes()))
	must(t, err)
	if err = schema.Validate(value); err != nil {
		t.Fatalf("schema: %v\n%s", err, out.String())
	}
	return code, doc, diagnostics.String()
}

func TestContentSearchMissingJSONAndExplicitScope(t *testing.T) {
	env, _, _ := publishedFixture(t)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("cached query opened archive")
		return nil, errors.New("offline")
	}
	code, doc, diagnostics := searchJSONRun(t, env, "visible", "--all-projects")
	if code != 0 || doc.Coverage.Present || doc.Results == nil || doc.Coverage.Complete || doc.TotalMatched != nil {
		t.Fatalf("missing %d %+v %s", code, doc, diagnostics)
	}
	home, _ := env.readHome()
	cfg, _, err := config.Load(home)
	must(t, err)
	_, dir := contentSearchLocation(home, cfg)
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("created index: %v", err)
	}
}

func TestContentSearchJSONRefreshQueryAndExactContext(t *testing.T) {
	env, objects, id := publishedFixture(t)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects, nil }
	code, doc, diagnostics := searchJSONRun(t, env, "visible", "--refresh", "--session", id, "--harness", "codex", "--all-projects")
	if code != 0 || len(doc.Results) != 1 || doc.Refresh == nil || doc.Results[0].Reference == nil {
		t.Fatalf("query %d %+v %s", code, doc, diagnostics)
	}
	token := *doc.Results[0].Reference
	code, doc, diagnostics = searchJSONRun(t, env, "--read", token, "--max-bytes", "4", "--before", "1", "--after", "1")
	if code != 0 || doc.Context == nil || len(doc.Context.Selected.Text) > 4 || doc.Context.Selected.Next == nil {
		t.Fatalf("context %d %+v %s", code, doc, diagnostics)
	}
	next := *doc.Context.Selected.Next
	ref, err := decodeSearchReference(token)
	must(t, err)
	if next != ref.Start+len(doc.Context.Selected.Text) {
		t.Fatal("escaped offsets replaced raw bytes")
	}
}

func TestContentSearchConflictsBeforeIO(t *testing.T) {
	for _, args := range [][]string{{"--wait", "1s", "words"}, {"--refresh", "--cursor", "bad"}, {"--read", "bad"}, {"--refresh", "--project", "x"}, {"words", "--max-output-bytes", "2"}, {"words", "--before", "1"}, {"words", "--max-bytes", "4096"}, {"words", "--offset", "-1"}, {"--read", "bad", "--limit", "20"}, {"--refresh", "--wait", "0s"}} {
		var out bytes.Buffer
		if code := Run(append([]string{"search"}, args...), nil, &out, &out, Env{}); code != 2 {
			t.Fatalf("%v: %d %s", args, code, out.String())
		}
	}
}

func TestContentSearchOutputBudgetNeverSkipsPageHits(t *testing.T) {
	cursor := "next"
	doc := contentSearchDocument{SchemaVersion: 1, Kind: contentSearchResults, OK: true, Results: []contentSearchHit{{Session: "one", Title: strings.Repeat("\u0085", 8192)}}, Returned: 1, NextCursor: &cursor}

	var out, diagnostics bytes.Buffer
	code := writeContentSearch(doc, contentSearchOptions{json: true, outputBytes: 4096}, &out, &diagnostics)
	var result contentSearchDocument
	must(t, json.Unmarshal(out.Bytes(), &result))
	if code != 1 || len(out.Bytes()) > 4096 || result.Error.Code != "output_too_large" || result.NextCursor != nil || len(result.Results) != 0 {
		t.Fatalf("oversized page %d %+v", code, result)
	}
}

func TestContentSearchRebuildAndHiddenPass(t *testing.T) {
	env, objects, _ := publishedFixture(t)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects, nil }
	code, doc, diag := searchJSONRun(t, env, "--refresh", "--rebuild")
	if code != 0 || !doc.Refresh.Rebuild {
		t.Fatalf("rebuild %d %+v %s", code, doc, diag)
	}
	var out bytes.Buffer
	if code = Run([]string{"_search-index"}, nil, &out, &out, env); code != 0 {
		t.Fatalf("hidden %d %s", code, out.String())
	}
	home, _ := env.readHome()
	cfg, _, err := config.Load(home)
	must(t, err)
	ns, dir := contentSearchLocation(home, cfg)
	index, err := search.OpenRead(t.Context(), dir, ns)
	must(t, err)
	defer func() { _ = index.Close() }()
	if _, err = os.Stat(filepath.Join(home, "search-worker.lock")); err != nil {
		t.Fatalf("stable home lock missing: %v", err)
	}
}

func TestContentSearchOutsideProjectRequiresExplicitQueryScope(t *testing.T) {
	env, _, _ := publishedFixture(t)
	env.WorkingDir = func() (string, error) { return "", errors.New("no cwd") }
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("scope refusal opened storage")
		return nil, errors.New("offline")
	}
	code, doc, diag := searchJSONRun(t, env, "visible")
	if code != 1 || doc.Error == nil || doc.Error.Code != "scope_required" {
		t.Fatalf("scope %d %+v %s", code, doc, diag)
	}
	var out bytes.Buffer
	if code = Run([]string{"_search-index", "--help"}, nil, &out, &out, Env{}); code != 0 {
		t.Fatalf("hidden help %d %s", code, out.String())
	}
}

func TestContentSearchNamedWaitCompletesItsOwnTarget(t *testing.T) {
	env, objects, id := publishedFixture(t)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects, nil }
	code, doc, diag := searchJSONRun(t, env, "--refresh", "--session", id, "--harness", "codex", "--wait", "1s")
	if code != 0 || !doc.Refresh.Completed || doc.Refresh.Passes != 1 {
		t.Fatalf("named wait %d %+v %s", code, doc, diag)
	}
}

func TestContentSearchContextUsesOneVerifiedSourceRead(t *testing.T) {
	env, objects, id := publishedFixture(t)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects, nil }
	code, doc, diag := searchJSONRun(t, env, "visible", "--refresh", "--session", id, "--harness", "codex", "--all-projects")
	if code != 0 || len(doc.Results) == 0 || doc.Results[0].Reference == nil {
		t.Fatalf("setup %d %+v %s", code, doc, diag)
	}
	token := *doc.Results[0].Reference
	counted := &searchRefreshStore{ObjectStore: objects}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return counted, nil }
	code, doc, diag = searchJSONRun(t, env, "--read", token, "--before", "2", "--after", "2")
	if code != 0 || counted.reads != 2 {
		t.Fatalf("context read metadata+source once: %d reads=%d %+v %s", code, counted.reads, doc, diag)
	}
}

func TestContentSearchSchemaRejectsInvalidSuccessAndNullArrays(t *testing.T) {
	data, err := os.ReadFile("../../schemas/search-output.schema.json")
	must(t, err)
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	must(t, err)
	compiler := jsonschema.NewCompiler()
	const id = "https://raw.githubusercontent.com/wangjohn/agent-archive/main/schemas/search-output.schema.json"
	must(t, compiler.AddResource(id, value))
	schema, err := compiler.Compile(id)
	must(t, err)
	for _, raw := range []string{`{"schema_version":2,"kind":"search"}`, `{"schema_version":1,"kind":"search_context","ok":true,"error":null,"returned":0,"results":[],"generation":0,"next_cursor":null,"truncated":false,"total_matched_known":false,"total_matched":null}`, `{"schema_version":1,"kind":"search_refresh","ok":false,"error":null,"returned":0,"results":null,"generation":0,"next_cursor":null,"truncated":false,"total_matched_known":false,"total_matched":null}`} {
		value, err = jsonschema.UnmarshalJSON(strings.NewReader(raw))
		must(t, err)
		if err = schema.Validate(value); err == nil {
			t.Fatalf("accepted invalid output: %s", raw)
		}
	}
}

type shortSearchWriter struct{ calls int }

func (w *shortSearchWriter) Write(data []byte) (int, error) { w.calls++; return len(data) / 2, nil }

func TestContentSearchDoesNotRetryShortJSONWrite(t *testing.T) {
	env, _, _ := publishedFixture(t)
	out := &shortSearchWriter{}
	var diagnostics bytes.Buffer
	code := Run([]string{"search", "visible", "--all-projects", "--json"}, nil, out, &diagnostics, env)
	if code != 1 || out.calls != 1 || !strings.Contains(diagnostics.String(), "short write") {
		t.Fatalf("short writer code=%d calls=%d %s", code, out.calls, diagnostics.String())
	}
}

type contentSearchCountingStore struct {
	*storagetest.MemoryStore
	sources  int
	metadata int
}

func (s *contentSearchCountingStore) Get(ctx context.Context, key string) ([]byte, error) {
	if strings.Contains(key, "/source.") {
		s.sources++
	}
	if strings.HasSuffix(key, "/metadata.json") {
		s.metadata++
	}
	return s.MemoryStore.Get(ctx, key)
}

func (s *contentSearchCountingStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if strings.Contains(key, "/source.") {
		s.sources++
	}
	if strings.HasSuffix(key, "/metadata.json") {
		s.metadata++
	}
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

func TestContentSearchWaitMakesBoundedLegacyProgressThenReportsIncomplete(t *testing.T) {
	env, objects := contentSearchCorpus(t, 40)
	counted := &contentSearchCountingStore{MemoryStore: objects}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return counted, nil }
	started := time.Now()
	code, doc, diag := searchJSONRun(t, env, "--refresh", "--wait", "30s")
	if code != 1 || doc.Error == nil || doc.Error.Code != "coverage_incomplete" || doc.Coverage.Indexed != 40 || doc.Coverage.Pending != 0 || doc.Refresh.Completed || counted.sources != 40 {
		t.Fatalf("legacy progress code=%d sourceGET=%d %+v %s", code, counted.sources, doc, diag)
	}
	t.Logf("40 sessions: passes=%d loaded=%d reused=%d source_GET=%d metadata_GET=%d verification_checks=%d elapsed=%s", doc.Refresh.Passes, doc.Refresh.Loaded, doc.Refresh.Reused, counted.sources, counted.metadata, doc.Refresh.VerificationChecks, time.Since(started))
}

func TestContentSearchWaitBusyIsFiniteWithoutArchiveReads(t *testing.T) {
	env, _, _ := publishedFixture(t)
	home, _ := env.readHome()
	release, err := local.NamedLock(home, "search-worker.lock")
	must(t, err)
	defer release()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("busy wait opened archive")
		return nil, errors.New("busy")
	}
	started := time.Now()
	code, doc, diag := searchJSONRun(t, env, "--refresh", "--wait", "1s")
	if code != 1 || doc.Error == nil || doc.Error.Code != "wait_timeout" || doc.Coverage != nil || time.Since(started) > 5*time.Second {
		t.Fatalf("wait bound %d %+v %s", code, doc, diag)
	}
	if doc.Refresh.Passes > 3 {
		t.Fatalf("busy wait spun: %d passes", doc.Refresh.Passes)
	}
}

func TestContentSearchNamedWaitIgnoresOtherFailedSession(t *testing.T) {
	env, objects, id := publishedFixture(t)
	key, err := archive.MetadataObjectKey("codex", id)
	must(t, err)
	metadata, err := reader.ReadMetadata(t.Context(), objects, key)
	must(t, err)
	metadata.SessionID = "other-failed-session"
	otherKey, err := archive.MetadataObjectKey("codex", metadata.SessionID)
	must(t, err)
	data, err := json.Marshal(metadata)
	must(t, err)
	must(t, objects.Put(t.Context(), otherKey, data))
	home, _ := env.readHome()
	cfg, _, err := config.Load(home)
	must(t, err)
	ns, dir := contentSearchLocation(home, cfg)
	index, err := search.Open(t.Context(), dir, ns)
	must(t, err)
	must(t, index.Observe(t.Context(), metadata, parsersFor(env), env.now(), 0))
	must(t, index.Close())
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects, nil }
	code, doc, diag := searchJSONRun(t, env, "--refresh", "--session", id, "--harness", "codex", "--wait", "1s")
	if code != 0 || !doc.Refresh.Completed || doc.Coverage.Failed == 0 {
		t.Fatalf("target wait inherited other failure: %d %+v %s", code, doc, diag)
	}
}

func (s *contentSearchCountingStore) GetVersioned(ctx context.Context, key string) ([]byte, string, error) {
	if strings.Contains(key, "/source.") {
		s.sources++
	}
	if strings.HasSuffix(key, "/metadata.json") {
		s.metadata++
	}
	return s.MemoryStore.GetVersioned(ctx, key)
}

func TestContentSearchCancelledRefreshJSONKeepsObservationUnknown(t *testing.T) {
	env, objects, id := publishedFixture(t)
	signals := make(chan os.Signal, 1)
	env.Interrupts = func() (<-chan os.Signal, func()) { return signals, func() {} }
	wrapped := &searchRefreshStore{ObjectStore: objects, interrupt: signals}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return wrapped, nil }
	code, doc, diag := searchJSONRun(t, env, "--refresh", "--session", id, "--harness", "codex")
	if code != 1 || doc.Error == nil || doc.Error.Code != "interrupted" || doc.Coverage != nil {
		t.Fatalf("unknown cancelled observation %d %+v %s", code, doc, diag)
	}
}

func TestContentSearchQueryAfterWaitFailureReturnsOneDocumentAndCachedPage(t *testing.T) {
	env, objects, id := publishedFixture(t)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects, nil }
	code, _, diag := searchJSONRun(t, env, "--refresh", "--session", id, "--harness", "codex")
	if code != 0 {
		t.Fatal(diag)
	}
	home, _ := env.readHome()
	release, err := local.NamedLock(home, "search-worker.lock")
	must(t, err)
	defer release()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("busy refresh or cached query opened archive")
		return nil, errors.New("offline")
	}
	code, doc, diag := searchJSONRun(t, env, "visible", "--all-projects", "--refresh", "--wait", "1s")
	if code != 1 || doc.Error == nil || doc.Error.Code != "wait_timeout" || doc.Refresh == nil || len(doc.Results) != 1 || doc.Coverage == nil || !doc.Coverage.Present {
		t.Fatalf("combined document %d %+v %s", code, doc, diag)
	}
}

func (s *contentSearchCountingStore) GetLimitedVersioned(ctx context.Context, key string, limit int64) ([]byte, string, error) {
	if strings.Contains(key, "/source.") {
		s.sources++
	}
	if strings.HasSuffix(key, "/metadata.json") {
		s.metadata++
	}
	return s.MemoryStore.GetLimitedVersioned(ctx, key, limit)
}
