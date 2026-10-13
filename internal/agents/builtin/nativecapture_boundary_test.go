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
	ImportPath     string   `json:"ImportPath"`
	Dir            string   `json:"Dir"`
	Export         string   `json:"Export"`
	Name           string   `json:"Name"`
	GoFiles        []string `json:"GoFiles"`
	TestGoFiles    []string `json:"TestGoFiles"`
	XTestGoFiles   []string `json:"XTestGoFiles"`
	IgnoredGoFiles []string `json:"IgnoredGoFiles"`
}

func TestNativeCaptureWholeSourceBoundary(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "list", "-p=1", "-deps", "-test", "-export", "-json", "github.com/wangjohn/agent-archive/...")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var pkgs []capturePackage
	exports := map[string]string{}
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
		// Test variants supply test-only dependency exports. Canonical packages
		// below already include their complete TestGoFiles and profile checks.
		if strings.Contains(p.ImportPath, " [") || strings.HasSuffix(p.ImportPath, ".test") {
			continue
		}
		pkgs = append(pkgs, p)
		if len(p.XTestGoFiles) > 0 {
			external := p
			external.ImportPath += "_test"
			external.GoFiles = append([]string(nil), p.XTestGoFiles...)
			external.TestGoFiles = nil
			external.XTestGoFiles = nil
			external.IgnoredGoFiles = nil
			pkgs = append(pkgs, external)
		}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file := exports[path]
		if file == "" {
			return nil, fmt.Errorf("missing compiled export for %s", path)
		}
		return os.Open(file)
	})
	for _, p := range pkgs {
		if !strings.HasPrefix(p.ImportPath, "github.com/wangjohn/agent-archive/") {
			continue
		}
		files := append(append([]string{}, p.GoFiles...), p.TestGoFiles...)
		var parsed []*ast.File
		for _, name := range files {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			parsed = append(parsed, f)
		}
		// Uncompiled profiles remain fail closed for either capture boundary.
		for _, name := range p.IgnoredGoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
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
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, 0)
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
		info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
		cfg := types.Config{Importer: imp}
		if _, err := cfg.Check(p.ImportPath, fset, parsed, info); err != nil {
			t.Fatal(err)
		}
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
			if obj.Pkg() != nil && obj.Pkg().Path() == captureDomainPath && !calls[sel] && p.ImportPath != captureDomainPath && p.ImportPath != "github.com/wangjohn/agent-archive/internal/agents/cursor" {
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
				if captureAdapterConstructor(obj) {
					if !captureAdapterReferenceAllowed(p.ImportPath, rel, fixture) {
						t.Errorf("forbidden native capture adapter constructor %s at %s", obj.Name(), fset.Position(id.Pos()))
					}
					continue
				}
				if obj.Pkg() == nil || obj.Pkg().Path() != captureDomainPath || !obj.Exported() {
					continue
				}
				if captureObjectAllowed(p.ImportPath, obj, fixture) {
					continue
				}
				t.Errorf("forbidden native capture symbol %s at %s", obj.Name(), fset.Position(id.Pos()))
			}
		}
	}
}

func captureFixtureAllowed(pkg, file string) bool {
	switch pkg {
	case "github.com/wangjohn/agent-archive/internal/state":
		return file == "settled_firstsave_test.go" || file == "settled_capture_provenance_test.go"
	case "github.com/wangjohn/agent-archive/internal/backfill":
		return file == "settled_cursor_import_test.go"
	case "github.com/wangjohn/agent-archive/internal/agents/builtin":
		return file == "nativecapture_binding_test.go"
	default:
		return false
	}
}

func captureSymbolAllowed(pkg, name string, fixture bool) bool {
	if pkg == captureDomainPath {
		return true
	}
	if pkg == "github.com/wangjohn/agent-archive/internal/agents/cursor" || fixture {
		return name == "CaptureSettledCursor" || name == "Proof" || name == "WithVerifiedCapture" || name == "Close"
	}
	return name == "Proof" || name == "WithVerifiedCapture" || name == "Close"
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
	if pkg == captureDomainPath || pkg == "github.com/wangjohn/agent-archive/internal/agents/cursor" || fixture {
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
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == captureDomainPath && named.Obj().Name() == "Proof"
}

// These typed source probes check architecture restrictions, not native mint authority.
func TestNativeCaptureTypedAliasAndMethodValueControls(t *testing.T) {
	domain := types.NewPackage(captureDomainPath, "nativecapture")
	proofObj := types.NewTypeName(token.NoPos, domain, "Proof", nil)
	proof := types.NewNamed(proofObj, types.NewStruct(nil, nil), nil)
	domain.Scope().Insert(proofObj)
	closeMethod := types.NewFunc(token.NoPos, domain, "Close", types.NewSignatureType(types.NewVar(token.NoPos, domain, "p", types.NewPointer(proof)), nil, nil, types.NewTuple(), types.NewTuple(), false))
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
				if obj.Pkg() != nil && obj.Pkg().Path() == captureDomainPath && !calls[selector] {
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
	return path == captureDomainPath || path == captureCursorAdapterPath
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
