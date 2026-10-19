package setupjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/config"
)

func nativeImportJournal(t *testing.T, managed bool) Journal {
	t.Helper()
	j, _, _ := nativeJournalFixture(t)
	for _, target := range []*[]byte{&j.Changes[0].Before, &j.Changes[0].After} {
		var cfg config.Config
		must(t, json.Unmarshal(*target, &cfg))
		cfg.NativeImportProtection = true
		encoded, err := json.Marshal(cfg)
		must(t, err)
		*target = encoded
	}
	if managed {
		j.Jobs = []ManagedJob{{Role: SearchRole, Backend: "launchd", Ref: "com.agent-archive.search", Definition: "/synthetic/Library/LaunchAgents/com.agent-archive.search.plist", Intent: Keep}}
	}
	return j
}

func TestNativeImportJournalBothWireFormsPreserveExactFence(t *testing.T) {
	for _, managed := range []bool{false, true} {
		for _, filesOnly := range []bool{false, true} {
			j := nativeImportJournal(t, managed)
			j.FilesOnly = filesOnly
			raw, err := json.Marshal(j)
			must(t, err)
			var restored Journal
			must(t, json.Unmarshal(raw, &restored))
			if !restored.NativeImportProtection || !restored.NativeAuthorityProtection || restored.NativeAdmissionProtection || restored.FilesOnly != filesOnly || !bytes.Equal(restored.Changes[0].Before, j.Changes[0].Before) {
				t.Fatal("historical recovery authority or snapshot changed", string(raw))
			}
			roundtrip, err := json.Marshal(restored)
			must(t, err)
			if !bytes.Equal(raw, roundtrip) {
				t.Fatal("unstable recovery fence")
			}
			// The native16 predecessor has no import capability, even though it knows
			// the surrounding files_only object and both other native members.
			var doc map[string]json.RawMessage
			must(t, json.Unmarshal(raw, &doc))
			var oldFence struct {
				Version   int    `json:"version"`
				Writer    string `json:"writer"`
				Value     bool   `json:"value"`
				Admission string `json:"admission,omitempty"`
			}
			decoder := json.NewDecoder(bytes.NewReader(doc["files_only"]))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&oldFence) == nil {
				t.Fatal("pre-import recovery accepted import authority")
			}
		}
	}
}

func TestNativeImportJournalRefusesMalformedAuthorityWithoutReplacingReceiver(t *testing.T) {
	for _, managed := range []bool{false, true} {
		raw, err := json.Marshal(nativeImportJournal(t, managed))
		must(t, err)
		valid := string(raw)
		for _, changed := range []string{
			strings.Replace(valid, `"import":"muse-historical-import"`, `"import":"future"`, 1),
			strings.Replace(valid, `"import":"muse-historical-import"`, `"import":null`, 1),
			strings.Replace(valid, `"import":"muse-historical-import"`, `"IMPORT":"muse-historical-import"`, 1),
			strings.Replace(valid, `"import":"muse-historical-import"`, `"import":"muse-historical-import","import":"muse-historical-import"`, 1),
			strings.Replace(valid, `,"import":"muse-historical-import"`, ``, 1),
			strings.Replace(valid, `"native_import_protection":true`, `"native_import_protection":false`, 1),
			strings.Replace(valid, `"native_import_protection":true`, `"native_import_protection":null`, 1),
			strings.Replace(valid, `"native_import_protection":true`, `"NATIVE_IMPORT_PROTECTION":true`, 1),
			strings.Replace(valid, `"native_import_protection":true`, `"native_import_protection":true,"native_import_protection":true`, 1),
		} {
			if changed == valid {
				t.Fatal("inert mutation")
			}
			restored := Journal{Backend: "unchanged"}
			err := json.Unmarshal([]byte(changed), &restored)
			if !errors.Is(err, ErrUnsupportedRecoveryAuthority) || restored.Backend != "unchanged" {
				t.Fatal("malformed import authority was not preserved", err, changed)
			}
		}
	}
}
