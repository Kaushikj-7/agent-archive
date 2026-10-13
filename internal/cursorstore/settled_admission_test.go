package cursorstore

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

type settledAdmissionBudget struct {
	rows  int
	bytes int64
}

func (b *settledAdmissionBudget) RemainingRows() int    { return b.rows }
func (b *settledAdmissionBudget) RemainingBytes() int64 { return b.bytes }
func (b *settledAdmissionBudget) Charge(rows int, bytes int64) error {
	if rows > b.rows || bytes > b.bytes {
		return agentapi.Wrap(agentapi.Limit, agentapi.ErrReadBudget)
	}
	b.rows -= rows
	b.bytes -= bytes
	return nil
}

// A typed budget refusal must precede decoded header maps/IDs, even when the
// same body would be malformed with sufficient decoded-view capacity.
func TestSettledAdmissionReservesDecodedHeadersBeforeParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	writeDB(t, path, false, map[string]any{"composerData:c": `{"_v":18,"composerId":"c","fullConversationHeadersOnly":"malformed header list"}`})
	visits := 0
	_, err := ReadSettledAdmission(t.Context(), path, "c", 1<<20, 1<<20, &settledAdmissionBudget{rows: 1024, bytes: 1024}, func(agentapi.DatabaseRecord) error { visits++; return nil })
	if !errors.Is(err, agentapi.ErrReadBudget) || visits != 0 {
		t.Fatalf("header decode preceded prospective owner refusal: visits=%d err=%v", visits, err)
	}
	_, err = ReadSettledAdmission(t.Context(), path, "c", 1<<20, 1<<20, &settledAdmissionBudget{rows: 1024, bytes: 1 << 20}, func(agentapi.DatabaseRecord) error { visits++; return nil })
	if err == nil || agentapi.Failure(err) == agentapi.Limit || visits != 0 {
		t.Fatalf("adequate owner changed malformed-header contract: visits=%d err=%v", visits, err)
	}
}

func TestSettledAdmissionReservesHeaderCountBeforeParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	writeDB(t, path, false, map[string]any{"composerData:c": `{"_v":18,"composerId":"c","fullConversationHeadersOnly":[null,null,null]}`})
	visits := 0
	_, err := ReadSettledAdmission(t.Context(), path, "c", 1<<20, 1<<20, &settledAdmissionBudget{rows: 5, bytes: 1 << 20}, func(agentapi.DatabaseRecord) error { visits++; return nil })
	if !errors.Is(err, agentapi.ErrReadBudget) || visits != 0 {
		t.Fatalf("header IDs parsed before prospective row refusal: visits=%d err=%v", visits, err)
	}
	_, err = ReadSettledAdmission(t.Context(), path, "c", 1<<20, 1<<20, &settledAdmissionBudget{rows: 1024, bytes: 1 << 20}, func(agentapi.DatabaseRecord) error { visits++; return nil })
	if err == nil || agentapi.Failure(err) == agentapi.Limit || visits != 0 {
		t.Fatalf("adequate header-count owner lost malformed control: visits=%d err=%v", visits, err)
	}
}
