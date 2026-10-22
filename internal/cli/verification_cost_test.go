package cli

import (
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A pass with nothing to read back decodes no session's published state:
// the summary at its head says when the last publication was. Before, each
// pass decoded every session's whole state (about 158 ms for 200 sessions)
// only to find it already verified.
//
// Not parallel: state.PublishedStateLoads counts every load in the process.
func TestReadBackOfVerifiedSessionsDecodesNothing(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	remote := storagetest.NewMemoryStore()
	total := maxVerificationsPerPass + 3
	cfg, store := publishSyntheticSessions(t, home, project, remote, at, total)
	now := at.Add(time.Hour)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	env.Now = func() time.Time { return now }
	verified := 0
	for range 2 {
		summary, err := verifyPublications(home, cfg, env, store, remote)
		if err != nil {
			t.Fatal(err)
		}
		verified += summary.Verified
	}
	if verified != total {
		t.Fatalf("verified %d of %d", verified, total)
	}
	loads := state.PublishedStateLoads()
	summary, err := verifyPublications(home, cfg, env, store, remote)
	if err != nil || summary != (verificationSummary{Enumerated: true}) {
		t.Fatalf("%#v %v", summary, err)
	}
	if n := state.PublishedStateLoads() - loads; n != 0 {
		t.Fatalf("a pass with nothing to read back decoded %d published states", n)
	}
}
