package nativecodec

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/archive"
)

func TestCodexInspectionDiscardsRowsAndIdentityCollections(t *testing.T) {
	var raw strings.Builder
	for i := range 256 {
		fmt.Fprintf(&raw, `{"type":"message","session_id":"session-%d","agent_id":"agent-%d","timestamp":"2020-01-01T00:00:00Z","content":"visible","password":"SECRET"}`+"\n", i, i)
	}
	full, err := FilterCodexCaptureEncodedJSONL(strings.NewReader(raw.String()), "ordinary.jsonl", nil, nil, archive.CaptureBoundary{})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	inspected, err := InspectCodexCaptureEncodedJSONL(strings.NewReader(raw.String()), "ordinary.jsonl", func(safe map[string]any) {
		seen++
		if _, present := safe["password"]; present {
			t.Fatal("observer saw preprivacy content")
		}
	}, nil, nil, archive.CaptureBoundary{})
	if err != nil {
		t.Fatal(err)
	}
	if seen != len(full.Records) || inspected.Boundary != full.Boundary || inspected.NativeStartAt != full.NativeStartAt {
		t.Fatalf("inspection counters differ: seen=%d summary=%+v retained=%+v", seen, inspected, full.Boundary)
	}
	if len(inspected.Records) != 0 || len(inspected.SessionIDs) != 0 || len(inspected.AgentIDs) != 0 || len(inspected.LocalIdentity.Candidates) != 0 || len(inspected.Gaps) != 0 {
		t.Fatal("inspection accumulated transcript collections")
	}
}
