package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/wangjohn/agent-archive/internal/testutil/musefixture"
)

// An unsupported native root ahead of a supported one is refused on its own
// row; it does not stop the preview from locating the sources after it.
func TestBackfillNativeImportUnsupportedSourceDoesNotStarveLaterSources(t *testing.T) {
	t.Parallel()
	f := newPublicNativeImportFixture(t)
	raw, err := os.ReadFile(filepath.Join("..", "agents", "muse", "testdata", "synthetic-exec-conversation.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// A permission profile without a minimal runtime set is not a supported
	// startup shape, so this root's startup validation fails.
	unsupported := bytes.Replace(raw, []byte(`minimal_runtime_set_version\":1,`), []byte(`minimal_runtime_set_version\":null,`), 1)
	if bytes.Equal(unsupported, raw) {
		t.Fatal("fixture no longer carries a minimal runtime set version")
	}
	const id = "00000000-0000-7000-8000-000000000000" // sorts before publicNativeImportID
	path := filepath.Join(f.native, "2026", "10", "15", id, "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, musefixture.Rewrite(t, unsupported, id, f.project), 0o600); err != nil {
		t.Fatal(err)
	}
	out, stderr, code := f.run(nil, "--harness", "muse", "--dry-run", "--json", "--project", f.project)
	var result struct {
		Sessions []nativeImportRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &result); code != 0 || err != nil || len(result.Sessions) != 2 {
		t.Fatal(code, err, out, stderr)
	}
	if result.Sessions[0].Status != nativeImportRefused {
		t.Fatalf("unsupported root = %s %q, want refused", result.Sessions[0].Status, result.Sessions[0].Reason)
	}
	if result.Sessions[1].Status != nativeImportSelected {
		t.Fatalf("supported root after an unsupported one = %s %q, want selected", result.Sessions[1].Status, result.Sessions[1].Reason)
	}
}
