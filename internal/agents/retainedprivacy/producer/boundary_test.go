package producer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const retainedDomain = "github.com/wangjohn/agent-archive/internal/agents/retainedprivacy"

// Every source file is parsed, including ignored files and alternate build tags.
// Go's internal visibility closes construction; this gate closes every package
// inside that visibility domain and the fixed producer's operational callers.
func TestRetainedPrivacyWholeSourceAuthorityBoundary(t *testing.T) {
	root := filepath.Clean("../../../../")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, problem := range retainedBoundaryProblems(filepath.ToSlash(rel), raw) {
			t.Errorf("%s: %s", rel, problem)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func retainedBoundaryProblems(path string, raw []byte) []string {
	var problems []string
	fail := func(message string) { problems = append(problems, message) }
	file, err := parser.ParseFile(token.NewFileSet(), path, raw, parser.ParseComments)
	if err != nil {
		return []string{err.Error()}
	}
	imports := map[string]string{}
	for _, spec := range file.Imports {
		value, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			fail("invalid import")
			continue
		}
		name := filepath.Base(value)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = value
		if value == retainedDomain+"/producer" && path != "internal/agents/builtin/retained_privacy.go" {
			fail("fixed producer imported outside its builtin projection")
		}
		if strings.HasPrefix(value, retainedDomain+"/internal/") && path != "internal/agents/retainedprivacy/proof.go" && path != "internal/agents/retainedprivacy/producer/transform.go" && !strings.HasPrefix(path, "internal/agents/retainedprivacy/internal/sealed/") {
			fail("sealed construction domain escaped")
		}
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.HasPrefix(comment.Text, "//go:linkname") && strings.Contains(comment.Text, retainedDomain) {
				fail("linkname cannot bypass retained construction")
			}
		}
	}
	domainSource := strings.HasPrefix(path, "internal/agents/retainedprivacy/") && !strings.HasSuffix(path, "_test.go")
	if domainSource {
		allowedFiles := map[string]bool{"internal/agents/retainedprivacy/proof.go": true, "internal/agents/retainedprivacy/producer/transform.go": true, "internal/agents/retainedprivacy/internal/sealed/proof.go": true, "internal/agents/retainedprivacy/internal/sealed/request.go": true}
		if !allowedFiles[path] {
			fail("unreviewed source in the sealed operation domain")
		}
		for _, imported := range imports {
			if imported == "unsafe" || imported == "reflect" {
				fail("retained domain cannot bypass private proof fields")
			}
		}
	}
	allowedMints := map[*ast.SelectorExpr]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || path != "internal/agents/retainedprivacy/producer/transform.go" || fn.Name.Name != "Transform" || fn.Body == nil || len(fn.Body.List) == 0 {
			continue
		}
		last, ok := fn.Body.List[len(fn.Body.List)-1].(*ast.ReturnStmt)
		if !ok || len(last.Results) == 0 {
			continue
		}
		call, ok := last.Results[0].(*ast.CallExpr)
		if !ok {
			continue
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok {
			allowedMints[selector] = true
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		qualifier, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		if imports[qualifier.Name] == retainedDomain+"/internal/sealed" && selector.Sel.Name == "New" && !allowedMints[selector] {
			fail("sealed mint reference escaped terminal fixed producer return")
		}
		return true
	})
	if domainSource {
		allowedTypes := map[string]bool{}
		allowedFunctions := map[string]bool{}
		switch path {
		case "internal/agents/retainedprivacy/proof.go":
			for _, name := range []string{"Proof", "Request", "Policy", "Facts", "Expectation"} {
				allowedTypes[name] = true
			}
			allowedFunctions["FreezeRequest"] = true
		case "internal/agents/retainedprivacy/internal/sealed/request.go":
			for _, name := range []string{"Policy", "Expectation", "Request", "Lease"} {
				allowedTypes[name] = true
			}
			for _, name := range []string{"FreezeRequest", "Borrow", "Check", "Context", "Budget", "Close"} {
				allowedFunctions[name] = true
			}
		case "internal/agents/retainedprivacy/internal/sealed/proof.go":
			allowedTypes["Proof"], allowedTypes["Facts"] = true, true
			for _, name := range []string{"New", "Digest", "WithVerifiedTransform", "Close", "Err", "Deadline"} {
				allowedFunctions[name] = true
			}
		case "internal/agents/retainedprivacy/producer/transform.go":
			allowedFunctions["Transform"] = true
		}
		for _, decl := range file.Decls {
			switch declaration := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					switch item := spec.(type) {
					case *ast.TypeSpec:
						if ast.IsExported(item.Name.Name) && !allowedTypes[item.Name.Name] {
							fail("new exported sealed-domain type")
						}
					case *ast.ValueSpec:
						for _, name := range item.Names {
							if ast.IsExported(name.Name) {
								fail("sealed operation cannot expose a mutable issuer")
							}
						}
					}
				}
			case *ast.FuncDecl:
				if ast.IsExported(declaration.Name.Name) && !allowedFunctions[declaration.Name.Name] {
					fail("new exported sealed-domain operation")
				}
			}
		}
	}

	for _, decl := range file.Decls {
		function, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		ast.Inspect(function, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			qualifier, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			imported := imports[qualifier.Name]
			if imported == retainedDomain+"/internal/sealed" && selector.Sel.Name == "New" {
				if path != "internal/agents/retainedprivacy/producer/transform.go" || function.Name.Name != "Transform" {
					fail("proof mint outside fixed transform")
				}
				direct := false
				ast.Inspect(function, func(parent ast.Node) bool {
					call, ok := parent.(*ast.CallExpr)
					if ok && call.Fun == selector {
						direct = true
					}
					return true
				})
				if !direct {
					fail("proof mint cannot escape as a function value")
				}
			}
			if selector.Sel.Name == "fixedPrivacy" && path != "internal/agents/builtin/registry.go" && path != "internal/agents/builtin/retained_privacy.go" && !strings.HasSuffix(path, "_test.go") {
				fail("fixed provider storage accessed outside its composition root")
			}
			return true
		})
	}
	if path == "internal/agents/retainedprivacy/producer/transform.go" {
		allowedImports := map[string]bool{"errors": true, "sync": true, "github.com/wangjohn/agent-archive/internal/agentapi": true, "github.com/wangjohn/agent-archive/internal/agentmeta": true, "github.com/wangjohn/agent-archive/internal/archive": true, retainedDomain + "/internal/sealed": true}
		for _, name := range []string{"claude", "codex", "cursor", "muse"} {
			allowedImports["github.com/wangjohn/agent-archive/internal/agents/"+name] = true
		}
		for _, value := range imports {
			if !allowedImports[value] {
				fail("fixed producer imported a new operation: " + value)
			}
		}
		foundFilter := false
		mintCount := 0
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if ast.IsExported(function.Name.Name) && function.Name.Name != "Transform" {
				fail("new exported producer operation")
			}
			ast.Inspect(function, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				qualifier, ok := selector.X.(*ast.Ident)
				if ok && imports[qualifier.Name] == retainedDomain+"/internal/sealed" && selector.Sel.Name == "New" {
					mintCount++
				}
				return true
			})
			if function.Name.Name == "fixedFilter" {
				foundFilter = true
				problems = append(problems, fixedFilterProblems(function, imports)...)
			}
		}
		if !foundFilter || mintCount != 1 {
			fail("fixed filter and one terminal proof mint required")
		}
	}
	return problems
}

