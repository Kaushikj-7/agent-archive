package muse

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

// routeMetadataMutation edits the synthetic route-metadata lane: metadata
// without a model, route facts, metadata with the model, an unsupported
// branch observation and the original opening. Rows are renumbered after
// change.
func routeMetadataMutation(t *testing.T, change func([]map[string]any) []map[string]any) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(readSynthetic(t, "synthetic-route-metadata.jsonl")), []byte("\n"))
	rows := make([]map[string]any, 0, 5)
	for _, line := range lines[1:] {
		m, _, err := decodeObject(line)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, m)
	}
	out := [][]byte{lines[0]}
	for i, row := range change(rows) {
		row["sequence"] = i + 3
		wire, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, wire)
	}
	return append(bytes.Join(out, []byte("\n")), '\n')
}

func TestRouteMetadataStartup(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-route-metadata.jsonl")
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-route-metadata.golden.jsonl"), append(bytes.Join(out.Records, []byte("\n")), '\n'))
	for _, g := range out.Gaps {
		if g.Code == "model_identity_unavailable" {
			t.Fatal("the second metadata row did not supply the startup model")
		}
	}
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

func TestRouteMetadataStartupRefusesUnqualifiedOrders(t *testing.T) {
	t.Parallel()
	record := func(m map[string]any) map[string]any { return object(object(m["payload"])["record"]) }
	unknown := func(r []map[string]any) map[string]any {
		wire, _ := json.Marshal(r[3])
		extra, _, _ := decodeObject(wire)
		extra["id"] = "00000000-0000-4000-8000-000000000096"
		return extra
	}
	cases := map[string]func([]map[string]any) []map[string]any{
		"route before metadata": func(r []map[string]any) []map[string]any {
			return []map[string]any{r[1], r[0], r[2], r[3], r[4]}
		},
		"metadata pair before route": func(r []map[string]any) []map[string]any {
			return []map[string]any{r[0], r[2], r[1], r[3], r[4]}
		},
		"omitted record before route": func(r []map[string]any) []map[string]any {
			return []map[string]any{r[0], unknown(r), r[1], r[2], r[3], r[4]}
		},
		"omitted record before second metadata": func(r []map[string]any) []map[string]any {
			return []map[string]any{r[0], r[1], unknown(r), r[2], r[3], r[4]}
		},
		"route for another directory": func(r []map[string]any) []map[string]any {
			record(r[1])["cwd"] = "/work/elsewhere"
			return r
		},
		"second metadata for another workspace": func(r []map[string]any) []map[string]any {
			record(r[2])["workspace_root"] = "/work/elsewhere"
			return r
		},
		"second metadata for another provider": func(r []map[string]any) []map[string]any {
			record(r[2])["provider_id"] = "other"
			return r
		},
		"second metadata for another tool surface": func(r []map[string]any) []map[string]any {
			record(r[2])["tool_surface_version"] = "3"
			return r
		},
		"second metadata for another build": func(r []map[string]any) []map[string]any {
			object(record(r[2])["build"])["sha"] = "other-sha"
			return r
		},
		"second metadata for another search mode": func(r []map[string]any) []map[string]any {
			record(r[2])["web_search_mode"] = "other-mode"
			return r
		},
		"second metadata without a model": func(r []map[string]any) []map[string]any {
			delete(record(r[2]), "model_id")
			return r
		},
		"second metadata with an extra member": func(r []map[string]any) []map[string]any {
			record(r[2])["future"] = "synthetic"
			return r
		},
		"first metadata with a model": func(r []map[string]any) []map[string]any {
			record(r[0])["model_id"] = "synthetic-model"
			return r
		},
		"second metadata for another session": func(r []map[string]any) []map[string]any {
			object(r[2]["stream"])["id"] = "00000000-0000-7000-8000-000000000099"
			return r
		},
		"route before metadata time": func(r []map[string]any) []map[string]any {
			r[1]["recorded_at"] = json.Number("1792083858882703")
			return r
		},
		"second metadata before route time": func(r []map[string]any) []map[string]any {
			r[2]["recorded_at"] = json.Number("1792083858882704")
			return r
		},
		"opening resumes": func(r []map[string]any) []map[string]any {
			record(r[4])["resume"] = true
			return r
		},
	}
	for index, name := range map[int]string{0: "caused metadata", 1: "caused route", 2: "caused second metadata", 4: "caused opening"} {
		cases[name] = func(r []map[string]any) []map[string]any { r[index]["causation_id"] = r[1]["id"]; return r }
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw := routeMetadataMutation(t, mutate)
			if out, err := filterBytes(t, raw, agentapi.ReadLimits{}); err == nil || len(out.Records) != 0 {
				t.Fatal("filter accepted unqualified route-metadata startup")
			}
			if _, err := inspectSynthetic(t, raw); err == nil {
				t.Fatal("header accepted unqualified route-metadata startup")
			}
			if _, err := routeStartupFacts(t, raw); err == nil {
				t.Fatal("source facts accepted unqualified route-metadata startup")
			}
		})
	}
}

func TestRouteMetadataStartupRetainedGuards(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, readSynthetic(t, "synthetic-route-metadata.jsonl"), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	// Retained rows: frame children 0 and 1, metadata 2, route 3, metadata 4.
	for name, mutate := range map[string]func(archive.SourceBundle){
		"route cwd":                 func(b archive.SourceBundle) { object(b.NativeRecords[3]["fields"])["cwd"] = "/work/other" },
		"second metadata workspace": func(b archive.SourceBundle) { object(b.NativeRecords[4]["fields"])["workspace_root"] = "/work/other" },
		"second metadata provider":  func(b archive.SourceBundle) { object(b.NativeRecords[4]["fields"])["provider_id"] = "other" },
		"second metadata model":     func(b archive.SourceBundle) { delete(object(b.NativeRecords[4]["fields"]), "model_id") },
		"second metadata cause":     func(b archive.SourceBundle) { b.NativeRecords[4]["causation_id"] = b.NativeRecords[3]["id"] },
	} {
		t.Run(name, func(t *testing.T) {
			bundle := cloneBundle(t, bundleFor(t, out))
			mutate(bundle)
			assertRetainedRefused(t, bundle)
		})
	}
}
