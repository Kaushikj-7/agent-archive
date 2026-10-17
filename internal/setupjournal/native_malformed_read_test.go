package setupjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestNativeProtectedMalformedJournalRecoverPreservesRecord(t *testing.T) {
	t.Parallel()
	for _, managed := range []bool{false, true} {
		name := "legacy"
		if managed {
			name = "managed"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for mutation, corrupt := range map[string]func([]byte) []byte{
				"before-selector":    func(raw []byte) []byte { return append([]byte(`{"broken":!,`), raw[1:]...) },
				"after-selector":     func(raw []byte) []byte { return append(bytes.Clone(raw), '!') },
				"truncated-selector": func([]byte) []byte { return []byte(`{"native_replay_protection":true,"changes":[`) },
			} {
				t.Run(mutation, func(t *testing.T) {
					t.Parallel()
					journal := nativeImportJournal(t, managed)
					target := journal.Changes[0].Path
					current := bytes.Clone(journal.Changes[0].After)
					must(t, os.WriteFile(target, current, 0600))
					raw, err := json.Marshal(journal)
					must(t, err)
					malformed := corrupt(raw)
					home := t.TempDir()
					path := JournalPath(home)
					must(t, os.WriteFile(path, malformed, 0600))
					err = Recover(home, nil, func() (func(), error) {
						t.Fatal("malformed journal reached collector/backend locks")
						return nil, errors.New("unexpected recovery")
					})
					if !errors.Is(err, ErrUnsupportedRecoveryAuthority) {
						t.Fatal("malformed protected recovery lost typed authority refusal", err)
					}
					got, readErr := os.ReadFile(path)
					if readErr != nil || !bytes.Equal(got, malformed) {
						t.Fatal("malformed protected recovery changed record", readErr)
					}
					got, readErr = os.ReadFile(target)
					if readErr != nil || !bytes.Equal(got, current) {
						t.Fatal("malformed protected recovery changed target", readErr)
					}
				})
			}
		})
	}
}
