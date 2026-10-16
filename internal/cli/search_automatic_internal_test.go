package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/collector"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
	"github.com/wangjohn/agent-archive/internal/storage"
)

type automaticFixtureState string

const (
	automaticFixtureStateMissing     automaticFixtureState = "missing"
	automaticFixtureStateDisabled    automaticFixtureState = "disabled"
	automaticFixtureStatePaused      automaticFixtureState = "paused"
	automaticFixtureStateUninstalled automaticFixtureState = "uninstalled"
	automaticFixtureStatePending     automaticFixtureState = "pending"
	automaticFixtureStateBusy        automaticFixtureState = "busy"
)

func TestAutomaticSearchNeverCreatesIndexWithoutCurrentConsent(t *testing.T) {
	t.Parallel()
	for _, state := range []automaticFixtureState{automaticFixtureStateMissing, automaticFixtureStateDisabled, automaticFixtureStatePaused, automaticFixtureStateUninstalled, automaticFixtureStatePending, automaticFixtureStateBusy} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			env, objects, _ := publishedFixture(t)
			home, err := env.readHome()
			must(t, err)
			cfg := enableSearchForTest(t, home)
			_, dir := contentSearchLocation(home, cfg)
			switch state {
			case automaticFixtureStateMissing:
				must(t, os.Remove(filepath.Join(home, "config.json")))
			case automaticFixtureStateDisabled:
				cfg.SearchIndexing = false
				must(t, config.Save(home, cfg))
			case automaticFixtureStatePaused:
				cfg.Paused = true
				must(t, config.Save(home, cfg))
			case automaticFixtureStateUninstalled:
				cfg.Archive.Enabled = false
				must(t, config.Save(home, cfg))
			case automaticFixtureStatePending:
				must(t, os.WriteFile(setupjournal.JournalPath(home), []byte(automaticFixtureStatePending), 0o600))
			case automaticFixtureStateBusy:
				release, err := local.NamedLock(home, "search-worker.lock")
				must(t, err)
				defer release()
			}
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
				t.Fatal("no-op automatic pass opened archive")
				return objects, nil
			}
			var out, diagnostics bytes.Buffer
			if code := Run([]string{"_search-index"}, nil, &out, &diagnostics, env); code != 0 || out.Len() != 0 || diagnostics.Len() != 0 {
				t.Fatal("scheduled no-op was not quiet", code, out.String(), diagnostics.String())
			}
			if _, err = os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("scheduled no-op created cache", err)
			}
		})
	}
}

func TestAutomaticSearchRechecksConsentUnderStableHomeLock(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	home, err := env.readHome()
	must(t, err)
	expected := enableSearchForTest(t, home)
	current := expected
	current.SearchIndexing = false
	must(t, config.Save(home, current))
	_, err = runContentSearchPass(t.Context(), home, expected, env, contentSearchOptions{automatic: true, refresh: true})
	if !errors.Is(err, errAutomaticSearchSkipped) {
		t.Fatal("under-lock disabled config admitted", err)
	}
	_, dir := contentSearchLocation(home, expected)
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("under-lock refusal created cache", err)
	}
}

func TestPeriodicSearchProgressesWithoutHintsAndExplicitRefreshWorksPaused(t *testing.T) {
	t.Parallel()
	env, objects, _ := publishedFixture(t)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects, nil }
	home, err := env.readHome()
	must(t, err)
	cfg := enableSearchForTest(t, home)
	var out, diagnostics bytes.Buffer
	complete := false
	for range 16 {
		if code := Run([]string{"_search-index"}, nil, &out, &diagnostics, env); code != 0 {
			t.Fatal(code, diagnostics.String())
		}
		namespace, dir := contentSearchLocation(home, cfg)
		index, e := search.OpenRead(t.Context(), dir, namespace)
		must(t, e)
		observation, e := index.Observation(t.Context())
		must(t, e)
		must(t, index.Close())
		if observation.Indexed > 0 && observation.Pending == 0 {
			complete = true
			break
		}
	}
	if !complete {
		t.Fatal("periodic passes did not progress without publication hints")
	}
	cfg.Paused = true
	must(t, config.Save(home, cfg))
	if code, doc, diag := searchJSONRun(t, env, "--refresh"); code != 0 || doc.Refresh == nil {
		t.Fatal("explicit paused refresh failed", code, doc, diag)
	}
	cfg, _, err = config.Load(home)
	must(t, err)
	if !cfg.Paused || !cfg.SearchIndexing {
		t.Fatal("explicit refresh changed local consent or pause")
	}
}

