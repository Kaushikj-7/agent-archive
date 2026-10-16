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
	// The independently required plain gate runs this exact body without race
	// instrumentation. Local short runs retain the audit unless that gate owns it.
	if testing.Short() && os.Getenv("AGENT_ARCHIVE_PLAIN_SOURCE_AUDIT_OWNED") == "1" {
		t.Skip("whole-source audit owned by required plain source-audit gate; race body NOT RUN")
	}
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
		captureCheckExports(t, checked, fset)
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
				if obj.Pkg() != nil && obj.Pkg().Path() == captureFactsPath && obj.Name() == "BindLiveCaptureRequest" {
					if !captureLiveIssuerReferenceAllowed(p.ImportPath, rel, obj, f, id, info) {
						t.Errorf("live owner issuer outside exact state binding at %s", fset.Position(id.Pos()))
					}
					continue
				}
				if obj.Pkg() != nil && obj.Pkg().Path() == captureCodecPath && obj.Exported() {
					if !captureCodecObjectAllowed(p.ImportPath, rel, obj, calls, id) {
						t.Errorf("forbidden codec bridge object %s at %s", obj.Name(), fset.Position(id.Pos()))
					}
					continue
				}
				if obj.Pkg() != nil && obj.Pkg().Path() == captureProducerPath && obj.Exported() {
					if !captureProducerReferenceAllowed(p.ImportPath, rel, obj, f, id) {
						t.Errorf("forbidden fixed producer reference at %s", fset.Position(id.Pos()))
					}
					continue
				}
				if obj.Pkg() != nil && obj.Pkg().Path() == captureSealedPath && (obj.Name() == "New" || obj.Name() == "NewLive") && !captureMintReferenceAllowed(p.ImportPath, rel, obj, f, id) {
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
	capturePackageSealed   capturePackagePath = captureSealedPath
	capturePackageProducer capturePackagePath = captureProducerPath
	capturePackageCodec    capturePackagePath = captureCodecPath
	capturePackageBuiltin  capturePackagePath = "github.com/wangjohn/agent-archive/internal/agents/builtin"
)

type captureFixtureFile string

const (
	captureFixtureFirstSave  captureFixtureFile = "settled_firstsave_test.go"
	captureFixtureProvenance captureFixtureFile = "settled_capture_provenance_test.go"
	captureFixtureImport     captureFixtureFile = "settled_cursor_import_test.go"
	captureFixtureBinding    captureFixtureFile = "nativecapture_binding_test.go"
)

type captureAuthorityFile string

const (
	captureAuthorityProof       captureAuthorityFile = "proof.go"
	captureAuthorityCursor      captureAuthorityFile = "cursor.go"
	captureAuthorityAdapter     captureAuthorityFile = "settled_import.go"
	captureAuthorityCodec       captureAuthorityFile = "codec.go"
	captureAuthorityLiveCursor  captureAuthorityFile = "live_cursor.go"
	captureAuthorityLiveAdapter captureAuthorityFile = "live_import.go"
	captureAuthorityLiveProof   captureAuthorityFile = "live_proof.go"
)

type captureSymbolName string

const (
	captureSymbolCursor         captureSymbolName = "CaptureSettledCursor"
	captureSymbolLiveCursor     captureSymbolName = "CaptureLiveCursor"
	captureSymbolLiveProof      captureSymbolName = "LiveProof"
	captureSymbolLiveLoan       captureSymbolName = "WithVerifiedLiveCapture"
	captureSymbolNewLive        captureSymbolName = "NewLive"
	captureSymbolProof          captureSymbolName = "Proof"
	captureSymbolLoan           captureSymbolName = "WithVerifiedCapture"
	captureSymbolClose          captureSymbolName = "Close"
	captureSymbolDigest         captureSymbolName = "Digest"
	captureSymbolNewReservation captureSymbolName = "NewReservation"
	captureSymbolNew            captureSymbolName = "New"
	captureSymbolRefuse         captureSymbolName = "Refuse"
	captureSymbolAvailable      captureSymbolName = "Available"
	captureSymbolRelease        captureSymbolName = "Release"
	captureSymbolReserve        captureSymbolName = "Reserve"
	captureSymbolReservation    captureSymbolName = "Reservation"
)

