package collector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/state"
)

type initialSweepChange string

const (
	initialSweepInitial            initialSweepChange = "initial"
	initialSweepFutureReceipt      initialSweepChange = "future-receipt"
	initialSweepOrphanProgress     initialSweepChange = "orphan-progress"
	initialSweepMalformedPublished initialSweepChange = "malformed-published"
	initialSweepChangedOwner       initialSweepChange = "changed-owner"
)

func TestNativeImportInitialReleaseSweepPreservesStageAndRefusesFalseRelease(t *testing.T) {
	for _, change := range []initialSweepChange{initialSweepInitial, initialSweepFutureReceipt, initialSweepOrphanProgress, initialSweepMalformedPublished, initialSweepChangedOwner} {
		t.Run(string(change), func(t *testing.T) {
			f := newHistoricalNativeStage(t)
			id := f.reg.ArchiveSessionID
			write := func(relative string, raw []byte) {
				t.Helper()
				path := filepath.Join(f.store.Home(), relative)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch change {
			case initialSweepInitial:
			case initialSweepFutureReceipt:
				write(filepath.Join("sessions", id, "import-stage", "release.json"), []byte(`{"version":99}`))
			case initialSweepOrphanProgress:
				write(filepath.Join("sessions", id, "import-stage", "release-progress.json"), []byte(`{"version":3,"receipt_sha256":"`+strings.Repeat("a", 64)+`","phase":"local-complete"}`))
			case initialSweepMalformedPublished:
				write(filepath.Join("published", id+".json"), []byte(`{"publication_version":3`))
			case initialSweepChangedOwner:
				reg := f.reg
				receipt := *reg.NativeImport
				receipt.Membership.ReceiptID = strings.Repeat("b", 32)
				reg.NativeImport = &receipt
				raw, err := json.Marshal(reg)
				if err != nil {
					t.Fatal(err)
				}
				write(filepath.Join("registrations", id+".json"), raw)
			}
			before := captureHistoricalReleaseFiles(t, f)
			reopened, err := state.Open(f.store.Home())
			if err != nil {
				t.Fatal(err)
			}
			holds, closeResult, err := reopened.ResumeStagedReleases(t.Context(), f.remote, f.opts.CollectorGuard)
			if closeResult != nil {
				defer closeResult()
			}
			if change == initialSweepInitial {
				if err != nil || len(holds) != 0 {
					t.Fatal("initial stage was treated as released publication", err, holds)
				}
			} else if err == nil && holds[id] == nil {
				t.Fatal("unacknowledged recovery evidence was accepted", change)
			}
			assertHistoricalReleaseFiles(t, f, before)
		})
	}
}
