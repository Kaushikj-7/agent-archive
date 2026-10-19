package evidence

import (
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/local"
	"path/filepath"
	"testing"
)

func FuzzSkillDeclarationPaths(f *testing.F) {
	for _, seed := range []struct {
		path string
		id   string
	}{{"nested/body.md", "review"}, {"../outside.md", "review"}, {"/outside.md", "review"}, {"a\\b", "review"}, {"a//b", "review"}, {"a\x00b", "review"}, {"a\nb", "review"}, {"body.md", "../private"}, {"body.md", "review:secret"}, {"body.md", ""}} {
		f.Add(seed.path, seed.id)
	}
	f.Fuzz(func(t *testing.T, path, id string) {
		if !validSkillDeclaration(agentapi.SkillFile{Path: path, ID: id}) {
			return
		}
		root := filepath.Join(string(filepath.Separator), "synthetic", "package")
		resolved := filepath.Join(root, filepath.FromSlash(path))
		if !local.PathWithin(resolved, root) || resolved == root {
			t.Fatal("accepted declaration can escape package")
		}
		if !validSkillScope("user_muse_package:" + id) {
			t.Fatal("accepted ID cannot form bounded safe logical snapshot scope")
		}
	})
}
