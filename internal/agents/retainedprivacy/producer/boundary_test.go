package producer

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type retainedSourcePath string

const (
	retainedPublicPath   retainedSourcePath = "internal/agents/retainedprivacy/proof.go"
	retainedRequestPath  retainedSourcePath = "internal/agents/retainedprivacy/internal/sealed/request.go"
	retainedProofPath    retainedSourcePath = "internal/agents/retainedprivacy/internal/sealed/proof.go"
	retainedProducerPath retainedSourcePath = "internal/agents/retainedprivacy/producer/transform.go"
)

type retainedImportPath string

const (
	retainedUnsafe  retainedImportPath = "unsafe"
	retainedReflect retainedImportPath = "reflect"
)

type retainedPrivateType string

const (
	retainedProofType     retainedPrivateType = "Proof"
	retainedTransformType retainedPrivateType = "transform"
)

type retainedOperation string

const (
	retainedLoanOperation  retainedOperation = "*Proof.WithVerifiedTransform"
	retainedCloseOperation retainedOperation = "*Proof.Close"
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
	domainSource := strings.HasPrefix(path, "internal/agents/retainedprivacy/") && (!strings.HasSuffix(path, "_test.go") || strings.HasPrefix(path, "internal/agents/retainedprivacy/internal/sealed/"))
	if domainSource {
		allowedFiles := map[string]bool{"internal/agents/retainedprivacy/proof.go": true, "internal/agents/retainedprivacy/producer/transform.go": true, "internal/agents/retainedprivacy/internal/sealed/proof.go": true, "internal/agents/retainedprivacy/internal/sealed/request.go": true}
		if !allowedFiles[path] {
			fail("unreviewed source in the sealed operation domain")
		}
		if want, ok := retainedDomainImports[path]; ok {
			if len(imports) != len(want) {
				fail("sealed-domain import surface changed")
			}
			for name, imported := range imports {
				if want[name] != imported {
					fail("sealed-domain type or operation import identity changed: " + name)
				}
			}
		}
		for _, imported := range imports {
			if retainedImportPath(imported) == retainedUnsafe || retainedImportPath(imported) == retainedReflect {
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
		problems = append(problems, retainedDeclarationProblems(path, file, imports)...)
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

func retainedDeclarationProblems(path string, file *ast.File, imports map[string]string) []string {
	var problems []string
	fail := func(message string) { problems = append(problems, message) }
	allowedTypes := map[string]bool{}
	allowed := map[string]string{}
	switch retainedSourcePath(path) {
	case retainedPublicPath:
		for _, name := range []string{"Proof", "Request", "Policy", "Facts", "Expectation"} {
			allowedTypes[name] = true
		}
		allowed["FreezeRequestCaptured"] = "func(context.Context, Expectation, []byte, *agentapi.NativeReadBudget, *agentapi.NativeReadReservation) (*Request, error)"
	case retainedRequestPath:
		for _, name := range []string{"Policy", "Expectation", "Request", "requestState", "Lease", "leaseCustody"} {
			allowedTypes[name] = true
		}
		allowed["FreezeRequestCaptured"] = "func(context.Context, Expectation, []byte, *agentapi.NativeReadBudget, *agentapi.NativeReadReservation) (*Request, error)"
		allowed["*Request.Borrow"] = "func() (*Lease, error)"
		allowed["*Request.Close"] = "func() error"
		allowed["*Request.matches"] = "func(*Request, *agentapi.NativeReadBudget) error"
		allowed["*Lease.Check"] = "func() error"
		allowed["*Lease.Context"] = "func() context.Context"
		allowed["*Lease.Budget"] = "func() *agentapi.NativeReadBudget"
		allowed["*Lease.Close"] = "func()"
	case retainedProofPath:
		for _, name := range []string{"Facts", "Proof", "transform", "loanContext"} {
			allowedTypes[name] = true
		}
		allowed["New"] = "func(*Lease, archive.SourceBundle, archive.CompressedSource, Facts, string, func()) *Proof"
		allowed["Digest"] = "func(context.Context, *agentapi.NativeReadBudget, any) (string, error)"
		allowed["*Proof.WithVerifiedTransform"] = "func(context.Context, *Request, *agentapi.NativeReadBudget, func(context.Context, archive.SourceBundle, archive.CompressedSource, Facts) error) error"
		allowed["*Proof.Close"] = "func() error"
		allowed["joinLoanContext"] = "func(context.Context, context.Context) (context.Context, func())"
		allowed["loanContext.Err"] = "func() error"
		allowed["loanContext.Deadline"] = "func() (time.Time, bool)"
	case retainedProducerPath:
		allowedTypes["skillPolicy"] = true
		allowed["Transform"] = "func(*sealed.Request) (*sealed.Proof, error)"
		allowed["fixedFilter"] = "func(string) (agentapi.TranscriptFilter, error)"
		allowed["supportedSkill"] = "func(string) bool"
		allowed["limitSkills"] = "func([]archive.SupplementalEvidence, string) []archive.SupplementalEvidence"
	}
	if strings.Contains(path, "/internal/sealed/") {
		allowedNames := map[*ast.Ident]bool{}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.SelectorExpr:
				allowedNames[n.Sel] = true
			case *ast.FuncDecl:
				if path == "internal/agents/retainedprivacy/internal/sealed/proof.go" && n.Name.Name == "New" && n.Recv == nil {
					allowedNames[n.Name] = true
				}
			}
			return true
		})
		ast.Inspect(file, func(node ast.Node) bool {
			if name, ok := node.(*ast.Ident); ok && name.Name == "New" && !allowedNames[name] {
				fail("bare sealed constructor reference escaped its declaration")
			}
			return true
		})
	}
	for _, decl := range file.Decls {
		switch declaration := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range declaration.Specs {
				switch item := spec.(type) {
				case *ast.TypeSpec:
					if !allowedTypes[item.Name.Name] {
						fail("new sealed-domain type or alias")
					}
					if item.Name.Name == "skillPolicy" && retainedTypeText(item.Type) != "string" {
						fail("skill policy must remain a closed string enum")
					}
					if path != "internal/agents/retainedprivacy/proof.go" && item.Assign.IsValid() {
						fail("private construction types cannot be aliased")
					}
					if path == "internal/agents/retainedprivacy/proof.go" {
						selector, ok := item.Type.(*ast.SelectorExpr)
						var qualifier *ast.Ident
						if ok {
							qualifier, _ = selector.X.(*ast.Ident)
						}
						if !item.Assign.IsValid() || !ok || qualifier == nil || selector.Sel.Name != item.Name.Name || imports[qualifier.Name] != retainedDomain+"/internal/sealed" {
							fail("public aliases must bind the exact sealed type and import identity")
						}
					} else if want, ok := retainedStructShapes[item.Name.Name]; ok {
						shape, err := parser.ParseExpr(want)
						if err != nil || retainedStructShape(item.Type) != retainedStructShape(shape) {
							fail("sealed custody field shape changed: " + item.Name.Name)
						}
					}

				case *ast.ValueSpec:
					if path == string(retainedProducerPath) && declaration.Tok == token.CONST && len(item.Names) == 1 && len(item.Values) == 1 && retainedTypeText(item.Type) == "skillPolicy" {
						choices := map[string]string{"skillNone": `"none"`, "skillMetadata": `"metadata"`, "skillBody": `"body"`}
						if want, ok := choices[item.Names[0].Name]; ok && retainedTypeText(item.Values[0]) == want {
							continue
						}
					}
					fail("sealed domain cannot expose or retain a mutable package-level issuer")
				}
			}
		case *ast.FuncDecl:
			key := declaration.Name.Name
			if declaration.Recv != nil && len(declaration.Recv.List) == 1 {
				key = retainedTypeText(declaration.Recv.List[0].Type) + "." + key
			}
			signature := retainedFunctionType(declaration.Type)
			if want, ok := allowed[key]; !ok || signature != want {
				fail("unreviewed receiver, method or operation signature: " + key + " " + signature)
			}
			ast.Inspect(declaration, func(node ast.Node) bool {
				switch expression := node.(type) {
				case *ast.CompositeLit:
					typ, ok := expression.Type.(*ast.Ident)
					if ok && (retainedPrivateType(typ.Name) == retainedProofType || retainedPrivateType(typ.Name) == retainedTransformType) && (path != "internal/agents/retainedprivacy/internal/sealed/proof.go" || key != "New") {
						fail("private proof construction outside its sole constructor")
					}
				case *ast.CallExpr:
					name, ok := expression.Fun.(*ast.Ident)
					if ok && name.Name == "new" && len(expression.Args) == 1 {
						typ, ok := expression.Args[0].(*ast.Ident)
						if ok && (retainedPrivateType(typ.Name) == retainedProofType || retainedPrivateType(typ.Name) == retainedTransformType) {
							fail("private proof allocation must use its sole constructor")
						}
					}
				case *ast.SelectorExpr:
					if expression.Sel.Name == "transform" && (path != "internal/agents/retainedprivacy/internal/sealed/proof.go" || (retainedOperation(key) != retainedLoanOperation && retainedOperation(key) != retainedCloseOperation)) {
						fail("private proof custody accessed outside its checked loan/close")
					}
				}
				return true
			})
		}
	}
	return problems
}

