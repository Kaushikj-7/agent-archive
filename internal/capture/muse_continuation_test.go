package capture

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
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/local"
)

func TestMuseOwnerAcknowledgementKeepsReplacedEpoch(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(admissionIntentDir(home), "observation.json")
	intent := agentapi.AdmissionIntent{Version: 2, Harness: "muse", NativeMode: agentapi.NativeOwnerContinuation, NativeOwnerID: strings.Repeat("a", 32), NativeWindowGeneration: strings.Repeat("b", 32), NativeSessionID: "opaque-root", ProjectRoot: "/synthetic/project", ObservedCwd: "/synthetic/project", DestinationID: strings.Repeat("c", 64), ObservedAt: time.Now().UTC(), Event: "Stop", NativeAdmission: &archive.NativeAdmission{Contract: "muse-root-admission", HookContract: "muse-root-hooks-v1", RootFingerprint: strings.Repeat("d", 64), PolicyFingerprint: strings.Repeat("e", 64), WindowGeneration: strings.Repeat("f", 32)}, Effects: []agentapi.ReplayEffect{{Event: agentapi.LifecycleEvent{Kind: agentapi.EventStop, Session: agentapi.NativeSession{Agent: agentmeta.Muse, NativeID: "opaque-root"}, ProjectRoot: "/synthetic/project", NativeHookContract: "muse-root-hooks-v1", NativeEvent: "Stop", Reason: "native_root_observation"}}}}
	if !RecognizedAdmissionIntent(intent) {
		t.Fatal("invalid fixture")
	}
	changed := intent
	changed.NativeWindowGeneration = strings.Repeat("0", 32)
	if err := local.Write(path, changed); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	acknowledgements := 0
	after := func(effect effectName) error {
		if effect != effectIntentAck {
			t.Fatalf("unexpected effect %v", effect)
		}
		acknowledgements++
		return nil
	}
	if err := ackMuseOwnerIntent(t.Context(), home, path, intent, after); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, current) || acknowledgements != 0 {
		t.Fatal("replaced epoch acknowledged", err, acknowledgements)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ackMuseOwnerIntent(ctx, home, path, changed, after); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	current, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(before, current) {
		t.Fatal("canceled acknowledgement changed queue", err)
	}
	if err := ackMuseOwnerIntent(t.Context(), home, path, changed, after); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || acknowledgements != 1 {
		t.Fatal("exact record not acknowledged", err, acknowledgements)
	}
}
