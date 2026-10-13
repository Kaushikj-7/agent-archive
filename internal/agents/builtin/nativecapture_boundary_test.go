package builtin

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const captureDomainPath = "github.com/wangjohn/agent-archive/internal/agents/nativecapture"

type capturePackage struct {
	ImportPath      string   `json:"ImportPath"`
	ForTest         string   `json:"ForTest"`
	Dir             string   `json:"Dir"`
	Export          string   `json:"Export"`
	Name            string   `json:"Name"`
	GoFiles         []string `json:"GoFiles"`
	CgoFiles        []string `json:"CgoFiles"`
	CompiledGoFiles []string `json:"CompiledGoFiles"`
	TestGoFiles     []string `json:"TestGoFiles"`
	XTestGoFiles    []string `json:"XTestGoFiles"`
	IgnoredGoFiles  []string `json:"IgnoredGoFiles"`
}

func TestNativeCaptureWholeSourceBoundary(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "list", "-p=1", "-deps", "-compiled", "-test", "-export", "-json", "github.com/wangjohn/agent-archive/...")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var pkgs []capturePackage
	exports := map[string]string{}
	variantImports := map[string]map[string]string{}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	for {
		var p capturePackage
		err := decoder.Decode(&p)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
		if p.ForTest != "" {
			if variantImports[p.ForTest] == nil {
				variantImports[p.ForTest] = map[string]string{}
			}
			plain, _, _ := strings.Cut(p.ImportPath, " [")
			variantImports[p.ForTest][plain] = p.ImportPath
		}
		// Test variants supply test-only dependency exports. Canonical packages
		// below already include their complete TestGoFiles and profile checks.
		if strings.Contains(p.ImportPath, " [") || strings.HasSuffix(p.ImportPath, ".test") {
			continue
		}
		pkgs = append(pkgs, p)
		if len(p.XTestGoFiles) > 0 {
			external := p
			external.ForTest = p.ImportPath
			external.ImportPath += "_test"
			external.GoFiles = append([]string(nil), p.XTestGoFiles...)
			external.CompiledGoFiles = append([]string(nil), p.XTestGoFiles...)
			external.CgoFiles = nil
			external.TestGoFiles = nil
			external.XTestGoFiles = nil
			external.IgnoredGoFiles = nil
			pkgs = append(pkgs, external)
		}
	}
	captureScanOriginalAuthority(t, pkgs)
	fset := token.NewFileSet()
	for _, p := range pkgs {
		if !strings.HasPrefix(p.ImportPath, "github.com/wangjohn/agent-archive/") {
			continue
		}
		files := append(append([]string{}, p.CompiledGoFiles...), p.TestGoFiles...)
		var parsed []*ast.File
		for _, name := range files {
			f, err := parser.ParseFile(fset, captureSourcePath(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			parsed = append(parsed, f)
		}
		// Uncompiled profiles remain fail closed for either capture boundary.
		for _, name := range p.IgnoredGoFiles {
			f, err := parser.ParseFile(fset, captureSourcePath(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, i := range f.Imports {
				if captureBoundaryImport(strings.Trim(i.Path.Value, "\"")) {
					t.Fatalf("unqualified build-profile domain import %s", filepath.Join(p.Dir, name))
				}
			}
		}
		// External tests use the same typed wrapper checks below. Keep the
		// original direct authority-domain import refusal for those tests.
		for _, name := range p.XTestGoFiles {
			f, err := parser.ParseFile(fset, captureSourcePath(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, i := range f.Imports {
				if strings.Trim(i.Path.Value, "\"") == captureDomainPath {
					t.Fatalf("unqualified build-profile domain import %s", filepath.Join(p.Dir, name))
				}
			}
		}
		if len(parsed) == 0 {
			continue
		}
		// Each test context has its own compiler-selected dependency variants.
		// A fresh importer avoids reusing a production-only type from another
		// package's context when external tests need test-exported methods.
		testContext := p.ForTest
		if testContext == "" {
			testContext = p.ImportPath
		}
		imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
			if variant := variantImports[testContext][path]; variant != "" {
				path = variant
			}
			file := exports[path]
			if file == "" {
				return nil, fmt.Errorf("missing compiled export for %s", path)
			}
			return os.Open(file)
		})
		info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
		cfg := types.Config{Importer: imp}
		checked, err := cfg.Check(p.ImportPath, fset, parsed, info)
		if err != nil {
			t.Fatal(err)
		}
		captureCheckExports(t, checked)
		calls := map[ast.Expr]bool{}
		for _, f := range parsed {
			ast.Inspect(f, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					calls[c.Fun] = true
				}
				return true
			})
		}
		for sel, selection := range info.Selections {
			obj := selection.Obj()
			if obj.Pkg() != nil && obj.Pkg().Path() == captureSealedPath && !calls[sel] && p.ImportPath != captureSealedPath && p.ImportPath != captureProducerPath && p.ImportPath != "github.com/wangjohn/agent-archive/internal/agents/cursor" {
				t.Errorf("domain method value forbidden at %s", fset.Position(sel.Pos()))
			}
		}
		for _, f := range parsed {
			path := fset.Position(f.Pos()).Filename
			rel, err := filepath.Rel(p.Dir, path)
			if err != nil {
				t.Fatal(err)
			}
			fixture := captureFixtureAllowed(p.ImportPath, rel)
			for _, i := range f.Imports {
				if captureBoundaryImport(strings.Trim(i.Path.Value, "\"")) && i.Name != nil && i.Name.Name == "." {
					t.Errorf("domain dot import forbidden: %s", path)
				}
			}
			for id, obj := range info.Uses {
				if id.Pos() < f.Pos() || id.Pos() > f.End() {
					continue
				}
				if obj.Pkg() != nil && obj.Pkg().Path() == captureProducerPath && obj.Exported() {
					if p.ImportPath != captureCursorAdapterPath || rel != "settled_import.go" || obj.Name() != "CaptureSettledCursor" || !captureDirectCallObject(f, id, "CaptureSettledRequest", false) {
						t.Errorf("forbidden fixed producer reference at %s", fset.Position(id.Pos()))
					}
					continue
				}
				if obj.Pkg() != nil && obj.Pkg().Path() == captureSealedPath && obj.Name() == "New" && (p.ImportPath != captureProducerPath || !captureDirectCallObject(f, id, "CaptureSettledCursor", true)) {
					t.Errorf("issuer mint outside final capture return at %s", fset.Position(id.Pos()))
					continue
				}
				if captureAdapterConstructor(obj) {
					if !captureAdapterReferenceAllowed(p.ImportPath, rel, fixture) {
						t.Errorf("forbidden native capture adapter constructor %s at %s", obj.Name(), fset.Position(id.Pos()))
					}
					continue
				}
				if obj.Pkg() == nil || (obj.Pkg().Path() != captureDomainPath && obj.Pkg().Path() != captureSealedPath) || !obj.Exported() {
					continue
				}
				if captureAuthorityObjectAllowed(p.ImportPath, rel, obj, calls, id) || captureObjectAllowed(p.ImportPath, obj, fixture) && obj.Pkg().Path() != captureSealedPath {
					continue
				}
				t.Errorf("forbidden native capture symbol %s at %s", obj.Name(), fset.Position(id.Pos()))
			}
		}
	}
}

type capturePackagePath string

const (
	capturePackageDomain   capturePackagePath = captureDomainPath
	capturePackageCursor   capturePackagePath = captureCursorAdapterPath
	capturePackageState    capturePackagePath = "github.com/wangjohn/agent-archive/internal/state"
	capturePackageBackfill capturePackagePath = "github.com/wangjohn/agent-archive/internal/backfill"
	capturePackageBuiltin  capturePackagePath = "github.com/wangjohn/agent-archive/internal/agents/builtin"
)

type captureFixtureFile string

const (
	captureFixtureFirstSave  captureFixtureFile = "settled_firstsave_test.go"
	captureFixtureProvenance captureFixtureFile = "settled_capture_provenance_test.go"
	captureFixtureImport     captureFixtureFile = "settled_cursor_import_test.go"
	captureFixtureBinding    captureFixtureFile = "nativecapture_binding_test.go"
)

type captureSymbolName string

const (
	captureSymbolCursor captureSymbolName = "CaptureSettledCursor"
	captureSymbolProof  captureSymbolName = "Proof"
	captureSymbolLoan   captureSymbolName = "WithVerifiedCapture"
	captureSymbolClose  captureSymbolName = "Close"
)

func captureFixtureAllowed(pkg, file string) bool {
	switch capturePackagePath(pkg) {
	case capturePackageState:
		return captureFixtureFile(file) == captureFixtureFirstSave || captureFixtureFile(file) == captureFixtureProvenance
	case capturePackageBackfill:
		return captureFixtureFile(file) == captureFixtureImport
	case capturePackageBuiltin:
		return captureFixtureFile(file) == captureFixtureBinding
	case capturePackageDomain, capturePackageCursor:
		return false
	default:
		return false
	}
}

func captureSymbolAllowed(pkg, name string, fixture bool) bool {
	if capturePackagePath(pkg) == capturePackageDomain {
		return true
	}
	symbol := captureSymbolName(name)
	if capturePackagePath(pkg) == capturePackageCursor || fixture {
		return symbol == captureSymbolCursor || symbol == captureSymbolProof || symbol == captureSymbolLoan || symbol == captureSymbolClose
	}
	return symbol == captureSymbolProof || symbol == captureSymbolLoan || symbol == captureSymbolClose
}

func TestNativeCaptureSymbolBoundaryControls(t *testing.T) {
	const core = "github.com/wangjohn/agent-archive/internal/state"
	for _, name := range []string{"CaptureSettledCursor", "UnexpectedExport", "NativeReader", "RegisterIssuer"} {
		if captureSymbolAllowed(core, name, false) {
			t.Fatalf("core symbol allowed: %s", name)
		}
	}
	for _, name := range []string{"Proof", "WithVerifiedCapture", "Close"} {
		if !captureSymbolAllowed(core, name, false) {
			t.Fatalf("opaque consumer refused: %s", name)
		}
	}
	if captureFixtureAllowed(core, "unlisted_test.go") {
		t.Fatal("broad test exception")
	}
	if !captureSymbolAllowed("github.com/wangjohn/agent-archive/internal/agents/cursor", "CaptureSettledCursor", false) {
		t.Fatal("fixed adapter refused")
	}
}

func captureObjectAllowed(pkg string, obj types.Object, fixture bool) bool {
	if !captureSymbolAllowed(pkg, obj.Name(), fixture) {
		return false
	}
	if capturePackagePath(pkg) == capturePackageDomain || capturePackagePath(pkg) == capturePackageCursor || fixture {
		return true
	}
	if obj.Name() == "Proof" {
		_, ok := obj.(*types.TypeName)
		return ok
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return false
	}
	signature, ok := fn.Type().(*types.Signature)
	if !ok || signature.Recv() == nil {
		return false
	}
	receiver := types.Unalias(signature.Recv().Type())
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = types.Unalias(pointer.Elem())
	}
	named, ok := receiver.(*types.Named)
	return ok && named.Obj().Pkg() != nil && (named.Obj().Pkg().Path() == captureDomainPath || named.Obj().Pkg().Path() == captureSealedPath) && named.Obj().Name() == "Proof"
}

