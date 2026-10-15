package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Timings are synthetic advisory observations, while matching request counts
// deterministically prove hints add no archive reads to capture or existing reads.
func TestSearchEnabledCaptureAndReadOperationParity(t *testing.T) {
	samples := map[string]storagetest.ReadMetrics{}
	for _, enabled := range []bool{false, true} {
		home, env, remote := collectFixture(t, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
		if enabled {
			cfg := enableSearchForTest(t, home)
			namespace, dir := contentSearchLocation(home, cfg)
			index, e := search.Open(t.Context(), dir, namespace)
			must(t, e)
			must(t, index.Close())
		}
		measured := storagetest.NewMeasuredStore(remote, 0)
		env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return measured, nil }
		reg := theRegistration(t, home)
		payload, err := json.Marshal(map[string]any{"hook_event_name": "Stop", "session_id": "native-1", "cwd": reg.ProjectRoot, "transcript_path": reg.TranscriptPath})
		must(t, err)
		started := time.Now()
		var diagnostics bytes.Buffer
		if code := Run([]string{"_hook", "--harness", "codex"}, bytes.NewReader(payload), io.Discard, &diagnostics, env); code != 0 {
			t.Fatal(code, diagnostics.String())
		}
		t.Logf("enabled=%v actual hook: %s requests=%+v", enabled, time.Since(started), measured.Metrics())
		if measured.Metrics().Gets != 0 || measured.Metrics().Lists != 0 {
			t.Fatal("hook performed archive IO")
		}
		started = time.Now()
		if code := Run([]string{"sync"}, nil, io.Discard, &diagnostics, env); code != 0 {
			t.Fatal(code, diagnostics.String())
		}
		samples[fmt.Sprint(enabled, "/sync")] = measured.Metrics()
		t.Logf("enabled=%v actual sync publication: %s requests=%+v", enabled, time.Since(started), measured.Metrics())
		for _, args := range [][]string{{"list", "--all-projects", "--json"}, {"show", reg.ArchiveSessionID}, {"handoff", "--source", "archive", reg.ArchiveSessionID}} {
			measured = storagetest.NewMeasuredStore(remote, 0)
			started = time.Now()
			diagnostics.Reset()
			if code := Run(args, nil, io.Discard, &diagnostics, env); code != 0 {
				t.Fatal(args, code, diagnostics.String())
			}
			samples[fmt.Sprint(enabled, "/", args[0])] = measured.Metrics()
			t.Logf("enabled=%v actual %s: %s requests=%+v", enabled, args[0], time.Since(started), measured.Metrics())
		}
	}
	for _, command := range []string{"sync", "list", "show", "handoff"} {
		a, b := samples["false/"+command], samples["true/"+command]
		if a.Gets != b.Gets || a.Lists != b.Lists {
			t.Fatal("search enablement added archive IO", command, a, b)
		}
	}
}

func TestSearchEnabledBackfillOperationParity(t *testing.T) {
	var baseline storagetest.ReadMetrics
	for _, enabled := range []bool{false, true} {
		f, remote := newImportFixture(t)
		var hintTime time.Duration
		hintCalls := 0
		if enabled {
			cfg := enableSearchForTest(t, f.data)
			namespace, dir := contentSearchLocation(f.data, cfg)
			index, err := search.Open(t.Context(), dir, namespace)
			must(t, err)
			must(t, index.Close())
			consumers := f.env.changeConsumers(f.data, namespace)
			sink := consumers[0].sink
			consumers[0].sink = changeSinkFunc(func(ctx context.Context, batch SessionChangeBatch) error {
				started := time.Now()
				defer func() { hintTime += time.Since(started); hintCalls++ }()
				return sink.Hint(ctx, batch)
			})
			f.env.sessionChangeConsumers = consumers
		}
		measured := storagetest.NewMeasuredStore(remote, 0)
		f.env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return measured, nil }
		started := time.Now()
		out, diag, code := f.importRun(t, strings.NewReader("y\n"), true)
		if code != 0 {
			t.Fatal(code, out, diag)
		}
		metric := measured.Metrics()
		t.Logf("enabled=%v actual backfill: %s requests=%+v", enabled, time.Since(started), metric)
		t.Logf("enabled=%v direct production hint calls=%d elapsed=%s", enabled, hintCalls, hintTime)
		if !enabled {
			baseline = metric
		} else if baseline.Gets != metric.Gets || baseline.Lists != metric.Lists {
			t.Fatal("search enablement added backfill archive IO", baseline, metric)
		}
	}
}

