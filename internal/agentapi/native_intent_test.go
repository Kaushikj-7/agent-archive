package agentapi

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func nativeIntentShapeFixture() AdmissionIntent {
	return AdmissionIntent{Version: 2, Harness: "muse", NativeMode: NativeStartupCandidate, NativeSessionID: "opaque-root", ProjectRoot: "/synthetic/project", ObservedCwd: "/synthetic/project", DestinationID: strings.Repeat("a", 64), ObservedAt: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), Event: "SessionStart", NativeAdmission: &archive.NativeAdmission{Contract: "muse-root-admission", HookContract: "muse-root-hooks-v1", RootFingerprint: strings.Repeat("b", 64), PolicyFingerprint: strings.Repeat("c", 64), WindowGeneration: strings.Repeat("d", 32)}}
}

func TestNativeIntentShapeAndWireBoundsPreserveLegacy(t *testing.T) {
	t.Parallel()
	valid := nativeIntentShapeFixture()
	if !ValidNativeAdmissionIntentShape(valid) {
		t.Fatal("valid private startup refused")
	}
	for _, mutate := range []func(*AdmissionIntent){
		func(i *AdmissionIntent) { i.ProjectRoot = "relative" },
		func(i *AdmissionIntent) { i.ProjectRoot = "/" + strings.Repeat("a", 4096) },
		func(i *AdmissionIntent) { i.NativeOwnerID = "unproven-owner" },
		func(i *AdmissionIntent) {
			if err := json.Unmarshal([]byte(`"future"`), &i.NativeMode); err != nil {
				t.Fatal(err)
			}
		},
		func(i *AdmissionIntent) {
			i.ProjectRoot = "/" + strings.Repeat("\x01", 4094)
			i.ObservedCwd = i.ProjectRoot
		},
	} {
		next := valid
		mutate(&next)
		if ValidNativeAdmissionIntentShape(next) {
			t.Fatal("unsupported or oversized private shape accepted")
		}
		raw, err := json.Marshal(next)
		if err != nil {
			t.Fatal(err)
		}
		var decoded AdmissionIntent
		if json.Unmarshal(raw, &decoded) == nil {
			t.Fatal("unsupported private wire accepted")
		}
	}
	for _, version := range []int{0, 1} {
		legacy := AdmissionIntent{Version: version, Harness: "claude", Event: "SessionStart", NativeSessionID: "legacy", ProjectRoot: "/" + strings.Repeat("p", MaxNativeAdmissionIntentBytes+1)}
		raw, err := json.Marshal(legacy)
		if err != nil {
			t.Fatal(err)
		}
		var decoded AdmissionIntent
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal("legacy limit changed", err)
		}
		again, err := json.Marshal(decoded)
		if err != nil || !bytes.Equal(raw, again) {
			t.Fatalf("legacy wire changed: %v", err)
		}
		if bytes.Contains(raw, []byte("native_window_generation")) || bytes.Contains(raw, []byte("NativeHookContract")) {
			t.Fatal("native fields leaked into legacy wire")
		}
	}
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	padded := append([]byte(nil), raw...)
	padded = append(padded, bytes.Repeat([]byte(" "), MaxNativeAdmissionIntentBytes)...)
	var decoded AdmissionIntent
	// The queue reader enforces the raw bound before encoding/json strips whitespace.
	if decoded.UnmarshalJSON(padded) == nil {
		t.Fatal("oversized private record accepted")
	}
}

func FuzzNativeAdmissionIntentWire(f *testing.F) {
	raw, err := json.Marshal(nativeIntentShapeFixture())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(raw)
	f.Add([]byte(`{"version":2,"native_mode":"future"}`))
	f.Add([]byte(`{"version":2,"Version":1}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			return
		}
		var intent AdmissionIntent
		if err := json.Unmarshal(raw, &intent); err != nil {
			return
		}
		if intent.Version == 2 && !ValidNativeAdmissionIntentShape(intent) {
			t.Fatal("decoder accepted unsupported private authority")
		}
		if intent.Version == 2 {
			canonical, err := json.MarshalIndent(intent, "", "  ")
			if err != nil || len(canonical)+1 > MaxNativeAdmissionIntentBytes {
				t.Fatal("decoded private record exceeds bound", err)
			}
		}
	})
}