// These typed source probes check architecture restrictions, not native mint authority.
func TestNativeCaptureTypedAliasAndMethodValueControls(t *testing.T) {
	domain := types.NewPackage(captureDomainPath, "nativecapture")
	sealed := types.NewPackage(captureSealedPath, "sealed")
	proofObj := types.NewTypeName(token.NoPos, sealed, "Proof", nil)
	proof := types.NewNamed(proofObj, types.NewStruct(nil, nil), nil)
	aliasObj := types.NewTypeName(token.NoPos, domain, "Proof", nil)
	types.NewAlias(aliasObj, proof)
	domain.Scope().Insert(aliasObj)
	closeMethod := types.NewFunc(token.NoPos, sealed, "Close", types.NewSignatureType(types.NewVar(token.NoPos, domain, "p", types.NewPointer(proof)), nil, nil, types.NewTuple(), types.NewTuple(), false))
	proof.AddMethod(closeMethod)
	ctor := types.NewFunc(token.NoPos, domain, "CaptureSettledCursor", types.NewSignatureType(nil, nil, nil, types.NewTuple(), types.NewTuple(types.NewVar(token.NoPos, domain, "", types.NewPointer(proof))), false))
	domain.Scope().Insert(ctor)
	domain.MarkComplete()
	tests := []struct {
		name          string
		source        string
		wantViolation bool
	}{
		{name: "qualified-function-alias", source: `package sample;import alias "` + captureDomainPath + `";var ctor=alias.CaptureSettledCursor`, wantViolation: true},
		{name: "dot-import", source: `package sample;import . "` + captureDomainPath + `";var ctor=CaptureSettledCursor`, wantViolation: true},
		{name: "opaque-type-alias-loan", source: `package sample;import alias "` + captureDomainPath + `";type P=alias.Proof;func use(p *P){p.Close()}`, wantViolation: false},
		{name: "opaque-method-expression", source: `package sample;import alias "` + captureDomainPath + `";var closeLater=(*alias.Proof).Close`, wantViolation: true},
		{name: "opaque-method-value", source: `package sample;import alias "` + captureDomainPath + `";type P=alias.Proof;var p *P;var closeLater=p.Close`, wantViolation: true},
		{name: "shadowed-package-name", source: `package sample;import alias "` + captureDomainPath + `";var p *alias.Proof;type local struct{};func(local)Close(){};func use(){alias:=local{};alias.Close()}`, wantViolation: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "probe.go", test.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
			cfg := types.Config{Importer: captureProbeImporter{domain: domain}}
			if _, err = cfg.Check("sample", fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			violation := false
			for _, i := range file.Imports {
				if i.Name != nil && i.Name.Name == "." {
					violation = true
				}
			}
			for _, obj := range info.Uses {
				if obj.Pkg() != nil && obj.Pkg().Path() == captureDomainPath && obj.Exported() && !captureObjectAllowed("sample", obj, false) {
					violation = true
				}
			}
			calls := map[ast.Expr]bool{}
			ast.Inspect(file, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					calls[call.Fun] = true
				}
				return true
			})
			for selector, selection := range info.Selections {
				obj := selection.Obj()
				if obj.Pkg() != nil && (obj.Pkg().Path() == captureDomainPath || obj.Pkg().Path() == captureSealedPath) && !calls[selector] {
					violation = true
				}
			}
			if violation != test.wantViolation {
				t.Fatalf("typed boundary violation=%v want=%v", violation, test.wantViolation)
			}
		})
	}
}

