package state

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeImportSelectionMarkerClosedAndOrdinaryBytesUnchanged(t *testing.T) {
	t.Parallel()
	q := ReadySelection{Version: 2, BundleSHA256: strings.Repeat("a", 64), MetadataSHA256: strings.Repeat("b", 64), SourceSetSHA256: strings.Repeat("c", 64), BindingSHA256: strings.Repeat("d", 64), DestinationID: "destination", AdmissionContext: strings.Repeat("e", 64), PolicyContext: strings.Repeat("f", 64), Predecessor: ReadyPredecessorAbsent, Purpose: "capture", SHA256: strings.Repeat("1", 64)}
	// Freeze the pre-import ordinary field order, spelling, and omission rules.
	old := struct {
		Version           int              `json:"version"`
		BundleSHA256      string           `json:"bundle_sha256"`
		MetadataSHA256    string           `json:"metadata_sha256"`
		SourceSetSHA256   string           `json:"source_set_sha256"`
		BindingSHA256     string           `json:"binding_sha256"`
		DestinationID     string           `json:"destination_id"`
		AdmissionContext  string           `json:"admission_context"`
		PolicyContext     string           `json:"policy_context"`
		Predecessor       ReadyPredecessor `json:"predecessor"`
		PredecessorSHA256 string           `json:"predecessor_sha256,omitempty"`
		Purpose           string           `json:"purpose"`
		SHA256            string           `json:"sha256"`
	}{q.Version, q.BundleSHA256, q.MetadataSHA256, q.SourceSetSHA256, q.BindingSHA256, q.DestinationID, q.AdmissionContext, q.PolicyContext, q.Predecessor, q.PredecessorSHA256, q.Purpose, q.SHA256}
	before, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(q)
	if err != nil || !bytes.Equal(before, after) || readySHA(before) != readySHA(after) {
		t.Fatal("ordinary selecting bytes/hash changed", err)
	}
	q.NativeImportAuthority = strings.Repeat("2", 64)
	raw, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{
		{`"native_import_authority":"` + q.NativeImportAuthority + `"`, `"native_import_authority":null`},
		{`"native_import_authority":"` + q.NativeImportAuthority + `"`, `"Native_Import_Authority":"` + q.NativeImportAuthority + `"`},
		{`"native_import_authority":"` + q.NativeImportAuthority + `"`, `"native_import_authority":"future"`},
		{`"native_import_authority":"` + q.NativeImportAuthority + `"`, `"native_import_authority":"` + q.NativeImportAuthority + `","native_import_authority":"` + q.NativeImportAuthority + `"`},
	} {
		changed := bytes.Replace(raw, []byte(change[0]), []byte(change[1]), 1)
		if bytes.Equal(raw, changed) {
			t.Fatal("inert codec mutation")
		}
		var decoded ReadySelection
		if err := json.Unmarshal(changed, &decoded); err == nil {
			t.Fatal("unsupported selecting marker accepted")
		}
	}
}

func TestNativeImportSelectionMarkerCannotSubstituteForCurrentOwner(t *testing.T) {
	f, reg := historicalNativeOwnerFixture(t)
	marker, err := nativeImportSelectionAuthority(reg)
	if err != nil {
		t.Fatal(err)
	}
	q := ReadySelection{NativeImportAuthority: marker}
	if err := validateNativeImportSelectionAuthority(reg, q); err != nil {
		t.Fatal(err)
	}
	q.NativeImportAuthority = strings.Repeat("f", 64)
	if err := validateNativeImportSelectionAuthority(reg, q); err == nil {
		t.Fatal("marker-only substitution accepted")
	}
	ordinary := reg
	ordinary.NativeImport = nil
	if err := validateNativeImportSelectionAuthority(ordinary, ReadySelection{NativeImportAuthority: marker}); err == nil {
		t.Fatal("ordinary owner claimed historical selection")
	}
	f.assertReleased(t)
}
