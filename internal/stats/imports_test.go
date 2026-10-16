package stats

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/importgraph"
)

const (
	statsImportPath = "github.com/wangjohn/agent-archive/internal/stats"
	modulePrefix    = "github.com/wangjohn/agent-archive/internal/"
	archivePath     = "github.com/wangjohn/agent-archive/internal/archive"
	identityPath    = "github.com/wangjohn/agent-archive/internal/agentmeta"
	codexFactsPath  = "github.com/wangjohn/agent-archive/internal/codexmeta"
	wirePath        = "github.com/wangjohn/agent-archive/internal/jsonwire"
)

// The engine is pure: it takes session metadata and options and returns
// numbers. So its production code imports only the archive's types (and the
// standard library), reaching pure agent identities through archive, never
// the command line, the bucket, the disk, or the network, and reads no clock.
// The command wires the outside world to it;
// depguard's rule in .golangci.yml says the same on macOS.
func TestStatsImportBoundary(t *testing.T) {
	t.Parallel()
	direct, all := importgraph.Imports(t, statsImportPath)
	importgraph.Forbid(t, "internal/stats", direct,
		"os", "os/exec", "os/signal", "io/ioutil", "io/fs", "path/filepath", "net", "net/http", "math/rand", "math/rand/v2",
		"golang.org/x/term",
		identityPath,
		wirePath,
		codexFactsPath,
	)
	for _, path := range all {
		if strings.HasPrefix(path, modulePrefix) && path != archivePath && path != identityPath && path != codexFactsPath {
			t.Errorf("internal/stats reaches %s; only internal/archive and its pure agentmeta/codexmeta dependencies are allowed", path)
		}
	}
	importgraph.Forbid(t, "internal/stats (transitively)", all, "net/http", "os/exec")
}

// Nothing in the engine may read the clock or the machine: time.Now and its
// relatives, and the process environment. Now and the zone come in through
// Options.
func TestStatsReadsNoClockOrEnvironment(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no source files: %v", err)
	}
	clock := []string{"Now", "Since", "Until", "After", "AfterFunc", "Tick", "NewTicker", "NewTimer", "Sleep"}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			if path, _ := strconv.Unquote(spec.Path.Value); path == "os" {
				t.Errorf("%s imports os", name)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "time" && slices.Contains(clock, sel.Sel.Name) {
				t.Errorf("%s: time.%s reads the clock; take it from Options", fset.Position(sel.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no production files checked")
	}
}
