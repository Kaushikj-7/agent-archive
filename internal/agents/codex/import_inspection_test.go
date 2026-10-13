package codex

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
)

type inspectionFile struct {
	agentapi.FileInput
	*strings.Reader
	check error
}

func (f inspectionFile) ReadAt(p []byte, off int64) (int, error) { return f.Reader.ReadAt(p, off) }
func (f inspectionFile) Length() int64                           { return f.Size() }
func (f inspectionFile) Check() error                            { return f.check }

func TestImportInspectionMatchesRetainedRowsAndNativeStart(t *testing.T) {
	for _, row := range []string{
		`{"type":"response_item","timestamp":"2020-01-01T00:00:00Z","payload":{"type":"message","role":"user","content":"visible"}}`,
		`{"type":"event_msg","payload":{"message":"visible"}}`,
		`{"type":"turn_context","payload":{"instructions":"hidden instruction"}}`,
		`{"type":"message","role":42,"content":"visible"}`,
		`{"type":"message","role":null,"content":"visible"}`,
		`{"type":"event_msg","Role":"user","payload":{"message":"visible"}}`,
		`{"type":"message","ROLE":42,"role":"user","content":"visible"}`,
		`{"type":"event_msg","Role":"user","role":null,"payload":{"message":"visible"}}`,
		`{"type":"unknown","content":"visible"}`,
		`{invalid}`,
	} {
		t.Run(row, func(t *testing.T) {
			raw := `{"type":"session_meta","timestamp":"2021-01-01T00:00:00Z","payload":{"id":"session","source":"cli"}}` + "\n" + row + "\n"
			in := agentapi.NativeInput{File: inspectionFile{Reader: strings.NewReader(raw)}}
			c := agentapi.FilterContext{Filename: "ordinary.jsonl"}
			full, fullErr := (Filter{}).Filter(t.Context(), in, c)
			summary, err := (Filter{}).InspectFilteredImport(t.Context(), in, c)
			if (err == nil) != (fullErr == nil) {
				t.Fatalf("errors summary=%v full=%v", err, fullErr)
			}
			if err != nil {
				return
			}
			inspector := Imports()
			want, err := inspector.InspectImport(t.Context(), agentapi.ImportInspectionRequest{Filtered: full})
			if err != nil {
				t.Fatal(err)
			}
			got, err := inspector.(agentapi.SummaryImportInspector).InspectImportSummary(t.Context(), agentapi.ImportInspectionRequest{}, summary)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("inspection summary=%+v retained=%+v err=%v", got, want, err)
			}
			if summary.RetainedBytes != int64(full.Boundary.RetainedBytes) || summary.RetainedRecords != full.Boundary.RetainedRecords {
				t.Fatalf("boundary summary=%+v full=%+v", summary, full.Boundary)
			}
		})
	}
}

func TestImportInspectionReleasesBudgetAndPreservesFinalChecks(t *testing.T) {
	dir := t.TempDir()
	ref, _ := historyFile(t, dir, threadA, threadA, 0, nil, strings.Repeat("safe content ", 200))
	budget := agentapi.NewNativeReadBudget(2 << 20)
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	defer pass.Close()
	snap, err := pass.Read(t.Context(), ref, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	before, _ := budget.Charged()
	c := agentapi.FilterContext{Filename: filepath.Base(ref.Path), Limits: agentapi.ReadLimits{ReadBudget: budget}}
	_, err = (Filter{}).InspectFilteredImport(t.Context(), snap.Input(), c)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := budget.Charged()
	if before != after {
		t.Fatalf("inspection retained charge %d", after-before)
	}
	reservation := budget.Reservation()
	pressure := budget.Available() - (70 << 10)
	if !reservation.Reserve(pressure) {
		t.Fatal("pressure reservation refused")
	}
	_, err = (Filter{}).InspectFilteredImport(t.Context(), snap.Input(), c)
	if !agentapi.HasFailure(err, agentapi.Limit) {
		t.Fatalf("encoding pressure was not a limit refusal: %v", err)
	}
	after, _ = budget.Charged()
	if after != before+pressure {
		t.Fatalf("pressure refusal retained charge %d", after-before-pressure)
	}
	reservation.Release(pressure)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = (Filter{}).InspectFilteredImport(ctx, snap.Input(), c)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, _ = budget.Charged()
	if before != after {
		t.Fatalf("cancellation retained charge %d", after-before)
	}
	in := agentapi.NativeInput{File: inspectionFile{Reader: strings.NewReader(`{"type":"message","content":"visible"}` + "\n"), check: io.ErrUnexpectedEOF}}
	_, err = (Filter{}).InspectFilteredImport(t.Context(), in, agentapi.FilterContext{})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lost final check: %v", err)
	}
}

func TestImportInspectionRetainsLateMetadataAndLimitRefusals(t *testing.T) {
	rows := `{"type":"message","content":"visible"}` + "\n"
	for _, tail := range []string{
		`{"type":"session_meta","payload":{"id":"x","history_mode":"unsupported"}}` + "\n",
		`{"type":"session_meta","payload":{"id":"x","forked_from_id":"ancestor"}}` + "\n",
		"",
	} {
		for _, bound := range []agentapi.ReadLimits{{}, {Records: 1}, {FilteredBytes: 1}, {RecordBytes: 8}} {
			in := agentapi.NativeInput{File: inspectionFile{Reader: strings.NewReader(rows + tail)}}
			c := agentapi.FilterContext{Filename: "ordinary.jsonl", Limits: bound}
			_, want := (Filter{}).Filter(t.Context(), in, c)
			_, got := (Filter{}).InspectFilteredImport(t.Context(), in, c)
			if (got == nil) != (want == nil) || (got != nil && got.Error() != want.Error()) {
				t.Fatalf("different refusal: got=%v want=%v tail=%s bounds=%+v", got, want, tail, bound)
			}
		}
	}
}

func BenchmarkCodexImportInspection(b *testing.B) {
	raw := strings.Repeat(`{"type":"response_item","timestamp":"2020-01-01T00:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic visible content"}]}}`+"\n", 2048)
	in := agentapi.NativeInput{File: inspectionFile{Reader: strings.NewReader(raw)}}
	c := agentapi.FilterContext{Filename: "ordinary.jsonl"}
	for _, streaming := range []bool{false, true} {
		name := "retained"
		if streaming {
			name = "summary"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for b.Loop() {
				var err error
				if streaming {
					_, err = (Filter{}).InspectFilteredImport(b.Context(), in, c)
				} else {
					var full archive.FilteredTranscript
					full, err = (Filter{}).Filter(b.Context(), in, c)
					if err == nil {
						_, err = Imports().InspectImport(b.Context(), agentapi.ImportInspectionRequest{Filtered: full})
					}
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