type captureProbeImporter struct{ domain *types.Package }

func (i captureProbeImporter) Import(path string) (*types.Package, error) {
	if path != captureDomainPath {
		return nil, fmt.Errorf("unexpected probe import %s", path)
	}
	return i.domain, nil
}

const captureCursorAdapterPath = "github.com/wangjohn/agent-archive/internal/agents/cursor"

func captureBoundaryImport(path string) bool {
	return capturePackagePath(path) == capturePackageDomain || capturePackagePath(path) == capturePackageCursor
}

func captureAdapterConstructor(obj types.Object) bool {
	if obj.Pkg() == nil || obj.Pkg().Path() != captureCursorAdapterPath {
		return false
	}
	function, ok := obj.(*types.Func)
	if !ok {
		return false
	}
	signature, ok := function.Type().(*types.Signature)
	return ok && signature.Recv() == nil && (obj.Name() == "CaptureSettledRequest" || obj.Name() == "CaptureSettledImport")
}

func captureAdapterReferenceAllowed(pkg, file string, fixture bool) bool {
	return fixture || pkg == captureCursorAdapterPath && file == "settled_import.go" || pkg == "github.com/wangjohn/agent-archive/internal/agents/builtin" && file == "registry.go"
}

func TestNativeCaptureAdapterWrapperTypedControls(t *testing.T) {
	adapter := types.NewPackage(captureCursorAdapterPath, "cursor")
	for _, name := range []string{"CaptureSettledRequest", "CaptureSettledImport"} {
		adapter.Scope().Insert(types.NewFunc(token.NoPos, adapter, name, types.NewSignatureType(nil, nil, nil, types.NewTuple(), types.NewTuple(), false)))
	}
	adapter.MarkComplete()
	for _, test := range []struct {
		name   string
		source string
	}{
		{name: "renamed-function-value", source: `package sample;import renamed "` + captureCursorAdapterPath + `";var ctor=renamed.CaptureSettledRequest`},
		{name: "legacy-function-value", source: `package sample;import renamed "` + captureCursorAdapterPath + `";var ctor=renamed.CaptureSettledImport`},
		{name: "direct-call", source: `package sample;import renamed "` + captureCursorAdapterPath + `";func use(){renamed.CaptureSettledRequest()}`},
		{name: "dot-alias", source: `package sample;import . "` + captureCursorAdapterPath + `";var ctor=CaptureSettledRequest`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "unlisted_test.go", test.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
			cfg := types.Config{Importer: captureAdapterProbeImporter{adapter: adapter}}
			if _, err = cfg.Check("sample", fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			seen := false
			for _, obj := range info.Uses {
				if captureAdapterConstructor(obj) {
					seen = true
					if captureAdapterReferenceAllowed("github.com/wangjohn/agent-archive/internal/state", "unlisted_test.go", false) {
						t.Fatal("unlisted adapter constructor admitted")
					}
					if !captureAdapterReferenceAllowed("github.com/wangjohn/agent-archive/internal/state", "settled_firstsave_test.go", true) {
						t.Fatal("designated fixture refused")
					}
				}
			}
			if !seen {
				t.Fatal("typed constructor probe did not resolve actual adapter object")
			}
		})
	}
}

