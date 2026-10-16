package agentapi_test

import (
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/agents/claude"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func retainedFixture(t *testing.T) (archive.SessionRegistration, archive.SourceBundle) {
	t.Helper()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	root := t.TempDir()
	reg := archive.SessionRegistration{ArchiveSessionID: "synthetic-session", NativeSessionID: "synthetic-native", ProjectID: archive.ProjectID(root), ProjectRoot: root, TranscriptPath: root + "/synthetic.jsonl", Harness: archive.Harness{Name: "claude", Version: "historical-producer"}, SessionStartedAt: at}
	filtered, err := (claude.Filter{}).FilterJSONL(strings.NewReader(`{"type":"user","message":{"role":"user","content":"Synthetic prompt"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.NewSourceBundle(reg, claude.Filter{}, filtered, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	return reg, bundle
}

type retainedLeaseOperation string

const (
	leaseCompression retainedLeaseOperation = "compression"
	leaseEnvelope    retainedLeaseOperation = "envelope"
	leaseRefilter    retainedLeaseOperation = "refilter"
)

func TestRetainedLeasesPreserveAnotherSessionsScope(t *testing.T) {
	reg, bundle := retainedFixture(t)
	for _, operation := range []retainedLeaseOperation{leaseCompression, leaseEnvelope, leaseRefilter} {
		t.Run(string(operation), func(t *testing.T) {
			budget := agentapi.NewNativeReadBudget(8 << 20)
			var release func()
			var err error
			switch operation {
			case leaseCompression:
				_, release, err = agentapi.CompressRetainedSource(t.Context(), bundle, budget)
			case leaseEnvelope:
				_, release, err = agentapi.DetachRetainedEnvelope(t.Context(), bundle, budget)
			case leaseRefilter:
				_, release, err = agentapi.RefilterRetainedSource(t.Context(), reg, claude.Filter{}, bundle, budget)
			}
			if err != nil {
				t.Fatal(err)
			}
			if used, _ := budget.Charged(); used <= 0 {
				t.Fatal("operation did not retain its output charge")
			}
			end := budget.BeginScope(10)
			defer end()
			current := budget.Reservation()
			if !current.Reserve(10) {
				t.Fatal("current session reservation refused")
			}
			release()
			release()
			if used, _ := budget.Charged(); used != 10 || current.Available() != 0 {
				t.Fatalf("old lease changed current owner: used=%d available=%d", used, current.Available())
			}
			current.Release(10)
		})
	}
}

func TestRetainedStagedImportPreservesSourceFormatAndProducer(t *testing.T) {
	reg, input := retainedFixture(t)
	reg.StagedImportSHA256 = strings.Repeat("a", 64)
	input.Capture.SourceFormat = "claude-import-snapshot"
	reg.Harness.Version = "current-producer"
	out, release, err := agentapi.RefilterRetainedSource(t.Context(), reg, claude.Filter{}, input, agentapi.NewNativeReadBudget(8<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if out.Capture.SourceFormat != input.Capture.SourceFormat || out.Capture.Harness != input.Capture.Harness {
		t.Fatalf("retained import adopted current source observations: %+v", out.Capture)
	}
}
