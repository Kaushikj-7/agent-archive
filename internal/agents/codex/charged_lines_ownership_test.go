package codex

import (
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

// A reader may grow and close after its creating scope has ended. Its overlapping
// old/new buffers still belong to that original owner, never a later session.
func TestChargedLineReaderGrowthAndCloseKeepOriginalScope(t *testing.T) {
	t.Parallel()
	budget := agentapi.NewNativeReadBudget(128 << 10)
	pass := &relatedSourcePass{env: agentapi.SourceEnvironment{ReadBudget: budget}}
	originalEnd := budget.BeginScope(64 << 10)
	text := strings.Repeat("x", 12<<10)
	lines, err := pass.newChargedLineReader(strings.NewReader(text+"\n"), 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	originalEnd()
	currentEnd := budget.BeginScope(10)
	defer currentEnd()
	current := budget.Reservation()
	if !current.Reserve(10) {
		t.Fatal("current reserve")
	}
	line, more, err := lines.next(t.Context())
	if err != nil || !more || string(line) != text {
		t.Fatal("prior reader growth used current owner's ceiling", more, err)
	}
	if lines.lineCharge <= lineReaderScratch {
		t.Fatal("fixture did not grow the reader")
	}
	lines.close()
	lines.close()
	if pass.bytes != 0 {
		t.Fatal("reader charge leaked", pass.bytes)
	}
	if current.Reserve(1) || !budget.ScopeExceeded() {
		t.Fatal("prior reader cleanup lowered current scope charges")
	}
	current.Release(10)
	if used, _ := budget.Charged(); used != 0 {
		t.Fatal("leaked", used)
	}
}
