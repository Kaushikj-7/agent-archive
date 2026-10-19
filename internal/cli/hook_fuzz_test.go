package cli

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/config"
)

// hookFuzzSeeds are hook payloads shaped like each app's, one invocation per
// line; $P stands for the project folder and $T for a transcript in it, so
// the fuzzer starts from payloads the hook admits and mutates from there.
var hookFuzzSeeds = []struct {
	harness string
	lines   []string
}{
	{"claude", []string{
		`{"hook_event_name":"SessionStart","session_id":"c1","source":"startup","cwd":"$P","transcript_path":"$T"}`,
		`{"hook_event_name":"UserPromptSubmit","session_id":"c1","cwd":"$P","transcript_path":"$T","prompt":"hi"}`,
		`{"hook_event_name":"SubagentStop","session_id":"c1","cwd":"$P","agent_id":"a1","agent_transcript_path":"$P/agent-a1.jsonl"}`,
		`{"hook_event_name":"Stop","session_id":"c1","cwd":"$P","transcript_path":"$T","last_assistant_message":"done"}`,
		`{"hook_event_name":"SessionEnd","session_id":"c1","cwd":"$P","transcript_path":"$T","reason":"exit"}`,
	}},
	{"claude-code", []string{
		`{"hook_event_name":"SessionStart","session_id":"c2","source":"resume","cwd":"$P","transcript_path":"$T"}`,
	}},
	{"codex", []string{
		`{"hook_event_name":"SessionStart","session_id":"x1","source":"startup","cwd":"$P","transcript_path":"$T","model":"m"}`,
		`{"hook_event_name":"UserPromptSubmit","session_id":"x1","cwd":"$P","turn_id":"t1"}`,
		`{"hook_event_name":"Stop","session_id":"x1","cwd":"$P","transcript_path":"$T"}`,
	}},
	{"cursor", []string{
		`{"hook_event_name":"beforeSubmitPrompt","conversation_id":"k1","session_id":"k1","workspace_roots":["$P"],"transcript_path":null,"cursor_version":"3.21.13"}`,
		`{"hook_event_name":"afterAgentResponse","conversation_id":"k1","workspace_roots":["$P"],"transcript_path":"$T","text":"ok"}`,
		`{"hook_event_name":"subagentStop","conversation_id":"k1","workspace_roots":["$P"],"subagent_transcript_path":"$P/sub.jsonl"}`,
		`{"hook_event_name":"stop","conversation_id":"k1","workspace_roots":["$P"],"transcript_path":"$T","status":"completed"}`,
	}},
	{"muse", []string{`{"hook_event_name":"SessionStart","session_id":"fuzz-session","cwd":"$P","transcript_path":null,"source":"startup"}`, `{"hook_event_name":"Stop","session_id":"fuzz-session","cwd":"$P","transcript_path":null}`}},
	{"unknown", []string{`{"hook_event_name":"Stop","session_id":"u"}`}},
	{"codex", []string{``, `null`, `[]`, `"text"`, `{"hook_event_name":7}`, `{"hook_event_name":"Stop","session_id":{"a":1},"cwd":["$P"]}`}},
}

// FuzzHookPayload feeds arbitrary input to the hidden _hook command of each
// app, as a hook would: one or more invocations, one per line of the input.
// Whatever it is sent, a hook must exit 0 (a nonzero exit blocks the user's
// turn in Claude Code), must not panic (recovered, it still exits 0, but it
// says so on stderr), and must write nowhere but its data directory: the
// project folder and HOME it is pointed at stay exactly as they were. Each
// input starts from a fresh data directory with all three apps set up for
// the project, so an input can walk a session from its start to its stop.
func FuzzHookPayload(f *testing.F) {
	for _, seed := range hookFuzzSeeds {
		f.Add(seed.harness, strings.Join(seed.lines, "\n"))
	}
	sandbox := f.TempDir()
	project := filepath.Join(sandbox, "project")
	userHome := filepath.Join(sandbox, "home")
	for _, dir := range []string{project, userHome} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			f.Fatal(err)
		}
	}
	transcript := filepath.Join(project, "transcript.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"turn_context","model":"synthetic"}`+"\n"), 0o600); err != nil {
		f.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "agent-a1.jsonl"), nil, 0o600); err != nil {
		f.Fatal(err)
	}
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	cfg := pairTestConfig(now, []string{"claude", "codex", "cursor"}, project)
	before := hookSandboxSnapshot(f, sandbox)

	f.Fuzz(func(t *testing.T, harness, input string) {
		home := filepath.Join(t.TempDir(), "data")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := config.Save(home, cfg); err != nil {
			t.Fatal(err)
		}
		env := testEnv(t, home, now)
		env.UserHomeDir = func() (string, error) { return userHome, nil }
		if harness == "muse" {
			env.LookupEnv = func(key string) (string, bool) {
				_, present := museTestIdentityEnvironment(key)
				return "fuzz-session", present
			}
		}
		input = strings.NewReplacer("$T", transcript, "$P", project).Replace(input)
		for i, line := range strings.Split(input, "\n") {
			if i == 8 {
				break
			}
			var stderr bytes.Buffer
			if code := runHookCommand([]string{"--harness", harness}, strings.NewReader(line), &stderr, env); code != 0 {
				t.Fatalf("hook exited %d for %q", code, line)
			}
			if strings.Contains(stderr.String(), "internal error") {
				t.Fatalf("hook panicked for %q: %s", line, &stderr)
			}
		}
		if after := hookSandboxSnapshot(t, sandbox); after != before {
			t.Fatalf("the hook wrote outside its data directory:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})
}

// hookSandboxSnapshot lists every entry under root with its type, size, and
// modification time.
func hookSandboxSnapshot(tb testing.TB, root string) string {
	tb.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %s %d %d\n", path, info.Mode(), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return b.String()
}
