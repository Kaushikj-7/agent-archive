package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

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
