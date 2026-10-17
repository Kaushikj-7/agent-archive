package archive

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// boundaryComparisons returns the position of every comparison in src that
// puts SessionStartedAt against ActivatedAt or DestinationSince: a time
// method call (Before, After, Equal, Compare, Sub) or an operator whose
// operands mention both. It sees direct uses only, not a start copied into a
// variable first; review has to catch those.
func boundaryComparisons(t *testing.T, fset *token.FileSet, file *ast.File) []token.Position {
	t.Helper()
	return comparisons(fset, file, []string{"SessionStartedAt"}, []string{"ActivatedAt", "DestinationSince"})
}

// comparisons returns the position of every comparison in file whose
// operands mention a name from left and a name from right, as
// boundaryComparisons describes.
func comparisons(fset *token.FileSet, file *ast.File, left, right []string) []token.Position {
	mentions := func(node ast.Node, names ...string) bool {
		found := false
		ast.Inspect(node, func(n ast.Node) bool {
			if selector, ok := n.(*ast.SelectorExpr); ok {
				for _, name := range names {
					if selector.Sel.Name == name {
						found = true
					}
				}
			}
			return !found
		})
		return found
	}
	var out []token.Position
	ast.Inspect(file, func(n ast.Node) bool {
		var sides []ast.Node
		switch expr := n.(type) {
		case *ast.CallExpr:
			selector, ok := expr.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			//lint:ignore LV1001 method names from parsed Go source are an open domain
			switch selector.Sel.Name {
			case "Before", "After", "Equal", "Compare", "Sub":
			default:
				return true
			}
			sides = append(sides, selector.X)
			for _, arg := range expr.Args {
				sides = append(sides, arg)
			}
		case *ast.BinaryExpr:
			// && and || join comparisons; Inspect reaches each one.
			if expr.Op == token.LAND || expr.Op == token.LOR {
				return true
			}
			sides = []ast.Node{expr.X, expr.Y}
		default:
			return true
		}
		// An empty left matches any other operand.
		start, boundary := len(left) == 0, false
		for _, side := range sides {
			start = start || mentions(side, left...)
			boundary = boundary || mentions(side, right...)
		}
		if start && boundary {
			out = append(out, fset.Position(n.Pos()))
		}
		return true
	})
	return out
}

// Project activation and the storage destination are admission boundaries.
// A comparison of SessionStartedAt with either one outside Admitted() fails
// silently for imports, whose start is long before both: the collector skips
// the session, or retention leaves its objects behind. Compare Admitted().
func TestNoBoundaryComparesSessionStartedAtOutsideAdmitted(t *testing.T) {
	t.Parallel()
	walkSources(t, func(path string, fset *token.FileSet, file *ast.File) {
		for _, position := range boundaryComparisons(t, fset, file) {
			t.Errorf("%s: compares SessionStartedAt with an admission boundary; use SessionRegistration.Admitted()", position)
		}
	})
}

// A registration's destination is decided by config.InCurrentDestination,
// which compares its DestinationID and falls back to Admitted() against
// DestinationSince only when the ID is empty. Comparing anything with
// DestinationSince elsewhere, directly or through a variable holding an
// admission, skips the ID: a session admitted into a bucket the
// configuration switched away from and back to would look foreign.
func TestNoDestinationTimeComparisonOutsideInCurrentDestination(t *testing.T) {
	t.Parallel()
	walkSources(t, func(path string, fset *token.FileSet, file *ast.File) {
		for _, position := range destinationComparisons(fset, file) {
			t.Errorf("%s: compares with DestinationSince; use config.InCurrentDestination", position)
		}
	})
}

// destinationComparisonAllowed names the functions that may compare with
// DestinationSince, as package.function: the rule itself, and backfill's
// clock check, which compares the import's admission time (not a
// registration) with the destination's start.
var destinationComparisonAllowed = map[string]bool{
	"config.InCurrentDestination": true,
	// Native validation opens a fresh raw-validation window using genuine native
	// birth, not ownership of already retained/frozen publication bytes.
	"config.NativeValidationInCurrentDestination": true,
	// Discovery opens fresh-start permissions, not publication permission.
	"config.ReconcileDiscovery":    true,
	"config.ReconcileCodexCapture": true,
	// Muse window issuance and structural window validation compare the new
	// permission clock, not a retained registration destination. Owner checks
	// continue through NativeValidationInCurrentDestination.
	"config.ReconcileMuseCapture": true,
	"config.validateMuseCapture":  true,
	"backfill.CheckClock":         true,
}