func TestDuplicateAutomaticTickDoesNotOverlapWorkerOrCollector(t *testing.T) {
	t.Parallel()
	env, objects, _ := publishedFixture(t)
	home, err := env.readHome()
	must(t, err)
	enableSearchForTest(t, home)
	entered, finish := make(chan struct{}), make(chan struct{})
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { close(entered); <-finish; return objects, nil }
	done := make(chan int, 1)
	go func() { var out, diag bytes.Buffer; done <- Run([]string{"_search-index"}, nil, &out, &diag, env) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not enter bounded pass")
	}
	release, err := local.Lock(home)
	must(t, err)
	release()
	reg := theRegistration(t, home)
	payload, err := json.Marshal(map[string]any{"hook_event_name": "Stop", "session_id": "native-1", "cwd": reg.ProjectRoot, "transcript_path": reg.TranscriptPath})
	must(t, err)
	started := time.Now()
	if code := Run([]string{"_hook", "--harness", "codex"}, bytes.NewReader(payload), io.Discard, io.Discard, env); code != 0 {
		t.Fatal("concurrent hook failed", code)
	}
	t.Logf("actual hook while independent worker holds search fence: %s", time.Since(started))
	var out, diag bytes.Buffer
	code := Run([]string{"_search-index"}, nil, &out, &diag, env)
	close(finish)
	if code != 0 || out.Len() != 0 || diag.Len() != 0 {
		t.Fatal("duplicate scheduled tick was not quiet", code, out.String(), diag.String())
	}
	if code = <-done; code != 0 {
		t.Fatal("first automatic pass failed", code)
	}
}

type hintDropState string

const (
	hintDropDestination hintDropState = "destination"
	hintDropNamespace   hintDropState = "namespace"
	hintDropBusy        hintDropState = "busy"
	hintDropDisabled    hintDropState = "disabled"
	hintDropPending     hintDropState = "pending"
)

func TestSearchHintDropsDestinationOrNamespaceChangesAndBusyLifecycle(t *testing.T) {
	t.Parallel()
	env, _, _ := publishedFixture(t)
	home, err := env.readHome()
	must(t, err)
	original := enableSearchForTest(t, home)
	namespace, dir := contentSearchLocation(home, original)
	index, err := search.Open(t.Context(), dir, namespace)
	must(t, err)
	must(t, index.Close())
	sink := searchChangeSink{home: home, namespace: namespace}
	batch := SessionChangeBatch{DestinationID: original.DestinationID(), Sessions: []collector.PublicationCandidate{{Harness: "codex", ArchiveSessionID: "candidate"}}}
	for _, state := range []hintDropState{hintDropDestination, hintDropNamespace, hintDropBusy, hintDropDisabled, hintDropPending} {
		current := original
		switch state {
		case hintDropDestination:
			current.Storage.Bucket = "other"
		case hintDropNamespace:
			current.DestinationSince = current.DestinationSince.Add(time.Second)
		case hintDropDisabled:
			current.SearchIndexing = false
		case hintDropBusy, hintDropPending:
		}
		must(t, config.Save(home, current))
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		if state == hintDropBusy {
			release, e := local.NamedLock(home, "search-worker.lock")
			must(t, e)
			must(t, sink.Hint(ctx, batch))
			release()
		} else {
			if state == hintDropPending {
				must(t, os.WriteFile(setupjournal.JournalPath(home), []byte(hintDropPending), 0o600))
			}
			must(t, sink.Hint(ctx, batch))
			if state == hintDropPending {
				must(t, os.Remove(setupjournal.JournalPath(home)))
			}
		}
		cancel()
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "content.sqlite"))
	must(t, err)
	defer func() { _ = db.Close() }()
	var count int
	must(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_observed").Scan(&count))
	if count != 0 {
		t.Fatal("dropped hint changed original namespace", count)
	}
	must(t, config.Save(home, original))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	must(t, sink.Hint(ctx, batch))
	must(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM search_observed").Scan(&count))
	if count != 1 {
		t.Fatal("valid hint did not seed identity", count)
	}

}

func TestAutomaticSearchRequiresExplicitUpgradeAndRefusesFutureSchema(t *testing.T) {
	t.Parallel()
	env, objects, _ := publishedFixture(t)
	home, err := env.readHome()
	must(t, err)
	cfg := enableSearchForTest(t, home)
	namespace, dir := contentSearchLocation(home, cfg)
	index, err := search.Open(t.Context(), dir, namespace)
	must(t, err)
	must(t, index.Close())
	db, err := sql.Open("sqlite", filepath.Join(dir, "content.sqlite"))
	must(t, err)
	for _, statement := range []string{"DROP TABLE search_discovery", "DROP TABLE search_observed", "DROP INDEX search_coverage_gaps", "PRAGMA user_version=2"} {
		_, err = db.ExecContext(t.Context(), statement)
		must(t, err)
	}
	must(t, db.Close())
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("automatic old-schema pass opened archive")
		return nil, nil
	}
	var out, diag bytes.Buffer
	if code := Run([]string{"_search-index"}, nil, &out, &diag, env); code != 1 {
		t.Fatal("automatic pass migrated old schema", code, diag.String())
	}
	db, err = sql.Open("sqlite", filepath.Join(dir, "content.sqlite"))
	must(t, err)
	var version, objectsCount int
	must(t, db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version))
	must(t, db.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_schema WHERE name='search_discovery'").Scan(&objectsCount))
	must(t, db.Close())
	if version != 2 || objectsCount != 0 {
		t.Fatal("automatic admission performed DDL", version, objectsCount)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects, nil }
	if code, _, diagnostic := searchJSONRun(t, env, "--refresh"); code != 0 {
		t.Fatal("explicit upgrade failed", code, diagnostic)
	}
	index, err = search.OpenRead(t.Context(), dir, namespace)
	must(t, err)
	must(t, index.Close())
	db, err = sql.Open("sqlite", filepath.Join(dir, "content.sqlite"))
	must(t, err)
	_, err = db.ExecContext(t.Context(), "PRAGMA user_version=5")
	must(t, err)
	must(t, db.Close())
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("future schema admitted archive IO")
		return nil, nil
	}
	out.Reset()
	diag.Reset()
	if code := Run([]string{"_search-index"}, nil, &out, &diag, env); code != 1 {
		t.Fatal("future schema admitted", code, diag.String())
	}
	db, err = sql.Open("sqlite", filepath.Join(dir, "content.sqlite"))
	must(t, err)
	must(t, db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version))
	must(t, db.Close())
	if version != 5 {
		t.Fatal("automatic pass rewrote future schema", version)
	}
}

