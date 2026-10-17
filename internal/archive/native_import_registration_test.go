package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func nativeImportRegistration(t *testing.T) SessionRegistration {
	t.Helper()
	receipt := nativeImportFixture()
	at := time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC)
	binding := &NativeSourceBinding{Agent: "muse", Version: 2, NativeID: "ordinary-id", NativeCreatedAt: at, Cwd: "/work", Path: "/native/session.jsonl", RootRevision: "one", Checkpoint: &receipt.Confirmation.SelectedSource}
	digest, err := NativeImportBindingDigest(binding)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Confirmation.SelectedBindingSHA256 = digest
	return SessionRegistration{NativeImport: &receipt, NativeBinding: binding, ArchiveSessionID: strings.Repeat("1", 32), NativeSessionID: binding.NativeID, ProjectID: receipt.Confirmation.ProjectID, ProjectRoot: "/work", Harness: Harness{Name: "muse"}, TranscriptPath: binding.Path, SessionStartedAt: at, RegisteredAt: at.Add(time.Hour), AdmittedAt: at.Add(time.Hour), Origin: SessionOriginImport, ImportBatch: NewImportBatch(receipt.Confirmation.BatchID), DestinationID: receipt.Confirmation.DestinationID}
}

func TestNativeImportRegistrationPreservesRetainedProofAfterStageCleanupAndDetach(t *testing.T) {
	t.Parallel()
	reg := nativeImportRegistration(t)
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"attached", "detached"} {
		reg.NativeImport.Membership.State = state
		reg.StagedImportSHA256 = ""
		raw, err := json.Marshal(reg)
		if err != nil {
			t.Fatal(err)
		}
		var got SessionRegistration
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if !got.RetainedOnly() || got.InBatch(reg.ImportBatch.Recorded()) != (state == "attached") || got.CaptureFrozen || got.ValidateNativeImport() != nil {
			t.Fatal("stage cleanup/detach changed original retained proof")
		}
	}
}

func TestNativeImportRegistrationRefusesChangedOwnerFacts(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*SessionRegistration){
		"hook origin":                   func(r *SessionRegistration) { r.Origin = SessionOriginHook },
		"different batch":               func(r *SessionRegistration) { r.ImportBatch = NewImportBatch("other") },
		"different project":             func(r *SessionRegistration) { r.ProjectID = "other" },
		"different destination":         func(r *SessionRegistration) { r.DestinationID = "other" },
		"different original birth":      func(r *SessionRegistration) { r.SessionStartedAt = r.SessionStartedAt.Add(time.Second) },
		"different binding proof":       func(r *SessionRegistration) { r.NativeBinding.Cwd = "/other" },
		"different selected checkpoint": func(r *SessionRegistration) { r.NativeImport.Confirmation.SelectedSource.Boundary++ },
		"child":                         func(r *SessionRegistration) { r.NativeChild = true },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reg := nativeImportRegistration(t)
			change(&reg)
			if !errors.Is(reg.Validate(), ErrUnsupportedNativeImport) {
				t.Fatal("changed authority accepted")
			}
		})
	}
}

func TestNativeImportRegistrationClosedTopSelectorPreservesCurrentValue(t *testing.T) {
	t.Parallel()
	reg := nativeImportRegistration(t)
	raw, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{`"native_import":{`, `"Native_Import":{`},
		{`"native_import":{`, `"native_\u0069mport":null,"native_import":{`},
		{`"native_import":{`, `"native_import":null,"native_import":{`},
		{`"native_import":{`, `"native_import":null,"ignored":{`},
		{`"contract":"muse-historical-import-v1"`, `"contract":"future"`},
		{`"selected_binding_sha256":`, `"Selected_Binding_SHA256":`},
	} {
		changed := bytes.Replace(raw, []byte(pair[0]), []byte(pair[1]), 1)
		if bytes.Equal(raw, changed) {
			t.Fatal("inert mutation")
		}
		before, _ := reg.NativeImport.IdentityDigest()
		if err := json.Unmarshal(changed, &reg); !errors.Is(err, ErrUnsupportedNativeImport) {
			t.Fatal("unsupported receipt not preserved", err)
		}
		after, _ := reg.NativeImport.IdentityDigest()
		if before != after {
			t.Fatal("failed decode replaced current receipt")
		}
	}
}

func TestNativeImportAbsentKeepsLegacyDecoderSemantics(t *testing.T) {
	t.Parallel()
	reg := SessionRegistration{ArchiveSessionID: "existing", NativeSessionID: "native"}
	if err := json.Unmarshal([]byte(`null`), &reg); err != nil || reg.ArchiveSessionID != "existing" {
		t.Fatal("null legacy registration changed", err)
	}
	if err := json.Unmarshal([]byte(`{"project_id":"new"}`), &reg); err != nil || reg.ArchiveSessionID != "existing" || reg.ProjectID != "new" {
		t.Fatal("partial legacy decoding changed", err)
	}
}