func fixedFilterProblems(function *ast.FuncDecl, imports map[string]string) []string {
	var problems []string
	want := map[string]string{"Claude": "claude", "Codex": "codex", "Cursor": "cursor", "Muse": "muse"}
	if function.Body == nil || len(function.Body.List) != 1 {
		return []string{"fixed filter must contain only its production switch"}
	}
	selection, ok := function.Body.List[0].(*ast.SwitchStmt)
	if !ok {
		return []string{"fixed production switch missing"}
	}
	for _, statement := range selection.Body.List {
		arm, ok := statement.(*ast.CaseClause)
		if !ok {
			problems = append(problems, "unexpected filter switch arm")
			continue
		}
		if len(arm.List) == 0 {
			continue
		}
		if len(arm.List) != 1 || len(arm.Body) != 1 {
			problems = append(problems, "filter choice must return one concrete zero-value filter")
			continue
		}
		name, ok := arm.List[0].(*ast.SelectorExpr)
		if !ok {
			problems = append(problems, "filter identity is not closed")
			continue
		}
		harness, found := want[name.Sel.Name]
		if !found {
			problems = append(problems, "unexpected production filter identity")
			continue
		}
		delete(want, name.Sel.Name)
		result, ok := arm.Body[0].(*ast.ReturnStmt)
		if !ok || len(result.Results) != 2 {
			problems = append(problems, "filter branch is not a direct return")
			continue
		}
		literal, ok := result.Results[0].(*ast.CompositeLit)
		if !ok || len(literal.Elts) != 0 {
			problems = append(problems, "filter must be the actual zero-value production implementation")
			continue
		}
		typ, ok := literal.Type.(*ast.SelectorExpr)
		if !ok {
			problems = append(problems, "concrete filter type missing")
			continue
		}
		qualifier, ok := typ.X.(*ast.Ident)
		if !ok || typ.Sel.Name != "Filter" || imports[qualifier.Name] != "github.com/wangjohn/agent-archive/internal/agents/"+harness {
			problems = append(problems, "filter identity does not select its production implementation")
		}
	}
	if len(want) != 0 {
		problems = append(problems, "production filter matrix incomplete")
	}
	return problems
}

func TestRetainedPrivacyBoundaryRejectsHiddenMintAndProducerSubstitution(t *testing.T) {
	actual, err := os.ReadFile("transform.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{"\nvar issuer = sealed.New\n", "\nvar ExportedIssuer any\n", "\nfunc OtherIssuer(){}\n"} {
		candidate := append(append([]byte(nil), actual...), []byte(extra)...)
		if len(retainedBoundaryProblems("internal/agents/retainedprivacy/producer/transform.go", candidate)) == 0 {
			t.Fatal("extra issuer escaped a complete otherwise-valid producer")
		}
	}

	for _, fixture := range []struct{ name, path, source string }{
		{"hidden mint", "internal/agents/retainedprivacy/producer/hidden.go", "//go:build unavailable\n\npackage producer\nimport s \"" + retainedDomain + "/internal/sealed\"\nvar Issuer=s.New\n"},
		{"mint callback", "internal/agents/retainedprivacy/producer/transform.go", "package producer\nimport s \"" + retainedDomain + "/internal/sealed\"\nfunc Transform(){f:=s.New;_ = f}\n"},
		{"external sealed alias", "internal/agents/retainedprivacy/extra.go", "package retainedprivacy\nimport s \"" + retainedDomain + "/internal/sealed\"\ntype Issuer=s.Proof\n"},
		{"external producer", "internal/collector/alternate.go", "package collector\nimport p \"" + retainedDomain + "/producer\"\nvar Transform=p.Transform\n"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if len(retainedBoundaryProblems(fixture.path, []byte(fixture.source))) == 0 {
				t.Fatal("source authority escape accepted")
			}
		})
	}
}
