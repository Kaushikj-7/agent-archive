package agentapi

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func nativeReplayTestWire(tb testing.TB, replay string) []byte {
	tb.Helper()
	raw, err := json.Marshal(nativeIntentShapeFixture())
	if err != nil {
		tb.Fatal(err)
	}
	if replay == "" {
		return raw
	}
	return append(append(raw[:len(raw)-1], []byte(`,"replay":`)...), append([]byte(replay), '}')...)
}

func TestNativeStartupReplayWireIsClosedAndNormalized(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		wire   string
		valid  bool
		marker *archive.Replay
	}{
		{"", true, nil}, {`{}`, true, &archive.Replay{}}, {`{"run_id":"run-1:a.b_2"}`, true, &archive.Replay{RunID: "run-1:a.b_2"}},
		{`null`, false, nil}, {`[]`, false, nil}, {`"raw"`, false, nil}, {`{"run_id":null}`, false, nil}, {`{"run_id":""}`, false, nil},
		{`{"run_id":" run "}`, false, nil}, {`{"run_id":"é"}`, false, nil}, {`{"run_id":1}`, false, nil},
		{`{"run_id":"one","run_id":"two"}`, false, nil}, {`{"run_id":"one","Run_ID":"two"}`, false, nil},
		{`{"Run_id":"one"}`, false, nil}, {`{"unknown":true}`, false, nil}, {`{"run_id":"one","extra":{}}`, false, nil},
		{`{"run_id":"` + strings.Repeat("a", 129) + `"}`, false, nil},
	} {
		var actual AdmissionIntent
		err := json.Unmarshal(nativeReplayTestWire(t, test.wire), &actual)
		if (err == nil) != test.valid {
			t.Fatalf("wire %q acceptance: %v", test.wire, err)
		}
		if err != nil {
			continue
		}
		if (actual.Replay == nil) != (test.marker == nil) || actual.Replay != nil && *actual.Replay != *test.marker {
			t.Fatalf("normalized wire %q", test.wire)
		}
	}
	for _, marker := range []*archive.Replay{{}, {RunID: "run-1"}} {
		intent := nativeContinuationShapeFixture()
		intent.Replay = marker
		raw, err := json.Marshal(intent)
		if err != nil {
			t.Fatal(err)
		}
		var decoded AdmissionIntent
		if ValidNativeAdmissionIntentShape(intent) || json.Unmarshal(raw, &decoded) == nil {
			t.Fatal("continuation accepted replay relabel")
		}
	}
	ordinary := nativeReplayTestWire(t, "")
	var parsed AdmissionIntent
	if err := json.Unmarshal(ordinary, &parsed); err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(parsed)
	if err != nil || !bytes.Equal(ordinary, again) {
		t.Fatal("ordinary startup wire changed", err)
	}
	for _, version := range []int{0, 1} {
		intent := AdmissionIntent{Version: version, Harness: "claude", Replay: &archive.Replay{RunID: "legacy"}}
		raw, err := json.Marshal(intent)
		if err != nil {
			t.Fatal(err)
		}
		var decoded AdmissionIntent
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal("legacy replay changed", err)
		}
		again, err := json.Marshal(decoded)
		if err != nil || !bytes.Equal(raw, again) {
			t.Fatal("legacy bytes changed", err)
		}
	}
}

func FuzzNativeReplayIntentWire(f *testing.F) {
	for _, seed := range []string{"", `{}`, `{"run_id":"run-1"}`, `{"run_id":null}`, `{"run_id":"a","run_id":"b"}`, `{"Run_id":"a"}`} {
		f.Add(nativeReplayTestWire(f, seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > MaxNativeAdmissionIntentBytes+1 {
			return
		}
		var intent AdmissionIntent
		if json.Unmarshal(raw, &intent) != nil {
			return
		}
		if intent.Version != 2 {
			return
		}
		if !ValidNativeAdmissionIntentShape(intent) {
			t.Fatal("accepted unsupported shape")
		}
		if intent.Replay != nil && (intent.NativeMode != NativeStartupCandidate || intent.Replay.RunID != "" && !archive.IsReplayRunID(intent.Replay.RunID)) {
			t.Fatal("accepted unnormalized provenance")
		}
		canonical, err := json.Marshal(intent)
		if err != nil {
			t.Fatal(err)
		}
		var roundtrip AdmissionIntent
		if json.Unmarshal(canonical, &roundtrip) != nil {
			t.Fatal("accepted value cannot roundtrip")
		}
	})
}
