package collector

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agents/cursor"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

// A Cursor session captured from its agent transcript, which records no chat
// name, is published with the name Cursor gave the chat in its database,
// whether the session was admitted by a hook or imported.
func TestCursorTranscriptSessionPublishesItsChatName(t *testing.T) {
	for _, origin := range []archive.SessionOrigin{archive.SessionOriginHook, archive.SessionOriginImport} {
		t.Run(string(origin), func(t *testing.T) {
			const id = "00000000-0000-4000-8000-000000000001"
			db := newCursorDB(t, false)
			db.namedChatSaying(id, "Invented chat name", 1, "hi")
			dir := filepath.Join(t.TempDir(), "projects", "work-widget", "agent-transcripts", id)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := writeTranscript(t, dir, id+".jsonl", `{"role":"user","message":{"content":[{"type":"text","text":"ship it"}]}}`+"\n")
			local := newTestStore(t)
			reg := registration(t, path)
			reg.Harness, reg.NativeSessionID, reg.Origin = archive.Harness{Name: "cursor"}, id, origin
			if origin == archive.SessionOriginImport {
				reg.StartedAtSource, reg.ImportBatch = archive.StartedAtSourceTranscript, archive.NewImportBatch("2026-10-15-1")
			}
			if err := local.SaveRegistration(reg); err != nil {
				t.Fatal(err)
			}
			remote := storagetest.NewMemoryStore()
			now := reg.RegisteredAt.Add(time.Hour)
			opts := Options{Sources: testSources, Parsers: testParsers, Labels: testSources, MachineID: "machine", CursorDatabase: db.path, Now: func() time.Time { return now }}
			for range 2 {
				if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) > 0 {
					t.Fatalf("%+v %v", result, err)
				}
				now = now.Add(time.Hour)
			}
			metadata := fetchMetadata(t, remote, "cursor", reg.ArchiveSessionID)
			bundle := fetchBundle(t, remote, metadata)
			if bundle.Capture.SourceFormat != "cursor-jsonl" {
				t.Fatalf("source format = %q", bundle.Capture.SourceFormat)
			}
			label, _, ok := archive.CurrentSessionLabel(bundle)
			if !ok || label.Contract != cursor.LabelContract || label.Source != archive.SessionLabelDatabase || label.Name != "Invented chat name" {
				t.Fatalf("retained label = %+v, %v", label, ok)
			}
			if metadata.Name != "Invented chat name" || archive.DisplayTitle(metadata) != "Invented chat name" {
				t.Fatalf("metadata name = %q, display title = %q", metadata.Name, archive.DisplayTitle(metadata))
			}
		})
	}
}
