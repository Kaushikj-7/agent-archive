package cli

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestBackfillForegroundDoesNotReopenSettledGroups(t *testing.T) {
	f, _ := foregroundChildren(t, 17)
	resources := decorateForegroundSource(t, f, nil)
	out, stderr, code := f.importRun(t, nil, false, "--yes", "--harness", "codex")
	if code != 0 {
		t.Fatal("foreground grouped publication", code, stderr, out)
	}
	resources.check(t)
	for i := range 17 {
		path := filepath.Join(f.userHome, ".codex", "sessions", fmt.Sprintf("rollout-11111111-1111-4111-8111-%012d.jsonl", i))
		// Fresh staging checks the selected revision once after reconciliation.
		// Later import groups must not reopen this settled owner's native input.
		if signatures := resources.signatures[path]; signatures != 1 {
			t.Errorf("settled owner %d native signature reads=%d want=1", i, signatures)
		}
	}
	t.Logf("logical=17 provider_passes=%d source_snapshots=%d signatures=%v", resources.passes, resources.snapshots, resources.signatures)
}
