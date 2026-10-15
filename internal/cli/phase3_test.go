package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestShowResolvesTitleSubstring(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mem.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m.Title = "Fix flaky OAuth callback tests"
	encoded, _ := json.Marshal(m)
	if err := mem.Put(t.Context(), key, encoded); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	// Multi-word titles are not valid SESSION_ID keys; they must still match.
	if code := Run([]string{"show", "OAuth callback"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), id) || !strings.Contains(out.String(), "Fix flaky OAuth") {
		t.Fatalf("show by title:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"show", "OAuth callback", "--harness", "codex"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("show by title with harness: code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), id) {
		t.Fatalf("show by title with harness returned wrong session:\n%s", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"show", id[:minShortSessionID], "--harness", "codex"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("show by short ID with harness: code=%d stderr=%s", code, errOut.String())
	}
}

func TestBareCommandBrowsesOnTTY(t *testing.T) {
	t.Parallel()
	env, _, id := publishedFixture(t)
	stdin := strings.NewReader("q\n")
	var out, errOut bytes.Buffer
	env.IsTerminal = func(stream any) bool {
		return stream == any(stdin) || stream == any(&out)
	}
	if code := Run(nil, stdin, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s out=%s", code, errOut.String(), out.String())
	}
	text := out.String()
	if !strings.Contains(text, "TITLE") || !strings.Contains(text, id[:8]) {
		t.Fatalf("expected interactive list:\n%s", text)
	}
	if strings.Contains(text, "Get started") {
		t.Fatalf("should not print usage on TTY browse:\n%s", text)
	}
}

func TestShowAmbiguousHarnessDoesNotFuzzyMatch(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	metadata, err := readSingleMetadata(t, mem)
	if err != nil {
		t.Fatal(err)
	}
	metadata.Harness.Name = "cursor"
	metadata.Title = id // title equals the session id so fuzzy would match if tried
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey("cursor", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Put(t.Context(), key, data); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"show", id}, nil, &out, &errOut, env); code != 1 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "more than one harness") {
		t.Fatalf("stderr=%s", errOut.String())
	}
}

func TestShowTitleDisambiguationHonorsNormalized(t *testing.T) {
	t.Parallel()
	env, mem, id := publishedFixture(t)
	key, err := archive.MetadataObjectKey("codex", id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := mem.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	var first archive.Metadata
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	first.Title = "Shared title alpha"
	encoded, _ := json.Marshal(first)
	if err := mem.Put(t.Context(), key, encoded); err != nil {
		t.Fatal(err)
	}
	second := first
	second.SessionID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	second.Title = "Shared title beta"
	secondKey, err := archive.MetadataObjectKey("codex", second.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(second)
	if err := mem.Put(t.Context(), secondKey, encoded); err != nil {
		t.Fatal(err)
	}
	// Also need a source bundle for --normalized on the chosen session.
	stdin := strings.NewReader(id[:8] + "\n")
	var out, errOut bytes.Buffer
	env.IsTerminal = func(stream any) bool {
		return stream == any(stdin) || stream == any(&out)
	}
	code := Run([]string{"show", "--normalized", "Shared title"}, stdin, &out, &errOut, env)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s out=%s", code, errOut.String(), out.String())
	}
	text := out.String()
	if !strings.Contains(text, id) {
		t.Fatalf("expected published session after pick:\n%s", text)
	}
	if strings.Count(text, "{") < 2 {
		t.Fatalf("expected metadata + normalized JSON after pick:\n%s", text)
	}
}
