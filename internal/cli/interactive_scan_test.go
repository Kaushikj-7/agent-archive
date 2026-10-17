package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// classifiedCalls says, for each file, how many times it may use a
// terminal check (or read standard input) and why. A terminal check decides
// one of two kinds of thing:
//
//   - interaction (a picker, a prompt, a pager, the alternate screen, reading
//     a secret): must go through Env.interactive, which AGENT_ARCHIVE_NONINTERACTIVE
//     can turn off. Those never appear in these tables.
//   - presentation (colour, redrawing a line in place, wrapping to the
//     terminal's width) or mechanism (restoring modes a prompt changed): may
//     look at the terminal directly, since NO_COLOR and the terminal, not the
//     switch, govern them. Each is listed here with what it decides.
//
// A new use fails TestTerminalChecksAreClassified until it is listed, which
// is when to decide which of the two it is.
//
// The scan counts every mention, not only calls, so a method value
// (check := env.isTerminal) or a renamed import (import t "golang.org/x/term")
// cannot slip past it. It is syntactic, so it cannot follow a terminal check
// made through a value passed in from another package, or a stream read
// through a helper that is itself listed; that is what the classification
// comments and code review are for.
type classifiedCalls map[string]map[string]int

// rawTerminalChecks are mentions of Env.isTerminal.
var rawTerminalChecks = classifiedCalls{
	// The definition of interactive and its two companions: they look at the
	// terminal so that no one else has to.
	"interactive.go": {"isTerminal": 2},
	// backfill's progress line redraws in place on a terminal; that is
	// presentation. It asks nothing.
	"backfill_import.go": {"isTerminal": 1},
	// stats --html refuses to write a web page onto a terminal (it asks for
	// --output instead). That is a refusal, not a prompt, and it holds inside
	// an agent too, where a page on the pty would land in the agent's
	// context, so it asks the terminal itself rather than Env.interactive.
	"stats.go": {"isTerminal": 1},
	// eval export --ids-from - refuses a terminal as its list of inputs:
	// a refusal, not a prompt, which holds inside an agent too.
	"eval_export.go": {"isTerminal": 1},
}

// injectedTerminalChecks are mentions of the Env.IsTerminal field itself,
// which Env.isTerminal wraps.
var injectedTerminalChecks = classifiedCalls{
	// Env.isTerminal: the nil check and the call.
	"cli.go": {"IsTerminal": 2},
}

// terminalPackageUses are uses of packages that ask a file descriptor about
// its terminal (golang.org/x/term and its kin), by whatever name they are
// imported under.
var terminalPackageUses = classifiedCalls{
	// Shared browser cooked-input readiness: reached only after interaction
	// is enabled; polls descriptors without changing terminal modes.
	"browser_line_unix.go": {"unix.PollFd": 1, "unix.POLLIN": 1, "unix.Poll": 1},
	// Env.isTerminal itself, the default for Env.IsTerminal.
	"cli.go": {"term.IsTerminal": 1},
	// styleFor: colour, redrawing, and width. Presentation, governed by
	// NO_COLOR and TERM.
	// and Env.terminalSize, the window size the browser and pickers fit.
	"ui.go": {"term.IsTerminal": 2, "term.GetSize": 2},
	// Key mode: the session browser reads keys one at a time. It is set up
	// only by runBrowser, which runs after browseInteractive, and
	// its terminal is restored on every way out. The prompt line guard also
	// restores hidden input before interrupts and job control; the pending-input
	// probe only checks readiness and never consumes an answer. Before ordinary
	// prompt emission, echoSuppressed inspects the original ECHO/ECHONL modes
	// so completion counts only actual character echo and supplies missing
	// newline echo without changing the user's modes.
	"keys_unix.go": {
		"term.IsTerminal": 1, "unix.IoctlGetTermios": 5, "unix.IoctlSetTermios": 5,
		"unix.ICANON": 2, "unix.ECHO": 4, "unix.ECHONL": 3, "unix.IEXTEN": 1, "unix.ISIG": 2, "unix.ICRNL": 1,
		"unix.VMIN": 1, "unix.VTIME": 1, "unix.VQUIT": 1,
		"unix.Select": 2, "unix.FdSet": 2, "unix.Timeval": 1, "unix.NsecToTimeval": 1, "unix.Read": 1, "unix.EINTR": 2,
		"unix.Kill": 3, "unix.SIGSTOP": 3, "unix.Termios": 2,
	},
	// Prompt capabilities use descriptor identity/size for presentation; guarded
	// hidden reads and lifecycle restoration require terminal mechanisms.
	"prompt_render.go": {"term.IsTerminal": 4, "term.GetSize": 1},
	"keys_darwin.go":   {"unix.TIOCGETA": 1, "unix.TIOCSETA": 1, "unix.TIOCSETAF": 1},
	"keys_linux.go":    {"unix.TCGETS": 1, "unix.TCSETS": 1, "unix.TCSETSF": 1},
	// saveTerminalState restores modes the pager or a prompt changed; it does
	// nothing unless something interactive already ran.
	"list_browse.go": {"term.IsTerminal": 1, "term.GetState": 1, "term.Restore": 1},
	// prompter.secret hides what is typed. Reached only from the setup
	// prompts (behind Env.interactive) and from readR2Secret, which refuses a
	// terminal that interaction is switched off for before it gets here.
	"prompt.go": {"term.IsTerminal": 1, "term.GetState": 1, "term.Restore": 2},
	// readSecret, prompter.secret's reader (above), and the same elsewhere.
	"secret_unix.go": {
		"unix.IoctlGetTermios": 1, "unix.IoctlSetTermios": 2, "unix.ECHO": 1, "unix.ICANON": 1,
		"unix.ISIG": 1, "unix.ICRNL": 1,
	},
	"secret_other.go": {"term.ReadPassword": 1},
}

