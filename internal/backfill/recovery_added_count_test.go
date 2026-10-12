package backfill

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
)

// Cursor emits identities without scanning headers. Added-source admission
// must enforce the same count bound for both native discovery paths.
func TestAddedCursorTranscriptsCountBudget(t *testing.T) {
	for _, count := range []int{64, 65} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			tr, env, _, _, _ := addedSourcesFixture(t)
			inv := inventoryBaseline(t, env)
			for n := range count {
				tr.write(filepath.Join("home", ".cursor", "projects", "known-slug", "agent-transcripts", fmt.Sprintf("added-%d.txt", n)), "[user]\nhello\n")
			}
			added, ok := inv.added(t.Context())
			want := count <= recoveryAddedSourceLimit
			if ok != want || (ok && len(added.candidates) != count) {
				t.Fatalf("Cursor additions: count=%d got=%d current=%v want=%v", count, len(added.candidates), ok, want)
			}
		})
	}
}
