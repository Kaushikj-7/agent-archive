package collector

import (
	"context"
	"encoding/json"
	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewUsesFilteredHeadPromptAndRecentTailName(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "preview.jsonl")
	lines := []string{`{"type":"user","sessionId":"native-session","message":{"role":"user","content":"Fix widget"}}`, `{"type":"user","isMeta":true,"message":{"role":"user","content":"Injected instructions"}}`}
	tool, _ := json.Marshal(map[string]any{"type": "tool_result", "content": strings.Repeat("noise", 10000)})
	lines = append(lines, string(tool), `{"type":"custom-title","customTitle":"Recent widget name"}`, `{"type":"custom-title","customTitle":"unfinished"`)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := transcriptio.Open(transcriptio.OS{}, path, transcriptio.OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	p, err := PreviewTranscript(context.Background(), s, testClaudePreview(), PreviewLimits{HeadBytes: 1024, TailBytes: 1024, RecordBytes: 1024}, "native-session")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Recent widget name" || p.Title != "Fix widget" || p.NameComplete || p.Bytes > 2049 || strings.Contains(p.Title, "Injected") {
		t.Fatalf("preview %+v", p)
	}
}

func TestPreviewDoesNotPromoteTailReplyAcrossUnreadGap(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "preview.jsonl")
	raw := `{"type":"user","message":{"role":"user","content":"<command-name>/review</command-name>"}}` + "\n" + strings.Repeat("x", 5000) + "\n" + `{"type":"assistant","message":{"role":"assistant","content":"Response"}}` + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := transcriptio.Open(transcriptio.OS{}, path, transcriptio.OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	p, err := PreviewTranscript(context.Background(), s, testClaudePreview(), PreviewLimits{HeadBytes: 512, TailBytes: 512, RecordBytes: 512}, "native-session")
	if err != nil || p.Title != "" {
		t.Fatalf("gap inferred prompt %+v %v", p, err)
	}
}

func testClaudePreview() agentapi.RecordPreviewer {
	preview, _ := testParsers.LookupPreview("claude")
	return preview
}

func TestPartialPreviewCannotProveGeneratedNameIsFinal(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "preview.jsonl")
	lines := []string{`{"type":"user","sessionId":"own","message":{"role":"user","content":"Fix widget"}}`, `{"type":"ai-title","aiTitle":"Generated","sessionId":"own"}`, strings.Repeat("x", 2000), `{"type":"custom-title","customTitle":"Hidden middle rename","sessionId":"own"}`, strings.Repeat("x", 2000), `{"type":"ai-title","aiTitle":"Latest generated","sessionId":"own"}`, `{"type":"custom-title","customTitle":"Fork title","sessionId":"parent"}`}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := transcriptio.Open(transcriptio.OS{}, path, transcriptio.OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	preview, err := PreviewTranscript(context.Background(), s, testClaudePreview(), PreviewLimits{HeadBytes: 512, TailBytes: 512, RecordBytes: 512}, "own")
	if err != nil {
		t.Fatal(err)
	}
	if preview.NameComplete || preview.Name != "Latest generated" || preview.Title != "Fix widget" {
		t.Fatalf("partial preview %+v", preview)
	}
}

type partialRecordPreview struct{}

func (partialRecordPreview) PreviewRecord(context.Context, []byte) (archive.RecordPreview, error) {
	return archive.RecordPreview{Gaps: []archive.CaptureGap{{Code: "preview_partial"}}}, nil
}

func TestDecoderPartialGapCannotClaimCompleteLabels(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "preview.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := transcriptio.Open(transcriptio.OS{}, path, transcriptio.OpenPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	preview, err := PreviewTranscript(t.Context(), snapshot, partialRecordPreview{}, PreviewLimits{HeadBytes: 1024, TailBytes: 1024, RecordBytes: 1024}, "own")
	if err != nil || preview.NameComplete || len(preview.Gaps) != 1 || preview.Gaps[0].Code != "preview_partial" {
		t.Fatalf("partial %+v %v", preview, err)
	}
}
