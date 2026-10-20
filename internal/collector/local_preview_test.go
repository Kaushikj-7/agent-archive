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

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/state"
)

const testPreviewWindow = 64 << 10

var testPreviewLimits = PreviewLimits{HeadBytes: testPreviewWindow, TailBytes: testPreviewWindow, RecordBytes: testPreviewWindow}

// writeLargeClaudeTranscript writes a synthetic Claude transcript of about
// size bytes: a prompt holding a secret, filler replies, and then a name,
// a pull-request link and a branch in its last records.
func writeLargeClaudeTranscript(t *testing.T, size int) string {
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
	b.WriteString(`{"type":"custom-title","customTitle":"Widget repair","sessionId":"native-large"}` + "\n")
	b.WriteString(`{"type":"pr-link","sessionId":"native-large","prNumber":"213","prRepository":"example-org/widget-tools","prUrl":"https://github.com/example-org/widget-tools/pull/213"}` + "\n")
	b.WriteString(`{"type":"assistant","sessionId":"native-large","timestamp":"2026-01-02T01:00:00Z","gitBranch":"widget-fix","message":{"role":"assistant","content":"Done"}}` + "\n")
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
	reg := archive.SessionRegistration{ArchiveSessionID: "large", NativeSessionID: "native-large", ProjectID: "project", ProjectRoot: "/synthetic/project", SessionStartedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), RegisteredAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Harness: archive.Harness{Name: "claude"}, TranscriptPath: writeLargeClaudeTranscript(t, 2<<20)}
	preview, err := PreviewLocalTranscript(t.Context(), home, reg, "", testSources, testParsers, 1<<20, testPreviewLimits)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Bytes > 2*testPreviewWindow+1 || strings.Contains(preview.Title, "sk-abcdefghijklmnopqrstuv") || !strings.Contains(preview.Title, "[REDACTED]") {
		t.Fatalf("preview read %d bytes, title %q", preview.Bytes, preview.Title)
	}
	bundle, err := ReadLocalBundle(t.Context(), home, reg, time.Now(), "", testSources)
	if err != nil {
		t.Fatal(err)
	}
	parser, _ := testParsers.LookupParser("claude")
	analysis, err := parser.Parse(t.Context(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	full, hasPrompt := archive.LabelsFromAnalysis(analysis)
	got := archive.Labels{Name: preview.Name, Title: preview.Title, Branch: preview.Branch, PullRequests: preview.PullRequests}
	if !hasPrompt || got.Name != full.Name || got.Title != full.Title || got.Branch != full.Branch || !slices.Equal(got.PullRequests, full.PullRequests) || len(got.PullRequests) != 1 {
		t.Fatalf("preview labels %+v, full read %+v", got, full)
	}
}

// Sessions a preview cannot stand in for are left to the full read.
func TestLocalPreviewLeavesOtherSourcesToTheFullRead(t *testing.T) {
	t.Parallel()
	large := writeLargeClaudeTranscript(t, 2<<20)
	base := archive.SessionRegistration{ArchiveSessionID: "large", NativeSessionID: "native-large", ProjectID: "project", ProjectRoot: "/synthetic/project", SessionStartedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), RegisteredAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Harness: archive.Harness{Name: "claude"}, TranscriptPath: large}
	for _, tc := range []struct {
		name     string
		edit     func(*archive.SessionRegistration)
		minBytes int64
	}{
		{name: "shorter than the minimum", minBytes: 4 << 20},
		{name: "cursor database chat", edit: func(r *archive.SessionRegistration) {
			*r = archive.SessionRegistration{ArchiveSessionID: "chat", NativeSessionID: "composer", SourceKind: archive.SourceKindCursorSQLite, SourceKey: "composer", Harness: archive.Harness{Name: "cursor"}}
		}},
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
			minBytes := tc.minBytes
			if minBytes == 0 {
				minBytes = 1 << 20
			}
			_, err := PreviewLocalTranscript(t.Context(), newTestStore(t).Home(), reg, "", testSources, testParsers, minBytes, testPreviewLimits)
			if !errors.Is(err, ErrPreviewUnavailable) {
				t.Fatalf("got %v, want ErrPreviewUnavailable", err)
			}
		})
	}
}

// A session label reaches the full read as evidence, not from the
// transcript, so a session with one is read whole.
func TestLocalPreviewLeavesLabeledSessionsToTheFullRead(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	reg := archive.SessionRegistration{ArchiveSessionID: "large", NativeSessionID: "native-large", ProjectID: "project", ProjectRoot: "/synthetic/project", SessionStartedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), RegisteredAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Harness: archive.Harness{Name: "claude"}, TranscriptPath: writeLargeClaudeTranscript(t, 2<<20)}
	if err := s.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	label := archive.SessionLabel{NativeID: "native-large", State: archive.SessionLabelPresent, Name: "Thread name", Source: archive.SessionLabelDatabase, Contract: "synthetic"}
	if err := s.SaveRequest(reg.ArchiveSessionID, "hook", time.Now(), label.Evidence(time.Now().UTC(), "claude")); err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewLocalTranscript(t.Context(), s.Home(), reg, "", testSources, testParsers, 1<<20, testPreviewLimits); !errors.Is(err, ErrPreviewUnavailable) {
		t.Fatalf("got %v, want ErrPreviewUnavailable", err)
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
	if _, err := PreviewLocalTranscript(t.Context(), s.Home(), reg, "", refuseNativeSources{t}, testParsers, 0, testPreviewLimits); !errors.Is(err, state.ErrDurableStorageRecovery) {
		t.Fatalf("got %v, want ErrDurableStorageRecovery", err)
	}
}