// promptSites are the mentions of newPrompter (and of a prompter built by
// hand): every place agent-archive can ask a question. The comment says what
// stops the question when interaction is off.
var promptSites = classifiedCalls{
	"picker_browse.go":    {"newPrompter": 1},                  // bounded list browser is reached only after browseInteractive, through runListCommand.
	"purge.go":            {"newPrompter": 1},                  // Exact digest consent is refused when noninteractive unless --yes.
	"machines_revoke.go":  {"newPrompter": 1},                  // confirmation and token prompt require Env.interactive; --yes/--json never prompt
	"machines_own_key.go": {"newPrompter": 1},                  // own-key confirmation/token require Env.interactive; --yes uses environment only
	"pairing_rollout.go":  {"newPrompter": 1},                  // first-run question requires terminal input/output, fresh setup, and Env.interactive
	"machines_verify.go":  {"newPrompter": 1},                  // readManagementToken prompts only with env.interactive(stdin), without --yes or --json.
	"issuance.go":         {"newPrompter": 1},                  // reserveSpare builds a nil-input/discard-output prompter for checkKey diagnostics only; it never reads or asks a question. Selection questions use the caller's source prompter behind its interactive gate.
	"pairing_source.go":   {"newPrompter": 1},                  // source requires interactive input/output unless deliberate --yes; the scripted path never prompts
	"pairing_receive.go":  {"newPrompter": 2},                  // receiver refuses prompts-off unless --yes; redirected bundle input switches code and review to a checked private terminal
	"prompt.go":           {"newPrompter": 2, "prompter{}": 1}, // the definition, and typedInput.prompter, which handoff's picker and ambiguous-title chooser ask through (both behind browseInteractive: see handoff_select.go and handoff_title.go)
	"setup.go":            {"newPrompter": 3},                  // interactive setup, buffered-input handoff, and output-only completion buffer; runSetupCommand requires Env.interactive
	"setup_review.go":     {"newPrompter": 1},                  // Details is rendered to an output-only buffer; it never reads or asks a question.
	"setup_flags.go":      {"newPrompter": 1, "prompter{}": 1}, // setup --yes: only reads a secret, guarded in readR2Secret; the literal has no input, it only prints
	"uninstall.go":        {"newPrompter": 1},                  // runUninstallCommand refuses unless env.interactive(stdin) or --yes
	"backfill.go":         {"newPrompter": 1},                  // runBackfillCommand refuses unless env.interactive(stdin) or --yes
	"backfill_undo.go":    {"newPrompter": 1},                  // runBackfillUndo refuses unless env.interactive(stdin) or --yes
	"list_browse.go":      {"newPrompter": 1},                  // selectArchivedSession: reached only after browseInteractive
	"inspect.go":          {"newPrompter": 2},                  // list and show browsers: reached only after browseInteractive
	"show_resolve.go":     {"newPrompter": 1},                  // the ambiguity picker, after browseInteractive
	"handoff.go":          {"newPrompter": 1},                  // "Continue in:": offersDestinations requires browseInteractive
	"handoff_match.go":    {"newPrompter": 1},                  // "Hand off this session?" for a repository-only match: gateRepoMatch refuses without browseInteractive
	"handoff_worktree.go": {"newPrompter": 1},                  // the active-source y/N/w question: checkActiveSource asks only when env.interactive(stdin) and (stderr)
}

