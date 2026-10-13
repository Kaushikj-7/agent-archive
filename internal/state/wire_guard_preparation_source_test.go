package state

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// This is a feature-preparation condition: the actual-parent source lacks the
// lower package. It is not evidence of a prior production behavior defect.
func TestWirePolicyPreparationSource(t *testing.T) {
	source, err := os.ReadFile("../wirepolicy/guards.go")
	if err != nil {
		t.Fatalf("wirepolicy preparation absent: %v", err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "guards.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	expectedImports := map[string]bool{
		"bytes":         true,
		"encoding/json": true,
		"strings":       true,
		"github.com/wangjohn/agent-archive/internal/stateerrors": true,
	}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || !expectedImports[path] || imp.Name != nil {
			t.Fatalf("unapproved wirepolicy dependency: %s", imp.Path.Value)
		}
		delete(expectedImports, path)
	}
	if len(expectedImports) != 0 {
		t.Fatalf("missing fixed dependencies: %v", expectedImports)
	}
	expectedFunctions := map[string]bool{"RejectDuplicateAuthorityKeys": true, "StrictStagedKeys": true, "RejectStagedNullAliases": true}
	for _, decl := range file.Decls {
		if function, ok := decl.(*ast.FuncDecl); ok {
			if function.Recv != nil || !expectedFunctions[function.Name.Name] {
				t.Fatalf("unapproved wirepolicy function: %s", function.Name.Name)
			}
			delete(expectedFunctions, function.Name.Name)
			continue
		}
		if general, ok := decl.(*ast.GenDecl); !ok || general.Tok != token.IMPORT {
			t.Fatal("wirepolicy gained type, mutable state or authority")
		}
	}
	if len(expectedFunctions) != 0 {
		t.Fatalf("missing fixed guards: %v", expectedFunctions)
	}
	alias, err := os.ReadFile("durable_quota.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(alias), "var ErrDurableStorageRecovery = stateerrors.ErrDurableStorageRecovery") {
		t.Fatal("state recovery sentinel is not the shared identity")
	}
	sentinel, err := os.ReadFile("../stateerrors/recovery.go")
	if err != nil {
		t.Fatal(err)
	}
	const definition = `errors.New("durable publication evidence requires recovery; native substitution and automatic cleanup are forbidden")`
	if strings.Count(string(sentinel), definition) != 1 {
		t.Fatal("shared recovery sentinel definition changed")
	}
}