func captureFixtureAllowed(pkg, file string) bool {
	switch capturePackagePath(pkg) {
	case capturePackageState:
		return captureFixtureFile(file) == captureFixtureFirstSave || captureFixtureFile(file) == captureFixtureProvenance
	case capturePackageBackfill:
		return captureFixtureFile(file) == captureFixtureImport
	case capturePackageBuiltin:
		return captureFixtureFile(file) == captureFixtureBinding
	case capturePackageDomain, capturePackageCursor, capturePackageSealed, capturePackageProducer, capturePackageCodec:
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
		return symbol == captureSymbolCursor || symbol == captureSymbolLiveCursor || symbol == captureSymbolProof || symbol == captureSymbolLiveProof || symbol == captureSymbolLoan || symbol == captureSymbolLiveLoan || symbol == captureSymbolClose
	}
	return symbol == captureSymbolProof || symbol == captureSymbolLiveProof || symbol == captureSymbolLoan || symbol == captureSymbolLiveLoan || symbol == captureSymbolClose
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
	if captureSymbolName(obj.Name()) == captureSymbolProof || captureSymbolName(obj.Name()) == captureSymbolLiveProof {
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
	return ok && named.Obj().Pkg() != nil && (named.Obj().Pkg().Path() == captureDomainPath || named.Obj().Pkg().Path() == captureSealedPath) && (named.Obj().Name() == "Proof" || named.Obj().Name() == "LiveProof")
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
	if path != i.domain.Path() {
		return nil, fmt.Errorf("unexpected probe import %s", path)
	}
	return i.domain, nil
}

const captureCursorAdapterPath = "github.com/wangjohn/agent-archive/internal/agents/cursor"

func captureBoundaryImport(path string) bool {
	return path == captureFactsPath || capturePackagePath(path) == capturePackageDomain || capturePackagePath(path) == capturePackageCursor || capturePackagePath(path) == capturePackageSealed || capturePackagePath(path) == capturePackageProducer || capturePackagePath(path) == capturePackageCodec
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
	return ok && signature.Recv() == nil && (obj.Name() == "CaptureSettledRequest" || obj.Name() == "CaptureSettledImport" || obj.Name() == "CaptureLiveRequest")
}

func captureAdapterReferenceAllowed(pkg, file string, fixture bool) bool {
	return fixture || pkg == captureCursorAdapterPath && (captureAuthorityFile(file) == captureAuthorityAdapter || captureAuthorityFile(file) == captureAuthorityLiveAdapter) || pkg == "github.com/wangjohn/agent-archive/internal/agents/builtin" && file == "registry.go"
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

const captureFactsPath = "github.com/wangjohn/agent-archive/internal/nativefacts"

const captureCodecPath = "github.com/wangjohn/agent-archive/internal/agents/capturecodec"

func captureSourcePath(dir, name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(dir, name)
}

func captureAuthorityImportAllowed(pkg, file, imported string) bool {
	switch capturePackagePath(imported) {
	case capturePackageSealed:
		return pkg == captureDomainPath && (captureAuthorityFile(file) == captureAuthorityProof || captureAuthorityFile(file) == captureAuthorityLiveProof) || pkg == captureProducerPath && (captureAuthorityFile(file) == captureAuthorityCursor || captureAuthorityFile(file) == captureAuthorityLiveCursor)
	case capturePackageProducer:
		return pkg == captureCursorAdapterPath && (captureAuthorityFile(file) == captureAuthorityAdapter || captureAuthorityFile(file) == captureAuthorityLiveAdapter)
	case capturePackageCodec:
		return pkg == captureProducerPath && (captureAuthorityFile(file) == captureAuthorityCursor || captureAuthorityFile(file) == captureAuthorityLiveCursor)
	case capturePackageDomain, capturePackageCursor, capturePackageState, capturePackageBackfill, capturePackageBuiltin:
		return true
	default:
		return true
	}
}

// Walk original source independently of go list: a directory with only ignored
// profiles must not acquire authority merely because no package was selected.
func captureScanOriginalAuthority(t *testing.T, packages []capturePackage) {
	t.Helper()
	selectedDirs := map[string]bool{}
	selectedSources := map[string]bool{}
	for _, pkg := range packages {
		selectedDirs[pkg.Dir] = true
		for _, name := range append(append(append(append([]string{}, pkg.GoFiles...), pkg.CgoFiles...), pkg.TestGoFiles...), pkg.XTestGoFiles...) {
			selectedSources[captureSourcePath(pkg.Dir, name)] = true
		}
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	if err := captureOriginalAuthorityError(root, selectedDirs, selectedSources); err != nil {
		t.Fatal(err)
	}
}

func captureOriginalAuthorityError(root string, selectedDirs, selectedSources map[string]bool) error {
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
		if (pkg == captureFactsPath || pkg == captureDomainPath || strings.HasPrefix(pkg, captureDomainPath+"/") || pkg == captureCodecPath || strings.HasPrefix(pkg, captureCodecPath+"/")) && !selectedSources[path] {
			return fmt.Errorf("unqualified closed-domain profile source %s", path)
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		if pkg == captureDomainPath {
			if (captureAuthorityFile(entry.Name()) != captureAuthorityProof && captureAuthorityFile(entry.Name()) != captureAuthorityLiveProof) || len(f.Decls) != 2 || len(f.Imports) != 1 {
				return fmt.Errorf("neutral facade shape changed in %s", path)
			}
			decl, ok := f.Decls[1].(*ast.GenDecl)
			if !ok || decl.Tok != token.TYPE || len(decl.Specs) != 1 {
				return fmt.Errorf("neutral facade declaration in %s", path)
			}
			alias, ok := decl.Specs[0].(*ast.TypeSpec)
			expected := "Proof"
			if captureAuthorityFile(entry.Name()) == captureAuthorityLiveProof {
				expected = "LiveProof"
			}
			if !ok || alias.Name.Name != expected || !alias.Assign.IsValid() {
				return fmt.Errorf("neutral facade must only alias Proof in %s", path)
			}
		}
		if pkg == captureCodecPath || strings.HasPrefix(pkg, captureCodecPath+"/") {
			if pkg != captureCodecPath || captureAuthorityFile(entry.Name()) != captureAuthorityCodec {
				return fmt.Errorf("unlisted codec bridge source %s", path)
			}
		}
		for _, spec := range f.Imports {
			imported := strings.Trim(spec.Path.Value, "\"")
			if pkg == captureCodecPath && !captureCodecDependencyAllowed(imported) {
				return fmt.Errorf("forbidden codec bridge dependency %s in %s", imported, path)
			}
			if captureBoundaryImport(imported) && !selectedDirs[filepath.Dir(path)] {
				return fmt.Errorf("unqualified ignored-only directory domain import in %s", path)
			}
			if captureBoundaryImport(imported) && spec.Name != nil && spec.Name.Name == "." {
				return fmt.Errorf("domain dot import in %s", path)
			}
			if !captureAuthorityImportAllowed(pkg, entry.Name(), imported) {
				return fmt.Errorf("forbidden issuer/producer import %s in %s", imported, path)
			}
			if (capturePackagePath(imported) == capturePackageSealed || capturePackagePath(imported) == capturePackageProducer) && spec.Name != nil && spec.Name.Name == "." {
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
	if pkg == captureDomainPath && (captureAuthorityFile(file) == captureAuthorityProof || captureAuthorityFile(file) == captureAuthorityLiveProof) {
		name, ok := obj.(*types.TypeName)
		return ok && (name.Name() == "Proof" || name.Name() == "LiveProof")
	}
	if pkg == captureSealedPath {
		return true
	}
	if pkg == captureProducerPath && (captureAuthorityFile(file) == captureAuthorityCursor || captureAuthorityFile(file) == captureAuthorityLiveCursor) {
		switch captureSymbolName(obj.Name()) {
		case captureSymbolProof, captureSymbolLiveProof, captureSymbolReservation:
			_, ok := obj.(*types.TypeName)
			return ok
		case captureSymbolReserve, captureSymbolRelease, captureSymbolAvailable, captureSymbolRefuse:
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
				if captureExactCalleeIdent(expression, id) {
					return true
				}
			}
			return false
		case captureSymbolCursor, captureSymbolLoan, captureSymbolClose:
			break
		case captureSymbolNew, captureSymbolNewLive, captureSymbolNewReservation, captureSymbolDigest:
			fn, ok := obj.(*types.Func)
			if !ok {
				return false
			}
			signature, ok := fn.Type().(*types.Signature)
			if !ok || signature.Recv() != nil {
				return false
			}
			for expression := range calls {
				if captureExactCalleeIdent(expression, id) {
					return true
				}
			}
		}
	}
	return captureObjectAllowed(pkg, obj, false) && (captureSymbolName(obj.Name()) == captureSymbolProof || captureSymbolName(obj.Name()) == captureSymbolLiveProof || captureSymbolName(obj.Name()) == captureSymbolClose || captureSymbolName(obj.Name()) == captureSymbolLoan || captureSymbolName(obj.Name()) == captureSymbolLiveLoan)
}

func captureCheckExports(t *testing.T, pkg *types.Package, fset *token.FileSet) {
	t.Helper()
	if pkg.Path() == captureFactsPath {
		captureCheckLiveFactsShape(t, pkg, fset)
		return
	}
	var allowed map[string]bool
	switch capturePackagePath(pkg.Path()) {
	case capturePackageDomain:
		allowed = map[string]bool{"Proof": true, "LiveProof": true}
	case capturePackageSealed:
		allowed = map[string]bool{"Proof": true, "Reservation": true, "New": true, "NewReservation": true, "Digest": true, "LiveProof": true, "NewLive": true}
	case capturePackageProducer:
		allowed = map[string]bool{"CaptureSettledCursor": true, "CaptureLiveCursor": true}
	case capturePackageCodec:
		allowed = captureCodecExports()
	case capturePackageCursor, capturePackageState, capturePackageBackfill, capturePackageBuiltin:
		return
	default:
		return
	}
	for _, name := range pkg.Scope().Names() {
		object := pkg.Scope().Lookup(name)
		if object.Exported() && !allowed[name] {
			t.Errorf("unexpected authority export %s.%s", pkg.Path(), name)
		}
	}
	if pkg.Path() == captureCodecPath {
		for _, name := range pkg.Scope().Names() {
			if _, ok := pkg.Scope().Lookup(name).(*types.Var); ok {
				t.Errorf("mutable codec bridge global %s", name)
			}
		}
		for _, name := range []string{"Bubble", "ComposerMetadata", "Reservation"} {
			object, ok := pkg.Scope().Lookup(name).(*types.TypeName)
			if !ok || object.IsAlias() {
				t.Errorf("codec bridge %s must be a neutral defined type", name)
			}
		}
		expectedFields := map[string]map[string]string{"Bubble": {"ID": "string", "Value": "encoding/json.RawMessage"}, "ComposerMetadata": {"Chat": "github.com/wangjohn/agent-archive/internal/agentapi.DatabaseChat", "Counted": "bool", "Subagents": "[]string"}}
		for name, fields := range expectedFields {
			object, ok := pkg.Scope().Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			structure, ok := object.Type().Underlying().(*types.Struct)
			if !ok || structure.NumFields() != len(fields) {
				t.Errorf("codec bridge %s must retain its neutral field shape", name)
				continue
			}
			for i := range structure.NumFields() {
				field := structure.Field(i)
				if types.TypeString(field.Type(), func(p *types.Package) string { return p.Path() }) != fields[field.Name()] {
					t.Errorf("codec bridge field %s.%s is not neutral", name, field.Name())
				}
			}
		}
		for _, name := range []string{"ValidateSettledMetadata", "DecodeComposerData", "FilterComposerRecordsBounded", "NewSourceBundle"} {
			if _, ok := pkg.Scope().Lookup(name).(*types.Func); !ok {
				t.Errorf("codec bridge %s must be a fixed function", name)
			}
		}
		if _, ok := pkg.Scope().Lookup("MaxComposerVersion").(*types.Const); !ok {
			t.Error("codec bridge version must be immutable")
		}
	}
	if pkg.Path() == captureProducerPath {
		captureCheckLiveProducerSignature(t, pkg)
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
		for _, typeName := range []string{"Proof", "LiveProof", "Reservation"} {
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
			methodCount := named.NumMethods()
			for i := range methodCount {
				name := named.Method(i).Name()
				permitted := (captureSymbolName(typeName) == captureSymbolProof && (captureSymbolName(name) == captureSymbolClose || captureSymbolName(name) == captureSymbolLoan) || captureSymbolName(typeName) == captureSymbolLiveProof && (captureSymbolName(name) == captureSymbolClose || captureSymbolName(name) == captureSymbolLiveLoan)) || captureSymbolName(typeName) == captureSymbolReservation && (captureSymbolName(name) == captureSymbolReserve || captureSymbolName(name) == captureSymbolRelease || captureSymbolName(name) == captureSymbolAvailable || captureSymbolName(name) == captureSymbolRefuse)
				if named.Method(i).Exported() && !permitted {
					t.Errorf("unexpected sealed method %s.%s", typeName, name)
				}
			}
		}
		for _, name := range []string{"New", "NewLive", "NewReservation", "Digest"} {
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
		pkg      string
		file     string
		imported string
		allowed  bool
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
				if ok && captureExactCalleeIdent(call.Fun, id) {
					accepted = true
				}
			} else if call, ok := node.(*ast.CallExpr); ok && captureExactCalleeIdent(call.Fun, id) {
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
		name    string
		body    string
		allowed bool
	}{
		{"final-mint", `func CaptureSettledCursor(){return issuer.New(),nil}`, true},
		{"renamed-final-mint", `func CaptureSettledCursor(){return renamed.New(),nil}`, true},
		{"function-value", `func CaptureSettledCursor(){mint:=issuer.New;_ = mint}`, false},
		{"extra-wrapper", `func AnotherCapture(){return issuer.New(),nil}`, false},
		{"nested-mint", `func CaptureSettledCursor(){f:=func(){return issuer.New(),nil};_=f}`, false},
		{"early-conditional-mint", `func CaptureSettledCursor(){if bad{return issuer.New(),nil};return nil,nil}`, false},
		{"nested-factory-final-return", `func CaptureSettledCursor(){return func()func(){return issuer.New}()(),nil}`, false},
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
	if err := captureOriginalAuthorityError(root, map[string]bool{}, map[string]bool{}); err == nil || !strings.Contains(err.Error(), "unqualified closed-domain profile source") {
		t.Fatal("ignored-only issuer directory escaped", err)
	}
}

func TestNativeCaptureIgnoredClosedDomainDeclarationControls(t *testing.T) {
	for _, test := range []struct {
		name   string
		dir    string
		file   string
		source string
	}{
		{"ignored-exact-producer", "internal/agents/nativecapture/producer", "cursor.go", "//go:build unavailable_profile\n\npackage producer\nimport issuer \"" + captureSealedPath + "\"\nfunc CaptureSettledCursor(){return issuer.New(),nil}\n"},
		{"ignored-hidden-sealed-export", "internal/agents/nativecapture/internal/sealed", "hidden.go", "//go:build unavailable_profile\n\npackage sealed\nvar ExportedIssuer any\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, filepath.FromSlash(test.dir))
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, test.file), []byte(test.source), 0600); err != nil {
				t.Fatal(err)
			}
			if err := captureOriginalAuthorityError(root, map[string]bool{dir: true}, map[string]bool{}); err == nil || !strings.Contains(err.Error(), "unqualified closed-domain profile source") {
				t.Fatal("ignored issuer declaration escaped", err)
			}
		})
	}
}

// Exact callee identity excludes identifiers inside function-literal bodies or
// receiver expressions from becoming authority-bearing direct calls.
func captureExactCalleeIdent(expression ast.Expr, id *ast.Ident) bool {
	for {
		parenthesized, ok := expression.(*ast.ParenExpr)
		if !ok {
			break
		}
		expression = parenthesized.X
	}
	switch callee := expression.(type) {
	case *ast.Ident:
		return callee == id
	case *ast.SelectorExpr:
		return callee.Sel == id
	default:
		return false
	}
}

func TestNativeCaptureTypedHelperDirectCalleeControls(t *testing.T) {
	sealed := types.NewPackage(captureSealedPath, "sealed")
	reservationObject := types.NewTypeName(token.NoPos, sealed, "Reservation", nil)
	reservation := types.NewNamed(reservationObject, types.NewStruct(nil, nil), nil)
	sealed.Scope().Insert(reservationObject)
	results := types.NewTuple(types.NewVar(token.NoPos, sealed, "", types.Typ[types.Int]))
	for _, name := range []string{"Digest", "NewReservation"} {
		sealed.Scope().Insert(types.NewFunc(token.NoPos, sealed, name, types.NewSignatureType(nil, nil, nil, types.NewTuple(), results, false)))
	}
	reservation.AddMethod(types.NewFunc(token.NoPos, sealed, "Reserve", types.NewSignatureType(types.NewVar(token.NoPos, sealed, "owner", types.NewPointer(reservation)), nil, nil, types.NewTuple(), results, false)))
	sealed.MarkComplete()
	for _, test := range []struct {
		name    string
		body    string
		symbol  string
		allowed bool
	}{
		{"direct-digest", `_ = issuer.Digest()`, "Digest", true},
		{"parenthesized-digest", `_ = (issuer.Digest)()`, "Digest", true},
		{"direct-reservation", `_ = issuer.NewReservation()`, "NewReservation", true},
		{"direct-reserve", `_ = owner.Reserve()`, "Reserve", true},
		{"digest-value", `_ = issuer.Digest`, "Digest", false},
		{"digest-nested-factory", `_ = func()func()int{return issuer.Digest}()`, "Digest", false},
		{"reservation-nested-factory", `_ = func()func()int{return issuer.NewReservation}()`, "NewReservation", false},
		{"reserve-nested-factory", `_ = func()func()int{return owner.Reserve}()`, "Reserve", false},
		{"reserve-method-expression", `_ = (*issuer.Reservation).Reserve`, "Reserve", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "cursor.go", `package producer;import issuer "`+captureSealedPath+`";func use(owner *issuer.Reservation){`+test.body+`}`, 0)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
			cfg := types.Config{Importer: captureProbeImporter{domain: sealed}}
			if _, err := cfg.Check(captureProducerPath, fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			calls := map[ast.Expr]bool{}
			ast.Inspect(file, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					calls[call.Fun] = true
				}
				return true
			})
			found := false
			for id, object := range info.Uses {
				if object.Pkg() != nil && object.Pkg().Path() == captureSealedPath && object.Name() == test.symbol {
					found = true
					if captureAuthorityObjectAllowed(captureProducerPath, "cursor.go", object, calls, id) != test.allowed {
						t.Fatal("typed direct callee policy changed")
					}
				}
			}
			if !found {
				t.Fatal("probe has no resolved helper object")
			}
		})
	}
}

func captureCodecExports() map[string]bool {
	return map[string]bool{"Bubble": true, "ComposerMetadata": true, "Reservation": true, "MaxComposerVersion": true, "ValidateSettledMetadata": true, "DecodeComposerData": true, "FilterComposerRecordsBounded": true, "NewSourceBundle": true}
}

func captureCodecDependencyAllowed(path string) bool {
	dependencies := map[string]bool{"context": true, "encoding/json": true, "time": true, "github.com/wangjohn/agent-archive/internal/agentapi": true, "github.com/wangjohn/agent-archive/internal/agents/nativecodec": true, "github.com/wangjohn/agent-archive/internal/archive": true}
	return dependencies[path]
}

func captureCodecObjectAllowed(pkg, file string, object types.Object, calls map[ast.Expr]bool, id *ast.Ident) bool {
	if pkg == captureCodecPath && captureAuthorityFile(file) == captureAuthorityCodec {
		return true
	}
	if pkg != captureProducerPath || (captureAuthorityFile(file) != captureAuthorityCursor && captureAuthorityFile(file) != captureAuthorityLiveCursor) {
		return false
	}
	if field, ok := object.(*types.Var); ok && field.IsField() {
		return map[string]bool{"ID": true, "Value": true, "Chat": true, "Counted": true, "Subagents": true}[field.Name()]
	}
	if !captureCodecExports()[object.Name()] {
		return false
	}
	switch object.(type) {
	case *types.TypeName, *types.Const:
		return true
	case *types.Func:
		for expression := range calls {
			if captureExactCalleeIdent(expression, id) {
				return true
			}
		}
	}
	return false
}

func TestNativeCaptureCodecBridgeTypedImporterControls(t *testing.T) {
	const codecPath = "github.com/wangjohn/agent-archive/internal/agents/capturecodec"
	bridge := types.NewPackage(codecPath, "capturecodec")
	bridge.Scope().Insert(types.NewFunc(token.NoPos, bridge, "ValidateSettledMetadata", types.NewSignatureType(nil, nil, nil, types.NewTuple(), types.NewTuple(), false)))
	bridge.MarkComplete()
	for _, test := range []struct {
		name    string
		pkg     string
		file    string
		body    string
		allowed bool
	}{
		{"actual-producer-direct", captureProducerPath, "cursor.go", `bridge.ValidateSettledMetadata()`, true},
		{"unlisted-file", captureProducerPath, "extra.go", `bridge.ValidateSettledMetadata()`, false},
		{"neutral-parent", captureDomainPath, "proof.go", `bridge.ValidateSettledMetadata()`, false},
		{"ordinary-consumer", capturePackageStateString(), "import_stage.go", `bridge.ValidateSettledMetadata()`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, test.file, `package probe;import bridge "`+codecPath+`";func use(){`+test.body+`}`, 0)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
			cfg := types.Config{Importer: captureProbeImporter{domain: bridge}}
			if _, err := cfg.Check(test.pkg, fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, object := range info.Uses {
				if object.Pkg() != nil && object.Pkg().Path() == codecPath && object.Exported() {
					found = true
					if captureAuthorityImportAllowed(test.pkg, test.file, object.Pkg().Path()) != test.allowed {
						t.Fatal("typed codec importer policy changed")
					}
				}
			}
			if !found {
				t.Fatal("missing resolved codec object")
			}
		})
	}
}

