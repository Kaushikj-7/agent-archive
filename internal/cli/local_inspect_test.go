package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/wangjohn/agent-archive/internal/archive"
)

func hostileLocalNative(t *testing.T) (Env, string) {
	t.Helper()
	env, data, base, _ := museNativeEnv(t)
	workspace := filepath.Join(base, "checkout"+string([]rune{0x202e, 0x009b}))
	err := os.Mkdir(workspace, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	env.WorkingDir = func() (string, error) { return workspace, nil }
	prompt := "Prompt " + strings.Repeat("\u202e\u009b", 64) + " visible"
	raw := museNativeBytes(t, "exec", museSyntheticID, workspace)
	encoded, err := json.Marshal(prompt)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"Explain the synthetic parser."`), encoded, 1)
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	var named map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &named); err != nil {
		t.Fatal(err)
	}
	named["payload_type"] = "session.name.changed"
	named["payload"] = map[string]any{"session_id": museSyntheticID, "new_name": "Name\u202e\u009b visible", "previous_name": nil, "source": "automatic", "operation_id": "00000000-0000-4000-8000-000000000003"}
	lines[len(lines)-1], err = json.Marshal(named)
	if err != nil {
		t.Fatal(err)
	}
	writeMuseNative(t, data, museSyntheticID, append(bytes.Join(lines, []byte("\n")), '\n'))
	return env, workspace
}

func TestLocalReadJSONEscapesNativeDisplayControls(t *testing.T) {
	t.Parallel()
	env, workspace := hostileLocalNative(t)
	for _, args := range [][]string{{"list", "--source", "local", "--harness", "muse", "--json"}, {"show", museSyntheticID, "--source", "local", "--harness", "muse", "--json", "--transcript", "--max-bytes", "0"}} {
		t.Run(args[0], func(t *testing.T) {
			t.Parallel()
			var out, problems bytes.Buffer
			if code := Run(args, nil, &out, &problems, env); code != 0 {
				t.Fatalf("code %d: %s", code, problems.String())
			}
			if strings.ContainsAny(out.String(), "\u202e\u009b") || !strings.Contains(out.String(), `\u202e`) || !strings.Contains(out.String(), `\u009b`) {
				t.Fatal("native JSON contains literal display controls instead of escapes")
			}
			var document map[string]any
			if err := json.Unmarshal(out.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
			var session map[string]any
			if args[0] == "list" {
				session = document["sessions"].([]any)[0].(map[string]any)
			} else {
				session = document["session"].(map[string]any)
				turns := document["view"].(map[string]any)["turns"].([]any)
				if !strings.Contains(turns[0].(map[string]any)["text"].(string), "\u202e\u009b") {
					t.Fatal("display escaping changed native prompt content")
				}
			}
			if session["project"] != workspace || !strings.Contains(session["name"].(string), "visible") {
				t.Fatal("native path or generated name was lost")
			}
		})
	}
}

func TestLocalReadJSONBudgetCountsEscapedOutput(t *testing.T) {
	t.Parallel()
	env, _ := hostileLocalNative(t)
	full, problems, code := runShow(t, env, museSyntheticID, "--source", "local", "--harness", "muse", "--json", "--transcript", "--max-bytes", "0")
	if code != 0 {
		t.Fatalf("code %d: %s", code, problems)
	}
	var document any
	if err := json.Unmarshal([]byte(full), &document); err != nil {
		t.Fatal(err)
	}
	unescaped, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	escaped := append(archive.DisplayJSON(unescaped), '\n')
	budget := len(unescaped) + 1
	if len(escaped) <= budget {
		t.Fatal("fixture does not expand at the display boundary")
	}
	out, problems, code := runShow(t, env, museSyntheticID, "--source", "local", "--harness", "muse", "--json", "--transcript", "--max-bytes", strconv.Itoa(budget))
	if code != 1 || out != "" || !strings.Contains(problems, "exceeds --max-bytes") {
		t.Fatalf("escaped output exceeded its budget: code %d bytes %d: %s", code, len(out), problems)
	}
}

type localShortWriter struct{ calls int }

func (w *localShortWriter) Write(data []byte) (int, error) {
	w.calls++
	return len(data) / 2, nil
}

func TestLocalReadJSONRejectsShortSuccessfulWrites(t *testing.T) {
	t.Parallel()
	env, data, workspace, _ := museNativeEnv(t)
	writeMuseNative(t, data, museSyntheticID, museNativeBytes(t, "exec", museSyntheticID, workspace))
	for _, args := range [][]string{{"list", "--source", "local", "--harness", "muse", "--json"}, {"show", museSyntheticID, "--source", "local", "--harness", "muse", "--json"}} {
		t.Run(args[0], func(t *testing.T) {
			t.Parallel()
			var problems bytes.Buffer
			out := &localShortWriter{}
			if code := Run(args, nil, out, &problems, env); code != 1 || out.calls != 1 || !strings.Contains(problems.String(), io.ErrShortWrite.Error()) {
				t.Fatalf("short successful write: code %d calls %d: %s", code, out.calls, problems.String())
			}
		})
	}
}

func TestMuseLocalListAndShowBeforeSetupWriteNoArchiveState(t *testing.T) {
	t.Parallel()
	env, data, workspace, counter := museNativeEnv(t)
	archiveHome := museNativeArchiveHome(t, env)
	env.Home = func() (string, error) {
		t.Fatal("local read resolved archive state home")
		return "", errors.New("forbidden")
	}
	path := writeMuseNative(t, data, museSyntheticID, museNativeBytes(t, "exec", museSyntheticID, workspace))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{"list", "--source", "local", "--harness", "muse", "--json"}, nil, &out, &errOut, env)
	if code != 0 || !strings.Contains(out.String(), museSyntheticID) || counter.calls.Load() != 0 {
		t.Fatalf("list code %d: %s %s", code, out.String(), errOut.String())
	}
	if !json.Valid(out.Bytes()) {
		t.Fatal("invalid local list JSON")
	}
	shown, problems, code := runShow(t, env, museSyntheticID, "--source", "local", "--harness", "muse", "--transcript", "--json")
	if code != 0 || !json.Valid([]byte(shown)) || !strings.Contains(shown, `"view"`) || !strings.Contains(shown, `"child_coverage_unavailable"`) || counter.calls.Load() != 1 {
		t.Fatalf("show code %d filters %d: %s %s", code, counter.calls.Load(), shown, problems)
	}
	if _, err := os.Stat(archiveHome); !os.IsNotExist(err) {
		t.Fatalf("local reads created archive state: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("local reads changed native source")
	}
}

func TestLocalReadJSONMatchesPublishedSchema(t *testing.T) {
	t.Parallel()
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	var schemaID string
	for _, name := range []string{"metadata.schema.json", "local-read.schema.json"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", name))
		if err != nil {
			t.Fatal(err)
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		id := document.(map[string]any)["$id"].(string)
		if err := compiler.AddResource(id, document); err != nil {
			t.Fatal(err)
		}
		if name == "local-read.schema.json" {
			schemaID = id
		}
	}
	schema, err := compiler.Compile(schemaID)
	if err != nil {
		t.Fatal(err)
	}
	env, data, workspace, _ := museNativeEnv(t)
	writeMuseNative(t, data, museSyntheticID, museNativeFixtureBytes(t, "synthetic-own-observations.jsonl", museSyntheticID, workspace))
	for _, args := range [][]string{{"list", "--source", "local", "--harness", "muse", "--json"}, {"show", museSyntheticID, "--source", "local", "--harness", "muse", "--json"}, {"show", museSyntheticID, "--source", "local", "--harness", "muse", "--json", "--transcript"}} {
		var out, problems bytes.Buffer
		if code := Run(args, nil, &out, &problems, env); code != 0 {
			t.Fatalf("%v: %d %s", args, code, problems.String())
		}
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
		document := value.(map[string]any)
		document["archive_session_id"] = "must-not-be-local-identity"
		if err := schema.Validate(document); err == nil {
			t.Fatal("local schema accepted unknown archive identity field")
		}
		delete(document, "archive_session_id")
		document["schema_version"] = 2
		if err := schema.Validate(document); err == nil {
			t.Fatal("local schema accepted unrecognized version")
		}
	}
}

func TestMuseLocalShowRefusesConflictingCopies(t *testing.T) {
	t.Parallel()
	env, data, workspace, _ := museNativeEnv(t)
	raw := museNativeBytes(t, "exec", museSyntheticID, workspace)
	writeMuseNative(t, data, museSyntheticID, raw)
	copyPath := filepath.Join(data, "muse", "sessions", "2000", "01", "02", museSyntheticID, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(copyPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	out, problems, code := runShow(t, env, museSyntheticID, "--source", "local", "--harness", "muse", "--json")
	if code == 0 || out != "" || !strings.Contains(problems, "ambiguous") {
		t.Fatalf("selected conflicting copy: %d %s %s", code, out, problems)
	}
}

func TestMuseLocalShowOutputBudgetCreatesNoFullTranscriptArtifact(t *testing.T) {
	t.Parallel()
	env, data, workspace, _ := museNativeEnv(t)
	archiveHome := museNativeArchiveHome(t, env)
	writeMuseNative(t, data, museSyntheticID, museNativeBytes(t, "exec", museSyntheticID, workspace))
	for _, jsonOut := range []bool{false, true} {
		args := []string{museSyntheticID, "--source", "local", "--harness", "muse", "--transcript", "--max-bytes", "1"}
		if jsonOut {
			args = append(args, "--json")
		}
		out, problems, code := runShow(t, env, args...)
		if code == 0 || out != "" || !strings.Contains(problems, "exceeds --max-bytes") {
			t.Fatalf("output budget: %d %s %s", code, out, problems)
		}
	}
	if _, err := os.Stat(archiveHome); !os.IsNotExist(err) {
		t.Fatalf("local output budget wrote state/artifact: %v", err)
	}
}

func TestMuseLocalShowValidatesEntireSourceAndScope(t *testing.T) {
	t.Parallel()
	env, data, workspace, _ := museNativeEnv(t)
	raw := append(museNativeBytes(t, "exec", museSyntheticID, workspace), []byte("invalid suffix\n")...)
	writeMuseNative(t, data, museSyntheticID, raw)
	out, _, code := runShow(t, env, museSyntheticID, "--source", "local", "--harness", "muse", "--json")
	if code == 0 || out != "" {
		t.Fatal("selected show accepted invalid native suffix")
	}
	_, _, code = runShow(t, env, museSyntheticID, "--source", "local", "--harness", "muse", "--project", "/nonexistent/synthetic-checkout")
	if code == 0 {
		t.Fatal("invented missing checkout scope")
	}
}

func TestLocalReadRejectsUnsupportedArchiveFlagsWithoutStorage(t *testing.T) {
	t.Parallel()
	env, _, _, _ := museNativeEnv(t)
	for _, args := range [][]string{{"list", "--source", "local", "--complete"}, {"list", "--source", "local", "--rebuild-index"}, {"list", "--source", "local", "--limit", "-1"}, {"list", "--source", "local", "--harness", "unknown"}, {"show", "native", "--source", "local", "--revisions"}, {"show", "native", "--source", "local", "--revision", "one"}, {"show", "native", "--source", "local", "--project", "/synthetic", "--all-projects"}, {"show", "native", "--source", "local", "--transcript", "--full", "--json"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 2 || out.Len() != 0 {
			t.Fatalf("accepted unsupported arguments %v: %d %s", args, code, out.String())
		}
	}
}

func TestLocalShowLabelsConformToArchivedMetadataFromSameAnalysis(t *testing.T) {
	t.Parallel()
	env, data, workspace, _ := museNativeEnv(t)
	writeMuseNative(t, data, museSyntheticID, museNativeBytes(t, "exec", museSyntheticID, workspace))
	discovered, err := discoverLocalRead(t.Context(), env, localReadOptions{harness: "muse"})
	if err != nil || len(discovered.Candidates) != 1 {
		t.Fatalf("local discovery: %v %+v", err, discovered.Coverage)
	}
	target, err := handoffFromNative(t.Context(), discovered.Candidates[0], env.nativeFiles(), env)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := archive.BuildMetadataWithAnalysis(target.bundle, target.analysis, nil, "synthetic-machine", discovered.Candidates[0].StartedAt, env.Now(), archive.SourceReference{Key: "sessions/muse/synthetic/source.jsonl.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, archive.ParserInfo{Name: "muse", Version: "0.6.0"})
	if err != nil || metadata.Title == "" {
		t.Fatal("same Analysis has no archived title", err)
	}
	out, problems, code := runShow(t, env, museSyntheticID, "--source", "local", "--harness", "muse", "--json")
	if code != 0 {
		t.Fatalf("local show %d: %s", code, problems)
	}
	var document struct {
		Session localReadRow `json:"session"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatal(err)
	}
	if document.Session.Name != metadata.Name || document.Session.Title != metadata.Title {
		t.Fatalf("local labels %+v differ from archive name %q title %q", document.Session, metadata.Name, metadata.Title)
	}
	summary, problems, code := runShow(t, env, museSyntheticID, "--source", "local", "--harness", "muse")
	if code != 0 || !strings.Contains(summary, archive.DisplayTitle(metadata)) {
		t.Fatalf("local summary omits shared title: %d %s %s", code, summary, problems)
	}
	transcript, problems, code := runShow(t, env, museSyntheticID, "--source", "local", "--harness", "muse", "--transcript")
	if code != 0 || !strings.Contains(transcript, archive.DisplayTitle(metadata)) {
		t.Fatalf("local transcript omits shared title: %d %s %s", code, transcript, problems)
	}
	// A single bounded preview still cannot derive the human first-prompt title.
	var listOutput, listProblems bytes.Buffer
	code = Run([]string{"list", "--source", "local", "--harness", "muse", "--json"}, nil, &listOutput, &listProblems, env)
	listed, problems := listOutput.String(), listProblems.String()
	if code != 0 {
		t.Fatalf("local list %d: %s", code, problems)
	}
	var listing struct {
		Sessions []localReadRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(listed), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Sessions) != 1 || listing.Sessions[0].Title != "" {
		t.Fatal("bounded preview invented full source title")
	}
}
