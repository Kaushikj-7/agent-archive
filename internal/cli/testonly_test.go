package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/catalog"
	"github.com/wangjohn/agent-archive/internal/config"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// Helpers only tests use, kept out of the production files so deadcode
// (golang.org/x/tools/cmd/deadcode) reports only code that is really dead.

type browseReadDependencies interface {
	metadataCacheDependencies
	interrupts() (<-chan os.Signal, func())
}

// loadSessionsForBrowse lists metadata with the same filters list uses, for
// interactive show and handoff. It reads every match, newest activity first,
// for the caller to scope and limit.
func loadSessionsForBrowse(env browseReadDependencies, store storage.ObjectStore, opts listOptions, stderr io.Writer, command string) ([]archive.Metadata, error) {
	ctx, stopReads := readCommandContext(env)
	defer stopReads()
	return loadSessionsForBrowseContext(catalog.WithReadView(ctx), env, store, opts, stderr, command)
}

func installedVersionSupport(discovery applicationDiscovery, verifiedVersions []string) string {
	state, _ := installedVersionSupportDetail(discovery, verifiedVersions)
	return state
}

// pendingSessions counts every accepted session state.Outstanding reports
// as pending, a session waiting for its transcript included: status reports
// it as pending, because from the user's side it is.
func pendingSessions(home string, cfg config.Config) (int, error) {
	blocking, waiting, err := pendingSessionCounts(home, cfg)
	return blocking + waiting, err
}

// Verify only new publications or evidence invalidated by configuration changes.
// Status reads this local result; it never downloads conversation content.
//
// A failed read-back is recorded with a retry time and never surfaces as a
// pass error: the publication itself succeeded, so `sync` still exits 0 and
// Status.LastError stays free for genuine collection failures. Status reports
// the pending or failed verification from the record instead.
//
// Local state that cannot be read fails only its own session's read-back:
// an unreadable registration is skipped (the collector pass has already
// reported it), and a session whose published state or verification record
// cannot be read is left out and returned as an error once every other
// session has been handled.
func verifyPublications(home string, cfg config.Config, env Env, store *state.Store, remote storage.ObjectStore) (verificationSummary, error) {
	return verifyPublicationsWithin(context.Background(), home, cfg, env, store, remote)
}

func setUpTestConfig(t *testing.T, home, projectRoot string, activatedAt time.Time) {
	t.Helper()
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		MachineID: "machine-1",
		Storage:   credentialsTestConfig(),
		Archive: archive.Config{
			SchemaVersion: 1, MachineID: "machine-1", Enabled: true,
			Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(projectRoot), Root: projectRoot, Included: true, ActivatedAt: activatedAt}},
		},
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
}

// writeTestTranscript creates a transcript file with the given contents (empty
// for a conversation that has not started yet) and returns its path.
func writeTestTranscript(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// cursorDesktopPayload is a Cursor desktop hook payload in the shape observed
// on 3.21.13: every event carries conversation_id and session_id (the same
// value), cursor_version, workspace_roots, composer_mode, model, model_id, and
// model_params. transcript is the transcript_path value; nil sends JSON null.
func cursorDesktopPayload(event, conversation, project string, transcript any) map[string]any {
	return map[string]any{
		"hook_event_name": event, "conversation_id": conversation, "session_id": conversation,
		"cursor_version": "3.21.13", "workspace_roots": []any{project}, "composer_mode": "agent",
		"model": "synthetic-model", "model_id": "synthetic-model-id",
		"model_params":    []any{map[string]any{"id": "effort", "value": "high"}},
		"transcript_path": transcript,
	}
}

// cursorTranscriptLocation mirrors Cursor's own layout,
// ~/.cursor/projects/<workspace>/agent-transcripts/<id>/<id>.jsonl, under a
// temporary directory.
func cursorTranscriptLocation(t *testing.T, conversation string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "projects", "work-widget", "agent-transcripts", conversation)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, conversation+".jsonl")
}

func onlyCursorRegistration(t *testing.T, home string) archive.SessionRegistration {
	t.Helper()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations = %#v err=%v", regs, err)
	}
	return regs[0]
}
