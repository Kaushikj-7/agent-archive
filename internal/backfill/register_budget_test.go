package backfill

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// The active parent vector used to bypass the caller's native/state ledger.
func TestRegistrationSharesReadBudgetBeforeReservingParents(t *testing.T) {
	t.Parallel()
	r, candidate, _ := stagingFixture(t)
	r.ReadBudget = agentapi.NewNativeReadBudget(512)
	before, err := os.ReadFile(filepath.Join(r.Home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run([]Candidate{candidate}); !errors.Is(err, agentapi.ErrReadBudget) {
		t.Fatal("active parent bypassed shared charge", err)
	}
	regs, err := r.Store.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatal("pressure admitted an owner", regs, err)
	}
	after, err := os.ReadFile(filepath.Join(r.Home, "config.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("pressure changed policy", err)
	}
	if used, _ := r.ReadBudget.Charged(); used != 0 {
		t.Fatal("pressure leaked reservation", used)
	}
}
