package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/fileapply"
	"github.com/wangjohn/agent-archive/internal/setupjournal"
)

func TestNativeProtectedMalformedJournalAbandonPreservesRecord(t *testing.T) {
	t.Parallel()
	for _, replay := range []bool{false, true} {
		for _, filesOnly := range []bool{false, true} {
			name := "import"
			if replay {
				name = "replay"
			}
			if filesOnly {
				name += "-files"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				for mutation, corrupt := range map[string]func([]byte) []byte{
					"trailing":         func(raw []byte) []byte { return append(bytes.Clone(raw), '!') },
					"before-selector":  func(raw []byte) []byte { return append([]byte(`{"broken":!,`), raw[1:]...) },
					"after-selector":   func(raw []byte) []byte { return append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"broken":!}`)...) },
					"escaped-selector": func([]byte) []byte { return []byte(`{"broken":!,"native\u005fimport_protection":true}`) },
					"alias-selector":   func([]byte) []byte { return []byte(`{"broken":!,"NATIVE_REPLAY_PROTECTION":true}`) },
					"fence-only": func([]byte) []byte {
						return []byte(`{"broken":!,"files_only":{"version":1,"writer":"native-authority-v16","value":false,"import":"muse-historical-import"}}`)
					},
				} {
					t.Run(mutation, func(t *testing.T) {
						t.Parallel()
						// Each corrupt record owns an independent home and target, so abandon
						// cannot borrow a sibling's lock or mutate a shared fixture.
						localHome, localUser := t.TempDir(), t.TempDir()
						localEnv := setupTestEnv(t, localHome, localUser, newFakeKeychain(), time.Now())
						target := filepath.Join(localUser, "synthetic-settings.json")
						current := []byte("keep current settings")
						must(t, os.WriteFile(target, current, 0600))
						journal := setupjournal.Journal{FilesOnly: filesOnly, NativeImportProtection: !replay, NativeReplayProtection: replay, Changes: []fileapply.Change{{Path: target, Before: []byte("old settings"), After: current, Existed: true, Mode: 0600}}}
						raw, err := json.Marshal(journal)
						must(t, err)
						malformed := corrupt(raw)

						path := setupjournal.JournalPath(localHome)
						must(t, os.WriteFile(path, malformed, 0600))
						var out, problems bytes.Buffer
						code := Run([]string{"setup", "--abandon-recovery"}, nil, &out, &problems, localEnv)
						if code != 1 {
							t.Fatalf("protected malformed journal was discarded: code %d, %s%s", code, &out, &problems)
						}
						got, err := os.ReadFile(path)
						if err != nil || !bytes.Equal(got, malformed) {
							t.Fatal("protected malformed journal was moved or removed", err)
						}
						aside, err := filepath.Glob(path + ".*" + movedAsideSuffix)
						must(t, err)
						if len(aside) != 0 {
							t.Fatal("protected malformed journal quarantined", aside)
						}
						got, err = os.ReadFile(target)
						if err != nil || !bytes.Equal(got, current) {
							t.Fatal("protected malformed journal changed target", err)
						}
						if calls := fakeSched(localEnv).calls; len(calls) != 0 {
							t.Fatal("malformed journal reached scheduler", calls)
						}
					})
				}
			})
		}
	}
}