func TestNativeCaptureCodecBridgeTypedValueControls(t *testing.T) {
	bridge := types.NewPackage(captureCodecPath, "capturecodec")
	bridge.Scope().Insert(types.NewFunc(token.NoPos, bridge, "ValidateSettledMetadata", types.NewSignatureType(nil, nil, nil, types.NewTuple(), types.NewTuple(), false)))
	bridge.MarkComplete()
	for _, test := range []struct {
		name    string
		body    string
		allowed bool
	}{
		{"direct-fixed-transform", `bridge.ValidateSettledMetadata()`, true},
		{"transform-function-value", `_ = bridge.ValidateSettledMetadata`, false},
		{"transform-nested-factory", `_ = func()func(){return bridge.ValidateSettledMetadata}()`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "cursor.go", `package producer;import bridge "`+captureCodecPath+`";func use(){`+test.body+`}`, 0)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
			cfg := types.Config{Importer: captureProbeImporter{domain: bridge}}
			if _, err := cfg.Check(captureProducerPath, fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			calls := map[ast.Expr]bool{}
			ast.Inspect(file, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					calls[call.Fun] = true
				}
				return true
			})
			found := false
			for id, object := range info.Uses {
				if object.Pkg() != nil && object.Pkg().Path() == captureCodecPath && object.Exported() {
					found = true
					if captureCodecObjectAllowed(captureProducerPath, "cursor.go", object, calls, id) != test.allowed {
						t.Fatal("codec transform callee policy changed")
					}
				}
			}
			if !found {
				t.Fatal("missing resolved codec function")
			}
		})
	}
}

