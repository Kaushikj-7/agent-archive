package backfill

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

type jsonOutputWriter func([]byte) (int, error)

func (f jsonOutputWriter) Write(p []byte) (int, error) { return f(p) }

func TestRenderJSONReportsIncompleteWrites(t *testing.T) {
	failure := errors.New("synthetic output failure")
	for _, tc := range []struct {
		name   string
		writer io.Writer
		want   error
	}{
		{"failed", jsonOutputWriter(func([]byte) (int, error) { return 0, failure }), failure},
		{"short", jsonOutputWriter(func(p []byte) (int, error) { return len(p) - 1, nil }), io.ErrShortWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := RenderJSON(tc.writer, Plan{RetentionDays: 0}); !errors.Is(err, tc.want) {
				t.Fatalf("output error = %v, want %v", err, tc.want)
			}
		})
	}
	var output bytes.Buffer
	if err := RenderJSON(&output, Plan{RetentionDays: 0}); err != nil {
		t.Fatal(err)
	}
	var plan map[string]any
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if output.Bytes()[output.Len()-1] != '\n' {
		t.Fatal("JSON output lost final newline")
	}
}
