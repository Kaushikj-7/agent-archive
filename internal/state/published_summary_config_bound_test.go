package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
)

type configReplacementReservation struct {
	stateReservation
	replace func()
}

func (r *configReplacementReservation) Reserve(n int64) bool {
	if r.replace != nil {
		replace := r.replace
		r.replace = nil
		replace()
	}
	return r.stateReservation.Reserve(n)
}

func TestPublishedSummaryConfigReplacementCannotExceedReservation(t *testing.T) {
	t.Parallel()
	s, _ := summaryCacheFixture(t)
	path := filepath.Join(s.Home(), "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Whitespace leaves the replacement fully valid and supported. A refusal
	// must come from the read boundary, rather than malformed configuration.
	replacement := append(bytes.Clone(raw), bytes.Repeat([]byte(" "), 1<<20)...)
	budget := agentapi.NewNativeReadBudget(128 << 10)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	home, err := local.OpenRootedHome(s.Home())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := home.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, _, err := read.summaryConfig(home); err != nil {
		t.Fatalf("original supported configuration: %v", err)
	}
	owner := &configReplacementReservation{stateReservation: budget.Reservation(), replace: func() {
		if err := os.WriteFile(path, replacement, 0600); err != nil {
			t.Fatal(err)
		}
	}}
	read.resourceReservation = func() stateReservation { return owner }
	if _, _, err := read.summaryConfig(home); err == nil {
		t.Fatal("decoded a replacement larger than the complete read budget")
	}
	used, peak := budget.Charged()
	if used != 0 || peak > 128<<10 {
		t.Fatalf("configuration read escaped or retained its reservation: used=%d peak=%d", used, peak)
	}
}
