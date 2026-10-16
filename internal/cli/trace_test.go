package cli

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

// withTrace sets AGENT_ARCHIVE_TRACE to value in env, keeping every other
// variable as it was.
func withTrace(env Env, value string) Env {
	previous := env.LookupEnv
	env.LookupEnv = func(key string) (string, bool) {
		if key == envTrace {
			return value, true
		}
		if previous != nil {
			return previous(key)
		}
		return "", false
	}
	return env
}

// With AGENT_ARCHIVE_TRACE on, a command's output is unchanged and stderr
// ends with the timing tree: fixed span names and counts, never a session
// ID or title. Not parallel: the recorder is process-wide.
func TestTraceWritesTheTimingTreeToStderr(t *testing.T) {
	env, _, id := publishedFixture(t)
	var plain, plainErr bytes.Buffer
	if code := Run([]string{"list", "--limit", "0"}, nil, &plain, &plainErr, env); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, plainErr.String())
	}
	if strings.Contains(plainErr.String(), "trace") {
		t.Fatalf("traced without AGENT_ARCHIVE_TRACE:\n%s", plainErr.String())
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"list", "--limit", "0"}, nil, &out, &errOut, withTrace(env, "1")); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if out.String() != plain.String() {
		t.Fatalf("tracing changed stdout:\n%s\nwant:\n%s", out.String(), plain.String())
	}
	got := errOut.String()
	for _, want := range []string{"agent-archive trace", "  list  ", "load config", "open store", spinnerSpan, "list metadata", "list objects", "read sidecars", "sidecars 1", "from cache"} {
		if !strings.Contains(got, want) {
			t.Errorf("trace lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, id) || strings.Contains(got, id[:8]) || strings.Contains(got, "sessions/") {
		t.Errorf("trace names a session or key:\n%s", got)
	}
}

// Catalog summaries and cached metadata bodies are distinct work. In
// particular, a warm summary query must not report a body-cache hit.
// Not parallel: the trace recorder is process-wide.
func TestTraceCatalogDistinguishesSummaryReuseFromBodyReads(t *testing.T) {
	a := newScopedArchive(t)
	a.add(t, a.id, "private catalog title", a.label, func(m *archive.Metadata) { m.Name = "private catalog name" })
	env, id := a.env, a.id
	reads := 0
	env.observeListBody = func(string, bool) { reads++ }
	var cold, warm bytes.Buffer
	for _, tc := range []struct {
		output *bytes.Buffer
		reads  int
		counts string
		reused string
	}{
		{&cold, 1, "sidecars 1, from cache 0, downloaded 1", "from catalog 0"},
		{&warm, 0, "sidecars 0, from cache 0, downloaded 0", "from catalog 1"},
	} {
		reads = 0
		var stderr bytes.Buffer
		if code := Run([]string{"list", "--limit", "0"}, nil, tc.output, &stderr, withTrace(env, "1")); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
		if reads != tc.reads {
			t.Fatalf("body reads=%d want=%d", reads, tc.reads)
		}
		got := stderr.String()
		for _, want := range []string{"list metadata", "read metadata headers", "read sidecars", "refresh session catalog", "query session catalog", "summaries 1", tc.counts, tc.reused} {
			if !strings.Contains(got, want) {
				t.Errorf("trace lacks %q:\n%s", want, got)
			}
		}
		for _, private := range []string{id, id[:8], "sessions/", "private catalog title", "private catalog name", "native-1", "visible", a.dir} {
			if strings.Contains(got, private) {
				t.Errorf("trace contains private value %q:\n%s", private, got)
			}
		}
	}
	if cold.String() != warm.String() {
		t.Fatal("catalog refresh changed list output")
	}
}

// A value that is not a switch leaves tracing off without failing the
// command, and the hook's internal commands never trace.
// Not parallel: a trace it failed to suppress would start the process-wide recorder.
func TestTraceOffForInvalidValuesAndInternalCommands(t *testing.T) {
	env, _, _ := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"list"}, nil, &out, &errOut, withTrace(env, "verbose")); code != 0 || strings.Contains(errOut.String(), "trace") {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	// A lookup that reports the variable unset, whatever value it hands
	// back with that, is off.
	errOut.Reset()
	unset := env
	unset.LookupEnv = func(string) (string, bool) { return "1", false }
	if code := Run([]string{"list"}, nil, &out, &errOut, unset); code != 0 || strings.Contains(errOut.String(), "trace") {
		t.Fatalf("traced with the variable unset: exit=%d stderr=%s", code, errOut.String())
	}
	errOut.Reset()
	Run([]string{"_collect"}, nil, &out, &errOut, withTrace(env, "1"))
	if strings.Contains(errOut.String(), "agent-archive trace") {
		t.Fatalf("an internal command traced:\n%s", errOut.String())
	}
}