type captureAdapterProbeImporter struct{ adapter *types.Package }

func (i captureAdapterProbeImporter) Import(path string) (*types.Package, error) {
	if path != captureCursorAdapterPath {
		return nil, fmt.Errorf("unexpected adapter import %s", path)
	}
	return i.adapter, nil
}

const captureSealedPath = captureDomainPath + "/internal/sealed"
const captureProducerPath = captureDomainPath + "/producer"

func captureSourcePath(dir, name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(dir, name)
}

func captureAuthorityImportAllowed(pkg, file, imported string) bool {
	switch imported {
	case captureSealedPath:
		return pkg == captureDomainPath && file == "proof.go" || pkg == captureProducerPath && file == "cursor.go"
	case captureProducerPath:
		return pkg == captureCursorAdapterPath && file == "settled_import.go"
	default:
		return true
	}
}

// Walk original source independently of go list: a directory with only ignored
// profiles must not acquire authority merely because no package was selected.
func captureScanOriginalAuthority(t *testing.T, packages []capturePackage) {
	selectedDirs := map[string]bool{}
	for _, pkg := range packages {
		selectedDirs[pkg.Dir] = true
	}
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	if err := captureOriginalAuthorityError(root, selectedDirs); err != nil {
		t.Fatal(err)
	}
}

