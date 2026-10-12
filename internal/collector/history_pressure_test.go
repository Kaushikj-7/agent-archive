package collector

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestNativeHistoryScannerPressurePreservesRequestAndRetries(t *testing.T) {
	scan, lookup := reconciliationFixture(t)
	ref := reconciliationNative(t, scan.reg.ProjectRoot, revisionC, 2, true, strings.Repeat("safe text ", 2000))
	lookup.set.Current = &ref
	lookup.set.Candidates = []agentapi.SourceRef{ref}
	raw, err := os.ReadFile(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(128 << 20)
	reservation := budget.Reservation()
	scan.opts.CodexRollouts = &lifecycleLookup{reconciliationLookup: lookup, budget: budget}
	original, err := os.ReadFile(scan.reg.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	pressure := budget.Available() - int64(len(raw)+len(original)) - (256 << 10) - 6000
	if !reservation.Reserve(pressure) {
		t.Fatal("reserve")
	}
	if err := scan.local.SaveRequest(scan.id(), "stop", scan.now); err != nil {
		t.Fatal(err)
	}
	request, found, err := scan.local.LoadRequest(scan.id())
	if err != nil || !found {
		t.Fatal(err)
	}
	scan.req = request
	_, ok, err := scan.read()
	if ok || !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatalf("valid history under capacity pressure: ok=%v err=%v", ok, err)
	}
	current, found, err := scan.local.LoadRequest(scan.id())
	if err != nil || !found || current.Token != request.Token {
		t.Fatal("resource pressure acknowledged request", err)
	}
	if _, blocked := scan.published.Blocked(); blocked {
		t.Fatal("resource pressure became a permanent size gap")
	}
	if _, found, err := scan.local.LoadScanSignature(scan.id()); err != nil || found {
		t.Fatal("resource pressure settled scan", err)
	}
	reservation.Release(pressure)
	if _, ok, err := scan.read(); !ok || err != nil {
		t.Fatal("retry after release", err)
	}
	scan.releaseRetained()
	used, _ := budget.Charged()
	if used != 0 {
		t.Fatal("read ownership leaked", used)
	}
}