// Only known commands trace, so the root span carries a command's own name,
// never a word from the command line; help and version print no tree.
// Not parallel: a trace it failed to suppress would start the process-wide recorder.
func TestTraceOnlyKnownCommands(t *testing.T) {
	env, _, _ := publishedFixture(t)
	for _, args := range [][]string{{"frobnicate-private-word"}, {"--version"}, {"help"}} {
		var out, errOut bytes.Buffer
		Run(args, nil, &out, &errOut, withTrace(env, "1"))
		if strings.Contains(errOut.String(), "agent-archive trace") {
			t.Fatalf("%v traced:\n%s", args, errOut.String())
		}
	}
}

// tracedCommands must name exactly the user-facing commands Run dispatches
// (every case but the internal "_" ones, help and version), so a new
// command can't be silently left untraced. Read from Run's own switch.
func TestTracedCommandsMatchRunsSwitch(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "cli.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	dispatched := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Run" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					name, _ := strconv.Unquote(lit.Value)
					if !strings.HasPrefix(name, "_") && !strings.HasPrefix(name, "-") && name != "help" && name != "version" {
						dispatched[name] = true
					}
				}
			}
			return true
		})
	}
	if len(dispatched) == 0 {
		t.Fatal("found no command cases in Run")
	}
	for name := range dispatched {
		if !tracedCommands[name] {
			t.Errorf("Run dispatches %q but tracedCommands lacks it", name)
		}
	}
	for name := range tracedCommands {
		if !dispatched[name] {
			t.Errorf("tracedCommands has %q, which Run does not dispatch", name)
		}
	}
}

// The trace is written once, however many times finishTraceNow is called
// (early by a handoff, then by Run's deferred call), and a launched agent
// does not inherit the switch.
// Not parallel: it starts the process-wide recorder.
func TestTraceFinishesOnce(t *testing.T) {
	var errOut bytes.Buffer
	startTrace("handoff", &errOut, withTrace(Env{}, "1"))
	finishTraceNow()
	written := errOut.String()
	if !strings.Contains(written, "agent-archive trace") {
		t.Fatalf("finishTraceNow wrote nothing:\n%s", written)
	}
	finishTraceNow()
	if errOut.String() != written {
		t.Fatalf("a second finishTraceNow wrote the trace again:\n%s", errOut.String())
	}
	for _, kv := range childEnv([]string{envTrace + "=1", "HOME=/h"}, launchEnvironmentKeys(Env{})) {
		if strings.HasPrefix(kv, envTrace+"=") {
			t.Fatalf("a launched agent inherits %s", kv)
		}
	}
}

// A handoff continued in this terminal has written its whole trace by the
// time the agent starts, so the tree times the handoff and appears before
// the agent takes over the terminal.
// Not parallel: it starts the process-wide recorder.
func TestTraceIsWrittenBeforeAHandoffLaunchesHere(t *testing.T) {
	f := newHandoffFixture(t, false)
	f.env.Executable = func() (string, error) { return "/opt/agent-archive", nil }
	env := withTrace(f.env, "1")
	launched, tracedFirst := false, false
	env.LaunchHandoff = func(_ launchSpec, _ io.Reader, _, stderr io.Writer) error {
		launched = true
		if b, ok := stderr.(*bytes.Buffer); ok {
			tracedFirst = strings.Contains(b.String(), "agent-archive trace")
		}
		return nil
	}
	_, errOut, code := runPicker(t, env, "", f.id, "--to", "claude")
	if code != 0 || !launched || !tracedFirst {
		t.Fatalf("code=%d launched=%v trace written before launch=%v stderr=%s", code, launched, tracedFirst, errOut)
	}
	if strings.Count(errOut, "agent-archive trace") != 1 {
		t.Fatalf("trace written %d times:\n%s", strings.Count(errOut, "agent-archive trace"), errOut)
	}
}

// Eval was added to tracing by the published parent. Its local JSON Lines
// stay unchanged and the trace names no native session or source path.
// Not parallel: the trace recorder is process-wide.
func TestEvalExportTracePreservesLocalRecords(t *testing.T) {
	f := localEvalFixture(t)
	path := filepath.Join(f.userHome, ".claude", "projects", "slug-c-lev-1", "c-lev-1.jsonl")
	plain, plainErr, code := f.evalLines(t, "", "--file", path, "--harness", "claude")
	if code != 0 || plainErr != "" {
		t.Fatalf("plain code %d stderr %s", code, plainErr)
	}
	f.env = withTrace(f.env, "1")
	got, stderr, code := f.evalLines(t, "", "--file", path, "--harness", "claude")
	if code != 0 || !reflect.DeepEqual(got, plain) || !strings.Contains(stderr, "agent-archive trace") {
		t.Fatalf("trace code %d records %v stderr %s", code, got, stderr)
	}
	if strings.Contains(stderr, path) || strings.Contains(stderr, "c-lev-1") {
		t.Fatalf("trace exposed local identity: %s", stderr)
	}
}