// Signatures and field types below resolve only through these exact imports.
var retainedDomainImports = map[string]map[string]string{
	"internal/agents/retainedprivacy/proof.go":                   {"context": "context", "agentapi": "github.com/wangjohn/agent-archive/internal/agentapi", "sealed": "github.com/wangjohn/agent-archive/internal/agents/retainedprivacy/internal/sealed"},
	"internal/agents/retainedprivacy/internal/sealed/request.go": {"bytes": "bytes", "context": "context", "json": "encoding/json", "errors": "errors", "sync": "sync", "agentapi": "github.com/wangjohn/agent-archive/internal/agentapi", "agentmeta": "github.com/wangjohn/agent-archive/internal/agentmeta", "archive": "github.com/wangjohn/agent-archive/internal/archive"},
	"internal/agents/retainedprivacy/internal/sealed/proof.go":   {"bytes": "bytes", "context": "context", "sha256": "crypto/sha256", "hex": "encoding/hex", "json": "encoding/json", "errors": "errors", "sync": "sync", "time": "time", "agentapi": "github.com/wangjohn/agent-archive/internal/agentapi", "agentmeta": "github.com/wangjohn/agent-archive/internal/agentmeta", "archive": "github.com/wangjohn/agent-archive/internal/archive"},
	"internal/agents/retainedprivacy/producer/transform.go":      {"errors": "errors", "sync": "sync", "agentapi": "github.com/wangjohn/agent-archive/internal/agentapi", "agentmeta": "github.com/wangjohn/agent-archive/internal/agentmeta", "claude": "github.com/wangjohn/agent-archive/internal/agents/claude", "codex": "github.com/wangjohn/agent-archive/internal/agents/codex", "cursor": "github.com/wangjohn/agent-archive/internal/agents/cursor", "muse": "github.com/wangjohn/agent-archive/internal/agents/muse", "sealed": "github.com/wangjohn/agent-archive/internal/agents/retainedprivacy/internal/sealed", "archive": "github.com/wangjohn/agent-archive/internal/archive"},
}

