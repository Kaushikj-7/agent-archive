package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/search"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// This timestamp bounds the real decode/parse/install interval after the
// existing source barrier. It neither stalls nor instruments production work.
type timedSearchSourceRead struct {
	*sourceOverlapStore
	returned chan time.Time
}

func (s *timedSearchSourceRead) GetLimited(ctx context.Context, key string, limit int64) ([]byte, error) {
	body, err := s.sourceOverlapStore.GetLimited(ctx, key, limit)
	if err == nil && strings.Contains(key, "/source.") {
		select {
		case s.returned <- time.Now():
		default:
		}
	}
	return body, err
}

// One matched synthetic sync sample per mode measures ordinary real indexing
// contention. It makes no latency, CPU-isolation or hardware-capacity promise.
func TestCaptureWhileVerifiedSearchWorkProceeds(t *testing.T) {
	var baseline storagetest.ReadMetrics
	for _, concurrent := range []bool{false, true} {
		t.Run(strconv.FormatBool(concurrent), func(t *testing.T) {
			home, env, objects := collectFixture(t, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
			first := theRegistration(t, home)
			var raw strings.Builder
			for i := range 2000 {
				raw.WriteString(`{"type":"response_item","id":"index` + strconv.Itoa(i) + `","payload":{"type":"message","role":"user","content":"indexwork synthetic record ` + strconv.Itoa(i) + `"}}` + "\n")
			}
			must(t, os.WriteFile(first.TranscriptPath, []byte(raw.String()), 0o600))
			secondPath := filepath.Join(first.ProjectRoot, "capture.jsonl")
			secondRaw := `{"type":"response_item","id":"capture1","payload":{"type":"message","role":"user","content":"capture original"}}` + "\n"
			must(t, os.WriteFile(secondPath, []byte(secondRaw), 0o600))
			payload, err := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-2", "cwd": first.ProjectRoot, "transcript_path": secondPath})
			must(t, err)
			var diagnostics bytes.Buffer
			if code := Run([]string{"_hook", "--harness", "codex"}, bytes.NewReader(payload), &diagnostics, &diagnostics, env); code != 0 {
				t.Fatal("second registration", code, diagnostics.String())
			}
			if code := Run([]string{"sync"}, nil, &diagnostics, &diagnostics, env); code != 0 {
				t.Fatal("seed publication", code, diagnostics.String())
			}
			localState, err := state.Open(home)
			must(t, err)
			registrations, err := localState.LoadRegistrations()
			must(t, err)
			var second archive.SessionRegistration
			for _, reg := range registrations {
				if reg.NativeSessionID == "native-2" {
					second = reg
				}
			}
			if len(registrations) != 2 || second.ArchiveSessionID == "" {
				t.Fatal("two private sessions required", registrations)
			}
			secondKey, err := archive.MetadataObjectKey("codex", second.ArchiveSessionID)
			must(t, err)
			seeded, err := objects.Get(t.Context(), secondKey)
			must(t, err)
			var original archive.Metadata
			must(t, json.Unmarshal(seeded, &original))
			// Collection has a due-time throttle; advance the injected clock so
			// this measures a real changed publication rather than a quiet pass.
			at := env.now().Add(4 * time.Minute)
			env.Now = func() time.Time { return at }
			secondRaw += `{"type":"response_item","id":"capture2","payload":{"type":"message","role":"user","content":"capture changed source"}}` + "\n"
			must(t, os.WriteFile(secondPath, []byte(secondRaw), 0o600))
			captureReads := storagetest.NewMeasuredStore(objects, 0)
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return captureReads, nil }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type completion struct {
				pass     contentSearchPass
				err      error
				finished time.Time
			}
			done := make(chan completion, 1)
			entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan time.Time, 1)
			var releaseOnce sync.Once
			started, joined := false, false
			defer func() {
				cancel()
				releaseOnce.Do(func() { close(release) })
				if started && !joined {
					<-done
				}
			}()
			var indexingReads *storagetest.MeasuredStore
			if concurrent {
				cfg := enableSearchForTest(t, home)
				worker := env
				indexingReads = storagetest.NewMeasuredStore(&timedSearchSourceRead{&sourceOverlapStore{MemoryStore: objects, entered: entered, release: release}, returned}, 0)
				worker.OpenStore = func(config.Config) (storage.ObjectStore, error) { return indexingReads, nil }
				started = true
				go func() {
					pass, err := runContentSearchPass(ctx, home, cfg, worker, contentSearchOptions{refresh: true, session: first.ArchiveSessionID, query: search.Query{Harness: "codex"}})
					done <- completion{pass, err, time.Now()}
				}()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("worker did not reach verified source barrier")
				}
			}
			diagnostics.Reset()
			captureStart := time.Now()
			releaseOnce.Do(func() { close(release) })
			code := Run([]string{"sync"}, nil, &diagnostics, &diagnostics, env)
			captureEnd := time.Now()
			if code != 0 {
				t.Fatal("concurrent publication", code, diagnostics.String())
			}
			metrics := captureReads.Metrics()
			t.Logf("concurrent=%v sync=%s capture_requests=%+v index_source_records=2000 index_raw_B=%d", concurrent, captureEnd.Sub(captureStart), metrics, raw.Len())
			if !concurrent {
				baseline = metrics
			} else {
				finished := <-done
				joined = true
				if finished.err != nil || finished.pass.Progress.Loaded != 1 || finished.pass.Progress.Writes.Inserted == 0 {
					t.Fatal("real verified parse/index installation did not complete", finished)
				}
				sourceReturned := <-returned
				overlap := min(captureEnd.UnixNano(), finished.finished.UnixNano()) - max(captureStart.UnixNano(), sourceReturned.UnixNano())
				if overlap <= 0 {
					t.Fatal("capture did not overlap the source-return-to-install interval")
				}
				t.Logf("real source-return-to-install=%s overlap_with_sync=%s index_requests=%+v index_progress=%+v", finished.finished.Sub(sourceReturned), time.Duration(overlap), indexingReads.Metrics(), finished.pass.Progress)
				if baseline.Gets != metrics.Gets || baseline.Lists != metrics.Lists {
					t.Fatal("indexing changed capture request counts", baseline, metrics)
				}
				queryCode, doc, diag := searchJSONRun(t, env, "indexwork", "--all-projects")
				if queryCode != 0 || len(doc.Results) != 1 || doc.Results[0].Session != first.ArchiveSessionID {
					t.Fatal("verified installed search result missing", queryCode, doc, diag)
				}
			}
			key, err := archive.MetadataObjectKey("codex", second.ArchiveSessionID)
			must(t, err)
			data, err := objects.Get(t.Context(), key)
			must(t, err)
			var metadata archive.Metadata
			must(t, json.Unmarshal(data, &metadata))
			if metadata.SourceBundle.SHA256 == original.SourceBundle.SHA256 {
				t.Fatal("sync did not publish the changed second source")
			}
			source, err := objects.Get(t.Context(), metadata.SourceBundle.Key)
			must(t, err)
			if storage.SHA256Hex(source) != metadata.SourceBundle.SHA256 {
				t.Fatal("capture publication is not verified")
			}
		})
	}
}
