package backfill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Negative/hidden ownership facts are renewed even when another unchanged
// session supplies the selected root witness. Genuine tail appends remain valid.
func TestFirstRunRecoveryRenewsHiddenGrowingHeader(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("rewrite=%v", rewrite), func(t *testing.T) {
			tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			hiddenID := "00000000-0000-0000-0000-000000000066"
			plain := tr.mkdir("home/plain")
			body := codexTranscript(hiddenID, hiddenID, plain, fixedNow.Add(-48*time.Hour))
			path := tr.write(filepath.Join("home", codexFile(hiddenID)), body)
			p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}, Since: fixedNow.Add(-24 * time.Hour).Format(dateLayout)})
			if c := candidate(t, p, hiddenID); c.Skip != SkipFilteredOut {
				t.Fatalf("negative ownership participant was not hidden: %+v", c)
			}
			if c := candidate(t, p, goneID); c.ProjectRoot != root || c.ProjectResolution == nil {
				t.Fatalf("unchanged fixture did not recover: %+v", c)
			}
			if err := p.CheckRecovery(t.Context()); err != nil {
				t.Fatal("unchanged confirmation", err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if rewrite {
				clone := tr.repo("home/new-competing-clone-with-longer-name")
				rewritten := strings.Replace(body, plain, clone, 1) + `{"type":"tail"}` + "\n"
				if err := os.WriteFile(path, []byte(rewritten), before.Mode()); err != nil {
					t.Fatal(err)
				}
			} else {
				appendRecord(t, path, `{"type":"tail"}`+"\n")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || after.Size() <= before.Size() {
				t.Fatal("mutation did not preserve inode/mode while growing", err)
			}
			err = p.CheckRecovery(t.Context())
			if (err != nil) != rewrite {
				t.Fatalf("confirmation after hidden growth: %v, rewrite=%v", err, rewrite)
			}
		})
	}
}