func TestNativeCaptureCodecBridgeOriginalProfileControls(t *testing.T) {
	for _, test := range []struct {
		name     string
		dir      string
		file     string
		source   string
		selected bool
		allowed  bool
	}{
		{"fixed-producer-import", "internal/agents/nativecapture/producer", "cursor.go", "package producer\nimport bridge \"" + captureCodecPath + "\"\nfunc use(){bridge.ValidateSettledMetadata()}\n", true, true},
		{"ordinary-bridge-import", "internal/state", "import_stage.go", "package state\nimport bridge \"" + captureCodecPath + "\"\nfunc use(){bridge.ValidateSettledMetadata()}\n", true, false},
		{"ignored-bridge-export", "internal/agents/capturecodec", "hidden.go", "//go:build unavailable_profile\n\npackage capturecodec\nvar Reader any\n", false, false},
		{"effectful-bridge-dependency", "internal/agents/capturecodec", "codec.go", "package capturecodec\nimport \"os\"\nfunc use(){os.Getwd()}\n", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, filepath.FromSlash(test.dir))
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, test.file)
			if err := os.WriteFile(path, []byte(test.source), 0600); err != nil {
				t.Fatal(err)
			}
			selected := map[string]bool{}
			if test.selected {
				selected[path] = true
			}
			err := captureOriginalAuthorityError(root, map[string]bool{dir: true}, selected)
			if (err == nil) != test.allowed {
				t.Fatal("codec original profile boundary changed", err)
			}
		})
	}
}

