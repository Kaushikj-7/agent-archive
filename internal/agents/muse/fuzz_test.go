package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/transcriptio"
)

type memoryInput struct{ *bytes.Reader }

func (in memoryInput) Length() int64 { return in.Size() }

func (in memoryInput) Stamp() transcriptio.Stamp { return transcriptio.Stamp{Size: in.Size()} }

func (memoryInput) Check() error { return nil }

func (memoryInput) Records(context.Context, bool, int64, int64, func([]byte) bool) (transcriptio.RecordWindow, error) {
	return transcriptio.RecordWindow{}, errFormat
}

func FuzzMuseNative(f *testing.F) {
	for _, name := range []string{"synthetic-exec-empty.jsonl", "synthetic-exec-conversation.jsonl", "synthetic-msp-empty.jsonl", "synthetic-msp-conversation.jsonl"} {
		raw, e := os.ReadFile(filepath.Join("testdata", name))
		if e != nil {
			f.Fatal(e)
		}
		f.Add(raw)
		if name == "synthetic-exec-empty.jsonl" {
			f.Add(bytes.Replace(raw, []byte(`,"model_id":"synthetic-model"`), nil, 1))
		}
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		budget := agentapi.NewNativeReadBudget(8 << 20)
		out, release, e := (Filter{}).FilterLeased(t.Context(), agentapi.NativeInput{File: memoryInput{bytes.NewReader(raw)}}, agentapi.FilterContext{Limits: agentapi.ReadLimits{RawBytes: 1 << 20, RecordBytes: 1 << 20, FilteredBytes: 1 << 20}}, budget)
		release()
		if e != nil {
			if len(out.Records) != 0 {
				t.Fatal("partial successful artifact")
			}
			return
		}
		b := bundleFor(t, out)
		safe, e := (Filter{}).Refilter(t.Context(), b, time.Time{})
		if e != nil || !reflect.DeepEqual(out.Records, safe.Records) {
			t.Fatalf("refilter: %v", e)
		}
		if _, e := (Parser{}).Parse(t.Context(), b); e != nil {
			t.Fatal(e)
		}
		if !(Filter{}).EvidenceExtends(b, b) {
			t.Fatal("identity changed")
		}
	})
}

func FuzzMuseRetained(f *testing.F) {
	for _, name := range []string{"synthetic-exec-empty.golden.jsonl", "synthetic-msp-conversation.golden.jsonl"} {
		raw, e := os.ReadFile(filepath.Join("testdata", name))
		if e != nil {
			f.Fatal(e)
		}
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		b := archive.SourceBundle{SchemaVersion: archive.SourceSchemaVersion, Capture: archive.SourceCapture{Harness: archive.Harness{Name: "muse"}, AdapterName: "muse", SourceFormat: "muse-jsonl"}}
		for line := range bytes.SplitSeq(bytes.TrimSpace(raw), []byte("\n")) {
			var m map[string]any
			if json.Unmarshal(line, &m) != nil {
				return
			}
			b.NativeRecords = append(b.NativeRecords, m)
		}
		budget := agentapi.NewNativeReadBudget(8 << 20)
		out, release, e := (Filter{}).RefilterLeased(t.Context(), b, time.Time{}, budget)
		release()
		if e != nil {
			return
		}
		next := bundleFor(t, out)
		again, e := (Filter{}).Refilter(t.Context(), next, time.Time{})
		if e != nil || !reflect.DeepEqual(out.Records, again.Records) {
			t.Fatalf("not idempotent: %v", e)
		}
	})
}
