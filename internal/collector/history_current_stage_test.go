package collector

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

type currentHistoryStageFault string

const (
	currentStageMissing currentHistoryStageFault = "missing"
	currentStageCorrupt currentHistoryStageFault = "corrupt"
	currentStageSwap    currentHistoryStageFault = "swap"
	currentStageForeign currentHistoryStageFault = "foreign"
)

func TestCurrentHistoryStageRefusesMissingCorruptAndForeignPayloads(t *testing.T) {
	for _, kind := range []currentHistoryStageFault{currentStageMissing, currentStageCorrupt, currentStageSwap, currentStageForeign} {
		t.Run(string(kind), func(t *testing.T) {
			scan, p := privacyJournal(t)
			defer scan.releaseRetained()
			if len(p.SourceBytes) != 0 || p.Sources[0].Payload.Kind != state.PublicationHistoryStage {
				t.Fatal("fixture did not restart through current stage")
			}
			remote := &historyCrashStore{MemoryStore: scan.remote.(*storagetest.MemoryStore)}
			scan.remote = remote
			current := p.Sources[0]
			path := filepath.Join(scan.local.Home(), "sessions", scan.id(), "pending-sources", current.Reference.SHA256+".gz")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case currentStageMissing:
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case currentStageCorrupt:
				before = bytes.Repeat([]byte{'x'}, len(before))
				if err := os.WriteFile(path, before, 0600); err != nil {
					t.Fatal(err)
				}
			case currentStageSwap:
				other := p.History.Sources[1]
				before, err = scan.local.ReadPendingSource(scan.id(), other)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, before, 0600); err != nil {
					t.Fatal(err)
				}
			case currentStageForeign:
				handle := *p.Sources[0].Payload.Stage
				handle.OwnerSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				p.Sources[0].Payload.Stage = &handle
			}
			if _, err := scan.publishPending(scan.ctx, p); err == nil {
				t.Fatal("unavailable or foreign current stage published")
			}
			if remote.puts != 0 {
				t.Fatalf("unproved current source made %d remote writes", remote.puts)
			}
			if _, found, err := scan.local.LoadPending(scan.id()); err != nil || !found {
				t.Fatal("refusal discharged pending work", found, err)
			}
			if kind != currentStageMissing {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, before) {
					t.Fatal("refusal replaced staged evidence", err)
				}
			}
		})
	}
}
