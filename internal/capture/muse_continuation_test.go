package capture

import (
	"bytes"
	"context"
	"encoding/json"
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
	intent := museContinuationIntentFixture()
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

func museContinuationIntentFixture() agentapi.AdmissionIntent {
	return agentapi.AdmissionIntent{Version: 2, Harness: "muse", NativeMode: agentapi.NativeOwnerContinuation, NativeOwnerID: strings.Repeat("a", 32), NativeWindowGeneration: strings.Repeat("b", 32), NativeSessionID: "opaque-root", ProjectRoot: "/synthetic/project", ObservedCwd: "/synthetic/project", DestinationID: strings.Repeat("c", 64), ObservedAt: time.Now().UTC(), Event: "Stop", NativeAdmission: &archive.NativeAdmission{Contract: "muse-root-admission", HookContract: "muse-root-hooks-v1", RootFingerprint: strings.Repeat("d", 64), PolicyFingerprint: strings.Repeat("e", 64), WindowGeneration: strings.Repeat("f", 32)}, Effects: []agentapi.ReplayEffect{{Event: agentapi.LifecycleEvent{Kind: agentapi.EventStop, Session: agentapi.NativeSession{Agent: agentmeta.Muse, NativeID: "opaque-root"}, ProjectRoot: "/synthetic/project", NativeHookContract: "muse-root-hooks-v1", NativeEvent: "Stop", Reason: "native_root_observation"}}}}
}

func TestMuseIntentExpiryPreservesUnsupportedAndBoundary(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		startup bool
		age     time.Duration
		future  string
		removed bool
	}{
		{name: "startup_expired", startup: true, age: 24*time.Hour + time.Nanosecond, removed: true},
		{name: "continuation_expired", age: 24*time.Hour + time.Nanosecond, removed: true},
		{name: "startup_boundary", startup: true, age: 24 * time.Hour},
		{name: "continuation_boundary", age: 24 * time.Hour},
		{name: "future_clock", startup: true, age: -2 * time.Minute},
		{name: "future_version", startup: true, age: 25 * time.Hour, future: "version"},
		{name: "future_identity", startup: true, age: 25 * time.Hour, future: "contract"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home, project := t.TempDir(), t.TempDir()
			setUpTestConfig(t, home, project, at.Add(-48*time.Hour))
			intent := museContinuationIntentFixture()
			intent.ObservedAt = at.Add(-tc.age)
			if tc.startup {
				intent.NativeMode = agentapi.NativeStartupCandidate
				intent.NativeOwnerID = ""
				intent.NativeWindowGeneration = ""
				intent.Event = "SessionStart"
				intent.Effects = nil
			}
			if !RecognizedAdmissionIntent(intent) {
				t.Fatal("invalid fixture")
			}
			raw, err := json.Marshal(intent)
			if err != nil {
				t.Fatal(err)
			}
			if tc.future == "version" {
				raw = bytes.Replace(raw, []byte(`"version":2`), []byte(`"version":3`), 1)
			}
			if tc.future == "contract" {
				raw = bytes.Replace(raw, []byte(`muse-root-admission`), []byte(`future-root-admission`), 1)
			}
			path := filepath.Join(admissionIntentDir(home), "expiry.json")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := replayMuseOwnerIntents(t.Context(), home, at, func(effectName) error { t.Fatal("expiry attempted effects"); return nil }); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if tc.removed {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("expired recognized record retained", err)
				}
				return
			}
			if err != nil || !bytes.Equal(after, raw) {
				t.Fatal("pending or unsupported record changed", err)
			}
		})
	}
}

func TestMuseIntentExpiryCancellationPreservesQueue(t *testing.T) {
	t.Parallel()
	home, project := t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	setUpTestConfig(t, home, project, at.Add(-48*time.Hour))
	intent := museContinuationIntentFixture()
	intent.ObservedAt = at.Add(-25 * time.Hour)
	path := filepath.Join(admissionIntentDir(home), "canceled-expiry.json")
	if err := local.Write(path, intent); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := replayMuseOwnerIntents(ctx, home, at, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("canceled expiry mutated queue", err)
	}
}
