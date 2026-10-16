package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// Recorded publication verification survives unavailable discovery. Publication
// and health use real local producers and isolated MemoryStore readback; the
// result does not establish current native capture health or provider acceptance.
func TestStatusRecordedArchiveWithProducedUnavailableDiscovery(t *testing.T) {
	for _, verified := range []bool{false, true} {
		name := "published-without-readback"
		if verified {
			name = "verified-at-recorded-time"
		}
		t.Run(name, func(t *testing.T) {
			canonical := func() string {
				dir, err := filepath.EvalSymlinks(t.TempDir())
				must(t, err)
				must(t, os.Chmod(dir, 0700))
				return dir
			}
			home, userHome, project, codex := canonical(), canonical(), canonical(), canonical()
			for _, dir := range []string{"sessions", "archived_sessions"} {
				must(t, os.Mkdir(filepath.Join(codex, dir), 0700))
			}
			now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			cfg := pairTestConfig(now, []string{"codex"}, project)
			cfg.Discovery = &config.DiscoveryConfig{Enabled: true, CodexHomes: []string{codex}}
			must(t, config.ReconcileDiscovery(&cfg, config.Config{}, now.Add(-time.Hour)))
			must(t, config.Save(home, cfg))
			store, err := state.Open(home)
			must(t, err)
			remote := storagetest.NewMemoryStore()
			const id = "recorded-archive"
			publishPairSession(t, home, store, remote, cfg, now, id, project, verified)
			reg, found, err := store.LoadRegistration(id)
			must(t, err)
			if !found || reg.Imported() || !cfg.AcceptSession(reg) || reg.ProjectRoot != project {
				t.Fatalf("automatic publication owner premise: %+v found=%v", reg, found)
			}
			_, publishedAt, found, err := store.LoadLastPublished(id)
			must(t, err)
			if !found || !publishedAt.Equal(now) {
				t.Fatalf("publication time: found=%v at=%v", found, publishedAt)
			}
			source, found, err := store.LoadLastPublishedSource(id)
			must(t, err)
			if !found {
				t.Fatal("published source reference absent")
			}
			sourceBytes, err := remote.Get(context.Background(), source.Key)
			must(t, err)
			if len(sourceBytes) == 0 || len(sourceBytes) != source.CompressedBytes || storage.SHA256Hex(sourceBytes) != source.SHA256 {
				t.Fatal("actual published source bytes do not match retained reference")
			}
			published, err := store.LoadPublishedState(id)
			must(t, err)
			metadataKey, err := archive.MetadataObjectKey(reg.Harness.Name, id)
			must(t, err)
			metadataBytes, err := remote.Get(context.Background(), metadataKey)
			must(t, err)
			if len(metadataBytes) == 0 || !bytes.Equal(metadataBytes, published.Metadata()) {
				t.Fatal("actual remote metadata does not match published bytes")
			}
			var metadata archive.Metadata
			must(t, json.Unmarshal(metadataBytes, &metadata))
			if metadata.SourceBundle != source {
				t.Fatal("actual metadata source reference differs")
			}
			if verified {
				evidence, err := readVerification(home, id)
				must(t, err)
				if evidence.ConfigurationID != sessionVerificationConfigurationID(cfg, reg) || !evidence.PublishedAt.Equal(publishedAt) || !evidence.VerifiedAt.Equal(now) || evidence.SourceSHA256 != source.SHA256 {
					t.Fatalf("readback binding: %+v", evidence)
				}
			}
			// Empty owned directories require only the finite observe/validate catalog
			// stages. Retry that scheduling progression at most four times, never a
			// provider failure, diagnostic, missing summary or arbitrary setup failure.
			runHealth := func(at time.Time) discovery.Health {
				health, err := discovery.Run(context.Background(), store, cfg, discovery.Options{Sources: productionAgents, Now: func() time.Time { return at }})
				must(t, err)
				stored, found, err := discovery.ReadHealth(home)
				must(t, err)
				producedJSON, err := json.Marshal(health)
				must(t, err)
				storedJSON, err := json.Marshal(stored)
				must(t, err)
				if !found || health.LastAttempt.IsZero() || !bytes.Equal(producedJSON, storedJSON) {
					t.Fatalf("actual health producer/readback premise: found=%v produced=%+v stored=%+v", found, health, stored)
				}
				return health
			}
			healthy := runHealth(now)
			for stage := 1; healthy.Pending && stage < 4; stage++ {
				if len(healthy.Errors) != 0 || healthy.Outcomes["source_unavailable"] != 0 {
					t.Fatalf("ordinary health unavailable: %+v", healthy)
				}
				healthy = runHealth(now.Add(time.Duration(stage) * time.Second))
			}
			if healthy.Pending || len(healthy.Errors) != 0 || healthy.Outcomes["source_unavailable"] != 0 || healthy.LastReconciled.IsZero() {
				t.Fatalf("ordinary healthy finite catalog premise: %+v", healthy)
			}
			env := pairStatusEnv(t, home, userHome, now, "codex")
			before, err := readStatus(env)
			must(t, err)
			if len(before.Apps) != 1 {
				t.Fatalf("configured app count: %+v", before.Apps)
			}
			app := before.Apps[0]
			wantVerified := 0
			if verified {
				wantVerified = 1
			}
			if app.Sessions != 1 || app.PublishedSessions != 1 || app.VerifiedSessions != wantVerified || app.ReadBackVerified != verified || app.Discovery == nil || app.Discovery.Pending {
				t.Fatalf("ordinary producer-backed status premise: %+v", app)
			}
			if verified && app.State != "published; source verified" {
				t.Fatalf("ordinary verified state: %q", app.State)
			}
			basic := renderStatus(before, false)
			if strings.Contains(basic, "capture status unknown") || !strings.Contains(basic, "automatic capture") {
				t.Fatalf("ordinary basic status: %s", basic)
			}
			// Snapshot actual persisted evidence and every isolated remote object. The
			// regular-file source-root intervention must alter none of them.
			saved := map[string][]byte{}
			for _, path := range []string{filepath.Join(home, "config.json"), filepath.Join(home, "registrations", id+".json"), filepath.Join(home, "published", id+".json")} {
				raw, err := os.ReadFile(path)
				must(t, err)
				saved[path] = raw
			}
			if verified {
				raw, err := os.ReadFile(verificationPath(home, id))
				must(t, err)
				saved[verificationPath(home, id)] = raw
			}
			request, requestFound, err := store.LoadRequest(id)
			must(t, err)
			objects, err := remote.List(context.Background(), "")
			must(t, err)
			if len(objects) < 2 {
				t.Fatalf("source/metadata publication objects absent: %+v", objects)
			}
			remoteBefore := map[string][]byte{}
			for _, object := range objects {
				raw, err := remote.Get(context.Background(), object.Key)
				must(t, err)
				remoteBefore[object.Key] = raw
			}
			retained := codex + "-retained"
			must(t, os.Rename(codex, retained))
			t.Cleanup(func() { must(t, os.Remove(codex)); must(t, os.Rename(retained, codex)) })
			must(t, os.WriteFile(codex, []byte("owned regular-file root intervention"), 0600))
			unavailable := runHealth(now.Add(10 * time.Second))
			for stage := 1; !slices.Contains(unavailable.Errors, "source_root_unavailable") && stage < 4; stage++ {
				if len(unavailable.Errors) != 0 {
					t.Fatalf("unexpected intervention diagnostic: %+v", unavailable)
				}
				unavailable = runHealth(now.Add(time.Duration(10+stage) * time.Second))
			}
			if !unavailable.Pending || !slices.Contains(unavailable.Errors, "source_root_unavailable") {
				t.Fatalf("ENOTDIR producer cause not witnessed: %+v", unavailable)
			}
			healthJSON, err := json.Marshal(unavailable)
			must(t, err)
			for _, private := range []string{codex, project, reg.NativeSessionID, "turn_context"} {
				if bytes.Contains(healthJSON, []byte(private)) {
					t.Fatalf("private health diagnostic: %s", healthJSON)
				}
			}
			after, err := readStatus(env)
			must(t, err)
			if len(after.Apps) != 1 {
				t.Fatal("unavailable app vanished")
			}
			later := after.Apps[0]
			if later.Sessions != app.Sessions || later.PublishedSessions != app.PublishedSessions || later.VerifiedSessions != app.VerifiedSessions || later.ReadBackVerified != app.ReadBackVerified || later.State != app.State || !later.VerifiedAt.Equal(app.VerifiedAt) || later.Discovery == nil || !slices.Contains(later.Discovery.Errors, "source_root_unavailable") {
				t.Fatalf("recorded evidence lost under produced unknown: before=%+v after=%+v", app, later)
			}
			basic = renderStatus(after, false)
			want := "capture status unknown; included projects"
			if verified {
				want = "discovery unknown; archive verified at recorded time; included projects"
			}
			row := (statusScreen{now: now}).appRow(later, 0)
			if row.cells[1] != want || strings.Contains(basic, "automatic capture") || !strings.Contains(basic, "Discovery health is unknown") {
				t.Fatalf("recorded verification status: %s", basic)
			}
			if !strings.Contains(appCounts(later), "1 archived") || (verified && !strings.Contains(appCounts(later), "1 verified")) {
				t.Fatalf("known archive counts: %s", appCounts(later))
			}
			verbose := renderVerboseStatus(after)
			if !strings.Contains(verbose, "Discovery health is unknown") || strings.Contains(verbose, "discovery unknown; archive verified at recorded time") {
				t.Fatalf("verbose discovery scope changed: %s", verbose)
			}
			var jsonOut bytes.Buffer
			if code := runStatusCommand([]string{"--json"}, &jsonOut, &jsonOut, env); code != 0 {
				t.Fatalf("JSON status exit%d: %s", code, &jsonOut)
			}
			var document statusView
			must(t, json.Unmarshal(jsonOut.Bytes(), &document))
			if len(document.Apps) != 1 || document.Apps[0].VerifiedSessions != wantVerified || document.Apps[0].ReadBackVerified != verified || !document.Apps[0].VerifiedAt.Equal(app.VerifiedAt) || document.Apps[0].Discovery == nil || !slices.Contains(document.Apps[0].Discovery.Errors, "source_root_unavailable") {
				t.Fatalf("JSON evidence separation: %s", &jsonOut)
			}
			for path, raw := range saved {
				actual, err := os.ReadFile(path)
				must(t, err)
				if !bytes.Equal(actual, raw) {
					t.Fatalf("retained evidence changed: %s", path)
				}
			}
			actualRequest, actualFound, err := store.LoadRequest(id)
			must(t, err)
			if actualFound != requestFound || !reflect.DeepEqual(actualRequest, request) {
				t.Fatal("request evidence changed")
			}
			afterObjects, err := remote.List(context.Background(), "")
			must(t, err)
			if !reflect.DeepEqual(afterObjects, objects) {
				t.Fatal("remote object listing changed")
			}
			for key, raw := range remoteBefore {
				actual, err := remote.Get(context.Background(), key)
				must(t, err)
				if !bytes.Equal(actual, raw) {
					t.Fatalf("remote evidence changed: %s", key)
				}
			}
		})
	}
}
