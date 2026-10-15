package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// setupSearchProducer uses actual setup and hook/sync commands, with every
// external effect injected into private fixtures. It grants no background
// indexing consent: the acceptance scenario explicitly requests manual refresh.
func setupSearchProducer(t *testing.T, objects storage.ObjectStore, needle string, at time.Time) (Env, string) {
	t.Helper()
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return objects, nil }
	env.AWSProfiles = func() ([]AWSProfile, error) {
		return []AWSProfile{{Name: "synthetic-search", Region: "us-east-1"}}, nil
	}
	setupYes(t, env, "", 0, "--yes", "--provider", "s3", "--bucket", "synthetic-search", "--aws-profile", "synthetic-search", "--project", project, "--apps", "codex", "--codex-discovery", "off", "--codex-capture-scope", "included-projects")
	transcript := writeCodexTranscript(t, project)
	record, err := json.Marshal(map[string]any{"type": "response_item", "id": "m1", "payload": map[string]any{"type": "message", "role": "assistant", "content": needle + " synthetic setup publication λ"}})
	must(t, err)
	must(t, os.WriteFile(transcript, append([]byte("{\"type\":\"turn_context\",\"model\":\"gpt-test\"}\n"), record...), 0600))
	payload, err := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-" + needle, "cwd": project, "transcript_path": transcript})
	must(t, err)
	var out, diagnostics bytes.Buffer
	if code := Run([]string{"_hook", "--harness", "codex"}, bytes.NewReader(payload), &out, &diagnostics, env); code != 0 {
		t.Fatalf("hook%d %s", code, &diagnostics)
	}
	out.Reset()
	if code := Run([]string{"sync"}, nil, &out, &diagnostics, env); code != 0 {
		t.Fatalf("publication%d %s", code, &diagnostics)
	}
	local, err := state.Open(home)
	must(t, err)
	registrations, err := local.LoadRegistrations()
	must(t, err)
	if len(registrations) != 1 {
		t.Fatalf("hook registrations: %+v", registrations)
	}
	id := registrations[0].ArchiveSessionID
	key, err := archive.MetadataObjectKey("codex", id)
	must(t, err)
	data, err := objects.Get(t.Context(), key)
	must(t, err)
	var metadata archive.Metadata
	must(t, json.Unmarshal(data, &metadata))
	source, err := objects.Get(t.Context(), metadata.SourceBundle.Key)
	must(t, err)
	if storage.SHA256Hex(source) != metadata.SourceBundle.SHA256 {
		t.Fatal("published source differs from verified metadata pointer")
	}
	return env, id
}

// The catalog case uses the existing qualified synthetic catalog projection of
// verified legacy publications, not a live migration/provider qualification.
// Every Run reopens persisted search state; no process-local index is shared.
func TestSearchActualSetupPublicationAndMissedProducerCatchup(t *testing.T) {
	for _, mode := range []string{"legacy", "qualified-catalog"} {
		t.Run(mode, func(t *testing.T) {
			objects := storagetest.NewMemoryStore()
			first, firstID := setupSearchProducer(t, objects, "visible", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
			code, initial, diag := searchJSONRun(t, first, "visible", "--all-projects", "--refresh")
			if code != 0 || len(initial.Results) != 1 || initial.Results[0].Session != firstID || initial.Results[0].Reference == nil {
				t.Fatalf("initial%d %+v %s", code, initial, diag)
			}
			// A second installation publishes after receiver indexing. No hint is sent
			// to the receiver; its local cached query must remain network-free/partial.
			_, secondID := setupSearchProducer(t, objects, "remoteproof", first.now().Add(time.Minute))
			cached := first
			cached.OpenStore = func(config.Config) (storage.ObjectStore, error) {
				t.Error("cached query fetched archive")
				return nil, errors.New("offline")
			}
			code, missed, diag := searchJSONRun(t, cached, "remoteproof", "--all-projects")
			if code != 0 || len(missed.Results) != 0 {
				t.Fatalf("unobserved producer%d %+v %s", code, missed, diag)
			}
			if mode == "qualified-catalog" {
				remote := privateCatalogFromLegacy(t, objects)
				first.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
			}
			code, caught, diag := searchJSONRun(t, first, "remoteproof", "--all-projects", "--refresh")
			if code != 0 || len(caught.Results) != 1 || caught.Results[0].Session != secondID || caught.Results[0].Reference == nil {
				t.Fatalf("catchup%d %+v %s", code, caught, diag)
			}
			code, context, diag := searchJSONRun(t, first, "--read", *caught.Results[0].Reference, "--max-bytes", "4096", "--before", "1", "--after", "1")
			if code != 0 || context.Context == nil || !strings.Contains(context.Context.Selected.Text, "remoteproof") {
				t.Fatalf("verified selected context%d %+v %s", code, context, diag)
			}
			// A fresh invocation reopens the same index without archive availability.
			code, restarted, diag := searchJSONRun(t, cached, "remoteproof", "--all-projects")
			if code != 0 || len(restarted.Results) != 1 || restarted.Results[0].Session != secondID || restarted.Results[0].Reference == nil || *restarted.Results[0].Reference != *caught.Results[0].Reference {
				t.Fatalf("persisted restart%d %+v %s", code, restarted, diag)
			}
		})
	}
}
