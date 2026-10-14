package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// chooserCallers are the places a person chooses a session (the table of
// design section 5 of dev/specs/session-finding.md). Each names the chain of
// functions from the command down to the one that calls runBrowser: every
// function in a chain must call the next, and the last must call runBrowser.
var chooserCallers = []struct {
	name  string
	chain []string
}{
	{"list", []string{"runListCommand"}},
	{"list (bounded catalog)", []string{"runListCommand", "runBoundedListBrowser"}},
	{"show (no ID)", []string{"runShowCommand", "runBareShow"}},
	{"show --json (no ID)", []string{"runShowCommand", "runBareShow", "selectArchivedSession"}},
	{`show "<words>", several matches`, []string{"runShowCommand", "resolveShowQuery"}},
	{"handoff before setup", []string{"runHandoffCommand", "resolveNativeHandoff", "chooseNativePreviews"}},
	{"handoff (no selector)", []string{"runHandoffCommand", "chooseHandoffSession", "selectHandoffSession"}},
	{`handoff "<words>", several matches`, []string{"runHandoffCommand", "resolveHandoffQuery", "handoffQueryResolver.resolve", "handoffQueryResolver.choose"}},
}

// browserRunners are the files that may run the picker's loop: the browser
// itself and what it is built from. A chooser anywhere else is a second
// browser.
var browserRunners = map[string]bool{
	"browser.go": true, "browser_filter.go": true, "list_browse.go": true, "list_browse_keys.go": true,
}

// browserLoops are the functions that read a person's choice of a session,
// as a screen of rows with a prompt (the key browser and its line-mode
// fallback), and matchBrowseRow, which turns a typed answer into one of the
// rows: a chooser of its own, reading lines, would reuse it. Only the
// browser's files call them.
var browserLoops = map[string]bool{"pickScoped": true, "pickRows": true, "pickKeys": true, "pick": true, "matchBrowseRow": true}

// funcCalls is, for each function of the package that is not a test, the
// names it calls, by "Receiver.name" for a method and "name" for a function,
// and the files it is in.
func funcCalls(t *testing.T) (calls map[string]map[string]bool, files map[string]string) {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	calls, files = map[string]map[string]bool{}, map[string]string{}
	fset := token.NewFileSet()
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				key = receiverName(fn.Recv.List[0].Type) + "." + key
			}
			called := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					called[calleeName(call.Fun)] = true
				}
				return true
			})
			calls[key], files[key] = called, name
		}
	}
	return calls, files
}

// receiverName is the type name of a method's receiver.
func receiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return receiverName(e.X)
	case *ast.Ident:
		return e.Name
	}
	return ""
}

// calleeName is the name a call is made by: a function's, or a method's.
func calleeName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

// shortName is a function's name without its receiver.
func shortName(key string) string {
	_, name, _ := strings.Cut(key, ".")
	if name == "" {
		return key
	}
	return name
}

// Every place a person chooses a session runs runBrowser, so a new chooser
// cannot quietly grow a loop of its own: each caller of the design's table
// reaches it, only those places call it, and nothing outside the browser's
// own files runs its loop.
func TestEveryChooserReachesTheOneBrowser(t *testing.T) {
	t.Parallel()
	calls, files := funcCalls(t)
	for _, caller := range chooserCallers {
		for i, fn := range caller.chain {
			if calls[fn] == nil {
				t.Errorf("%s: %s is not a function of the package", caller.name, fn)
				continue
			}
			next := "runBrowser"
			if i < len(caller.chain)-1 {
				next = shortName(caller.chain[i+1])
			}
			if !calls[fn][next] {
				t.Errorf("%s: %s does not call %s", caller.name, fn, next)
			}
		}
	}
	// runBrowser is called by the callers' functions, and by nothing else:
	// a new chooser adds a function here and must be added to the table.
	var callers []string
	for fn, called := range calls {
		if called["runBrowser"] {
			callers = append(callers, fn)
		}
	}
	sort.Strings(callers)
	var listed []string
	for _, caller := range chooserCallers {
		listed = append(listed, caller.chain[len(caller.chain)-1])
	}
	// The functions that run it directly are the last of a chain; bare show
	// and show --json share runBareShow, which calls it for one and goes on to
	// selectArchivedSession for the other.
	want := map[string]bool{}
	for _, fn := range listed {
		want[fn] = true
	}
	for _, fn := range callers {
		if !want[fn] {
			t.Errorf("%s calls runBrowser but is not in chooserCallers: add the chooser to the table of design section 5 and to this list", fn)
		}
	}
	// And only the browser's own files run its loop.
	for fn, called := range calls {
		for loop := range browserLoops {
			if called[loop] && !browserRunners[files[fn]] {
				t.Errorf("%s (%s) calls %s: a chooser outside the one browser; call runBrowser instead", fn, files[fn], loop)
			}
		}
	}
}
