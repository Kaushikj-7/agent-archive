package backfill

import (
	"context"
	"io"
	"io/fs"
	"os"
	"testing"
)

func TestRecoverySourceInventoryHeaderRenewalBound(t *testing.T) {
	tr := newTree(t)
	inv := newRecoverySourceInventory(tr.env())
	seen := inv.environment()
	const header = "header\n"
	for _, name := range []string{"first", "second"} {
		file := tr.write("home/store/"+name+".jsonl", header)
		info, err := seen.Lstat(file)
		if err != nil {
			t.Fatal(err)
		}
		f, err := seen.open(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, f); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		inv.ownContent(file, info)
		appendRecord(t, file, "tail\n")
	}
	if !inv.currentBound(t.Context(), 2*int64(len(header))) {
		t.Fatal("complete prefix allowance rejected genuine appends")
	}
	if inv.currentBound(t.Context(), int64(len(header))) {
		t.Fatal("exhausted prefix budget certified a partial inventory")
	}
}

func TestRecoverySourceInventoryPrefixCancellation(t *testing.T) {
	tr := newTree(t)
	file := tr.write("home/store/source.jsonl", "header\n")
	inv := newRecoverySourceInventory(tr.env())
	seen := inv.environment()
	info, err := seen.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	f, err := seen.open(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	inv.ownContent(file, info)
	appendRecord(t, file, "tail\n")
	fresh, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if inv.prefixCurrent(ctx, file, fresh, inv.appendable[file]) {
		t.Fatal("cancelled prefix entry accepted")
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	probes := 0
	inv.env.Lstat = func(path string) (fs.FileInfo, error) {
		probes++
		if probes != 1 {
			t.Fatal("continued inventory probes after cancellation")
		}
		current, err := os.Lstat(path)
		cancel()
		return current, err
	}
	if inv.currentBound(ctx, recoveryHeaderRenewalBytes) || ctx.Err() == nil || probes != 1 {
		t.Fatal("cancellation during growing-file stat accepted or resumed", probes)
	}
}

func (i *recoverySourceInventory) currentBound(ctx context.Context, remaining int64) bool {
	grown, ok := i.compareBound(ctx, remaining)
	return ok && !grown
}
