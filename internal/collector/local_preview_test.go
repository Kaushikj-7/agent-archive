package collector

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/codex"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

const testPreviewWindow = 64 << 10

var testPreviewLimits = PreviewLimits{HeadBytes: testPreviewWindow, TailBytes: testPreviewWindow, RecordBytes: testPreviewWindow}

// largeTranscriptTail is a name, a pull-request link and a branch.
var largeTranscriptTail = []string{
	`{"type":"custom-title","customTitle":"Widget repair","sessionId":"native-large"}`,
	`{"type":"pr-link","sessionId":"native-large","prNumber":"213","prRepository":"example-org/widget-tools","prUrl":"https://github.com/example-org/widget-tools/pull/213"}`,
	`{"type":"assistant","sessionId":"native-large","timestamp":"2026-01-02T01:00:00Z","gitBranch":"widget-fix","message":{"role":"assistant","content":"Done"}}`,
}

// writeLargeClaudeTranscript writes a synthetic Claude transcript of about
// size bytes: a prompt holding a secret on branch main, filler replies, and
// then tail (largeTranscriptTail when nil).
func writeLargeClaudeTranscript(t *testing.T, size int, tail ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "native-large.jsonl")
	var b strings.Builder
	b.WriteString(`{"type":"user","uuid":"u1","sessionId":"native-large","timestamp":"2026-01-02T00:00:00Z","gitBranch":"main","message":{"role":"user","content":"Fix the widget, key sk-abcdefghijklmnopqrstuv"}}` + "\n")
	reply, err := json.Marshal(map[string]any{"type": "assistant", "sessionId": "native-large", "timestamp": "2026-01-02T00:00:01Z", "message": map[string]any{"role": "assistant", "content": strings.Repeat("widget work ", 1000)}})
	if err != nil {
		t.Fatal(err)
	}
	for b.Len() < size {
		b.Write(reply)
		b.WriteString("\n")
	}
	if tail == nil {
		tail = largeTranscriptTail
	}
	for _, line := range tail {
		b.WriteString(line + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A large transcript is previewed from its filtered windows alone, and its
// labels are the ones a full read derives.
func TestLocalPreviewReadsOnlyTheWindowsOfALargeTranscript(t *testing.T) {
	t.Parallel()
	home := newTestStore(t).Home()
	reg := largeRegistration(t)
	preview, err := PreviewLocalTranscript(t.Context(), home, reg, "", testSources, testParsers, testPreviewLimits)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Bytes > 2*testPreviewWindow+1 || strings.Contains(preview.Title, "sk-abcdefghijklmnopqrstuv") || !strings.Contains(preview.Title, "[REDACTED]") || len(preview.PullRequests) != 1 || preview.Branch != "widget-fix" {
		t.Fatalf("preview read %d bytes: %+v", preview.Bytes, preview)
	}
	requirePreviewMatchesFullRead(t, home, reg, preview)
}

// The preview takes a branch and pull requests from the records a full parse
// takes them from: a sidechain record's too, and the last branch recorded,
// so a HEAD there clears an earlier branch.
func TestLocalPreviewLabelsMatchTheFullReadAtTheEdges(t *testing.T) {
	t.Parallel()
	home := newTestStore(t).Home()
	reg := largeRegistration(t, `{"type":"pr-link","isSidechain":true,"sessionId":"native-large","prNumber":"214","prRepository":"example-org/widget-tools"}`,
		`{"type":"assistant","sessionId":"native-large","timestamp":"2026-01-02T01:00:00Z","gitBranch":"HEAD","message":{"role":"assistant","content":"Detached"}}`)
	preview, err := PreviewLocalTranscript(t.Context(), home, reg, "", testSources, testParsers, testPreviewLimits)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Branch != "" || len(preview.PullRequests) != 1 || preview.PullRequests[0].Number != 214 {
		t.Fatalf("preview %+v", preview)
	}
	requirePreviewMatchesFullRead(t, home, reg, preview)
}

func largeRegistration(t *testing.T, tail ...string) archive.SessionRegistration {
	t.Helper()
	return archive.SessionRegistration{ArchiveSessionID: "large", NativeSessionID: "native-large", ProjectID: "project", ProjectRoot: "/synthetic/project", SessionStartedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), RegisteredAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Harness: archive.Harness{Name: "claude"}, TranscriptPath: writeLargeClaudeTranscript(t, 2<<20, tail...)}
}

func requirePreviewMatchesFullRead(t *testing.T, home string, reg archive.SessionRegistration, preview TranscriptPreview) {
	t.Helper()
	bundle, err := ReadLocalBundle(t.Context(), home, reg, time.Now(), "", testSources)
	if err != nil {
		t.Fatal(err)
	}
	parser, _ := testParsers.LookupParser(reg.Harness.Name)
	analysis, err := parser.Parse(t.Context(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	full, hasPrompt := archive.LabelsFromAnalysis(analysis)
	got := archive.Labels{Name: preview.Name, Title: preview.Title, Branch: preview.Branch, PullRequests: preview.PullRequests}
	if !hasPrompt || got.Name != full.Name || got.Title != full.Title || got.Branch != full.Branch || !slices.Equal(got.PullRequests, full.PullRequests) {
		t.Fatalf("preview labels %+v, full read %+v", got, full)
	}
}

// Sessions a preview cannot stand in for are left to the full read.
func TestLocalPreviewLeavesOtherSourcesToTheFullRead(t *testing.T) {
	t.Parallel()
	large := writeLargeClaudeTranscript(t, 2<<20)
	base := archive.SessionRegistration{ArchiveSessionID: "large", NativeSessionID: "native-large", ProjectID: "project", ProjectRoot: "/synthetic/project", SessionStartedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), RegisteredAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Harness: archive.Harness{Name: "claude"}, TranscriptPath: large}
	for _, tc := range []struct {
		name string
		edit func(*archive.SessionRegistration)
	}{
		{name: "cursor database chat", edit: func(r *archive.SessionRegistration) {
			*r = archive.SessionRegistration{ArchiveSessionID: "chat", NativeSessionID: "composer", SourceKind: archive.SourceKindCursorSQLite, SourceKey: "composer", Harness: archive.Harness{Name: "cursor"}}
		}},
		{name: "subagent", edit: func(r *archive.SessionRegistration) { r.ParentSessionID = "parent" }},
		{name: "no record previewer", edit: func(r *archive.SessionRegistration) { r.Harness.Name = "cursor" }},
		{name: "staged import", edit: func(r *archive.SessionRegistration) { r.StagedImportSHA256 = strings.Repeat("a", 64) }},
		{name: "historical project", edit: func(r *archive.SessionRegistration) { r.HistoricalProject = &archive.HistoricalProjectProof{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := base
			if tc.edit != nil {
				tc.edit(&reg)
			}
			_, err := PreviewLocalTranscript(t.Context(), newTestStore(t).Home(), reg, "", testSources, testParsers, testPreviewLimits)
			if !errors.Is(err, ErrPreviewUnavailable) {
				t.Fatalf("got %v, want ErrPreviewUnavailable", err)
			}
		})
	}
}

// A transcript the windows span is previewed whole, as one window, so a
// record across the end of the head window that starts before the tail
// window is still read. One holding a record over the record limit is read
// whole by the full read instead.
func TestLocalPreviewReadsASpannedTranscriptWhole(t *testing.T) {
	t.Parallel()
	reply := func(n int) string {
		b, err := json.Marshal(map[string]any{"type": "assistant", "sessionId": "native-large", "timestamp": "2026-01-02T00:00:01Z", "message": map[string]any{"role": "assistant", "content": strings.Repeat("w", n)}})
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	write := func(middle string) string {
		var b strings.Builder
		b.WriteString(`{"type":"user","uuid":"u1","sessionId":"native-large","timestamp":"2026-01-02T00:00:00Z","message":{"role":"user","content":"Fix the widget"}}` + "\n")
		for b.Len() < testPreviewWindow*5/8 {
			b.WriteString(reply(1 << 10))
		}
		at := b.Len()
		b.WriteString(middle)
		for b.Len() < testPreviewWindow*15/8 {
			b.WriteString(reply(1 << 10))
		}
		if at+len(middle) <= testPreviewWindow || at >= b.Len()-testPreviewWindow || b.Len() > 2*testPreviewWindow {
			t.Fatalf("fixture does not straddle: record %d-%d of %d", at, at+len(middle), b.Len())
		}
		path := filepath.Join(t.TempDir(), "native-large.jsonl")
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	home := newTestStore(t).Home()
	reg := largeRegistration(t)
	padding := strings.Repeat(" ", testPreviewWindow/2)
	reg.TranscriptPath = write(`{"type":"pr-link","sessionId":"native-large","prNumber":"215","prRepository":"example-org/widget-tools"}` + padding + "\n")
	preview, err := PreviewLocalTranscript(t.Context(), home, reg, "", testSources, testParsers, testPreviewLimits)
	if err != nil || !preview.NameComplete || len(preview.PullRequests) != 1 || preview.PullRequests[0].Number != 215 {
		t.Fatalf("straddling record not read: %+v %v", preview, err)
	}
	requirePreviewMatchesFullRead(t, home, reg, preview)
	reg.TranscriptPath = write(reply(testPreviewWindow + 1))
	if _, err := PreviewLocalTranscript(t.Context(), home, reg, "", testSources, testParsers, testPreviewLimits); !errors.Is(err, ErrPreviewUnavailable) {
		t.Fatalf("record over the limit: got %v, want ErrPreviewUnavailable", err)
	}
}

// A Codex session with a thread name is previewed from its windows, named
// from its label as a full read names it: by a name Codex can interpret,
// none when the label confirms there is none, and not by one it cannot.
func TestLocalPreviewNamesALabeledCodexSessionAsTheFullRead(t *testing.T) {
	t.Parallel()
	const native = "01900000-0000-7000-8000-000000000001"
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	var b strings.Builder
	b.WriteString(`{"type":"session_meta","payload":{"id":"` + native + `","cli_version":"0.159.2","history_mode":"legacy"}}` + "\n")
	b.WriteString(`{"type":"response_item","timestamp":"2026-01-02T00:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the widget"}]}}` + "\n")
	reply := `{"type":"response_item","timestamp":"2026-01-02T00:00:01Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("widget work ", 1000) + `"}]}}` + "\n"
	for b.Len() < 2<<20 {
		b.WriteString(reply)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		label archive.SessionLabel
		want  string
	}{
		{"named", archive.SessionLabel{State: archive.SessionLabelPresent, Name: "Thread   name", Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}, "Thread name"},
		{"confirmed absent", archive.SessionLabel{State: archive.SessionLabelAbsent, Source: archive.SessionLabelDatabase, Contract: codex.LabelContract}, ""},
		{"uninterpretable", archive.SessionLabel{State: archive.SessionLabelPresent, Name: "Thread name", Source: archive.SessionLabelDatabase, Contract: "synthetic"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			reg := registration(t, path)
			reg.NativeSessionID = native
			if err := s.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			tc.label.NativeID = native
			if err := s.SaveRequest(reg.ArchiveSessionID, "hook", time.Now(), tc.label.Evidence(time.Now().UTC(), "codex")); err != nil {
				t.Fatal(err)
			}
			preview, err := PreviewLocalTranscript(t.Context(), s.Home(), reg, "", testSources, testParsers, testPreviewLimits)
			if err != nil {
				t.Fatal(err)
			}
			if preview.Bytes > 2*testPreviewWindow+1 || preview.Name != tc.want || preview.Title != "Fix the widget" {
				t.Fatalf("preview read %d bytes: %+v", preview.Bytes, preview)
			}
			requirePreviewMatchesFullRead(t, s.Home(), reg, preview)
		})
	}
}

// A session label reaches a full read as evidence, not from the transcript,
// so a session with one whose previewer cannot name it from the label, or
// whose label names another session, is read whole.
func TestLocalPreviewLeavesLabelsItCannotApplyToTheFullRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		harness string
		native  string
	}{
		{"no namer", "claude", "native-large"},
		{"another session's label", "codex", "native-other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newTestStore(t)
			reg := largeRegistration(t)
			if err := s.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			label := archive.SessionLabel{NativeID: tc.native, State: archive.SessionLabelPresent, Name: "Thread name", Source: archive.SessionLabelDatabase, Contract: "synthetic"}
			if err := s.SaveRequest(reg.ArchiveSessionID, "hook", time.Now(), label.Evidence(time.Now().UTC(), tc.harness)); err != nil {
				t.Fatal(err)
			}
			if _, err := PreviewLocalTranscript(t.Context(), s.Home(), reg, "", testSources, testParsers, testPreviewLimits); !errors.Is(err, ErrPreviewUnavailable) {
				t.Fatalf("got %v, want ErrPreviewUnavailable", err)
			}
		})
	}
}

// The preview keeps the full read's durable read check, which refuses before
// any source is opened.
func TestLocalPreviewRefusesUnknownDurableStateBeforeTheSource(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := os.WriteFile(filepath.Join(s.Home(), "pending", ".pending-123"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "absent", NativeSessionID: "native", Harness: archive.Harness{Name: "codex"}, TranscriptPath: "/synthetic/never-open"}
	if _, err := PreviewLocalTranscript(t.Context(), s.Home(), reg, "", refuseNativeSources{t}, testParsers, testPreviewLimits); !errors.Is(err, state.ErrDurableStorageRecovery) {
		t.Fatalf("got %v, want ErrDurableStorageRecovery", err)
	}
}
