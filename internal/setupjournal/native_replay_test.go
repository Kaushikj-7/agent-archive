package setupjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

func TestNativeReplayJournalBothFormsPreserveStrongCapability(t *testing.T) {
	for _, managed := range []bool{false, true} {
		j := nativeImportJournal(t, managed)
		// Only the before snapshot is stronger: rollback must preserve that floor.
		var cfg config.Config
		must(t, json.Unmarshal(j.Changes[0].Before, &cfg))
		cfg.NativeReplayProtection = true
		var err error
		j.Changes[0].Before, err = json.Marshal(cfg)
		must(t, err)
		raw, err := json.Marshal(j)
		must(t, err)
		var got Journal
		must(t, json.Unmarshal(raw, &got))
		if !got.NativeReplayProtection || !got.NativeAdmissionProtection || !got.NativeImportProtection || !bytes.Equal(got.Changes[0].Before, j.Changes[0].Before) {
			t.Fatal("journal lost stronger snapshot capability")
		}
		for _, pair := range [][2]string{
			{`muse-root-admission-replay`, `muse-root-admission`},
			{`"native_replay_protection":true`, `"native_replay_protection":false`},
			{`"native_replay_protection":true`, `"native_replay_protection":null`},
			{`"native_replay_protection":true`, `"NATIVE_REPLAY_PROTECTION":true`},
			{`"native_replay_protection":true`, `"native_replay_protection":true,"native_replay_protection":true`},
		} {
			changed := bytes.Replace(raw, []byte(pair[0]), []byte(pair[1]), 1)
			if bytes.Equal(raw, changed) {
				t.Fatal("inert mutation")
			}
			if err := json.Unmarshal(changed, &got); !errors.Is(err, ErrUnsupportedRecoveryAuthority) {
				t.Fatal("strong recovery authority not preserved", err)
			}
		}
	}
}