func captureOriginalAuthorityError(root string, selectedDirs map[string]bool) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := "github.com/wangjohn/agent-archive/" + filepath.ToSlash(rel)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		if pkg == captureDomainPath {
			if entry.Name() != "proof.go" || len(f.Decls) != 2 || len(f.Imports) != 1 {
				return fmt.Errorf("neutral facade shape changed in %s", path)
			}
			decl, ok := f.Decls[1].(*ast.GenDecl)
			if !ok || decl.Tok != token.TYPE || len(decl.Specs) != 1 {
				return fmt.Errorf("neutral facade declaration in %s", path)
			}
			alias, ok := decl.Specs[0].(*ast.TypeSpec)
			if !ok || alias.Name.Name != "Proof" || !alias.Assign.IsValid() {
				return fmt.Errorf("neutral facade must only alias Proof in %s", path)
			}
		}
		for _, spec := range f.Imports {
			imported := strings.Trim(spec.Path.Value, "\"")
			if captureBoundaryImport(imported) && !selectedDirs[filepath.Dir(path)] {
				return fmt.Errorf("unqualified ignored-only directory domain import in %s", path)
			}
			if captureBoundaryImport(imported) && spec.Name != nil && spec.Name.Name == "." {
				return fmt.Errorf("domain dot import in %s", path)
			}
			if !captureAuthorityImportAllowed(pkg, entry.Name(), imported) {
				return fmt.Errorf("forbidden issuer/producer import %s in %s", imported, path)
			}
			if (imported == captureSealedPath || imported == captureProducerPath) && spec.Name != nil && spec.Name.Name == "." {
				return fmt.Errorf("authority dot import in %s", path)
			}
		}
		return nil
	})
}

