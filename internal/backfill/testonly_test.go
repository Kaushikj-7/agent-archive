package backfill

import (
	"context"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"path/filepath"
	"strings"
)

// Helpers only tests use, kept out of the production files so deadcode
// (golang.org/x/tools/cmd/deadcode) reports only code that is really dead.

// CursorStateDatabase is where Cursor keeps its chats under home on this
// machine (see platform.Locations).
func CursorStateDatabase(home string) string {
	return (Environment{Home: home, NativePaths: builtin.NewBuiltins()}).cursorStateDatabase()
}

// CursorDatabaseReader is CursorDatabaseReaderFor for the state.vscdb under
// home on this machine.
func CursorDatabaseReader(home string) func(context.Context) (CursorDatabaseResult, error) {
	return CursorDatabaseReaderFor(Environment{Sources: testSources, Discovery: builtin.NewBuiltins(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: home})
}

// cursorSlug is the folder name Cursor gives a workspace under
// ~/.cursor/projects: the absolute path without its leading separator, with
// every character other than an ASCII letter or digit replaced by '-'. It
// cannot be reversed reliably, so candidates are converted and compared.
func cursorSlug(path string) string {
	path = strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator))
	return slugName(path)
}

func slugName(name string) string {
	b := []byte(name)
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

func cursorWorkspaceFolders(env Environment) []string {
	storage := cursorWorkspaceStorage(env)
	if storage == "" {
		return nil
	}
	entries, err := env.readDir(storage)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := env.readFile(filepath.Join(storage, entry.Name(), "workspace.json"))
		if err != nil {
			continue
		}
		if folder := workspaceMetadataFolder(env, "cursor", data); folder != "" {
			out = append(out, folder)
		}
	}
	return out
}

// CheckRecovery is AdmitRecovery that refuses unless every imported session
// is still current: what a test asserts about one plan's evidence.
func (p Plan) CheckRecovery(ctx context.Context) error {
	_, changed, err := p.AdmitRecovery(ctx)
	if err == nil && changed > 0 {
		err = errRecoveryChanged
	}
	return err
}

// current reports an inventory with no membership change at all.
func (i *recoverySourceInventory) current(ctx context.Context) bool {
	grown, ok := i.compare(ctx)
	return ok && !grown
}
