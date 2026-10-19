package cli

import (
	"encoding/json"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage/storagetest"
)

func readPathSelectedSourceReference() (archive.SourceReference, error) {
	bundle := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, ArchiveSessionID: "00000001000000000000000000000001", NativeSessionID: "native-00000001", ProjectID: "project-0", Capture: archive.SourceCapture{Harness: archive.Harness{Name: "codex"}, AdapterName: "codex", AdapterVersion: "1", SourceFormat: "jsonl", FilterVersion: archive.FilterVersion, CapturedAt: storagetest.BenchmarkTime}, NativeRecords: []map[string]any{{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Synthetic read path benchmark request"}}}}}}
	compressed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		return archive.SourceReference{}, err
	}
	key, err := archive.SourceObjectKey(bundle, compressed.SHA256)
	if err != nil {
		return archive.SourceReference{}, err
	}
	return archive.SourceReference{Key: key, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}, nil
}

func TestReadPathOutputRejectsIncompleteDocumentEnvelopes(t *testing.T) {
	t.Parallel()
	const size = 100
	mem := storagetest.NewMemoryStore()
	storagetest.SeedReadPathArchive(t, mem, size, 0)
	for _, command := range []readPathCommand{readPathListPiped, readPathShowShort, readPathHandoffQuery} {
		env := readPathEnv(t, t.TempDir(), mem, command)
		code, out, diag := runReadPathBenchmark(readPathBenchmarkArgs(command, false), command, env)
		if err := validateReadPathOutput(command, size, code, out, false); err != nil {
			t.Fatalf("valid fixture %s: %v %s", command, err, diag)
		}
		var document map[string]any
		if err := json.Unmarshal(out, &document); err != nil {
			t.Fatal(err)
		}
		var fields []string
		switch command {
		case readPathListPiped:
			fields = []string{"schema_version", "limit", "returned", "total_matched_known", "total_matched", "truncated"}
		case readPathShowShort:
			fields = []string{"schema_version", "harness", "captured_at", "filter_version", "source_bundle"}
		case readPathHandoffQuery:
			fields = []string{"handoff_version", "session.harness", "exchanges"}
		case readPathListPicker, readPathShowPicker, readPathHandoffPicker, readPathListScoped:
			t.Fatal("unexpected picker or scoped envelope control", command)
		default:
			t.Fatal("unknown envelope control", command)
		}
		for _, field := range fields {
			var mutated map[string]any
			if err := json.Unmarshal(out, &mutated); err != nil {
				t.Fatal(err)
			}
			if field == "session.harness" {
				delete(mutated["session"].(map[string]any), "harness")
			} else {
				delete(mutated, field)
			}
			bad, err := json.Marshal(mutated)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateReadPathOutput(command, size, 0, bad, false); err == nil {
				t.Fatal("accepted stripped document field", command, field)
			}
		}
		if command == readPathHandoffQuery {
			exchanges := document["exchanges"].([]any)
			delete(exchanges[0].(map[string]any), "prompt")
			bad, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateReadPathOutput(command, size, 0, bad, false); err == nil {
				t.Fatal("accepted handoff without fixture prompt")
			}
		}
	}
}
