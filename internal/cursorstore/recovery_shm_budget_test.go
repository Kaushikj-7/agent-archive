package cursorstore

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// Synthetic header-only native classification deliberately stops before SQL;
// a short shm cannot supply settled evidence at any available payload budget.
func recoveryShortShmFixture(t *testing.T, frames bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.vscdb")
	header := make([]byte, 100)
	copy(header, "SQLite format 3\x00")
	header[18], header[19] = 2, 2
	if err := os.WriteFile(path, header, 0o600); err != nil {
		t.Fatal(err)
	}
	var wal []byte
	if frames {
		wal = []byte{1}
	}
	if err := os.WriteFile(path+"-wal", wal, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-shm", []byte{1}, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRecoveryReservesShmBudgetBeforeRefusedObservation(t *testing.T) {
	path := recoveryShortShmFixture(t, false)
	budget := &recoveryTestBudget{bytes: 200}
	err := ReadRecovery(t.Context(), path, budget, func(context.Context, *sql.DB) error {
		t.Fatal("SQL opened despite exhausted header budget")
		return nil
	})
	if !agentapi.HasFailure(err, agentapi.Limit) || budget.usedBytes != 200 {
		t.Fatalf("expected pre-observation limit, got %v, budget %+v", err, budget)
	}
}

func TestRecoveryShortShmChargesReservationAndNonemptyWALStaysLocked(t *testing.T) {
	path := recoveryShortShmFixture(t, false)
	budget := &recoveryTestBudget{bytes: 392}
	read := func(context.Context, *sql.DB) error {
		t.Fatal("SQL opened despite unsettled source")
		return nil
	}
	err := ReadRecovery(t.Context(), path, budget, read)
	if ReasonOf(err) != Locked || budget.usedBytes != 392 {
		t.Fatalf("short shm refusal lost reservation: %v, %+v", err, budget)
	}
	path = recoveryShortShmFixture(t, true)
	budget = &recoveryTestBudget{bytes: 200}
	err = ReadRecovery(t.Context(), path, budget, read)
	if ReasonOf(err) != Locked || budget.usedBytes != 200 {
		t.Fatalf("live WAL refusal changed: %v, %+v", err, budget)
	}
}
