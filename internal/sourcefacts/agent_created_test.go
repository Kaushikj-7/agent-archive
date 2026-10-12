package sourcefacts

import (
	"strings"
	"testing"
)

// An agent-created thread's header yields its identity and a native format,
// so the rollout lookup inspects it rather than refusing it every pass.
func TestCodexAgentCreatedThreadHeaderCarriesIdentity(t *testing.T) {
	t.Parallel()
	m := map[string]any{"id": testID, "session_id": testID, "timestamp": "2026-10-01T12:00:00Z", "cwd": "/included", "source": "vscode", "originator": "Codex Desktop", "cli_version": "0.160.0", "history_mode": "paginated", "thread_source": "agent_created_thread"}
	h := ReadCodexHeader(strings.NewReader(testRecords(t, m, nil)), "rollout-2026-10-01T12-00-00-"+testID+".jsonl")
	if h.Outcome != "native_format" || h.Identity == nil || h.Identity.ThreadID != testID || h.Identity.Child || h.Profile != CodexPaginatedJSONL {
		t.Fatalf("outcome %s identity %+v profile %s", h.Outcome, h.Identity, h.Profile)
	}
}