func captureAuthorityObjectAllowed(pkg, file string, obj types.Object, calls map[ast.Expr]bool, id *ast.Ident) bool {
	if obj.Pkg() == nil || obj.Pkg().Path() != captureSealedPath {
		return false
	}
	if pkg == captureDomainPath && file == "proof.go" {
		name, ok := obj.(*types.TypeName)
		return ok && name.Name() == "Proof"
	}
	if pkg == captureSealedPath {
		return true
	}
	if pkg == captureProducerPath && file == "cursor.go" {
		switch obj.Name() {
		case "Proof", "Reservation":
			_, ok := obj.(*types.TypeName)
			return ok
		case "Reserve", "Release", "Available", "Refuse":
			fn, ok := obj.(*types.Func)
			if !ok {
				return false
			}
			signature, ok := fn.Type().(*types.Signature)
			if !ok || signature.Recv() == nil {
				return false
			}
			receiver := types.Unalias(signature.Recv().Type())
			if pointer, ok := receiver.(*types.Pointer); ok {
				receiver = types.Unalias(pointer.Elem())
			}
			named, ok := receiver.(*types.Named)
			if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != captureSealedPath || named.Obj().Name() != "Reservation" {
				return false
			}
			for expression := range calls {
				if expression.Pos() <= id.Pos() && id.End() <= expression.End() {
					return true
				}
			}
			return false
		case "New", "NewReservation", "Digest":
			fn, ok := obj.(*types.Func)
			if !ok {
				return false
			}
			signature, ok := fn.Type().(*types.Signature)
			if !ok || signature.Recv() != nil {
				return false
			}
			for expression := range calls {
				if expression.Pos() <= id.Pos() && id.End() <= expression.End() {
					return true
				}
			}
		}
	}
	return captureObjectAllowed(pkg, obj, false) && (obj.Name() == "Proof" || obj.Name() == "Close" || obj.Name() == "WithVerifiedCapture")
}

func captureCheckExports(t *testing.T, pkg *types.Package) {
	t.Helper()
	var allowed map[string]bool
	switch pkg.Path() {
	case captureDomainPath:
		allowed = map[string]bool{"Proof": true}
	case captureSealedPath:
		allowed = map[string]bool{"Proof": true, "Reservation": true, "New": true, "NewReservation": true, "Digest": true}
	case captureProducerPath:
		allowed = map[string]bool{"CaptureSettledCursor": true}
	default:
		return
	}
	for _, name := range pkg.Scope().Names() {
		object := pkg.Scope().Lookup(name)
		if object.Exported() && !allowed[name] {
			t.Errorf("unexpected authority export %s.%s", pkg.Path(), name)
		}
	}
	if pkg.Path() == captureProducerPath {
		fn, ok := pkg.Scope().Lookup("CaptureSettledCursor").(*types.Func)
		if !ok {
			t.Error("fixed producer must be a function")
			return
		}
		signature, ok := fn.Type().(*types.Signature)
		if !ok || signature.Recv() != nil || signature.Variadic() || signature.Params().Len() != 1 || signature.Results().Len() != 2 {
			t.Error("fixed producer signature changed")
			return
		}
		if types.TypeString(signature.Params().At(0).Type(), func(p *types.Package) string { return p.Path() }) != "*github.com/wangjohn/agent-archive/internal/nativefacts.CaptureRequest" || types.TypeString(signature.Results().At(0).Type(), func(p *types.Package) string { return p.Path() }) != "*"+captureSealedPath+".Proof" || !types.Identical(signature.Results().At(1).Type(), types.Universe.Lookup("error").Type()) {
			t.Error("fixed producer accepts or returns arbitrary capture authority")
		}
	}
	if pkg.Path() == captureSealedPath {
		for _, typeName := range []string{"Proof", "Reservation"} {
			object, ok := pkg.Scope().Lookup(typeName).(*types.TypeName)
			if !ok {
				t.Errorf("sealed %s is not a type", typeName)
				continue
			}
			named, ok := types.Unalias(object.Type()).(*types.Named)
			if !ok {
				t.Errorf("sealed %s is not concrete", typeName)
				continue
			}
			for i := 0; i < named.NumMethods(); i++ {
				name := named.Method(i).Name()
				permitted := typeName == "Proof" && (name == "Close" || name == "WithVerifiedCapture") || typeName == "Reservation" && (name == "Reserve" || name == "Release" || name == "Available" || name == "Refuse")
				if named.Method(i).Exported() && !permitted {
					t.Errorf("unexpected sealed method %s.%s", typeName, name)
				}
			}
		}
		for _, name := range []string{"New", "NewReservation", "Digest"} {
			fn, ok := pkg.Scope().Lookup(name).(*types.Func)
			if !ok {
				t.Errorf("sealed %s must be a fixed function", name)
				continue
			}
			signature, ok := fn.Type().(*types.Signature)
			if !ok || signature.Recv() != nil {
				t.Errorf("sealed %s has wrong kind", name)
			}
		}
	}
	if pkg.Path() == captureDomainPath {
		name, ok := pkg.Scope().Lookup("Proof").(*types.TypeName)
		if !ok || !name.IsAlias() {
			t.Error("neutral Proof must be an opaque sealed alias")
			return
		}
		named, ok := types.Unalias(name.Type()).(*types.Named)
		if !ok || named.Obj().Pkg().Path() != captureSealedPath || named.Obj().Name() != "Proof" {
			t.Error("neutral Proof aliases wrong authority")
		}
	}
}

