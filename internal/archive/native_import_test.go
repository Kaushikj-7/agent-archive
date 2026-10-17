package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func nativeImportFixture() NativeImport {
	return NativeImport{Contract: "muse-historical-import-v1", Confirmation: NativeImportConfirmation{BatchID: "2026-10-17-1", BatchInstance: strings.Repeat("a", 32), ScopeSHA256: strings.Repeat("b", 64), DestinationID: "destination", ProjectID: "project", RootFingerprint: strings.Repeat("c", 64), SelectedBindingSHA256: strings.Repeat("f", 64), SelectedSource: NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 1234, SHA256: strings.Repeat("d", 64)}}, Membership: NativeImportMembership{ReceiptID: strings.Repeat("e", 32), State: "attached", Effects: []string{"registration"}}}
}

func TestNativeImportReceiptIdentitySurvivesDetachment(t *testing.T) {
	t.Parallel()
	value := nativeImportFixture()
	identity, err := value.IdentityDigest()
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"attached", "detached"} {
		value.Membership.State = state
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded NativeImport
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		got, err := decoded.IdentityDigest()
		if err != nil || got != identity || decoded.Membership.State != state {
			t.Fatalf("receipt state %s: %s %v", state, got, err)
		}
	}
	value.Membership.ReceiptID = strings.Repeat("f", 32)
	got, err := value.IdentityDigest()
	if err != nil || got == identity {
		t.Fatal("replacement receipt reused the old identity")
	}
}

func TestNativeImportRefusesUnsupportedNestedAuthority(t *testing.T) {
	t.Parallel()
	valid, err := json.Marshal(nativeImportFixture())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"null receipt":            []byte("null"),
		"future contract":         bytes.Replace(valid, []byte("muse-historical-import-v1"), []byte("muse-historical-import-v2"), 1),
		"duplicate contract":      bytes.Replace(valid, []byte(`"contract":`), []byte(`"contract":"muse-historical-import-v1","contract":`), 1),
		"aliased contract":        bytes.Replace(valid, []byte(`"contract":`), []byte(`"Contract":`), 1),
		"null confirmation":       bytes.Replace(valid, []byte(`"confirmation":{`), []byte(`"confirmation":null,"ignored":{`), 1),
		"duplicate instance":      bytes.Replace(valid, []byte(`"batch_instance":`), []byte(`"batch_instance":"`+strings.Repeat("a", 32)+`","batch_instance":`), 1),
		"aliased project":         bytes.Replace(valid, []byte(`"project_id":`), []byte(`"Project_ID":`), 1),
		"missing scope":           bytes.Replace(valid, []byte(`"scope_sha256":"`+strings.Repeat("b", 64)+`",`), nil, 1),
		"null checkpoint":         bytes.Replace(valid, []byte(`"selected_source":{`), []byte(`"selected_source":null,"ignored":{`), 1),
		"future checkpoint":       bytes.Replace(valid, []byte(`"version":1`), []byte(`"version":2`), 1),
		"future state":            bytes.Replace(valid, []byte(`"state":"attached"`), []byte(`"state":"released"`), 1),
		"aliased state":           bytes.Replace(valid, []byte(`"state":`), []byte(`"State":`), 1),
		"null effects":            bytes.Replace(valid, []byte(`["registration"]`), []byte(`null`), 1),
		"duplicate effects":       bytes.Replace(valid, []byte(`["registration"]`), []byte(`["registration","registration"]`), 1),
		"unqualified link effect": bytes.Replace(valid, []byte(`["registration"]`), []byte(`["registration","parent_link"]`), 1),
		"trailing object":         append(bytes.Clone(valid), []byte(`{}`)...),
		"oversize":                bytes.Repeat([]byte(" "), MaxNativeImportBytes+1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			value := nativeImportFixture()
			before, _ := value.IdentityDigest()
			if err := value.UnmarshalJSON(raw); !errors.Is(err, ErrUnsupportedNativeImport) {
				t.Fatalf("unsupported receipt returned %v", err)
			}
			after, _ := value.IdentityDigest()
			if before != after || value.Membership.State != "attached" {
				t.Fatal("failed decode replaced current authority")
			}
		})
	}
}

func TestNativeImportWriterRefusesInvalidAuthority(t *testing.T) {
	t.Parallel()
	value := nativeImportFixture()
	value.Membership.Effects = []string{"parent_link"}
	if _, err := json.Marshal(value); !errors.Is(err, ErrUnsupportedNativeImport) {
		t.Fatalf("writer emitted unknown owned effects: %v", err)
	}
	value = nativeImportFixture()
	value.Confirmation.DestinationID = "destination\x00suffix"
	if _, err := json.Marshal(value); !errors.Is(err, ErrUnsupportedNativeImport) {
		t.Fatalf("writer emitted invalid destination: %v", err)
	}
}

func FuzzNativeImportAuthority(f *testing.F) {
	raw, err := json.Marshal(nativeImportFixture())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(raw)
	f.Add([]byte(`{"contract":"future"}`))
	f.Add([]byte("null"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		var value NativeImport
		if value.UnmarshalJSON(raw) != nil {
			return
		}
		if value.Validate() != nil {
			t.Fatal("decoder retained invalid authority")
		}
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded) > MaxNativeImportBytes {
			t.Fatalf("decoded authority cannot round trip: %v", err)
		}
		var again NativeImport
		if err := json.Unmarshal(encoded, &again); err != nil {
			t.Fatal(err)
		}
	})
}