// inputReads are the ways a command reads a stream it was handed, other than
// through a prompter. A read of standard input that waits for a person must
// be refused when interaction is off; a read of a file need not be.
var inputReads = classifiedCalls{
	"capabilities.go":        {"io.Copy": 1},                           // fixed version subprocess stdout through the 4096-byte capped writer; never terminal input
	"codex_label_host.go":    {"bufio.NewReaderSize": 1, "io.Copy": 1}, // bounded native process stdout/stderr only; never terminal input or an approval prompt
	"setup_project_scope.go": {"json.NewDecoder": 1, "io.ReadAll": 1},  // explicit JSON or --yes file/stdin scope, never an implicit prompt
	"pairing_receive.go":     {"io.ReadAll": 1, "bufio.NewReader": 1},  // bounded bundle input and buffered first-run answers; code/review preserve the checked terminal
	"pairing_ledger.go":      {"io.ReadAll": 1},                        // bounded local ledger files, never input
	"machines_revoke.go":     {"io.ReadAll": 1, "json.NewDecoder": 1},  // explicitly selected bounded operator binding file, never stdin

	// The prompter's own line reader: every prompt (see promptSites); and
	// handoff's one buffer for its answers (typedInput), read only by the
	// picker (and the ambiguous-title chooser) and the "Continue in:" prompt,
	// both behind browseInteractive. A launched agent run here gets stdin
	// itself, not through a reader: it is the agent's terminal, not a
	// question, and runs only when browseInteractive.
	"prompt.go": {"bufio.NewReader": 2},
	// The closed hook payload comes from the agent; the second decoder checks
	// bounded JSON in memory for the optional strict native contract.
	"hook_command.go": {"json.NewDecoder": 2, "io.ReadAll": 1},
	// purge apply's typed digest: refused in runPurgeApply when the switch is
	// on and --yes is not given.
	// Files, not standard input.
	"setup_aws.go": {"bufio.NewScanner": 1},
	// eval export --ids-from -: a list piped in, refused when standard input
	// is a terminal, so it never waits for a person.
	"eval_export.go": {"bufio.NewScanner": 1},
	// copyRootFile: a file copied into the new worktree.
	"handoff_worktree.go": {"io.Copy": 1},
	"feedback.go":         {"io.ReadAll": 1},
	// stats --prices: a file the person names, read in full, bounded.
	"stats.go": {"io.ReadAll": 1},
}

// terminalImports are packages whose only use here is to ask about, or
// change, a terminal.
var terminalImports = map[string]bool{
	"golang.org/x/term":          true,
	"golang.org/x/sys/unix":      true,
	"github.com/mattn/go-isatty": true,
}

// readers are the selectors that read a stream to its end or line by line.
var readers = map[string]bool{
	"bufio.NewReader": true, "bufio.NewReaderSize": true, "bufio.NewScanner": true,
	"json.NewDecoder": true, "io.ReadAll": true, "io.ReadFull": true, "io.Copy": true,
	"fmt.Fscan": true, "fmt.Fscanf": true, "fmt.Fscanln": true, "ioutil.ReadAll": true,
}

// terminalUses counts, by kind and name, everything in file that asks about
// a terminal, builds a prompt, or reads a stream.
func terminalUses(t *testing.T, file *ast.File) map[string]map[string]int {
	t.Helper()
	found := map[string]map[string]int{}
	note := func(kind, name string) {
		if found[kind] == nil {
			found[kind] = map[string]int{}
		}
		found[kind][name]++
	}
	terminalPackages := map[string]bool{}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		if !terminalImports[importPath] {
			continue
		}
		local := path.Base(importPath)
		if spec.Name != nil {
			local = spec.Name.Name
		}
		//lint:ignore LV1001 the import name as written in source, not a value this package defines
		if local == "." || local == "_" {
			note("term", "import "+local+" "+importPath)
			continue
		}
		terminalPackages[local] = true
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			x, _ := n.X.(*ast.Ident)
			switch {
			case n.Sel.Name == "isTerminal":
				note("isTerminal", "isTerminal")
			case x != nil && terminalPackages[x.Name]:
				note("term", x.Name+"."+n.Sel.Name)
			case n.Sel.Name == "IsTerminal":
				note("field", "IsTerminal")
			case n.Sel.Name == "ModeCharDevice":
				// A hand-rolled check: os.Stdin.Stat() and ModeCharDevice.
				note("term", "ModeCharDevice")
			case x != nil && readers[x.Name+"."+n.Sel.Name]:
				note("read", x.Name+"."+n.Sel.Name)
			case x != nil && x.Name == "os" && n.Sel.Name == "Stdin":
				note("read", "os.Stdin")
			}
		case *ast.Ident:
			// Every mention: a call, a method value, an alias.
			if n.Name == "newPrompter" {
				note("newPrompter", "newPrompter")
			}
		case *ast.CompositeLit:
			// A prompter built by hand skips newPrompter.
			if id, ok := n.Type.(*ast.Ident); ok && id.Name == "prompter" {
				note("newPrompter", "prompter{}")
			}
		}
		return true
	})
	return found
}

