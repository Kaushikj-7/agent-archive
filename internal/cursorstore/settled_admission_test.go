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

func TestSettledAdmissionCountsInlineFallbackBeforeDecode(t *testing.T) {
	for _, raw := range []string{
		`{"_v":18,"composerId":"c","conversation":[{"bubbleId":"b1"},{"bubbleId":"b2"},{"bubbleId":"b3"}]}`,
		`{"_v":18,"composerId":"c","fullConversationHeadersOnly":null,"conversation":[{"bubbleId":"b1"},{"bubbleId":"b2"},{"bubbleId":"b3"}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.vscdb")
			writeDB(t, path, false, map[string]any{"composerData:c": raw})
			visits := 0
			_, err := ReadSettledAdmission(t.Context(), path, "c", 1<<20, 1<<20, &settledAdmissionBudget{rows: 5, bytes: 1 << 20}, func(agentapi.DatabaseRecord) error { visits++; return nil })
			if !errors.Is(err, agentapi.ErrReadBudget) || visits != 0 {
				t.Fatalf("inline decode preceded row refusal: visits=%d err=%v", visits, err)
			}
			_, err = ReadSettledAdmission(t.Context(), path, "c", 1<<20, 1<<20, &settledAdmissionBudget{rows: 1024, bytes: 1 << 20}, func(agentapi.DatabaseRecord) error { visits++; return nil })
			if err != nil || visits == 0 {
				t.Fatalf("adequate inline fallback: visits=%d err=%v", visits, err)
			}
		})
	}
}

func TestSettledHeaderCountKeepsOrdinaryPrecedence(t *testing.T) {
	raw := []byte(`{"fullConversationHeadersOnly":[],"conversation":[{"bubbleId":"fallback"}]}`)
	count, err := settledHeaderCount(t.Context(), raw, 0)
	if err != nil || count != 0 {
		t.Fatalf("non-null full headers lost precedence: %d %v", count, err)
	}
	_, ids, err := decodeHeaders(raw)
	if err != nil || len(ids) != 0 {
		t.Fatalf("ordinary header precedence changed: %v %v", ids, err)
	}
}
