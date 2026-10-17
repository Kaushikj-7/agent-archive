package collector

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/state"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// cursorTextTranscript is a synthetic plain-text Cursor transcript with the
// given number of user/assistant exchanges.
func cursorTextTranscript(exchanges, bodyBytes int) string {
	var b strings.Builder
	for i := range exchanges {
		fmt.Fprintf(&b, "user: request %d\nassistant: answer %d %s\n", i, i, strings.Repeat("a", bodyBytes))
	}
	return b.String()
}

func cursorTextSession(t *testing.T, local *state.Store, content string) string {
	t.Helper()
	path := writeTranscript(t, t.TempDir(), "cursor.txt", content)
	reg := registration(t, path)
	reg.Harness.Name = "cursor"
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	return path
}

// A text transcript over the record limit used to be refused by the adapter
// on every pass. It is now the record_size_limit gap: recorded once, never an
// error, and cleared when the transcript changes. Not parallel: it lowers
// recordLimit.
func TestCursorTextOverTheLimitBlocksOnceAndClearsOnChange(t *testing.T) {
	withCollectorRecordLimit(t, 4096)
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	path := cursorTextSession(t, local, cursorTextTranscript(40, 200))
	at := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	result := runAt(t, local, remote, at)
	if len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("an oversize text transcript must be a gap, not an error: %#v", result)
	}
	if reason, blocked, _ := local.LoadBlocked("session-1"); !blocked || reason != state.BlockedReasonRecordTooLarge {
		t.Fatalf("reason=%q blocked=%t", reason, blocked)
	}
	cache := publishedPath(local, "session-1")
	before := mtime(t, cache)
	if result := runAt(t, local, remote, at.Add(time.Minute)); len(result.Errors) != 0 {
		t.Fatalf("second pass: %#v", result)
	}
	if !mtime(t, cache).Equal(before) {
		t.Fatal("the gap was rewritten on the second pass")
	}
	writeTranscript(t, dirOf(path), baseOf(path), cursorTextTranscript(3, 200))
	if result := runAt(t, local, remote, at.Add(2*time.Minute)); len(result.Published) != 1 || len(result.Errors) != 0 {
		t.Fatalf("the smaller transcript did not publish: %#v; errors: %v", result, result.Errors)
	}
	if _, blocked, _ := local.LoadBlocked("session-1"); blocked {
		t.Fatal("the gap outlived the oversize transcript")
	}
}

// The rewrite guard compares text transcripts by prefix. Per-section
// sanitizing keeps that sound: a section appended to a transcript over 64 KB
// still extends the published text (under filter 6 both would have been cut
// to the same 64 KB, hiding the new activity), and a truncated file is still
// a rewrite.
func TestCursorTextAppendExtendsAndTruncationIsARewrite(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	content := cursorTextTranscript(100, 1000)
	if len(content) <= 64*1024 {
		t.Fatalf("test transcript is only %d bytes", len(content))
	}
	path := cursorTextSession(t, local, content)
	at := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	if result := runAt(t, local, remote, at); len(result.Published) != 1 {
		t.Fatalf("first publication: %#v", result)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("user: one more thing\nassistant: done\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	result := runAt(t, local, remote, at.Add(time.Hour))
	if len(result.Published) != 1 || len(result.Errors) != 0 {
		t.Fatalf("an appended section did not publish: %#v", result)
	}
	if _, blocked, _ := local.LoadBlocked("session-1"); blocked {
		t.Fatal("an appended section was treated as a rewrite")
	}
	bundle, _, _, err := local.LoadLastPublished("session-1")
	if err != nil || len(bundle.NativeText) != 1 || !strings.HasSuffix(bundle.NativeText[0].Content, "user: one more thing\nassistant: done") {
		t.Fatalf("the appended section is not in the published text (err=%v)", err)
	}

	writeTranscript(t, dirOf(path), baseOf(path), cursorTextTranscript(50, 1000))
	if result := runAt(t, local, remote, at.Add(2*time.Hour)); len(result.Published) != 0 || len(result.Errors) != 0 {
		t.Fatalf("result = %#v", result)
	}
	if reason, blocked, _ := local.LoadBlocked("session-1"); !blocked || reason != state.BlockedReasonTranscriptRewritten {
		t.Fatalf("a truncated text transcript was not a rewrite: reason=%q blocked=%t", reason, blocked)
	}
}

// An edit inside an already-published section, even one that keeps the file
// the same length, means the published snapshot is no longer a prefix of the
// file: it is a rewrite, not new activity.
func TestCursorTextMidSectionEditIsARewrite(t *testing.T) {
	t.Parallel()
	local := newTestStore(t)
	remote := storagetest.NewMemoryStore()
	content := cursorTextTranscript(100, 1000)
	path := cursorTextSession(t, local, content)
	at := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	if result := runAt(t, local, remote, at); len(result.Published) != 1 {
		t.Fatalf("first publication: %#v", result)
	}
	edited := strings.Replace(content, "answer 50 ", "answer 5x ", 1)
	if len(edited) != len(content) || edited == content {
		t.Fatal("the edit must change bytes without changing the length")
	}
	writeTranscript(t, dirOf(path), baseOf(path), edited)
	if result := runAt(t, local, remote, at.Add(time.Hour)); len(result.Published) != 0 || len(result.Errors) != 0 {
		t.Fatalf("result = %#v", result)
	}
	if reason, blocked, _ := local.LoadBlocked("session-1"); !blocked || reason != state.BlockedReasonTranscriptRewritten {
		t.Fatalf("a mid-section edit was not a rewrite: reason=%q blocked=%t", reason, blocked)
	}
}
