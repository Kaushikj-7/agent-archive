package jsonwire_test

import (
	"bytes"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	"strings"
	"testing"
)

func nativeImportWireFixture() archive.NativeImport {
	return archive.NativeImport{Contract: "muse-historical-import-v1", Confirmation: archive.NativeImportConfirmation{BatchID: "confirmed-1", BatchInstance: strings.Repeat("a", 32), ScopeSHA256: strings.Repeat("b", 64), DestinationID: "destination<&>", ProjectID: "project", RootFingerprint: strings.Repeat("c", 64), SelectedBindingSHA256: strings.Repeat("d", 64), SelectedSource: archive.NativeCheckpoint{Version: 1, Domain: "muse-ordinary", Algorithm: "sha256", Boundary: 12, SHA256: strings.Repeat("e", 64)}}, Membership: archive.NativeImportMembership{ReceiptID: strings.Repeat("f", 32), State: archive.NativeImportAttached, Effects: []string{"registration"}}}
}

func TestNativeImportWireBoundAndRefusal(t *testing.T) {
	receipt := nativeImportWireFixture()
	for _, value := range []any{receipt, &receipt, (*archive.NativeImport)(nil), archive.SessionRegistration{}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		n, err := jsonwire.Bound(t.Context(), value, 1<<20)
		if err != nil || n < int64(len(raw)) {
			t.Fatalf("%T bound=%d bytes=%d: %v", value, n, len(raw), err)
		}
		if absent, ok := value.(*archive.NativeImport); ok && absent == nil && n != 4 {
			t.Fatalf("nil receipt count = %d, want 4", n)
		}
		if _, err := jsonwire.Bound(t.Context(), value, 1); err == nil {
			t.Fatalf("%T ignored limit", value)
		}
		var output bytes.Buffer
		budget := &nativeImportWireBudget{}
		if err := jsonwire.Encode(t.Context(), &output, value, budget); err != nil || output.String() != string(raw)+"\n" || budget.used != 0 {
			t.Fatalf("%T streaming mismatch: %v", value, err)
		}
	}
	receipt.Contract = "future"
	for _, invalid := range []any{receipt, &receipt} {
		if _, err := jsonwire.Bound(t.Context(), invalid, 1<<20); err == nil {
			t.Fatalf("%T future receipt accepted", invalid)
		}
	}
}

type nativeImportWireBudget struct{ used int64 }

func (b *nativeImportWireBudget) Reserve(n int64) bool {
	if n < 0 || n > (1<<20)-b.used {
		return false
	}
	b.used += n
	return true
}

func (b *nativeImportWireBudget) Release(n int64) { b.used -= n }

func (b *nativeImportWireBudget) Available() int64 { return (1 << 20) - b.used }

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
	if _, err := jsonwire.Bound(t.Context(), receipt, 1<<20); err == nil {
		t.Fatal("sizer accepted escaped over-limit receipt")
	}
}
