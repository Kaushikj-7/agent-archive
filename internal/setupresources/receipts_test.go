package setupresources

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInterruptedMutationRemainsUncertain(t *testing.T) {
	home := t.TempDir()
	receipt, err := Begin(home, Receipt{Provider: "r2", Account: "0123456789abcdef0123456789abcdef", Bucket: "archive", CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate process death before the provider outcome can be saved.
	receipts, err := List(home)
	if err != nil || len(receipts) != 1 || receipts[0].State != RequestSent {
		t.Fatalf("uncertain receipt lost: %v %v", receipts, err)
	}
	if err = Finish(home, receipt, Created); err != nil {
		t.Fatal(err)
	}
	receipts, err = List(home)
	if err != nil || receipts[0].State != Created {
		t.Fatalf("creation not durable: %v %v", receipts, err)
	}
	if err = Finish(home, receipt, Secured); err != nil {
		t.Fatal(err)
	}
	if err = Finish(home, receipt, Deleted); err != nil {
		t.Fatal(err)
	}
	if err = Finish(home, receipt, Created); err == nil {
		t.Fatal("deleted resource resurrected")
	}
}

func TestFailedOutcomeWritePreservesUncertainty(t *testing.T) {
	home := t.TempDir()
	receipt, err := Begin(home, Receipt{Provider: "s3", Profile: "default", Bucket: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	changed := receipt
	changed.Bucket = "other"
	if err = Finish(home, changed, Created); err == nil {
		t.Fatal("binding change accepted")
	}
	receipts, err := List(home)
	if err != nil || receipts[0].State != RequestSent {
		t.Fatal("failed save changed prior evidence")
	}
	if err = os.WriteFile(filepath.Join(home, "setup-draft.json"), []byte("draft"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(home, "setup-draft.json")); err != nil {
		t.Fatal(err)
	}
	receipts, err = List(home)
	if err != nil || len(receipts) != 1 {
		t.Fatal("discarding draft lost receipt")
	}
}

func TestCorruptAndPublicReceiptsFailClosed(t *testing.T) {
	home := t.TempDir()
	receipt, err := Begin(home, Receipt{Provider: "s3", Profile: "default", Bucket: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "setup-resources", "bucket-"+receipt.ID+".json")
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = List(home); err == nil {
		t.Fatal("public receipt accepted")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = List(home); err == nil {
		t.Fatal("corrupt receipt accepted")
	}
}
