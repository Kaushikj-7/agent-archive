package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
	"github.com/wangjohn/agent-archive/internal/state"
)

func TestStartupCandidateAcknowledgementBindsReplayMarker(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		old  *archive.Replay
		next *archive.Replay
	}{
		{"ordinary_to_replay", nil, &archive.Replay{RunID: "run"}},
		{"replay_to_ordinary", &archive.Replay{RunID: "run"}, nil},
		{"marker_to_id", &archive.Replay{}, &archive.Replay{RunID: "run"}},
		{"id_to_marker", &archive.Replay{RunID: "run"}, &archive.Replay{}},
		{"different_id", &archive.Replay{RunID: "one"}, &archive.Replay{RunID: "two"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			store, err := state.Open(home)
			if err != nil {
				t.Fatal(err)
			}
			intent := agentapi.AdmissionIntent{Version: 2, Harness: "muse", NativeMode: agentapi.NativeStartupCandidate, NativeSessionID: "opaque-root", ProjectRoot: "/synthetic/project", ObservedCwd: "/synthetic/project", DestinationID: strings.Repeat("a", 64), ObservedAt: time.Now().UTC(), Event: "SessionStart", NativeAdmission: &archive.NativeAdmission{Contract: "muse-root-admission", HookContract: "muse-root-hooks-v1", RootFingerprint: strings.Repeat("b", 64), PolicyFingerprint: strings.Repeat("c", 64), WindowGeneration: strings.Repeat("d", 32)}, Replay: test.old}
			candidate := startupCandidate{path: filepath.Join(home, "admission-intents", "startup.json"), intent: intent}
			replacement := intent
			replacement.Replay = test.next
			if err := local.Write(candidate.path, replacement); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(candidate.path)
			if err != nil {
				t.Fatal(err)
			}
			p := pass{ctx: context.Background(), local: store}
			if err := p.ackStartupCandidate(candidate); err == nil {
				t.Fatal("marker-only replacement acknowledged")
			}
			after, err := os.ReadFile(candidate.path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("changed marker queue mutated", err)
			}
			// Canonical whole-intent equality still permits the exact same marker.
			var exact agentapi.AdmissionIntent
			if err := json.Unmarshal(before, &exact); err != nil {
				t.Fatal(err)
			}
			candidate.intent = exact
			if err := p.ackStartupCandidate(candidate); err != nil {
				t.Fatal("exact marker not acknowledged", err)
			}
			if _, err := os.Stat(candidate.path); !os.IsNotExist(err) {
				t.Fatal("exact queue record retained", err)
			}
		})
	}
}
