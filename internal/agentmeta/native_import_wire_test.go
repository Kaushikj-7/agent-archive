package agentmeta_test

import (
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/archive"
	"strings"
	"testing"
)

func nativeImportWireFixture() archive.NativeImport {
	return archive.NativeImport{Contract: "muse-historical-import-v1", Confirmation: archive.NativeImportConfirmation{BatchID: "confirmed-1", BatchInstance: strings.Repeat("a", 32), ScopeSHA256: strings.Repeat("b", 64), DestinationID: "destination<&>", ProjectID: "project", RootFingerprint: strings.Repeat("c", 64), SelectedBindingSHA256: strings.Repeat("d", 64), SelectedSource: archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 12, SHA256: strings.Repeat("e", 64)}}, Membership: archive.NativeImportMembership{ReceiptID: strings.Repeat("f", 32), State: "attached", Effects: []string{"registration"}}}
}

func TestNativeImportWireBoundAndRefusal(t *testing.T) {
	receipt := nativeImportWireFixture()
	for _, value := range []any{receipt, &receipt, (*archive.NativeImport)(nil), archive.SessionRegistration{}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		n, err := agentmeta.JSONWireBound(t.Context(), value, 1<<20)
		if err != nil || n < int64(len(raw)) {
			t.Fatalf("%T bound=%d bytes=%d: %v", value, n, len(raw), err)
		}
		if _, err := agentmeta.JSONWireBound(t.Context(), value, 1); err == nil {
			t.Fatalf("%T ignored limit", value)
		}

	}
	receipt.Contract = "future"
	if _, err := agentmeta.JSONWireBound(t.Context(), receipt, 1<<20); err == nil {
		t.Fatal("future receipt accepted")
	}
}

func TestNativeImportWireEscapedMaximumRefusesBeforeEncoding(t *testing.T) {
	receipt := nativeImportWireFixture()
	receipt.Confirmation.DestinationID = strings.Repeat("<", 4096)
	receipt.Confirmation.ProjectID = strings.Repeat("&", 4096)
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(receipt); err == nil {
		t.Fatal("codec accepted escaped over-limit receipt")
	}
	if _, err := agentmeta.JSONWireBound(t.Context(), receipt, 1<<20); err == nil {
		t.Fatal("sizer accepted escaped over-limit receipt")
	}
}