func TestHintFullDriverAndDiagnosticContentionWallTime(t *testing.T) {
	home, env, _ := collectFixture(t, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	cfg := enableSearchForTest(t, home)
	namespace, dir := contentSearchLocation(home, cfg)
	index, err := search.Open(t.Context(), dir, namespace)
	must(t, err)
	defer func() { _ = index.Close() }()
	// Busy lifecycle admission must drop before SQLite, without waiting for a pass.
	release, err := local.NamedLock(home, "search-worker.lock")
	must(t, err)
	candidates := []collector.PublicationCandidate{{Harness: "codex", ArchiveSessionID: "synthetic"}}
	started := time.Now()
	dispatchSessionChanges(t.Context(), cfg.DestinationID(), candidates, env.changeConsumers(home, namespace))
	elapsed := time.Since(started)
	release()
	t.Logf("actual full hint dispatch while worker fence held: %s (0 archive IO)", elapsed)
	if elapsed > 250*time.Millisecond {
		t.Fatal("busy lifecycle hint waited", elapsed)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "content.sqlite"))
	must(t, err)
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(t.Context(), nil)
	must(t, err)
	_, err = tx.ExecContext(t.Context(), "UPDATE search_state SET generation=generation+1")
	must(t, err)
	started = time.Now()
	dispatchSessionChanges(t.Context(), cfg.DestinationID(), candidates, env.changeConsumers(home, namespace))
	elapsed = time.Since(started)
	must(t, tx.Rollback())
	t.Logf("actual full hint open/admission/contended transaction/close/diagnostic: %s (0 archive IO)", elapsed)
	if elapsed > 250*time.Millisecond {
		t.Fatal("busy database hint exceeded bounded slice", elapsed)
	}
	if log, err := os.ReadFile(filepath.Join(home, "search-error.log")); err == nil {
		if len(log) > 128 || bytes.Contains(log, []byte("synthetic")) {
			t.Fatal("diagnostic was not bounded/content-free", string(log))
		}
	}

}

// sourceOverlapStore stalls an actual verified-source GET, after discovery and
// writer admission. Capture uses the same archive through an unstalled wrapper.
type sourceOverlapStore struct {
	*storagetest.MemoryStore
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (s *sourceOverlapStore) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	if strings.Contains(key, "/source.") {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.MemoryStore.GetLimited(ctx, key, limit)
}

func TestCapturePublishesWhileSearchSourceReadIsInFlight(t *testing.T) {
	for _, command := range []string{"sync", "backfill"} {
		t.Run(command, func(t *testing.T) {
			env, objects, _ := publishedFixture(t)
			home, err := env.readHome()
			must(t, err)
			var capture func() (int, string)
			if command == "sync" {
				reg := theRegistration(t, home)
				f, e := os.OpenFile(reg.TranscriptPath, os.O_APPEND|os.O_WRONLY, 0)
				must(t, e)
				_, e = f.WriteString("\n" + `{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"new visible publication"}}`)
				must(t, e)
				must(t, f.Close())
				capture = func() (int, string) {
					var out, diag bytes.Buffer
					code := Run([]string{"sync"}, nil, &out, &diag, env)
					return code, diag.String()
				}
			} else {
				f, remote := newImportFixture(t)
				// Seed only synthetic archive objects, so an independent worker has
				// a real verified source to load during this fixture's first import.
				rows, e := objects.List(t.Context(), "")
				must(t, e)
				for _, row := range rows {
					body, e := objects.Get(t.Context(), row.Key)
					must(t, e)
					must(t, remote.Put(t.Context(), row.Key, body))
				}
				env, objects, home = f.env, remote, f.data
				capture = func() (int, string) {
					f.env = env
					_, diag, code := f.importRun(t, strings.NewReader("y\n"), true)
					return code, diag
				}
			}
			at := env.now().Add(4 * time.Minute)
			env.Now = func() time.Time { return at }
			cfg := enableSearchForTest(t, home)
			measured := storagetest.NewMeasuredStore(objects, 0)
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return measured, nil }
			entered, release := make(chan struct{}), make(chan struct{})
			worker := env
			worker.OpenStore = func(config.Config) (storage.ObjectStore, error) {
				return &sourceOverlapStore{MemoryStore: objects, entered: entered, release: release}, nil
			}
			done := make(chan error, 1)
			go func() {
				_, e := runContentSearchPass(t.Context(), home, cfg, worker, contentSearchOptions{automatic: true, refresh: true})
				done <- e
			}()
			defer func() { close(release); must(t, <-done) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("worker never reached verified source read")
			}
			priorSources, err := objects.List(t.Context(), "")
			must(t, err)
			prior := map[string]string{}
			for _, row := range priorSources {
				if strings.Contains(row.Key, "/source.") {
					prior[row.Key] = row.ETag
				}
			}
			started := time.Now()
			code, diag := capture()
			if code != 0 {
				t.Fatal("capture blocked or failed during source load", code, diag)
			}
			select {
			case e := <-done:
				done <- e
				t.Fatal("worker stopped before capture finished", e)
			default:
			}
			published, err := objects.List(t.Context(), "")
			must(t, err)
			changed := false
			for _, row := range published {
				changed = changed || strings.Contains(row.Key, "/source.") && prior[row.Key] != row.ETag
			}
			if !changed {
				t.Fatal("capture did not publish a changed source while indexing")
			}
			t.Logf("actual %s completed during verified source GET: %s archive IO=%+v", command, time.Since(started), measured.Metrics())
		})
	}
}
