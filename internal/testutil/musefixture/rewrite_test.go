package musefixture

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRewritePreservesExistingSyntheticFixtureBytes(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"synthetic-exec-empty.jsonl", "synthetic-exec-conversation.jsonl", "synthetic-msp-conversation.jsonl", "synthetic-tui-conversation.jsonl", "synthetic-own-tools.jsonl", "synthetic-committed-read-effects.jsonl", "synthetic-own-observations.jsonl", "synthetic-model-resolution-omissions.jsonl"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(filepath.Join("..", "..", "agents", "muse", "testdata", name))
			if err != nil {
				t.Fatal(err)
			}
			if got := Rewrite(t, raw, syntheticID, "/work/synthetic-muse"); !bytes.Equal(got, raw) {
				t.Fatal("identity rewrite changed the existing fixture bytes")
			}
		})
	}
}
