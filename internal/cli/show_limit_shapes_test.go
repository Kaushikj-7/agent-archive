package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// codexLines builds a Codex transcript: the session header, then the records
// emit writes through line.
func codexLines(project string, emit func(line func(payload string))) string {
	var b strings.Builder
	at := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	line := func(payload string) {
		at = at.Add(time.Second)
		fmt.Fprintf(&b, `{"type":"response_item","timestamp":%q,"payload":%s}`+"\n", at.Format(time.RFC3339), payload)
	}
	fmt.Fprintf(&b, `{"type":"session_meta","timestamp":"2026-01-02T00:00:00Z","payload":{"id":"native-1","cwd":%q}}`+"\n", project)
	fmt.Fprintf(&b, `{"type":"turn_context","timestamp":"2026-01-02T00:00:01Z","payload":{"cwd":%q,"model":"gpt-test"}}`+"\n", project)
	emit(line)
	return b.String()
}

// newFixtureFrom registers a Codex session whose transcript build writes,
// and syncs it, so `show` reads it back from the archive.
func newFixtureFrom(t *testing.T, build func(project string) string) handoffFixture {
	t.Helper()
	f := newHandoffFixture(t, false)
	if err := os.WriteFile(filepath.Join(f.project, "codex.jsonl"), []byte(build(f.project)), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runSyncCommand(nil, &out, &errOut, f.env); code != 0 {
		t.Fatalf("sync code=%d stderr=%s", code, errOut.String())
	}
	return f
}

func codexMessage(role, text string) string {
	kind := "output_text"
	if role == "user" {
		kind = "input_text"
	}
	b, _ := json.Marshal(text)
	return fmt.Sprintf(`{"type":"message","role":%q,"content":[{"type":%q,"text":%s}]}`, role, kind, b)
}

// Whatever shape a long session has, `show --transcript` prints no more than
// the limit (when the limit leaves room for a header and one exchange), is
// valid UTF-8, and is valid JSON with --json.
func TestShowTranscriptStaysWithinTheLimitForEveryShapeOfSession(t *testing.T) {
	t.Parallel()
	promptRepeats, exchanges, steps := 200000, 4000, 20000
	longSteps, stepRepeats, toolExchanges, resultRepeats := 30, 20000, 60, 1200
	if testing.Short() {
		// Keep every shape and output mode, with multi-byte content still
		// large enough to exercise the default limit and protected-tail cuts.
		promptRepeats, exchanges, steps = 20000, 2200, 5000
		longSteps, stepRepeats, toolExchanges, resultRepeats = 6, 4000, 8, 400
	}
	shapes := map[string]func(project string) string{
		// One prompt that is megabytes of multi-byte text.
		"one huge prompt": func(p string) string {
			return codexLines(p, func(line func(string)) {
				line(codexMessage("user", strings.Repeat("é世🙂 ", promptRepeats)))
				line(codexMessage("assistant", "ok"))
			})
		},
		// Thousands of exchanges, each tiny.
		"thousands of tiny exchanges": func(p string) string {
			return codexLines(p, func(line func(string)) {
				for i := range exchanges {
					line(codexMessage("user", fmt.Sprintf("q%d", i)))
					line(codexMessage("assistant", "a"))
				}
			})
		},
		// One autonomous run: a prompt and thousands of steps, so there is no
		// older exchange to drop.
		"one exchange of thousands of steps": func(p string) string {
			return codexLines(p, func(line func(string)) {
				line(codexMessage("user", "hi"))
				for i := range steps {
					line(codexMessage("assistant", fmt.Sprintf("step %d some words here", i)))
				}
			})
		},
		// Every step of the protected tail is long.
		"long steps in the newest exchange": func(p string) string {
			return codexLines(p, func(line func(string)) {
				line(codexMessage("user", "hi"))
				for range longSteps {
					line(codexMessage("assistant", strings.Repeat("é🙂 ", stepRepeats)))
				}
			})
		},
		// Long tool results, which --full prints.
		"long tool results": func(p string) string {
			return codexLines(p, func(line func(string)) {
				for i := range toolExchanges {
					line(codexMessage("user", fmt.Sprintf("q%d", i)))
					for j := range 3 {
						line(fmt.Sprintf(`{"type":"function_call","name":"exec_command","call_id":"c%d_%d","arguments":"{\"cmd\":\"ls\"}"}`, i, j))
						out, _ := json.Marshal(strings.Repeat("line of 🙂 output\n", resultRepeats))
						line(fmt.Sprintf(`{"type":"function_call_output","call_id":"c%d_%d","output":%s}`, i, j, out))
					}
					line(codexMessage("assistant", "done"))
				}
			})
		},
	}
	for name, build := range shapes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixtureFrom(t, build)
			for _, tc := range []struct {
				args  []string
				limit int
			}{
				{[]string{"--transcript"}, 120000},
				{[]string{"--transcript", "--full"}, 120000},
				{[]string{"--transcript", "--json"}, 120000},
				{[]string{"--transcript", "--max-bytes", "5000"}, 5000},
				{[]string{"--transcript", "--full", "--max-bytes", "5000"}, 5000},
				{[]string{"--transcript", "--json", "--max-bytes", "8000"}, 8000},
			} {
				out, errOut, code := runShow(t, f.env, append([]string{f.id}, tc.args...)...)
				if code != 0 || strings.Contains(errOut, "warning") {
					t.Errorf("%v: code=%d stderr=%q", tc.args, code, firstN(errOut, 300))
				}
				if len(out) > tc.limit {
					t.Errorf("%v: printed %d bytes, over %d", tc.args, len(out), tc.limit)
				}
				if !utf8.ValidString(out) {
					t.Errorf("%v: output is not valid UTF-8", tc.args)
				}
				if slices.Contains(tc.args, "--json") {
					dec := json.NewDecoder(strings.NewReader(out))
					for docs := 0; dec.More(); docs++ {
						var v any
						if err := dec.Decode(&v); err != nil {
							t.Errorf("%v: document %d is not valid JSON: %v", tc.args, docs, err)
							break
						}
					}
				}
			}
			// A limit no output can meet still prints valid text, exits 0, and
			// says so on stderr.
			for _, args := range [][]string{{"--transcript", "--max-bytes", "1"}, {"--transcript", "--full", "--max-bytes", "300"}, {"--transcript", "--json", "--max-bytes", "1"}} {
				out, errOut, code := runShow(t, f.env, append([]string{f.id}, args...)...)
				limit := mustAtoi(t, args[len(args)-1])
				warned := strings.Contains(errOut, "over the "+args[len(args)-1]+"-byte limit")
				if code != 0 || !utf8.ValidString(out) || (len(out) > limit && !warned) {
					t.Errorf("%v: code=%d, %d bytes, stderr=%q", args, code, len(out), errOut)
				}
			}
		})
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