func TestAutomaticSearchRetriesOnlyEmptyInterruptedInitialization(t *testing.T) {
	t.Parallel()
	for _, nonempty := range []bool{false, true} {
		t.Run(strconv.FormatBool(nonempty), func(t *testing.T) {
			t.Parallel()
			env, objects, _ := publishedFixture(t)
			home, err := env.readHome()
			must(t, err)
			cfg := enableSearchForTest(t, home)
			namespace, dir := contentSearchLocation(home, cfg)
			if nonempty {
				must(t, os.MkdirAll(dir, 0o700))
				db, e := sql.Open("sqlite", filepath.Join(dir, "content.sqlite"))
				must(t, e)
				_, e = db.ExecContext(t.Context(), "CREATE TABLE unknown_application(content TEXT)")
				must(t, e)
				must(t, db.Close())
			} else {
				interrupted, cancel := context.WithCancel(t.Context())
				cancel()
				aborted, e := search.Open(interrupted, dir, namespace)
				if aborted != nil {
					_ = aborted.Close()
					t.Fatal("interrupted initialization unexpectedly opened")
				}
				if !errors.Is(e, context.Canceled) {
					t.Fatal("fixture did not interrupt initial Open", e)
				}
				_, e = search.OpenRead(t.Context(), dir, namespace)
				if !errors.Is(e, search.ErrUninitialized) || !errors.Is(e, search.ErrUpgradeRequired) {
					t.Fatal("interrupted initial schema not proven empty", e)
				}
			}

			env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
				if nonempty {
					t.Fatal("nonempty schema0 opened archive")
				}
				return objects, nil
			}
			var out, diag bytes.Buffer
			code := Run([]string{"_search-index"}, nil, &out, &diag, env)
			if nonempty {
				if code != 1 {
					t.Fatal("nonempty schema0 accepted", code, diag.String())
				}
				return
			}
			if code != 0 {
				t.Fatal("empty initial database could not recover", code, diag.String())
			}
			index, err := search.OpenRead(t.Context(), dir, namespace)
			must(t, err)
			must(t, index.Close())
		})
	}
}