func captureProducerReferenceAllowed(pkg, file string, obj types.Object, f *ast.File, id *ast.Ident) bool {
	if pkg != captureCursorAdapterPath {
		return false
	}
	if obj.Name() == "CaptureSettledCursor" {
		return file == "settled_import.go" && captureDirectCallObject(f, id, "CaptureSettledRequest", false)
	}
	return obj.Name() == "CaptureLiveCursor" && file == "live_import.go" && captureDirectCallObject(f, id, "CaptureLiveRequest", false)
}

func captureMintReferenceAllowed(pkg, file string, obj types.Object, f *ast.File, id *ast.Ident) bool {
	if pkg != captureProducerPath {
		return false
	}
	if obj.Name() == "New" {
		return file == "cursor.go" && captureDirectCallObject(f, id, "CaptureSettledCursor", true)
	}
	return obj.Name() == "NewLive" && file == "live_cursor.go" && captureDirectCallObject(f, id, "CaptureLiveCursor", true)
}

func captureLiveIssuerReferenceAllowed(pkg, file string, obj types.Object, f *ast.File, id *ast.Ident, info *types.Info) bool {
	if pkg != capturePackageStateString() || file != "cursor_scratch.go" || obj.Pkg() == nil || obj.Pkg().Path() != captureFactsPath || obj.Name() != "BindLiveCaptureRequest" {
		return false
	}
	function, ok := obj.(*types.Func)
	if !ok {
		return false
	}
	signature, ok := function.Type().(*types.Signature)
	if !ok || signature.Recv() != nil {
		return false
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "NewLiveCaptureRequest" || fn.Recv == nil || fn.Body == nil {
			continue
		}
		if len(fn.Recv.List) != 1 {
			return false
		}
		receiver, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			return false
		}
		receiverName, ok := receiver.X.(*ast.Ident)
		if !ok {
			return false
		}
		receiverObject, ok := info.Uses[receiverName].(*types.TypeName)
		if !ok || receiverObject.Pkg() == nil || receiverObject.Pkg().Path() != capturePackageStateString() || receiverObject.Name() != "Store" {
			return false
		}
		allowed := false
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if _, nested := node.(*ast.FuncLit); nested {
				return false
			}
			call, ok := node.(*ast.CallExpr)
			if !ok || !captureExactCalleeIdent(call.Fun, id) || len(call.Args) != 6 {
				return true
			}
			// The closed issuer must bind actual concrete state-owner method values,
			// never forwarding arbitrary callbacks supplied by another integration.
			var ownerObject types.Object
			for i, name := range []string{"Reserve", "OpenWorkspace", "Close", "Err"} {
				sel, ok := call.Args[i+2].(*ast.SelectorExpr)
				if !ok {
					return true
				}
				receiver, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				object := info.Uses[receiver]
				if object == nil {
					return true
				}
				if ownerObject == nil {
					ownerObject = object
				} else if ownerObject != object {
					return true
				}
				selection := info.Selections[sel]
				if selection == nil || selection.Obj().Name() != name || types.TypeString(selection.Recv(), func(p *types.Package) string { return p.Path() }) != "*"+capturePackageStateString()+".CursorScratch" {
					return true
				}
			}
			allowed = true
			return true
		})
		return allowed
	}
	return false
}