// destinationComparisons returns every comparison in file that mentions
// DestinationSince, whatever the other operand, outside the allowed
// functions.
func destinationComparisons(fset *token.FileSet, file *ast.File) []token.Position {
	var allowed []ast.Node
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && destinationComparisonAllowed[file.Name.Name+"."+fn.Name.Name] {
			allowed = append(allowed, fn)
		}
	}
	var out []token.Position
	for _, position := range comparisons(fset, file, nil, []string{"DestinationSince"}) {
		inside := false
		for _, fn := range allowed {
			start, end := fset.Position(fn.Pos()), fset.Position(fn.End())
			inside = inside || (position.Offset >= start.Offset && position.Offset < end.Offset)
		}
		if !inside {
			out = append(out, position)
		}
	}
	return out
}

// walkSources parses every non-test Go file in the module and calls fn.
func walkSources(t *testing.T, fn func(path string, fset *token.FileSet, file *ast.File)) {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		fn(path, fset, file)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The guard must recognise the comparisons it exists to forbid.
func TestBoundaryGuardDetectsStartComparisons(t *testing.T) {
	t.Parallel()
	src := `package p
func a() {
	_ = r.SessionStartedAt.Before(c.DestinationSince)
	_ = p.ActivatedAt.IsZero() || !r.SessionStartedAt.Before(p.ActivatedAt)
	_ = now.Sub(r.SessionStartedAt) > 0 && r.SessionStartedAt == c.DestinationSince
	_ = r.Admitted().Before(c.DestinationSince)
	_ = r.SessionStartedAt.Before(parent.SessionStartedAt)
}`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "guard.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, position := range boundaryComparisons(t, fset, file) {
		lines = append(lines, position.Line)
	}
	if len(lines) != 3 || lines[0] != 3 || lines[1] != 4 || lines[2] != 5 {
		t.Fatalf("flagged lines %v, want [3 4 5]", lines)
	}
}

// The destination guard must recognise any comparison with DestinationSince,
// through a local variable too, outside the allowed functions, and leave
// other uses of the field and other comparisons alone.
func TestDestinationGuardDetectsComparisons(t *testing.T) {
	t.Parallel()
	src := `package config
func a() {
	_ = r.Admitted().Before(c.DestinationSince)
	_ = !reg.AdmittedAt.IsZero() && reg.AdmittedAt.After(cfg.DestinationSince)
	admitted := r.Admitted()
	_ = admitted.Before(c.DestinationSince)
	_ = c.DestinationSince.IsZero() || !c.DestinationSince.After(admitted)
	_ = r.Admitted().Before(p.ActivatedAt)
	next.DestinationSince = now
}
func (c Config) InCurrentDestination(r archive.SessionRegistration) bool {
	return c.DestinationSince.IsZero() || !r.Admitted().Before(c.DestinationSince)
}
func CheckClock() {
	_ = admittedAt.Before(cfg.DestinationSince)
}`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "guard.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, position := range destinationComparisons(fset, file) {
		lines = append(lines, position.Line)
	}
	// CheckClock is allowed only in package backfill.
	if want := []int{3, 4, 6, 7, 15}; !slices.Equal(lines, want) {
		t.Fatalf("flagged lines %v, want %v", lines, want)
	}
}

func TestDestinationGuardAllowsOnlyNamedNativeValidationPolicy(t *testing.T) {
	t.Parallel()
	source := `package config
func (c Config) NativeValidationInCurrentDestination() bool {
 return !binding.NativeCreatedAt.Before(c.DestinationSince)
}
func unrelated() bool {
 return !binding.NativeCreatedAt.Before(c.DestinationSince)
}`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "native.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	positions := destinationComparisons(fset, file)
	if len(positions) != 1 || positions[0].Line != 6 {
		t.Fatalf("guard positions %v, want only unrelated line 6", positions)
	}
	file.Name.Name = "state"
	if positions = destinationComparisons(fset, file); len(positions) != 2 {
		t.Fatalf("policy name exempted outside config: %v", positions)
	}
}
