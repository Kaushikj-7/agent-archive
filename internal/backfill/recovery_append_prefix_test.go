package backfill

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A changed nonimported transcript can become an evidence copy only if its
// ORIGINAL inspected prefix is unchanged. A surviving destination witness must
// not hide a competing clone revealed by a growing rewrite of that prefix.
func TestRecoveryAppendedWitnessRequiresOriginalHeader(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  bool
		rewrite bool
		clone   bool
	}{
		{name: "unchanged"},
		{name: "genuine_append", change: true},
		{name: "growing_header_rewrite", change: true, rewrite: true},
		{name: "appended_competing_clone", change: true, clone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, env, cfg, root, goneID := firstRunRecoveryFixture(t, false)
			id := "00000000-0000-0000-0000-000000000088"
			cwd := tr.mkdir("home/plain")
			if tc.clone {
				cwd = tr.repo("home/competing-clone")
			}
			body := codexTranscript(id, id, cwd, fixedNow.Add(-48*time.Hour))
			path := tr.write(filepath.Join("home", codexFile(id)), body)
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := false
			env.Open = func(name string) (io.ReadCloser, error) {
				f, err := os.Open(name)
				if err != nil || name != path || !tc.change {
					return f, err
				}
				return &mutationReader{ReadCloser: f, afterClose: func() {
					if changed {
						return
					}
					changed = true
					if tc.rewrite {
						clone := tr.repo("home/new-competing-clone-with-longer-name")
						rewritten := strings.Replace(body, cwd, clone, 1) + "{}\n"
						if err := os.WriteFile(path, []byte(rewritten), before.Mode()); err != nil {
							t.Fatal(err)
						}
					} else {
						appendRecord(t, path, "{}\n")
					}
					after, err := os.Lstat(path)
					if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || after.Size() <= before.Size() {
						t.Fatal("mutation did not retain inode/mode while growing", err)
					}
				}}, nil
			}
			p := plan(t, env, nil, cfg, Filters{Harnesses: []string{"codex"}})
			if changed != tc.change {
				t.Fatal("header mutation control not exercised", changed)
			}
			if tc.change {
				if c := candidate(t, p, id); c.Skip != SkipSourceChanged || c.ProjectRoot != "" {
					t.Fatalf("changed witness became an import/destination: %+v", c)
				}
			}
			c := candidate(t, p, goneID)
			if tc.rewrite || tc.clone {
				if c.ProjectResolution != nil || c.Skip != SkipWorktreeUnresolved {
					t.Fatalf("changed or conflicting original header certified recovery: %+v", c)
				}
				return
			}
			if c.Skip != "" || c.ProjectRoot != root || c.ProjectResolution == nil {
				t.Fatalf("unchanged/append control disabled recovery: %+v", c)
			}
			if err := p.CheckRecovery(t.Context()); err != nil {
				t.Fatal("unchanged/append confirmation", err)
			}
			if tc.change {
				appendRecord(t, path, "{}\n")
				if err := p.CheckRecovery(t.Context()); err != nil {
					t.Fatal("further genuine append disabled confirmation", err)
				}
			}
		})
	}
}