func captureCheckLiveProducerSignature(t *testing.T, pkg *types.Package) {
	t.Helper()
	fn, ok := pkg.Scope().Lookup("CaptureLiveCursor").(*types.Func)
	if !ok {
		t.Error("fixed live producer must be a function")
		return
	}
	signature, ok := fn.Type().(*types.Signature)
	if !ok || signature.Recv() != nil || signature.Variadic() || signature.Params().Len() != 1 || signature.Results().Len() != 2 {
		t.Error("live producer signature changed")
		return
	}
	fullName := func(p *types.Package) string { return p.Path() }
	if types.TypeString(signature.Params().At(0).Type(), fullName) != "*"+captureFactsPath+".LiveCaptureRequest" || types.TypeString(signature.Results().At(0).Type(), fullName) != "*"+captureSealedPath+".LiveProof" || !types.Identical(signature.Results().At(1).Type(), types.Universe.Lookup("error").Type()) {
		t.Error("live producer accepts arbitrary structural scratch authority")
	}
}
func captureCheckLiveFactsShape(t *testing.T, pkg *types.Package, fset *token.FileSet) {
	t.Helper()
	for _, name := range pkg.Scope().Names() {
		obj := pkg.Scope().Lookup(name)
		if obj.Exported() && !captureLiveFactsExportAllowed(obj, fset) {
			t.Errorf("unexpected nativefacts authority export %s", name)
		}
	}
	captureCheckLiveIssuerSignature(t, pkg)
	for name, methods := range map[string]map[string]bool{"CapturePolicy": {}, "CaptureSkillMode": {}, "SettledImportFacts": {"UnmarshalJSON": true}, "CaptureRequest": {"Borrow": true, "Close": true, "MatchesBudget": true, "MatchesPolicy": true}, "RequestLease": {"Context": true, "Budget": true, "Reservation": true, "Registration": true, "Reviewed": true, "Locator": true, "At": true, "Policy": true, "Check": true, "Close": true}, "CapturedScope": {"Reserve": true, "Release": true, "Available": true, "Refuse": true}, "LiveCaptureRequest": {"Borrow": true, "Close": true}, "LiveCaptureLease": {"Original": true, "Context": true, "Err": true, "Reserve": true, "OpenWorkspace": true, "Close": true}} {
		obj, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok || obj.IsAlias() {
			t.Errorf("live binding %s must be concrete", name)
			continue
		}
		named, ok := obj.Type().(*types.Named)
		if !ok {
			t.Error("live binding not named")
			continue
		}
		if name == "CaptureRequest" || name == "RequestLease" || name == "CapturedScope" || name == "LiveCaptureRequest" || name == "LiveCaptureLease" {
			structure, ok := named.Underlying().(*types.Struct)
			if !ok || structure.NumFields() != 1 || structure.Field(0).Exported() {
				t.Errorf("live binding %s exposes owner mutation", name)
			}
		}
		for i := range named.NumMethods() {
			method := named.Method(i)
			if method.Exported() && !methods[method.Name()] {
				t.Errorf("unexpected live authority method %s.%s", name, method.Name())
			}
		}
	}
}

