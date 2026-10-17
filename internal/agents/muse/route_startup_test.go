package muse

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wangjohn/agent-archive/internal/agentapi"
	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/testutil/golden"
)

func routeStartupFacts(t *testing.T, raw []byte) (archive.NativeSourceBinding, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	budget := agentapi.NewNativeReadBudget(8 << 20)
	pass, err := (SourceProvider{}).OpenPass(t.Context(), agentapi.SourceEnvironment{ReadBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pass.Close(); err != nil {
			t.Error(err)
		}
		if used, _ := budget.Charged(); used != 0 {
			t.Errorf("startup leaked %d bytes", used)
		}
	})
	snap, err := pass.Read(t.Context(), agentapi.SourceRef{Path: path}, agentapi.ReadLimits{})
	if err != nil {
		return archive.NativeSourceBinding{}, err
	}
	defer func() {
		if err := snap.Close(); err != nil {
			t.Error(err)
		}
	}()
	return snap.(*sourceSnapshot).startupFacts(t.Context())
}

func TestExactRouteFactsStartup(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-route-startup.jsonl")
	out, err := filterBytes(t, raw, agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	golden.Check(t, filepath.Join("testdata", "synthetic-route-startup.golden.jsonl"), append(bytes.Join(out.Records, []byte("\n")), '\n'))
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
	if err != nil || facts.NativeID != header.NativeID || facts.StreamID != header.NativeID || facts.Cwd != header.Directory || !facts.NativeCreatedAt.Equal(header.StartedAt) || !facts.NativeCreatedAt.Equal(out.NativeStartAt) || facts.Checkpoint != nil || facts.RootRevision != "" {
		t.Fatalf("startup consumers disagree: %+v %+v %v", header, facts, err)
	}
	// The lane ends after the original opening, within the existing prefix bound.
	if _, err := routeStartupFacts(t, append(bytes.Clone(raw), []byte("unknown suffix\n")...)); err != nil {
		t.Fatal(err)
	}
	if _, err := filterBytes(t, append(bytes.Clone(raw), []byte("unknown suffix\n")...), agentapi.ReadLimits{}); err == nil {
		t.Fatal("prefix facts bypassed whole-file validation")
	}
}

func routeStartupMutation(t *testing.T, change func([]map[string]any) []map[string]any) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(readSynthetic(t, "synthetic-route-startup.jsonl")), []byte("\n"))
	rows := make([]map[string]any, 0, 3)
	for _, line := range lines[1:] {
		m, _, err := decodeObject(line)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, m)
	}
	for i, row := range change(rows) {
		row["sequence"] = i + 3
		wire, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			lines = lines[:1]
		}
		lines = append(lines, wire)
	}
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

func TestRouteFactsStartupRejectsUnqualifiedNativePrefixes(t *testing.T) {
	t.Parallel()
	cases := map[string]func([]map[string]any) []map[string]any{
		"route schema": func(r []map[string]any) []map[string]any { r[1]["payload_schema_version"] = 1; return r },
		"route kind":   func(r []map[string]any) []map[string]any { object(r[1]["payload"])["kind"] = "unknown"; return r },
		"unknown type": func(r []map[string]any) []map[string]any { r[1]["payload_type"] = "runtime.session.future"; return r },
		"missing cwd": func(r []map[string]any) []map[string]any {
			delete(object(object(r[1]["payload"])["record"]), "cwd")
			return r
		},
		"workspace": func(r []map[string]any) []map[string]any {
			object(object(r[1]["payload"])["record"])["cwd"] = "/work/other"
			return r
		},
		"route owner": func(r []map[string]any) []map[string]any {
			object(r[1]["stream"])["id"] = "00000000-0000-7000-8000-000000000099"
			return r
		},
		"opening owner": func(r []map[string]any) []map[string]any {
			object(object(r[2]["payload"])["record"])["session_id"] = "00000000-0000-7000-8000-000000000099"
			return r
		},
		"resume": func(r []map[string]any) []map[string]any {
			object(object(r[2]["payload"])["record"])["resume"] = true
			return r
		},
		"untyped resume": func(r []map[string]any) []map[string]any {
			object(object(r[2]["payload"])["record"])["resume"] = "false"
			return r
		},
		"missing opening": func(r []map[string]any) []map[string]any { return r[:2] },
		"late metadata":   func(r []map[string]any) []map[string]any { return []map[string]any{r[1], r[0], r[2]} },
		"duplicate route": func(r []map[string]any) []map[string]any {
			wire, _ := json.Marshal(r[1])
			extra, _, _ := decodeObject(wire)
			extra["id"] = "00000000-0000-4000-8000-000000000098"
			return []map[string]any{r[0], r[1], extra, r[2]}
		},
		"interposed metadata": func(r []map[string]any) []map[string]any {
			wire, _ := json.Marshal(r[0])
			extra, _, _ := decodeObject(wire)
			extra["id"] = "00000000-0000-4000-8000-000000000098"
			return []map[string]any{r[0], r[1], extra, r[2]}
		},
		"route before birth": func(r []map[string]any) []map[string]any { r[1]["recorded_at"] = 1; return r },
	}
	for name, payload := range map[string]struct {
		typ  string
		body map[string]any
	}{
		"interposed resumed":       {"session.resumed", map[string]any{"kind": "session_resumed", "record": map[string]any{"schema_version": 1, "session_id": "00000000-0000-7000-8000-000000000001"}}},
		"interposed retained fact": {"runtime.retained_fact", map[string]any{"kind": "retained_runtime_fact", "record": map[string]any{"payload_type": "top_level_turn.run_origin", "payload_schema_version": 1, "payload": map[string]any{}}}},
	} {
		cases[name] = func(r []map[string]any) []map[string]any {
			wire, _ := json.Marshal(r[1])
			extra, _, _ := decodeObject(wire)
			extra["id"] = "00000000-0000-4000-8000-000000000098"
			extra["payload_type"], extra["payload_schema_version"], extra["payload"] = payload.typ, 1, payload.body
			return []map[string]any{r[0], r[1], extra, r[2]}
		}
	}
	var privateEffect map[string]any
	lines := bytes.Split(bytes.TrimSpace(readSynthetic(t, "synthetic-native-omissions.jsonl")), []byte("\n"))
	for _, line := range lines[1:] {
		extra, _, err := decodeObject(line)
		if err == nil && str(extra["payload_type"]) == "approval_wait.effect.started" {
			privateEffect = extra
			break
		}
	}
	if privateEffect == nil {
		t.Fatal("missing synthetic effect")
	}
	cases["interposed private effect"] = func(r []map[string]any) []map[string]any {
		return []map[string]any{r[0], r[1], privateEffect, r[2]}
	}
	for _, index := range []int{0, 1, 2} {
		cases[[]string{"metadata cause", "route cause", "opening cause"}[index]] = func(r []map[string]any) []map[string]any { r[index]["causation_id"] = r[1]["id"]; return r }
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw := routeStartupMutation(t, mutate)
			if _, err := filterBytes(t, raw, agentapi.ReadLimits{}); err == nil {
				t.Fatal("filter accepted unqualified prefix")
			}
			if _, err := inspectSynthetic(t, raw); err == nil {
				t.Fatal("header accepted unqualified prefix")
			}
			if _, err := routeStartupFacts(t, raw); err == nil {
				t.Fatal("source facts accepted unqualified prefix")
			}
		})
	}
}

