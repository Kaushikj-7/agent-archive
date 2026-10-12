package backfill

import (
	"github.com/wangjohn/agent-archive/internal/agentmeta"
	"github.com/wangjohn/agent-archive/internal/agents/builtin"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// Regression: 2026-09 review B-23. A batch file whose ID is missing,
// malformed, or not its file's name, or that has no start, is not a batch:
// LoadBatches names it as unreadable instead of returning it, so undo (which
// must see every batch) stops rather than select sessions by an empty ID.
func TestLoadBatchesRejectsInvalidBatchFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
		body string
		want string
	}{
		{"empty object", "zz", `{}`, "no valid import ID"},
		{"empty id", "2026-09-23-7", `{"id":"","started_at":"2026-09-23T10:00:00Z"}`, "no valid import ID"},
		{"future start only", "zz", `{"started_at":"2027-01-01T00:00:00Z"}`, "no valid import ID"},
		{"path in id", "x", `{"id":"../2026-09-23-1","started_at":"2026-09-23T10:00:00Z"}`, "no valid import ID"},
		{"id of another file", "2026-09-23-2", `{"id":"2026-09-23-1","started_at":"2026-09-23T10:00:00Z"}`, "does not match its file name"},
		{"no start", "2026-09-23-3", `{"id":"2026-09-23-3"}`, "no start time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUndoFixture(t)
			good := f.batch("2026-09-22-1", fixedNow.Add(-24*time.Hour))
			if err := os.WriteFile(filepath.Join(f.home, "imports", tc.file+".json"), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			batches, err := LoadBatches(f.home)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `"`+tc.file+`"`) {
				t.Fatalf("err %v, want it to name %q and say %q", err, tc.file, tc.want)
			}
			if len(batches) != 1 || batches[0].ID != good.ID {
				t.Fatalf("batches %+v, want only the valid one", batches)
			}
		})
	}
}

func TestValidBatchID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"2026-09-23-1", "2026-09-23-12"} {
		if !ValidBatchID(id) {
			t.Errorf("ValidBatchID(%q) = false", id)
		}
	}
	for _, id := range []string{"", "2026-09-23", "2026-09-23-0", "2026-09-23-01", "../2026-09-23-1", "2026-09-23-1/x", "2026-09-23-1\n", "x2026-09-23-1"} {
		if ValidBatchID(id) {
			t.Errorf("ValidBatchID(%q) = true", id)
		}
	}
}