func TestLiveCaptureIssuerTypedReferenceControls(t *testing.T) {
	facts := types.NewPackage(captureFactsPath, "nativefacts")
	anyType := types.Universe.Lookup("any").Type()
	params := types.NewTuple(types.NewVar(token.NoPos, facts, "original", anyType), types.NewVar(token.NoPos, facts, "captured", anyType), types.NewVar(token.NoPos, facts, "reserve", anyType), types.NewVar(token.NoPos, facts, "open", anyType), types.NewVar(token.NoPos, facts, "close", anyType), types.NewVar(token.NoPos, facts, "check", anyType))
	facts.Scope().Insert(types.NewFunc(token.NoPos, facts, "BindLiveCaptureRequest", types.NewSignatureType(nil, nil, nil, params, types.NewTuple(), false)))
	facts.MarkComplete()
	prefix := `package state; import facts "` + captureFactsPath + `";type Store struct{};type CursorScratch struct{};func(*CursorScratch)Reserve(){};func(*CursorScratch)OpenWorkspace(){};func(*CursorScratch)Close(){};func(*CursorScratch)Err(){};`
	for _, test := range []struct {
		name, body, file string
		allowed          bool
	}{
		{"exact-owner", `func(*Store)NewLiveCaptureRequest(){scratch:=new(CursorScratch);facts.BindLiveCaptureRequest(nil,nil,scratch.Reserve,scratch.OpenWorkspace,scratch.Close,scratch.Err)}`, "cursor_scratch.go", true},
		{"different-owner", `func(*Store)NewLiveCaptureRequest(){scratch:=new(CursorScratch);other:=new(CursorScratch);facts.BindLiveCaptureRequest(nil,nil,scratch.Reserve,other.OpenWorkspace,scratch.Close,scratch.Err)}`, "cursor_scratch.go", false},
		{"function-value", `func(*Store)NewLiveCaptureRequest(){ctor:=facts.BindLiveCaptureRequest;_ = ctor}`, "cursor_scratch.go", false},
		{"reexport", `var Constructor=facts.BindLiveCaptureRequest`, "cursor_scratch.go", false},
		{"nested-callback", `func(*Store)NewLiveCaptureRequest(){f:=func(){scratch:=new(CursorScratch);facts.BindLiveCaptureRequest(nil,nil,scratch.Reserve,scratch.OpenWorkspace,scratch.Close,scratch.Err)};_ = f}`, "cursor_scratch.go", false},
		{"wrong-receiver", `type Other struct{};func(*Other)NewLiveCaptureRequest(){scratch:=new(CursorScratch);facts.BindLiveCaptureRequest(nil,nil,scratch.Reserve,scratch.OpenWorkspace,scratch.Close,scratch.Err)}`, "cursor_scratch.go", false},
		{"test-issuer", `func(*Store)NewLiveCaptureRequest(){scratch:=new(CursorScratch);facts.BindLiveCaptureRequest(nil,nil,scratch.Reserve,scratch.OpenWorkspace,scratch.Close,scratch.Err)}`, "cursor_scratch_test.go", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, test.file, prefix+test.body, 0)
			if err != nil {
				t.Fatal(err)
			}
			info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
			cfg := types.Config{Importer: captureProbeImporter{domain: facts}}
			if _, err = cfg.Check(capturePackageStateString(), fset, []*ast.File{file}, info); err != nil {
				t.Fatal(err)
			}
			seen := false
			for id, obj := range info.Uses {
				if obj.Pkg() != nil && obj.Pkg().Path() == captureFactsPath && obj.Name() == "BindLiveCaptureRequest" {
					seen = true
					if got := captureLiveIssuerReferenceAllowed(capturePackageStateString(), test.file, obj, file, id, info); got != test.allowed {
						t.Fatalf("issuer allowed=%v want=%v", got, test.allowed)
					}
				}
			}
			if !seen {
				t.Fatal("probe did not resolve actual issuer object")
			}
		})
	}
}