// Every custody and data carrier is closed, including unexported fields and tags.
// A function-valued issuer cannot be smuggled through an otherwise unchanged method.
var retainedStructShapes = map[string]string{
	"leaseCustody": "struct {\n\tmu sync.Mutex\n\towner *agentapi.NativeReadReservation\n\tcharged int64\n\tclosed bool\n}",
	"Policy":       "struct {\n\tFilterVersion  string `json:\"filter_version\"`\n\tAdapterVersion string `json:\"adapter_version\"`\n\tSkillEvidence  string `json:\"skill_evidence\"`\n}",
	"Expectation":  "struct {\n\tCoveredToken string                      `json:\"covered_token\"`\n\tHadRequest   bool                        `json:\"had_request\"`\n\tRegistration archive.SessionRegistration `json:\"registration\"`\n\tStageSHA256  string                      `json:\"stage_sha256\"`\n\tMetadata     archive.Metadata            `json:\"metadata\"`\n\tOldPolicy    Policy                      `json:\"old_policy\"`\n\tTarget       Policy                      `json:\"target\"`\n}",
	"Request":      "struct{ state *requestState }",
	"requestState": "struct {\n\tmu               sync.Mutex\n\tctx              context.Context\n\tcancel           context.CancelCauseFunc\n\tbudget           *agentapi.NativeReadBudget\n\tcaptured         *agentapi.NativeReadBudget\n\towner            *agentapi.NativeReadReservation\n\traw              []byte\n source []byte\n\tcharged          int64\n\tclosed bool\n borrowed bool\n}",
	"Lease":        "struct {\n\trequest *Request\n\tWire    Expectation\n\tSource  []byte\n\tcustody *leaseCustody\n}",
	"Facts":        "struct {\n\tOwnerSHA256    string                  `json:\"owner_sha256\"`\n\tCoveredToken   string                  `json:\"covered_token\"`\n\tHadRequest     bool                    `json:\"had_request\"`\n\tStageSHA256    string                  `json:\"stage_sha256\"`\n\tInput          archive.SourceReference `json:\"input\"`\n\tOutput         archive.SourceReference `json:\"output\"`\n\tOriginalPolicy Policy                  `json:\"original_policy\"`\n\tTargetPolicy   Policy                  `json:\"target_policy\"`\n}",
	"Proof":        "struct{ transform *transform }",
	"transform":    "struct {\n\tmu        sync.Mutex\n\tlease     *Lease\n\tbundle    archive.SourceBundle\n\tsource    archive.CompressedSource\n\tfacts     Facts\n\tbundleSHA string\n\trelease   func()\n\tclosed    bool\n}",
	"loanContext":  "struct {\n\tcontext.Context\n\toriginal context.Context\n}",
}

