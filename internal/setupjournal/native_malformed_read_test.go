package setupjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/state"
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

func TestNativeProtectedRawJournalReadPreservesReceiverAndLegacyGarbage(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string][]byte{
		"malformed-before": []byte(`{"broken":!,"native_import_protection":true}`),
		"malformed-after":  []byte(`{"native_replay_protection":true,"broken":!}`),
		"escaped":          []byte(`{"broken":!,"native\u005fimport_protection":true}`),
		"alias":            []byte(`{"broken":!,"NATIVE_IMPORT_PROTECTION":true}`),
		"duplicate":        []byte(`{"native_import_protection":true,"native_import_protection":false}`),
		"fence":            []byte(`{"broken":!,"files_only":{"writer":"native-authority-v16"}}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			must(t, os.WriteFile(JournalPath(home), raw, 0600))
			receiver := Journal{Backend: "unchanged"}
			err := Read(home, &receiver)
			if !errors.Is(err, ErrUnsupportedRecoveryAuthority) || receiver.Backend != "unchanged" {
				t.Fatal("raw boundary lost protection or replaced receiver", err)
			}
			got, err := os.ReadFile(JournalPath(home))
			must(t, err)
			if !bytes.Equal(raw, got) {
				t.Fatal("raw boundary changed record")
			}
		})
	}
	for name, raw := range map[string][]byte{
		"ordinary":               []byte(`{"changes":[`),
		"selector-value":         []byte(`{"note":"native_import_protection","changes":[`),
		"escaped-selector-value": []byte(`{"note":"\"native_import_protection\":true","changes":[`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			must(t, os.WriteFile(JournalPath(home), raw, 0600))
			receiver := Journal{Backend: "unchanged"}
			err := Read(home, &receiver)
			if err == nil || errors.Is(err, ErrUnsupportedRecoveryAuthority) {
				t.Fatal("truly unprotected garbage changed classification", err)
			}
			if receiver.Backend != "unchanged" {
				t.Fatal("garbage replaced receiver")
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		var receiver Journal
		if err := Read(t.TempDir(), &receiver); !os.IsNotExist(err) {
			t.Fatal("missing journal lost existing missing-file semantics", err)
		}
	})
}

func TestNativeJournalWhitespaceClassifierPreservesRefusal(t *testing.T) {
	t.Parallel()
	whitespace := []byte(" \t\r\n\u0085\u00a0\u2000")
	unknown := bytes.Repeat([]byte(`"unknown":0,`), 32)
	tail := bytes.Repeat(whitespace, 128)
	for name, raw := range map[string][]byte{
		"sticky-before-tail":  append(append([]byte(`{"broken":!,"native_import_protection":true,`), unknown...), tail...),
		"sticky-after-fields": append(append(append([]byte(`{"broken":!,`), unknown...), []byte(`"native_replay_protection":true}`)...), tail...),
		"space-before-colon":  append(append(append([]byte(`{"broken":!,"native_import_protection"`), whitespace...), []byte(`:true}`)...), tail...),
		"space-before-fence":  append(append(append([]byte(`{"broken":!,"files_only":`), whitespace...), []byte(`{"writer":"native-authority-v16"}}`)...), tail...),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !nativeJournalAuthorityPresent(raw) {
				t.Fatal("left whitespace hid protected key/fence")
			}
		})
	}
	ordinary := append(append([]byte(`{"broken":!,`), unknown...), tail...)
	if nativeJournalAuthorityPresent(ordinary) {
		t.Fatal("unprotected fields and trailing whitespace acquired authority")
	}
}

func TestNativeRawJournalLegacySyntaxCompatibility(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string][]byte{
		"truncated":      []byte(`{"changes":[`),
		"empty":          {},
		"garbled":        []byte(`not json`),
		"selector-value": []byte(`{"note":"native_import_protection","changes":[`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			must(t, os.WriteFile(JournalPath(home), raw, 0600))
			receiver := Journal{Backend: "unchanged"}
			err := Read(home, &receiver)
			if !state.IsUndecodable(err) || errors.Is(err, ErrUnsupportedRecoveryAuthority) {
				t.Fatal("unprotected garbage lost legacy syntax classification", err)
			}
			if receiver.Backend != "unchanged" {
				t.Fatal("syntax normalization assigned receiver")
			}
			err = Recover(home, nil, func() (func(), error) {
				t.Fatal("undecodable record reached recovery mutation")
				return nil, errors.New("unexpected recovery")
			})
			var blocked *RecoveryBlockedError
			if !errors.As(err, &blocked) {
				t.Fatal("legacy garbage lost recoverable way out", err)
			}
			got, err := os.ReadFile(JournalPath(home))
			must(t, err)
			if !bytes.Equal(got, raw) {
				t.Fatal("syntax refusal changed record")
			}
		})
	}
	t.Run("valid-unsupported", func(t *testing.T) {
		t.Parallel()
		raw := []byte(`{"schema_version":999,"changes":{"files":[]},"jobs":[],"files_only":false}`)
		var direct Journal
		original := direct.UnmarshalJSON(raw)
		if original == nil || errors.Is(original, ErrUnsupportedRecoveryAuthority) || state.IsUndecodable(original) {
			t.Fatal("fixture is not a closed valid-wire refusal", original)
		}
		home := t.TempDir()
		must(t, os.WriteFile(JournalPath(home), raw, 0600))
		receiver := Journal{Backend: "unchanged"}
		err := Read(home, &receiver)
		if err == nil || err.Error() != original.Error() || state.IsUndecodable(err) || receiver.Backend != "unchanged" {
			t.Fatal("valid unsupported wire was accepted, reclassified or assigned", err)
		}
	})
}
