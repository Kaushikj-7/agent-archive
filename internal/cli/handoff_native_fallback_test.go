package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/nativesessions"
)

func TestUnusedHeaderReservationsPermitInitialPreviews(t *testing.T) {
	f := newNativeFixture(t)
	padding := []byte(strings.Repeat("{}\n", 16) + strings.Repeat("x", int(nativeWindowBytes)))
	for i := range 257 {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
		path := f.add(t, "codex", id, "Known local conversation", 0)
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = file.Write(padding)
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("append: %v %v", err, closeErr)
		}
	}
	files := &nativeReadMeter{}
	r, err := nativesessions.Discover(context.Background(), f.env.agentRegistry(), files, f.env.nativeStoreRoots, nativesessions.Scope{Directories: []string{f.cwd}}, nativesessions.Limits{Files: 10000, HeaderBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes, TotalBytes: nativeReadBudget, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	catalog := &nativePreviewCatalog{sources: f.env.agentRegistry(), previews: f.env.agentRegistry(), ctx: context.Background(), files: files, candidates: r.Candidates, reserved: r.Coverage.ReservedBytes, stderr: io.Discard, now: f.at}
	more, err := catalog.load()
	read, _, _, _ := files.counts()
	t.Logf("verified=%d reserved=%d actualRead=%d rows=%d more=%v error=%v", len(r.Candidates), r.Coverage.ReservedBytes, read, len(catalog.rows), more, err)
	f.env.IsTerminal = func(any) bool { return true }
	var out, stderr bytes.Buffer
	code := Run([]string{"handoff", "--no-preamble"}, strings.NewReader("q\n"), &out, &stderr, f.env)
	t.Logf("interactive exit=%d output=%q stderr=%q", code, out.String(), stderr.String())
	if err != nil || len(r.Candidates) != 256 || len(catalog.rows) != 50 || catalog.next != 50 || !more || r.Coverage.ReservedBytes != r.Coverage.ReadBytes || read > nativeReadBudget {
		t.Fatalf("unused headers prevented bounded previews: coverage=%+v rows=%d next=%d more=%v read=%d err=%v", r.Coverage, len(catalog.rows), catalog.next, more, read, err)
	}
	if code != 0 || !strings.Contains(stderr.String(), "labels inspected for 50 of 256") {
		t.Fatalf("picker did not expose initial previews: code=%d stderr=%s", code, stderr.String())
	}
}

func TestIncompleteLatestOffersKnownCandidates(t *testing.T) {
	f := newNativeFixture(t)
	f.add(t, "claude", "native-known", "Known local conversation", 0)
	bad := f.add(t, "claude", "native-bad", "Malformed source", 0)
	if err := os.WriteFile(bad, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runHandoff(t, f.env, "--latest")
	t.Logf("exit=%d stderr=%q", code, stderr)
	if code != 1 || !strings.Contains(stderr, "complete local") {
		t.Fatal("fixture did not produce incomplete-latest refusal")
	}
	if !strings.Contains(stderr, "agent-archive handoff native-known --harness claude --source local") {
		t.Error("incomplete latest never offers a copyable qualified recipe")
	}
}

func TestIncompleteLatestWithDestinationRequiresExplicitPicker(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	f.add(t, "claude", "native-current", "Current work", 0)
	f.add(t, "claude", "native-other", "Other work", 0)
	bad := f.add(t, "claude", "native-bad", "Malformed", 0)
	if err := os.WriteFile(bad, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.env.LookupEnv = agentEnv(map[string]string{"CLAUDE_CODE_SESSION_ID": "native-current"})
	f.env.IsTerminal = func(any) bool { return true }
	var out, stderr bytes.Buffer
	input := strings.NewReader("2\n")
	target, picked, code := resolveNativeHandoff(handoffOptions{latest: true, to: "codex"}, true, newTypedInput(input), &out, &stderr, f.env)
	if code != 0 || !picked || target.native == nil || target.native.NativeID != "native-other" || strings.Contains(target.describe, "newest") || !strings.Contains(stderr.String(), "select a known native ID explicitly") {
		t.Fatalf("latest fallback picked automatically: code=%d picked=%v target=%+v stderr=%s", code, picked, target, stderr.String())
	}
	f.env.IsTerminal = func(any) bool { return false }
	out.Reset()
	stderr.Reset()
	_, picked, code = resolveNativeHandoff(handoffOptions{latest: true, to: "codex"}, false, newTypedInput(strings.NewReader("")), &out, &stderr, f.env)
	if code != 1 || picked || out.Len() != 0 || !strings.Contains(stderr.String(), "agent-archive handoff native-current --harness claude --source local") {
		t.Fatalf("noninteractive latest fell through to current: code=%d picked=%v stderr=%s", code, picked, stderr.String())
	}
}

func TestFullHeaderBudgetKeepsVerifiedIDsWithoutPreviewReads(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	padding := strings.Repeat("x", int(nativeWindowBytes))
	for i := range 257 {
		path := f.add(t, "claude", fmt.Sprintf("native-%04d", i), "Filtered conversation", 0)
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := file.WriteString(padding)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("append: %v %v", writeErr, closeErr)
		}
	}
	meter := &nativeReadMeter{}
	r, err := nativesessions.Discover(context.Background(), f.env.agentRegistry(), meter, f.env.nativeStoreRoots, nativesessions.Scope{Directories: []string{f.cwd}}, nativesessions.Limits{Files: 10000, HeaderBytes: nativeWindowBytes, RecordBytes: nativeWindowBytes, TotalBytes: nativeReadBudget, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	before, opensBefore, _, _ := meter.counts()
	var stderr bytes.Buffer
	n := &nativePreviewCatalog{sources: f.env.agentRegistry(), previews: f.env.agentRegistry(), ctx: context.Background(), files: meter, candidates: r.Candidates, reserved: r.Coverage.ReservedBytes, stderr: &stderr, now: f.at}
	more, err := n.load()
	after, opensAfter, active, full := meter.counts()
	if err != nil || more || len(n.rows) != 256 || n.next != 0 || !n.exhausted || before != nativeReadBudget || after != before || opensBefore != opensAfter || active != 0 || full != 0 || !strings.Contains(stderr.String(), "labels inspected for 0 of 256") {
		t.Fatalf("lost bounded identities or read beyond budget: coverage=%+v rows=%d before=%d after=%d err=%v", r.Coverage, len(n.rows), before, after, err)
	}
	for i, row := range n.rows {
		if row.Index != i+1 || nativeRowIndex(row, r.Candidates) != i || row.Title != r.Candidates[i].NativeID || row.Hint != "label not inspected" {
			t.Fatalf("unselectable or misleading row: %+v", row)
		}
	}
}

func TestIncompleteLatestRecipesKeepCandidateCheckout(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t)
	f.cwd = t.TempDir() + "/checkout 'quoted'"
	if err := os.Mkdir(f.cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	checkoutLink := filepath.Join(t.TempDir(), "checkout-link")
	if err := os.Symlink(f.cwd, checkoutLink); err != nil {
		t.Fatal(err)
	}
	f.cwd = checkoutLink
	canonical, err := filepath.EvalSymlinks(f.cwd)
	if err != nil {
		t.Fatal(err)
	}
	f.add(t, "claude", "native-known", "Other checkout work", 0)
	bad := f.add(t, "claude", "native-bad", "Malformed", 0)
	if err := os.WriteFile(bad, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	f.env.WorkingDir = func() (string, error) { return elsewhere, nil }
	_, stderr, code := runHandoff(t, f.env, "--latest", "--project", f.cwd)
	if code != 1 || !strings.Contains(stderr, "--source local --project "+shellQuote(canonical)) {
		t.Fatalf("recipe lost candidate checkout: code=%d stderr=%s", code, stderr)
	}
	out, stderr, code := runHandoff(t, f.env, "native-known", "--harness", "claude", "--source", "local", "--project", canonical)
	if code != 0 || !strings.Contains(out, "Other checkout work") {
		t.Fatalf("recipe cannot resolve candidate: code=%d stderr=%s", code, stderr)
	}
}
