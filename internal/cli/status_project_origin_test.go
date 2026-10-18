package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/discovery"
	"github.com/wangjohn/agent-archive/internal/sourcefacts"
)

func TestStatusKeepsOriginRecoveryFailuresActionable(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	for _, outcome := range []sourcefacts.RecoveryOutcome{
		sourcefacts.RecoveryOriginCycle, sourcefacts.RecoveryOriginMissing,
		sourcefacts.RecoveryOriginUnreadable, sourcefacts.RecoveryOriginConflict,
		sourcefacts.RecoveryOriginUnsupported, sourcefacts.RecoveryOriginUnresolved,
		sourcefacts.RecoveryOriginLimit, sourcefacts.RecoveryOriginChanged,
	} {
		for _, verbose := range []bool{false, true} {
			health := discovery.Health{Enabled: true, Supported: true, LastAttempt: at, Outcomes: map[string]int{string(outcome): 1}}
			view := statusView{configured: true, State: "Waiting for capture", Background: "loaded", Apps: []appStatus{{Name: "codex", Hooks: "installed", Discovery: &health}}}
			var out bytes.Buffer
			printStatus(&out, view, statusScreen{style: styleFor(&out), now: at, verbose: verbose})
			for _, want := range []string{"Project Git origin evidence pending", "backfill --dry-run", "--map-project"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("%s verbose=%v: missing %q in %s", outcome, verbose, want, out.String())
				}
			}
		}
	}
	if projectOriginEvidencePending(map[string]int{"untrusted-origin-or-path": 1}) {
		t.Fatal("unknown source text became a recognized origin diagnostic")
	}
}
