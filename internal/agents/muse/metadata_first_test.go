package muse

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// metadataFirstStartup derives the metadata-first lane from the synthetic
// capture-selection session: the frame, then metadata, the session start
// command, start intake, session started and the original opening. change may
// edit the five records after the frame, in that order.
func metadataFirstStartup(t *testing.T, change func(rows []map[string]any)) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(readSynthetic(t, "synthetic-msp-empty.jsonl")), []byte("\n"))
	rows := make([]map[string]any, 0, 6)
	for _, line := range lines[1:] {
		m, _, err := decodeObject(line)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, m)
	}
	// Capture selection, command, start intake, started, metadata, opening.
	selection, metadata := rows[0], rows[4]
	metadata["recorded_at"] = selection["recorded_at"]
	rows = append([]map[string]any{metadata}, append(rows[1:4], rows[5])...)
	if change != nil {
		change(rows)
	}
	out := [][]byte{lines[0]}
	for i, row := range rows {
		row["sequence"] = i + 3
		wire, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, wire)
	}
	return append(bytes.Join(out, []byte("\n")), '\n')
}

func TestMetadataFirstStartup(t *testing.T) {
	t.Parallel()
	raw := metadataFirstStartup(t, nil)
	// The native input is also a fuzz seed.
	golden.Check(t, filepath.Join("testdata", "synthetic-metadata-first.jsonl"), raw)
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-metadata-first.golden.jsonl"), append(bytes.Join(out.Records, []byte("\n")), '\n'))
	bundle := bundleFor(t, out)
	if _, err := (Parser{}).Parse(t.Context(), bundle); err != nil {
		t.Fatal(err)
	}
	again, err := (Filter{}).Refilter(t.Context(), bundle, time.Time{})
	if err != nil || !reflect.DeepEqual(out.Records, again.Records) || !reflect.DeepEqual(out.Gaps, again.Gaps) {
		t.Fatalf("refilter changed retained evidence: %v", err)
	}
	header, err := inspectSynthetic(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := routeStartupFacts(t, raw)
	if err != nil || facts.NativeID != header.NativeID || facts.StreamID != header.NativeID || facts.Cwd != header.Directory || !facts.NativeCreatedAt.Equal(header.StartedAt) || !facts.NativeCreatedAt.Equal(out.NativeStartAt) {
		t.Fatalf("startup consumers disagree: %+v %+v %v", header, facts, err)
	}
}

func TestMetadataFirstStartupRefusesUnqualifiedOrders(t *testing.T) {
	t.Parallel()
	record := func(m map[string]any) map[string]any { return object(object(m["payload"])["record"]) }
	for name, change := range map[string]func(rows []map[string]any){
		"opening resumes": func(rows []map[string]any) { record(rows[4])["resume"] = true },
		"caused metadata": func(rows []map[string]any) { rows[0]["causation_id"] = rows[1]["causation_id"] },
		"caused opening":  func(rows []map[string]any) { rows[4]["causation_id"] = rows[1]["causation_id"] },
		"other workspace": func(rows []map[string]any) { record(rows[0])["workspace_root"] = "/work/elsewhere" },
		"other model":     func(rows []map[string]any) { record(rows[0])["model_id"] = "other-model" },
		"metadata after the command": func(rows []map[string]any) {
			at := rows[1]["recorded_at"]
			rows[0]["recorded_at"] = json.Number("9" + string(at.(json.Number)))
		},
		"start intake before the command": func(rows []map[string]any) { rows[1], rows[2] = rows[2], rows[1] },
		"opening before start":            func(rows []map[string]any) { rows[3], rows[4] = rows[4], rows[3] },
		"resume activation": func(rows []map[string]any) {
			object(object(rows[2]["payload"])["payload"])["activation_source"] = "resume"
		},
	} {
		raw := metadataFirstStartup(t, change)
		if out, err := filterBytes(t, raw, agentapi.ReadLimits{}); err == nil || len(out.Records) != 0 {
			t.Errorf("%s: unqualified metadata-first startup admitted", name)
		}
		if _, err := inspectSynthetic(t, raw); err == nil {
			t.Errorf("%s: header inspection admitted the startup", name)
		}
	}
}