func TestNativeCaptureIssuerImporterControls(t *testing.T) {
	for _, test := range []struct {
		pkg, file, imported string
		allowed             bool
	}{
		{captureDomainPath, "proof.go", captureSealedPath, true},
		{captureProducerPath, "cursor.go", captureSealedPath, true},
		{captureCursorAdapterPath, "settled_import.go", captureProducerPath, true},
		{captureDomainPath, "factory.go", captureSealedPath, false},
		{captureDomainPath, "proof_test.go", captureSealedPath, false},
		{captureDomainPath + "/hidden", "ignored_darwin.go", captureSealedPath, false},
		{captureDomainPath + "/hidden_test", "external_test.go", captureSealedPath, false},
		{captureDomainPath, "proof.go", captureProducerPath, false},
		{capturePackageStateString(), "import_stage.go", captureProducerPath, false},
	} {
		if captureAuthorityImportAllowed(test.pkg, test.file, test.imported) != test.allowed {
			t.Fatalf("issuer import policy %s/%s", test.pkg, test.file)
		}
	}
}

func capturePackageStateString() string { return string(capturePackageState) }

func captureDirectCallObject(file *ast.File, id *ast.Ident, function string, finalReturn bool) bool {
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != function || fn.Recv != nil || fn.Body == nil {
			continue
		}
		accepted := false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if _, nested := node.(*ast.FuncLit); nested {
				return false
			}
			if finalReturn {
				ret, ok := node.(*ast.ReturnStmt)
				if !ok || len(ret.Results) == 0 || len(fn.Body.List) == 0 || fn.Body.List[len(fn.Body.List)-1] != ret {
					return true
				}
				call, ok := ret.Results[0].(*ast.CallExpr)
				if ok && call.Fun.Pos() <= id.Pos() && id.End() <= call.Fun.End() {
					accepted = true
				}
			} else if call, ok := node.(*ast.CallExpr); ok && call.Fun.Pos() <= id.Pos() && id.End() <= call.Fun.End() {
				accepted = true
			}
			return true
		})
		if accepted {
			return true
		}
	}
	return false
}

func TestNativeCaptureFinalIssuerCallControls(t *testing.T) {
	for _, test := range []struct {
		name, body string
		allowed    bool
	}{
		{"final-mint", `func CaptureSettledCursor(){return issuer.New(),nil}`, true},
		{"renamed-final-mint", `func CaptureSettledCursor(){return renamed.New(),nil}`, true},
		{"function-value", `func CaptureSettledCursor(){mint:=issuer.New;_ = mint}`, false},
		{"extra-wrapper", `func AnotherCapture(){return issuer.New(),nil}`, false},
		{"nested-mint", `func CaptureSettledCursor(){f:=func(){return issuer.New(),nil};_=f}`, false},
		{"nonfinal-mint", `func CaptureSettledCursor(){p:=issuer.New();return p,nil}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "cursor.go", "package probe;"+test.body, 0)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			ast.Inspect(file, func(node ast.Node) bool {
				if id, ok := node.(*ast.Ident); ok && id.Name == "New" {
					found = true
					if captureDirectCallObject(file, id, "CaptureSettledCursor", true) != test.allowed {
						t.Fatal("final issuer restriction changed")
					}
				}
				return true
			})
			if !found {
				t.Fatal("probe lacks issuer reference")
			}
		})
	}
}

func TestNativeCaptureIgnoredOnlyDirectoryIssuerControl(t *testing.T) {
	root := t.TempDir()
	hidden := filepath.Join(root, "internal", "agents", "nativecapture", "unlisted")
	if err := os.MkdirAll(hidden, 0700); err != nil {
		t.Fatal(err)
	}
	source := "//go:build unavailable_profile\n\npackage unlisted\nimport renamed \"" + captureSealedPath + "\"\nvar mint = renamed.New\n"
	if err := os.WriteFile(filepath.Join(hidden, "issuer.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if err := captureOriginalAuthorityError(root, map[string]bool{}); err == nil || !strings.Contains(err.Error(), "forbidden issuer/producer import") {
		t.Fatal("ignored-only issuer directory escaped", err)
	}
}