func captureLiveFactsExportAllowed(obj types.Object, fset *token.FileSet) bool {
	allowed := map[string]bool{"CapturePolicy": true, "CaptureRequest": true, "FreezeCaptureRequest": true, "RequestLease": true, "CapturedScope": true, "CaptureSkillMode": true, "CaptureSkillNone": true, "CaptureSkillMetadata": true, "CaptureSkillBody": true, "SettledImportFacts": true, "LiveCaptureRequest": true, "LiveCaptureLease": true, "BindLiveCaptureRequest": true}
	if allowed[obj.Name()] {
		return true
	}
	// Test entrypoints do not add a production factory or mutable export. Their
	// actual signature and source file must be Go's ordinary private test body.
	if fset == nil || !strings.HasSuffix(fset.Position(obj.Pos()).Filename, "_test.go") || !strings.HasPrefix(obj.Name(), "Test") {
		return false
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return false
	}
	signature, ok := fn.Type().(*types.Signature)
	return ok && signature.Recv() == nil && !signature.Variadic() && signature.Params().Len() == 1 && signature.Results().Len() == 0 && types.TypeString(signature.Params().At(0).Type(), func(p *types.Package) string { return p.Path() }) == "*testing.T"
}

func captureCheckLiveIssuerSignature(t *testing.T, pkg *types.Package) {
	t.Helper()
	fn, ok := pkg.Scope().Lookup("BindLiveCaptureRequest").(*types.Func)
	if !ok {
		t.Error("live issuer must be a fixed function")
		return
	}
	signature, ok := fn.Type().(*types.Signature)
	if !ok || signature.Recv() != nil || signature.Variadic() || signature.Params().Len() != 6 || signature.Results().Len() != 2 {
		t.Error("live issuer signature changed")
		return
	}
	fullName := func(p *types.Package) string { return p.Path() }
	expected := []string{"*" + captureFactsPath + ".CaptureRequest", "*" + captureFactsPath + ".RequestLease", "func(int64) error", "func() (*os.Root, error)", "func() error", "func() error"}
	for i, want := range expected {
		if got := types.TypeString(signature.Params().At(i).Type(), fullName); got != want {
			t.Errorf("live issuer argument%d=%s want%s", i, got, want)
		}
	}
	if types.TypeString(signature.Results().At(0).Type(), fullName) != "*"+captureFactsPath+".LiveCaptureRequest" || !types.Identical(signature.Results().At(1).Type(), types.Universe.Lookup("error").Type()) {
		t.Error("live issuer returns arbitrary authority")
	}
}

func TestLiveCaptureFactsRejectsUnlistedFactoryExports(t *testing.T) {
	facts := types.NewPackage(captureFactsPath, "nativefacts")
	for _, name := range []string{"ForgeLive", "LiveFactory", "TestForgeLive", "LiveOwner"} {
		fn := types.NewFunc(token.NoPos, facts, name, types.NewSignatureType(nil, nil, nil, types.NewTuple(), types.NewTuple(), false))
		if captureLiveFactsExportAllowed(fn, nil) {
			t.Fatalf("unlisted production factory accepted %s", name)
		}
	}
}
