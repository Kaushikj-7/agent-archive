package config

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestNativeReplayFenceComposesAndRemainsOptIn(t *testing.T) {
	for _, imports := range []bool{false, true} {
		for _, deletion := range []bool{false, true} {
			cfg := Config{MachineID: "synthetic", RetentionDays: 90, NativeReplayProtection: true, NativeImportProtection: imports, DeletionProtection: deletion, DurableStorageProtection: deletion, SourceSetPublicationProtection: deletion}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var got Config
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if !got.NativeReplayProtection || !got.NativeAdmissionProtection || !got.NativeAuthorityProtection || got.NativeImportProtection != imports || got.MuseCapture != nil || got.Archive.Enabled {
				t.Fatal("replay fence altered permission")
			}
			var previous Config
			PreserveWriterFence(&previous, got)
			if !previous.NativeReplayProtection || !previous.NativeAdmissionProtection {
				t.Fatal("rollback lost replay floor")
			}
			if _, err := json.Marshal(previous); err != nil {
				t.Fatal(err)
			}
			rawOld := bytes.Replace(raw, []byte("muse-root-admission-replay"), []byte("muse-root-admission"), 1)
			if json.Unmarshal(rawOld, &got) == nil {
				t.Fatal("weaker selector accepted replay authority")
			}
		}
	}
	raw, err := json.Marshal(Config{RetentionDays: 90, NativeAdmissionProtection: true})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("replay")) {
		t.Fatal("ordinary native protection autoenabled replay")
	}
}

func TestNativeReplayFenceRefusesAmbiguousSelectors(t *testing.T) {
	raw, err := json.Marshal(Config{RetentionDays: 90, NativeReplayProtection: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{`"native_replay_protection":true`, `"native_replay_protection":false`},
		{`"native_replay_protection":true`, `"native_replay_protection":null`},
		{`"native_replay_protection":true`, `"NATIVE_REPLAY_PROTECTION":true`},
		{`"native_replay_protection":true`, `"native_replay_protection":true,"native_replay_protection":true`},
		{`"native_admission_protection":true`, `"native_admission_protection":false`},
		{`"muse-root-admission-replay"`, `"future"`},
	} {
		changed := bytes.Replace(raw, []byte(pair[0]), []byte(pair[1]), 1)
		if bytes.Equal(raw, changed) {
			t.Fatal("inert mutation")
		}
		var got Config
		if json.Unmarshal(changed, &got) == nil {
			t.Fatal("ambiguous replay authority accepted")
		}
	}
}
