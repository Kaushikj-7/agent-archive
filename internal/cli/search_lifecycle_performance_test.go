package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
		if enabled {
			cfg := enableSearchForTest(t, f.data)
			namespace, dir := contentSearchLocation(f.data, cfg)
			index, err := search.Open(t.Context(), dir, namespace)
			must(t, err)
			must(t, index.Close())
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
