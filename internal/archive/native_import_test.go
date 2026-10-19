package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func nativeImportFixture() NativeImport {
	return NativeImport{Contract: "muse-historical-import-v1", Confirmation: NativeImportConfirmation{BatchID: "2026-10-17-1", BatchInstance: strings.Repeat("a", 32), ScopeSHA256: strings.Repeat("b", 64), DestinationID: "destination", ProjectID: "project", RootFingerprint: strings.Repeat("c", 64), SelectedBindingSHA256: strings.Repeat("f", 64), SelectedSource: NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 1234, SHA256: strings.Repeat("d", 64)}}, Membership: NativeImportMembership{ReceiptID: strings.Repeat("e", 32), State: NativeImportAttached, Effects: []string{"registration"}}}
}

func TestNativeImportReceiptIdentitySurvivesDetachment(t *testing.T) {
	t.Parallel()
	value := nativeImportFixture()
	identity, err := NativeImportIdentityDigest(&value)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []NativeImportMembershipState{NativeImportAttached, NativeImportDetached} {
		value.Membership.State = state
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded NativeImport
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		got, err := NativeImportIdentityDigest(&decoded)
		if err != nil || got != identity || decoded.Membership.State != state {
			t.Fatalf("receipt state %s: %s %v", state, got, err)
		}
	}
	value.Membership.ReceiptID = strings.Repeat("f", 32)
	got, err := NativeImportIdentityDigest(&value)
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
			t.Parallel()
			value := nativeImportFixture()
			before, _ := NativeImportIdentityDigest(&value)
			if err := value.UnmarshalJSON(raw); !errors.Is(err, ErrUnsupportedNativeImport) {
				t.Fatalf("unsupported receipt returned %v", err)
			}
			after, _ := NativeImportIdentityDigest(&value)
			if before != after || value.Membership.State != NativeImportAttached {
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

func TestNativeImportCodecAndDigestCompatibility(t *testing.T) {
	t.Parallel()
	value := nativeImportFixture()
	// Frozen pre-repair field order and bytes, including the original digest domain.
	const expectedWire = `{"contract":"muse-historical-import-v1","confirmation":{"batch_id":"2026-10-17-1","batch_instance":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","scope_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","destination_id":"destination","project_id":"project","root_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","selected_binding_sha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","selected_source":{"version":1,"domain":"muse-ordinary","algorithm":"sha256","boundary":1234,"sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}},"membership":{"receipt_id":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","state":"attached","effects":["registration"]}}`
	const expectedDigest = "81f76cf0e69b8d2a61864b532200784c2e5d01301e062c48af442a882f71cc89"
	for _, input := range []any{value, &value} {
		encoded, err := json.Marshal(input)
		if err != nil || string(encoded) != expectedWire {
			t.Fatalf("%T wire changed: %s %v", input, encoded, err)
		}
	}
	got, err := NativeImportIdentityDigest(&value)
	if err != nil || got != expectedDigest {
		t.Fatalf("digest changed: %s %v", got, err)
	}
	value.Membership.State = NativeImportDetached
	got, err = NativeImportIdentityDigest(&value)
	if err != nil || got != expectedDigest {
		t.Fatalf("detached digest changed: %s %v", got, err)
	}
	if !errors.Is(validateNativeImport(nil), ErrUnsupportedNativeImport) {
		t.Fatal("nil receipt validated")
	}
	if _, err := NativeImportIdentityDigest(nil); !errors.Is(err, ErrUnsupportedNativeImport) {
		t.Fatal("nil digest accepted", err)
	}
	var absent *NativeImport
	encoded, err := json.Marshal(absent)
	if err != nil || string(encoded) != "null" {
		t.Fatal("nil codec changed", string(encoded), err)
	}
	value.Contract = "future"
	for _, input := range []any{value, &value} {
		if _, err := json.Marshal(input); !errors.Is(err, ErrUnsupportedNativeImport) {
			t.Fatalf("%T future codec accepted: %v", input, err)
		}
	}
	if _, err := NativeImportIdentityDigest(&value); !errors.Is(err, ErrUnsupportedNativeImport) {
		t.Fatal("future digest accepted", err)
	}
}
