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
	for _, name := range []string{"synthetic-exec-empty.jsonl", "synthetic-exec-conversation.jsonl", "synthetic-msp-empty.jsonl", "synthetic-msp-conversation.jsonl", "synthetic-native-omissions.jsonl", "synthetic-model-resolution-omissions.jsonl", "synthetic-own-observations.jsonl", "synthetic-own-tools.jsonl", "synthetic-committed-read-effects.jsonl"} {
		raw, e := os.ReadFile(filepath.Join("testdata", name))
		if e != nil {
			f.Fatal(e)
		}
		f.Add(raw)
		if name == "synthetic-native-omissions.jsonl" {
			read := bytes.Replace(raw, []byte(`"parallel_profile":{"kind":"ineligible"}`), []byte(`"parallel_profile":{"kind":"file_read","subject":"PRIVATE_EFFECT_SUBJECT_SENTINEL"}`), 1)
			f.Add(bytes.Replace(read, []byte(`"task_completion":{"kind":"terminal","terminal":{"kind":"completed"}}`), []byte(`"task_completion":{"kind":"complete"}`), 1))
			f.Add(bytes.Replace(read, []byte(`"outcome":{"kind":"completed","output_ref_count":1,"task_completion":{"kind":"terminal","terminal":{"kind":"completed"}}}`), []byte(`"outcome":{"kind":"failed","reason":"PRIVATE_EFFECT_REASON_SENTINEL"}`), 1))
		}
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
	for _, name := range []string{"synthetic-exec-empty.golden.jsonl", "synthetic-msp-conversation.golden.jsonl", "synthetic-native-omissions.golden.jsonl", "synthetic-model-resolution-omissions.golden.jsonl", "synthetic-own-observations.golden.jsonl", "synthetic-own-tools.golden.jsonl", "synthetic-committed-read-effects.golden.jsonl"} {
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

func FuzzMuseHeaderPreview(f *testing.F) {
	for _, name := range []string{"synthetic-exec-conversation.jsonl", "synthetic-msp-conversation.jsonl", "synthetic-model-resolution-omissions.jsonl"} {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(raw)
		if name == "synthetic-model-resolution-omissions.jsonl" {
			f.Add(bytes.Replace(raw, []byte(`"provider_id":"synthetic"`), []byte(`"provider_id":"foreign"`), 1))
			lines := bytes.Split(raw, []byte("\n"))
			f.Add(append(bytes.Join(lines[:3], []byte("\n")), '\n'))
		}
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 256<<10 {
			t.Skip()
		}
		h, err := (NativeHeaders{}).InspectHeader(agentapi.NativeHeaderRequest{Purpose: agentapi.DiscoveryHandoff, Path: "/store/00000000-0000-7000-8000-000000000001/session.jsonl", Scan: func(visit func([]byte) bool) error {
			for line := range bytes.SplitSeq(raw, []byte("\n")) {
				if !visit(line) {
					break
				}
			}
			return nil
		}})
		if err == nil && (h.NativeID == "" || !filepath.IsAbs(h.Directory) || h.StartedAt.IsZero()) {
			t.Fatal("unqualified header")
		}
		for line := range bytes.SplitSeq(raw, []byte("\n")) {
			p, err := (Previewer{}).PreviewRecord(t.Context(), line)
			if err == nil && (p.Title != "" || p.Kind == archive.TurnKindHumanPrompt || p.Name != "" && (p.NameSource != archive.SessionNameGenerated || p.NameNativeID == "")) {
				t.Fatal("unqualified preview")
			}
		}
	})
}