// A use outside the tables is a decision nobody has made: whether it may ask
// the person something an agent's shell would hang on.
// TestTerminalChecksAreClassified makes that decision explicit.
func TestTerminalChecksAreClassified(t *testing.T) {
	t.Parallel()
	found := map[string]map[string]map[string]int{"isTerminal": {}, "field": {}, "term": {}, "newPrompter": {}, "read": {}}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for kind, names := range terminalUses(t, parsed) {
			found[kind][file] = names
		}
	}
	compare(t, "Env.isTerminal", rawTerminalChecks, found["isTerminal"], "use env.interactive(stream) for anything that asks, pages, or takes over the screen; list a presentation-only check in rawTerminalChecks")
	compare(t, "Env.IsTerminal field", injectedTerminalChecks, found["field"], "use env.interactive(stream); Env.isTerminal is the only reader of the field")
	compare(t, "terminal package", terminalPackageUses, found["term"], "use env.interactive(stream); list a presentation or mechanism use in terminalPackageUses")
	compare(t, "newPrompter", promptSites, found["newPrompter"], "a new question must be unreachable when interaction is off: gate it with env.interactive, then list it in promptSites")
	compare(t, "stream read", inputReads, found["read"], "a read of standard input that waits for a person must be refused when interaction is off (see runPurgeApply); list a read that never waits for one in inputReads")
}

// The scan must notice the ways a check could be hidden from a plain search
// for calls: a method value, a renamed import, an alias of newPrompter, a
// prompter built by hand, a hand-rolled character device test.
func TestTerminalScanNoticesHiddenChecks(t *testing.T) {
	t.Parallel()
	source := `package cli

import (
	"bufio"
	"os"
	t "golang.org/x/term"
)

func a(e Env) { check := e.isTerminal; _ = check }
func b(e Env) { _ = t.IsTerminal; _ = t.ReadPassword }
func c(in io.Reader) { _ = bufio.NewReader(in); p := newPrompter; _ = p; _ = &prompter{} }
func d() { info, _ := os.Stdin.Stat(); _ = info.Mode() & os.ModeCharDevice }
func e2(e Env) { _ = e.IsTerminal }
`
	parsed, err := parser.ParseFile(token.NewFileSet(), "x.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]int{
		"isTerminal":  {"isTerminal": 1},
		"term":        {"t.IsTerminal": 1, "t.ReadPassword": 1, "ModeCharDevice": 1},
		"read":        {"bufio.NewReader": 1, "os.Stdin": 1},
		"newPrompter": {"newPrompter": 1, "prompter{}": 1},
		"field":       {"IsTerminal": 1},
	}
	got := terminalUses(t, parsed)
	for kind, names := range want {
		for name, count := range names {
			if got[kind][name] != count {
				t.Errorf("%s %s: noticed %d, want %d", kind, name, got[kind][name], count)
			}
		}
	}
}

func compare(t *testing.T, what string, want classifiedCalls, got map[string]map[string]int, advice string) {
	t.Helper()
	var files []string
	seen := map[string]bool{}
	for file := range want {
		files, seen[file] = append(files, file), true
	}
	for file := range got {
		if !seen[file] {
			files = append(files, file)
		}
	}
	sort.Strings(files)
	for _, file := range files {
		for name := range union(want[file], got[file]) {
			if want[file][name] != got[file][name] {
				t.Errorf("%s: %d %s use(s) of %s, but %d are classified. %s", file, got[file][name], what, name, want[file][name], advice)
			}
		}
	}
}

func union(a, b map[string]int) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}
