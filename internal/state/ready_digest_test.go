package state

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestReadyDigestMatchesMarshaledDigestWithOneEncodedReservation(t *testing.T) {
	s := newTestStore(t)
	value := struct {
		Text    []string `json:"text"`
		Records [][]byte `json:"records"`
	}{[]string{"<a&b>\n\"quoted\"", strings.Repeat("x", 1<<16)}, [][]byte{[]byte("raw\x00bytes")}}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.readyDigest(t.Context(), value); err != nil || got != readySHA(raw) {
		t.Fatalf("digest %q, want %q: %v", got, readySHA(raw), err)
	}
	// One encoded size plus the preflight scratch suffices; json.Marshal's
	// second copy is never charged.
	capacity := int64(len(raw)) + 32<<10 + 1<<10
	budget := agentapi.NewNativeReadBudget(capacity)
	read, closeRead := s.WithReadBudget(t.Context(), budget)
	defer closeRead()
	if got, err := read.readyDigest(t.Context(), value); err != nil || got != readySHA(raw) {
		t.Fatalf("digest needed more than one encoded size: %q %v", got, err)
	}
	if budget.Available() != capacity {
		t.Fatal("digest reservation leaked", budget.Available())
	}
}
