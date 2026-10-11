package setupresources

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/wangjohn/agent-archive/internal/testutil/golden" // Registers the repository-wide -update test flag.
)

func TestNewMutationRequiresReadableRecoveryHistory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"corrupt", []byte("{}"), 0600},
		{"public", nil, 0644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			r, err := Begin(home, Receipt{Provider: S3, Profile: "default", Bucket: "archive"})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "setup-resources", "bucket-"+r.ID+".json")
			if tc.data != nil {
				if err = os.WriteFile(path, tc.data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err = os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err = Begin(home, Receipt{Provider: S3, Profile: "default", Bucket: "another"}); err == nil {
				t.Fatal("mutation accepted with unreadable recovery history")
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil || len(entries) != 1 {
				t.Fatalf("failed preflight changed receipt history: %v %v", entries, err)
			}
		})
	}
}

func TestNewMutationDoesNotExceedRecoveryReceiptLimit(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	r, err := Begin(home, Receipt{Provider: S3, Profile: "default", Bucket: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < maxReceipts; i++ {
		r.ID = fmt.Sprintf("%032x", i)
		data, marshalErr := json.Marshal(r)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if err = os.WriteFile(filepath.Join(home, "setup-resources", "bucket-"+r.ID+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = Begin(home, Receipt{Provider: S3, Profile: "default", Bucket: "another"}); err == nil {
		t.Fatal("mutation exceeded recovery receipt limit")
	}
	receipts, err := List(home)
	if err != nil || len(receipts) != maxReceipts {
		t.Fatalf("preflight made existing history unreadable: %d %v", len(receipts), err)
	}
}

func TestInterruptedMutationRemainsUncertain(t *testing.T) {
	home := t.TempDir()
	receipt, err := Begin(home, Receipt{Provider: R2, Account: "0123456789abcdef0123456789abcdef", Bucket: "archive", CreatedAt: time.Now().UTC()})
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
	receipt, err := Begin(home, Receipt{Provider: S3, Profile: "default", Bucket: "archive"})
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
	receipt, err := Begin(home, Receipt{Provider: S3, Profile: "default", Bucket: "archive"})
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

func TestS3DottedNamesKeepTheirBinding(t *testing.T) {
	home := t.TempDir()
	r, err := Begin(home, Receipt{Provider: S3, Profile: "default", Bucket: "my.archive"})
	if err != nil {
		t.Fatal(err)
	}
	if err = Finish(home, r, Created); err != nil {
		t.Fatal(err)
	}
	receipts, err := List(home)
	if err != nil || len(receipts) != 1 || receipts[0].Bucket != "my.archive" {
		t.Fatalf("%v %v", receipts, err)
	}
	if _, err = Begin(home, Receipt{Provider: R2, Account: "0123456789abcdef0123456789abcdef", Bucket: "my.archive"}); err == nil {
		t.Fatal("R2 accepted dotted name")
	}
}

func TestCrashTemporaryFilesDoNotHideCommittedReceipts(t *testing.T) {
	home := t.TempDir()
	r, err := Begin(home, Receipt{Provider: S3, Profile: "default", Bucket: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "setup-resources", ".pending-crashed-write"), []byte("partial JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	receipts, err := List(home)
	if err != nil || len(receipts) != 1 || receipts[0].ID != r.ID || receipts[0].State != RequestSent {
		t.Fatalf("crash hid intent: %v %v", receipts, err)
	}
	if err = Finish(home, r, Created); err != nil {
		t.Fatal(err)
	}
	if err = Finish(home, r, CleanupPending); err != nil {
		t.Fatal(err)
	}
	// The loaded binding can complete recovery in a later process.
	receipts, err = List(home)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("%v %v", receipts, err)
	}
	if err = Finish(home, receipts[0], Deleted); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptReadsBoundOversizedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := readReceipt(path)
	if err != nil || len(data) != 16385 {
		t.Fatalf("read %d bytes: %v", len(data), err)
	}
}