func retainedStructShape(node ast.Expr) string {
	shape, ok := node.(*ast.StructType)
	if !ok {
		return "not a struct"
	}
	var fields []string
	for _, field := range shape.Fields.List {
		tag := ""
		if field.Tag != nil {
			tag = field.Tag.Value
		}
		typ := retainedTypeText(field.Type)
		if len(field.Names) == 0 {
			fields = append(fields, ":"+typ+":"+tag)
		}
		for _, name := range field.Names {
			fields = append(fields, name.Name+":"+typ+":"+tag)
		}
	}
	return strings.Join(fields, "|")
}

func retainedTypeText(node ast.Node) string {
	var out bytes.Buffer
	_ = format.Node(&out, token.NewFileSet(), node)
	return out.String()
}

func retainedFunctionType(original *ast.FuncType) string {
	copyType := *original
	withoutNames := func(original *ast.FieldList) *ast.FieldList {
		if original == nil {
			return nil
		}
		result := &ast.FieldList{}
		for _, field := range original.List {
			for range max(1, len(field.Names)) {
				result.List = append(result.List, &ast.Field{Type: field.Type})
			}
		}
		return result
	}
	copyType.Params = withoutNames(original.Params)
	copyType.Results = withoutNames(original.Results)
	return retainedTypeText(&copyType)
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
	requestSource, err := os.ReadFile("../internal/sealed/request.go")
	if err != nil {
		t.Fatal(err)
	}
	forged := append(append([]byte(nil), requestSource...), []byte("\nfunc(r *Request) Context(bundle archive.SourceBundle,source archive.CompressedSource,facts Facts)*Proof{return &Proof{transform:&transform{bundle:bundle,source:source,facts:facts}}}\n")...)
	if len(retainedBoundaryProblems("internal/agents/retainedprivacy/internal/sealed/request.go", forged)) == 0 {
		t.Fatal("same-name method on another receiver minted private proof")
	}
	issuer := bytes.Replace(requestSource, []byte("type Lease struct {"), []byte("type Lease struct {\nIssuer func(*Lease, archive.SourceBundle, archive.CompressedSource, Facts, string, func()) *Proof"), 1)
	issuer = bytes.Replace(issuer, []byte("&Lease{request: r,"), []byte("&Lease{Issuer: New, request: r,"), 1)
	if len(retainedBoundaryProblems("internal/agents/retainedprivacy/internal/sealed/request.go", issuer)) == 0 {
		t.Fatal("bare constructor escaped through a new lease field")
	}
	bare := bytes.Replace(requestSource, []byte("return l.request.state.ctx"), []byte("issuer := New; _ = issuer; return l.request.state.ctx"), 1)
	if len(retainedBoundaryProblems("internal/agents/retainedprivacy/internal/sealed/request.go", bare)) == 0 {
		t.Fatal("bare constructor escaped through an existing operation")
	}
	fieldsOnly := bytes.Replace(requestSource, []byte("type Lease struct {"), []byte("type Lease struct {\nIssuer any"), 1)
	if len(retainedBoundaryProblems("internal/agents/retainedprivacy/internal/sealed/request.go", fieldsOnly)) == 0 {
		t.Fatal("lease field surface changed without review")
	}
	public, err := os.ReadFile("../proof.go")
	if err != nil {
		t.Fatal(err)
	}
	foreign := bytes.Replace(public, []byte("type Proof = sealed.Proof"), []byte("type Proof = foreign.Proof"), 1)
	foreign = bytes.Replace(foreign, []byte("import ("), []byte("import (\nforeign \"example.invalid/foreign\""), 1)
	if len(retainedBoundaryProblems("internal/agents/retainedprivacy/proof.go", foreign)) == 0 {
		t.Fatal("foreign public alias replaced sealed proof")
	}
	rebound := bytes.Replace(public, []byte(retainedDomain+"/internal/sealed"), []byte("example.invalid/sealed"), 1)
	if len(retainedBoundaryProblems("internal/agents/retainedprivacy/proof.go", rebound)) == 0 {
		t.Fatal("same-named foreign import replaced sealed alias identity")
	}
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

	for _, fixture := range []struct {
		name   string
		path   string
		source string
	}{
		{"same-package test issuer", "internal/agents/retainedprivacy/internal/sealed/issuer_test.go", "package sealed\nfunc ForgedTestIssuer() *Proof { return New(nil, nil, nil, nil, \"\", nil) }\n"},
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