// Regression: 2026-09 review B-23. PlanUndo refuses a batch without a valid
// ID outright, whatever the registrations carry.
func TestPlanUndoRefusesBatchWithoutID(t *testing.T) {
	t.Parallel()
	f := newUndoFixture(t)
	f.include("/p")
	f.register("hook-session", "/p", "", fixedNow.UTC())
	for _, id := range []string{"", "not-an-id"} {
		b := Batch{ID: id, StartedAt: fixedNow.Add(-time.Hour)}
		if _, err := PlanUndo(Environment{Sources: testSources, Discovery: builtin.NewBuiltins(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: f.home}, f.store, f.cfg, []Batch{b}, b, ""); err == nil {
			t.Fatalf("ID %q: planned an undo", id)
		}
	}
}

// Invariant (B-23 hardening): whatever registrations and batches exist,
// undo only ever selects registrations InBatch proves are the batch's: an
// import registration carrying its ID. Hook registrations, including one
// carrying an import ID, and other imports' sessions are never selected.
func TestPlanUndoSelectsOnlyProvablyImportedRegistrations(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	ids := []string{"2026-09-23-1", "2026-09-23-2", "2026-09-22-1"}
	for round := range 20 {
		f := newUndoFixture(t)
		f.include("/p")
		f.include("/q")
		for i := range 12 {
			batch := ""
			if rng.IntN(3) > 0 {
				batch = ids[rng.IntN(len(ids))]
			}
			origin := archive.SessionOriginHook
			if rng.IntN(2) == 0 {
				origin = archive.SessionOriginImport
			}
			root := []string{"/p", "/q"}[rng.IntN(2)]
			native := "s-" + string(rune('a'+round)) + "-" + string(rune('a'+i))
			if _, err := f.store.RegisterNewSession(agentmeta.SessionKey{Agent: agentmeta.ID(archive.CanonicalHarness("claude")), NativeID: native}, func(id string) archive.SessionRegistration {
				return archive.SessionRegistration{
					ArchiveSessionID: id, NativeSessionID: native, ProjectID: archive.ProjectID(root), ProjectRoot: root,
					Harness: archive.Harness{Name: "claude"}, SessionStartedAt: fixedNow.Add(-2 * time.Hour),
					RegisteredAt: fixedNow, AdmittedAt: fixedNow, Origin: origin, ImportBatch: archive.NewImportBatch(batch), DestinationID: "dest",
				}
			}); err != nil {
				t.Fatal(err)
			}
		}
		var batches []Batch
		for _, id := range ids {
			b := Batch{ID: id, StartedAt: fixedNow.Add(-time.Hour), DestinationID: "dest"}
			batches = append(batches, b)
		}
		for _, b := range batches {
			plan, err := PlanUndo(Environment{Sources: testSources, Discovery: builtin.NewBuiltins(), DatabaseCatalogs: builtin.NewBuiltins(), NativePaths: builtin.NewBuiltins(), Worktrees: builtin.NewBuiltins(), Workspaces: builtin.NewBuiltins(), Children: builtin.NewBuiltins(), Imports: builtin.NewBuiltins(), Home: f.home}, f.store, f.cfg, batches, b, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range plan.Sessions {
				if s.Registration.Origin != archive.SessionOriginImport || s.Registration.ImportBatch.Recorded() != b.ID {
					t.Fatalf("round %d: undo of %s selected %+v", round, b.ID, s.Registration)
				}
			}
		}
	}
}

// Guard (B-23 hardening): every test of a registration's import ID in
// production code goes through SessionRegistration.InBatch, so no caller
// can select a hook-captured session, or everything with an empty ID, by
// testing the ID itself. The compiler does most of it: archive.ImportBatch
// is not a string and cannot be compared with ==, even once copied into a
// variable. What is left is its one way out, Recorded(): this test allows
// it only in the files listed below, and flags it, or the field, anywhere as
// an operand of == or != (except against "", which asks whether there is an
// ID at all), as a switch tag, as a map index, or as an argument to a
// membership or comparison function.
func TestImportBatchComparedOnlyThroughInBatch(t *testing.T) {
	t.Parallel()
	// file -> why it reads the recorded ID.
	recordedAllowed := map[string]string{
		"internal/backfill/batch.go":     "lists the IDs in use, so a new import picks another",
		"internal/backfill/undo.go":      "names the import a kept project belongs to",
		"internal/state/import_stage.go": "locates the confirmed batch journal; membership still uses InBatch",
	}
	membership := map[string]bool{"Contains": true, "ContainsFunc": true, "Index": true, "IndexFunc": true, "EqualFold": true, "Compare": true, "HasPrefix": true, "HasSuffix": true}
	skipDirs := map[string]bool{"testdata": true, "vendor": true}
	emptyLiterals := map[string]bool{`""`: true, "``": true}
	var offenders []string
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); skipDirs[name] || strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		isField := func(e ast.Expr) bool {
			e = ast.Unparen(e)
			if call, ok := e.(*ast.CallExpr); ok {
				if fun, ok := call.Fun.(*ast.SelectorExpr); ok && fun.Sel.Name == "Recorded" {
					e = fun.X
				}
			}
			sel, ok := e.(*ast.SelectorExpr)
			return ok && sel.Sel.Name == "ImportBatch"
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		isEmpty := func(e ast.Expr) bool {
			lit, ok := ast.Unparen(e).(*ast.BasicLit)
			return ok && lit.Kind == token.STRING && emptyLiterals[lit.Value]
		}
		flag := func(n ast.Node, how string) {
			offenders = append(offenders, fset.Position(n.Pos()).String()+": "+how)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "InBatch" && fn.Recv != nil && file.Name.Name == "archive" {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if n.Sel.Name == "Recorded" && isField(n.X) && recordedAllowed[rel] == "" {
						flag(n, "read with Recorded() outside the files allowed to")
					}
				case *ast.BinaryExpr:
					if n.Op != token.EQL && n.Op != token.NEQ {
						break
					}
					if isField(n.X) && !isEmpty(n.Y) || isField(n.Y) && !isEmpty(n.X) {
						flag(n, "compared with "+n.Op.String())
					}
				case *ast.SwitchStmt:
					if n.Tag != nil && isField(n.Tag) {
						flag(n, "switched on")
					}
				case *ast.IndexExpr:
					if isField(n.Index) {
						flag(n, "used as an index")
					}
				case *ast.CallExpr:
					name := ""
					switch fun := ast.Unparen(n.Fun).(type) {
					case *ast.SelectorExpr:
						name = fun.Sel.Name
					case *ast.Ident:
						name = fun.Name
					}
					if membership[name] && slices.ContainsFunc(n.Args, isField) {
						flag(n, "passed to "+name)
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("test import IDs through backfill.InBatch:\n%s", strings.Join(offenders, "\n"))
	}
}

// InBatch needs an import registration and a non-empty, equal ID.
func TestInBatch(t *testing.T) {
	t.Parallel()
	imported := archive.SessionRegistration{Origin: archive.SessionOriginImport, ImportBatch: archive.NewImportBatch("2026-09-23-1")}
	hook := archive.SessionRegistration{Origin: archive.SessionOriginHook, ImportBatch: archive.NewImportBatch("2026-09-23-1")}
	unbatched := archive.SessionRegistration{Origin: archive.SessionOriginImport}
	for _, tc := range []struct {
		reg  archive.SessionRegistration
		id   string
		want bool
	}{
		{imported, "2026-09-23-1", true},
		{imported, "2026-09-23-2", false},
		{imported, "", false},
		{hook, "2026-09-23-1", false},
		{unbatched, "", false},
	} {
		if got := tc.reg.InBatch(tc.id); got != tc.want {
			t.Errorf("InBatch(%+v, %q) = %v", tc.reg, tc.id, got)
		}
	}
}

// B-25: a relative --since or --until continues a batch when typed alike,
// whatever day it names now; otherwise the days are compared, as for a
// batch that recorded no value.
func TestBatchFiltersMatchRelativeBoundsAsTyped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a    BatchFilters
		b    BatchFilters
		want bool
	}{
		{BatchFilters{Since: "2026-08-24", SinceArg: "30d"}, BatchFilters{Since: "2026-08-25", SinceArg: "30d"}, true},
		{BatchFilters{Since: "2026-08-24", SinceArg: "30d"}, BatchFilters{Since: "2026-08-24", SinceArg: "720h"}, false},
		{BatchFilters{Since: "2026-08-24", SinceArg: "30d"}, BatchFilters{Since: "2026-08-24"}, true},
		{BatchFilters{Since: "2026-08-24"}, BatchFilters{Since: "2026-08-25", SinceArg: "30d"}, false},
		{BatchFilters{Until: "2026-09-01", UntilArg: "3d"}, BatchFilters{Until: "2026-09-02", UntilArg: "3d"}, true},
	} {
		if got := tc.a.equal(tc.b); got != tc.want {
			t.Errorf("%+v vs %+v: %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	if got := shellWord("~/my code/it's"); got != `'~/my code/it'\''s'` {
		t.Errorf("shellWord: %s", got)
	}
	if got := shellWord("~/code/repo-1"); got != "~/code/repo-1" {
		t.Errorf("shellWord: %s", got)
	}
}
