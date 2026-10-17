package jsonwire_test

import (
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/jsonwire"
	_ "github.com/wangjohn/agent-archive/internal/testutil/golden"
	"testing"
)

func TestImportBatchBoundMatchesItsStringWire(t *testing.T) {
	for _, id := range []string{"", "ordinary-import", "escaped \" <& \n"} {
		batch := archive.NewImportBatch(id)
		n, err := jsonwire.Bound(t.Context(), batch, 4096)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(batch)
		if err != nil {
			t.Fatal(err)
		}
		if n != int64(len(raw)) {
			t.Fatal("import batch wire bound", n, len(raw))
		}
	}
}

func TestImportBatchNullAndPointerWire(t *testing.T) {
	var batch archive.ImportBatch
	if err := json.Unmarshal([]byte("null"), &batch); err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{batch, &batch, (*archive.ImportBatch)(nil)} {
		n, err := jsonwire.Bound(t.Context(), value, 4096)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(value)
		if err != nil || n != int64(len(raw)) {
			t.Fatal(n, string(raw), err)
		}
	}
}
