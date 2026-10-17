package collector

import (
	"bytes"
	"context"
	"errors"
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

func TestStartupCandidateAcknowledgementHonorsCancellationAndExactEpoch(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	intent := agentapi.AdmissionIntent{Version: 2, Harness: "muse", NativeMode: agentapi.NativeStartupCandidate, NativeSessionID: "opaque-root", ProjectRoot: "/synthetic/project", ObservedCwd: "/synthetic/project", DestinationID: strings.Repeat("a", 64), ObservedAt: time.Now().UTC(), Event: "SessionStart", NativeAdmission: &archive.NativeAdmission{Contract: "muse-root-admission", HookContract: "muse-root-hooks-v1", RootFingerprint: strings.Repeat("b", 64), PolicyFingerprint: strings.Repeat("c", 64), WindowGeneration: strings.Repeat("d", 32)}}
	candidate := startupCandidate{path: filepath.Join(home, "admission-intents", "startup.json"), intent: intent}
	if err := local.Write(candidate.path, intent); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(candidate.path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p := pass{ctx: ctx, local: store}
	if err := p.ackStartupCandidate(candidate); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(candidate.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("canceled startup acknowledgement changed queue", err)
	}
	changed := intent
	proof := *intent.NativeAdmission
	proof.WindowGeneration = strings.Repeat("e", 32)
	changed.NativeAdmission = &proof
	if err := local.Write(candidate.path, changed); err != nil {
		t.Fatal(err)
	}
	before, err = os.ReadFile(candidate.path)
	if err != nil {
		t.Fatal(err)
	}
	p.ctx = t.Context()
	if err := p.ackStartupCandidate(candidate); err == nil {
		t.Fatal("replaced startup epoch acknowledged")
	}
	after, err = os.ReadFile(candidate.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("replaced startup epoch removed", err)
	}
	candidate.intent = changed
	if err := p.ackStartupCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(candidate.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exact startup retained", err)
	}
}
