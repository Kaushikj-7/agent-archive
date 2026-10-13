package state

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// This observer uses only existing state entrypoints and can run unchanged on
// the actual-parent arm. Both arms must preserve successful and refused inputs.
func TestPublicationWireGuardCompatibility(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		keys   []string
		guard  func([]byte, []string) error
		refuse bool
	}{
		{"nested authority is not top level", `{"payload":{"commit":1,"COMMIT":2},"commit":1}`, []string{"commit"}, rejectDuplicateAuthorityKeys, false},
		{"escaped duplicate authority", `{"commit":1,"\u0063ommit":2}`, []string{"commit"}, rejectDuplicateAuthorityKeys, true},
		{"case duplicate authority", `{"commit":1,"COMMIT":2}`, []string{"commit"}, rejectDuplicateAuthorityKeys, true},
		{"invalid json", `{"commit":`, []string{"commit"}, rejectDuplicateAuthorityKeys, true},
		{"unrelated duplicate stays ordinary", `{"other":1,"other":2}`, []string{"commit"}, rejectDuplicateAuthorityKeys, false},
		{"strict complete keys", `{"version":3,"proof":{}}`, []string{"version", "proof"}, strictStagedKeys, false},
		{"strict missing key", `{"version":3}`, []string{"version", "proof"}, strictStagedKeys, true},
		{"strict null", `{"version":3,"proof":null}`, []string{"version", "proof"}, strictStagedKeys, true},
		{"strict extra key", `{"version":3,"proof":{},"extra":0}`, []string{"version", "proof"}, strictStagedKeys, true},
		{"strict wrong case", `{"Version":3,"proof":{}}`, []string{"version", "proof"}, strictStagedKeys, true},
		{"alias absent allowed", `{"ordinary":null}`, []string{"proof"}, rejectStagedNullAliases, false},
		{"alias string null allowed", `{"proof":"null"}`, []string{"proof"}, rejectStagedNullAliases, false},
		{"alias null refused", `{"proof": null}`, []string{"proof"}, rejectStagedNullAliases, true},
		{"alias wrong case refused", `{"Proof":{}}`, []string{"proof"}, rejectStagedNullAliases, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.guard([]byte(tc.raw), tc.keys)
			if tc.refuse {
				// The guard must return the sentinel object itself, not a wrapper or
				// a fresh error with the same message. The sentinel is pointer-backed.
				actual, expected := reflect.ValueOf(err), reflect.ValueOf(ErrDurableStorageRecovery)
				if !actual.IsValid() || actual.Type() != expected.Type() || actual.Kind() != reflect.Pointer || actual.Pointer() != expected.Pointer() {
					t.Fatalf("recovery identity lost: %v", err)
				}
				if !errors.Is(errors.Join(errors.New("outer"), err), ErrDurableStorageRecovery) {
					t.Fatalf("wrapped recovery identity lost: %v", err)
				}
			} else if err != nil {
				t.Fatalf("ordinary input refused: %v", err)
			}
		})
	}
}

func TestPublicationWireGuardActualCodecCompatibility(t *testing.T) {
	var ordinary PendingPublication
	if err := json.Unmarshal([]byte(`{"bundle":{}}`), &ordinary); err != nil {
		t.Fatalf("ordinary pending positive: %v", err)
	}
	if ordinary.CursorSelection != nil || ordinary.StagedSelection != nil || ordinary.Selection != nil || ordinary.Catalog != nil {
		t.Fatal("ordinary pending acquired authority")
	}
	for _, raw := range []string{
		`{"commit":null,"COMMIT":null}`,
		`{"cursor_commit4":null,"\u0063ursor_commit4":null}`,
		`{"journal_version":4,"cursor_commit4":null}`,
		`{"journal_version":3,"staged_commit3":null}`,
	} {
		var pending PendingPublication
		if err := json.Unmarshal([]byte(raw), &pending); !errors.Is(err, ErrDurableStorageRecovery) {
			t.Fatalf("pending authority refusal lost for %s: %v", raw, err)
		}
	}
}
