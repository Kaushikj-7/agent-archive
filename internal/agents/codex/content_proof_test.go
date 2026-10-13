package codex

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
)

func TestOrdinaryContentProofMatchesInputAndRejectsRestoredStampRewrite(t *testing.T) {
	dir := t.TempDir()
	ref, raw := historyFile(t, dir, threadA, threadA, 0, nil, "first")
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	}()
	content, ok := snapshot.(agentapi.VerifiedContentSnapshot)
	if !ok {
		t.Fatalf("ordinary proof missing: %T", snapshot)
	}
	proof, err := content.ContentProof(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	input := snapshot.Input().File
	actual, err := io.ReadAll(io.NewSectionReader(input, 0, input.Length()))
	if err != nil {
		t.Fatal(err)
	}
	if proof.Boundary != int64(len(actual)) || proof.SHA256 != sha256.Sum256(actual) || !bytes.Equal(actual, raw) {
		t.Fatalf("proof differs from filtered boundary: %+v", proof)
	}
	info, err := os.Stat(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := bytes.Replace(raw, []byte("first"), []byte("other"), 1)
	if err := os.WriteFile(ref.Path, rewritten, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(ref.Path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := content.VerifyContent(t.Context()); err == nil {
		t.Fatal("same-size restored-time rewrite accepted")
	}
	if _, err := content.ContentProof(t.Context()); err == nil {
		t.Fatal("stale proof returned")
	}
}

func TestLegacySnapshotHidesContentProof(t *testing.T) {
	root := t.TempDir()
	ref := legacyTestFile(t, root, threadA, map[string]any{})
	ref.Key = threadA
	lookup := &historyLookup{thread: agentapi.CodexRolloutSet{Revision: "one"}}
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{CodexRollouts: lookup, LegacyUnboundRegistration: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, ok := snapshot.(agentapi.VerifiedContentSnapshot); ok {
		t.Fatal("legacy wrapper exposes content proof")
	}
}

func TestOrdinaryContentProofUsesCapturedCompleteBoundary(t *testing.T) {
	dir := t.TempDir()
	ref, raw := historyFile(t, dir, threadA, threadA, 0, nil, "complete")
	raw = append(raw, []byte(`{"type":`)...)
	if err := os.WriteFile(ref.Path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	}()
	proof, err := snapshot.(agentapi.VerifiedContentSnapshot).ContentProof(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	input := snapshot.Input().File
	prefix, err := io.ReadAll(io.NewSectionReader(input, 0, input.Length()))
	if err != nil {
		t.Fatal(err)
	}
	if proof.Boundary != input.Length() || proof.Boundary >= int64(len(raw)) || proof.SHA256 != sha256.Sum256(prefix) {
		t.Fatalf("proof used whole file instead of captured prefix: %+v len=%d input=%d", proof, len(raw), input.Length())
	}
}
