package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// passCost is the work one pass did that a settled session must not cost:
// full decodes of published state (whole source bundles) and local writes,
// each of which is a pair of F_FULLFSYNCs on macOS.
type passCost struct {
	loads  int64
	writes int
	// SourceSet cache hits still read and hash the complete published wire.
	summaryBytes int64
}

// measurePass runs one pass and counts its decodes of published state and
// the files it wrote under the data directory. The decodes are counted for the
// whole process, so a test that checks them must not be parallel.
func measurePass(t *testing.T, local *state.Store, remote storage.ObjectStore, opts Options) (Result, passCost) {
	t.Helper()
	before := snapshotMtimes(t, local.Home())
	loads := state.PublishedStateLoads()
	summaryBytes := state.SummaryValidationBytes()
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil {
		t.Fatal(err)
	}
	cost := passCost{loads: state.PublishedStateLoads() - loads, summaryBytes: state.SummaryValidationBytes() - summaryBytes}
	for path, mtime := range snapshotMtimes(t, local.Home()) {
		// status.json is the pass's own summary, rewritten every pass.
		if was, ok := before[path]; (!ok || !was.Equal(mtime)) && filepath.Base(path) != "status.json" {
			cost.writes++
		}
	}
	return result, cost
}

// snapshotMtimes records the modification time of every file and directory
// under root but root itself (where status.json is written every pass). A
// directory's time moves when a file in it is created or removed, so a file
// written and removed again within the pass (a scan journal) counts too.
func snapshotMtimes(t *testing.T, root string) map[string]time.Time {
	t.Helper()
	out := map[string]time.Time{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && path != root {
			out[path] = info.ModTime()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Applications delete their own transcripts after 30 days while the archive
// keeps sessions for 90, so in a mature archive most sessions have lost their
// transcript. Each such session used to cost every pass a scan journal (two
// F_FULLFSYNCs), a decode of its whole published state, and a block that
// removed its signature so the next pass paid again: 200 of them took two
// seconds a pass, against 17 ms settled. A missing transcript is now a gap
// recorded at the transcript's absence and skipped on a stat while it lasts,
// and a returning transcript is read at once.
//
// Not parallel, like every test that reads measurePass's cost: the count of
// published-state loads is process-wide.
func TestMissingTranscriptsCostNothingPerPass(t *testing.T) {
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	dir := t.TempDir()
	const sessions = 20
	for i := range sessions {
		path := writeTranscript(t, dir, fmt.Sprintf("s%02d.jsonl", i), codexTranscript)
		reg := registration(t, path)
		reg.ArchiveSessionID, reg.NativeSessionID = fmt.Sprintf("s%02d", i), fmt.Sprintf("n%02d", i)
		if err := local.SaveRegistration(reg); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at }}
	if r, _ := measurePass(t, local, remote, opts); len(r.Published) != sessions {
		t.Fatalf("%#v", r)
	}
	for i := range sessions {
		if err := os.Remove(filepath.Join(dir, fmt.Sprintf("s%02d.jsonl", i))); err != nil {
			t.Fatal(err)
		}
	}
	at = at.Add(time.Hour)
	if r, _ := measurePass(t, local, remote, opts); len(r.Errors) != 0 {
		t.Fatalf("recording the gaps: %#v", r.Errors)
	}
	for pass := range 2 {
		at = at.Add(time.Hour)
		r, cost := measurePass(t, local, remote, opts)
		if len(r.Errors) != 0 || len(r.Skipped) != sessions {
			t.Fatalf("pass %d: %#v", pass, r)
		}
		if cost.loads != 0 || cost.writes != 0 {
			t.Fatalf("pass %d over %d sessions whose transcripts are gone: %d published-state decodes, %d writes; want none", pass, sessions, cost.loads, cost.writes)
		}
	}
	// One comes back: read at once, and its gap cleared.
	writeTranscript(t, dir, "s00.jsonl", codexTranscript)
	at = at.Add(time.Hour)
	if r, _ := measurePass(t, local, remote, opts); len(r.Errors) != 0 {
		t.Fatalf("%#v", r.Errors)
	}
	if _, blocked, _ := local.LoadBlocked("s00"); blocked {
		t.Fatal("a returning transcript was skipped on the stale gap")
	}
}

// Subagents were never skipped, and each one's scan decoded its parent's
// whole bundle to look for its link: 400 unchanged subagents took 25 seconds
// a pass. A subagent is now skipped on a stat like any session once its
// parent links it, the check reading only summaries (the parent's once a
// pass). Not parallel: measurePass's count of loads is process-wide.
func TestSettledSubagentsCostNothingPerPass(t *testing.T) {
	home := t.TempDir()
	local, err := openTestStore(home)
	if err != nil {
		t.Fatal(err)
	}
	parentStart := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	parentPath := writeTranscript(t, home, "parent.jsonl", `{"type":"assistant","sessionId":"parent-native","timestamp":"2026-09-21T10:01:00Z","message":{"role":"assistant","content":"plan"}}`+"\n")
	parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: parentPath, SessionStartedAt: parentStart, RegisteredAt: parentStart}
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	stopAt := parentStart.Add(3 * time.Minute)
	const children = 10
	for i := range children {
		agent := fmt.Sprintf("agent-%d", i)
		line := fmt.Sprintf(`{"type":"assistant","sessionId":"parent-native","agentId":%q,"timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"found it"}}`, agent)
		path := writeTranscript(t, home, agent+".jsonl", line+"\n")
		if err := local.SaveSubagentCandidate(state.SubagentCandidate{ArchiveSessionID: "child-" + agent, NativeSessionID: "parent-native:subagent:" + agent, ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, AgentID: agent, TranscriptPath: path, ObservedAt: stopAt}); err != nil {
			t.Fatal(err)
		}
	}
	remote := storagetest.NewMemoryStore()
	now := stopAt.Add(time.Minute)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return now }}
	// Settle: children publish, the parent picks up their links.
	for range 4 {
		now = now.Add(10 * time.Minute)
		if r, _ := measurePass(t, local, remote, opts); len(r.Errors) != 0 {
			t.Fatalf("%v", r.Errors)
		}
	}
	summary, _, err := local.LoadPublishedSummary("parent")
	if err != nil || len(summary.LinkedPublished) != children {
		t.Fatalf("parent links %d of %d children (%v)", len(summary.LinkedPublished), children, err)
	}
	// Exact in-memory projection reuse removes repeated bundle decoding, but
	// validating these authority-bearing summaries still reads every wire byte.
	var publishedBytes int64
	for _, id := range append([]string{"parent"}, summary.LinkedPublished...) {
		info, err := os.Stat(filepath.Join(local.Home(), "published", id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		publishedBytes += info.Size()
	}
	for pass := range 2 {
		now = now.Add(10 * time.Minute)
		r, cost := measurePass(t, local, remote, opts)
		if len(r.Errors) != 0 || len(r.Skipped) != children+1 {
			t.Fatalf("pass %d: %#v", pass, r)
		}
		if cost.loads != 0 || cost.writes != 0 {
			t.Fatalf("pass %d over %d settled subagents: %d published-state decodes, %d writes; want none", pass, children, cost.loads, cost.writes)
		}
		if cost.summaryBytes < publishedBytes {
			t.Fatalf("pass %d validated %d wire bytes, less than the %d-byte settled set", pass, cost.summaryBytes, publishedBytes)
		}
		t.Logf("pass %d: %d complete-wire bytes hashed; %d source-state decodes, %d writes", pass, cost.summaryBytes, cost.loads, cost.writes)
	}
}

// A size-limit gap, and a rewritten transcript's gap, are skipped on a stat
// while the transcript sits untouched (they used to be read in full every
// pass to reach the same gap), are never signed as settled, and are read
// again as soon as it changes. Not parallel: measurePass's count of loads is
// process-wide.
func TestGapsAreSkippedOnlyWhileTheTranscriptIsUntouched(t *testing.T) {
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	reg := settledSession(t, local, codexTranscript)
	// The settled fixture published into its own temporary store. Seed this
	// test's store with that exact committed body and source before mutation.
	published, err := local.LoadPublishedState(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	bundle, _, found := published.LastPublished()
	if !found {
		t.Fatal("fixture has no publication")
	}
	compressed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	ref, found := published.LastPublishedSource()
	if !found || compressed.SHA256 != ref.SHA256 {
		t.Fatal("fixture source differs")
	}
	if err := remote.Put(t.Context(), ref.Key, compressed.Bytes); err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(t.Context(), key, published.Metadata()); err != nil {
		t.Fatal(err)
	}

	rewritten := strings.Replace(codexTranscript, "visible", "VISIBLE", 1)
	if err := os.WriteFile(reg.TranscriptPath, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := mtime(t, reg.TranscriptPath).Add(time.Second)
	if err := os.Chtimes(reg.TranscriptPath, moved, moved); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 2, 1, 0, 0, 0, time.UTC)
	opts := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at }}
	measurePass(t, local, remote, opts)
	if reason, blocked, _ := local.LoadBlocked(reg.ArchiveSessionID); !blocked || reason != state.BlockedReasonTranscriptRewritten {
		t.Fatalf("reason=%q blocked=%t", reason, blocked)
	}
	signature, found, _ := local.LoadScanSignature(reg.ArchiveSessionID)
	if !found || signature.Blocked != state.BlockedReasonTranscriptRewritten {
		t.Fatalf("signature = %#v (found %t), want one recording the gap", signature, found)
	}
	at = at.Add(time.Hour)
	if r, cost := measurePass(t, local, remote, opts); len(r.Skipped) != 1 || cost.loads != 0 || cost.writes != 0 {
		t.Fatalf("an untouched gap cost %d decodes and %d writes: %#v", cost.loads, cost.writes, r)
	}
	// Changed: read again. It extends what was published now, so it publishes.
	if err := os.WriteFile(reg.TranscriptPath, []byte(codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	at = at.Add(time.Hour)
	if r, _ := measurePass(t, local, remote, opts); len(r.Published) != 1 {
		t.Fatalf("the changed transcript was not read: %#v", r)
	}
}

// A size-limit gap stands only while the limits it was reached under do: a
// raised limit reads the transcript again, and status names the gap while
// it lasts. Not parallel: measurePass's count of loads is process-wide.
func TestSizeLimitGapIsSkippedUntilTheLimitChanges(t *testing.T) {
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	path := writeTranscript(t, t.TempDir(), "codex.jsonl", codexTranscript)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	small := Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at }, MaxTranscriptBytes: 8}
	measurePass(t, local, remote, small)
	if reason, blocked, _ := local.LoadBlocked(reg.ArchiveSessionID); !blocked || reason != state.BlockedReasonTranscriptTooLarge {
		t.Fatalf("reason=%q blocked=%t", reason, blocked)
	}
	at = at.Add(time.Hour)
	if r, cost := measurePass(t, local, remote, small); len(r.Skipped) != 1 || cost.loads != 0 || cost.writes != 0 {
		t.Fatalf("an untouched size gap cost %d decodes and %d writes: %#v", cost.loads, cost.writes, r)
	}
	if status, _ := local.LoadStatus(); !strings.Contains(status.LastError, "size limit") {
		t.Fatalf("status.LastError = %q, want the size-limit gap named", status.LastError)
	}
	if unchanged, _ := unchangedSinceLastScan(context.Background(), local, reg, Options{Sources: testSources, Parsers: testParsers, MachineID: "m"}); unchanged {
		t.Fatal("a raised limit left the gap skipped")
	}
	at = at.Add(time.Hour)
	if r, _ := measurePass(t, local, remote, Options{Sources: testSources, Parsers: testParsers, MachineID: "m", Now: func() time.Time { return at }}); len(r.Published) != 1 {
		t.Fatalf("the raised limit did not capture the session: %#v", r)
	}
	if status, _ := local.LoadStatus(); status.LastError != "" {
		t.Fatalf("status.LastError = %q after the gap ended", status.LastError)
	}
}