func TestRouteFactsStartupRetainedGuards(t *testing.T) {
	t.Parallel()
	out, err := filterBytes(t, readSynthetic(t, "synthetic-route-startup.jsonl"), agentapi.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(archive.SourceBundle){
		func(b archive.SourceBundle) { b.NativeRecords[3]["causation_id"] = b.NativeRecords[3]["id"] },
		func(b archive.SourceBundle) { object(b.NativeRecords[3]["fields"])["cwd"] = "/work/other" },
		func(b archive.SourceBundle) { object(b.NativeRecords[4]["fields"])["resume"] = true },
		func(b archive.SourceBundle) { b.NativeRecords[3]["payload_schema"] = 1 },
		func(b archive.SourceBundle) { object(b.NativeRecords[3]["fields"])["future"] = true },
		func(b archive.SourceBundle) { object(b.NativeRecords[3]["pos"])["line"] = 4 },
		func(b archive.SourceBundle) { b.NativeRecords[3]["ts_us"] = 1 },
		func(b archive.SourceBundle) {
			object(b.NativeRecords[3]["stream"])["id"] = "00000000-0000-7000-8000-000000000099"
		},
		func(b archive.SourceBundle) { b.NativeRecords[4] = b.NativeRecords[3] },
	} {
		bundle := cloneBundle(t, bundleFor(t, out))
		mutate(bundle)
		assertRetainedRefused(t, bundle)
	}
}

type routeStartupFramingKind string

const (
	routeStartupChecksum     routeStartupFramingKind = "checksum"
	routeStartupFrameSchema  routeStartupFramingKind = "frame schema"
	routeStartupLeadingBlank routeStartupFramingKind = "leading blank"
	routeStartupUnterminated routeStartupFramingKind = "unterminated"
)

func TestRouteFactsStartupKeepsPhysicalAndParentGuards(t *testing.T) {
	t.Parallel()
	raw := readSynthetic(t, "synthetic-route-startup.jsonl")
	for name, input := range map[routeStartupFramingKind][]byte{
		routeStartupChecksum:     bytes.Replace(raw, []byte(`"content_sha256":"sha256:`), []byte(`"content_sha256":"sha256:0`), 1),
		routeStartupFrameSchema:  bytes.Replace(raw, []byte(`"frame_schema_version":1`), []byte(`"frame_schema_version":2`), 1),
		routeStartupLeadingBlank: append([]byte("\n"), raw...),
		routeStartupUnterminated: bytes.TrimSuffix(raw, []byte("\n")),
	} {
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()
			if _, err := filterBytes(t, input, agentapi.ReadLimits{}); err == nil {
				t.Fatal("filter accepted physical mutation")
			}
			if _, err := routeStartupFacts(t, input); err == nil {
				t.Fatal("startup accepted physical mutation")
			}
			if name == routeStartupChecksum || name == routeStartupFrameSchema {
				if _, err := inspectSynthetic(t, input); err == nil {
					t.Fatal("header accepted malformed frame")
				}
			}
		})
	}
	_, err := (NativeHeaders{}).InspectHeader(agentapi.NativeHeaderRequest{Purpose: agentapi.DiscoveryHandoff, Path: "/store/unrelated/session.jsonl", Scan: func(visit func([]byte) bool) error {
		for line := range bytes.SplitSeq(bytes.TrimSpace(raw), []byte("\n")) {
			if !visit(line) {
				break
			}
		}
		return nil
	}})
	if err == nil {
		t.Fatal("header accepted unrelated parent")
	}
}
